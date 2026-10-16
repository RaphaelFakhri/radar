package topology

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/skyhook-io/radar/pkg/k8score"
	"github.com/skyhook-io/radar/pkg/karpenter"
	"github.com/skyhook-io/radar/pkg/resourceid"
)

// Every Edge literal that can be a manages edge must say what evidence it
// rests on; a literal of any other constant type must not claim any. A new
// manages producer that omits the field would leave its edges unclassified,
// and ownership questions would silently treat them as unknown.
func TestEveryManagesEdgeLiteralDeclaresItsEvidence(t *testing.T) {
	fset, files := parsePackageSources(t)
	literals := 0
	for _, file := range files {
		violations, found := edgeEvidenceViolations(fset, file)
		literals += found
		for _, violation := range violations {
			t.Error(violation)
		}
	}
	if literals == 0 {
		t.Fatal("found no Edge literals; the guard no longer inspects producers")
	}
}

// Like the node checker, this one is syntactic: an Edge assembled field by
// field from its zero value is invisible to it, and
// TestBuiltManagesEdgesCarryEvidence catches such an edge at runtime.
func TestEdgeEvidenceCheckerCatchesBypasses(t *testing.T) {
	for _, tc := range []struct {
		name, src  string
		violations int
	}{
		{"manages without evidence", `var e = Edge{ID: "x", Type: EdgeManages}`, 1},
		{"manages with evidence", `var e = Edge{ID: "x", Type: EdgeManages, evidence: evidenceAssociation}`, 0},
		{"other type without evidence", `var e = Edge{ID: "x", Type: EdgeUses}`, 0},
		{"other type with evidence", `var e = Edge{ID: "x", Type: EdgeUses, evidence: evidenceAssociation}`, 1},
		{"computed type without evidence", `func f(t EdgeType) Edge { return Edge{ID: "x", Type: t} }`, 1},
		{"computed type with evidence", `func f(t EdgeType) Edge { return Edge{ID: "x", Type: t, evidence: noEvidence} }`, 0},
		{"no type", `var e = Edge{ID: "x"}`, 1},
		{"elided slice element", `var e = []Edge{{ID: "x", Type: EdgeManages}}`, 1},
		{"elided map value", `var e = map[string]Edge{"a": {ID: "x", Type: EdgeManages}}`, 1},
		{"elided pointer element", `var e = []*Edge{{ID: "x", Type: EdgeManages}}`, 1},
		{"positional fields", `var e = Edge{"x", "a", "b", EdgeManages, "", false, 0, "", ""}`, 2},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, tc.name+".go", "package topology\n"+tc.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got, _ := edgeEvidenceViolations(fset, file); len(got) != tc.violations {
			t.Errorf("%s: %d violations %q, want %d", tc.name, len(got), got, tc.violations)
		}
	}
}

// edgeEvidenceViolations lists the Edge literals in file that break the
// evidence rule, and counts the Edge literals it saw.
func edgeEvidenceViolations(fset *token.FileSet, file *ast.File) ([]string, int) {
	var violations []string
	literals := 0
	inspectWithAncestors(file, func(n ast.Node, ancestors []ast.Node) {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isElementLiteral(lit, ancestors, "Edge") {
			return
		}
		literals++
		var edgeType, evidence ast.Expr
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				violations = append(violations, fmt.Sprintf("%s: Edge literal with positional fields", fset.Position(lit.Pos())))
				break
			}
			switch key, _ := kv.Key.(*ast.Ident); {
			case key == nil:
			case key.Name == "Type":
				edgeType = kv.Value
			case key.Name == "evidence":
				evidence = kv.Value
			}
		}
		constType, isConst := edgeType.(*ast.Ident)
		isConst = isConst && strings.HasPrefix(constType.Name, "Edge")
		switch {
		case edgeType == nil:
			violations = append(violations, fmt.Sprintf("%s: Edge literal without a Type", fset.Position(lit.Pos())))
		case isConst && constType.Name == "EdgeManages" && evidence == nil:
			violations = append(violations, fmt.Sprintf("%s: manages Edge literal does not declare its evidence", fset.Position(lit.Pos())))
		case !isConst && evidence == nil:
			violations = append(violations, fmt.Sprintf("%s: Edge literal of a computed type does not declare its evidence", fset.Position(lit.Pos())))
		case isConst && constType.Name != "EdgeManages" && evidence != nil:
			violations = append(violations, fmt.Sprintf("%s: %s Edge literal declares manages evidence", fset.Position(lit.Pos()), constType.Name))
		}
	})
	return violations, literals
}

