package trace

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/skyhook-io/radar/internal/issues"
	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/probe"
)

// A deny-all NetworkPolicy issue shows on the Service hop it isolates. The
// Pods hop keeps only its own per-port policy finding, so the same isolation
// isn't reported twice there.
func TestServiceTraceShowsNetworkPolicyIssueOnce(t *testing.T) {
	defer k8s.ResetTestState()
	labels := map[string]string{"app": "rec"}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "recommendation", Namespace: "shop"},
		Spec:       corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 80}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "rec-1", Namespace: "shop", Labels: labels},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-all", Namespace: "shop", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute))},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	client := fake.NewClientset(svc, pod, policy)
	if err := k8s.InitTestResourceCache(client); err != nil {
		t.Fatalf("init: %v", err)
	}
	fakeDisc := client.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDisc.Resources = []*metav1.APIResourceList{{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{
		{Name: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}},
	}}}
	restore, err := k8s.SetTestResourceDiscovery(fakeDisc)
	if err != nil {
		t.Fatalf("SetTestResourceDiscovery: %v", err)
	}
	defer restore()

	allow := IssueAccess{
		CanReadClusterScoped: func(string, string) bool { return true },
		CanReadRelated:       func(issues.Ref) bool { return true },
		CanReadSubject:       func(issues.Ref) bool { return true },
		CanReadEvidence:      func(issues.EvidenceRead) bool { return true },
	}
	deps := Deps{Cache: k8s.GetResourceCache(), Dynamic: k8s.GetDynamicResourceCache(), Discovery: k8s.GetResourceDiscovery(), Issues: issues.NewCacheProvider(), IssueAccess: allow}

	codes := func(h Hop) map[string]int {
		out := map[string]int{}
		for _, f := range h.Findings {
			out[f.Code]++
		}
		return out
	}
	var svcHop, podsHop Hop
	waitFor(t, func() bool {
		tr, err := BuildTraceWithOptions(context.Background(), deps, "Service", "shop", "recommendation", Options{})
		if err != nil || len(tr.Downstream) < 2 {
			return false
		}
		svcHop, podsHop = tr.Downstream[0], tr.Downstream[1]
		return codes(svcHop)[k8s.NetworkPolicyDenyAllIngressFingerprint] == 1
	})
	if c := codes(podsHop); c[k8s.NetworkPolicyDenyAllIngressFingerprint] != 0 || c[codePolicyWouldDeny] != 1 {
		t.Fatalf("Pods hop findings = %v, want only netpol:would-deny", c)
	}
	// The in-cluster test settles the prediction only for its own protocol
	// and Service port, which the trace records.
	if podsHop.Meta["policyProtocol"] != "TCP" || podsHop.Meta["policyPort"] == nil {
		t.Errorf("Pods hop meta = %v, want the prediction's protocol and Service port", podsHop.Meta)
	}

	deps.IssueAccess.CanReadRelated = nil
	tr, err := BuildTraceWithOptions(context.Background(), deps, "Service", "shop", "recommendation", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if codes(tr.Downstream[0])[k8s.NetworkPolicyDenyAllIngressFingerprint] != 0 {
		t.Fatal("the policy issue reached the Service hop without a related-resource authorizer")
	}
}

// Traffic that got through to the pods settles the Service hop's policy issues
// (one per policy) into a single note, as it does the Pods hop's per-port
// prediction; a dropped data path keeps them.
func TestReconcilePolicyDowngradesServiceDenyAll(t *testing.T) {
	trace := func(dataOK bool) *Trace {
		svc := Hop{Resource: ResourceRef{Kind: "Service", Namespace: "ns", Name: "rec"}, Edge: "entry:Service", Findings: []Finding{
			{Code: k8s.NetworkPolicyDenyAllIngressFingerprint, Severity: SeverityWarning, Message: "NetworkPolicy denies all ingress - Would block all inbound traffic (default-deny)"},
			{Code: k8s.NetworkPolicyDenyAllIngressFingerprint, Severity: SeverityWarning, Message: "NetworkPolicy denies all ingress - Would block all inbound traffic (deny-rec)"},
		}}
		pods := wouldDenyHop([]probe.Result{
			{Layer: probe.LayerTCP, Target: "10.0.0.5:80", Port: 80, Path: probe.PathData, OK: dataOK},
		}, false)
		pods.Edge = "Service->Pods"
		return &Trace{Downstream: []Hop{svc, pods}}
	}

	tr := trace(true)
	reconcilePolicyFindings(tr)
	if got := tr.Downstream[0].Findings; len(got) != 1 || got[0].Severity != SeverityInfo || !strings.Contains(got[0].Message, "Traffic got through") {
		t.Errorf("Service hop findings = %+v, want one info note after traffic got through", got)
	}

	tr = trace(false)
	reconcilePolicyFindings(tr)
	for _, f := range tr.Downstream[0].Findings {
		if f.Severity != SeverityWarning {
			t.Errorf("Service hop finding %+v, want both warnings kept when the data path was dropped", f)
		}
	}
}

