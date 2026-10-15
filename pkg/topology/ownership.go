package topology

import (
	"slices"
	"strings"

	"github.com/skyhook-io/radar/pkg/resourceid"
)

// Ownership answers what recorded ownerReferences say about the objects one
// projection of a build shows. Every answer comes from object facts, never
// from edges: a manages edge may stand for GitOps management, a spec link or a
// display shortcut, none of which is ownership.
//
// A reference resolves only to an object of the same group, Kind and name, in
// the dependent's namespace (namespaced owner kind) or at cluster scope
// (cluster-scoped owner kind), with the same UID. Fewer visible objects can
// only turn a live reference into a withheld one, never into a different
// owner.

// refState is what one recorded ownerReference resolves to in a projection.
type refState uint8

const (
	// refLive: resolved and visible in this projection.
	refLive refState = iota + 1
	// refWithheld: resolved in the build, hidden by the projection.
	refWithheld
	// refCollapsed: resolved to an intermediate the build crosses without
	// drawing it, such as a ReplicaSet hidden by the ReplicaSet toggle.
	refCollapsed
	// refUnrepresented: the build observed no object of that identity.
	refUnrepresented
	// refReplaced: the identity exists with a different UID; the reference
	// names a deleted incarnation.
	refReplaced
	// refInvalidScope: a cluster-scoped dependent names a namespaced owner
	// kind, which Kubernetes never resolves.
	refInvalidScope
	// refConflicted: the build observed two incarnations of the identity, so
	// neither can be verified.
	refConflicted
)

// resolved reports a reference that names an existing, verified object.
func (s refState) resolved() bool {
	return s == refLive || s == refWithheld || s == refCollapsed
}

// ownerLink is one recorded ownerReference and what it resolves to.
type ownerLink struct {
	key        objectKey
	controller bool
	state      refState
}

// ownershipIndex is derived from a build's facts once, on first use.
type ownershipIndex struct {
	// scopes records whether a kind's objects are namespaced, for every
	// kind discovery described at build time or the build observed.
	scopes map[resourceid.GroupKind]bool
	// dependents maps an observed object to the observed objects whose
	// recorded ownerReferences name its identity, in key order.
	dependents map[objectKey][]objectKey
	// recorders maps an identity to the local managers whose recorded
	// members include it, in key order.
	recorders map[objectKey][]objectKey
	// applicationsByName maps an Argo CD Application name to the observed
	// Applications of that name, in key order.
	applicationsByName map[string][]objectKey
}

func (r *factsRegistry) ownership() *ownershipIndex {
	r.ownershipOnce.Do(func() {
		idx := &ownershipIndex{
			scopes:             map[resourceid.GroupKind]bool{},
			dependents:         map[objectKey][]objectKey{},
			recorders:          map[objectKey][]objectKey{},
			applicationsByName: map[string][]objectKey{},
		}
		for key := range r.objects {
			idx.scopes[resourceid.GroupKind{Group: key.group, Kind: key.kind}] = key.namespace != ""
		}
		for gk, namespaced := range r.scopes {
			idx.scopes[gk] = namespaced
		}
		for key, facts := range r.objects {
			for _, owner := range facts.owners {
				ownerKey, state := idx.ownerKey(key, owner)
				if state != 0 {
					continue
				}
				if _, observed := r.objects[ownerKey]; !observed {
					continue
				}
				// One object's references are indexed together, so a second
				// reference to the same identity finds itself last.
				if deps := idx.dependents[ownerKey]; len(deps) == 0 || deps[len(deps)-1] != key {
					idx.dependents[ownerKey] = append(deps, key)
				}
			}
		}
		for _, dependents := range idx.dependents {
			slices.SortFunc(dependents, compareObjectKeys)
		}
		for manager, membership := range r.managers {
			if membership.state != membershipRecorded {
				continue
			}
			for member := range membership.members {
				idx.recorders[member] = append(idx.recorders[member], manager)
			}
		}
		for _, recorders := range idx.recorders {
			slices.SortFunc(recorders, compareObjectKeys)
		}
		for key := range r.objects {
			if key.group == argoApplicationGroup && key.kind == argoApplicationKind {
				idx.applicationsByName[key.name] = append(idx.applicationsByName[key.name], key)
			}
		}
		for _, apps := range idx.applicationsByName {
			slices.SortFunc(apps, compareObjectKeys)
		}
		r.ownershipIdx = idx
	})
	return r.ownershipIdx
}

