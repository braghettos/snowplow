// Q-RBAC-DECOUPLE C(d) v4 §6.4 — anti-defect test for Q-RBACC-DEFECT-2.
//
// CRITICAL CONTRACT (per the v4 spec §6.4): this test MUST FAIL on the
// v3 implementation tagged 0.25.296 (proves it catches the defect) and
// PASS on the v4 implementation. The v3 path on cache MISS was:
//
//   first request (L1 miss) → l1cache.ResolveAndCache runs the resolver,
//                             marshals the v3 wrapper, calls
//                             api.RefilterRESTAction internally to populate
//                             Result.Status (apiref-safe shape) — but the
//                             refiltered BYTES were silently DISCARDED.
//   dispatcher returns       → wri.Write(result.Raw) → user receives the
//                             v3 wrapper with the UNFILTERED ProtectedDict
//                             (every namespace visible). Silent RBAC leak.
//
// In v4 (Fix-2b):
//
//   first request (L1 miss) → l1cache.ResolveAndCache populates BOTH
//                             Result.Raw (the wrapper, written to L1) AND
//                             Result.Refiltered (the per-user-refiltered CR).
//   dispatcher returns       → wri.Write(result.Refiltered) → user receives
//                             ONLY their RBAC-permitted view.
//
// This test exercises the REAL `restActionHandler.ServeHTTP` via
// httptest.NewServer — it does NOT use the simulator pattern that the
// v3 envtest at internal/resolvers/restactions/api/dispatcher_refilter
// _envtest_test.go uses. The test-design lesson (§6.10) is that the v3
// simulator BYPASSED the dispatcher MISS path entirely, which is why the
// v3 anti-defect test PASSED while the real MISS path leaked. v4 envtests
// use the real handler so dispatcher-level integration bugs cannot hide.
//
// PLACEMENT NOTE: spec §10 lists this test under
// internal/resolvers/restactions/api/dispatcher_miss_envtest_test.go,
// but that path creates an import cycle (dispatchers → api). The
// architect's INTENT is "real-handler test, not simulator" — placement
// in this package is the only valid way to honor that intent.
package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// restActionGVR is the canonical RESTAction GVR — the dispatcher reads
// it from the request query parameters; the test seeds the L2 cache
// (cache.GetKey) under the same GVR so objects.Get hits L2 and returns
// the unstructured CR without needing a real K8s apiserver.
var restActionGVRForTest = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// missTestStubRBAC is a minimal RBACEvaluator: allow[user][ns] == true
// grants list access on namespaces. Mirrors the stub used by the api
// package's refilter_test.go and dispatcher_refilter_envtest_test.go,
// duplicated here because Go's `internal/...` linkage rules forbid
// importing test-only types across packages.
type missTestStubRBAC struct {
	allow map[string]map[string]bool
}

func (s *missTestStubRBAC) EvaluateRBAC(username string, _ []string, _ string, _ schema.GroupResource, ns string) bool {
	if s == nil {
		return false
	}
	if u, ok := s.allow[username]; ok {
		return u[ns]
	}
	return false
}

