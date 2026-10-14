package server

import (
	"testing"
	"time"

	"github.com/skyhook-io/radar/internal/issues"
	"github.com/skyhook-io/radar/pkg/auth"
)

func TestGitopsIssuesMemo_ComposesOncePerKeyWithinTTL(t *testing.T) {
	m := newGitopsIssuesMemo(time.Minute)
	calls := 0
	compose := func() ([]issues.Issue, []issues.Issue) {
		calls++
		return []issues.Issue{{ID: "a"}}, nil
	}
	alice := gitopsIssuesMemoKey(1, &auth.User{Username: "alice", Groups: []string{"b", "a"}}, []string{"prod", "dev"})
	aliceAgain := gitopsIssuesMemoKey(1, &auth.User{Username: "alice", Groups: []string{"a", "b"}}, []string{"dev", "prod"})
	bob := gitopsIssuesMemoKey(1, &auth.User{Username: "bob"}, []string{"prod", "dev"})

	m.load(alice, compose)
	m.load(aliceAgain, compose)
	if calls != 1 {
		t.Fatalf("same user + namespaces (any order) must share one composition, got %d calls", calls)
	}
	m.load(bob, compose)
	if calls != 2 {
		t.Fatalf("a different user must not read another user's composition, got %d calls", calls)
	}
	m.clear()
	m.load(alice, compose)
	if calls != 3 {
		t.Fatalf("clear must drop entries, got %d calls", calls)
	}
	// The same user and namespaces on the next cluster's cache compose afresh:
	// a request landing before the post-switch clear can't read the old set.
	m.load(gitopsIssuesMemoKey(2, &auth.User{Username: "alice", Groups: []string{"a", "b"}}, []string{"dev", "prod"}), compose)
	if calls != 4 {
		t.Fatalf("another cache generation must not share a composition, got %d calls", calls)
	}
}

func TestGitopsIssuesMemo_DisabledComposesEveryTime(t *testing.T) {
	var m *gitopsIssuesMemo
	calls := 0
	compose := func() ([]issues.Issue, []issues.Issue) { calls++; return nil, nil }
	m.load("k", compose)
	m.load("k", compose)
	if calls != 2 {
		t.Fatalf("nil memo must compose inline, got %d calls", calls)
	}
}
