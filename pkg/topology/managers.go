package topology

import (
	"slices"
	"strings"

	"github.com/skyhook-io/radar/pkg/resourceid"
)

// Manager resolution answers which GitOps or Helm release deploys an object,
// keeping what was observed apart from what is concluded. In order:
//
//  1. Recorded membership: a local manager's own status lists the object.
//  2. Inheritance through the object's controller reference, or through its
//     sole reference when it records no controller (marked weak). A withheld
//     reference answers withheld: the projection must not name a manager the
//     full build would not.
//  3. The object's own claims (labels and annotations), graded and vetted
//     against recorded membership.
//
// A named manager may itself be hidden from a projection's viewer; callers
// authorize a manager before naming it.

// managerState is the outcome of resolving a manager.
type managerState uint8

const (
	managerResolved managerState = iota + 1
	// managerNone: a completed negative; nothing manages the object.
	managerNone
	// managerWithheld: the answer rests on an object the projection hides.
	managerWithheld
	// managerAmbiguous: several managers record the object and its own
	// claims do not choose between them.
	managerAmbiguous
)

// managerGrade is how strong the evidence at the end of the chain is.
type managerGrade uint8

const (
	// gradeHookOrigin: a Helm release created the object as a hook; it is
	// not part of the release.
	gradeHookOrigin managerGrade = iota + 1
	// gradeClaim: the object's own metadata names the manager, unrefuted.
	gradeClaim
	// gradeRecorded: the manager's status lists the object.
	gradeRecorded
)

// claimTool orders surviving claims of equal grade.
type claimTool uint8

const (
	toolFluxHelmRelease claimTool = iota + 1
	toolFluxKustomization
	toolArgoTrackingID
	toolArgoInstance
	toolHelm
)

// rejectReason is why a claim on the object's metadata does not count.
type rejectReason uint8

const (
	// rejectStale: the named manager records its members, and neither the
	// object nor any of its origin ancestors is among them.
	rejectStale rejectReason = iota + 1
	// rejectRemote: the named manager applies to another cluster.
	rejectRemote
	// rejectCopied: an Argo CD tracking id that names another object, as a
	// copy made from a tracked object carries.
	rejectCopied
	// rejectIncomplete: the claim does not identify a manager.
	rejectIncomplete
)

type managerClaim struct {
	manager objectKey
	tool    claimTool
	grade   managerGrade
}

type rejectedClaim struct {
	claim  managerClaim
	reason rejectReason
}

type managerAnswer struct {
	state   managerState
	manager objectKey
	grade   managerGrade
	// inherited: decided by an object up the reference chain, via.
	inherited bool
	// weak: the chain passes through a sole reference that is not a
	// controller.
	weak bool
	// via is the object whose membership or claim decided; zero when the
	// answer is withheld.
	via objectKey
	// conflicts are other managers that record or claim the object, in key
	// order.
	conflicts []objectKey
	rejected  []rejectedClaim
}

const (
	fluxHelmReleaseKind   = "HelmRelease"
	fluxKustomizationKind = "Kustomization"
	argoApplicationKind   = "Application"
)

// isManagerKind reports an object that is itself a release manager.
func isManagerKind(key objectKey) bool {
	switch {
	case key.group == argoApplicationGroup && key.kind == argoApplicationKind,
		key.group == fluxKustomizeGroup && key.kind == fluxKustomizationKind,
		key.group == fluxHelmGroup && key.kind == fluxHelmReleaseKind:
		return true
	}
	return false
}

// managerOf resolves x's effective manager.
func (v *ownershipView) managerOf(x objectKey) managerAnswer {
	return v.managerOfChain(x, nil)
}

func (v *ownershipView) managerOfChain(x objectKey, below []objectKey) managerAnswer {
	facts, observed := v.facts.object(x)
	if !observed || facts.conflicted || len(below) > maxControllerDepth || slices.Contains(below, x) {
		return managerAnswer{state: managerNone}
	}
	if recorders := v.index.recorders[x]; len(recorders) > 0 {
		return v.recordedManager(x, facts, recorders)
	}
	links, _ := v.references(x)
	through, weak := -1, false
	if i := slices.IndexFunc(links, func(link ownerLink) bool { return link.controller }); i >= 0 {
		through = i
	} else if len(links) == 1 {
		through, weak = 0, true
	}
	if through >= 0 {
		switch link := links[through]; link.state {
		case refLive, refCollapsed:
			if up := v.managerOfChain(link.key, append(below, x)); up.state != managerNone {
				up.inherited = true
				up.weak = up.weak || weak
				return up
			}
		case refWithheld:
			return managerAnswer{state: managerWithheld}
		}
	}
	return v.claimedManager(x, facts)
}

