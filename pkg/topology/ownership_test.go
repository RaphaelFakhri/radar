package topology

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skyhook-io/radar/pkg/resourceid"
)

var (
	webDeploy  = objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "web"}
	webRS      = objectKey{group: "apps", kind: "ReplicaSet", namespace: "shop", name: "web-7d9f8"}
	webPod     = objectKey{kind: "Pod", namespace: "shop", name: "web-7d9f8-0"}
	apiRS      = objectKey{group: "apps", kind: "ReplicaSet", namespace: "shop", name: "api-6b7"}
	apiPod     = objectKey{kind: "Pod", namespace: "shop", name: "api-6b7-a"}
	bigRS      = objectKey{group: "apps", kind: "ReplicaSet", namespace: "shop", name: "big-8f9"}
	nightlyCJ  = objectKey{group: "batch", kind: "CronJob", namespace: "team", name: "nightly"}
	nightlyJob = objectKey{group: "batch", kind: "Job", namespace: "team", name: "nightly-29001"}
	olderJob   = objectKey{group: "batch", kind: "Job", namespace: "team", name: "nightly-28999"}
	nightlyPod = objectKey{kind: "Pod", namespace: "team", name: "nightly-29001-x"}
)

func charView(t *testing.T, opts BuildOptions) (*Topology, *ownershipView) {
	t.Helper()
	topo := charBuild(t, charCluster(), opts)
	return topo, newOwnershipView(topo)
}

// testObject is one object a unit test records, drawn or as a hop.
type testObject struct {
	key    objectKey
	uid    types.UID
	owners []metav1.OwnerReference
	hop    bool
}

// testView records objects directly and answers for the visible ones, so
// resolution is tested apart from how producers build nodes.
func testView(objects []testObject, visible ...objectKey) *ownershipView {
	capture := newFactsCapture(len(objects))
	for _, o := range objects {
		meta := &metav1.ObjectMeta{Namespace: o.key.namespace, Name: o.key.name, UID: o.uid, OwnerReferences: o.owners}
		if o.hop {
			capture.observeHop(o.key, meta)
		} else {
			capture.observe(o.key, meta)
		}
	}
	registry := capture.freeze()
	v := &ownershipView{facts: registry, index: registry.ownership(), visible: map[objectKey]bool{}}
	for _, key := range visible {
		v.visible[key] = true
	}
	return v
}

func ownerRef(apiVersion, kind, name, uid string, controller bool) metav1.OwnerReference {
	ref := metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(uid)}
	if controller {
		ref.Controller = &controller
	}
	return ref
}

func TestReferenceStatesInBuilds(t *testing.T) {
	full, fullView := charView(t, charSSEOptions())
	_, defaultView := charView(t, DefaultBuildOptions())
	projected := newOwnershipView(charStrippedProjection(full))
	for _, tc := range []struct {
		name      string
		view      *ownershipView
		dependent objectKey
		want      ownerLink
	}{
		{"drawn and visible", fullView, webPod, ownerLink{key: webRS, controller: true, state: refLive}},
		{"hidden by the ReplicaSet toggle", defaultView, webPod, ownerLink{key: webRS, controller: true, state: refCollapsed}},
		{"stripped from a projection", projected, webPod, ownerLink{key: webRS, controller: true, state: refWithheld}},
		{"names a deleted incarnation", fullView, apiRS, ownerLink{key: objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "api"}, controller: true, state: refReplaced}},
	} {
		links, ok := tc.view.references(tc.dependent)
		if !ok || !slices.Contains(links, tc.want) {
			t.Errorf("%s: references(%+v) = %+v, want %+v among them", tc.name, tc.dependent, links, tc.want)
		}
	}
}

