package readreason

import "testing"

// The codes are wire values clients branch on; a rename here must fail.
func TestCodeWireValues(t *testing.T) {
	for code, want := range map[Code]string{
		RBACDenied:             "rbac_denied",
		CollectorAccessDenied:  "collector_access_denied",
		CollectorUnauthorized:  "collector_unauthorized",
		PermissionCheckFailed:  "permission_check_failed",
		NoNamespaceAccess:      "no_namespace_access",
		KindSyncPending:        "kind_sync_pending",
		KindSyncFailed:         "kind_sync_failed",
		KindNotWatched:         "kind_not_watched",
		CollectorScopeLimited:  "collector_scope_limited",
		UnknownKind:            "unknown_kind",
		CollectionNotSupported: "collection_not_supported",
		NamespaceExcluded:      "namespace_excluded",
		NamespaceScopeLimited:  "namespace_scope_limited",
		OutsideNamespaceScope:  "outside_namespace_scope",
		NamespaceRequired:      "namespace_required",
		ReadError:              "read_error",
	} {
		if string(code) != want {
			t.Errorf("code %q must stay %q on the wire", code, want)
		}
	}
}
