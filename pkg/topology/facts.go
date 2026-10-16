package topology

import (
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skyhook-io/radar/pkg/resourceid"
)

// Object facts are what one Build observed about each Kubernetes object it
// turned into a node or a grouped member: the incarnation, the recorded
// ownerReferences, the GitOps and Helm claims on the object's own metadata,
// and its deletion state. They also record, marked as hops, the objects a
// build crosses or counts without drawing: a resources build's undrawn
// ReplicaSets that in-scope Pods name as their controller, and the Pods a
// summary-mode build counts instead of drawing, so ownership resolves the same
// whichever way the Pod tier is rendered. Membership facts are the member
// lists GitOps managers record in their status. Both are keyed by exact
// resource identity, frozen when Build returns, and never mutated afterwards,
// so every projection of a topology can share them.
//
// Facts are evidence, not answers. A fact about an object Radar did not render
// says nothing about whether the caller may see it; queries resolve visible
// nodes first and consult facts only for them.

// objectKey is an exact resource identity: the same group, Kind, namespace
// and name resourceid.ResourceKey encodes, kept as a comparable struct so
// capturing a key never allocates.
type objectKey struct {
	group     string
	kind      string
	namespace string
	name      string
}

// objectFacts is one object's observation. All fields are copies: nothing
// retains the informer object or its maps.
type objectFacts struct {
	uid    types.UID
	owners []ownerFact
	claims *claimFacts
	// deleting is set when the object has a deletionTimestamp; foreground and
	// orphan record the matching finalizers.
	deleting   bool
	foreground bool
	orphan     bool
	// conflicted marks a key observed with two different UIDs in one build.
	// Its incarnation is unknown, so nothing may be resolved against it.
	conflicted bool
	// hop marks an object the build crosses or counts without drawing it as
	// a node or a group member.
	hop bool
}

// ownerFact is one recorded ownerReference, in exact API-group terms.
type ownerFact struct {
	group              string
	kind               string
	name               string
	uid                types.UID
	controller         bool
	blockOwnerDeletion bool
}

// claimFacts are the management claims an object's own metadata makes. Values
// are recorded as written; grading them against recorded inventories is the
// caller's job.
type claimFacts struct {
	fluxKustomizationName      string
	fluxKustomizationNamespace string
	fluxHelmReleaseName        string
	fluxHelmReleaseNamespace   string
	argoTrackingID             string
	argoInstallationID         string
	argoInstance               string
	helmReleaseName            string
	helmReleaseNamespace       string
	helmHook                   bool
}

const (
	helmHookAnnotation           = "helm.sh/hook"
	argoInstallationIDAnnotation = "argocd.argoproj.io/installation-id"
	foregroundDeletionFinalizer  = "foregroundDeletion"
	orphanFinalizer              = "orphan"
)

// membershipState is what a GitOps manager's status records about the
// objects it applies.
type membershipState uint8

const (
	// membershipAbsent: no member list Radar reads — the manager has not
	// reconciled yet, or its kind (a HelmRelease) records none Radar reads.
	membershipAbsent membershipState = iota
	// membershipRecorded: the status lists the members; the list may be empty.
	membershipRecorded
	// membershipRemote: the manager applies to another cluster, so its list
	// names objects there and matches nothing here.
	membershipRemote
)

// membershipFacts is one manager's recorded member list.
type membershipFacts struct {
	state   membershipState
	members map[objectKey]struct{}
}

// factsRegistry is the frozen, read-only set of facts a Topology carries.
type factsRegistry struct {
	objects map[objectKey]objectFacts
	// members maps a synthetic PodGroup node ID to the Pods it stands for.
	members map[string][]objectKey
	// managers maps a GitOps manager's identity to its recorded members.
	managers map[objectKey]membershipFacts
	// scopes is discovery's answer, at build time, to whether each custom
	// owner kind the build's ownerReferences name is namespaced. It is how an
	// owner the build never observed still resolves to an exact identity.
	scopes map[resourceid.GroupKind]bool
}