// Every built manages edge carries evidence, and no other edge does. A
// synthetic Pod group target is always marked aggregate, and an edge the UI
// hides when an intermediate kind is visible is always a shortcut.
func TestBuiltManagesEdgesCarryEvidence(t *testing.T) {
	for _, tc := range charCases() {
		t.Run(tc.name, func(t *testing.T) {
			topo := charBuild(t, charCluster(), tc.opts())
			manages := 0
			for _, e := range topo.Edges {
				if e.Type != EdgeManages {
					if e.evidence != noEvidence {
						t.Errorf("%s (%s): carries manages evidence %s", e.ID, e.Type, evidenceNames(e.evidence))
					}
					continue
				}
				manages++
				if e.evidence == noEvidence {
					t.Errorf("%s: manages edge without evidence", e.ID)
				}
				if toGroup := strings.HasPrefix(e.Target, "podgroup-"); toGroup != (e.evidence&evidenceAggregate != 0) {
					t.Errorf("%s: aggregate=%v for a target that is a Pod group=%v", e.ID, e.evidence&evidenceAggregate != 0, toGroup)
				}
				if e.SkipIfKindVisible != "" && e.evidence&evidenceShortcut == 0 {
					t.Errorf("%s: hidden when %s is visible but not marked a shortcut", e.ID, e.SkipIfKindVisible)
				}
			}
			if tc.name != "traffic" && manages == 0 {
				t.Fatal("no manages edges; the fixture no longer exercises producers")
			}
		})
	}
}

