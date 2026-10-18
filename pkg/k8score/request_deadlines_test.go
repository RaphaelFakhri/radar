package k8score

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestRequestDeadlineFor(t *testing.T) {
	cases := []struct {
		name   string
		method string
		url    string
		header http.Header
		want   time.Duration
		server string // path prefix of the configured server URL
	}{
		{"cluster list", "GET", "/api/v1/secrets?resourceVersion=5", nil, 2 * time.Minute, ""},
		{"namespaced list page", "GET", "/apis/apps/v1/namespaces/a/deployments?limit=500&continue=x", nil, 2 * time.Minute, ""},
		{"single get", "GET", "/api/v1/namespaces/a/secrets/db", nil, 2 * time.Minute, ""},
		{"namespace object", "GET", "/api/v1/namespaces/log", nil, 2 * time.Minute, ""},
		{"namespace status", "GET", "/api/v1/namespaces/a/status", nil, 2 * time.Minute, ""},
		{"object named log", "GET", "/api/v1/namespaces/a/configmaps/log", nil, 2 * time.Minute, ""},
		{"crd resource named proxy", "GET", "/apis/example.com/v1/namespaces/a/proxy", nil, 2 * time.Minute, ""},
		{"proxied server prefix", "GET", "/k8s/clusters/c-1/api/v1/namespaces/a/pods/p/log", nil, 0, "/k8s/clusters/c-1"},
		{"discovery", "GET", "/version", nil, 2 * time.Minute, ""},
		{"API-looking server prefix, log", "GET", "/api/v1/clusters/c-1/api/v1/namespaces/a/pods/p/log", nil, 0, "/api/v1/clusters/c-1"},
		{"API-looking server prefix, proxy tail", "GET", "/api/v1/clusters/c-1/api/v1/namespaces/a/services/prom:9090/proxy/api/v1/query", nil, 0, "/api/v1/clusters/c-1"},
		{"server prefix ending in proxy, list", "GET", "/api/v1/clusters/c-1/proxy/api/v1/secrets", nil, 2 * time.Minute, "/api/v1/clusters/c-1/proxy/"},
		{"prefix-less list under apis", "GET", "/apis/apps/v1beta2/deployments", nil, 2 * time.Minute, ""},
		{"api path inside a proxied tail", "GET", "/api/v1/namespaces/m/services/prom:9090/proxy/api/v1/query", nil, 0, ""},
		{"watch with timeout", "GET", "/api/v1/secrets?watch=true&timeoutSeconds=300", nil, 6 * time.Minute, ""},
		{"watch=1", "GET", "/api/v1/secrets?watch=1&timeoutSeconds=10", nil, 70 * time.Second, ""},
		{"watchlist", "GET", "/api/v1/secrets?watch=true&sendInitialEvents=true&timeoutSeconds=540", nil, 10 * time.Minute, ""},
		{"legacy watch path", "GET", "/api/v1/watch/namespaces/a/pods?timeoutSeconds=60", nil, 2 * time.Minute, ""},
		{"watch without timeout", "GET", "/api/v1/secrets?watch=true", nil, 0, ""},
		{"pod log", "GET", "/api/v1/namespaces/a/pods/p/log?container=c", nil, 0, ""},
		{"follow", "GET", "/api/v1/namespaces/a/pods/p/log?follow=true", nil, 0, ""},
		{"service proxy", "GET", "/api/v1/namespaces/m/services/prom:9090/proxy/api/v1/query", nil, 0, ""},
		{"node proxy", "GET", "/api/v1/nodes/n1/proxy/metrics", nil, 0, ""},
		{"exec upgrade", "GET", "/api/v1/namespaces/a/pods/p/exec?command=sh", http.Header{"Upgrade": {"websocket"}}, 0, ""},
		{"portforward", "GET", "/api/v1/namespaces/a/pods/p/portforward", nil, 0, ""},
		{"post", "POST", "/api/v1/namespaces/a/secrets", nil, 0, ""},
		{"patch", "PATCH", "/api/v1/namespaces/a/secrets/db", nil, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "https://apiserver"+tc.url, nil)
			for k, v := range tc.header {
				req.Header[k] = v
			}
			rt := WrapRequestDeadlines(tc.server)(nil).(*requestDeadlineRoundTripper)
			if got := rt.deadlineFor(req); got != tc.want {
				t.Fatalf("deadlineFor(%s %s) = %v, want %v", tc.method, tc.url, got, tc.want)
			}
		})
	}
}

func TestRequestDeadlines_EndStalledRequests(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/secrets": // stalls before sending headers
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case "/api/v1/configmaps": // sends headers, then stalls mid-body
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"kind":`)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case "/api/v1/namespaces/a/pods/p/log": // long-running: must not be cut
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, "done")
		}
	}))
	defer srv.Close()
	defer close(release)

	client := &http.Client{Transport: &requestDeadlineRoundTripper{
		next: http.DefaultTransport, deadline: 100 * time.Millisecond, watchGrace: time.Second,
	}}

	start := time.Now()
	_, err := client.Get(srv.URL + "/api/v1/secrets")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled LIST before headers: err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("deadline did not end the request promptly")
	}

	resp, err := client.Get(srv.URL + "/api/v1/configmaps")
	if err != nil {
		t.Fatalf("headers should arrive: %v", err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled body read: err = %v, want deadline exceeded", err)
	}

	resp, err = client.Get(srv.URL + "/api/v1/namespaces/a/pods/p/log")
	if err != nil {
		t.Fatalf("log request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || !strings.Contains(string(body), "done") {
		t.Fatalf("log stream was cut: body=%q err=%v", body, err)
	}

	// A caller's shorter deadline still wins.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/secrets", nil)
	start = time.Now()
	if _, err := client.Do(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline: err = %v", err)
	}
	if time.Since(start) > 90*time.Millisecond {
		t.Fatal("caller's shorter deadline was not honored")
	}
}

// A client built from a Host with an API-looking path prefix classifies its
// requests by what follows the prefix: the pod log stream is not cut, and a
// stalled LIST still is.
func TestRequestDeadlines_PrefixedServerURL(t *testing.T) {
	const prefix = "/api/v1/clusters/c-1/proxy"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, prefix) {
		case "/api/v1/namespaces/a/pods/p/log":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, "done")
		case "/api/v1/namespaces/a/secrets":
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(release)

	cfg := &rest.Config{Host: srv.URL + prefix}
	serverURL, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil {
		t.Fatalf("DefaultServerUrlFor: %v", err)
	}
	rt := WrapRequestDeadlines(serverURL.Path)(http.DefaultTransport).(*requestDeadlineRoundTripper)
	rt.deadline = 100 * time.Millisecond
	cfg.Transport = rt
	client := kubernetes.NewForConfigOrDie(cfg)

	ctx := context.Background()
	body, err := client.CoreV1().Pods("a").GetLogs("p", &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil || string(body) != "done" {
		t.Fatalf("log stream behind a prefixed server was cut: body=%q err=%v", body, err)
	}
	start := time.Now()
	if _, err := client.CoreV1().Secrets("a").List(ctx, metav1.ListOptions{}); err == nil {
		t.Fatal("stalled LIST behind a prefixed server should have hit the deadline")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("stalled LIST behind a prefixed server was not ended by the deadline")
	}
}