// object returns the facts recorded for an exact identity.
func (r *factsRegistry) object(key objectKey) (objectFacts, bool) {
	if r == nil {
		return objectFacts{}, false
	}
	facts, ok := r.objects[key]
	return facts, ok
}

// groupMembers returns the member identities of a synthetic group node.
func (r *factsRegistry) groupMembers(nodeID string) []objectKey {
	if r == nil {
		return nil
	}
	return r.members[nodeID]
}

// membership returns a manager's recorded members; a manager this build did
// not record is absent.
func (r *factsRegistry) membership(manager objectKey) membershipFacts {
	if r == nil {
		return membershipFacts{}
	}
	return r.managers[manager]
}

// factsCapture accumulates observations during one Build. A nil capture
// records nothing, so helpers that run outside Build (tests calling a single
// producer) need no special handling.
type factsCapture struct {
	objects  map[objectKey]objectFacts
	members  map[string][]objectKey
	managers map[objectKey]membershipFacts
	// ownerKinds are the kinds the observed ownerReferences name.
	ownerKinds map[resourceid.GroupKind]struct{}
	scopes     map[resourceid.GroupKind]bool
}

func newFactsCapture(sizeHint int) *factsCapture {
	return &factsCapture{
		objects:    make(map[objectKey]objectFacts, sizeHint),
		members:    map[string][]objectKey{},
		managers:   map[objectKey]membershipFacts{},
		ownerKinds: map[resourceid.GroupKind]struct{}{},
	}
}

// freeze hands the observations to the topology. The capture must not be used
// afterwards.
func (c *factsCapture) freeze() *factsRegistry {
	if c == nil {
		return nil
	}
	return &factsRegistry{objects: c.objects, members: c.members, managers: c.managers, scopes: c.scopes}
}

// observe records obj, which the build draws as a node or a group member,
// under key. Observing the same incarnation twice is a no-op; a different
// UID under the same key marks the key conflicted.
func (c *factsCapture) observe(key objectKey, obj metav1.Object) {
	c.record(key, obj, false)
}

// observeHop records obj, which the build crosses or counts without drawing.
func (c *factsCapture) observeHop(key objectKey, obj metav1.Object) {
	c.record(key, obj, true)
}

func (c *factsCapture) record(key objectKey, obj metav1.Object, hop bool) {
	if c == nil || key.name == "" || obj == nil {
		return
	}
	if existing, ok := c.objects[key]; ok {
		if existing.uid != obj.GetUID() && !existing.conflicted {
			existing.conflicted = true
			c.objects[key] = existing
		} else if existing.hop && !hop {
			existing.hop = false
			c.objects[key] = existing
		}
		return
	}
	facts := factsFor(obj)
	facts.hop = hop
	c.objects[key] = facts
	for _, owner := range facts.owners {
		c.ownerKinds[resourceid.GroupKind{Group: owner.group, Kind: owner.kind}] = struct{}{}
	}
}

// recordKindScopes asks discovery whether each custom owner kind the observed
// references name is namespaced. Built-in kinds are answered by the built-in
// table and are not asked.
func (c *factsCapture) recordKindScopes(discovery DynamicProvider) {
	if c == nil || discovery == nil {
		return
	}
	for gk := range c.ownerKinds {
		if b, builtin := resourceid.BuiltinForKind(gk.Kind); (builtin && b.Group == gk.Group) || gk.Group == "" {
			continue
		}
		if resource, found := discovery.GetResourceForGroupKind(gk.Group, gk.Kind); found {
			if c.scopes == nil {
				c.scopes = map[resourceid.GroupKind]bool{}
			}
			c.scopes[gk] = resource.Namespaced
		}
	}
}

// unstructuredKey is a dynamic object's exact identity.
func unstructuredKey(obj *unstructured.Unstructured) objectKey {
	return objectKey{group: resourceid.GroupFromAPIVersion(obj.GetAPIVersion()), kind: obj.GetKind(), namespace: obj.GetNamespace(), name: obj.GetName()}
}

