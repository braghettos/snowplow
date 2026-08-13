// rbac_test.go — hermetic auth-gate falsifier for GET /rbac (design §7 #4).
//
// /rbac mounts on the SAME middleware.UserConfig as /call (design §5): the JWT
// authenticates the caller. This test proves the auth GATE — a request with no
// Authorization header, and one with a malformed header, are rejected 401
// BEFORE the handler runs. NO apiserver / informer is touched (the auth gate
// fires first).
//
// The POSITIVE half (a VALID JWT reaches the handler and the enumeration runs
// under the SA with NONE of the caller's RBAC perms) requires a real
// clientconfig Secret + apiserver, so it is the kind integration falsifier #5
// (inspect_integration_test.go) — which also proves the dispatch-free property
// the design's whole rationale rests on.

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
)

const rbacTestAuthnNS = "krateo-system"

// rbacServer wires the production chain (UserConfig -> RBAC) on a test server,
// exactly as main.go mounts GET /rbac. The key source is the package-level
// refreshTestKeys (refreshes_test.go): both cases below are rejected on the
// Authorization header itself, before any key is resolved, so the source is
// never consulted — it only has to satisfy UserConfig's non-nil check.
func rbacServer(t *testing.T) string {
	t.Helper()
	h := middleware.UserConfig(refreshTestKeys, rbacTestAuthnNS)(RBAC())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestRBAC_Auth_NoJWT_401 — a request with NO Authorization header is rejected
// 401 by the UserConfig gate before the handler runs (design §7 #4).
func TestRBAC_Auth_NoJWT_401(t *testing.T) {
	base := rbacServer(t)

	resp, err := http.Get(base + "/rbac?apiRefName=foo&apiRefNamespace=krateo-system")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("FALSIFIER #4 FAIL: missing Authorization header must yield 401, got %d", resp.StatusCode)
	}
}

// TestRBAC_Auth_MalformedJWT_401 — a malformed bearer token is rejected 401.
func TestRBAC_Auth_MalformedJWT_401(t *testing.T) {
	base := rbacServer(t)

	req, _ := http.NewRequest(http.MethodGet, base+"/rbac?apiRefName=foo&apiRefNamespace=krateo-system", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("FALSIFIER #4 FAIL: a malformed JWT must yield 401, got %d", resp.StatusCode)
	}
}

// TestRBAC_Auth_NonBearer_401 — a non-Bearer Authorization scheme is rejected.
func TestRBAC_Auth_NonBearer_401(t *testing.T) {
	base := rbacServer(t)

	req, _ := http.NewRequest(http.MethodGet, base+"/rbac?apiRefName=foo&apiRefNamespace=krateo-system", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("FALSIFIER #4 FAIL: a non-Bearer scheme must yield 401, got %d", resp.StatusCode)
	}
}
