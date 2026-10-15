package topology

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skyhook-io/radar/pkg/resourceid"
)

// nodeConstructors build a Node from an object without recording facts; every
// call to one must itself be wrapped, so the wrapping decision stays at the
// producer that knows which object it holds.
var nodeConstructors = map[string]bool{
	"CreatePodNode":              true,
	"CreatePodGroupNode":         true,
	"CreateOrphanPodSummaryNode": true,
	"configMapNode":              true,
	"secretNode":                 true,
}

// Every node a producer builds must say where its facts come from: from the
// object it was built from (observedNode) or from no single object
// (syntheticNode). A new producer that appends a bare Node literal would
// silently leave its objects without incarnation or ownership facts.
func TestEveryNodeLiteralDeclaresItsFacts(t *testing.T) {
	fset, files := parsePackageSources(t)
	for _, file := range files {
		for _, violation := range nodeFactsViolations(fset, file) {
			t.Error(violation)
		}
	}
}

// The checker is syntactic: it sees Node literals, including elided elements
// of []Node, []*Node and map[...]Node literals, and every use of a
// constructor in nodeConstructors. A Node assembled field by field from its
// zero value is invisible to it; TestObjectFactsCoverEveryBuiltObject catches
// such a node at runtime when a fixture object builds it.
func TestNodeFactsCheckerCatchesBypasses(t *testing.T) {
	for _, tc := range []struct {
		name, src  string
		violations int
	}{
		{"bare literal", `func f() Node { return Node{ID: "x"} }`, 1},
		{"observed literal", `func f(c *factsCapture, o metav1.Object) Node { return observedNode(c, o, Node{ID: "x"}) }`, 0},
		{"synthetic literal", `func f() Node { return syntheticNode(Node{ID: "x"}) }`, 0},
		{"pointer literal", `var n = &Node{ID: "x"}`, 1},
		{"elided slice element", `var n = []Node{{ID: "x"}}`, 1},
		{"elided keyed slice element", `var n = []Node{0: {ID: "x"}}`, 1},
		{"elided map value", `var n = map[string]Node{"a": {ID: "x"}}`, 1},
		{"elided pointer element", `var n = []*Node{{ID: "x"}}`, 1},
		{"unwrapped constructor", `func f() Node { return CreatePodNode(nil, nil, true) }`, 1},
		{"wrapped constructor", `func f() Node { return syntheticNode(CreatePodNode(nil, nil, true)) }`, 0},
		{"constructor as a value", `var f = CreatePodNode`, 1},
		{"constructor body", `func configMapNode() Node { return Node{ID: "x"} }`, 0},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, tc.name+".go", "package topology\n"+tc.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := nodeFactsViolations(fset, file); len(got) != tc.violations {
			t.Errorf("%s: %d violations %q, want %d", tc.name, len(got), got, tc.violations)
		}
	}
}

// parsePackageSources parses this package's non-test sources.
func parsePackageSources(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no package sources found")
	}
	return fset, files
}

// inspectWithAncestors walks root, passing each node its ancestors, nearest
// last.
func inspectWithAncestors(root ast.Node, visit func(n ast.Node, ancestors []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		visit(n, stack)
		stack = append(stack, n)
		return true
	})
}

func nodeFactsViolations(fset *token.FileSet, file *ast.File) []string {
	var violations []string
	for _, decl := range file.Decls {
		fn, _ := decl.(*ast.FuncDecl)
		inspectWithAncestors(decl, func(n ast.Node, ancestors []ast.Node) {
			parent := nearest(ancestors, 0)
			switch x := n.(type) {
			case *ast.CompositeLit:
				if !isElementLiteral(x, ancestors, "Node") || wrappedByFactsHelper(parent, x) {
					return
				}
				if _, isReturn := parent.(*ast.ReturnStmt); isReturn && fn != nil && nodeConstructors[fn.Name.Name] {
					return
				}
				violations = append(violations, fmt.Sprintf("%s: Node literal is not wrapped in observedNode or syntheticNode", fset.Position(x.Pos())))
			case *ast.Ident:
				if !nodeConstructors[x.Name] {
					return
				}
				if call, ok := parent.(*ast.CallExpr); ok && call.Fun == x {
					if !wrappedByFactsHelper(nearest(ancestors, 1), call) {
						violations = append(violations, fmt.Sprintf("%s: %s result is not wrapped in observedNode or syntheticNode", fset.Position(x.Pos()), x.Name))
					}
					return
				}
				if decl, ok := parent.(*ast.FuncDecl); ok && decl.Name == x {
					return
				}
				violations = append(violations, fmt.Sprintf("%s: %s is used as a value, so its results escape the wrapping check", fset.Position(x.Pos()), x.Name))
			}
		})
	}
	return violations
}