func TestReferenceStatesByScopeAndIncarnation(t *testing.T) {
	deploy := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "web"}
	node := objectKey{kind: "Node", name: "node-a"}
	xr := objectKey{group: "demo.example.io", kind: "XDatabase", name: "orders"}
	widget := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "w"}
	conflicted := objectKey{group: "apps", kind: "ReplicaSet", namespace: "shop", name: "twice"}
	objects := []testObject{
		{key: deploy, uid: "deploy"},
		{key: node, uid: "node"},
		{key: xr, uid: "xr"},
		{key: widget, uid: "widget"},
		{key: conflicted, uid: "first"},
		{key: conflicted, uid: "second"},
	}
	v := testView(objects, deploy, node, xr, widget, conflicted)
	for _, tc := range []struct {
		name      string
		dependent objectKey
		ref       metav1.OwnerReference
		want      ownerLink
	}{
		{"namespaced owner in the dependent's namespace", objectKey{kind: "Pod", namespace: "shop", name: "p"},
			ownerRef("apps/v1", "Deployment", "web", "deploy", true), ownerLink{key: deploy, controller: true, state: refLive}},
		{"cluster-scoped owner of a namespaced dependent", objectKey{kind: "Pod", namespace: "kube-system", name: "mirror"},
			ownerRef("v1", "Node", "node-a", "node", true), ownerLink{key: node, controller: true, state: refLive}},
		{"cluster-scoped custom owner of a cluster-scoped dependent", objectKey{group: "kubernetes.crossplane.io", kind: "Object", name: "o"},
			ownerRef("demo.example.io/v1", "XDatabase", "orders", "xr", true), ownerLink{key: xr, controller: true, state: refLive}},
		{"namespaced custom owner, scope learned from its observed objects", objectKey{kind: "ConfigMap", namespace: "shop", name: "c"},
			ownerRef("example.io/v1", "Widget", "w", "widget", false), ownerLink{key: widget, state: refLive}},
		{"namespaced owner named by a cluster-scoped dependent", objectKey{kind: "PersistentVolume", name: "pv"},
			ownerRef("apps/v1", "Deployment", "web", "deploy", true), ownerLink{key: objectKey{group: "apps", kind: "Deployment", name: "web"}, controller: true, state: refInvalidScope}},
		{"same identity, other incarnation", objectKey{kind: "Pod", namespace: "shop", name: "p"},
			ownerRef("apps/v1", "Deployment", "web", "older", true), ownerLink{key: deploy, controller: true, state: refReplaced}},
		{"built-in kind, object never observed", objectKey{kind: "Pod", namespace: "shop", name: "p"},
			ownerRef("apps/v1", "ReplicaSet", "gone", "gone", true), ownerLink{key: objectKey{group: "apps", kind: "ReplicaSet", namespace: "shop", name: "gone"}, controller: true, state: refUnrepresented}},
		{"kind never observed", objectKey{group: "apps", kind: "StatefulSet", namespace: "db", name: "pg"},
			ownerRef("postgresql.cnpg.io/v1", "Cluster", "pg", "pg", true), ownerLink{key: objectKey{group: "postgresql.cnpg.io", kind: "Cluster", name: "pg"}, controller: true, state: refUnrepresented}},
		{"two incarnations observed", objectKey{kind: "Pod", namespace: "shop", name: "p"},
			ownerRef("apps/v1", "ReplicaSet", "twice", "first", true), ownerLink{key: conflicted, controller: true, state: refConflicted}},
	} {
		owner := tc.ref
		fact := ownerFact{group: groupOf(owner.APIVersion), kind: owner.Kind, name: owner.Name, uid: owner.UID, controller: owner.Controller != nil && *owner.Controller}
		if got := v.resolve(tc.dependent, fact); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// An owner of a custom kind the build never observed resolves to an exact
// identity through the scope discovery gave the build: namespaced owners in
// the dependent's namespace, cluster-scoped ones at cluster scope, and a
// namespaced owner of a cluster-scoped dependent never.
func TestDiscoveredScopeResolvesUnobservedOwners(t *testing.T) {
	for _, tc := range []struct {
		name       string
		namespaced bool
		dependent  objectKey
		want       ownerLink
	}{
		{"namespaced kind", true, objectKey{group: "apps", kind: "StatefulSet", namespace: "db", name: "pg"},
			ownerLink{key: objectKey{group: "postgresql.cnpg.io", kind: "Cluster", namespace: "db", name: "pg"}, controller: true, state: refUnrepresented}},
		{"cluster-scoped kind", false, objectKey{group: "apps", kind: "StatefulSet", namespace: "db", name: "pg"},
			ownerLink{key: objectKey{group: "postgresql.cnpg.io", kind: "Cluster", name: "pg"}, controller: true, state: refUnrepresented}},
		{"namespaced kind named by a cluster-scoped dependent", true, objectKey{kind: "PersistentVolume", name: "pv"},
			ownerLink{key: objectKey{group: "postgresql.cnpg.io", kind: "Cluster", name: "pg"}, controller: true, state: refInvalidScope}},
	} {
		capture := newFactsCapture(1)
		capture.observe(tc.dependent, &metav1.ObjectMeta{Name: tc.dependent.name, Namespace: tc.dependent.namespace, UID: "dependent"})
		capture.scopes = map[resourceid.GroupKind]bool{{Group: "postgresql.cnpg.io", Kind: "Cluster"}: tc.namespaced}
		registry := capture.freeze()
		v := &ownershipView{facts: registry, index: registry.ownership(), visible: map[objectKey]bool{tc.dependent: true}}
		got := v.resolve(tc.dependent, ownerFact{group: "postgresql.cnpg.io", kind: "Cluster", name: "pg", uid: "pg", controller: true})
		if got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func groupOf(apiVersion string) string {
	return factsFor(&metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{APIVersion: apiVersion}}}).owners[0].group
}

func TestOwnerPolicy(t *testing.T) {
	deploy := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "web"}
	other := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "other"}
	dependent := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "w"}
	controllerRef := ownerRef("apps/v1", "Deployment", "web", "deploy", true)
	plainRef := ownerRef("apps/v1", "Deployment", "other", "other", false)
	for _, tc := range []struct {
		name    string
		owners  []metav1.OwnerReference
		visible []objectKey
		want    ownerAnswer
	}{
		{"controller live", []metav1.OwnerReference{plainRef, controllerRef}, []objectKey{deploy, other},
			ownerAnswer{link: ownerLink{key: deploy, controller: true, state: refLive}}},
		{"withheld controller never yields to a weaker owner", []metav1.OwnerReference{plainRef, controllerRef}, []objectKey{other},
			ownerAnswer{link: ownerLink{key: deploy, controller: true, state: refWithheld}}},
		{"replaced controller", []metav1.OwnerReference{ownerRef("apps/v1", "Deployment", "web", "older", true), plainRef}, []objectKey{deploy, other},
			ownerAnswer{link: ownerLink{key: deploy, controller: true, state: refReplaced}}},
		{"sole reference without controller", []metav1.OwnerReference{plainRef}, []objectKey{other},
			ownerAnswer{link: ownerLink{key: other, state: refLive}}},
		{"several references without controller", []metav1.OwnerReference{plainRef, ownerRef("apps/v1", "Deployment", "web", "deploy", false)}, []objectKey{deploy, other},
			ownerAnswer{several: true}},
		{"no references", nil, []objectKey{deploy, other}, ownerAnswer{}},
	} {
		v := testView([]testObject{{key: deploy, uid: "deploy"}, {key: other, uid: "other"}, {key: dependent, uid: "w", owners: tc.owners}}, append(tc.visible, dependent)...)
		if got := v.owner(dependent); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestChildren(t *testing.T) {
	full, fullView := charView(t, charSSEOptions())
	big := fullView.children(bigRS)
	if len(big) != 7 {
		t.Fatalf("children of the grouped ReplicaSet = %d, want its 7 Pods", len(big))
	}
	for _, child := range big {
		if child.key.kind != "Pod" || !child.controller {
			t.Errorf("grouped child %+v", child)
		}
	}
	if got := fullView.children(nightlyCJ); !slices.Equal(got, []childLink{{key: olderJob, controller: true}, {key: nightlyJob, controller: true}}) {
		t.Errorf("CronJob children = %+v; want each Job once, through its live controller reference", got)
	}
	if got := fullView.children(objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "api"}); len(got) != 0 {
		t.Errorf("a Deployment whose ReplicaSet names its previous incarnation has children %+v", got)
	}
	projected := charProjection(full, map[NodeKind]bool{KindPodGroup: true}, nil)
	if got := newOwnershipView(projected).children(bigRS); len(got) != 0 {
		t.Errorf("children of a ReplicaSet whose Pod group is stripped = %+v", got)
	}
	widget := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "web-widget"}
	if got := newOwnershipView(projected).children(webDeploy); !slices.Equal(got, []childLink{{key: webRS, controller: true}, {key: widget, controller: true}}) {
		t.Errorf("Deployment children = %+v; want its ReplicaSet and the custom resource it controls", got)
	}
}