// k8sNamespacesListBody returns a serialized K8s NamespaceList containing
// the given names. Shape matches what kube-apiserver returns for
// `GET /api/v1/namespaces`.
func k8sNamespacesListBody(names []string) string {
	items := make([]map[string]any, 0, len(names))
	for _, n := range names {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": n},
		})
	}
	body := map[string]any{
		"kind":       "NamespaceList",
		"apiVersion": "v1",
		"items":      items,
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// missTestEnv stands up a fake K8s upstream (httptest.NewServer that
// returns a NamespaceList), a snowplow ServeHTTP server wrapping
// restActionHandler with a per-request middleware that installs ctx
// values, and a NewMem cache pre-seeded with the unstructured RESTAction
// CR. Returns a tear-down func.
type missTestEnv struct {
	t          *testing.T
	c          *cache.MemCache
	upstream   *httptest.Server
	server     *httptest.Server
	upstreamCt *atomic.Int32
	identityH  string
	cr         *templates.RESTAction
	authnNS    string
	stopFns    []func()
}

func (e *missTestEnv) close() {
	for _, fn := range e.stopFns {
		fn()
	}
}

func newMissTestEnv(t *testing.T, namespacesInUpstream []string) *missTestEnv {
	t.Helper()

	// env.SetTestMode(true) preserves the endpoint ServerURL chosen by
	// our snowplow callback — production code rewrites unknown URLs to
	// https://kubernetes.default.svc which would break the httptest hop.
	env.SetTestMode(true)
	t.Cleanup(func() { env.SetTestMode(false) })

	upstreamCt := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCt.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(k8sNamespacesListBody(namespacesInUpstream)))
	}))

	// Build the RESTAction CR: a single api[] entry with UAF on
	// namespaces. The per-api filter extracts a flat array of names
	// from the K8s NamespaceList response — applyUserAccessFilter
	// requires an array shape to gate items per-user (it skips with a
	// "response not an array" warning otherwise). The outer filter
	// surfaces the array under `status.namespaces` for assertion
	// clarity. Mirrors the production `compositions-get-ns-and-crd`
	// shape (the one that leaked in production at 0.25.296) and the
	// pattern the v3 envtest at user_access_filter_envtest_test.go
	// uses.
	cr := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{{
				Name: "ns",
				Path: "/api/v1/namespaces",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:          "list",
					Resource:      "namespaces",
					NamespaceFrom: ptr.To("."),
				},
				// Per-api filter: produce an array of NS names.
				// applyUserAccessFilter then iterates this array and
				// drops items the requesting user can't list.
				Filter: ptr.To(`[.ns.items[].metadata.name]`),
			}},
			// Outer filter: lift dict["ns"] (now an array of strings)
			// under `status.namespaces` for the wire shape.
			Filter: ptr.To(`{namespaces: .ns}`),
		},
	}
	cr.Name = "compositions-get-ns-and-crd"
	cr.Namespace = "krateo-system"
	cr.APIVersion = "templates.krateo.io/v1"
	cr.Kind = "RESTAction"

	// Seed L2 cache with the unstructured CR so `objects.Get(ctx, ref)`
	// returns it without a real K8s apiserver.
	c := cache.NewMem(0)
	crUnstructured := mustToUnstructured(t, cr)
	cacheKey := cache.GetKey(restActionGVRForTest, cr.Namespace, cr.Name)
	if err := c.Set(context.Background(), cacheKey, crUnstructured); err != nil {
		t.Fatalf("seed L2 cache: %v", err)
	}

	// Forced binding-identity collision — both users' requests build the
	// same L1 key. Production reproduces this when two users in the same
	// group-set share a binding-identity hash.
	const identityH = "shared-binding-id-H"

	// snowplowEndpointFn points at our upstream httptest server with a
	// dummy SA token. The dispatcher routes UAF entries through this
	// callback (independent of the requesting user's RBAC).
	snowplowEndpointFn := func() (*endpoints.Endpoint, error) {
		return &endpoints.Endpoint{
			ServerURL: upstream.URL,
			Token:     "SNOWPLOW-SA-TEST",
		}, nil
	}

	authnNS := "krateo-system"
	handler := RESTAction(snowplowEndpointFn, nil)

	// Per-request middleware: install ctx values that production
	// middleware would. Reads (user, rbac) from request headers so each
	// test request can carry its own identity.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := r.Header.Get("X-Test-User")
		var rbac cache.RBACEvaluator
		switch r.Header.Get("X-Test-RBAC") {
		case "user-a":
			rbac = &missTestStubRBAC{allow: map[string]map[string]bool{
				"user-a": {"default": true, "shared-ns": true},
			}}
		case "user-b":
			rbac = &missTestStubRBAC{allow: map[string]map[string]bool{
				"user-b": {"kube-system": true, "shared-ns": true},
			}}
		}

		ctx := xcontext.BuildContext(r.Context(),
			xcontext.WithLogger(slog.Default()),
			xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: []string{"devs"}}),
			// Empty UserConfig satisfies objects.Get's UserConfig
			// requirement; the L2 cache hit short-circuits before the
			// actual K8s call needs the endpoint.
			xcontext.WithUserConfig(endpoints.Endpoint{ServerURL: upstream.URL}),
		)
		ctx = cache.WithCache(ctx, c)
		ctx = cache.WithRBACEvaluator(ctx, rbac)
		ctx = cache.WithBindingIdentity(ctx, identityH)
		// Provide the snowplow endpoint via context fallback as well, so
		// the dispatch fork in resolve.go can resolve UAF entries
		// regardless of whether opts.SnowplowEndpoint was set.
		ctx = cache.WithSnowplowEndpoint(ctx, func() (any, error) {
			return snowplowEndpointFn()
		})
		// Inject a stub rest.Config via the v4 test-only ctx fallback
		// so api.Resolve's `if opts.RC == nil` branch picks it up
		// instead of calling rest.InClusterConfig() (which fails outside
		// an in-cluster pod). The Host points at the upstream
		// httptest.Server so any non-UAF entry that fell back to the
		// per-user clientconfig path would also reach our mock — but
		// our test scenario uses UAF on every entry, so this Config is
		// only used to satisfy the early nil-check.
		ctx = cache.WithTestRestConfig(ctx, &rest.Config{Host: upstream.URL})

		handler.ServeHTTP(w, r.WithContext(ctx))
	})

	server := httptest.NewServer(wrapped)

	return &missTestEnv{
		t:          t,
		c:          c,
		upstream:   upstream,
		server:     server,
		upstreamCt: upstreamCt,
		identityH:  identityH,
		cr:         cr,
		authnNS:    authnNS,
		stopFns: []func(){
			upstream.Close,
			server.Close,
		},
	}
}

