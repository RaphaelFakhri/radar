package mcp

import (
	"context"
	"testing"

	pkgauth "github.com/skyhook-io/radar/pkg/auth"
	"github.com/skyhook-io/radar/pkg/ownership"
	"github.com/skyhook-io/radar/pkg/resourceid"
)

// A namespaced kind in any namespace: a cluster-wide grant, or a grant in one
// of the user's namespaces.
func TestOwnershipAccessDecidesAKindInAnyNamespace(t *testing.T) {
	t.Cleanup(func() { getPermCache().Invalidate() })
	secret := resourceid.GroupKind{Kind: "Secret"}
	for name, beta := range map[string]bool{"somewhere": true, "nowhere": false} {
		perms := &pkgauth.UserPermissions{AllowedNamespaces: []string{"alpha", "beta"}}
		perms.SetCanI("get", "", "secrets", "", false)
		perms.SetCanI("get", "", "secrets", "alpha", false)
		perms.SetCanI("get", "", "secrets", "beta", beta)
		getPermCache().Set(name, nil, perms)
		ctx := pkgauth.ContextWithUser(context.Background(), &pkgauth.User{Username: name})
		a := ownershipAccess(ctx, ownership.NewCatalog(nil, true))
		want := ownership.Denied
		if beta {
			want = ownership.Allowed
		}
		if got := a.Read(secret, ""); got != want {
			t.Errorf("%s: Read(Secret) in any namespace = %v, want %v", name, got, want)
		}
		if got := a.Read(secret, "alpha"); got != ownership.Denied {
			t.Errorf("%s: Read(Secret, alpha) = %v, want denied", name, got)
		}
	}
}

// A user who may read every namespace, when the cluster's namespaces can't be
// listed: there is no telling whether some namespace grants the kind.
func TestOwnershipAccessCannotTellWithoutTheClustersNamespaces(t *testing.T) {
	t.Cleanup(func() { getPermCache().Invalidate() })
	perms := &pkgauth.UserPermissions{}
	perms.SetCanI("get", "", "secrets", "", false)
	getPermCache().Set("gina", nil, perms)
	ctx := pkgauth.ContextWithUser(context.Background(), &pkgauth.User{Username: "gina"})
	a := ownershipAccess(ctx, ownership.NewCatalog(nil, true)).(*mcpAccess)
	a.clusterNamespaces = func(context.Context) []string { return nil }
	if got := a.Read(resourceid.GroupKind{Kind: "Secret"}, ""); got != ownership.Undecided {
		t.Errorf("Read(Secret) in any namespace without the namespace list = %v, want undecided", got)
	}
}