// nearest returns the ancestor depth levels above the innermost one.
func nearest(ancestors []ast.Node, depth int) ast.Node {
	if i := len(ancestors) - 1 - depth; i >= 0 {
		return ancestors[i]
	}
	return nil
}

// isElementLiteral reports a composite literal of the named type: written
// out, or elided as an element or value of a slice, array or map of that type
// or of pointers to it.
func isElementLiteral(lit *ast.CompositeLit, ancestors []ast.Node, typeName string) bool {
	if id, ok := lit.Type.(*ast.Ident); ok {
		return id.Name == typeName
	}
	if lit.Type != nil {
		return false
	}
	container, ok := nearest(ancestors, 0).(*ast.CompositeLit)
	if kv, isKeyed := nearest(ancestors, 0).(*ast.KeyValueExpr); isKeyed && kv.Value == lit {
		container, ok = nearest(ancestors, 1).(*ast.CompositeLit)
	}
	if !ok {
		return false
	}
	var elem ast.Expr
	switch t := container.Type.(type) {
	case *ast.ArrayType:
		elem = t.Elt
	case *ast.MapType:
		elem = t.Value
	}
	if star, ok := elem.(*ast.StarExpr); ok {
		elem = star.X
	}
	id, ok := elem.(*ast.Ident)
	return ok && id.Name == typeName
}

func wrappedByFactsHelper(parent ast.Node, child ast.Expr) bool {
	call, ok := parent.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || (id.Name != "observedNode" && id.Name != "syntheticNode") {
		return false
	}
	return len(call.Args) > 0 && call.Args[len(call.Args)-1] == child
}

// keyOf turns a resourceid.ResourceKey string back into the identity it encodes.
func keyOf(resourceKey string) objectKey {
	parts := strings.SplitN(resourceKey, "|", 4)
	return objectKey{group: parts[0], kind: parts[1], namespace: parts[2], name: parts[3]}
}

type charObservation struct {
	uid    types.UID
	owners int
}

// charGroundTruth indexes every fixture object by the exact key a node built
// from it is addressed by.
func charGroundTruth(f *charFixture) map[string]charObservation {
	truth := map[string]charObservation{}
	for _, o := range f.objects {
		truth[o.key()] = charObservation{uid: o.obj.GetUID(), owners: len(o.obj.GetOwnerReferences())}
	}
	return truth
}

// Every node built from a fixture object carries that object's facts under the
// keys IndexByResource addresses it by; synthetic nodes carry none.
func TestObjectFactsCoverEveryBuiltObject(t *testing.T) {
	for _, tc := range charCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := charCluster()
			truth := charGroundTruth(f)
			topo := charBuild(t, f, tc.opts())
			if topo.facts == nil {
				t.Fatal("built topology has no facts registry")
			}
			observedNodes := 0
			for i := range topo.Nodes {
				node := &topo.Nodes[i]
				for _, key := range nodeResourceKeys(node) {
					want, isObject := truth[key]
					got, hasFacts := topo.facts.object(keyOf(key))
					switch {
					case isObject && !hasFacts:
						t.Errorf("%s (%s): built from an object but has no facts", node.ID, key)
					case isObject && (got.uid != want.uid || len(got.owners) != want.owners):
						t.Errorf("%s: facts uid=%q owners=%d, object uid=%q owners=%d", node.ID, got.uid, len(got.owners), want.uid, want.owners)
					case !isObject && hasFacts:
						t.Errorf("%s (%s): synthetic node has facts", node.ID, key)
					case isObject:
						observedNodes++
					}
				}
				if node.Kind == KindPodGroup && !strings.HasPrefix(node.ID, "podgroup-orphans-") {
					members := topo.facts.groupMembers(node.ID)
					if len(members) == 0 {
						t.Errorf("%s: group records no members", node.ID)
					}
					for _, key := range members {
						member, hasFacts := topo.facts.object(key)
						if want, ok := truth[resourceid.ResourceKey(key.group, key.kind, key.namespace, key.name)]; !ok || !hasFacts || member.uid != want.uid {
							t.Errorf("%s: member %+v lacks its object's facts", node.ID, key)
						}
					}
				}
			}
			if observedNodes == 0 {
				t.Fatal("no observed nodes; the fixture no longer exercises capture")
			}
		})
	}
}

