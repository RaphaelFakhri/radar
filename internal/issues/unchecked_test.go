package issues

import (
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/issuesapi"
	"github.com/skyhook-io/radar/pkg/k8score"
)

func TestMissingRefUncheckedSources(t *testing.T) {
	client := fake.NewClientset()
	client.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("watch is not allowed"))
	})
	scopes := map[string]k8score.ResourceScope{
		k8score.Secrets:    {Enabled: true},
		k8score.ConfigMaps: {Enabled: true},
	}
	core, err := k8score.NewResourceCache(k8score.CacheConfig{
		Client:         client,
		ResourceTypes:  map[string]bool{k8score.Secrets: true, k8score.ConfigMaps: true},
		DeferredTypes:  map[string]bool{},
		ResourceScopes: scopes,
	})
	if err != nil {
		t.Fatalf("NewResourceCache: %v", err)
	}
	t.Cleanup(core.Stop)
	cache := &k8s.ResourceCache{ResourceCache: core}
	deadline := time.Now().Add(10 * time.Second)
	for !core.KindListOnly(k8score.Secrets, "") {
		if time.Now().After(deadline) {
			t.Fatal("Secrets informer never reported list-only")
		}
		time.Sleep(20 * time.Millisecond)
	}

	want := issuesapi.UncheckedSource{Source: issuesapi.UncheckedSourceMissingRef, Reason: issuesapi.UncheckedReasonListOnly, Resource: "secrets"}
	for _, namespaces := range [][]string{nil, {"shop"}} {
		got := MissingRefUncheckedSources(cache, namespaces)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("namespaces %v: got %+v, want only %+v", namespaces, got, want)
		}
	}
	if got := MissingRefUncheckedSources(cache, []string{}); got != nil {
		t.Fatalf("no namespace access: want nothing, got %+v", got)
	}
}