func TestControllerRootInBuilds(t *testing.T) {
	full, fullView := charView(t, charSSEOptions())
	_, defaultView := charView(t, DefaultBuildOptions())
	withheldRS := newOwnershipView(charStrippedProjection(full))
	for _, tc := range []struct {
		name  string
		view  *ownershipView
		start objectKey
		want  rootAnswer
	}{
		{"through a drawn ReplicaSet", fullView, webPod, rootAnswer{root: webDeploy}},
		{"through a hidden ReplicaSet", defaultView, webPod, rootAnswer{root: webDeploy}},
		{"through a Job", fullView, nightlyPod, rootAnswer{root: nightlyCJ}},
		{"stops at a withheld ReplicaSet", withheldRS, webPod, rootAnswer{root: webPod, stop: refWithheld}},
		{"stops at a replaced Deployment", fullView, apiPod, rootAnswer{root: apiRS, stop: refReplaced}},
		{"a root records no controller", fullView, webDeploy, rootAnswer{root: webDeploy}},
		{"an unobserved start", fullView, objectKey{kind: "Pod", namespace: "shop", name: "nope"}, rootAnswer{root: objectKey{kind: "Pod", namespace: "shop", name: "nope"}, stop: refUnrepresented}},
	} {
		if got := tc.view.controllerRoot(tc.start); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestControllerRootTerminals(t *testing.T) {
	node := objectKey{kind: "Node", name: "node-a"}
	mirror := objectKey{kind: "Pod", namespace: "kube-system", name: "etcd-node-a"}
	a := objectKey{group: "example.io", kind: "Loop", namespace: "x", name: "a"}
	b := objectKey{group: "example.io", kind: "Loop", namespace: "x", name: "b"}
	rs := objectKey{group: "apps", kind: "ReplicaSet", namespace: "x", name: "hidden"}
	pod := objectKey{kind: "Pod", namespace: "x", name: "p"}
	v := testView([]testObject{
		{key: node, uid: "node"},
		{key: mirror, uid: "mirror", owners: []metav1.OwnerReference{ownerRef("v1", "Node", "node-a", "node", true)}},
		{key: a, uid: "a", owners: []metav1.OwnerReference{ownerRef("example.io/v1", "Loop", "b", "b", true)}},
		{key: b, uid: "b", owners: []metav1.OwnerReference{ownerRef("example.io/v1", "Loop", "a", "a", true)}},
		{key: rs, uid: "rs", hop: true},
		{key: pod, uid: "p", owners: []metav1.OwnerReference{ownerRef("apps/v1", "ReplicaSet", "hidden", "rs", true)}},
	}, node, mirror, a, b, pod)
	if got := v.controllerRoot(mirror); got != (rootAnswer{root: mirror, node: true}) {
		t.Errorf("mirror Pod: %+v; want itself, ended by its Node", got)
	}
	for _, start := range []objectKey{a, b} {
		if got := v.controllerRoot(start); got != (rootAnswer{root: a, cycle: true}) {
			t.Errorf("cycle from %s: %+v; want the smallest member from either start", start.name, got)
		}
	}
	if got := v.controllerRoot(pod); got != (rootAnswer{root: rs, collapsed: true}) {
		t.Errorf("chain ending at a hidden ReplicaSet: %+v", got)
	}

	var chain []testObject
	for i := 0; i < 3*maxControllerDepth; i++ {
		obj := testObject{key: objectKey{group: "example.io", kind: "Link", namespace: "x", name: fmt.Sprint(i)}, uid: types.UID(fmt.Sprint(i))}
		obj.owners = []metav1.OwnerReference{ownerRef("example.io/v1", "Link", fmt.Sprint(i+1), fmt.Sprint(i+1), true)}
		chain = append(chain, obj)
	}
	keys := make([]objectKey, len(chain))
	for i := range chain {
		keys[i] = chain[i].key
	}
	if got := testView(chain, keys...).controllerRoot(keys[0]); !got.truncated || got.root != keys[maxControllerDepth] {
		t.Errorf("an over-long chain walked to %+v; want a walk bounded at %d hops and marked truncated", got, maxControllerDepth)
	}
}

func TestOriginParent(t *testing.T) {
	deploy := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "web"}
	sts := objectKey{group: "apps", kind: "StatefulSet", namespace: "db", name: "pg"}
	sole := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "sole"}
	several := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "several"}
	v := testView([]testObject{
		{key: deploy, uid: "deploy"},
		{key: sts, uid: "sts", owners: []metav1.OwnerReference{ownerRef("postgresql.cnpg.io/v1", "Cluster", "pg", "pg", true)}},
		{key: sole, uid: "sole", owners: []metav1.OwnerReference{ownerRef("apps/v1", "Deployment", "web", "deploy", false)}},
		{key: several, uid: "several", owners: []metav1.OwnerReference{ownerRef("apps/v1", "Deployment", "web", "deploy", false), ownerRef("apps/v1", "Deployment", "x", "x", false)}},
	}, deploy, sts, sole, several)
	if got, ok := v.originParent(sts); !ok || got != (originAnswer{link: ownerLink{key: objectKey{group: "postgresql.cnpg.io", kind: "Cluster", name: "pg"}, controller: true, state: refUnrepresented}}) {
		t.Errorf("an unrepresented controller still names the origin: %+v, %v", got, ok)
	}
	if got, ok := v.originParent(sole); !ok || got != (originAnswer{link: ownerLink{key: deploy, state: refLive}, weak: true}) {
		t.Errorf("sole reference: %+v, %v; want it, marked weak", got, ok)
	}
	if _, ok := v.originParent(several); ok {
		t.Error("several references without a controller name no origin")
	}
}