func TestObjectFactsRecordOwnersClaimsAndLifecycle(t *testing.T) {
	controller, block := true, true
	deleted := metav1.Now()
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team", Name: "api", UID: "deploy-uid", DeletionTimestamp: &deleted,
		Finalizers: []string{"foregroundDeletion", "example.com/cleanup"},
		Labels: map[string]string{
			"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system",
			"helm.toolkit.fluxcd.io/name": "api", "argocd.argoproj.io/instance": "team-api",
		},
		Annotations: map[string]string{
			"argocd.argoproj.io/tracking-id": "team-api:apps/Deployment:team/api", "argocd.argoproj.io/installation-id": "prod",
			"meta.helm.sh/release-name": "api", "meta.helm.sh/release-namespace": "team", "helm.sh/hook": "post-install",
		},
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "example.io/v1", Kind: "App", Name: "api", UID: "app-uid", Controller: &controller, BlockOwnerDeletion: &block},
			{APIVersion: "v1", Kind: "ConfigMap", Name: "api-lock", UID: "cm-uid"},
		},
	}}
	facts := factsFor(deploy)
	if facts.uid != "deploy-uid" || !facts.deleting || !facts.foreground || facts.orphan {
		t.Fatalf("lifecycle facts = %+v", facts)
	}
	wantOwners := []ownerFact{
		{group: "example.io", kind: "App", name: "api", uid: "app-uid", controller: true, blockOwnerDeletion: true},
		{group: "", kind: "ConfigMap", name: "api-lock", uid: "cm-uid"},
	}
	if len(facts.owners) != len(wantOwners) || facts.owners[0] != wantOwners[0] || facts.owners[1] != wantOwners[1] {
		t.Fatalf("owners = %+v", facts.owners)
	}
	want := claimFacts{
		fluxKustomizationName: "apps", fluxKustomizationNamespace: "flux-system", fluxHelmReleaseName: "api",
		argoTrackingID: "team-api:apps/Deployment:team/api", argoInstallationID: "prod", argoInstance: "team-api",
		helmReleaseName: "api", helmReleaseNamespace: "team", helmHook: true,
	}
	if facts.claims == nil || *facts.claims != want {
		t.Fatalf("claims = %+v", facts.claims)
	}

	// Facts are copies: changing the informer object afterwards changes nothing.
	deploy.OwnerReferences[0].Name = "mutated"
	deploy.Labels["kustomize.toolkit.fluxcd.io/name"] = "mutated"
	if facts.owners[0].name != "api" || facts.claims.fluxKustomizationName != "apps" {
		t.Fatal("facts alias the informer object")
	}
	if plain := factsFor(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", UID: "p"}}); plain.claims != nil || plain.owners != nil {
		t.Fatalf("an object with no claims or owners records empty facts, got %+v", plain)
	}
	if orphaned := factsFor(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"orphan"}}}); !orphaned.orphan || orphaned.deleting {
		t.Fatalf("orphan finalizer without deletion = %+v", orphaned)
	}
}

