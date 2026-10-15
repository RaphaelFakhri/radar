package topology

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func gcKeys(entries []gcEntry) []objectKey {
	keys := make([]objectKey, len(entries))
	for i, entry := range entries {
		keys[i] = entry.key
	}
	return keys
}

func TestGCEstimateInBuilds(t *testing.T) {
	_, view := charView(t, charSSEOptions())

	web, ok := view.gcEstimate(webDeploy)
	if !ok {
		t.Fatal("no estimate for a drawn Deployment")
	}
	widget := objectKey{group: "example.io", kind: "Widget", namespace: "shop", name: "web-widget"}
	var owned []objectKey
	pods := 0
	for _, entry := range web.expected {
		if entry.key.kind == "Pod" {
			pods++
		} else {
			owned = append(owned, entry.key)
		}
		if !entry.shown {
			t.Errorf("%+v is visible but not shown", entry.key)
		}
	}
	if !slices.Equal(owned, []objectKey{webRS, widget}) || pods == 0 || len(web.uncertain) != 0 {
		t.Fatalf("Deployment estimate = %+v; want its ReplicaSet, its Pods and the custom resource it controls, nothing uncertain", web)
	}

	big, _ := view.gcEstimate(bigRS)
	if len(big.expected) != 7 || len(big.uncertain) != 0 {
		t.Errorf("deleting the grouped ReplicaSet expects %d and is unsure of %d; want its 7 Pods", len(big.expected), len(big.uncertain))
	}

	nightly, _ := view.gcEstimate(nightlyCJ)
	if want := []objectKey{nightlyPod, olderJob, nightlyJob}; !slices.Equal(gcKeys(nightly.expected), want) {
		t.Errorf("CronJob estimate expects %+v, want %+v: a Job's reference to a deleted incarnation keeps nothing alive", gcKeys(nightly.expected), want)
	}

	api, _ := view.gcEstimate(objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "api"})
	if len(api.expected) != 0 || len(api.uncertain) != 0 {
		t.Errorf("a ReplicaSet naming the Deployment's previous incarnation is not its dependent: %+v", api)
	}

	if _, ok := view.gcEstimate(objectKey{kind: "Pod", namespace: "shop", name: "nope"}); ok {
		t.Error("an unobserved root has an estimate")
	}
}

// A projection that hides an intermediate can only make its dependents
// uncertain and hide what it lists, never retain or newly expect anything.
func TestGCEstimateOnlyNarrowsInProjections(t *testing.T) {
	full, fullView := charView(t, charSSEOptions())
	projections := []*Topology{
		charStrippedProjection(full),
		charProjection(full, map[NodeKind]bool{KindReplicaSet: true, KindJob: true, KindPodGroup: true}, nil),
	}
	roots := []objectKey{webDeploy, nightlyCJ, bigRS, objectKey{group: "apps", kind: "Deployment", namespace: "shop", name: "big"}}
	for i, projected := range projections {
		view := newOwnershipView(projected)
		for _, root := range roots {
			want, _ := fullView.gcEstimate(root)
			got, _ := view.gcEstimate(root)
			for _, entry := range got.expected {
				if !slices.Contains(gcKeys(want.expected), entry.key) {
					t.Errorf("projection %d, %s: expects %+v the full build does not", i, root.name, entry.key)
				}
			}
			for _, key := range gcKeys(want.expected) {
				if !slices.Contains(gcKeys(got.expected), key) && !slices.Contains(gcKeys(got.uncertain), key) {
					t.Errorf("projection %d, %s: %+v went from expected to retained", i, root.name, key)
				}
			}
			for _, key := range gcKeys(want.uncertain) {
				if !slices.Contains(gcKeys(got.uncertain), key) {
					t.Errorf("projection %d, %s: %+v is no longer uncertain", i, root.name, key)
				}
			}
		}
	}
}