// The in-cluster verify path settles the Service hop's deny-all policy issue
// with the Pods hop's prediction once real traffic got through, so the
// verdict re-derives off degraded; a dropped in-cluster request keeps both.
func TestApplyInClusterResultsSettlesServiceDenyAll(t *testing.T) {
	trace := func() *Trace {
		return &Trace{
			Subject: ResourceRef{Kind: "Service", Namespace: "shop", Name: "rec"},
			Verdict: VerdictDegraded,
			Downstream: []Hop{
				{Resource: ResourceRef{Kind: "Service", Namespace: "shop", Name: "rec"}, Edge: "entry:Service", Findings: []Finding{{
					Code: k8s.NetworkPolicyDenyAllIngressFingerprint, Severity: SeverityWarning, Message: "NetworkPolicy denies all ingress - Would block all inbound traffic",
				}}},
				{Resource: ResourceRef{Kind: "Pods", Namespace: "shop"}, Edge: "Service->Pods",
					Meta:     map[string]any{"policyVerdict": "would-deny", "policyNames": []string{"deny-all"}, "policyPort": ":80", "policyDenyPort": int32(80)},
					Findings: []Finding{{Code: codePolicyWouldDeny, Severity: SeverityWarning, Message: "A network rule here would block this traffic"}}},
			},
			Routes: []RouteResult{{Route: "rec", Target: "rec:80", Outcome: OutcomeReached, Confidence: ConfidenceIndirect,
				InClusterRequest: &ProbeRequest{Protocol: "http", Scheme: "http", Path: "/"}}},
			Coverage: &Coverage{Tested: 1, Passed: 1},
		}
	}

	tr := trace()
	ApplyInClusterResults(tr, map[string][]probe.Result{InClusterResultKey("rec", "rec:80", ""): cleanInClusterPass()})
	for i, h := range tr.Downstream {
		for _, f := range h.Findings {
			if f.Severity != SeverityInfo {
				t.Errorf("hop %d finding %s = %s, want info after real in-cluster traffic got through", i, f.Code, f.Severity)
			}
		}
	}
	if tr.Verdict == VerdictDegraded {
		t.Errorf("verdict = %s, want it re-derived off degraded", tr.Verdict)
	}

	tr = trace()
	ApplyInClusterResults(tr, map[string][]probe.Result{InClusterResultKey("rec", "rec:80", ""): {{
		Layer: probe.LayerTCP, Path: probe.PathData, Vantage: probe.VantageInCluster, OK: false, Detail: "connection timed out",
	}}})
	if f := tr.Downstream[0].Findings[0]; f.Severity != SeverityWarning {
		t.Errorf("Service hop finding = %s, want the warning kept when in-cluster traffic was dropped", f.Severity)
	}
}

