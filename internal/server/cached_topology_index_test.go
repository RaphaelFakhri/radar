package server

import (
	"testing"

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
	b := NewSSEBroadcaster()
	previous := topoWithEdge("deployment/ns/old", "replicaset/ns/old-1")
	b.updateCachedTopology(previous, b.topoEpoch.Load(), 1)
	if topo, idx := b.GetCachedTopologyForGeneration(1); topo != previous || idx == nil {
		t.Fatalf("same-generation reader lost the graph: %v %v", topo, idx)
	}
	if topo, idx := b.GetCachedTopologyForGeneration(2); topo != nil || idx != nil {
		t.Fatalf("reader of the new cache got the previous cluster's graph: %v", topo)
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
