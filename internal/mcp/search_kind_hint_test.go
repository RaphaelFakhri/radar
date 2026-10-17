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
		{Kind: "ResourceQuota", Reason: search.UnsearchedUnreadable},
		{Kind: "IngressClass", Group: "networking.k8s.io", Reason: search.UnsearchedClusterScoped},
	})
	for _, want := range []string{
		"kind:role is not a kind search scans; use list_resources kind=role, or get_resource for one object.",
		"Radar's cache of Certificate is not loaded yet for namespaces b, so an empty result for it doesn't mean none exist; retry shortly.",
		"Radar could not read ResourceQuota from its cache (Radar itself may not be allowed to list it), so an empty result for it doesn't mean none exist.",
		"IngressClass is cluster-scoped and this search was limited to namespaces; use list_resources kind=IngressClass.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q does not contain %q", got, want)
		}
	}
}
