package server

import (
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/skyhook-io/radar/internal/k8s"

	topology "github.com/skyhook-io/radar/pkg/topology"
)

func topoWithEdge(src, dst string) *topology.Topology {
	return &topology.Topology{
		Nodes: []topology.Node{{ID: src}, {ID: dst}},
		Edges: []topology.Edge{{ID: src + "->" + dst, Source: src, Target: dst, Type: topology.EdgeManages}},
	}
}

// GetCachedTopologyWithIndex must return a consistent (topo, index) pair, memoize
// the index across calls for the same topology, and rebuild it when the cached
// topology is replaced. These are the invariants the drawer-relationship
// optimization relies on.
func TestGetCachedTopologyWithIndex(t *testing.T) {
	b := NewSSEBroadcaster()

	// No topology cached yet → (nil, nil). (No k8s cache in tests, so
	// GetCachedTopology won't attempt a rebuild.)
	if topo, idx := b.GetCachedTopologyWithIndex(); topo != nil || idx != nil {
		t.Fatalf("expected (nil, nil) with no cached topology, got (%v, %v)", topo, idx)
	}

	topoA := topoWithEdge("deployment/ns/web", "replicaset/ns/web-1")
	b.updateCachedTopology(topoA, b.topoEpoch.Load(), 1)

	gotTopo, idxA := b.GetCachedTopologyWithIndex()
	if gotTopo != topoA {
		t.Fatalf("expected the cached topology pointer back")
	}
	if idxA == nil {
		t.Fatal("expected a non-nil index for a cached topology")
	}
	// The index reflects topoA's edge.
	if _, outgoing := idxA.EdgesFor("deployment/ns/web"); len(outgoing) != 1 {
		t.Fatalf("expected 1 outgoing edge for the deployment node, got %d", len(outgoing))
	}

	// Repeat call memoizes — same index pointer, no rebuild.
	_, idxA2 := b.GetCachedTopologyWithIndex()
	if idxA2 != idxA {
		t.Fatal("expected the memoized index pointer to be reused")
	}

	// Replacing the topology invalidates the index; the next call builds a fresh one.
	topoB := topoWithEdge("statefulset/ns/db", "pod/ns/db-0")
	b.updateCachedTopology(topoB, b.topoEpoch.Load(), 1)
	gotTopoB, idxB := b.GetCachedTopologyWithIndex()
	if gotTopoB != topoB {
		t.Fatalf("expected the replaced topology pointer back")
	}
	if idxB == nil || idxB == idxA {
		t.Fatal("expected a fresh index after the topology was replaced")
	}
	if _, outgoing := idxB.EdgesFor("statefulset/ns/db"); len(outgoing) != 1 {
		t.Fatalf("expected 1 outgoing edge in the new index, got %d", len(outgoing))
	}
	// The new index does not carry the old topology's nodes.
	if in, out := idxB.EdgesFor("deployment/ns/web"); len(in) != 0 || len(out) != 0 {
		t.Fatal("stale edge surfaced from a replaced topology's index")
	}

	// Clearing the topology (context-switch semantics) returns (nil, nil).
	b.updateCachedTopology(nil, b.topoEpoch.Load(), 1)
	if topo, idx := b.GetCachedTopologyWithIndex(); topo != nil || idx != nil {
		t.Fatalf("expected (nil, nil) after clearing topology, got (%v, %v)", topo, idx)
	}
}

// The broadcaster drops its graph when the switch callbacks run, but the new
// resource cache is published before they do. A reader holding that cache
// gets nothing rather than the previous cluster's graph.
func TestCachedTopologyForGenerationRejectsAnotherCache(t *testing.T) {
	useTestResourceCache(t, fake.NewClientset())
	current := k8s.GetResourceCache().Generation()
	b := NewSSEBroadcaster()
	graph := topoWithEdge("deployment/ns/old", "replicaset/ns/old-1")
	b.updateCachedTopology(graph, b.topoEpoch.Load(), current)
	if topo, idx := b.GetCachedTopologyForGeneration(current); topo != graph || idx == nil {
		t.Fatalf("same-generation reader lost the graph: %v %v", topo, idx)
	}
	if topo, idx := b.GetCachedTopologyForGeneration(current + 1); topo != nil || idx != nil {
		t.Fatalf("reader of another cache got this graph: %v", topo)
	}
}