// Fewer visible objects can only withhold an answer, never change it: every
// projection answers like the full build or reports withheld, and lists a
// subset of the full build's children.
func TestOwnershipAnswersOnlyNarrowInProjections(t *testing.T) {
	full, fullView := charView(t, charSSEOptions())
	projections := []*Topology{
		charStrippedProjection(full),
		charProjection(full, map[NodeKind]bool{KindDeployment: true, KindPodGroup: true}, map[string]bool{"cronjob/team/nightly": true}),
		charProjection(full, map[NodeKind]bool{KindReplicaSet: true, KindJob: true}, nil),
	}
	for i, projected := range projections {
		view := newOwnershipView(projected)
		for key := range full.facts.objects {
			want, got := fullView.owner(key), view.owner(key)
			if got != want && !(want.live() && got.link.state == refWithheld && got.link.key == want.link.key) {
				t.Errorf("projection %d: owner(%+v) = %+v, full build %+v", i, key, got, want)
			}
			wantRoot, gotRoot := fullView.controllerRoot(key), view.controllerRoot(key)
			if gotRoot != wantRoot && gotRoot.stop != refWithheld {
				t.Errorf("projection %d: controllerRoot(%+v) = %+v, full build %+v", i, key, gotRoot, wantRoot)
			}
			for _, child := range view.children(key) {
				if !slices.Contains(fullView.children(key), child) {
					t.Errorf("projection %d: child %+v of %+v the full build does not list", i, child, key)
				}
			}
			for _, owner := range view.owners(key) {
				if !slices.Contains(fullView.owners(key), owner) {
					t.Errorf("projection %d: owner %+v of %+v the full build does not list", i, owner, key)
				}
			}
			wantOrigin, wantOK := fullView.originParent(key)
			gotOrigin, gotOK := view.originParent(key)
			if gotOK != wantOK || gotOrigin.link.key != wantOrigin.link.key || gotOrigin.weak != wantOrigin.weak ||
				(gotOrigin.link.state != wantOrigin.link.state && gotOrigin.link.state != refWithheld) {
				t.Errorf("projection %d: originParent(%+v) = %+v, full build %+v", i, key, gotOrigin, wantOrigin)
			}
		}
	}
}