// The evidence each manages route records. Ownership routes record the
// ownerReference the producer read; routes that read a spec, status, label
// or naming convention record an association; display paths across a hidden
// kind record a shortcut, never the ownership they imply.
var charRouteEvidence = map[charRoute]edgeEvidence{
	{"sse-relationship-cache", "nodepool//default", "nodeclaim//default-x1", ""}:                                              evidenceOwner | evidenceController,
	{"sse-relationship-cache", "nodeclaim//default-x1", "node//node-b", ""}:                                                   evidenceAssociation,
	{"sse-relationship-cache", "capicluster/capi/prod", "kubeadmcontrolplane/capi/prod-cp", ""}:                               evidenceOwner | evidenceController,
	{"sse-relationship-cache", "capicluster/capi/prod", "machinedeployment/capi/prod-md", ""}:                                 evidenceOwner | evidenceController,
	{"sse-relationship-cache", "capicluster/capi/prod", "machinepool/capi/prod-mp", ""}:                                       evidenceOwner | evidenceController,
	{"sse-relationship-cache", "machinedeployment/capi/prod-md", "machineset/capi/prod-md-abc", ""}:                           evidenceOwner | evidenceController,
	{"sse-relationship-cache", "machineset/capi/prod-md-abc", "machine/capi/prod-md-abc-x", ""}:                               evidenceOwner | evidenceController,
	{"sse-relationship-cache", "machine/capi/prod-md-abc-x", "node//node-a", ""}:                                              evidenceAssociation,
	{"sse-relationship-cache", "cronworkflow/team/report", "workflow/team/report-1700", ""}:                                   evidenceOwner | evidenceController,
	{"sse-relationship-cache", "workflow/team/report-1700", "pod/team/report-1700-main", ""}:                                  evidenceAssociation,
	{"sse-relationship-cache", "cronworkflow/team/report", "pod/team/report-1700-main", "Workflow"}:                           evidenceShortcut,
	{"sse-relationship-cache", "cronjob/team/nightly", "job/team/nightly-29001", ""}:                                          evidenceOwner | evidenceController,
	{"sse-relationship-cache", "job/team/nightly-29001", "pod/team/nightly-29001-x", ""}:                                      evidenceOwner | evidenceController,
	{"sse-relationship-cache", "cronjob/team/nightly", "pod/team/nightly-29001-x", "Job"}:                                     evidenceShortcut,
	{"sse-relationship-cache", "scaledjob/team/queue-worker", "job/team/queue-worker-abc", ""}:                                evidenceOwner | evidenceController,
	{"sse-relationship-cache", "scaledjob/team/queue-worker", "pod/team/queue-worker-abc-x", "Job"}:                           evidenceShortcut,
	{"sse-relationship-cache", "deployment/shop/web", "replicaset/shop/web-7d9f8", ""}:                                        evidenceOwner | evidenceController,
	{"sse-relationship-cache", "replicaset/shop/web-7d9f8", "pod/shop/web-7d9f8-0", ""}:                                       evidenceOwner | evidenceController,
	{"sse-relationship-cache", "rollout/shop/checkout", "replicaset/shop/checkout-abc", ""}:                                   evidenceOwner | evidenceController,
	{"sse-relationship-cache", "rollout/shop/checkout", "pod/shop/checkout-abc-a", "ReplicaSet"}:                              evidenceShortcut,
	{"sse-relationship-cache", "rollout/shop/checkout", "analysisrun/shop/checkout-bg-1", ""}:                                 evidenceAssociation,
	{"sse-relationship-cache", "statefulset/team/db", "pod/team/db-0", ""}:                                                    evidenceOwner | evidenceController,
	{"sse-relationship-cache", "daemonset/infra/agent", "pod/infra/agent-node-a", ""}:                                         evidenceOwner | evidenceController,
	{"sse-relationship-cache", "gatewayclass//envoy", "gateway/shop/public", ""}:                                              evidenceBinding,
	{"sse-relationship-cache", "sealedsecret/team/db-creds", "secret/team/db-creds", ""}:                                      evidenceAssociation,
	{"sse-relationship-cache", "certificate/shop/web-tls", "secret/shop/web-tls", ""}:                                         evidenceAssociation,
	{"sse-relationship-cache", "helmrelease/shop/redis", "deployment/shop/redis", ""}:                                         evidenceOriginLabels,
	{"sse-relationship-cache", "helmrelease/shop/redis", "deployment/shop/redis-legacy", ""}:                                  evidenceInstanceLabel,
	{"sse-relationship-cache", "helmrelease/shop/redis", "service/shop/redis", ""}:                                            evidenceOriginLabels,
	{"sse-relationship-cache", "helmrelease/shop/redis", "statefulset/shop/redis-replica", ""}:                                evidenceOriginLabels,
	{"sse-relationship-cache", "helmrelease/shop/redis", "daemonset/shop/redis-exporter", ""}:                                 evidenceOriginLabels,
	{"sse-relationship-cache", "helmrelease/shop/redis", "job/shop/redis-migrate", ""}:                                        evidenceOriginLabels,
	{"sse-relationship-cache", "helmrelease/shop/redis", "cronjob/shop/redis-backup", ""}:                                     evidenceOriginLabels,
	{"sse-relationship-cache", "helmrelease/shop/redis", "rollout/shop/redis-proxy", ""}:                                      evidenceOriginLabels,
	{"sse-relationship-cache", "application/argocd/shop", "deployment/shop/web", ""}:                                          evidenceMembership,
	{"sse-relationship-cache", "application/argocd/shop", "service/shop/web", ""}:                                             evidenceMembership,
	{"sse-relationship-cache", "kustomization/flux-system/shop", "deployment/shop/cart", ""}:                                  evidenceMembership,
	{"sse-relationship-cache", "knativeservice/shop/hello", "knativeconfiguration/shop/hello", ""}:                            evidenceOwner | evidenceController,
	{"sse-relationship-cache", "knativeservice/shop/hello", "knativeroute/shop/hello", ""}:                                    evidenceOwner | evidenceController,
	{"sse-relationship-cache", "knativeconfiguration/shop/hello", "knativerevision/shop/hello-00001", ""}:                     evidenceOwner | evidenceController,
	{"sse-relationship-cache", "knativerevision/shop/hello-00001", "deployment/shop/hello-00001-deployment", ""}:              evidenceOwner | evidenceController,
	{"sse-relationship-cache", "broker/shop/default", "inmemorychannel/shop/default-kne-trigger", ""}:                         evidenceOwner | evidenceController,
	{"sse-relationship-cache", "deployment/shop/web", "widget/shop/web-widget/example.io", ""}:                                evidenceOwner | evidenceController,
	{"sse-relationship-cache", "databaseclaim/shop/orders-db/demo.example.io", "xdatabase//orders-db-xr/demo.example.io", ""}: evidenceAssociation,
	// The XR's resourceRefs and the composed resource's ownerReference link one pair.
	{"sse-relationship-cache", "xdatabase//orders-db-xr/demo.example.io", "object//orders-db-config/kubernetes.crossplane.io", ""}: evidenceAssociation | evidenceOwner | evidenceController,
	{"resources-default", "deployment/shop/web", "pod/shop/web-7d9f8-0", ""}:                                                       evidenceShortcut,
	{"resources-default", "rollout/shop/payments", "pod/shop/payments-6d8f-a", ""}:                                                 evidenceShortcut,
}