func TestGCEstimateRules(t *testing.T) {
	root := objectKey{group: "example.io", kind: "Root", namespace: "x", name: "r"}
	key := func(kind, name string) objectKey {
		return objectKey{group: "example.io", kind: kind, namespace: "x", name: name}
	}
	ref := func(kind, name string, controller bool) metav1.OwnerReference {
		return ownerRef("example.io/v1", kind, name, name, controller)
	}
	obj := func(kind, name string, owners ...metav1.OwnerReference) testObject {
		return testObject{key: key(kind, name), uid: typesUID(name), owners: owners}
	}
	visibleAll := func(objects []testObject) []objectKey {
		var keys []objectKey
		for _, o := range objects {
			keys = append(keys, o.key)
		}
		return keys
	}
	type want struct {
		expected  []string
		uncertain map[string]gcReason
	}
	for _, tc := range []struct {
		name    string
		objects []testObject
		hidden  []string
		// mutate adjusts recorded lifecycle facts after capture.
		mutate func(objects map[objectKey]objectFacts)
		want   want
	}{
		{name: "a chain of controllers",
			objects: []testObject{obj("Root", "r"), obj("Mid", "m", ref("Root", "r", true)), obj("Leaf", "l", ref("Mid", "m", true))},
			want:    want{expected: []string{"l", "m"}}},
		{name: "a second owner outside the deletion retains",
			objects: []testObject{obj("Root", "r"), obj("Other", "o"), obj("Shared", "s", ref("Root", "r", true), ref("Other", "o", false))},
			want:    want{}},
		{name: "a second owner deleting in the foreground leaves it in progress",
			objects: []testObject{obj("Root", "r"), obj("Other", "o"), obj("Shared", "s", ref("Root", "r", true), ref("Other", "o", false))},
			mutate: func(objects map[objectKey]objectFacts) {
				f := objects[key("Other", "o")]
				f.deleting, f.foreground = true, true
				objects[key("Other", "o")] = f
			},
			want: want{uncertain: map[string]gcReason{"s": gcInProgress}}},
		{name: "a second owner deleting with ordinary finalizers holds it only until it is gone",
			objects: []testObject{obj("Root", "r"), obj("Other", "o"), obj("Shared", "s", ref("Root", "r", true), ref("Other", "o", false))},
			mutate: func(objects map[objectKey]objectFacts) {
				f := objects[key("Other", "o")]
				f.deleting = true
				objects[key("Other", "o")] = f
			},
			want: want{uncertain: map[string]gcReason{"s": gcInProgress}}},
		{name: "a reference to a deleted incarnation keeps nothing alive",
			objects: []testObject{obj("Root", "r"), obj("Other", "o"), obj("Shared", "s", ref("Root", "r", true), ownerRef("example.io/v1", "Other", "o", "previous", false))},
			want:    want{expected: []string{"s"}}},
		{name: "two owners both inside the deletion, either order",
			objects: []testObject{obj("Root", "r"), obj("A", "a", ref("Root", "r", true)), obj("B", "b", ref("Root", "r", true)), obj("Shared", "s", ref("B", "b", false), ref("A", "a", true))},
			want:    want{expected: []string{"a", "b", "s"}}},
		{name: "an unverifiable reference",
			objects: []testObject{obj("Root", "r"), obj("Shared", "s", ref("Root", "r", true), ownerRef("missing.io/v1", "Gone", "g", "g", false))},
			want:    want{uncertain: map[string]gcReason{"s": gcUnverifiable}}},
		{name: "a reference to an uncertain object",
			objects: []testObject{obj("Root", "r"), obj("U", "u", ref("Root", "r", true), ownerRef("missing.io/v1", "Gone", "g", "g", false)), obj("Child", "c", ref("U", "u", true))},
			want:    want{uncertain: map[string]gcReason{"u": gcUnverifiable, "c": gcPotential}}},
		{name: "a withheld owner outside the deletion",
			objects: []testObject{obj("Root", "r"), obj("Other", "o"), obj("Shared", "s", ref("Root", "r", true), ref("Other", "o", false))},
			hidden:  []string{"o"},
			want:    want{uncertain: map[string]gcReason{"s": gcUnverifiable}}},
		{name: "a withheld member makes its dependents uncertain",
			objects: []testObject{obj("Root", "r"), obj("Mid", "m", ref("Root", "r", true)), obj("Leaf", "l", ref("Mid", "m", true))},
			hidden:  []string{"m"},
			want:    want{expected: []string{"m"}, uncertain: map[string]gcReason{"l": gcUnverifiable}}},
		{name: "owners that keep each other alive",
			objects: []testObject{obj("Root", "r"), obj("A", "a", ref("Root", "r", true), ref("B", "b", false)), obj("B", "b", ref("Root", "r", true), ref("A", "a", false))},
			want:    want{}},
		{name: "an orphaning root removes nothing",
			objects: []testObject{obj("Root", "r"), obj("Mid", "m", ref("Root", "r", true))},
			mutate: func(objects map[objectKey]objectFacts) {
				f := objects[root]
				f.orphan = true
				objects[root] = f
			},
			want: want{}},
		{name: "an orphaning member is removed but its dependents are not reached through it",
			objects: []testObject{
				obj("Root", "r"), obj("Mid", "m", ref("Root", "r", true)),
				obj("Leaf", "l", ref("Mid", "m", true)),
				obj("Other", "o", ref("Root", "r", true)),
				// Removed through Other only if the collector orphans it from
				// Mid first; if Other goes first, Mid keeps it, then orphans it.
				obj("Both", "b", ref("Mid", "m", true), ref("Other", "o", false)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				f := objects[key("Mid", "m")]
				f.orphan = true
				objects[key("Mid", "m")] = f
			},
			want: want{expected: []string{"m", "o"}, uncertain: map[string]gcReason{"b": gcOrphaned}}},
		{name: "an uncertain orphaning member reaches nothing either",
			objects: []testObject{
				obj("Root", "r"),
				obj("Mid", "m", ref("Root", "r", true), ownerRef("missing.io/v1", "Gone", "g", "g", false)),
				obj("Leaf", "l", ref("Mid", "m", true)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				f := objects[key("Mid", "m")]
				f.orphan = true
				objects[key("Mid", "m")] = f
			},
			want: want{uncertain: map[string]gcReason{"m": gcUnverifiable}}},
		{name: "objects that own each other behind an orphaning member settle",
			objects: []testObject{
				obj("Root", "r"),
				obj("F", "f", ref("Root", "r", true)),
				obj("B", "b", ref("Root", "r", true), ref("F", "f", false)),
				obj("C", "c", ref("B", "b", true), ref("D", "d", false)),
				obj("D", "d", ref("C", "c", true)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				for name, set := range map[string]func(*objectFacts){
					"f": func(f *objectFacts) { f.deleting, f.foreground = true, true },
					"b": func(f *objectFacts) { f.orphan = true },
					"c": func(f *objectFacts) { f.deleting, f.foreground = true, true },
					"d": func(f *objectFacts) { f.deleting, f.foreground = true, true },
				} {
					k := key(map[string]string{"f": "F", "b": "B", "c": "C", "d": "D"}[name], name)
					f := objects[k]
					set(&f)
					objects[k] = f
				}
			},
			// B's reference to F does not block, so B may still be deleted in
			// the foreground before F finishes, or orphan C after it.
			want: want{expected: []string{"b", "f"}, uncertain: map[string]gcReason{"c": gcOrphaned, "d": gcPotential}}},
		{name: "hidden objects that own each other behind an orphaning member settle",
			objects: []testObject{
				obj("Root", "r"),
				obj("F", "f", ref("Root", "r", true)),
				obj("B", "b", ref("Root", "r", true), ref("F", "f", false)),
				obj("C", "c", ref("B", "b", true), ref("D", "d", false)),
				obj("D", "d", ref("C", "c", true)),
			},
			hidden: []string{"c", "d"},
			mutate: func(objects map[objectKey]objectFacts) {
				f := objects[key("F", "f")]
				f.deleting, f.foreground = true, true
				objects[key("F", "f")] = f
				b := objects[key("B", "b")]
				b.orphan = true
				objects[key("B", "b")] = b
			},
			// B's reference to F does not block, so B may still be deleted in
			// the foreground before F finishes, or orphan C after it.
			want: want{expected: []string{"b", "f"}, uncertain: map[string]gcReason{"c": gcOrphaned, "d": gcPotential}}},
		{name: "a member deleted in the foreground removes an orphaning member's dependents",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", blocking(ref("F", "f", true))), obj("C", "c", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"c", "f", "m"}}},
		{name: "foreground propagates down blocking references to an orphaning member",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("X", "x", blocking(ref("F", "f", true))),
				obj("M", "m", blocking(ref("X", "x", true))), obj("C", "c", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"c", "f", "m", "x"}}},
		{name: "a non-blocking reference races the foreground owner's completion",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", ref("F", "f", true)), obj("C", "c", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"f", "m"}, uncertain: map[string]gcReason{"c": gcOrphaned}}},
		{name: "a root deleted in the foreground removes an orphaning member's dependents",
			objects: []testObject{
				obj("Root", "r"), obj("M", "m", blocking(ref("Root", "r", true))), obj("C", "c", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, root, func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"c", "m"}}},
		{name: "a member already deleting with orphaning keeps its dependents",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", blocking(ref("F", "f", true))), obj("C", "c", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan, f.deleting = true, true })
			},
			want: want{expected: []string{"f", "m"}}},
		{name: "foreground does not pass through a member already being deleted",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("X", "x", blocking(ref("F", "f", true))),
				obj("M", "m", blocking(ref("X", "x", true))), obj("C", "c", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("X", "x"), func(f *objectFacts) { f.deleting = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"f", "m", "x"}}},
		{name: "a second owner can let the collector drop a blocking reference",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))), obj("A", "a", ref("Root", "r", true)),
				obj("M", "m", blocking(ref("F", "f", true)), ref("A", "a", false)), obj("C", "c", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"a", "f", "m"}, uncertain: map[string]gcReason{"c": gcOrphaned}}},
		{name: "a foreground owner outside the deletion may decide an orphaning member",
			objects: []testObject{
				obj("Root", "r"), obj("Out", "o"),
				obj("B", "b", ref("Root", "r", true), ref("Out", "o", false)), obj("C", "c", ref("B", "b", true)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("Out", "o"), func(f *objectFacts) { f.deleting, f.foreground = true, true })
				setFacts(objects, key("B", "b"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{uncertain: map[string]gcReason{"b": gcInProgress, "c": gcOrphaned}}},
		{name: "a dependent an orphaning member releases while its other owner is being deleted in the foreground",
			objects: []testObject{
				obj("Root", "r"), obj("Out", "o"), obj("M", "m", ref("Root", "r", true)),
				obj("D", "d", ref("M", "m", true), ref("Out", "o", false)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
				setFacts(objects, key("Out", "o"), func(f *objectFacts) { f.deleting, f.foreground = true, true })
			},
			want: want{expected: []string{"m"}, uncertain: map[string]gcReason{"d": gcInProgress}}},
		{name: "a dependent an orphaning member releases while its other owner finishes an ordinary deletion",
			objects: []testObject{
				obj("Root", "r"), obj("Out", "o"), obj("M", "m", ref("Root", "r", true)),
				obj("D", "d", ref("M", "m", true), ref("Out", "o", false)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
				setFacts(objects, key("Out", "o"), func(f *objectFacts) { f.deleting = true })
			},
			want: want{expected: []string{"m"}, uncertain: map[string]gcReason{"d": gcInProgress}}},
		{name: "a dependent two orphaning owners release survives",
			objects: []testObject{
				obj("Root", "r"), obj("Out", "o"), obj("M", "m", ref("Root", "r", true)),
				obj("D", "d", ref("M", "m", true), ref("Out", "o", false)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
				setFacts(objects, key("Out", "o"), func(f *objectFacts) { f.deleting, f.orphan = true, true })
			},
			want: want{expected: []string{"m"}}},
		{name: "a released dependent whose other reference names a deleted incarnation",
			objects: []testObject{
				obj("Root", "r"), obj("Other", "o"), obj("M", "m", ref("Root", "r", true)),
				obj("D", "d", ref("M", "m", true), ownerRef("example.io/v1", "Other", "o", "previous", false)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"m"}, uncertain: map[string]gcReason{"d": gcOrphaned}}},
		{name: "a released dependent whose other owner cannot be verified",
			objects: []testObject{
				obj("Root", "r"), obj("M", "m", ref("Root", "r", true)),
				obj("D", "d", ref("M", "m", true), ownerRef("missing.io/v1", "Gone", "g", "g", false)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"m"}, uncertain: map[string]gcReason{"d": gcUnverifiable}}},
		{name: "the cycle breaker can unblock a sole blocking reference",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", blocking(ref("F", "f", true))),
				obj("C", "c", blocking(ref("M", "m", true))), obj("D", "d", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
				setFacts(objects, key("C", "c"), func(f *objectFacts) { f.deleting, f.foreground = true, true })
			},
			// M's worker may clear its blocking reference because C waits on
			// it, fail its foreground delete on the changed object, and orphan
			// D once F, no longer blocked, is gone.
			want: want{expected: []string{"f", "m"}, uncertain: map[string]gcReason{"c": gcOrphaned, "d": gcOrphaned}}},
		{name: "a root deleted in the foreground can trigger the cycle breaker",
			objects: []testObject{
				obj("Root", "r", ref("M", "m", false)),
				obj("M", "m", blocking(ref("Root", "r", true))), obj("D", "d", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, root, func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			// The request makes the root a foreground-deleting dependent of M
			// before M's worker runs.
			want: want{expected: []string{"m"}, uncertain: map[string]gcReason{"d": gcOrphaned}}},
		{name: "a root deleted in the background does not",
			objects: []testObject{
				obj("Root", "r", ref("M", "m", false)), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", blocking(ref("F", "f", true))), obj("D", "d", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"d", "f", "m"}}},
		{name: "a dependent naming M's UID under other coordinates unblocks it too",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", blocking(ref("F", "f", true))), obj("X", "x"),
				obj("C", "c", ownerRef("example.io/v1", "X", "x", "m", true)),
				obj("D", "d", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
				setFacts(objects, key("C", "c"), func(f *objectFacts) { f.deleting, f.foreground = true, true })
			},
			// The collector files C under M by UID, though the reference
			// names X.
			want: want{expected: []string{"f", "m"}, uncertain: map[string]gcReason{"d": gcOrphaned}}},
		{name: "so does a live object naming M's UID under other coordinates, which the collector can delete first",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", blocking(ref("F", "f", true))), obj("X", "x"),
				obj("C", "c", ownerRef("example.io/v1", "X", "x", "m", true)),
				obj("D", "d", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
				setFacts(objects, key("C", "c"), func(f *objectFacts) { f.foreground = true })
			},
			want: want{expected: []string{"f", "m"}, uncertain: map[string]gcReason{"d": gcOrphaned}}},
		{name: "so does a root deleted in the foreground that names M's UID under other coordinates",
			objects: []testObject{
				obj("Root", "r", ownerRef("example.io/v1", "X", "x", "m", false)), obj("X", "x"),
				obj("M", "m", blocking(ref("Root", "r", true))), obj("D", "d", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, root, func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"m"}, uncertain: map[string]gcReason{"d": gcOrphaned}}},
		{name: "only a dependent already deleting in the foreground unblocks",
			objects: []testObject{
				obj("Root", "r"), obj("F", "f", blocking(ref("Root", "r", true))),
				obj("M", "m", blocking(ref("F", "f", true))),
				obj("C", "c", blocking(ref("M", "m", true))), obj("E", "e", blocking(ref("M", "m", true))),
				obj("Old", "o", ownerRef("example.io/v1", "M", "m", "previous", true)),
				obj("D", "d", blocking(ref("M", "m", true))),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("F", "f"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("M", "m"), func(f *objectFacts) { f.orphan = true })
				setFacts(objects, key("C", "c"), func(f *objectFacts) { f.foreground = true })
				setFacts(objects, key("E", "e"), func(f *objectFacts) { f.deleting = true })
				setFacts(objects, key("Old", "o"), func(f *objectFacts) { f.deleting, f.foreground = true, true })
			},
			want: want{expected: []string{"c", "d", "e", "f", "m"}}},
		{name: "an orphaning member's dependent with a reference to a deleted incarnation",
			objects: []testObject{
				obj("Root", "r"), obj("A", "a"), obj("O", "o", ref("Root", "r", true)),
				obj("M", "m", ref("O", "o", true), ownerRef("example.io/v1", "A", "a", "previous", false)),
				obj("C", "c", ref("M", "m", true)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				setFacts(objects, key("O", "o"), func(f *objectFacts) { f.orphan = true })
			},
			want: want{expected: []string{"o"}, uncertain: map[string]gcReason{"m": gcOrphaned, "c": gcPotential}}},
		{name: "a hidden uncertain member is not shown",
			objects: []testObject{
				obj("Root", "r"),
				obj("Mid", "m", ref("Root", "r", true), ownerRef("missing.io/v1", "Gone", "g", "g", false)),
			},
			hidden: []string{"m"},
			want:   want{uncertain: map[string]gcReason{"m": gcUnverifiable}}},
		{name: "the most specific reason wins",
			objects: []testObject{
				obj("Root", "r"), obj("Other", "o"),
				obj("U", "u", ref("Root", "r", true), ownerRef("missing.io/v1", "Gone", "g", "g", false)),
				obj("Both", "b", ref("U", "u", true), ref("Other", "o", false)),
			},
			mutate: func(objects map[objectKey]objectFacts) {
				f := objects[key("Other", "o")]
				f.deleting, f.foreground = true, true
				objects[key("Other", "o")] = f
			},
			want: want{uncertain: map[string]gcReason{"u": gcUnverifiable, "b": gcInProgress}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hidden := map[string]bool{}
			for _, name := range tc.hidden {
				hidden[name] = true
			}
			var visible []objectKey
			for _, key := range visibleAll(tc.objects) {
				if !hidden[key.name] {
					visible = append(visible, key)
				}
			}
			v := testView(tc.objects, visible...)
			if tc.mutate != nil {
				tc.mutate(v.facts.objects)
			}
			got, ok := v.gcEstimate(root)
			if !ok {
				t.Fatal("no estimate")
			}
			var expected []string
			for _, entry := range got.expected {
				expected = append(expected, entry.key.name)
				if entry.shown == hidden[entry.key.name] {
					t.Errorf("%s shown=%v", entry.key.name, entry.shown)
				}
			}
			slices.Sort(expected)
			if !slices.Equal(expected, tc.want.expected) {
				t.Errorf("expected %v, want %v", expected, tc.want.expected)
			}
			uncertain := map[string]gcReason{}
			for _, entry := range got.uncertain {
				uncertain[entry.key.name] = entry.reason
				if entry.shown == hidden[entry.key.name] {
					t.Errorf("%s shown=%v", entry.key.name, entry.shown)
				}
			}
			if !slices.IsSortedFunc(got.uncertain, func(a, b gcEntry) int { return compareObjectKeys(a.key, b.key) }) {
				t.Errorf("uncertain entries out of key order: %+v", got.uncertain)
			}
			if len(uncertain) != len(tc.want.uncertain) {
				t.Errorf("uncertain %v, want %v", uncertain, tc.want.uncertain)
			}
			for name, reason := range tc.want.uncertain {
				if uncertain[name] != reason {
					t.Errorf("uncertain %v, want %v", uncertain, tc.want.uncertain)
				}
			}
			if got.orphanRoot != (tc.name == "an orphaning root removes nothing") {
				t.Errorf("orphanRoot = %v", got.orphanRoot)
			}
		})
	}
}

// Renaming objects changes the order they are visited in, never the answer.
func TestGCEstimateDoesNotDependOnVisitOrder(t *testing.T) {
	build := func(names [4]string) map[string]string {
		root := objectKey{group: "example.io", kind: "Root", namespace: "x", name: "r"}
		obj := func(name string, owners ...metav1.OwnerReference) testObject {
			return testObject{key: objectKey{group: "example.io", kind: "Thing", namespace: "x", name: name}, uid: typesUID(name), owners: owners}
		}
		ref := func(name string) metav1.OwnerReference { return ownerRef("example.io/v1", "Thing", name, name, false) }
		a, b, c, d := names[0], names[1], names[2], names[3]
		objects := []testObject{
			{key: root, uid: "r"},
			// a and b are both dependents of root; c depends on a and b; d on c
			// and on an object outside the deletion that c's fate decides.
			obj(a, ownerRef("example.io/v1", "Root", "r", "r", true)),
			obj(b, ownerRef("example.io/v1", "Root", "r", "r", true), ref(a)),
			obj(c, ref(a), ref(b)),
			obj(d, ref(c)),
		}
		var keys []objectKey
		for _, o := range objects {
			keys = append(keys, o.key)
		}
		got, _ := testView(objects, keys...).gcEstimate(root)
		classes := map[string]string{}
		for _, e := range got.expected {
			classes[e.key.name] = "expected"
		}
		for _, e := range got.uncertain {
			classes[e.key.name] = "uncertain"
		}
		return classes
	}
	orders := [][4]string{{"a", "b", "c", "d"}, {"z", "y", "x", "w"}, {"m", "a", "z", "b"}}
	var answers []map[string]string
	for _, names := range orders {
		classes := build(names)
		// Translate back to roles a, b, c, d.
		byRole := map[string]string{}
		for i, role := range []string{"a", "b", "c", "d"} {
			byRole[role] = classes[names[i]]
		}
		answers = append(answers, byRole)
	}
	for i := 1; i < len(answers); i++ {
		for role, class := range answers[0] {
			if answers[i][role] != class {
				t.Fatalf("visit order changed the answer: %v vs %v", answers[0], answers[i])
			}
		}
	}
	if answers[0]["d"] != "expected" {
		t.Fatalf("the chain should be fully expected: %v", answers[0])
	}
}

func typesUID(s string) types.UID { return types.UID(s) }

// A long owner chain is walked once, not once per depth.
func TestGCEstimateWalksADeepChainOnce(t *testing.T) {
	const depth = 10000
	key := func(i int) objectKey {
		return objectKey{group: "example.io", kind: "Link", namespace: "x", name: fmt.Sprint(i)}
	}
	objects := []testObject{{key: key(0), uid: "0"}}
	visible := []objectKey{key(0)}
	for i := 1; i <= depth; i++ {
		objects = append(objects, testObject{key: key(i), uid: types.UID(fmt.Sprint(i)), owners: []metav1.OwnerReference{
			ownerRef("example.io/v1", "Link", fmt.Sprint(i-1), fmt.Sprint(i-1), true)}})
		visible = append(visible, key(i))
	}
	v := testView(objects, visible...)
	start := time.Now()
	got, _ := v.gcEstimate(key(0))
	if len(got.expected) != depth {
		t.Fatalf("expected %d of a %d-deep chain", len(got.expected), depth)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a %d-deep chain took %v", depth, elapsed)
	}
}

// The queue reaches the least fixed point of classification, which repeated
// whole-graph rounds from nothing also reach; along the way no object's level
// may fall, the property that makes any visit order end in the same answer.
func TestGCEstimateMatchesRepeatedRoundsOnRandomGraphs(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for trial := 0; trial < 3000; trial++ {
		v, root, keys := randomGCGraph(rng)
		got, _ := v.gcEstimate(root)

		rootMember := gcRootMember(root, v.facts.objects[root])
		if rootMember.orphan && (!got.orphanRoot || len(got.expected) != 0) {
			t.Fatalf("trial %d: an orphaning root is expected to remove %+v", trial, got.expected)
		}
		members := map[objectKey]gcMember{root: rootMember}
		for round := 0; ; round++ {
			next := map[objectKey]gcMember{root: rootMember}
			for _, key := range keys {
				if key == root {
					continue
				}
				member := (&gcRun{v: v, root: root, members: members}).classify(key)
				if was := members[key]; member.level < was.level || member.foreground < was.foreground {
					t.Fatalf("trial %d: %s fell from %+v to %+v", trial, key.name, was, member)
				}
				if member.level > gcKept {
					next[key] = member
				}
			}
			if mapsEqual(next, members) {
				break
			}
			if round > 3*len(keys) {
				t.Fatalf("trial %d: rounds did not settle", trial)
			}
			members = next
		}
		want := map[objectKey]gcMember{}
		for key, member := range members {
			if key != root {
				want[key] = gcMember{level: member.level, reason: member.reason}
			}
		}
		have := map[objectKey]gcMember{}
		for _, e := range got.expected {
			have[e.key] = gcMember{level: gcExpected}
		}
		for _, e := range got.uncertain {
			have[e.key] = gcMember{level: gcUncertain, reason: e.reason}
		}
		if !mapsEqual(have, want) {
			t.Fatalf("trial %d: queue %v, rounds %v", trial, have, want)
		}
	}
}

func mapsEqual[V comparable](a, b map[objectKey]V) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// randomGCGraph builds a small graph of objects with random owner
// references (controller or not, current or previous incarnation, or to an
// object never observed) and random lifecycle and visibility.
func randomGCGraph(rng *rand.Rand) (*ownershipView, objectKey, []objectKey) {
	n := 3 + rng.IntN(8)
	key := func(i int) objectKey {
		return objectKey{group: "example.io", kind: "Thing", namespace: "x", name: fmt.Sprint(i)}
	}
	var objects []testObject
	var keys, visible []objectKey
	for i := 0; i < n; i++ {
		o := testObject{key: key(i), uid: types.UID(fmt.Sprint(i))}
		if i > 0 {
			controller := rng.IntN(2) == 0
			for r := 0; r < 1+rng.IntN(3); r++ {
				target := rng.IntN(n)
				if target == i {
					continue
				}
				uid := fmt.Sprint(target)
				switch rng.IntN(10) {
				case 0:
					uid = "previous"
				case 1:
					o.owners = append(o.owners, ownerRef("missing.io/v1", "Gone", "g", "g", false))
					continue
				}
				ref := ownerRef("example.io/v1", "Thing", fmt.Sprint(target), uid, controller && r == 0)
				if rng.IntN(2) == 0 {
					block := true
					ref.BlockOwnerDeletion = &block
				}
				o.owners = append(o.owners, ref)
			}
		}
		objects = append(objects, o)
		keys = append(keys, o.key)
		if i == 0 || rng.IntN(8) != 0 {
			visible = append(visible, o.key)
		}
	}
	v := testView(objects, visible...)
	for i := 0; i < n; i++ {
		f := v.facts.objects[key(i)]
		switch rng.IntN(10) {
		case 0:
			f.deleting, f.foreground = true, true
		case 1:
			f.deleting, f.orphan = true, true
		case 2, 3:
			f.orphan = i > 0
		case 4:
			f.deleting = true
		case 5:
			f.foreground = true
		}
		v.facts.objects[key(i)] = f
	}
	return v, key(0), keys
}

func blocking(ref metav1.OwnerReference) metav1.OwnerReference {
	block := true
	ref.BlockOwnerDeletion = &block
	return ref
}

func setFacts(objects map[objectKey]objectFacts, key objectKey, set func(*objectFacts)) {
	f := objects[key]
	set(&f)
	objects[key] = f
}

// Radar deletes with empty options, so the API server picks the policy: the
// root's own finalizer, else its kind's default, which orphans a batch/v1
// Job's Pods.
func TestGCEstimateRootPolicy(t *testing.T) {
	_, view := charView(t, charSSEOptions())
	if got, _ := view.gcEstimate(nightlyJob); !got.orphanRoot || len(got.expected)+len(got.uncertain) != 0 {
		t.Errorf("deleting a Job: %+v; want its Pods orphaned", got)
	}
	if got, _ := view.gcEstimate(webDeploy); got.orphanRoot || len(got.expected) == 0 {
		t.Errorf("deleting a Deployment: %+v; want its ReplicaSet and Pods", got)
	}

	rc := objectKey{kind: "ReplicationController", namespace: "x", name: "rc"}
	pod := objectKey{kind: "Pod", namespace: "x", name: "p"}
	v := testView([]testObject{
		{key: rc, uid: "rc"},
		{key: pod, uid: "p", owners: []metav1.OwnerReference{ownerRef("v1", "ReplicationController", "rc", "rc", true)}},
	}, rc, pod)
	if got, _ := v.gcEstimate(rc); !got.orphanRoot || len(got.expected) != 0 {
		t.Errorf("deleting a ReplicationController: %+v; want its Pods orphaned", got)
	}

	root := objectKey{group: "example.io", kind: "Root", namespace: "x", name: "r"}
	m := objectKey{group: "example.io", kind: "M", namespace: "x", name: "m"}
	c := objectKey{group: "example.io", kind: "C", namespace: "x", name: "c"}
	chain := []testObject{
		{key: root, uid: "r"},
		{key: m, uid: "m", owners: []metav1.OwnerReference{blocking(ownerRef("example.io/v1", "Root", "r", "r", true))}},
		{key: c, uid: "c", owners: []metav1.OwnerReference{blocking(ownerRef("example.io/v1", "M", "m", "m", true))}},
	}
	v = testView(chain, root, m, c)
	setFacts(v.facts.objects, m, func(f *objectFacts) { f.orphan = true })
	if got, _ := v.gcEstimate(root); got.orphanRoot || !slices.Equal(gcKeys(got.expected), []objectKey{m}) {
		t.Errorf("a plain root: %+v; want M only, its orphan finalizer keeping C", got)
	}
	setFacts(v.facts.objects, root, func(f *objectFacts) { f.foreground = true })
	if got, _ := v.gcEstimate(root); !slices.Equal(gcKeys(got.expected), []objectKey{c, m}) {
		t.Errorf("a root with the foreground finalizer: %+v; want M deleted in the foreground through its blocking reference, and C", got)
	}
	setFacts(v.facts.objects, root, func(f *objectFacts) { f.foreground, f.orphan = false, true })
	if got, _ := v.gcEstimate(root); !got.orphanRoot || len(got.expected)+len(got.uncertain) != 0 {
		t.Errorf("a root with the orphan finalizer: %+v; want M orphaned", got)
	}
}

// An orphaning root releases its dependents rather than reaching them; one
// left holding only a reference to a deleted incarnation may still be
// collected, and so may what it owns.
func TestGCEstimateOrphaningRootReleasesDependents(t *testing.T) {
	root := objectKey{group: "example.io", kind: "Root", namespace: "x", name: "r"}
	a := objectKey{group: "example.io", kind: "A", namespace: "x", name: "a"}
	m := objectKey{group: "example.io", kind: "M", namespace: "x", name: "m"}
	c := objectKey{group: "example.io", kind: "C", namespace: "x", name: "c"}
	sole := objectKey{group: "example.io", kind: "S", namespace: "x", name: "s"}
	v := testView([]testObject{
		{key: root, uid: "r"}, {key: a, uid: "a"},
		{key: m, uid: "m", owners: []metav1.OwnerReference{ownerRef("example.io/v1", "Root", "r", "r", true), ownerRef("example.io/v1", "A", "a", "previous", false)}},
		{key: c, uid: "c", owners: []metav1.OwnerReference{ownerRef("example.io/v1", "M", "m", "m", true)}},
		{key: sole, uid: "s", owners: []metav1.OwnerReference{ownerRef("example.io/v1", "Root", "r", "r", true)}},
	}, root, a, m, c, sole)
	setFacts(v.facts.objects, root, func(f *objectFacts) { f.orphan = true })
	got, _ := v.gcEstimate(root)
	if !got.orphanRoot || len(got.expected) != 0 {
		t.Fatalf("orphaning root: %+v; want nothing expected", got)
	}
	reasons := map[objectKey]gcReason{}
	for _, e := range got.uncertain {
		reasons[e.key] = e.reason
	}
	if len(reasons) != 2 || reasons[m] != gcOrphaned || reasons[c] != gcPotential {
		t.Fatalf("uncertain %+v; want M (orphaned) and C (potential), and the sole dependent kept", got.uncertain)
	}
}
