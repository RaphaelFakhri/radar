package topology

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skyhook-io/radar/pkg/resourceid"
)

var (
	shopApp      = objectKey{group: argoApplicationGroup, kind: argoApplicationKind, namespace: "argocd", name: "shop"}
	shopKS       = objectKey{group: fluxKustomizeGroup, kind: fluxKustomizationKind, namespace: "flux-system", name: "shop"}
	platformKS   = objectKey{group: fluxKustomizeGroup, kind: fluxKustomizationKind, namespace: "flux-system", name: "platform"}
	redisHR      = objectKey{group: fluxHelmGroup, kind: fluxHelmReleaseKind, namespace: "shop", name: "redis"}
	cartDeploy   = objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "cart"}
	redisDeploy  = objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "redis"}
	redisMigrate = objectKey{group: "batch", kind: "Job", namespace: "shop", name: "redis-migrate"}
)

func TestManagerOfInBuilds(t *testing.T) {
	full, view := charView(t, charSSEOptions())
	for _, tc := range []struct {
		name string
		x    objectKey
		want managerAnswer
	}{
		{"listed in an Application's status", webDeploy,
			managerAnswer{state: managerResolved, manager: shopApp, grade: gradeRecorded, via: webDeploy}},
		{"a Pod inherits through its ReplicaSet and Deployment", webPod,
			managerAnswer{state: managerResolved, manager: shopApp, grade: gradeRecorded, via: webDeploy, inherited: true}},
		{"listed in a Kustomization's inventory", cartDeploy,
			managerAnswer{state: managerResolved, manager: shopKS, grade: gradeRecorded, via: cartDeploy}},
		{"a HelmRelease's labels, its inventory unread", redisDeploy,
			managerAnswer{state: managerResolved, manager: redisHR, grade: gradeClaim, via: redisDeploy}},
		{"a Helm hook is not part of its release", redisMigrate,
			managerAnswer{state: managerResolved, manager: redisHR, grade: gradeHookOrigin, via: redisMigrate}},
	} {
		if got := view.managerOf(tc.x); !managerAnswersEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// The cart Pod's own label names the platform Kustomization, which does
	// not record it; its controller chain decides first.
	cartPod := objectKey{kind: "Pod", namespace: "shop", name: "cart-5c6-a"}
	if got := view.managerOf(cartPod); got.manager != shopKS || !got.inherited {
		t.Errorf("cart Pod: %+v; want the Kustomization that records its Deployment", got)
	}

	projected := newOwnershipView(charStrippedProjection(full))
	if got := projected.managerOf(webPod); got.state != managerWithheld {
		t.Errorf("a Pod whose ReplicaSet is stripped: %+v; want withheld", got)
	}
}

func managerAnswersEqual(a, b managerAnswer) bool {
	return a.state == b.state && a.manager == b.manager && a.grade == b.grade && a.inherited == b.inherited &&
		a.weak == b.weak && a.via == b.via && slices.Equal(a.conflicts, b.conflicts) && slices.Equal(a.rejected, b.rejected)
}

func TestReleaseUnitInBuilds(t *testing.T) {
	_, view := charView(t, charSSEOptions())
	for _, tc := range []struct {
		name   string
		x      objectKey
		unit   objectKey
		source releaseSource
	}{
		{"a manager is its own unit", shopApp, shopApp, releaseSelf},
		{"a managed Pod's unit is its manager", webPod, shopApp, releaseManager},
		{"an unmanaged Pod's unit is its controller root", nightlyPod, nightlyCJ, releaseControllerRoot},
	} {
		got := view.releaseUnit(tc.x)
		if got.state != managerResolved || got.unit != tc.unit || got.source != tc.source {
			t.Errorf("%s: %+v, want %+v from %d", tc.name, got, tc.unit, tc.source)
		}
	}
	if got := view.releaseUnit(apiPod); got.state != managerResolved || got.unit != apiRS || got.root.stop != refReplaced {
		t.Errorf("a Pod whose ReplicaSet names a deleted Deployment: %+v; want the ReplicaSet with the replaced stop", got)
	}
}

// claimObject is a test object with metadata claims.
type claimObject struct {
	testObject
	labels, annotations map[string]string
}

func managerView(objects []claimObject, memberships map[objectKey]membershipFacts, scopes map[resourceid.GroupKind]bool, visible ...objectKey) *ownershipView {
	capture := newFactsCapture(len(objects))
	for _, o := range objects {
		meta := &metav1.ObjectMeta{Namespace: o.key.namespace, Name: o.key.name, UID: o.uid, OwnerReferences: o.owners, Labels: o.labels, Annotations: o.annotations}
		if o.hop {
			capture.observeHop(o.key, meta)
		} else {
			capture.observe(o.key, meta)
		}
	}
	for manager, membership := range memberships {
		capture.recordMembership(manager, membership)
	}
	capture.scopes = scopes
	registry := capture.freeze()
	v := &ownershipView{facts: registry, index: registry.ownership(), visible: map[objectKey]bool{}}
	for _, key := range visible {
		v.visible[key] = true
	}
	return v
}

func members(keys ...objectKey) membershipFacts {
	m := membershipFacts{state: membershipRecorded, members: map[objectKey]struct{}{}}
	for _, key := range keys {
		m.members[key] = struct{}{}
	}
	return m
}

func ksLabels(name string) map[string]string {
	return map[string]string{fluxKustomizeNameLabel: name, fluxKustomizeNSLabel: "flux-system"}
}

func TestManagerResolutionRules(t *testing.T) {
	ks := func(name string) objectKey {
		return objectKey{group: fluxKustomizeGroup, kind: fluxKustomizationKind, namespace: "flux-system", name: name}
	}
	app := func(namespace, name string) objectKey {
		return objectKey{group: argoApplicationGroup, kind: argoApplicationKind, namespace: namespace, name: name}
	}
	x := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "x"}
	owner := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "owner"}
	cluster := objectKey{group: "postgresql.cnpg.io", kind: "Cluster", namespace: "shop", name: "pg"}
	sts := objectKey{group: "apps", kind: "StatefulSet", namespace: "shop", name: "pg"}
	obj := func(key objectKey, labels, annotations map[string]string, owners ...metav1.OwnerReference) claimObject {
		return claimObject{testObject: testObject{key: key, uid: typesUID(key.name), owners: owners}, labels: labels, annotations: annotations}
	}
	controllerOf := func(key objectKey) metav1.OwnerReference {
		return ownerRef(key.group+"/v1", key.kind, key.name, key.name, true)
	}
	for _, tc := range []struct {
		name        string
		objects     []claimObject
		memberships map[objectKey]membershipFacts
		scopes      map[resourceid.GroupKind]bool
		hidden      []objectKey
		subject     objectKey
		want        managerAnswer
	}{
		{name: "recorded by one manager",
			objects:     []claimObject{obj(x, nil, nil)},
			memberships: map[objectKey]membershipFacts{ks("a"): members(x)},
			subject:     x,
			want:        managerAnswer{state: managerResolved, manager: ks("a"), grade: gradeRecorded, via: x}},
		{name: "recorded by two, its claim chooses",
			objects:     []claimObject{obj(x, ksLabels("b"), nil)},
			memberships: map[objectKey]membershipFacts{ks("a"): members(x), ks("b"): members(x)},
			subject:     x,
			want:        managerAnswer{state: managerResolved, manager: ks("b"), grade: gradeRecorded, via: x, conflicts: []objectKey{ks("a")}}},
		{name: "recorded by two, nothing chooses",
			objects:     []claimObject{obj(x, nil, nil)},
			memberships: map[objectKey]membershipFacts{ks("b"): members(x), ks("a"): members(x)},
			subject:     x,
			want:        managerAnswer{state: managerAmbiguous, manager: ks("a"), grade: gradeRecorded, via: x, conflicts: []objectKey{ks("b")}}},
		{name: "a withheld controller never yields to its own claim",
			objects:     []claimObject{obj(owner, nil, nil), obj(x, ksLabels("a"), nil, controllerOf(owner))},
			memberships: map[objectKey]membershipFacts{ks("b"): members(owner)},
			hidden:      []objectKey{owner},
			subject:     x,
			want:        managerAnswer{state: managerWithheld}},
		{name: "an unmanaged controller falls through to its own claim",
			objects: []claimObject{obj(owner, nil, nil), obj(x, ksLabels("a"), nil, controllerOf(owner))},
			subject: x,
			want:    managerAnswer{state: managerResolved, manager: ks("a"), grade: gradeClaim, via: x}},
		{name: "a sole plain owner is inherited, marked weak",
			objects:     []claimObject{obj(owner, nil, nil), obj(x, nil, nil, ownerRef("apps/v1", "Deployment", "owner", "owner", false))},
			memberships: map[objectKey]membershipFacts{ks("a"): members(owner)},
			subject:     x,
			want:        managerAnswer{state: managerResolved, manager: ks("a"), grade: gradeRecorded, via: owner, inherited: true, weak: true}},
		{name: "a claim the manager's inventory refutes is stale",
			objects:     []claimObject{obj(x, ksLabels("a"), nil)},
			memberships: map[objectKey]membershipFacts{ks("a"): members(owner)},
			subject:     x,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{manager: ks("a"), tool: toolFluxKustomization, grade: gradeClaim}, reason: rejectStale}}}},
		{name: "a claim the inventory supports through an unobserved controller",
			objects:     []claimObject{obj(sts, ksLabels("a"), nil, controllerOf(cluster))},
			memberships: map[objectKey]membershipFacts{ks("a"): members(cluster)},
			scopes:      map[resourceid.GroupKind]bool{{Group: "postgresql.cnpg.io", Kind: "Cluster"}: true},
			subject:     sts,
			want:        managerAnswer{state: managerResolved, manager: ks("a"), grade: gradeClaim, via: sts}},
		{name: "an ancestor of unknown scope supports nothing",
			objects:     []claimObject{obj(sts, ksLabels("a"), nil, controllerOf(cluster))},
			memberships: map[objectKey]membershipFacts{ks("a"): members(objectKey{group: cluster.group, kind: cluster.kind, name: cluster.name})},
			subject:     sts,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{manager: ks("a"), tool: toolFluxKustomization, grade: gradeClaim}, reason: rejectStale}}}},
		{name: "a claim naming a remote manager",
			objects:     []claimObject{obj(x, ksLabels("a"), nil)},
			memberships: map[objectKey]membershipFacts{ks("a"): {state: membershipRemote}},
			subject:     x,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{manager: ks("a"), tool: toolFluxKustomization, grade: gradeClaim}, reason: rejectRemote}}}},
		{name: "a tracking id copied from another object",
			objects: []claimObject{obj(x, nil, map[string]string{argoTrackingIDAnnotation: "argocd_shop:apps/Deployment:shop/original"})},
			subject: x,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{manager: app("argocd", "shop"), tool: toolArgoTrackingID, grade: gradeClaim}, reason: rejectCopied}}}},
		{name: "a tracking id naming the object, bare application name resolved by observation",
			objects: []claimObject{obj(app("argocd", "shop"), nil, nil), obj(x, nil, map[string]string{argoTrackingIDAnnotation: "shop:apps/Deployment:shop/x"})},
			subject: x,
			want:    managerAnswer{state: managerResolved, manager: app("argocd", "shop"), grade: gradeClaim, via: x}},
		{name: "a core-group tracking id",
			objects: []claimObject{obj(objectKey{kind: "Service", namespace: "shop", name: "web"}, nil, map[string]string{argoTrackingIDAnnotation: "argocd_shop:/Service:shop/web"})},
			subject: objectKey{kind: "Service", namespace: "shop", name: "web"},
			want:    managerAnswer{state: managerResolved, manager: app("argocd", "shop"), grade: gradeClaim, via: objectKey{kind: "Service", namespace: "shop", name: "web"}}},
		{name: "a Flux claim without its namespace label",
			objects: []claimObject{obj(x, map[string]string{fluxKustomizeNameLabel: "a"}, nil)},
			subject: x,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{tool: toolFluxKustomization, grade: gradeClaim}, reason: rejectIncomplete}}}},
		{name: "claims order by grade, then tool",
			objects: []claimObject{obj(x, map[string]string{
				fluxHelmNameLabel: "release", fluxHelmNSLabel: "shop",
				fluxKustomizeNameLabel: "a", fluxKustomizeNSLabel: "flux-system",
			}, map[string]string{helmHookAnnotation: "pre-install"})},
			subject: x,
			want: managerAnswer{state: managerResolved, manager: ks("a"), grade: gradeClaim, via: x,
				conflicts: []objectKey{{group: fluxHelmGroup, kind: fluxHelmReleaseKind, namespace: "shop", name: "release"}}}},
		{name: "a native Helm release",
			objects: []claimObject{obj(x, nil, map[string]string{helmReleaseNameAnno: "db", helmReleaseNSAnno: "shop"})},
			subject: x,
			want:    managerAnswer{state: managerResolved, manager: objectKey{kind: fluxHelmReleaseKind, namespace: "shop", name: "db"}, grade: gradeClaim, via: x}},
		{name: "a copied tracking id cannot break a tie between recorders",
			objects:     []claimObject{obj(x, nil, map[string]string{argoTrackingIDAnnotation: "argocd_shop:apps/Deployment:shop/original"})},
			memberships: map[objectKey]membershipFacts{app("argocd", "shop"): members(x), app("argocd", "other"): members(x)},
			subject:     x,
			want:        managerAnswer{state: managerAmbiguous, manager: app("argocd", "other"), grade: gradeRecorded, via: x, conflicts: []objectKey{app("argocd", "shop")}}},
		{name: "a malformed tracking id cannot break a tie between recorders",
			objects:     []claimObject{obj(x, nil, map[string]string{argoTrackingIDAnnotation: "argocd_shop:garbage"})},
			memberships: map[objectKey]membershipFacts{app("argocd", "shop"): members(x), app("argocd", "other"): members(x)},
			subject:     x,
			want:        managerAnswer{state: managerAmbiguous, manager: app("argocd", "other"), grade: gradeRecorded, via: x, conflicts: []objectKey{app("argocd", "shop")}}},
		{name: "a cluster-scoped object's tracking id carries the destination namespace",
			objects: []claimObject{obj(objectKey{group: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "shop"}, nil,
				map[string]string{argoTrackingIDAnnotation: "argocd_shop:rbac.authorization.k8s.io/ClusterRole:destination/shop"})},
			subject: objectKey{group: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "shop"},
			want:    managerAnswer{state: managerResolved, manager: app("argocd", "shop"), grade: gradeClaim, via: objectKey{group: "rbac.authorization.k8s.io", kind: "ClusterRole", name: "shop"}}},
		{name: "a core cluster-scoped object's tracking id carries the destination namespace",
			objects: []claimObject{obj(objectKey{kind: "Namespace", name: "shop"}, nil, map[string]string{argoTrackingIDAnnotation: "argocd_shop:/Namespace:destination/shop"})},
			subject: objectKey{kind: "Namespace", name: "shop"},
			want:    managerAnswer{state: managerResolved, manager: app("argocd", "shop"), grade: gradeClaim, via: objectKey{kind: "Namespace", name: "shop"}}},
		{name: "a replacement's owners are not the missing incarnation's ancestry",
			objects: []claimObject{
				obj(owner, nil, nil, controllerOf(objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "grand"})),
				obj(objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "grand"}, nil, nil),
				{testObject: testObject{key: x, uid: "x", owners: []metav1.OwnerReference{ownerRef("apps/v1", "Deployment", "owner", "older", true)}}, labels: ksLabels("a")},
			},
			memberships: map[objectKey]membershipFacts{ks("a"): members(objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "grand"})},
			subject:     x,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{manager: ks("a"), tool: toolFluxKustomization, grade: gradeClaim}, reason: rejectStale}}}},
		{name: "a withheld sole owner never yields to its own claim",
			objects:     []claimObject{obj(owner, nil, nil), obj(x, ksLabels("a"), nil, ownerRef("apps/v1", "Deployment", "owner", "owner", false))},
			memberships: map[objectKey]membershipFacts{ks("b"): members(owner)},
			hidden:      []objectKey{owner},
			subject:     x,
			want:        managerAnswer{state: managerWithheld}},
		{name: "a collapsed controller is inherited through",
			objects: []claimObject{
				{testObject: testObject{key: owner, uid: "owner", hop: true}},
				obj(x, nil, nil, controllerOf(owner)),
			},
			memberships: map[objectKey]membershipFacts{ks("a"): members(owner)},
			hidden:      []objectKey{owner},
			subject:     x,
			want:        managerAnswer{state: managerResolved, manager: ks("a"), grade: gradeRecorded, via: owner, inherited: true}},
		{name: "a native Helm hook is not part of its release",
			objects: []claimObject{obj(x, nil, map[string]string{helmReleaseNameAnno: "db", helmReleaseNSAnno: "shop", helmHookAnnotation: "post-install"})},
			subject: x,
			want:    managerAnswer{state: managerResolved, manager: objectKey{kind: fluxHelmReleaseKind, namespace: "shop", name: "db"}, grade: gradeHookOrigin, via: x}},
		{name: "an Argo CD instance claim the Application's resources refute is stale",
			objects:     []claimObject{obj(app("argocd", "shop"), nil, nil), obj(x, map[string]string{argoInstanceLabel: "shop"}, nil)},
			memberships: map[objectKey]membershipFacts{app("argocd", "shop"): members(owner)},
			subject:     x,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{manager: app("argocd", "shop"), tool: toolArgoInstance, grade: gradeClaim}, reason: rejectStale}}}},
		{name: "a claim naming a remote HelmRelease",
			objects:     []claimObject{obj(x, map[string]string{fluxHelmNameLabel: "release", fluxHelmNSLabel: "shop"}, nil)},
			memberships: map[objectKey]membershipFacts{{group: fluxHelmGroup, kind: fluxHelmReleaseKind, namespace: "shop", name: "release"}: {state: membershipRemote}},
			subject:     x,
			want: managerAnswer{state: managerNone, rejected: []rejectedClaim{
				{claim: managerClaim{manager: objectKey{group: fluxHelmGroup, kind: fluxHelmReleaseKind, namespace: "shop", name: "release"}, tool: toolFluxHelmRelease, grade: gradeClaim}, reason: rejectRemote}}}},
		{name: "equal-grade claims order by tool",
			objects: []claimObject{obj(x, map[string]string{
				fluxHelmNameLabel: "release", fluxHelmNSLabel: "shop",
				fluxKustomizeNameLabel: "a", fluxKustomizeNSLabel: "flux-system",
			}, nil)},
			subject: x,
			want: managerAnswer{state: managerResolved, manager: objectKey{group: fluxHelmGroup, kind: fluxHelmReleaseKind, namespace: "shop", name: "release"}, grade: gradeClaim, via: x,
				conflicts: []objectKey{ks("a")}}},
		{name: "nothing manages it",
			objects: []claimObject{obj(x, nil, nil)},
			subject: x,
			want:    managerAnswer{state: managerNone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var visible []objectKey
			for _, o := range tc.objects {
				if !slices.Contains(tc.hidden, o.key) {
					visible = append(visible, o.key)
				}
			}
			v := managerView(tc.objects, tc.memberships, tc.scopes, visible...)
			if got := v.managerOf(tc.subject); !managerAnswersEqual(got, tc.want) {
				t.Errorf("%+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestReleaseUnitStates(t *testing.T) {
	ks := objectKey{group: fluxKustomizeGroup, kind: fluxKustomizationKind, namespace: "flux-system", name: "a"}
	other := objectKey{group: fluxKustomizeGroup, kind: fluxKustomizationKind, namespace: "flux-system", name: "b"}
	owner := objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "owner"}
	x := objectKey{kind: "Pod", namespace: "shop", name: "x"}
	pod := func(owners ...metav1.OwnerReference) claimObject {
		return claimObject{testObject: testObject{key: x, uid: "x", owners: owners}}
	}
	controller := ownerRef("apps/v1", "Deployment", "owner", "owner", true)
	deploy := claimObject{testObject: testObject{key: owner, uid: "owner"}}

	ambiguous := managerView([]claimObject{pod()}, map[objectKey]membershipFacts{ks: members(x), other: members(x)}, nil, x).releaseUnit(x)
	if ambiguous.state != managerAmbiguous || ambiguous.unit != (objectKey{}) {
		t.Errorf("ambiguous: %+v; want no unit", ambiguous)
	}
	withheldManager := managerView([]claimObject{deploy, pod(controller)}, map[objectKey]membershipFacts{ks: members(owner)}, nil, x).releaseUnit(x)
	if withheldManager.state != managerWithheld || withheldManager.unit != (objectKey{}) {
		t.Errorf("withheld manager: %+v; want no unit", withheldManager)
	}
	withheldRoot := managerView([]claimObject{deploy, pod(controller)}, nil, nil, x).releaseUnit(x)
	if withheldRoot.state != managerWithheld || withheldRoot.unit != (objectKey{}) {
		t.Errorf("unmanaged, controller withheld: %+v; want no unit, never the partial root", withheldRoot)
	}
	root := managerView([]claimObject{deploy, pod(controller)}, nil, nil, x, owner).releaseUnit(x)
	if root.state != managerResolved || root.unit != owner || root.source != releaseControllerRoot {
		t.Errorf("unmanaged: %+v; want the controller root", root)
	}
}

// Fewer visible objects can only withhold a manager, never name another.
func TestManagerAnswersOnlyNarrowInProjections(t *testing.T) {
	full, fullView := charView(t, charSSEOptions())
	projections := []*Topology{
		charStrippedProjection(full),
		charProjection(full, map[NodeKind]bool{KindReplicaSet: true, KindDeployment: true}, nil),
	}
	for i, projected := range projections {
		view := newOwnershipView(projected)
		for key := range full.facts.objects {
			want, got := fullView.managerOf(key), view.managerOf(key)
			if !managerAnswersEqual(got, want) && got.state != managerWithheld {
				t.Errorf("projection %d: managerOf(%+v) = %+v, full build %+v", i, key, got, want)
			}
			wantUnit, gotUnit := fullView.releaseUnit(key), view.releaseUnit(key)
			if gotUnit.unit != wantUnit.unit && gotUnit.state != managerWithheld {
				t.Errorf("projection %d: releaseUnit(%+v) = %+v, full build %+v", i, key, gotUnit, wantUnit)
			}
		}
	}
}

// A manager recorded 16 references up is inherited; one 17 up is past the
// walk's bound.
func TestManagerInheritanceDepthBound(t *testing.T) {
	ks := objectKey{group: fluxKustomizeGroup, kind: fluxKustomizationKind, namespace: "flux-system", name: "a"}
	link := func(i int) objectKey {
		return objectKey{group: "example.io", kind: "Link", namespace: "x", name: fmt.Sprint(i)}
	}
	for _, tc := range []struct {
		hops int
		want managerState
	}{{maxControllerDepth, managerResolved}, {maxControllerDepth + 1, managerNone}} {
		var objects []claimObject
		var visible []objectKey
		for i := 0; i <= tc.hops; i++ {
			o := claimObject{testObject: testObject{key: link(i), uid: types.UID(fmt.Sprint(i))}}
			if i < tc.hops {
				o.owners = []metav1.OwnerReference{ownerRef("example.io/v1", "Link", fmt.Sprint(i+1), fmt.Sprint(i+1), true)}
			}
			objects = append(objects, o)
			visible = append(visible, link(i))
		}
		v := managerView(objects, map[objectKey]membershipFacts{ks: members(link(tc.hops))}, nil, visible...)
		if got := v.managerOf(link(0)); got.state != tc.want {
			t.Errorf("recorded %d references up: %+v, want state %d", tc.hops, got, tc.want)
		}
	}
}

func TestReleaseUnitSelfNeedsTheManagerGroup(t *testing.T) {
	impostor := objectKey{group: "example.io", kind: argoApplicationKind, namespace: "shop", name: "x"}
	v := managerView([]claimObject{{testObject: testObject{key: impostor, uid: "x"}}}, nil, nil, impostor)
	if got := v.releaseUnit(impostor); got.source == releaseSelf {
		t.Errorf("an Application of another API group is its own release unit: %+v", got)
	}
}

// Over random graphs and projections, a projection never names a manager or
// release unit the full view does not: it answers the same or withholds.
func TestManagerAnswersOnlyNarrowOnRandomProjections(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	ks := func(i int) objectKey {
		return objectKey{group: fluxKustomizeGroup, kind: fluxKustomizationKind, namespace: "flux-system", name: fmt.Sprint(i)}
	}
	for trial := 0; trial < 1000; trial++ {
		n := 3 + rng.IntN(7)
		key := func(i int) objectKey {
			return objectKey{group: "example.io", kind: "Thing", namespace: "x", name: fmt.Sprint(i)}
		}
		var objects []claimObject
		var keys []objectKey
		memberships := map[objectKey]membershipFacts{}
		for i := 0; i < n; i++ {
			o := claimObject{testObject: testObject{key: key(i), uid: types.UID(fmt.Sprint(i))}}
			for r := 0; r < rng.IntN(3); r++ {
				target := rng.IntN(n)
				if target != i {
					o.owners = append(o.owners, ownerRef("example.io/v1", "Thing", fmt.Sprint(target), fmt.Sprint(target), r == 0 && rng.IntN(2) == 0))
				}
			}
			if rng.IntN(3) == 0 {
				o.labels = ksLabels(fmt.Sprint(rng.IntN(3)))
			}
			if rng.IntN(4) == 0 {
				m := memberships[ks(rng.IntN(3))]
				if m.members == nil {
					m = members()
				}
				m.members[key(i)] = struct{}{}
				memberships[ks(rng.IntN(3))] = m
			}
			objects = append(objects, o)
			keys = append(keys, o.key)
		}
		full := managerView(objects, memberships, nil, keys...)
		var shown []objectKey
		for _, k := range keys {
			if rng.IntN(3) != 0 {
				shown = append(shown, k)
			}
		}
		projected := managerView(objects, memberships, nil, shown...)
		for _, k := range keys {
			want, got := full.managerOf(k), projected.managerOf(k)
			if !managerAnswersEqual(got, want) && got.state != managerWithheld {
				t.Fatalf("trial %d: managerOf(%s) = %+v in the projection, %+v in full", trial, k.name, got, want)
			}
			wantUnit, gotUnit := full.releaseUnit(k), projected.releaseUnit(k)
			if gotUnit.unit != wantUnit.unit && gotUnit.state != managerWithheld {
				t.Fatalf("trial %d: releaseUnit(%s) = %+v in the projection, %+v in full", trial, k.name, gotUnit, wantUnit)
			}
		}
	}
}