// ownerKey is the identity a dependent's reference names, or the state that
// prevents it naming one.
func (idx *ownershipIndex) ownerKey(dependent objectKey, owner ownerFact) (objectKey, refState) {
	key := objectKey{group: owner.group, kind: owner.kind, name: owner.name}
	namespaced, known := idx.kindScope(resourceid.GroupKind{Group: owner.group, Kind: owner.kind})
	switch {
	case !known:
		return key, refUnrepresented
	case namespaced && dependent.namespace == "":
		return key, refInvalidScope
	case namespaced:
		key.namespace = dependent.namespace
	}
	return key, 0
}

// kindScope reports whether a kind's objects are namespaced: from the
// built-in table, else from discovery as the build recorded it, else from the
// objects of that kind the build observed. A kind none of them describes is
// one the build never saw, so nothing of it resolves.
func (idx *ownershipIndex) kindScope(gk resourceid.GroupKind) (namespaced, known bool) {
	if builtin, ok := resourceid.BuiltinForKind(gk.Kind); ok && builtin.Group == gk.Group {
		return builtin.Namespaced, true
	}
	namespaced, known = idx.scopes[gk]
	return namespaced, known
}

func compareObjectKeys(a, b objectKey) int {
	if c := strings.Compare(a.group, b.group); c != 0 {
		return c
	}
	if c := strings.Compare(a.kind, b.kind); c != 0 {
		return c
	}
	if c := strings.Compare(a.namespace, b.namespace); c != 0 {
		return c
	}
	return strings.Compare(a.name, b.name)
}

// ownershipView answers ownership questions for one projection: a built
// topology, or a per-user copy stripped from one. Building it is linear in
// the projection's nodes; answering is not.
type ownershipView struct {
	facts   *factsRegistry
	index   *ownershipIndex
	visible map[objectKey]bool
}

func newOwnershipView(t *Topology) *ownershipView {
	v := &ownershipView{facts: t.facts, visible: make(map[objectKey]bool, len(t.Nodes))}
	if t.facts == nil {
		v.facts = &factsRegistry{}
	}
	v.index = v.facts.ownership()
	for i := range t.Nodes {
		node := &t.Nodes[i]
		key := nodeObjectKey(node)
		if key.name == "" {
			continue
		}
		v.visible[key] = true
		// The same identities nodeResourceKeys gives: a Calico policy is
		// addressable through each API group that serves it.
		if IsCalicoPolicyKind(node.Kind) {
			for _, group := range calicoNodeAPIGroups(node) {
				if group != "" && group != key.group {
					alias := key
					alias.group = group
					v.visible[alias] = true
				}
			}
		}
		for _, member := range v.facts.groupMembers(node.ID) {
			v.visible[member] = true
		}
	}
	return v
}

// resolve says what one of dependent's recorded references resolves to.
func (v *ownershipView) resolve(dependent objectKey, owner ownerFact) ownerLink {
	key, state := v.index.ownerKey(dependent, owner)
	link := ownerLink{key: key, controller: owner.controller, state: state}
	if state != 0 {
		return link
	}
	facts, observed := v.facts.object(key)
	switch {
	case !observed:
		link.state = refUnrepresented
	case facts.conflicted:
		link.state = refConflicted
	case facts.uid != owner.uid:
		link.state = refReplaced
	case v.visible[key]:
		link.state = refLive
	case facts.hop:
		link.state = refCollapsed
	default:
		link.state = refWithheld
	}
	return link
}

// references are every ownerReference x records, in recorded order. An
// object the build did not observe, or observed twice with different UIDs,
// has none that can be read.
func (v *ownershipView) references(x objectKey) ([]ownerLink, bool) {
	facts, observed := v.facts.object(x)
	if !observed || facts.conflicted {
		return nil, false
	}
	links := make([]ownerLink, len(facts.owners))
	for i, owner := range facts.owners {
		links[i] = v.resolve(x, owner)
	}
	return links, true
}

// ownerAnswer is x's singular owner reference, if it has one.
type ownerAnswer struct {
	link ownerLink
	// several: no controller reference and more than one other reference,
	// so x has no singular owner.
	several bool
}

func (a ownerAnswer) live() bool { return a.link.state == refLive }

