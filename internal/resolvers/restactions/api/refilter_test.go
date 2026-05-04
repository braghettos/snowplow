// Q-RBAC-DECOUPLE C(d) v3 — RefilterRESTAction unit tests.
//
// The headline test is RefilterRESTAction_SameCachedBytes_TwoUsers_DifferentResults:
// it loads the same cached v3 wrapper bytes twice, refilters them under two
// users with disjoint RBAC stubs, and asserts the outputs differ. This is the
// unit-level analogue of the §6.4 envtest and the test that DIRECTLY catches
// Q-RBACC-DEFECT-1 — the v2 implementation could not produce two different
// outputs from the same cached entry because the filter ran inside the
// resolve pipeline (which doesn't execute on L1 hit).
//
// Other cases cover the trust-boundary contract (errors return (nil, err) so
// the caller falls through to the miss path) and the transitive child case.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// stubRBACEvaluator is the test-only RBACEvaluator. allow[user][ns] = true
// grants the user list access on the (group, resource) tuple in that
// namespace. The (verb, group, resource) values are ignored — the spec
// case isolates the per-namespace filter logic.
type stubRBACEvaluator struct {
	allow map[string]map[string]bool
}

func (s *stubRBACEvaluator) EvaluateRBAC(username string, _ []string, _ string, _ schema.GroupResource, ns string) bool {
	if s == nil {
		return false
	}
	if u, ok := s.allow[username]; ok {
		return u[ns]
	}
	return false
}

func mustMarshalCachedFor(t *testing.T, cr *templates.RESTAction, dict map[string]any) []byte {
	t.Helper()
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		t.Fatalf("MarshalCached: %v", err)
	}
	return raw
}

// ctxWith returns a context with logger + user identity + RBAC evaluator
// installed — the minimum surface RefilterRESTAction expects.
func ctxWith(t *testing.T, username string, ev cache.RBACEvaluator) context.Context {
	t.Helper()
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: []string{"devs"}}),
	)
	if ev != nil {
		ctx = cache.WithRBACEvaluator(ctx, ev)
	}
	return ctx
}

// makeNSCR returns a minimal RESTAction with a single api[] entry that
// mirrors the production "namespaces" shape — UserAccessFilter on a list
// response with NamespaceFrom="." and an outer JQ filter that hands the
// list through unchanged. The filter shape `{namespaces: <slice>}` makes
// it easy for tests to assert on the wire output.
func makeNSCR() *templates.RESTAction {
	return &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{{
				Name: "ns",
				Path: "/api/v1/namespaces",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:          "list",
					Resource:      "namespaces",
					NamespaceFrom: ptr.To("."),
				},
			}},
			Filter: ptr.To(`{namespaces: .ns}`),
		},
	}
}

// extractStatusNS unmarshals the wire bytes (json.Marshal of the CR), then
// reads cr.status.namespaces — the spec'd test surface. Any decode failure
// yields nil so callers can assert on length.
func extractStatusNS(raw []byte) []string {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	statusRaw, ok := top["status"]
	if !ok {
		return nil
	}
	statusBytes, _ := json.Marshal(statusRaw)
	var status map[string]any
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return nil
	}
	nsAny, ok := status["namespaces"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(nsAny))
	for _, v := range nsAny {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ── Headline: the Q-RBACC-DEFECT-1 anti-regression at the unit level ────

func TestRefilterRESTAction_SameCachedBytes_TwoUsers_DifferentResults(t *testing.T) {
	cr := makeNSCR()
	// Cached ProtectedDict: the unfiltered list of three namespaces. v2's
	// resolver would have already filtered this by user A's RBAC and
	// stored the filtered shape; v3 stores it raw.
	dict := map[string]any{
		"ns": []any{"ns-a", "ns-b", "ns-c"},
	}
	raw := mustMarshalCachedFor(t, cr, dict)

	// User A: only ns-a permitted.
	rbacA := &stubRBACEvaluator{allow: map[string]map[string]bool{
		"user-a": {"ns-a": true},
	}}
	outA, err := RefilterRESTAction(ctxWith(t, "user-a", rbacA), nil, raw)
	if err != nil {
		t.Fatalf("RefilterRESTAction(user-a): %v", err)
	}
	gotA := extractStatusNS(outA)
	if len(gotA) != 1 || gotA[0] != "ns-a" {
		t.Errorf("user-a: expected [ns-a], got %v", gotA)
	}

	// User B: only ns-b permitted. Same cached bytes.
	rbacB := &stubRBACEvaluator{allow: map[string]map[string]bool{
		"user-b": {"ns-b": true},
	}}
	outB, err := RefilterRESTAction(ctxWith(t, "user-b", rbacB), nil, raw)
	if err != nil {
		t.Fatalf("RefilterRESTAction(user-b): %v", err)
	}
	gotB := extractStatusNS(outB)
	if len(gotB) != 1 || gotB[0] != "ns-b" {
		t.Errorf("user-b: expected [ns-b], got %v", gotB)
	}

	// The bytes returned must differ — v2 would have produced identical
	// bytes here (the cached filtered shape served verbatim).
	if string(outA) == string(outB) {
		t.Errorf("user-a and user-b received byte-identical responses; refilter is not running per-user")
	}
}

// Burst test: 100 alternating user-a / user-b refilters must each see
// ONLY their own permitted item. Mirrors the §6.4 envtest's burst case
// at the unit level.
func TestRefilterRESTAction_BurstAlternating_NeverLeaksAcrossUsers(t *testing.T) {
	cr := makeNSCR()
	dict := map[string]any{"ns": []any{"ns-a", "ns-b", "ns-c"}}
	raw := mustMarshalCachedFor(t, cr, dict)

	rbacA := &stubRBACEvaluator{allow: map[string]map[string]bool{"user-a": {"ns-a": true}}}
	rbacB := &stubRBACEvaluator{allow: map[string]map[string]bool{"user-b": {"ns-b": true}}}

	for i := 0; i < 100; i++ {
		var (
			user string
			rbac cache.RBACEvaluator
			want string
		)
		if i%2 == 0 {
			user, rbac, want = "user-a", rbacA, "ns-a"
		} else {
			user, rbac, want = "user-b", rbacB, "ns-b"
		}
		out, err := RefilterRESTAction(ctxWith(t, user, rbac), nil, raw)
		if err != nil {
			t.Fatalf("iter %d (%s): refilter err: %v", i, user, err)
		}
		got := extractStatusNS(out)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("iter %d (%s): expected [%s], got %v", i, user, want, got)
		}
	}
}

