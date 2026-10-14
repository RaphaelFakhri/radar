package mcp

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/topology"
)

// Each test seeds summaryCtxTopoMemo through one call site while the cache
// holds the cluster the user is leaving, swaps the resource cache the way a
// context switch or namespace rescope does, then asks the same call site
// again. A call site that ignores the cache generation is handed the
// previous cluster's graph.

func useSwitchTestCluster(t *testing.T, deployment string) {
	t.Helper()
	replicas := int32(1)
	k8s.ResetResourceCache()
	if err := k8s.InitTestResourceCache(fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo"}},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: deployment, Namespace: "demo"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": deployment}},
			},
		},
	)); err != nil {
		t.Fatalf("init test resource cache: %v", err)
	}
}

func freshSummaryCtxTopoMemo(t *testing.T) {
	t.Helper()
	prev := summaryCtxTopoMemo
	summaryCtxTopoMemo = topology.NewMemoizer(5 * time.Second)
	t.Cleanup(func() { summaryCtxTopoMemo = prev })
	t.Cleanup(k8s.ResetTestState)
}

func hasNode(topo *topology.Topology, name string) bool {
	if topo == nil {
		return false
	}
	for _, n := range topo.Nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

func TestSummaryContextTopologyAfterSwitch(t *testing.T) {
	freshSummaryCtxTopoMemo(t)
	useSwitchTestCluster(t, "old-cluster-web")
	if before := buildSummaryContextTopology(k8s.GetResourceCache(), []string{"demo"}); !hasNode(before, "old-cluster-web") {
		t.Fatal("seed: expected old-cluster-web in the summary-context topology")
	}

	useSwitchTestCluster(t, "new-cluster-api")
	after := buildSummaryContextTopology(k8s.GetResourceCache(), []string{"demo"})
	if hasNode(after, "old-cluster-web") || !hasNode(after, "new-cluster-api") {
		t.Errorf("post-switch summary-context topology still describes the previous cluster: %+v", after)
	}
}

func TestResourceContextTopologyAfterSwitch(t *testing.T) {
	freshSummaryCtxTopoMemo(t)
	useSwitchTestCluster(t, "old-cluster-web")
	if before, _, _, _, ok := mcpTopologyForContext(); !ok || !hasNode(before, "old-cluster-web") {
		t.Fatalf("seed: expected old-cluster-web in the resource-context topology, got ok=%v", ok)
	}

	useSwitchTestCluster(t, "new-cluster-api")
	after, _, _, _, ok := mcpTopologyForContext()
	if !ok {
		t.Fatal("post-switch topology unavailable")
	}
	if hasNode(after, "old-cluster-web") || !hasNode(after, "new-cluster-api") {
		t.Errorf("post-switch resource-context topology still describes the previous cluster: %+v", after.Nodes)
	}
}

func TestResourceContextUsesHostRelationshipGraph(t *testing.T) {
	freshSummaryCtxTopoMemo(t)
	useSwitchTestCluster(t, "cluster-web")
	host := &topology.Topology{Nodes: []topology.Node{{ID: "deployment/demo/host-graph", Kind: "Deployment", Name: "host-graph"}}}
	index := topology.IndexByResource(host)
	hostGeneration := k8s.GetResourceCache().Generation()
	SetRelationshipTopologySource(func(generation uint64) (*topology.Topology, *topology.RelationshipsIndex) {
		if generation != hostGeneration {
			return nil, nil
		}
		return host, index
	})
	t.Cleanup(func() { SetRelationshipTopologySource(nil) })

	got, gotIndex, _, _, ok := mcpTopologyForContext()
	if !ok || got != host || gotIndex != index {
		t.Fatalf("resource context built its own graph instead of reusing the host's: %+v", got)
	}

	// After a switch the host's graph describes the previous cluster until it
	// rebuilds; resource context builds from the new cache instead.
	useSwitchTestCluster(t, "new-cluster-api")
	if built, _, _, _, ok := mcpTopologyForContext(); !ok || built == host || !hasNode(built, "new-cluster-api") {
		t.Fatalf("resource context served the previous cluster's graph after a switch: %+v", built)
	}

	// Before the host has a graph, resource context still builds one: a
	// fresh memo, so the build above can't answer for it.
	summaryCtxTopoMemo = topology.NewMemoizer(5 * time.Second)
	SetRelationshipTopologySource(func(uint64) (*topology.Topology, *topology.RelationshipsIndex) { return nil, nil })
	if built, _, _, _, ok := mcpTopologyForContext(); !ok || !hasNode(built, "new-cluster-api") {
		t.Fatalf("fallback build missing: %+v", built)
	}
}
