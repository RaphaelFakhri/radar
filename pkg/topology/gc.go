package topology

import (
	"slices"

	"k8s.io/apimachinery/pkg/types"
)

// The garbage-collection estimate answers what Radar's delete would remove,
// with Kubernetes' garbage collector acting on the recorded ownerReferences
// the build observed. Radar's delete, ordinary or force, sends background
// propagation, as kubectl does. The force delete also strips root's
// finalizers first, which can end a policy already under way sooner; the
// collector work that policy started races either request alike, so both
// share one estimate. It is an estimate over what Radar observes, not a
// completion promise: an object the build never observed is never listed,
// and zero listed means zero observed. Only ownerReferences count; GitOps
// membership, spec links and display shortcuts never do.
//
// It rests on two assumptions it does not check. Every recorded reference's
// API version is still served: the collector looks an owner up by that
// version, so a removed one stalls the reference's holder, or, while the
// collector's mapping is stale, makes a live owner look gone. And the
// collector has already observed what the build observed: where a member
// deletes in the foreground or orphans, the collector acts on the dependents
// it has seen, so one created just before the delete can be orphaned or
// removed against the estimate.

// gcReason is why a dependent's fate is uncertain. A larger value is more
// specific and wins.
type gcReason uint8

const (
	// gcUnverifiable: one of its references cannot be verified here: withheld
	// by the projection, unrepresented, invalid-scope or conflicted.
	gcUnverifiable gcReason = iota + 1
	// gcPotential: it references an object whose own fate is uncertain.
	gcPotential
	// gcOrphaned: an owner the deletion removes carries the orphan finalizer
	// and drops its reference to it, so whether it is removed depends on the
	// order the collector acts in.
	gcOrphaned
	// gcInProgress: an owner outside the deletion is itself being deleted, so
	// it holds the object only until that deletion finishes or orphans it.
	gcInProgress
)

// gcEntry is one object the deletion would, or might, remove.
type gcEntry struct {
	key objectKey
	// shown: visible in the projection. An entry that is not shown must not
	// be named to its viewer.
	shown  bool
	reason gcReason
}

// gcEstimate lists, in key order, the objects deleting root would remove and
// those it might.
type gcEstimate struct {
	root      objectKey
	expected  []gcEntry
	uncertain []gcEntry
}

// gcLevel orders how far the deletion reaches an object.
type gcLevel uint8

const (
	// gcKept: not reached, or retained by an owner outside the deletion.
	gcKept gcLevel = iota
	gcUncertain
	gcExpected
)

// gcForeground is whether the collector deletes a member with foreground
// propagation, which removes its dependents even when it carries the orphan
// finalizer.
type gcForeground uint8

const (
	gcBackground gcForeground = iota
	gcMaybeForeground
	gcForegroundDeletion
)

// gcOrphaning is whether deleting a member orphans its dependents instead of
// removing them, unless it is deleted in the foreground.
type gcOrphaning uint8

const (
	gcNoOrphan gcOrphaning = iota
	gcMaybeOrphan
	gcOrphan
)

type gcMember struct {
	level      gcLevel
	reason     gcReason
	foreground gcForeground
	orphan     gcOrphaning
}

// gcRun is one estimate in progress: the object the request deletes, and
// every object classified into its deletion so far.
type gcRun struct {
	v       *ownershipView
	root    objectKey
	members map[objectKey]gcMember
	// awaited holds, once first needed, every UID under which the collector
	// files something that can be deleted in the foreground before the object
	// with that UID chooses its own policy.
	awaited map[types.UID]bool
}

// gcRootMember is root's place in its own deletion. The request's background
// policy replaces root's orphan or foreground finalizer, whatever its kind,
// so a live root's dependents go in the background. A root already being
// deleted under one of those finalizers keeps it while a Pod's graceful
// deletion is pending, and the collector may already be acting on it, so it
// may still orphan its dependents or delete them in the foreground.
func gcRootMember(facts objectFacts) gcMember {
	member := gcMember{level: gcExpected}
	if facts.deleting && facts.orphan {
		member.orphan = gcMaybeOrphan
	}
	if facts.deleting && facts.foreground {
		member.foreground = gcMaybeForeground
	}
	return member
}