// recordedManager decides between the managers whose status lists x: the
// only one, else the one x's own metadata names, else ambiguous.
func (v *ownershipView) recordedManager(x objectKey, facts objectFacts, recorders []objectKey) managerAnswer {
	answer := managerAnswer{state: managerResolved, grade: gradeRecorded, via: x, manager: recorders[0]}
	if len(recorders) == 1 {
		return answer
	}
	var named []objectKey
	for _, claim := range v.claimsOf(x, facts) {
		if identifiesManager(claim, x, facts) && slices.Contains(recorders, claim.manager) && !slices.Contains(named, claim.manager) {
			named = append(named, claim.manager)
		}
	}
	if len(named) == 1 {
		answer.manager = named[0]
	} else {
		answer.state = managerAmbiguous
	}
	for _, recorder := range recorders {
		if recorder != answer.manager {
			answer.conflicts = append(answer.conflicts, recorder)
		}
	}
	return answer
}

// claimedManager grades x's own claims: the strongest surviving claim wins,
// then tool order; the others it names stay as conflicts.
func (v *ownershipView) claimedManager(x objectKey, facts objectFacts) managerAnswer {
	var surviving []managerClaim
	var rejected []rejectedClaim
	var ancestors []objectKey
	ancestorsRead := false
	for _, claim := range v.claimsOf(x, facts) {
		if claim.manager.name == "" {
			rejected = append(rejected, rejectedClaim{claim: claim, reason: rejectIncomplete})
			continue
		}
		membership := v.facts.membership(claim.manager)
		switch {
		case membership.state == membershipRemote:
			rejected = append(rejected, rejectedClaim{claim: claim, reason: rejectRemote})
			continue
		case !identifiesManager(claim, x, facts):
			rejected = append(rejected, rejectedClaim{claim: claim, reason: rejectCopied})
			continue
		case membership.state == membershipRecorded && (claim.tool == toolFluxHelmRelease || claim.tool == toolFluxKustomization || claim.tool == toolArgoInstance):
			if !ancestorsRead {
				ancestors, ancestorsRead = v.originAncestors(x), true
			}
			if !recordsAny(membership, x, ancestors) {
				rejected = append(rejected, rejectedClaim{claim: claim, reason: rejectStale})
				continue
			}
		}
		surviving = append(surviving, claim)
	}
	if len(surviving) == 0 {
		return managerAnswer{state: managerNone, rejected: rejected}
	}
	slices.SortStableFunc(surviving, func(a, b managerClaim) int {
		if a.grade != b.grade {
			return int(b.grade) - int(a.grade)
		}
		return int(a.tool) - int(b.tool)
	})
	answer := managerAnswer{state: managerResolved, manager: surviving[0].manager, grade: surviving[0].grade, via: x, rejected: rejected}
	for _, claim := range surviving[1:] {
		if claim.manager != answer.manager && !slices.Contains(answer.conflicts, claim.manager) {
			answer.conflicts = append(answer.conflicts, claim.manager)
		}
	}
	slices.SortFunc(answer.conflicts, compareObjectKeys)
	return answer
}

// identifiesManager reports a claim that names a manager for x itself: any
// claim but a tracking id, which must also name x, as a copied one names the
// object it was copied from.
func identifiesManager(claim managerClaim, x objectKey, facts objectFacts) bool {
	if claim.manager.name == "" {
		return false
	}
	return claim.tool != toolArgoTrackingID || argoTrackingIDNames(facts.claims.argoTrackingID, x)
}

func recordsAny(membership membershipFacts, x objectKey, ancestors []objectKey) bool {
	if _, ok := membership.members[x]; ok {
		return true
	}
	for _, ancestor := range ancestors {
		if _, ok := membership.members[ancestor]; ok {
			return true
		}
	}
	return false
}

// claimsOf reads the manager claims on x's own metadata, in tool order. A
// claim that cannot identify its manager carries an empty manager name.
func (v *ownershipView) claimsOf(x objectKey, facts objectFacts) []managerClaim {
	c := facts.claims
	if c == nil {
		return nil
	}
	releaseGrade := gradeClaim
	if c.helmHook {
		releaseGrade = gradeHookOrigin
	}
	var claims []managerClaim
	if c.fluxHelmReleaseName != "" {
		claims = append(claims, managerClaim{tool: toolFluxHelmRelease, grade: releaseGrade,
			manager: completeKey(fluxHelmGroup, fluxHelmReleaseKind, c.fluxHelmReleaseNamespace, c.fluxHelmReleaseName)})
	}
	if c.fluxKustomizationName != "" {
		claims = append(claims, managerClaim{tool: toolFluxKustomization, grade: gradeClaim,
			manager: completeKey(fluxKustomizeGroup, fluxKustomizationKind, c.fluxKustomizationNamespace, c.fluxKustomizationName)})
	}
	if c.argoTrackingID != "" {
		claim := managerClaim{tool: toolArgoTrackingID, grade: gradeClaim}
		if app, _, ok := strings.Cut(c.argoTrackingID, ":"); ok {
			claim.manager = v.argoApplication(app)
		}
		claims = append(claims, claim)
	}
	if c.argoInstance != "" {
		claims = append(claims, managerClaim{tool: toolArgoInstance, grade: gradeClaim, manager: v.argoApplication(c.argoInstance)})
	}
	if c.helmReleaseName != "" {
		claims = append(claims, managerClaim{tool: toolHelm, grade: releaseGrade,
			manager: completeKey("", fluxHelmReleaseKind, c.helmReleaseNamespace, c.helmReleaseName)})
	}
	return claims
}

