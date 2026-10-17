package mcp

import (
	"strings"
	"testing"

	"github.com/skyhook-io/radar/internal/search"
)

func TestSearchKindHint(t *testing.T) {
	if got := searchKindHint(nil); got != "" {
		t.Fatalf("no unsearched kinds must give no hint, got %q", got)
	}
	got := searchKindHint([]search.UnsearchedKind{
		{Kind: "role", Reason: search.UnsearchedNotIndexed},
		{Kind: "Certificate", Group: "cert-manager.io", Reason: search.UnsearchedUnavailable, Namespaces: []string{"b"}},
		{Kind: "IngressClass", Group: "networking.k8s.io", Reason: search.UnsearchedClusterScoped},
	})
	for _, want := range []string{
		"kind:role is not a kind search scans; use list_resources kind=role, or get_resource for one object.",
		"Certificate is not loaded in Radar's cache yet or could not be read in namespaces b; results for it may be incomplete, so use list_resources kind=Certificate group=cert-manager.io.",
		"IngressClass is cluster-scoped and this search was limited to namespaces; search without a namespace.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q does not contain %q", got, want)
		}
	}
}