// A switch publishes the new resource cache before its callbacks reset the
// broadcaster, and a failed init never runs them. A reader of the current
// cache rebuilds the shared graph once instead of falling back on every call;
// a reader still holding the replaced cache gets nothing.
func TestCachedTopologyForGenerationRepairsForTheCurrentCache(t *testing.T) {
	useTestResourceCache(t, fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "current-web", Namespace: "demo"}}))
	current := k8s.GetResourceCache().Generation()
	b := NewSSEBroadcaster()
	b.updateCachedTopology(topoWithEdge("deployment/demo/previous-web", "replicaset/demo/previous-web-1"), b.topoEpoch.Load(), current-1)

	topo, idx := b.GetCachedTopologyForGeneration(current)
	if topo == nil || idx == nil || !topologyHasNode(topo.Nodes, "current-web") || topologyHasNode(topo.Nodes, "previous-web") {
		t.Fatalf("reader of the current cache didn't get a graph built from it: %+v", topo)
	}
	if again, _ := b.GetCachedTopologyForGeneration(current); again != topo {
		t.Fatal("the repaired graph wasn't reused")
	}
	if stale, _ := b.GetCachedTopologyForGeneration(current - 1); stale != nil {
		t.Fatal("a reader of the replaced cache got the current cluster's graph")
	}
}

// The real build path stamps the generation it read, so a matching reader
// reuses the graph without a rebuild.
func TestRebuiltTopologyCarriesItsCacheGeneration(t *testing.T) {
	useTestResourceCache(t, fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"}}))
	b := NewSSEBroadcaster()
	if !b.rebuildCachedTopology() {
		t.Fatal("rebuild failed")
	}
	b.cachedTopologyMu.RLock()
	built := b.cachedTopology
	b.cachedTopologyMu.RUnlock()
	if topo, _ := b.cachedTopologyIfGeneration(k8s.GetResourceCache().Generation()); topo == nil || topo != built {
		t.Fatal("the built graph wasn't stamped with the generation of the cache it read")
	}
}

// Concurrent readers of the current cache share one repair: each gets the same
// rebuilt graph rather than starting its own full build.
func TestCachedTopologyRepairIsSingleFlight(t *testing.T) {
	useTestResourceCache(t, fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"}}))
	current := k8s.GetResourceCache().Generation()
	b := NewSSEBroadcaster()
	b.updateCachedTopology(topoWithEdge("deployment/demo/previous", "replicaset/demo/previous-1"), b.topoEpoch.Load(), current+1)

	const readers = 16
	got := make(chan *topology.Topology, readers)
	start := make(chan struct{})
	for range readers {
		go func() {
			<-start
			topo, _ := b.GetCachedTopologyForGeneration(current)
			got <- topo
		}()
	}
	close(start)
	var first *topology.Topology
	for range readers {
		topo := <-got
		if topo == nil {
			t.Fatal("a reader of the current cache got no graph")
		}
		if first == nil {
			first = topo
		} else if topo != first {
			t.Fatal("concurrent readers each rebuilt the graph")
		}
	}
}

// A dirty-flag rebuild and a generation repair share one build: a second
// reader arriving while the first rebuild is stalled must not start another,
// whether the graph it finds is stale or missing.
func TestDirtyRebuildAndRepairShareOneBuild(t *testing.T) {
	for _, stale := range []bool{true, false} {
		t.Run(map[bool]string{true: "stale graph", false: "no graph"}[stale], func(t *testing.T) {
			useTestResourceCache(t, fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"}}))
			current := k8s.GetResourceCache().Generation()
			b := NewSSEBroadcaster()
			if stale {
				b.updateCachedTopology(topoWithEdge("deployment/demo/previous", "replicaset/demo/previous-1"), b.topoEpoch.Load(), current+1)
			}
			b.markCachedTopologyDirty()

			var builds atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			real := rebuildCachedTopology
			rebuildCachedTopology = func(b *SSEBroadcaster) bool {
				if builds.Add(1) == 1 {
					close(entered)
					<-release
				}
				return real(b)
			}
			t.Cleanup(func() { rebuildCachedTopology = real })

			first := make(chan *topology.Topology, 1)
			go func() { topo, _ := b.GetCachedTopologyForGeneration(current); first <- topo }()
			<-entered
			second := make(chan *topology.Topology, 1)
			go func() { topo, _ := b.GetCachedTopologyForGeneration(current); second <- topo }()
			// Let the second reader reach the rebuild lock while the first holds it.
			time.Sleep(100 * time.Millisecond)
			close(release)
			a, c := <-first, <-second
			if a == nil || a != c {
				t.Fatalf("readers didn't share the rebuilt graph: %p %p", a, c)
			}
			if n := builds.Load(); n != 1 {
				t.Fatalf("%d builds for one repair", n)
			}
		})
	}
}