// The ownership index is built once per build and shared by every projection;
// concurrent first use from several projections must be safe.
func TestOwnershipIndexIsSharedAcrossProjections(t *testing.T) {
	full := charBuild(t, charCluster(), charSSEOptions())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view := newOwnershipView(charStrippedProjection(full))
			view.children(webDeploy)
			view.controllerRoot(webPod)
		}()
	}
	wg.Wait()
	if newOwnershipView(full).index != newOwnershipView(charStrippedProjection(full)).index {
		t.Fatal("projections of one build derived separate ownership indexes")
	}
}

// A view must see exactly the identities IndexByResource addresses nodes by,
// Calico's second API group and grouped Pods included.
func TestOwnershipViewSeesEveryIndexedIdentity(t *testing.T) {
	calico, err := NewBuilder(&mockProvider{}).WithDynamic(calicoTestDynamicForBoth()).Build(DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	builds := []*Topology{calico}
	for _, tc := range charCases() {
		builds = append(builds, charBuild(t, charCluster(), tc.opts()))
	}
	aliases := 0
	for _, topo := range builds {
		view := newOwnershipView(topo)
		want := map[objectKey]bool{}
		for i := range topo.Nodes {
			keys := nodeResourceKeys(&topo.Nodes[i])
			aliases += max(len(keys)-1, 0)
			for _, key := range keys {
				want[keyOf(key)] = true
			}
			for _, member := range topo.facts.groupMembers(topo.Nodes[i].ID) {
				want[member] = true
			}
		}
		if len(view.visible) != len(want) {
			t.Errorf("view sees %d identities, the index %d", len(view.visible), len(want))
		}
		for key := range want {
			if !view.visible[key] {
				t.Errorf("view does not see %+v", key)
			}
		}
	}
	if aliases == 0 {
		t.Fatal("no build exercised a node with a second identity")
	}
}

// Objects feeding a controller loop get the loop's own representative, never
// a smaller-named object on the way into it.
func TestControllerRootCycleRepresentativeIgnoresTheTail(t *testing.T) {
	key := func(name string) objectKey {
		return objectKey{group: "example.io", kind: "Loop", namespace: "x", name: name}
	}
	ref := func(name string) metav1.OwnerReference { return ownerRef("example.io/v1", "Loop", name, name, true) }
	objects := []testObject{
		{key: key("a-tail"), uid: "a-tail", owners: []metav1.OwnerReference{ref("b-cycle")}},
		{key: key("b-cycle"), uid: "b-cycle", owners: []metav1.OwnerReference{ref("c-cycle")}},
		{key: key("c-cycle"), uid: "c-cycle", owners: []metav1.OwnerReference{ref("b-cycle")}},
	}
	v := testView(objects, key("a-tail"), key("b-cycle"), key("c-cycle"))
	for _, start := range []string{"a-tail", "b-cycle", "c-cycle"} {
		if got := v.controllerRoot(key(start)); got != (rootAnswer{root: key("b-cycle"), cycle: true}) {
			t.Errorf("from %s: %+v; want the loop's smallest member", start, got)
		}
	}
}

// A Node controller ends the walk at the Pod, and a Node reference that does
// not resolve still reports why.
func TestControllerRootKeepsANodeReferencesState(t *testing.T) {
	node := objectKey{kind: "Node", name: "node-a"}
	pod := objectKey{kind: "Pod", namespace: "kube-system", name: "etcd"}
	for _, tc := range []struct {
		name    string
		objects []testObject
		visible []objectKey
		stop    refState
	}{
		{"live", []testObject{{key: node, uid: "node"}}, []objectKey{node}, 0},
		{"withheld", []testObject{{key: node, uid: "node"}}, nil, refWithheld},
		{"unrepresented", nil, nil, refUnrepresented},
		{"replaced", []testObject{{key: node, uid: "newer"}}, []objectKey{node}, refReplaced},
		{"conflicted", []testObject{{key: node, uid: "node"}, {key: node, uid: "other"}}, []objectKey{node}, refConflicted},
	} {
		objects := append(tc.objects, testObject{key: pod, uid: "pod", owners: []metav1.OwnerReference{ownerRef("v1", "Node", "node-a", "node", true)}})
		got := testView(objects, append(tc.visible, pod)...).controllerRoot(pod)
		if got != (rootAnswer{root: pod, node: true, stop: tc.stop}) {
			t.Errorf("%s Node: %+v", tc.name, got)
		}
	}
}

// A chain of exactly maxControllerDepth references is walked to its end; only
// a longer one is truncated.
func TestControllerRootDepthBoundary(t *testing.T) {
	for _, links := range []int{maxControllerDepth - 1, maxControllerDepth, maxControllerDepth + 1} {
		var objects []testObject
		var keys []objectKey
		for i := 0; i <= links; i++ {
			o := testObject{key: objectKey{group: "example.io", kind: "Link", namespace: "x", name: fmt.Sprint(i)}, uid: types.UID(fmt.Sprint(i))}
			if i < links {
				o.owners = []metav1.OwnerReference{ownerRef("example.io/v1", "Link", fmt.Sprint(i+1), fmt.Sprint(i+1), true)}
			}
			objects = append(objects, o)
			keys = append(keys, o.key)
		}
		got := testView(objects, keys...).controllerRoot(keys[0])
		want := rootAnswer{root: keys[links]}
		if links > maxControllerDepth {
			want = rootAnswer{root: keys[maxControllerDepth], truncated: true}
		}
		if got != want {
			t.Errorf("%d references: %+v, want %+v", links, got, want)
		}
	}
}

func TestOwnersAreTheLiveReferences(t *testing.T) {
	a := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "a"}
	b := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "b"}
	hidden := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "hidden"}
	dependent := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "w"}
	v := testView([]testObject{
		{key: a, uid: "a"}, {key: b, uid: "b"}, {key: hidden, uid: "hidden"},
		{key: dependent, uid: "w", owners: []metav1.OwnerReference{
			ownerRef("apps/v1", "Deployment", "a", "a", false),
			ownerRef("apps/v1", "Deployment", "hidden", "hidden", true),
			ownerRef("apps/v1", "Deployment", "b", "older", false),
			ownerRef("apps/v1", "Deployment", "b", "b", false),
		}},
	}, a, b, dependent)
	want := []ownerLink{{key: a, state: refLive}, {key: b, state: refLive}}
	if got := v.owners(dependent); !slices.Equal(got, want) {
		t.Errorf("owners = %+v, want only the live references %+v", got, want)
	}
}

// The origin is the controller reference wherever it is recorded, in its own
// state; a live weaker reference never stands in for it.
func TestOriginParentPrefersTheControllerInAnyState(t *testing.T) {
	plain := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "plain"}
	controller := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "controller"}
	dependent := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "w"}
	objects := []testObject{
		{key: plain, uid: "plain"}, {key: controller, uid: "controller"},
		{key: dependent, uid: "w", owners: []metav1.OwnerReference{
			ownerRef("apps/v1", "Deployment", "plain", "plain", false),
			ownerRef("apps/v1", "Deployment", "controller", "controller", true),
		}},
	}
	for _, tc := range []struct {
		name    string
		visible []objectKey
		state   refState
	}{{"visible", []objectKey{plain, controller, dependent}, refLive}, {"withheld", []objectKey{plain, dependent}, refWithheld}} {
		got, ok := testView(objects, tc.visible...).originParent(dependent)
		if !ok || got != (originAnswer{link: ownerLink{key: controller, controller: true, state: tc.state}}) {
			t.Errorf("%s controller: %+v, %v", tc.name, got, ok)
		}
	}
}
