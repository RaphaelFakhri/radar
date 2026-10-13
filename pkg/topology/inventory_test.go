package topology

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"strings"
	"testing"
)

type warmingGraphProvider struct {
	stubProvider
	state InventoryState
}

func (p *warmingGraphProvider) InventoryError(resource, namespace string) error {
	if resource == "pods" && p.state != "" {
		return &InventoryError{Resource: resource, State: p.state}
	}
	return nil
}
func (p *warmingGraphProvider) Pods() ([]*corev1.Pod, error) {
	return p.pods, p.InventoryError("pods", "")
}
func TestTopologyPendingInventoryPreservesFirstPaint(t *testing.T) {
	provider := &warmingGraphProvider{state: InventoryPending, stubProvider: stubProvider{
		pods:     []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "visible", Namespace: "team", Labels: map[string]string{"app": "waiting"}}}},
		services: []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "front", Namespace: "team"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "waiting"}}}},
	}}
	for _, view := range []ViewMode{ViewModeResources, ViewModeTraffic} {
		opts := DefaultBuildOptions()
		opts.ViewMode = view
		graph, err := NewBuilder(provider).Build(opts)
		if err != nil {
			t.Fatal(err)
		}
		visible := false
		for _, node := range graph.Nodes {
			if node.Kind == KindPod || node.Kind == KindPodGroup {
				visible = true
			}
		}
		if !visible {
			t.Fatalf("%s dropped observed Pods during first paint", view)
		}
		if len(graph.IncompleteInventories) != 1 || graph.IncompleteInventories[0].State != InventoryPending {
			t.Fatalf("inventory state lost: %+v", graph.IncompleteInventories)
		}
		for _, warning := range graph.Warnings {
			if strings.Contains(warning, "Failed to list Pods") {
				t.Fatalf("loading classified as failure: %s", warning)
			}
		}
		rel := GetRelationshipsWithObject("ConfigMap", "team", "settings", &corev1.ConfigMap{}, graph, provider, nil, nil)
		if rel == nil || len(rel.Warnings) == 0 {
			t.Fatal("graph-derived relationships claimed complete absence")
		}
		if !strings.Contains(rel.Warnings[0], "still loading") {
			t.Fatalf("loading copy: %v", rel.Warnings)
		}
		provider.state = ""
		if !InventoryWarningsChanged(graph, provider) {
			t.Fatal("empty inventory becoming ready could leave a stale loading warning")
		}
		provider.state = InventoryPending
	}
}
func TestInventoryWarningsDistinguishLoadingAndPartialData(t *testing.T) {
	pending := InventoryWarningFor("pods", &InventoryError{Resource: "pods", State: InventoryPending})
	partial := InventoryWarningFor("pods", &InventoryError{Resource: "pods", State: InventoryUncovered})
	if pending.Message != "Pod data is still loading" || !strings.Contains(partial.Message, "may be incomplete") {
		t.Fatalf("state-specific copy: %+v %+v", pending, partial)
	}
}

type unavailableCertificateInventory struct{ stubProvider }

func (*unavailableCertificateInventory) Secrets() ([]*corev1.Secret, error) {
	return nil, &InventoryError{Resource: "secrets", State: InventoryPending}
}
func TestCertificateExpiryDoesNotHideInventoryFailure(t *testing.T) {
	provider := &unavailableCertificateInventory{}
	if data, err := GetCertificateExpiry(provider, nil); data != nil || err == nil {
		t.Fatalf("failed read claimed empty expiry: %+v %v", data, err)
	}
	if data, err := GetCertificateExpiry(provider, []string{}); len(data) != 0 || err != nil {
		t.Fatalf("denied namespace set attempted inventory: %+v %v", data, err)
	}
}

func TestRelationshipInventoriesOnlyDescribeRelevantSources(t *testing.T) {
	secret := InventoryWarningFor("secrets", &InventoryError{Resource: "secrets", State: InventoryUnavailable})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "team"}}
	graph := &Topology{IncompleteInventories: []InventoryWarning{secret}}
	rel := GetRelationshipsWithObject("Pod", "team", "plain", pod, graph, &stubProvider{}, nil, nil)
	if rel != nil && len(rel.Warnings) > 0 {
		t.Fatalf("unreferenced disabled Secret inventory tainted Pod: %+v", rel)
	}
	pod.Spec.Containers = []corev1.Container{{EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}}}}}}
	rel = GetRelationshipsWithObject("Pod", "team", "plain", pod, graph, &stubProvider{}, nil, nil)
	if rel != nil && len(rel.IncompleteInventories) != 0 {
		t.Fatalf("Pod declaration does not require Secret graph inventory: %+v", rel)
	}
	graph.IncompleteInventories = []InventoryWarning{InventoryWarningFor("pods", &InventoryError{Resource: "pods", State: InventoryUncovered})}
	rel = GetRelationshipsWithObject("ConfigMap", "team", "settings", &corev1.ConfigMap{}, graph, &partialRelationshipInventory{}, nil, nil)
	if rel != nil && len(rel.Warnings) > 0 {
		t.Fatalf("covered namespace inherited global scope warning: %+v", rel)
	}
	graph.IncompleteInventories = []InventoryWarning{InventoryWarningFor("pods", &InventoryError{Resource: "pods", State: InventoryPending})}
	rel = GetRelationshipsWithObject("ConfigMap", "team", "settings", &corev1.ConfigMap{}, graph, &stubProvider{}, nil, nil)
	if rel == nil || len(rel.Warnings) != 1 {
		t.Fatalf("missing consumer looked complete: %+v", rel)
	}
}

