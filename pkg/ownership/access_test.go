package ownership_test

import (
	"sync/atomic"
	"testing"

	"github.com/skyhook-io/radar/pkg/ownership"
)

func TestAnyNamespace(t *testing.T) {
	namespaces := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	decide := func(byNamespace map[string]ownership.Decision) func(string) ownership.Decision {
		return func(ns string) ownership.Decision { return byNamespace[ns] }
	}
	for _, tc := range []struct {
		name       string
		namespaces []string
		decisions  map[string]ownership.Decision
		want       ownership.Decision
	}{
		{"none", nil, nil, ownership.Denied},
		{"all denied", namespaces, nil, ownership.Denied},
		{"one allowed, past the first batch", namespaces, map[string]ownership.Decision{"j": ownership.Allowed}, ownership.Allowed},
		{"one undecided", namespaces, map[string]ownership.Decision{"c": ownership.Undecided}, ownership.Undecided},
		{"allowed beats undecided", namespaces, map[string]ownership.Decision{"c": ownership.Undecided, "h": ownership.Allowed}, ownership.Allowed},
	} {
		if got := ownership.AnyNamespace(tc.namespaces, decide(tc.decisions)); got != tc.want {
			t.Errorf("%s: AnyNamespace = %v, want %v", tc.name, got, tc.want)
		}
	}
	var checked atomic.Int32
	ownership.AnyNamespace(namespaces, func(ns string) ownership.Decision {
		checked.Add(1)
		if ns == "a" {
			return ownership.Allowed
		}
		return ownership.Denied
	})
	if n := checked.Load(); n == int32(len(namespaces)) {
		t.Errorf("checked all %d namespaces after the first batch allowed", n)
	}
}
