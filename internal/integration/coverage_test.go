package integration

import (
	"encoding/json"
	"testing"
)

func TestKindCoverageJSON(t *testing.T) {
	b, err := json.Marshal(KindAccess{State: KindCoverageUncached, Uncached: []string{"b"}}.Coverage())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"state":"uncached","uncachedNamespaces":["b"]}` {
		t.Errorf("json = %s", got)
	}
	// uncachedNamespaces is always present: it is how a reader tells this
	// Radar from an older one, which listed uncached namespaces as denied.
	b, _ = json.Marshal(KindAccess{State: KindCoverageFull, All: true}.Coverage())
	if got := string(b); got != `{"state":"full","uncachedNamespaces":[]}` {
		t.Errorf("full coverage = %s", got)
	}
	b, _ = json.Marshal(KindAccess{State: KindCoveragePartial, Denied: []string{"a"}, Namespaces: map[string]bool{"b": true}}.Coverage())
	if got := string(b); got != `{"state":"partial","deniedNamespaces":["a"],"uncachedNamespaces":[],"allowedNamespaces":["b"]}` {
		t.Errorf("partial coverage = %s", got)
	}
}