type scopedCertificateInventory struct{ mockProvider }

func (*scopedCertificateInventory) InventoryError(resource, namespace string) error {
	if namespace == "covered" {
		return nil
	}
	return &InventoryError{Resource: resource, State: InventoryUncovered, Namespace: namespace}
}
func TestCertificateExpiryPreservesVisibleSubset(t *testing.T) {
	provider := &scopedCertificateInventory{mockProvider{secrets: []*corev1.Secret{genTLSSecret(t, "covered", "cert", 30)}}}
	if data, err := GetCertificateExpiry(provider, []string{"unwatched"}); data != nil || err == nil {
		t.Fatalf("entirely unwatched certificates claimed absent: %v %v", data, err)
	}
	for _, namespaces := range [][]string{nil, {"covered"}, {"covered", "unwatched"}} {
		data, err := GetCertificateExpiry(provider, namespaces)
		if err != nil || len(data) != 1 {
			t.Fatalf("visible certificate discarded for %v: %v %v", namespaces, data, err)
		}
	}
}

func TestDisabledCertificateInventoryRetainsEmptyAnnotationMap(t *testing.T) {
	provider := &disabledCertificateInventory{}
	data, err := GetCertificateExpiry(provider, nil)
	if err != nil || len(data) != 0 {
		t.Fatalf("disabled optional certificate annotations: %v %v", data, err)
	}
}

type disabledCertificateInventory struct {
	unavailableCertificateInventory
}

func (*disabledCertificateInventory) InventoryError(resource, namespace string) error {
	return &InventoryError{Resource: resource, State: InventoryUnavailable}
}

func TestServiceAndCustomSubjectsRetainSupportingWarnings(t *testing.T) {
	graph := &Topology{IncompleteInventories: []InventoryWarning{InventoryWarningFor("deployments", &InventoryError{Resource: "deployments", State: InventoryPending})}}
	for _, subject := range []struct{ kind, group string }{{"Service", ""}, {"Rollout", "argoproj.io"}} {
		warnings := relevantInventoryWarnings(graph, subject.kind, subject.group, "team", nil, &stubProvider{})
		if len(warnings) != 1 {
			t.Fatalf("%s lost missing workload evidence: %+v", subject.kind, warnings)
		}
	}
}

func TestCustomWorkloadReferencesRetainSecretWarning(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout", "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "credentials"}}}}}}}}}}
	graph := &Topology{IncompleteInventories: []InventoryWarning{InventoryWarningFor("secrets", &InventoryError{Resource: "secrets", State: InventoryPending})}}
	if warnings := relevantInventoryWarnings(graph, "Rollout", "argoproj.io", "team", object, &stubProvider{}); len(warnings) != 1 {
		t.Fatalf("tracked Rollout Secret inventory lost: %v", warnings)
	}
}

func TestPodAncestryWarningsFollowItsOwner(t *testing.T) {
	graph := &Topology{IncompleteInventories: []InventoryWarning{InventoryWarningFor("deployments", &InventoryError{Resource: "deployments", State: InventoryUnavailable})}}
	pod := &corev1.Pod{}
	if warnings := relevantInventoryWarnings(graph, "Pod", "", "team", pod, &stubProvider{}); len(warnings) != 0 {
		t.Fatalf("unowned Pod depends on Deployment inventory: %v", warnings)
	}
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "task"}}
	if warnings := relevantInventoryWarnings(graph, "Pod", "", "team", pod, &stubProvider{}); len(warnings) != 0 {
		t.Fatalf("Job Pod depends on Deployment inventory: %v", warnings)
	}
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "app"}}
	if warnings := relevantInventoryWarnings(graph, "Pod", "", "team", pod, &stubProvider{}); len(warnings) != 1 {
		t.Fatalf("Deployment ancestry looked complete: %v", warnings)
	}
}