// completeKey is a namespaced manager's identity, or an empty one when the
// claim omits the namespace.
func completeKey(group, kind, namespace, name string) objectKey {
	if namespace == "" {
		return objectKey{}
	}
	return objectKey{group: group, kind: kind, namespace: namespace, name: name}
}

// argoApplication resolves the Application an Argo CD claim names:
// "<namespace>_<name>" when Applications live in any namespace, otherwise a
// bare name, which resolves to the one observed Application of that name and
// keeps an empty namespace when there is none or several.
func (v *ownershipView) argoApplication(value string) objectKey {
	key := objectKey{group: argoApplicationGroup, kind: argoApplicationKind}
	if namespace, name, ok := strings.Cut(value, "_"); ok {
		if namespace == "" || name == "" {
			return objectKey{}
		}
		key.namespace, key.name = namespace, name
		return key
	}
	key.name = value
	if apps := v.index.applicationsByName[value]; len(apps) == 1 {
		return apps[0]
	}
	return key
}

// argoTrackingIDNames reports whether an Argo CD tracking id
// ("<app>:<group>/<kind>:<namespace>/<name>") names x itself. Argo CD writes
// its destination namespace into the id of a cluster-scoped object, so the
// namespace is compared only for namespaced objects.
func argoTrackingIDNames(trackingID string, x objectKey) bool {
	parts := strings.Split(trackingID, ":")
	if len(parts) != 3 {
		return false
	}
	group, kind, ok := strings.Cut(parts[1], "/")
	if !ok {
		return false
	}
	namespace, name, ok := strings.Cut(parts[2], "/")
	return ok && resourceid.NormalizeGroup(group) == x.group && kind == x.kind && name == x.name &&
		(namespace == x.namespace || x.namespace == "")
}

// originAncestors are the exact identities up x's origin chain, nearest
// first: through controller references, or a sole reference, and including
// an owner the build never observed when its identity is exact. A reference
// whose owner kind's scope is unknown has no exact identity and ends the
// chain; one that names a deleted incarnation contributes its identity but
// ends the chain too, since the observed replacement's owners are not its.
func (v *ownershipView) originAncestors(x objectKey) []objectKey {
	var ancestors []objectKey
	for current := x; len(ancestors) < maxControllerDepth; {
		origin, ok := v.originParent(current)
		if !ok {
			break
		}
		key := origin.link.key
		switch origin.link.state {
		case refInvalidScope:
			return ancestors
		case refUnrepresented:
			if _, known := v.index.kindScope(resourceid.GroupKind{Group: key.group, Kind: key.kind}); !known {
				return ancestors
			}
		}
		if key == x || slices.Contains(ancestors, key) {
			break
		}
		ancestors = append(ancestors, key)
		if !origin.link.state.resolved() {
			break
		}
		current = key
	}
	return ancestors
}

// releaseSource says where a release unit came from.
type releaseSource uint8

const (
	releaseSelf releaseSource = iota + 1
	releaseManager
	releaseControllerRoot
)

// releaseUnitAnswer is the unit x is released as.
type releaseUnitAnswer struct {
	// state is resolved, withheld or ambiguous; never none.
	state  managerState
	unit   objectKey
	source releaseSource
	// manager is the resolution the unit rests on, unless x is a manager.
	manager managerAnswer
	// root is the controller root, when x has no manager.
	root rootAnswer
}

// releaseUnit is x itself when x is a manager, else its resolved manager,
// else its controller root with that root's own stop reason. A withheld or
// ambiguous manager names no unit: the caller anchors x locally instead. A
// controller root never stops at a withheld reference here: manager
// resolution follows the same controller references and would already have
// answered withheld.
func (v *ownershipView) releaseUnit(x objectKey) releaseUnitAnswer {
	if isManagerKind(x) {
		return releaseUnitAnswer{state: managerResolved, unit: x, source: releaseSelf}
	}
	manager := v.managerOf(x)
	switch manager.state {
	case managerResolved:
		return releaseUnitAnswer{state: managerResolved, unit: manager.manager, source: releaseManager, manager: manager}
	case managerNone:
		root := v.controllerRoot(x)
		return releaseUnitAnswer{state: managerResolved, unit: root.root, source: releaseControllerRoot, manager: manager, root: root}
	}
	return releaseUnitAnswer{state: manager.state, manager: manager}
}