func TestObservingTwoIncarnationsUnderOneKeyIsConflicted(t *testing.T) {
	c := newFactsCapture(0)
	key := objectKey{kind: "ConfigMap", namespace: "a", name: "c"}
	first := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "c", UID: "one"}}
	c.observe(key, first)
	c.observe(key, first)
	if c.objects[key].conflicted {
		t.Fatal("the same incarnation observed twice is not a conflict")
	}
	c.observe(key, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "c", UID: "two"}})
	if facts, _ := c.freeze().object(key); !facts.conflicted {
		t.Fatal("two incarnations under one key must be marked conflicted")
	}
	var nilCapture *factsCapture
	nilCapture.observe(key, first)
	if got := observedNode(nilCapture, first, Node{ID: "x"}); got.ID != "x" || nilCapture.freeze() != nil {
		t.Fatal("a nil capture must record nothing and return the node")
	}
}

// A policy served through both Calico API groups is one node with two
// identities; each alias records its own observation.
func TestCalicoAliasesRecordTheirOwnObservations(t *testing.T) {
	d := calicoTestDynamicForBoth()
	for gvr, objs := range d.resources {
		for _, obj := range objs {
			obj.SetUID(types.UID(gvr.Group + "/" + obj.GetName()))
		}
	}
	provider := &calicoTestProvider{mockProvider: &mockProvider{}}
	topo, err := NewBuilder(provider).WithDynamic(d).Build(DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for i := range topo.Nodes {
		keys := nodeResourceKeys(&topo.Nodes[i])
		if len(keys) < 2 {
			continue
		}
		for _, key := range keys {
			group := key[:strings.Index(key, "|")]
			facts, ok := topo.facts.object(keyOf(key))
			if !ok || !strings.HasPrefix(string(facts.uid), group+"/") {
				t.Errorf("%s alias %s facts = %+v", topo.Nodes[i].ID, key, facts)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("fixture produced no multi-identity Calico node")
	}
}

// The registry is published with the graph: projections share it and nothing
// that filters or copies a graph changes it.
func TestFactsAreSharedByProjectionsAndReadOnly(t *testing.T) {
	topo := charBuild(t, charCluster(), charSSEOptions())
	before := len(topo.facts.objects)
	clone := *topo
	clone.Nodes = append([]Node(nil), topo.Nodes...)
	clone.Edges = append([]Edge(nil), topo.Edges...)
	clone.StripNodeKinds(map[NodeKind]bool{KindPod: true, KindSecret: true})
	clone.StripNodeIDs(map[string]bool{"deployment/shop/web": true})
	if clone.facts != topo.facts || len(topo.facts.objects) != before {
		t.Fatal("projections must share the registry without changing it")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx := IndexByResource(&clone)
			for j := range clone.Nodes {
				_, _ = clone.facts.object(nodeObjectKey(&clone.Nodes[j]))
			}
			_ = idx
		}()
	}
	wg.Wait()
}

// nodeObjectKey must address a node exactly as IndexByResource does, or facts
// recorded at build time could not be found from an indexed node.
func TestNodeObjectKeyMatchesIndexIdentity(t *testing.T) {
	for _, tc := range charCases() {
		topo := charBuild(t, charCluster(), tc.opts())
		for i := range topo.Nodes {
			node := &topo.Nodes[i]
			key := nodeObjectKey(node)
			if keys := nodeResourceKeys(node); len(keys) > 0 && keys[0] != resourceid.ResourceKey(key.group, key.kind, key.namespace, key.name) {
				t.Errorf("%s: %+v != %s", node.ID, key, keys[0])
			}
		}
	}
}

// Building outside Build (a test driving one producer) records nothing.
func TestProducersRunWithoutACapture(t *testing.T) {
	b := NewBuilder(&mockProvider{})
	graph := &Topology{}
	b.addReflectionRelationships(graph, DefaultBuildOptions())
	if graph.facts != nil {
		t.Fatal("no capture outside Build")
	}
	_, _, _ = addFluxSourceEdges(nil, nil, nil, []*unstructured.Unstructured{}, nil, DefaultBuildOptions())
}

// A drawn Pod's ownership passes through its ReplicaSet whether or not the
// ReplicaSet is drawn, and through a ReplicaSet already scaled to zero while
// its Pods terminate. Those ReplicaSets are observed; one no drawn Pod names
// is not.
func TestReplicaSetHopsAreObserved(t *testing.T) {
	one, zero := int32(1), int32(0)
	deploy := &appsv1.Deployment{ObjectMeta: charMeta("shop", "web", nil)}
	owned := func(name string, replicas *int32) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: charMeta("shop", name, nil, charController("apps/v1", "Deployment", "web", string(deploy.UID))),
			Spec:       appsv1.ReplicaSetSpec{Replicas: replicas},
		}
	}
	current, scaledDown, history := owned("web-new", &one), owned("web-old", &zero), owned("web-ancient", &zero)
	provider := &mockProvider{
		deployments: []*appsv1.Deployment{deploy},
		replicaSets: []*appsv1.ReplicaSet{current, scaledDown, history},
		pods: []*corev1.Pod{
			charPod("shop", "web-new-a", "node-a", nil, charPodSpec("", ""), charController("apps/v1", "ReplicaSet", "web-new", string(current.UID))),
			charPod("shop", "web-old-a", "node-a", nil, charPodSpec("", ""), charController("apps/v1", "ReplicaSet", "web-old", string(scaledDown.UID))),
		},
	}
	for _, mode := range []struct {
		includeReplicaSets, summary bool
	}{{false, false}, {true, false}, {true, true}} {
		includeReplicaSets := mode.includeReplicaSets
		opts := DefaultBuildOptions()
		opts.IncludeReplicaSets = includeReplicaSets
		opts.SummaryMode = mode.summary
		topo, err := NewBuilder(provider).Build(opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, rs := range []*appsv1.ReplicaSet{current, scaledDown} {
			facts, ok := topo.facts.object(replicaSetKey(rs))
			if !ok || facts.uid != rs.UID || len(facts.owners) != 1 {
				t.Errorf("%+v: %s facts = %+v, %v; want its incarnation and owner", mode, rs.Name, facts, ok)
			}
			if drawn := includeReplicaSets && rs == current; facts.hop == drawn {
				t.Errorf("%+v: %s hop=%v; want a hop exactly when it is not drawn", mode, rs.Name, facts.hop)
			}
		}
		if _, ok := topo.facts.object(replicaSetKey(history)); ok {
			t.Errorf("%+v: a ReplicaSet no Pod names was observed", mode)
		}
	}
}

// Reflection reads every ConfigMap and Secret in scope but draws only the
// linked ones, so only those are observed.
func TestReflectionObservesOnlyLinkedObjects(t *testing.T) {
	source := &corev1.ConfigMap{ObjectMeta: charMeta("app", "source", nil)}
	mirror := &corev1.ConfigMap{ObjectMeta: charMeta("edge", "mirror", nil)}
	mirror.Annotations = map[string]string{"reflector.v1.k8s.emberstack.com/reflects": "app/source"}
	unrelated := &corev1.ConfigMap{ObjectMeta: charMeta("app", "unrelated", nil)}
	opts := DefaultBuildOptions()
	opts.IncludeConfigMaps = true
	topo, err := NewBuilder(&mockProvider{configMaps: []*corev1.ConfigMap{source, mirror, unrelated}}).Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, cm := range []*corev1.ConfigMap{source, mirror} {
		if _, ok := topo.facts.object(objectKey{kind: "ConfigMap", namespace: cm.Namespace, name: cm.Name}); !ok {
			t.Errorf("linked ConfigMap %s/%s was not observed", cm.Namespace, cm.Name)
		}
	}
	if _, ok := topo.facts.object(objectKey{kind: "ConfigMap", namespace: "app", name: "unrelated"}); ok {
		t.Error("an unlinked, undrawn ConfigMap was observed")
	}
}

// The generic CRD pass considers every owned custom resource but draws at
// most maxPerKind per kind, and only those whose owner is drawn; the facts it
// records are bounded by what it draws.
func TestGenericCandidatesAreObservedOnlyWhenDrawn(t *testing.T) {
	deploy := &appsv1.Deployment{ObjectMeta: charMeta("shop", "web", nil)}
	d := newCharDynamic()
	var gadgets []*unstructured.Unstructured
	for i := 0; i < 60; i++ {
		gadgets = append(gadgets, charUnstructured("example.io/v1", "Gadget", "shop", fmt.Sprintf("drawn-%02d", i), nil, nil, nil, charController("apps/v1", "Deployment", "web", string(deploy.UID))))
	}
	for i := 0; i < 10; i++ {
		gadgets = append(gadgets, charUnstructured("example.io/v1", "Gadget", "shop", fmt.Sprintf("orphaned-%02d", i), nil, nil, nil, charController("apps/v1", "Deployment", "ghost", "uid-ghost")))
	}
	d.add("example.io", "v1", "Gadget", "gadgets", true, gadgets...)
	topo, err := NewBuilder(&mockProvider{deployments: []*appsv1.Deployment{deploy}}).WithDynamic(d).Build(DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	drawn := 0
	for i := range topo.Nodes {
		if topo.Nodes[i].Kind == "Gadget" {
			drawn++
		}
	}
	observed := 0
	for key := range topo.facts.objects {
		if key.kind == "Gadget" {
			observed++
			if strings.HasPrefix(key.name, "orphaned-") {
				t.Errorf("%s has no drawn owner but was observed", key.name)
			}
		}
	}
	if drawn == 0 || observed != drawn {
		t.Fatalf("drew %d Gadgets and observed %d; want every drawn one and no other", drawn, observed)
	}
}

// A summary-mode build counts Pods instead of drawing them, and records each
// as a hop, so ownership through a Pod resolves whichever way the Pod tier is
// rendered. Relationship-cache builds never summarize, however large the
// namespace.
func TestSummaryModeRecordsPodsAsHops(t *testing.T) {
	provider := &mockProvider{}
	for i := 0; i < SummaryModeThreshold*5; i++ {
		provider.pods = append(provider.pods, &corev1.Pod{ObjectMeta: charMeta("big", fmt.Sprintf("p-%05d", i), map[string]string{"app": "big"}), Status: corev1.PodStatus{Phase: corev1.PodRunning}})
	}
	// Traffic view counts only Pods a Service routes to.
	provider.services = []*corev1.Service{{ObjectMeta: charMeta("big", "big", nil), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "big"}, Ports: []corev1.ServicePort{{Port: 80}}}}}
	browser := DefaultBuildOptions()
	browser.Namespaces = []string{"big"}
	traffic := browser
	traffic.ViewMode = ViewModeTraffic
	cache := RelationshipBuildOptions()
	cache.Namespaces = []string{"big"}
	for _, tc := range []struct {
		name      string
		opts      BuildOptions
		summarize bool
	}{{"browser", browser, true}, {"traffic", traffic, true}, {"relationship cache", cache, false}} {
		topo, err := NewBuilder(provider).Build(tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		facts, observed := topo.facts.object(objectKey{kind: "Pod", namespace: "big", name: "p-00000"})
		if topo.SummaryMode != tc.summarize || !observed || facts.hop != tc.summarize {
			t.Errorf("%s: summary=%v, Pod observed=%v hop=%v; want summary=%v and the Pod a hop exactly when summarized", tc.name, topo.SummaryMode, observed, facts.hop, tc.summarize)
		}
	}
}

func TestDrawingAnObjectClearsItsHop(t *testing.T) {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web", UID: "rs"}}
	capture := newFactsCapture(1)
	capture.observeHop(replicaSetKey(rs), rs)
	capture.observe(replicaSetKey(rs), rs)
	if facts, _ := capture.freeze().object(replicaSetKey(rs)); facts.hop {
		t.Fatal("an object drawn after being crossed as a hop still reads as a hop")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "p", UID: "p"}}
	capture = newFactsCapture(1)
	capture.observe(podKey(pod), pod)
	capture.observeHop(podKey(pod), pod)
	if facts, _ := capture.freeze().object(podKey(pod)); facts.hop {
		t.Fatal("a drawn object became a hop when crossed again")
	}
}