// mustToUnstructured converts the typed RESTAction to an
// unstructured.Unstructured suitable for L2 cache seeding.
func mustToUnstructured(t *testing.T, cr *templates.RESTAction) *unstructured.Unstructured {
	t.Helper()
	b, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("marshal CR: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal CR: %v", err)
	}
	return &unstructured.Unstructured{Object: m}
}

// missTestRequest issues a GET to the snowplow test server for the seeded
// RESTAction CR with the given user identity headers. Returns
// (statusCode, body).
func missTestRequest(t *testing.T, e *missTestEnv, user string, rbac string) (int, []byte) {
	t.Helper()
	q := fmt.Sprintf(
		"%s/call?apiVersion=%s/%s&resource=%s&namespace=%s&name=%s",
		e.server.URL,
		restActionGVRForTest.Group, restActionGVRForTest.Version,
		restActionGVRForTest.Resource,
		e.cr.Namespace, e.cr.Name,
	)
	req, err := http.NewRequest(http.MethodGet, q, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Test-User", user)
	req.Header.Set("X-Test-RBAC", rbac)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// extractMissNS unmarshals the per-user response body and returns
// status.namespaces as a string slice. Returns nil on any decode failure
// so callers can assert "no leaked items" cleanly.
func extractMissNS(body []byte) []string {
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}
	st, ok := top["status"].(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := st["namespaces"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestDispatcherMissPath_PerUserFilteredBytes_NoLeak is the §6.4 anti-
// defect contract for Q-RBACC-DEFECT-2.
//
// Invariant: on cache MISS, the dispatcher MUST write per-user-refiltered
// bytes (NOT the v3 wrapper) to the HTTP response. The wrapper must
// remain in L1 for the next refilter-on-HIT to consume.
//
// On v3 (0.25.296) this test FAILS at the "wrapper marker" assertion:
// the response body contains `"schema_version":"v3"` and `"protected_dict"`
// because the dispatcher writes `result.Raw` (the wrapper) instead of
// `result.Refiltered`.
//
// On v4 (Fix-2b) it PASSES: the response body parses as the per-user
// filtered RESTAction CR and contains ONLY the requesting user's
// RBAC-permitted namespaces.
func TestDispatcherMissPath_PerUserFilteredBytes_NoLeak(t *testing.T) {
	upstreamNS := []string{"default", "kube-system", "shared-ns", "tenant-a", "tenant-b"}
	e := newMissTestEnv(t, upstreamNS)
	defer e.close()

	// First MISS: user-a (RBAC: default + shared-ns).
	codeA, bodyA := missTestRequest(t, e, "user-a", "user-a")
	if codeA != http.StatusOK {
		t.Fatalf("user-a MISS: status %d, body=%s, upstream_calls=%d", codeA, string(bodyA), e.upstreamCt.Load())
	}
	// Step 1: response body must be the per-user filtered shape, NOT the
	// v3 wrapper. The wrapper carries `"schema_version":"v3"` and
	// `"protected_dict"`. Their presence in the response is the
	// DEFECT-2 signature.
	if bytes.Contains(bodyA, []byte(`"schema_version"`)) {
		t.Errorf("Q-RBACC-DEFECT-2: response body contains v3 wrapper marker `\"schema_version\"`; dispatcher MISS path is leaking the unfiltered wrapper to HTTP\nbody=%s", truncate(bodyA, 600))
	}
	if bytes.Contains(bodyA, []byte(`"protected_dict"`)) {
		t.Errorf("Q-RBACC-DEFECT-2: response body contains `\"protected_dict\"`; the unfiltered ProtectedDict was served verbatim\nbody=%s", truncate(bodyA, 600))
	}
	// Step 2: status.namespaces must be exactly user-a's permitted set.
	gotA := extractMissNS(bodyA)
	if !equalUnordered(gotA, []string{"default", "shared-ns"}) {
		t.Errorf("user-a MISS: expected namespaces=[default shared-ns], got %v\nbody=%s", gotA, truncate(bodyA, 600))
	}

	// Step 3: L1 should now hold the v3 wrapper for the shared
	// binding-identity key. Subsequent reads must HIT and refilter
	// per-user (Path A — already covered by the v3 envtest, asserted
	// here as a regression guard for Path A under the v4 fix).
	l1Key := cache.ResolvedKey(e.identityH, restActionGVRForTest,
		e.cr.Namespace, e.cr.Name, -1, -1)
	rawCached, hit, err := e.c.GetRaw(context.Background(), l1Key)
	if err != nil {
		t.Fatalf("L1 GetRaw: %v", err)
	}
	if !hit {
		t.Fatalf("L1 should hold the v3 wrapper after the MISS, got hit=false (key=%s)", l1Key)
	}
	if !bytes.Contains(rawCached, []byte(`"schema_version":"v3"`)) {
		t.Errorf("L1 entry should be the v3 wrapper (with schema_version=v3) but the cached bytes do not contain that marker\ncached=%s", truncate(rawCached, 600))
	}

	// Second request: user-b in the same binding-identity group. This
	// time L1 HITs (Path A) and must refilter per user-b. Both v3 and
	// v4 should pass this; we keep it as a sanity check.
	codeB, bodyB := missTestRequest(t, e, "user-b", "user-b")
	if codeB != http.StatusOK {
		t.Fatalf("user-b HIT: status %d, body=%s", codeB, string(bodyB))
	}
	if bytes.Contains(bodyB, []byte(`"schema_version"`)) {
		t.Errorf("user-b HIT: response body unexpectedly contains v3 wrapper marker\nbody=%s", truncate(bodyB, 600))
	}
	gotB := extractMissNS(bodyB)
	if !equalUnordered(gotB, []string{"kube-system", "shared-ns"}) {
		t.Errorf("user-b HIT: expected namespaces=[kube-system shared-ns], got %v\nbody=%s", gotB, truncate(bodyB, 600))
	}

	// Cross-leak: bytes returned to A and B MUST differ.
	if bytes.Equal(bodyA, bodyB) {
		t.Errorf("user-a and user-b received byte-identical responses; per-user filtering is not running on the dispatcher MISS+HIT pair")
	}
}

// TestDispatcherMissPath_TwoUsersDisjointRBAC_AlwaysIsolated bombards
// the same L1 key from two users in alternating MISS/HIT pattern (with
// a forced cache flush between iterations to keep MISSes coming) and
// asserts strict per-user isolation throughout.
//
// On v3, an alternating burst would show one of the two users receiving
// the OTHER user's view inside the wrapper on every MISS that fires
// after a cache flush.
func TestDispatcherMissPath_TwoUsersDisjointRBAC_AlwaysIsolated(t *testing.T) {
	upstreamNS := []string{"default", "kube-system", "shared-ns"}
	e := newMissTestEnv(t, upstreamNS)
	defer e.close()

	l1Key := cache.ResolvedKey(e.identityH, restActionGVRForTest,
		e.cr.Namespace, e.cr.Name, -1, -1)

	// Flush the L1 entry between iterations to keep the dispatcher on
	// the MISS path. Each MISS exercises the Fix-2b code path.
	for i := 0; i < 20; i++ {
		// Erase any existing L1 entry.
		_ = e.c.Delete(context.Background(), l1Key)

		var (
			user, rbac string
			want       []string
		)
		if i%2 == 0 {
			user, rbac, want = "user-a", "user-a", []string{"default", "shared-ns"}
		} else {
			user, rbac, want = "user-b", "user-b", []string{"kube-system", "shared-ns"}
		}
		code, body := missTestRequest(t, e, user, rbac)
		if code != http.StatusOK {
			t.Fatalf("iter %d (%s): status %d body=%s", i, user, code, string(body))
		}
		if bytes.Contains(body, []byte(`"schema_version"`)) {
			t.Fatalf("iter %d (%s): wrapper leaked to HTTP body\nbody=%s", i, user, truncate(body, 600))
		}
		got := extractMissNS(body)
		if !equalUnordered(got, want) {
			t.Fatalf("iter %d (%s): expected %v, got %v\nbody=%s", i, user, want, got, truncate(body, 600))
		}
	}
}

// TestDispatcherMissPath_AfterMiss_L1HoldsV3Wrapper asserts that on
// MISS, the dispatcher writes the v3 wrapper to L1 (so subsequent HITs
// on Path A find a cache-shape-correct entry to refilter). The Fix-2b
// change MUST NOT alter the L1 wire shape.
func TestDispatcherMissPath_AfterMiss_L1HoldsV3Wrapper(t *testing.T) {
	upstreamNS := []string{"default", "kube-system"}
	e := newMissTestEnv(t, upstreamNS)
	defer e.close()

	code, _ := missTestRequest(t, e, "user-a", "user-a")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	l1Key := cache.ResolvedKey(e.identityH, restActionGVRForTest,
		e.cr.Namespace, e.cr.Name, -1, -1)
	cached, hit, err := e.c.GetRaw(context.Background(), l1Key)
	if err != nil || !hit {
		t.Fatalf("expected L1 hit after MISS, got hit=%v err=%v key=%s", hit, err, l1Key)
	}
	if !bytes.Contains(cached, []byte(`"schema_version":"v3"`)) {
		t.Errorf("L1 cached entry must hold v3 wrapper; got\n%s", truncate(cached, 600))
	}
	if !bytes.Contains(cached, []byte(`"protected_dict"`)) {
		t.Errorf("L1 cached entry must hold protected_dict (unfiltered); got\n%s", truncate(cached, 600))
	}
}

// equalUnordered is a small set-equality helper duplicated from the
// api package's test (Go's package-test linkage prevents importing it).
func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
		if m[s] < 0 {
			return false
		}
	}
	return true
}

// truncate returns at most n bytes of s, appending an ellipsis on truncation.
func truncate(s []byte, n int) string {
	if len(s) <= n {
		return string(s)
	}
	return string(s[:n]) + "...[truncated]"
}

