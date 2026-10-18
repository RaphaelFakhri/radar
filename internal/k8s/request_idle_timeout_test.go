package k8s

import (
	"testing"
	"time"

	"github.com/skyhook-io/radar/pkg/k8score"
)

func TestResolveRequestIdleTimeout(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", k8score.DefaultRequestIdleTimeout},
		{"  ", k8score.DefaultRequestIdleTimeout},
		{"90s", 90 * time.Second},
		{"5m", 5 * time.Minute},
		{"0", 0},
		{"0s", 0},
		{"-1s", k8score.DefaultRequestIdleTimeout},
		{"soon", k8score.DefaultRequestIdleTimeout},
		{"60", k8score.DefaultRequestIdleTimeout},
	}
	for _, tc := range cases {
		if got := resolveRequestIdleTimeout(tc.raw); got != tc.want {
			t.Errorf("resolveRequestIdleTimeout(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