// A real in-cluster pass settles the Service's deny-all issue whatever the
// port. It settles the Pods hop's prediction only when it went through the
// Service port the prediction was evaluated for, and only for TCP, the
// protocol of the in-cluster request.
func TestApplyInClusterResultsSettlesDenyAllAcrossPorts(t *testing.T) {
	type prediction struct {
		servicePort, podPort int32
		protocol             string
	}
	trace := func(pred prediction, passPort int32, ports ...PortMap) *Trace {
		return &Trace{
			Subject: ResourceRef{Kind: "Service", Namespace: "shop", Name: "rec"},
			Verdict: VerdictDegraded,
			Downstream: []Hop{
				{Resource: ResourceRef{Kind: "Service", Namespace: "shop", Name: "rec"}, Edge: "entry:Service",
					Config:   &HopConfig{Ports: ports},
					Findings: []Finding{{Code: k8s.NetworkPolicyDenyAllIngressFingerprint, Severity: SeverityWarning, Message: "NetworkPolicy denies all ingress"}}},
				{Resource: ResourceRef{Kind: "Pods", Namespace: "shop"}, Edge: "Service->Pods",
					Meta: map[string]any{"policyVerdict": "would-deny", "policyNames": []string{"deny-all"},
						"policyPort": fmt.Sprintf(":%d", pred.servicePort), "policyDenyPort": pred.podPort, "policyProtocol": pred.protocol},
					Findings: []Finding{{Code: codePolicyWouldDeny, Severity: SeverityWarning, Message: "A network rule here would block this traffic"}}},
			},
			Routes: []RouteResult{{Route: "rec", Target: fmt.Sprintf("rec:%d", passPort), Outcome: OutcomeReached, Confidence: ConfidenceIndirect,
				InClusterRequest: &ProbeRequest{Protocol: "http", Scheme: "http", Path: "/"}}},
			Coverage: &Coverage{Tested: 1, Passed: 1},
		}
	}
	for _, tt := range []struct {
		name        string
		pred        prediction
		passPort    int32
		ports       []PortMap
		wantPods    string
		wantHealthy bool
	}{
		{name: "Service port 80 forwards to targetPort 8080",
			pred: prediction{80, 8080, "TCP"}, passPort: 80, ports: []PortMap{{Port: 80, TargetPort: "8080"}},
			wantPods: SeverityInfo, wantHealthy: true},
		{name: "named targetPort",
			pred: prediction{80, 8080, "TCP"}, passPort: 80, ports: []PortMap{{Port: 80, TargetPort: "http"}},
			wantPods: SeverityInfo, wantHealthy: true},
		{name: "the prediction is about another Service port",
			pred: prediction{90, 9090, "TCP"}, passPort: 80, ports: []PortMap{{Port: 80, TargetPort: "8080"}, {Port: 90, TargetPort: "9090"}},
			wantPods: SeverityWarning},
		{name: "crossed ports: Service 90 forwards to the denied pod port 80",
			pred: prediction{90, 80, "TCP"}, passPort: 80, ports: []PortMap{{Port: 80, TargetPort: "8080"}, {Port: 90, TargetPort: "80"}},
			wantPods: SeverityWarning},
		{name: "a TCP pass doesn't settle a UDP denial on the same Service port",
			pred: prediction{53, 5353, "UDP"}, passPort: 53, ports: []PortMap{{Port: 53, TargetPort: "5353", Protocol: "UDP"}, {Port: 53, TargetPort: "8080", Protocol: "TCP"}},
			wantPods: SeverityWarning},
		{name: "a TCP pass doesn't settle a UDP denial on the same pod port",
			pred: prediction{53, 5353, "UDP"}, passPort: 53, ports: []PortMap{{Port: 53, TargetPort: "5353", Protocol: "UDP"}, {Port: 53, TargetPort: "5353", Protocol: "TCP"}},
			wantPods: SeverityWarning},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := trace(tt.pred, tt.passPort, tt.ports...)
			ApplyInClusterResults(tr, map[string][]probe.Result{
				InClusterResultKey("rec", fmt.Sprintf("rec:%d", tt.passPort), ""): cleanInClusterPass(),
			})
			if f := tr.Downstream[0].Findings[0]; f.Severity != SeverityInfo {
				t.Errorf("Service hop deny-all = %s, want info after a real pass", f.Severity)
			}
			if f := tr.Downstream[1].Findings[0]; f.Severity != tt.wantPods {
				t.Errorf("Pods hop prediction = %s, want %s", f.Severity, tt.wantPods)
			}
			if tt.wantHealthy && tr.Verdict == VerdictDegraded {
				t.Errorf("verdict = %s, want it off degraded", tr.Verdict)
			}
		})
	}
}

// On the Radar-process probe path, a data-path pass settles the Service's
// deny-all issue even when the prediction can't be pinned to one port.
func TestReconcilePolicySettlesDenyAllWithoutAPinnedPort(t *testing.T) {
	pods := wouldDenyHop([]probe.Result{
		{Layer: probe.LayerTCP, Target: "10.0.0.5:443", Port: 443, Path: probe.PathData, OK: true},
	}, false)
	pods.Edge = "Service->Pods"
	delete(pods.Meta, "policyDenyPort")
	tr := &Trace{Downstream: []Hop{
		{Resource: ResourceRef{Kind: "Service", Namespace: "ns", Name: "rec"}, Edge: "entry:Service", Findings: []Finding{{
			Code: k8s.NetworkPolicyDenyAllIngressFingerprint, Severity: SeverityWarning, Message: "NetworkPolicy denies all ingress",
		}}},
		pods,
	}}
	reconcilePolicyFindings(tr)
	if f := tr.Downstream[0].Findings[0]; f.Severity != SeverityInfo {
		t.Errorf("Service hop finding = %s, want info after traffic got through", f.Severity)
	}
	if f := tr.Downstream[1].Findings[0]; f.Severity != SeverityWarning {
		t.Errorf("Pods hop prediction = %s, want it kept: its port isn't pinned", f.Severity)
	}
}
