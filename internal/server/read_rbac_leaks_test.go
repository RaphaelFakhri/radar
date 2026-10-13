package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skyhook-io/radar/internal/auth"
	"github.com/skyhook-io/radar/internal/k8s"
	pkgtraffic "github.com/skyhook-io/radar/pkg/traffic"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// flowVisibleForNamespaces is the per-flow filter that backs both the
// /api/traffic/flows and the stream endpoint for multi-namespace-restricted
// users (the source can only filter by a single namespace or none).
func TestFlowVisibleForNamespaces(t *testing.T) {
	flow := func(srcNS, dstNS string) pkgtraffic.Flow {
		return pkgtraffic.Flow{
			Source:      pkgtraffic.Endpoint{Namespace: srcNS},
			Destination: pkgtraffic.Endpoint{Namespace: dstNS},
		}
	}
	allowed := map[string]bool{"team-a": true}

	tests := []struct {
		name    string
		allowed map[string]bool
		flow    pkgtraffic.Flow
		want    bool
	}{
		{"nil set = all-namespace access", nil, flow("anything", "elsewhere"), true},
		{"source in allowed", allowed, flow("team-a", "other"), true},
		{"dest in allowed", allowed, flow("other", "team-a"), true},
		{"neither endpoint allowed", allowed, flow("other", "elsewhere"), false},
		{"external (empty) endpoints alone are not visible", allowed, flow("", ""), false},
		{"allowed source + external dest visible", allowed, flow("team-a", ""), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flowVisibleForNamespaces(tt.flow, tt.allowed); got != tt.want {
				t.Errorf("flowVisibleForNamespaces = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNamespaceLookup(t *testing.T) {
	if namespaceLookup(nil) != nil {
		t.Error("nil (all-namespace access) must map to nil, not an empty set")
	}
	set := namespaceLookup([]string{"a", "b"})
	if !set["a"] || !set["b"] || set["c"] {
		t.Errorf("namespaceLookup membership wrong: %+v", set)
	}
}

// secretReadableNamespaces gates Secret-derived data (TLS cert metadata) by
// per-user secrets RBAC, since the shared cache can hold secrets the user's
// own RBAC excludes. Mirrors the secrets gate in preflightResourceList.
func TestSecretReadableNamespaces(t *testing.T) {
	clusterWideReq := func(username string) *http.Request {
		r, _ := http.NewRequest("GET", "/api/certificates", nil)
		return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{Username: username}))
	}

	t.Run("cluster-wide namespaces, cluster-scope secrets denied -> none", func(t *testing.T) {
		env := newAuthTestServer(t)
		env.srv.permCache.Set("broad", nil, &auth.UserPermissions{AllowedNamespaces: nil})
		env.srv.permCache.Get("broad", nil).SetCanI("list", "", "secrets", "", false)

		got := env.srv.secretReadableNamespaces(clusterWideReq("broad"), nil)
		if len(got) != 0 || got == nil {
			t.Errorf("denied cluster-scope secrets must yield empty (none), got %#v", got)
		}
	})

	t.Run("cluster-wide namespaces, cluster-scope secrets allowed -> all (nil)", func(t *testing.T) {
		env := newAuthTestServer(t)
		env.srv.permCache.Set("admin", nil, &auth.UserPermissions{AllowedNamespaces: nil})
		env.srv.permCache.Get("admin", nil).SetCanI("list", "", "secrets", "", true)

		if got := env.srv.secretReadableNamespaces(clusterWideReq("admin"), nil); got != nil {
			t.Errorf("allowed cluster-scope secrets must yield nil (all), got %#v", got)
		}
	})

	t.Run("cluster-wide namespaces, secrets only via RoleBinding -> that namespace", func(t *testing.T) {
		env := newAuthTestServer(t)
		env.srv.permCache.Set("bob", nil, &auth.UserPermissions{AllowedNamespaces: nil})
		perms := env.srv.permCache.Get("bob", nil)
		perms.SetCanI("list", "", "secrets", "", false)
		perms.SetCanI("list", "", "secrets", "default", true)

		got := env.srv.secretReadableNamespaces(clusterWideReq("bob"), nil)
		if len(got) != 1 || got[0] != "default" {
			t.Errorf("expected [default], got %#v", got)
		}
	})

	t.Run("namespace-restricted -> per-namespace subset", func(t *testing.T) {
		env := newAuthTestServer(t)
		env.srv.permCache.Set("alice", nil, &auth.UserPermissions{AllowedNamespaces: []string{"default", "kube-system"}})
		perms := env.srv.permCache.Get("alice", nil)
		perms.SetCanI("list", "", "secrets", "default", true)
		perms.SetCanI("list", "", "secrets", "kube-system", false)

		got := env.srv.secretReadableNamespaces(clusterWideReq("alice"), []string{"default", "kube-system"})
		if len(got) != 1 || got[0] != "default" {
			t.Errorf("expected [default], got %#v", got)
		}
	})
}

// A user who sees every namespace (e.g. `view` through a ClusterRoleBinding)
// but may list Secrets only where a RoleBinding grants it must still see the
// TLS certificates in those namespaces, and only those. The Secrets view's
// expiry column and the dashboard's certificate health both read TLS Secrets.
func TestProxyAuth_CertificatesFollowPerNamespaceSecretsRBAC(t *testing.T) {
	ctx := context.Background()
	for _, ns := range []string{"default", "broken"} {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rbac-cert-" + ns, Namespace: ns},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": selfSignedCertPEM(t, ns+".example.test"),
				"tls.key": []byte("unused"),
			},
		}
		if _, err := testFakeClient.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create secret in %s: %v", ns, err)
		}
		t.Cleanup(func() {
			_ = testFakeClient.CoreV1().Secrets(sec.Namespace).Delete(context.Background(), sec.Name, metav1.DeleteOptions{})
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		lister := k8s.GetResourceCache().Secrets()
		_, errA := lister.Secrets("default").Get("rbac-cert-default")
		_, errB := lister.Secrets("broken").Get("rbac-cert-broken")
		if errA == nil && errB == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test secrets did not reach the cache")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// secretsGrants maps a namespace ("" = cluster-wide) to the user's
	// list-secrets verdict. Every user can see all namespaces.
	tests := []struct {
		name          string
		secretsGrants map[string]bool
		wantNS        []string
	}{
		{
			name:          "secrets only via RoleBinding in default",
			secretsGrants: map[string]bool{"": false, "default": true, "broken": false},
			wantNS:        []string{"default"},
		},
		{
			name:          "secrets cluster-wide",
			secretsGrants: map[string]bool{"": true},
			wantNS:        []string{"broken", "default"},
		},
		{
			name:          "no secrets anywhere",
			secretsGrants: map[string]bool{"": false, "default": false, "broken": false},
			wantNS:        nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newAuthTestServer(t)
			perms := &auth.UserPermissions{AllowedNamespaces: nil}
			for ns, ok := range tt.secretsGrants {
				perms.SetCanI("list", "", "secrets", ns, ok)
			}
			env.srv.permCache.Set("dana", nil, perms)

			resp := env.authGet(t, "/api/secrets/certificate-expiry", "dana", "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("certificate-expiry: expected 200, got %d", resp.StatusCode)
			}
			var expiry map[string]CertExpiry
			if err := json.NewDecoder(resp.Body).Decode(&expiry); err != nil {
				t.Fatalf("decode certificate-expiry: %v", err)
			}
			var gotNS []string
			for _, ns := range []string{"broken", "default"} {
				if _, ok := expiry[ns+"/rbac-cert-"+ns]; ok {
					gotNS = append(gotNS, ns)
				}
			}
			if strings.Join(gotNS, ",") != strings.Join(tt.wantNS, ",") {
				t.Errorf("certificate-expiry namespaces = %v, want %v", gotNS, tt.wantNS)
			}

			dash := env.authGet(t, "/api/dashboard", "dana", "")
			defer dash.Body.Close()
			if dash.StatusCode != http.StatusOK {
				t.Fatalf("/api/dashboard: expected 200, got %d", dash.StatusCode)
			}
			var body DashboardResponse
			if err := json.NewDecoder(dash.Body).Decode(&body); err != nil {
				t.Fatalf("decode dashboard: %v", err)
			}
			gotTotal := 0
			if body.CertificateHealth != nil {
				gotTotal = body.CertificateHealth.Total
			}
			if gotTotal != len(tt.wantNS) {
				t.Errorf("dashboard certificateHealth.total = %d, want %d", gotTotal, len(tt.wantNS))
			}
		})
	}
}

func selfSignedCertPEM(t *testing.T, dnsName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		DNSNames:     []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The Tier-1 namespace gates: a user whose namespace access excludes the
// target namespace must get 403, not data, on these read paths.
func TestProxyAuth_NamespaceGatedReadPaths(t *testing.T) {
	paths := []string{
		"/api/workloads/deployment/default/web/pods",
		"/api/metrics/pods/default/web-0",
		"/api/metrics/pods/default/web-0/history",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			env := newAuthTestServer(t)
			// carol can see "other", never "default" — getUserNamespaces
			// intersects requested(default) with allowed(other) -> empty.
			env.srv.permCache.Set("carol", nil, &auth.UserPermissions{AllowedNamespaces: []string{"other"}})

			resp := env.authGet(t, p, "carol", "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s: expected 403 for namespace-restricted user, got %d", p, resp.StatusCode)
			}
		})
	}
}