// recordMembership records a manager's member list. The first record for a
// manager wins.
func (c *factsCapture) recordMembership(manager objectKey, facts membershipFacts) {
	if c == nil || manager.name == "" {
		return
	}
	if _, ok := c.managers[manager]; !ok {
		c.managers[manager] = facts
	}
}

// observedNode records the facts of the object a node is built from and
// returns the node unchanged. Every node built from a Kubernetes object goes
// through it; nodes that stand for no single object use syntheticNode.
func observedNode(c *factsCapture, obj metav1.Object, n Node) Node {
	if c != nil {
		c.observe(nodeObjectKey(&n), obj)
	}
	return n
}

// syntheticNode marks a node that stands for no single observed object: a Pod
// group, the Internet, a resource known only from another object's status or
// reference. It records nothing and returns the node unchanged.
func syntheticNode(n Node) Node {
	return n
}

// observeGroupMembers records the Pods a synthetic group node stands for.
func observeGroupMembers(c *factsCapture, groupNodeID string, pods []*corev1.Pod) {
	if c == nil {
		return
	}
	members := slices.Grow(c.members[groupNodeID], len(pods))
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		key := podKey(pod)
		c.observe(key, pod)
		members = append(members, key)
	}
	c.members[groupNodeID] = members
}

func podKey(pod *corev1.Pod) objectKey {
	return objectKey{kind: "Pod", namespace: pod.Namespace, name: pod.Name}
}

func replicaSetKey(rs *appsv1.ReplicaSet) objectKey {
	return objectKey{group: "apps", kind: "ReplicaSet", namespace: rs.Namespace, name: rs.Name}
}

// nodeObjectKey is a node's primary identity: the first of nodeResourceKeys.
// A node served through a second API group (Calico) records that alias
// separately when the producer observes it.
func nodeObjectKey(n *Node) objectKey {
	if n == nil || n.Name == "" {
		return objectKey{}
	}
	namespace := nodeNamespaceFromData(n)
	if namespace == "" {
		if parts := strings.Split(n.ID, "/"); len(parts) >= 3 {
			namespace = parts[1]
		}
	}
	return objectKey{group: nodeGroup(n), kind: KubernetesKindForNode(n), namespace: namespace, name: n.Name}
}

func factsFor(obj metav1.Object) objectFacts {
	facts := objectFacts{uid: obj.GetUID(), deleting: obj.GetDeletionTimestamp() != nil}
	for _, finalizer := range obj.GetFinalizers() {
		switch finalizer {
		case foregroundDeletionFinalizer:
			facts.foreground = true
		case orphanFinalizer:
			facts.orphan = true
		}
	}
	if refs := obj.GetOwnerReferences(); len(refs) > 0 {
		facts.owners = make([]ownerFact, len(refs))
		for i, ref := range refs {
			facts.owners[i] = ownerFact{
				group:              resourceid.GroupFromAPIVersion(ref.APIVersion),
				kind:               ref.Kind,
				name:               ref.Name,
				uid:                ref.UID,
				controller:         ref.Controller != nil && *ref.Controller,
				blockOwnerDeletion: ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion,
			}
		}
	}
	facts.claims = claimsFor(obj.GetLabels(), obj.GetAnnotations())
	return facts
}

func claimsFor(labels, annotations map[string]string) *claimFacts {
	claims := claimFacts{
		fluxKustomizationName:      labels[fluxKustomizeNameLabel],
		fluxKustomizationNamespace: labels[fluxKustomizeNSLabel],
		fluxHelmReleaseName:        labels[fluxHelmNameLabel],
		fluxHelmReleaseNamespace:   labels[fluxHelmNSLabel],
		argoTrackingID:             annotations[argoTrackingIDAnnotation],
		argoInstallationID:         annotations[argoInstallationIDAnnotation],
		argoInstance:               labels[argoInstanceLabel],
		helmReleaseName:            annotations[helmReleaseNameAnno],
		helmReleaseNamespace:       annotations[helmReleaseNSAnno],
	}
	_, claims.helmHook = annotations[helmHookAnnotation]
	if claims == (claimFacts{}) {
		return nil
	}
	recorded := new(claimFacts)
	*recorded = claims
	return recorded
}
