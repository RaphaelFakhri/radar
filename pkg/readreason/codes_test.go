package readreason

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// The codes are wire values clients branch on; a rename here must fail.
func TestCodeWireValues(t *testing.T) {
	want := map[Code]string{
		CallerForbidden:                "caller_forbidden",
		CallerNoNamespaceAccess:        "caller_no_namespace_access",
		CallerPermissionCheckFailed:    "caller_permission_check_failed",
		CollectorForbidden:             "collector_forbidden",
		CollectorCredentialsRejected:   "collector_credentials_rejected",
		CollectorNamespaceNotCollected: "collector_namespace_not_collected",
		KindSyncPending:                "kind_sync_pending",
		KindSyncTimedOut:               "kind_sync_timed_out",
		KindWatchStartFailed:           "kind_watch_start_failed",
		CollectorKindNotWatched:        "collector_kind_not_watched",
		KindUnknown:                    "kind_unknown",
		KindNotWatchable:               "kind_not_watchable",
		OutsideNamespacePin:            "outside_namespace_pin",
		OutsideNamespaceSelection:      "outside_namespace_selection",
		CallerNamespaceNotVisible:      "caller_namespace_not_visible",
		NamespaceNotFound:              "namespace_not_found",
		NamespaceRequired:              "namespace_required",
		ObjectNotFound:                 "object_not_found",
		ListTooLarge:                   "list_too_large",
		FilterEvalFailed:               "filter_eval_failed",
		ClusterConnecting:              "cluster_connecting",
		ClusterDisconnected:            "cluster_disconnected",
		ConnectionChanged:              "connection_changed",
		ReadError:                      "read_error",
	}
	for code, spelling := range want {
		if string(code) != spelling {
			t.Errorf("code %q must stay %q on the wire", code, spelling)
		}
	}
	if len(Codes()) != len(want) {
		t.Fatalf("declared %d codes, wire table has %d", len(Codes()), len(want))
	}
}

func TestEveryCodeHasAPrecedence(t *testing.T) {
	for _, c := range Codes() {
		if !slices.Contains(precedence, c) {
			t.Errorf("%s has no precedence", c)
		}
	}
	for _, c := range precedence {
		if !Known(c) {
			t.Errorf("precedence lists undeclared %s", c)
		}
	}
}

// The reported code must not depend on the order gaps were found in.
func TestPrimaryIgnoresOrder(t *testing.T) {
	codes := Codes()
	want := Primary(codes)
	for i := range codes {
		rotated := append(slices.Clone(codes[i:]), codes[:i]...)
		slices.Reverse(rotated)
		if got := Primary(rotated); got != want {
			t.Fatalf("Primary(%v) = %s, want %s", rotated, got, want)
		}
	}
}

// A read that served nothing because part of it is still loading names the
// loading: a retry returns at least that part. Otherwise the caller's own
// access comes before Radar's limits.
func TestPrimaryPrefersWhatAResolvesFirst(t *testing.T) {
	if got := Primary([]Code{CollectorNamespaceNotCollected, CallerForbidden, KindSyncPending}); got != KindSyncPending {
		t.Fatalf("Primary = %s", got)
	}
	if got := Primary([]Code{CollectorNamespaceNotCollected, CallerForbidden}); got != CallerForbidden {
		t.Fatalf("Primary = %s", got)
	}
	if got := Primary([]Code{KindSyncTimedOut, CollectorForbidden}); got != CollectorForbidden {
		t.Fatalf("Primary = %s", got)
	}
	if got := Primary([]Code{KindSyncPending, KindUnknown}); got != KindUnknown {
		t.Fatalf("Primary = %s", got)
	}
	if got := Primary(nil); got != "" {
		t.Fatalf("Primary(nil) = %q", got)
	}
}

// actionsFixture is the table the web UI mirrors; its test reads the same
// file. READREASON_UPDATE=1 rewrites it after a deliberate change.
const actionsFixture = "testdata/actions.json"

func TestActionsFixtureMatchesTable(t *testing.T) {
	table := map[Code]Action{}
	for _, c := range Codes() {
		table[c], _ = ActionFor(c)
	}
	want, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("READREASON_UPDATE") == "1" {
		if err := os.WriteFile(actionsFixture, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(actionsFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; rerun with READREASON_UPDATE=1", actionsFixture)
	}
}