// ── Trust-boundary contract: errors must return (nil, err) ────────────

func TestRefilterRESTAction_EmptyRaw_Error(t *testing.T) {
	out, err := RefilterRESTAction(ctxWith(t, "u", nil), nil, nil)
	if err == nil {
		t.Errorf("expected error on empty raw, got nil; out=%q", out)
	}
	if out != nil {
		t.Errorf("expected nil out on error, got %d bytes", len(out))
	}
}

func TestRefilterRESTAction_UnsupportedSchemaVersion_Error(t *testing.T) {
	// Hand-crafted v2-shape (no schema_version field): a CR JSON with no
	// wrapper. The unmarshal of CachedRESTAction either decodes a zero
	// SchemaVersion or fails — either way the function MUST return error
	// (so the dispatcher falls through to the miss path).
	cr := makeNSCR()
	v2Bytes, _ := json.Marshal(cr)
	out, err := RefilterRESTAction(ctxWith(t, "u", nil), nil, v2Bytes)
	if err == nil {
		t.Errorf("expected error on v2-shape input, got nil; out=%d bytes", len(out))
	}
	if out != nil {
		t.Errorf("expected nil out on schema mismatch, got %d bytes", len(out))
	}
	if !strings.Contains(err.Error(), "schema version") && !strings.Contains(err.Error(), "schema_version") && !strings.Contains(err.Error(), "nil CR") {
		// Either the wrapper decoded with an empty schema_version
		// (rejected with "schema version") or something else; we just
		// require it failed.
		t.Logf("note: rejection reason was %q (acceptable as long as err is non-nil)", err.Error())
	}
}

func TestRefilterRESTAction_NoUserInfo_Error(t *testing.T) {
	cr := makeNSCR()
	dict := map[string]any{"ns": []any{"ns-a"}}
	raw := mustMarshalCachedFor(t, cr, dict)

	// Context with NO user info installed.
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.Default()),
	)
	out, err := RefilterRESTAction(ctx, nil, raw)
	if err == nil {
		t.Errorf("expected error when UserInfo missing, got nil; out=%d bytes", len(out))
	}
	if out != nil {
		t.Errorf("expected nil out on missing UserInfo, got %d bytes", len(out))
	}
}

// ── Passthrough: api[] entries with no UserAccessFilter ─────────────────

func TestRefilterRESTAction_NoProtectedAPIEntries_Passthrough(t *testing.T) {
	cr := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{{
				Name: "ns",
				Path: "/api/v1/namespaces",
				// UserAccessFilter intentionally nil.
			}},
			Filter: ptr.To(`{namespaces: .ns}`),
		},
	}
	dict := map[string]any{"ns": []any{"x", "y", "z"}}
	raw := mustMarshalCachedFor(t, cr, dict)

	rbac := &stubRBACEvaluator{allow: map[string]map[string]bool{"u": {}}}
	out, err := RefilterRESTAction(ctxWith(t, "u", rbac), nil, raw)
	if err != nil {
		t.Fatalf("refilter err: %v", err)
	}
	got := extractStatusNS(out)
	if len(got) != 3 {
		t.Errorf("expected passthrough of all 3 items (no UserAccessFilter), got %v", got)
	}
}

// ── Audit log fires per request ────────────────────────────────────────
// Q-RBACC-V3-IMPL-5: refilter audit emission contract — same shape as the
// v2 miss-path audit log, fired from RefilterRESTAction so HTTP-time
// per-user denials are observable.

type captureSink struct {
	lines []string
}

func (s *captureSink) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (s *captureSink) WithAttrs(_ []slog.Attr) slog.Handler        { return s }
func (s *captureSink) WithGroup(_ string) slog.Handler             { return s }
func (s *captureSink) Handle(_ context.Context, r slog.Record) error {
	parts := []string{r.Message}
	r.Attrs(func(a slog.Attr) bool {
		parts = append(parts, a.Key+"="+a.Value.String())
		return true
	})
	s.lines = append(s.lines, strings.Join(parts, " "))
	return nil
}

func TestRefilterRESTAction_AuditLogFiresPerRequest(t *testing.T) {
	cr := makeNSCR()
	dict := map[string]any{"ns": []any{"ns-a", "ns-b", "ns-c"}}
	raw := mustMarshalCachedFor(t, cr, dict)

	rbac := &stubRBACEvaluator{allow: map[string]map[string]bool{"user-a": {"ns-a": true}}}
	sink := &captureSink{}
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.New(sink)),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "user-a", Groups: nil}),
	)
	ctx = cache.WithRBACEvaluator(ctx, rbac)

	if _, err := RefilterRESTAction(ctx, nil, raw); err != nil {
		t.Fatalf("refilter: %v", err)
	}
	hits := 0
	for _, line := range sink.lines {
		if strings.Contains(line, "audit=user_access_filter") && strings.Contains(line, "api_call=ns") {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("expected exactly one audit log line for api_call=ns, got %d (%v)", hits, sink.lines)
	}
}
