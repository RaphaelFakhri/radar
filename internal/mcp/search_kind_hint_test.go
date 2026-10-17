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
		{Kind: "Certificate", Group: "cert-manager.io", Reason: search.UnsearchedUnreadable, Namespaces: []string{"b"}},
		{Kind: "IngressClass", Group: "networking.k8s.io", Reason: search.UnsearchedClusterScoped},
	})
	for _, want := range []string{
		"kind:role doesn't name a kind search scans; check the spelling (a Kind or its plural), or use list_resources or get_resource.",
		"Radar couldn't read Certificate from its cache for namespaces b; it may still be loading, or Radar may not be allowed to list it. An empty result for it doesn't mean none exist.",
		"IngressClass is cluster-scoped and this search was limited to namespaces; use list_resources kind=IngressClass.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q does not contain %q", got, want)
		}
	}
}