func TestManagesRoutesRecordTheirEvidence(t *testing.T) {
	builds := map[string]*Topology{}
	for _, tc := range charCases() {
		builds[tc.name] = charBuild(t, charCluster(), tc.opts())
	}
	for _, route := range charRoutes {
		want, ok := charRouteEvidence[route]
		if !ok {
			t.Errorf("%s: %s -> %s has no expected evidence", route.build, route.source, route.target)
			continue
		}
		for _, edge := range builds[route.build].Edges {
			if edge.Source == route.source && edge.Target == route.target && edge.Type == EdgeManages && edge.SkipIfKindVisible == route.skipIfKindVisible && edge.evidence != want {
				t.Errorf("%s: %s -> %s evidence %s, want %s", route.build, route.source, route.target, evidenceNames(edge.evidence), evidenceNames(want))
			}
		}
	}
	if len(charRouteEvidence) != len(charRoutes) {
		t.Errorf("%d expected evidences for %d routes", len(charRouteEvidence), len(charRoutes))
	}
}

// A shortcut crosses an object the graph does not draw, so the object it
// crosses must be recorded: a shortcut is checked hop by hop against recorded
// controller references, never trusted for the ownership it implies.
func TestShortcutHopsHaveFacts(t *testing.T) {
	for _, tc := range charCases() {
		t.Run(tc.name, func(t *testing.T) {
			topo := charBuild(t, charCluster(), tc.opts())
			nodes := make(map[string]*Node, len(topo.Nodes))
			for i := range topo.Nodes {
				nodes[topo.Nodes[i].ID] = &topo.Nodes[i]
			}
			for _, e := range topo.Edges {
				if e.evidence&evidenceShortcut == 0 {
					continue
				}
				source := nodeObjectKey(nodes[e.Source])
				pods := topo.facts.groupMembers(e.Target)
				if len(pods) == 0 {
					pods = []objectKey{nodeObjectKey(nodes[e.Target])}
				}
				reached := false
				for _, pod := range pods {
					podFacts, _ := topo.facts.object(pod)
					for _, hopRef := range controllerOwners(podFacts) {
						hop := objectKey{group: hopRef.group, kind: hopRef.kind, namespace: pod.namespace, name: hopRef.name}
						hopFacts, ok := topo.facts.object(hop)
						if !ok {
							t.Errorf("%s: crosses %+v, which has no facts", e.ID, hop)
							continue
						}
						for _, ref := range controllerOwners(hopFacts) {
							reached = reached || (objectKey{group: ref.group, kind: ref.kind, namespace: hop.namespace, name: ref.name}) == source
						}
					}
				}
				if !reached {
					t.Errorf("%s: no recorded controller chain from %+v reaches the target", e.ID, source)
				}
			}
		})
	}
}

func controllerOwners(facts objectFacts) []ownerFact {
	var owners []ownerFact
	for _, owner := range facts.owners {
		if owner.controller {
			owners = append(owners, owner)
		}
	}
	return owners
}

func TestHelmReleaseLabelEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   edgeEvidence
	}{
		{"both origin labels", map[string]string{fluxHelmNameLabel: "redis", fluxHelmNSLabel: "shop"}, evidenceOriginLabels},
		{"name label only", map[string]string{fluxHelmNameLabel: "redis"}, evidencePartialOriginLabel},
		{"origin labels win over instance", map[string]string{fluxHelmNameLabel: "redis", fluxHelmNSLabel: "shop", "app.kubernetes.io/instance": "redis"}, evidenceOriginLabels},
		{"instance label only", map[string]string{"app.kubernetes.io/instance": "redis"}, evidenceInstanceLabel},
		{"origin labels name another namespace", map[string]string{fluxHelmNameLabel: "redis", fluxHelmNSLabel: "other"}, noEvidence},
		{"another namespace but matching instance", map[string]string{fluxHelmNameLabel: "redis", fluxHelmNSLabel: "other", "app.kubernetes.io/instance": "redis"}, evidenceInstanceLabel},
		{"another release", map[string]string{fluxHelmNameLabel: "cache", fluxHelmNSLabel: "shop"}, noEvidence},
		{"no labels", nil, noEvidence},
	} {
		if got := helmReleaseLabelEvidence(tc.labels, "redis", "shop"); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, evidenceNames(got), evidenceNames(tc.want))
		}
	}
}

func TestNodePoolClaimEvidence(t *testing.T) {
	controller := true
	claim := &unstructured.Unstructured{}
	claim.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "karpenter.sh/v1", Kind: "NodePool", Name: "default", Controller: &controller}})
	if got := nodePoolClaimEvidence(claim, "default", karpenter.RelationshipOwnerRef); got != evidenceOwner|evidenceController {
		t.Errorf("controller owner: %s", evidenceNames(got))
	}
	claim.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "karpenter.sh/v1", Kind: "NodePool", Name: "default"}})
	if got := nodePoolClaimEvidence(claim, "default", karpenter.RelationshipOwnerRef); got != evidenceOwner {
		t.Errorf("non-controller owner: %s", evidenceNames(got))
	}
	if got := nodePoolClaimEvidence(claim, "default", karpenter.RelationshipLabel); got != evidenceAssociation {
		t.Errorf("label: %s", evidenceNames(got))
	}
}

