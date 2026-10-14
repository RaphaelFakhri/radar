package server

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/skyhook-io/radar/internal/k8s"

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

// The composition reads the request's cache, the one its memo key names, so a
// switch after the request captured it can't compose the next cluster's
// issues under the previous cluster's key.
func TestGitopsComposeIssuesReadsTheRequestCache(t *testing.T) {
	crashing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "demo"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: 9,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	useTestResourceCache(t, fake.NewClientset(crashing))
	requestCache := k8s.GetResourceCache()
	resolver := newInsightsResolver(context.Background(), requestCache, []string{"demo"}, nil)

	useTestResourceCache(t, fake.NewClientset())
	flat, _ := resolver.composeIssues()
	found := false
	for _, issue := range flat {
		if issue.Name == "web-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("composition didn't come from the request's cache: %+v", flat)
	}
}