// gcEstimate classifies every observed object holding a verified reference
// into the deletion of root. A dependent is reached through a member of the
// deletion unless that member carries the orphan finalizer and is not deleted
// in the foreground. It is kept when it has an owner outside the deletion that
// exists and is not itself being deleted; otherwise it is uncertain when a
// reference cannot be verified, points at an uncertain member, points at a
// member that may orphan its dependents, or names an owner being deleted;
// otherwise it is expected. A dependent an orphaning member releases without
// reaching is still uncertain when its other references may not hold it once
// released: one names an owner being deleted without orphaning, cannot be
// verified, or names a deleted incarnation.
//
// Classification only ever raises an object's level and foreground status as
// its owners join the deletion, so working through a queue of objects whose
// owners changed reaches the same answer in any order, visits each object a
// bounded number of times, and ends.
func (v *ownershipView) gcEstimate(root objectKey) (gcEstimate, bool) {
	rootFacts, observed := v.facts.object(root)
	if !observed || rootFacts.conflicted {
		return gcEstimate{}, false
	}
	estimate := gcEstimate{root: root}
	run := gcRun{v: v, root: root, members: map[objectKey]gcMember{root: gcRootMember(rootFacts)}}
	queued := map[objectKey]bool{}
	var queue []objectKey
	enqueueDependents := func(of objectKey) {
		for _, dependent := range v.index.dependents[of] {
			if dependent != root && !queued[dependent] {
				queued[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	enqueueDependents(root)
	for len(queue) > 0 {
		dependent := queue[0]
		queue = queue[1:]
		queued[dependent] = false
		next := run.classify(dependent)
		current := run.members[dependent]
		if next.level < current.level || next.foreground < current.foreground {
			// Unreachable while classification is monotone; holding the state
			// keeps the queue finite even if a future rule is not.
			continue
		}
		run.members[dependent] = next
		if next.level != current.level || next.foreground != current.foreground {
			enqueueDependents(dependent)
		}
	}
	for key, member := range run.members {
		entry := gcEntry{key: key, shown: v.visible[key], reason: member.reason}
		switch {
		case key == root:
		case member.level == gcExpected:
			estimate.expected = append(estimate.expected, entry)
		case member.level == gcUncertain:
			estimate.uncertain = append(estimate.uncertain, entry)
		}
	}
	byKey := func(a, b gcEntry) int { return compareObjectKeys(a.key, b.key) }
	slices.SortFunc(estimate.expected, byKey)
	slices.SortFunc(estimate.uncertain, byKey)
	return estimate, true
}

// awaitedByForegroundDeletion reports whether something the collector files
// under uid can be deleted in the foreground before the object with uid
// chooses its own policy, which makes the collector's cycle breaker unblock
// that object's owner references. The collector files an object under every
// UID its ownerReferences carry, whatever coordinates they record. Such an
// object may already be deleting in the foreground, root included. Or its
// reference may carry uid without naming the object that has it: the
// collector treats that reference as dangling, so it can delete the object at
// any time. An object whose reference names uid's object correctly is deleted
// only after that object is gone or already being deleted in the foreground.
func (r *gcRun) awaitedByForegroundDeletion(uid types.UID) bool {
	if r.awaited == nil {
		r.awaited = map[types.UID]bool{}
		for key, facts := range r.v.facts.objects {
			for _, owner := range facts.owners {
				if facts.deleting && facts.foreground || !r.v.resolve(key, owner).state.resolved() {
					r.awaited[owner.uid] = true
				}
			}
		}
	}
	return r.awaited[uid]
}

// classify decides one dependent against the current members.
func (r *gcRun) classify(dependent objectKey) gcMember {
	v := r.v
	facts, _ := v.facts.object(dependent)
	links, readable := v.references(dependent)
	if !readable {
		return gcMember{}
	}
	reached := false
	var reason gcReason
	note := func(r gcReason) {
		if r > reason {
			reason = r
		}
	}
	// released: an orphaning member drops its reference without reaching it;
	// exposed is why the references left might not hold it afterwards.
	released := false
	var exposed gcReason
	for _, link := range links {
		if !link.state.resolved() {
			if link.state == refReplaced {
				exposed = max(exposed, gcOrphaned)
			} else {
				note(gcUnverifiable)
				exposed = max(exposed, gcUnverifiable)
			}
			continue
		}
		owner, _ := v.facts.object(link.key)
		member := r.members[link.key]
		switch {
		case member.level > gcKept:
			switch {
			case member.orphan == gcNoOrphan || member.foreground == gcForegroundDeletion:
				reached = true
			case member.orphan == gcMaybeOrphan || member.foreground == gcMaybeForeground:
				reached = true
				note(gcOrphaned)
			default:
				released = true
				note(gcOrphaned)
			}
			if member.level == gcUncertain {
				note(gcPotential)
			}
			if link.state == refWithheld {
				note(gcUnverifiable)
				exposed = max(exposed, gcUnverifiable)
			}
		case link.state == refWithheld:
			note(gcUnverifiable)
			exposed = max(exposed, gcUnverifiable)
		case owner.deleting:
			// Solid to the collector only until its own deletion completes,
			// and then gone; an orphaning one drops the reference first.
			note(gcInProgress)
			if !owner.orphan {
				exposed = max(exposed, gcInProgress)
			}
		default:
			return gcMember{}
		}
	}
	orphan := gcNoOrphan
	if facts.orphan {
		orphan = gcOrphan
	}
	switch {
	case !reached && released && exposed != 0:
		return gcMember{level: gcUncertain, reason: exposed, foreground: r.foregroundOf(facts, links), orphan: orphan}
	case !reached:
		return gcMember{}
	case reason != 0:
		return gcMember{level: gcUncertain, reason: reason, foreground: r.foregroundOf(facts, links), orphan: orphan}
	}
	return gcMember{level: gcExpected, foreground: r.foregroundOf(facts, links), orphan: orphan}
}

// foregroundOf says whether the collector deletes a member in the
// foreground. Its own finalizer decides when it has the foreground finalizer
// or is already being deleted, since a deletion under way keeps the policy it
// started with. Otherwise it inherits the foreground from a blocking reference
// to a member deleted in the foreground only when that owner is its one
// existing owner and nothing the collector files under its UID can be deleted
// in the foreground first. With another owner still present the
// collector can drop the blocking reference first, and its cycle breaker
// unblocks the reference of an object whose dependents wait on it. Any other
// foreground owner, in the deletion or already being deleted outside it, may
// or may not decide it.
func (r *gcRun) foregroundOf(facts objectFacts, links []ownerLink) gcForeground {
	switch {
	case facts.foreground:
		return gcForegroundDeletion
	case facts.deleting:
		return gcBackground
	}
	existing := 0
	for _, link := range links {
		if link.state.resolved() {
			existing++
		}
	}
	foreground := gcBackground
	for i, link := range links {
		if !link.state.resolved() {
			continue
		}
		member := r.members[link.key]
		owner, _ := r.v.facts.object(link.key)
		switch {
		case member.level > gcKept && member.foreground == gcForegroundDeletion && facts.owners[i].blockOwnerDeletion && existing == 1:
			if r.awaitedByForegroundDeletion(facts.uid) {
				return gcMaybeForeground
			}
			return gcForegroundDeletion
		case member.level > gcKept && member.foreground > gcBackground,
			member.level == gcKept && owner.deleting && owner.foreground:
			foreground = gcMaybeForeground
		}
	}
	return foreground
}