// Two producers that link the same pair produce one edge carrying both
// evidences, never a second edge and never a lost evidence.
func TestDuplicateManagesPairsMergeEvidence(t *testing.T) {
	t.Run("generic owner reference", func(t *testing.T) {
		f := charCluster()
		deployment := Node{ID: "deployment/shop/web", Kind: KindDeployment, Name: "web", Data: map[string]any{"namespace": "shop", "apiVersion": "apps/v1"}}
		widgetID := "widget/shop/web-widget/example.io"
		prior := Edge{ID: deployment.ID + "-to-" + widgetID, Source: deployment.ID, Target: widgetID, Type: EdgeManages, evidence: evidenceAssociation}
		_, edges := NewBuilder(f.provider).WithDynamic(f.dynamic).addGenericCRDNodes([]Node{deployment}, []Edge{prior}, DefaultBuildOptions())
		var merged []Edge
		for _, e := range edges {
			if e.Source == deployment.ID && e.Target == widgetID {
				merged = append(merged, e)
			}
		}
		if len(merged) != 1 || merged[0].evidence != evidenceAssociation|evidenceOwner|evidenceController {
			t.Fatalf("edges for the pair = %+v, want one carrying association and the owner reference", merged)
		}
	})

	t.Run("gitops membership", func(t *testing.T) {
		app := charUnstructured("argoproj.io/v1alpha1", "Application", "argocd", "shop", nil,
			map[string]any{"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "shop"}},
			map[string]any{"resources": []any{
				map[string]any{"group": "apps", "kind": "Deployment", "namespace": "shop", "name": "web"},
				map[string]any{"kind": "Service", "namespace": "shop", "name": "web"},
			}})
		nodes := []Node{
			{ID: "deployment/shop/web", Kind: KindDeployment, Name: "web", Data: map[string]any{"namespace": "shop", "apiVersion": "apps/v1"}},
			{ID: "service/shop/web", Kind: KindService, Name: "web", Data: map[string]any{"namespace": "shop", "apiVersion": "v1"}},
		}
		appID := "application/argocd/shop"
		prior := []Edge{
			{ID: appID + "-to-deployment/shop/web", Source: appID, Target: "deployment/shop/web", Type: EdgeManages, evidence: evidenceAssociation},
			{ID: appID + "-to-service/shop/web", Source: appID, Target: "service/shop/web", Type: EdgeConfigures},
		}
		edges := addGitOpsManagedResourceEdges(nil, nodes, prior, []*unstructured.Unstructured{app},
			map[string]string{"argocd/shop": appID}, nil, nil)
		if len(edges) != 2 {
			t.Fatalf("edges = %+v, want the two prior edges and no new one", edges)
		}
		if edges[0].Type != EdgeManages || edges[0].evidence != evidenceAssociation|evidenceMembership {
			t.Errorf("manages pair = %+v, want association and membership", edges[0])
		}
		if edges[1].Type != EdgeConfigures || edges[1].evidence != noEvidence {
			t.Errorf("configures edge with the same ID = %+v, want it untouched", edges[1])
		}
	})
}

// A pair the graph keeps as two edges (one Job naming its CronJob's current
// and previous incarnations) keeps each record's own evidence.
func TestDuplicatePublicPairsKeepTheirOwnEvidence(t *testing.T) {
	topo := charBuild(t, charCluster(), charSSEOptions())
	var got []edgeEvidence
	for _, e := range topo.Edges {
		if e.Source == "cronjob/team/nightly" && e.Target == "job/team/nightly-28999" && e.Type == EdgeManages {
			got = append(got, e.evidence)
		}
	}
	slices.Sort(got)
	if want := []edgeEvidence{evidenceOwner, evidenceOwner | evidenceController}; !slices.Equal(got, want) {
		t.Fatalf("duplicate pair evidence %v, want one controller and one plain owner record", got)
	}
}

// Recorded membership is the manager's whole member list in exact identities,
// whatever kinds the graph draws, with the three states kept apart: a list
// that is empty is not a list that is missing, a list naming another
// cluster's objects matches nothing here, and a list with an entry that names
// no object is not trusted to be complete.
func TestGitOpsMembershipFacts(t *testing.T) {
	inCluster := map[string]any{"server": "https://kubernetes.default.svc", "namespace": "shop"}
	remote := map[string]any{"server": "https://prod.example.com", "namespace": "shop"}
	argoApp := func(name string, destination map[string]any, status map[string]any) *unstructured.Unstructured {
		return charUnstructured("argoproj.io/v1alpha1", "Application", "argocd", name, nil, map[string]any{"destination": destination}, status)
	}
	kustomization := func(name string, spec, status map[string]any) *unstructured.Unstructured {
		return charUnstructured("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", name, nil, spec, status)
	}
	apps := []*unstructured.Unstructured{
		argoApp("shop", inCluster, map[string]any{"resources": []any{
			map[string]any{"group": "apps", "kind": "Deployment", "namespace": "shop", "name": "web"},
			map[string]any{"kind": "ConfigMap", "name": "settings"},
			map[string]any{"kind": "Namespace", "name": "shop"},
		}}),
		argoApp("nameless", inCluster, map[string]any{"resources": []any{
			map[string]any{"group": "apps", "kind": "Deployment", "namespace": "shop", "name": "web"},
			map[string]any{"kind": "ConfigMap"},
		}}),
		argoApp("garbled", inCluster, map[string]any{"resources": []any{
			map[string]any{"kind": "ConfigMap", "namespace": int64(7), "name": "settings"},
		}}),
		argoApp("empty", inCluster, map[string]any{"resources": []any{}}),
		argoApp("new", inCluster, nil),
		argoApp("elsewhere", remote, map[string]any{"resources": []any{
			map[string]any{"group": "apps", "kind": "Deployment", "namespace": "shop", "name": "web"},
		}}),
	}
	kustomizations := []*unstructured.Unstructured{
		kustomization("apps", nil, map[string]any{"inventory": map[string]any{"entries": []any{
			map[string]any{"id": "shop_cart_apps_Deployment", "v": "v1"},
			map[string]any{"id": "shop_cart__ConfigMap", "v": "v1"},
			map[string]any{"id": "_shop__Namespace", "v": "v1"},
		}}}),
		kustomization("partly", nil, map[string]any{"inventory": map[string]any{"entries": []any{
			map[string]any{"id": "shop_cart_apps_Deployment", "v": "v1"},
			map[string]any{"id": "malformed", "v": "v1"},
		}}}),
		kustomization("unreadable", nil, map[string]any{"inventory": map[string]any{"entries": "malformed"}}),
		kustomization("empty", nil, map[string]any{"inventory": map[string]any{}}),
		kustomization("new", nil, nil),
		kustomization("elsewhere", map[string]any{"kubeConfig": map[string]any{"secretRef": map[string]any{"name": "prod"}}},
			map[string]any{"inventory": map[string]any{"entries": []any{map[string]any{"id": "shop_cart_apps_Deployment", "v": "v1"}}}}),
	}
	capture := newFactsCapture(0)
	addGitOpsManagedResourceEdges(capture, nil, nil, apps,
		map[string]string{"argocd/shop": "application/argocd/shop"},
		kustomizations, nil)
	registry := capture.freeze()

	argo := func(name string) objectKey {
		return objectKey{group: "argoproj.io", kind: "Application", namespace: "argocd", name: name}
	}
	flux := func(name string) objectKey {
		return objectKey{group: "kustomize.toolkit.fluxcd.io", kind: "Kustomization", namespace: "flux-system", name: name}
	}
	for _, tc := range []struct {
		manager objectKey
		state   membershipState
		members []objectKey
	}{
		{argo("shop"), membershipRecorded, []objectKey{
			{group: "apps", kind: "Deployment", namespace: "shop", name: "web"},
			{kind: "ConfigMap", name: "settings"},
			{kind: "ConfigMap", namespace: "shop", name: "settings"},
			{kind: "Namespace", name: "shop"},
			{kind: "Namespace", namespace: "shop", name: "shop"},
		}},
		{argo("empty"), membershipRecorded, nil},
		{argo("nameless"), membershipAbsent, nil},
		{argo("garbled"), membershipAbsent, nil},
		{argo("new"), membershipAbsent, nil},
		{argo("elsewhere"), membershipRemote, nil},
		{flux("apps"), membershipRecorded, []objectKey{
			{group: "apps", kind: "Deployment", namespace: "shop", name: "cart"},
			{kind: "ConfigMap", namespace: "shop", name: "cart"},
			{kind: "Namespace", name: "shop"},
		}},
		{flux("empty"), membershipRecorded, nil},
		{flux("partly"), membershipAbsent, nil},
		{flux("unreadable"), membershipAbsent, nil},
		{flux("new"), membershipAbsent, nil},
		{flux("elsewhere"), membershipRemote, nil},
		{argo("unknown"), membershipAbsent, nil},
	} {
		got := registry.membership(tc.manager)
		if got.state != tc.state {
			t.Errorf("%s/%s: state %d, want %d", tc.manager.kind, tc.manager.name, got.state, tc.state)
		}
		if len(got.members) != len(tc.members) {
			t.Errorf("%s/%s: members %+v, want %+v", tc.manager.kind, tc.manager.name, got.members, tc.members)
		}
		for _, member := range tc.members {
			if _, ok := got.members[member]; !ok {
				t.Errorf("%s/%s: missing member %+v", tc.manager.kind, tc.manager.name, member)
			}
		}
	}
}

func evidenceNames(e edgeEvidence) string {
	if e == noEvidence {
		return "none"
	}
	names := []string{"owner", "controller", "association", "binding", "membership", "origin-labels", "partial-origin-label", "instance-label", "shortcut", "aggregate"}
	var set []string
	for i, name := range names {
		if e&(1<<i) != 0 {
			set = append(set, name)
		}
	}
	if rest := e &^ (1<<len(names) - 1); rest != 0 {
		set = append(set, "unknown")
	}
	return strings.Join(set, "|")
}

// A HelmRelease applied to another cluster is recorded as remote, so a claim
// naming it can be rejected; a local one, whose inventory Radar does not
// read, stays absent.
func TestRemoteHelmReleaseIsRecorded(t *testing.T) {
	d := newCharDynamic()
	local := charUnstructured("helm.toolkit.fluxcd.io/v2", "HelmRelease", "shop", "local", nil, map[string]any{"chart": map[string]any{}}, nil)
	remote := charUnstructured("helm.toolkit.fluxcd.io/v2", "HelmRelease", "shop", "remote", nil,
		map[string]any{"chart": map[string]any{}, "kubeConfig": map[string]any{"secretRef": map[string]any{"name": "prod"}}}, nil)
	d.add("helm.toolkit.fluxcd.io", "v2", "HelmRelease", "helmreleases", true, local, remote)
	topo, err := NewBuilder(&mockProvider{}).WithDynamic(d).Build(DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := topo.facts.membership(unstructuredKey(remote)).state; got != membershipRemote {
		t.Errorf("remote HelmRelease membership state %d, want remote", got)
	}
	if got := topo.facts.membership(unstructuredKey(local)).state; got != membershipAbsent {
		t.Errorf("local HelmRelease membership state %d, want absent", got)
	}
}

// A Pod group's owner edge carries the evidence of every member's reference,
// whichever member happens to represent the group.
func TestPodGroupEdgeCarriesEveryMembersEvidence(t *testing.T) {
	ds := &appsv1.DaemonSet{ObjectMeta: charMeta("infra", "agent", nil)}
	plain := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "agent", UID: ds.UID}
	pods := func(plainFirst bool) []*corev1.Pod {
		var pods []*corev1.Pod
		for i := 0; i < 6; i++ {
			ref := charController("apps/v1", "DaemonSet", "agent", string(ds.UID))
			if (i == 0) == plainFirst {
				ref = plain
			}
			pods = append(pods, charPod("infra", fmt.Sprintf("agent-%d", i), "node-a", map[string]string{"app": "agent"}, charPodSpec("", ""), ref))
		}
		return pods
	}
	for _, plainFirst := range []bool{true, false} {
		topo, err := NewBuilder(&mockProvider{daemonSets: []*appsv1.DaemonSet{ds}, pods: pods(plainFirst)}).Build(DefaultBuildOptions())
		if err != nil {
			t.Fatal(err)
		}
		var got []edgeEvidence
		for _, e := range topo.Edges {
			if e.Source == "daemonset/infra/agent" && strings.HasPrefix(e.Target, "podgroup-") {
				got = append(got, e.evidence)
			}
		}
		if want := []edgeEvidence{evidenceOwner | evidenceController | evidenceAggregate}; !slices.Equal(got, want) {
			t.Errorf("plain reference first=%v: group edge evidence %v, want %v", plainFirst, got, want)
		}
	}
}

// Knative dedupes channel edges by ID; a second reference to the same owner
// must add its evidence to the edge rather than vanish.
func TestKnativeChannelOwnerEvidenceMerges(t *testing.T) {
	broker := Node{ID: "broker/shop/default", Kind: "Broker", Name: "default", Data: map[string]any{"namespace": "shop", "apiVersion": "eventing.knative.dev/v1"}}
	channel := Node{ID: "inmemorychannel/shop/default-kne", Kind: "InMemoryChannel", Name: "default-kne", Data: map[string]any{"namespace": "shop", "apiVersion": "messaging.knative.dev/v1"}}
	obj := charUnstructured("messaging.knative.dev/v1", "InMemoryChannel", "shop", "default-kne", nil, nil, nil,
		metav1.OwnerReference{APIVersion: "eventing.knative.dev/v1", Kind: "Broker", Name: "default", UID: "previous"},
		charController("eventing.knative.dev/v1", "Broker", "default", "current"))
	edges := addKnativeReferenceEdges([]Node{broker, channel}, nil, nil, nil, []*unstructured.Unstructured{obj})
	if len(edges) != 1 || edges[0].Type != EdgeManages || edges[0].evidence != evidenceOwner|evidenceController {
		t.Fatalf("channel edges = %+v; want one manages edge carrying both references' evidence", edges)
	}
}

// Discovery's scope for a custom owner kind is recorded at build time, so an
// owner the build never observed still resolves to an exact identity.
func TestCustomOwnerKindScopeIsRecorded(t *testing.T) {
	for _, namespaced := range []bool{true, false} {
		d := newCharDynamic()
		d.disc.AddAPIResource(k8score.APIResource{Group: "example.io", Version: "v1", Kind: "Widget", Name: "widgets", Namespaced: namespaced, IsCRD: true, Verbs: []string{"get", "list", "watch"}})
		deploy := &appsv1.Deployment{ObjectMeta: charMeta("shop", "web", nil, charController("example.io/v1", "Widget", "owner", "uid-widget"))}
		topo, err := NewBuilder(&mockProvider{deployments: []*appsv1.Deployment{deploy}}).WithDynamic(d).Build(DefaultBuildOptions())
		if err != nil {
			t.Fatal(err)
		}
		if got, known := topo.facts.scopes[resourceid.GroupKind{Group: "example.io", Kind: "Widget"}]; !known || got != namespaced {
			t.Errorf("Widget scope = %v (known %v), want namespaced=%v", got, known, namespaced)
		}
	}
}

// A custom Kind that equals another resource's plural keeps its own scope,
// whichever of the two discovery listed first.
func TestCustomOwnerKindScopeSurvivesPluralCollisions(t *testing.T) {
	widget := k8score.APIResource{Group: "example.io", Version: "v1", Kind: "Widget", Name: "widgets", Namespaced: true, IsCRD: true, Verbs: []string{"get", "list", "watch"}}
	widgets := k8score.APIResource{Group: "example.io", Version: "v1", Kind: "Widgets", Name: "widgetrecords", Namespaced: false, IsCRD: true, Verbs: []string{"get", "list", "watch"}}
	for _, order := range [][]k8score.APIResource{{widget, widgets}, {widgets, widget}} {
		d := newCharDynamic()
		for _, res := range order {
			d.disc.AddAPIResource(res)
		}
		deploy := &appsv1.Deployment{ObjectMeta: charMeta("shop", "web", nil, charController("example.io/v1", "Widgets", "owner", "uid-owner"))}
		topo, err := NewBuilder(&mockProvider{deployments: []*appsv1.Deployment{deploy}}).WithDynamic(d).Build(DefaultBuildOptions())
		if err != nil {
			t.Fatal(err)
		}
		if namespaced, known := topo.facts.scopes[resourceid.GroupKind{Group: "example.io", Kind: "Widgets"}]; !known || namespaced {
			t.Errorf("%s listed first: Widgets scope namespaced=%v known=%v, want cluster-scoped", order[0].Kind, namespaced, known)
		}
	}
}

// Membership is read from the manager itself, not from how the graph draws
// it: an Application with no node still records its destination-defaulted
// members.
func TestUndrawnApplicationRecordsDefaultedMembers(t *testing.T) {
	app := charUnstructured("argoproj.io/v1alpha1", "Application", "argocd", "undrawn", nil,
		map[string]any{"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "shop"}},
		map[string]any{"resources": []any{map[string]any{"kind": "ConfigMap", "name": "settings"}}})
	capture := newFactsCapture(0)
	addGitOpsManagedResourceEdges(capture, nil, nil, []*unstructured.Unstructured{app}, nil, nil, nil)
	members := capture.freeze().membership(unstructuredKey(app)).members
	for _, want := range []objectKey{{kind: "ConfigMap", name: "settings"}, {kind: "ConfigMap", namespace: "shop", name: "settings"}} {
		if _, ok := members[want]; !ok {
			t.Errorf("members %v lack %+v", members, want)
		}
	}
}