func TestProxyAuth_PodBackedReadsRequireExactAccess(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		parentAllowed bool
		podsAllowed   bool
		wantStatus    int
	}{
		{name: "pods parent denied", path: "/api/workloads/deployments/default/nginx/pods?limit=1", podsAllowed: true, wantStatus: http.StatusForbidden},
		{name: "pods list denied", path: "/api/workloads/deployments/default/nginx/pods?limit=1", parentAllowed: true, wantStatus: http.StatusForbidden},
		{name: "pods both allowed", path: "/api/workloads/deployments/default/nginx/pods?limit=1", parentAllowed: true, podsAllowed: true, wantStatus: http.StatusOK},
		{name: "logs parent denied", path: "/api/workloads/deployments/default/nginx/logs", podsAllowed: true, wantStatus: http.StatusForbidden},
		{name: "logs pods denied", path: "/api/workloads/deployments/default/nginx/logs", parentAllowed: true, wantStatus: http.StatusForbidden},
		{name: "logs subresource denied", path: "/api/workloads/deployments/default/nginx/logs", parentAllowed: true, podsAllowed: true, wantStatus: http.StatusForbidden},
		{name: "stream parent denied", path: "/api/workloads/deployments/default/nginx/logs/stream", podsAllowed: true, wantStatus: http.StatusForbidden},
		{name: "stream pods denied", path: "/api/workloads/deployments/default/nginx/logs/stream", parentAllowed: true, wantStatus: http.StatusForbidden},
		{name: "stream subresource denied", path: "/api/workloads/deployments/default/nginx/logs/stream", parentAllowed: true, podsAllowed: true, wantStatus: http.StatusForbidden},
		{name: "pod logs subresource denied", path: "/api/pods/default/nginx/logs", parentAllowed: true, podsAllowed: true, wantStatus: http.StatusForbidden},
		{name: "pod stream subresource denied", path: "/api/pods/default/nginx/logs/stream", parentAllowed: true, podsAllowed: true, wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newAuthTestServer(t)
			env.srv.permCache.Set("alice", nil, &auth.UserPermissions{AllowedNamespaces: []string{"default"}})
			permissions := env.srv.permCache.Get("alice", nil)
			permissions.SetCanI("get", "apps", "deployments", "default", tt.parentAllowed)
			permissions.SetCanI("list", "", "pods", "default", tt.podsAllowed)

			resp := env.authGet(t, tt.path, "alice", "")
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestProxyAuth_JobSetMembersRequireParentAndJobsAccess(t *testing.T) {
	for _, test := range []struct {
		name         string
		parent, jobs bool
		wantStatus   int
	}{{"parent denied", false, true, http.StatusForbidden}, {"member list denied", true, false, http.StatusForbidden}, {"both allowed", true, true, http.StatusInternalServerError}} {
		t.Run(test.name, func(t *testing.T) {
			env := newAuthTestServer(t)
			permissions := &auth.UserPermissions{AllowedNamespaces: []string{"default"}}
			permissions.SetCanI("get", "jobset.x-k8s.io", "jobsets", "default", test.parent)
			permissions.SetCanI("list", "batch", "jobs", "default", test.jobs)
			env.srv.permCache.Set("alice", nil, permissions)
			resp := env.authGet(t, "/api/workloads/jobsets/default/training/runs", "alice", "")
			defer resp.Body.Close()
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, test.wantStatus)
			}
			if test.parent && test.jobs {
				body, err := io.ReadAll(resp.Body)
				if err != nil || !strings.Contains(string(body), "failed to get jobset default/training:") {
					t.Fatalf("did not reach JobSet lookup: %s (%v)", body, err)
				}
			}
		})
	}
}

func TestProxyAuth_JobSetEvidenceRequiresEachReadPermission(t *testing.T) {
	targets := []struct{ verb, group, resource string }{{"get", "jobset.x-k8s.io", "jobsets"}, {"list", "batch", "jobs"}, {"list", "", "pods"}, {"get", "", "pods/log"}}
	for _, endpoint := range []string{"resources", "logs"} {
		for denied := range targets {
			if endpoint == "resources" && denied == 3 {
				continue
			}
			t.Run(endpoint+"/"+targets[denied].resource, func(t *testing.T) {
				env := newAuthTestServer(t)
				permissions := &auth.UserPermissions{AllowedNamespaces: []string{"default"}}
				for i, target := range targets {
					permissions.SetCanI(target.verb, target.group, target.resource, "default", i != denied)
				}
				env.srv.permCache.Set("alice", nil, permissions)
				resp := env.authGet(t, "/api/jobsets/default/training/"+endpoint, "alice", "")
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d", resp.StatusCode)
				}
			})
		}
	}
}