// owner is x's controller reference, or its sole reference when it records
// no controller, in whatever state that reference resolves to. A reference
// that is not live never gives way to a weaker one: a withheld controller
// answers withheld.
func (v *ownershipView) owner(x objectKey) ownerAnswer {
	links, ok := v.references(x)
	if !ok {
		return ownerAnswer{}
	}
	for _, link := range links {
		if link.controller {
			return ownerAnswer{link: link}
		}
	}
	switch len(links) {
	case 0:
		return ownerAnswer{}
	case 1:
		return ownerAnswer{link: links[0]}
	}
	return ownerAnswer{several: true}
}

// owners are x's live references.
func (v *ownershipView) owners(x objectKey) []ownerLink {
	links, _ := v.references(x)
	var live []ownerLink
	for _, link := range links {
		if link.state == refLive {
			live = append(live, link)
		}
	}
	return live
}

// childLink is a visible dependent whose reference to x is live.
type childLink struct {
	key        objectKey
	controller bool
}

// children are the visible objects, Pods inside a drawn group included,
// whose recorded references name x's current incarnation, in key order.
func (v *ownershipView) children(x objectKey) []childLink {
	if !v.visible[x] {
		return nil
	}
	var children []childLink
	for _, dependent := range v.index.dependents[x] {
		if !v.visible[dependent] {
			continue
		}
		links, _ := v.references(dependent)
		child := childLink{key: dependent}
		named := false
		for _, link := range links {
			if link.key == x && link.state == refLive {
				named = true
				child.controller = child.controller || link.controller
			}
		}
		if named {
			children = append(children, child)
		}
	}
	return children
}

// rootAnswer is where x's chain of controller references ends.
type rootAnswer struct {
	root objectKey
	// collapsed: root is an intermediate the build does not draw.
	collapsed bool
	// stop is the state of the controller reference the walk could not
	// follow, or of x itself when x cannot be read; zero when root records no
	// controller.
	stop refState
	// node: root's controller is a Node, a terminal never collapsed into;
	// stop still reports a Node reference that does not resolve.
	node bool
	// cycle: the references loop; root is the smallest identity in the loop
	// itself, so every object on or feeding the loop gets the same root.
	cycle bool
	// truncated: the chain is longer than maxControllerDepth references,
	// which only corrupt references produce; root is where the walk gave up.
	truncated bool
}

// maxControllerDepth bounds the walk; real chains are a few hops deep.
const maxControllerDepth = 16

// controllerRoot follows controller references only, through drawn and
// collapsed objects, and stops at the first reference it cannot follow,
// reporting why. It follows at most maxControllerDepth references.
func (v *ownershipView) controllerRoot(x objectKey) rootAnswer {
	answer := rootAnswer{root: x}
	switch facts, observed := v.facts.object(x); {
	case !observed:
		answer.stop = refUnrepresented
		return answer
	case facts.conflicted:
		answer.stop = refConflicted
		return answer
	}
	chain := []objectKey{x}
	for {
		links, _ := v.references(answer.root)
		i := slices.IndexFunc(links, func(link ownerLink) bool { return link.controller })
		if i < 0 {
			return answer
		}
		controller := links[i]
		followable := controller.state == refLive || controller.state == refCollapsed
		if controller.key.group == "" && controller.key.kind == "Node" {
			answer.node = true
			if !followable {
				answer.stop = controller.state
			}
			return answer
		}
		if !followable {
			answer.stop = controller.state
			return answer
		}
		if at := slices.Index(chain, controller.key); at >= 0 {
			answer.root = slices.MinFunc(chain[at:], compareObjectKeys)
			answer.collapsed = v.facts.objects[answer.root].hop
			answer.cycle = true
			return answer
		}
		if len(chain) > maxControllerDepth {
			answer.truncated = true
			return answer
		}
		chain = append(chain, controller.key)
		answer.root = controller.key
		answer.collapsed = controller.state == refCollapsed
	}
}

// originAnswer is the reference x's management is inherited through.
type originAnswer struct {
	link ownerLink
	// weak: the sole recorded reference, which is not a controller.
	weak bool
}

// originParent is x's controller reference, otherwise its sole reference
// marked weak, in whatever state it resolves to: an unrepresented controller
// still names x's origin when claims are vetted against ancestry.
func (v *ownershipView) originParent(x objectKey) (originAnswer, bool) {
	links, ok := v.references(x)
	if !ok {
		return originAnswer{}, false
	}
	for _, link := range links {
		if link.controller {
			return originAnswer{link: link}, true
		}
	}
	if len(links) == 1 {
		return originAnswer{link: links[0], weak: true}, true
	}
	return originAnswer{}, false
}
