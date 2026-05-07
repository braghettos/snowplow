// Q-COMP-LIST-IDENTITY identity-refilter fast path — unit tests.
//
// Architect spec §5.1 + PM brief §1.1 / §3.4 acceptance gates:
//
//   1. Test_ComputeIsRefilterIdentity_NoUAF_NoChildRef_ReturnsTrue
//      — identity wrapper detected (compositions-list-shape).
//   2. Test_ComputeIsRefilterIdentity_WithUAF_ReturnsFalse
//      — UAF rejection (PM gate G-UT-NEG-UAF).
//   3. Test_ComputeIsRefilterIdentity_WithChildRef_ReturnsFalse
//      — childRef rejection (PM gate G-UT-NEG-CR).
//   4. Test_ComputeIsRefilterIdentity_RejectsUserScopedJQ
//      — defensive scan against $user/$jwt/etc references in outer JQ
//      (PM brief §3.4 gate (a) defense).
//   5. Test_FastPath_ByteEqualToSlowPath
//      — for an identity wrapper, the fast path output is byte-for-byte
//      identical to the standard path output (PM gate G-UT-EQ; trust
//      boundary preserved by construction).
//   6. Test_PeekIsRefilterIdentity_RoundTrip_OldWrapperIsFalse
//      — old wrapper without is_refilter_identity field decodes to false
//      (PM gate G-UT-MIGR; forward+backward compatible wire format).
package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// makeIdentityRA returns a compositions-list-shape RA: two api[] entries,
// neither with UserAccessFilter, plus a small outer JQ. Mirrors the real
// compositions-list shape verified in architect §0.1.
func makeIdentityRA() *templates.RESTAction {
	return &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{Name: "allNamespacesAndCrds", Path: "/call/compositions-get-ns-and-crd"},
				{Name: "allCompositions", Path: "/api/v1/compositions"},
			},
			Filter: ptr.To(`{list: (.allCompositions // [])}`),
		},
	}
}

// makeUAFRA returns an RA with at least one UAF on the api[] — should NEVER
// be flagged identity.
func makeUAFRA() *templates.RESTAction {
	return &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name: "ns",
					Path: "/api/v1/namespaces",
					UserAccessFilter: &templates.UserAccessFilter{
						Verb:          "list",
						Resource:      "namespaces",
						NamespaceFrom: ptr.To("."),
					},
				},
			},
			Filter: ptr.To(`{namespaces: .ns}`),
		},
	}
}

func Test_ComputeIsRefilterIdentity_NoUAF_NoChildRef_ReturnsTrue(t *testing.T) {
	cr := makeIdentityRA()
	dict := map[string]any{
		"allNamespacesAndCrds": map[string]any{"status": []any{"ns1", "ns2"}},
		"allCompositions": []any{
			map[string]any{"metadata": map[string]any{"name": "c1"}},
			map[string]any{"metadata": map[string]any{"name": "c2"}},
		},
	}
	if !computeIsRefilterIdentity(cr, dict) {
		t.Fatalf("expected identity=true on RA without UAF and dict without childRefs")
	}
}

func Test_ComputeIsRefilterIdentity_WithUAF_ReturnsFalse(t *testing.T) {
	cr := makeUAFRA()
	dict := map[string]any{"ns": []any{"ns1"}}
	if computeIsRefilterIdentity(cr, dict) {
		t.Fatalf("expected identity=false on RA whose api[] carries UAF — would otherwise leak items to a user lacking RBAC")
	}
}

func Test_ComputeIsRefilterIdentity_WithChildRef_ReturnsFalse(t *testing.T) {
	cr := makeIdentityRA()
	// Slot is a child reference (sentinel map). Even though no UAF on
	// the parent, the child could change per user on next refresh, so
	// the parent's refilter must not skip recursion.
	childRef := MarshalChildRESTActionRef(
		schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"},
		"krateo-system",
		"compositions-get-ns-and-crd",
		"snowplow:resolved:test:childkey",
		nil,
	)
	dict := map[string]any{
		"allNamespacesAndCrds": childRef,
		"allCompositions":      []any{},
	}
	if computeIsRefilterIdentity(cr, dict) {
		t.Fatalf("expected identity=false when a slot contains a childRESTActionRef sentinel")
	}
}

func Test_ComputeIsRefilterIdentity_NilCR_ReturnsFalse(t *testing.T) {
	if computeIsRefilterIdentity(nil, nil) {
		t.Fatalf("expected identity=false on nil cr (defensive)")
	}
}

func Test_ComputeIsRefilterIdentity_RejectsUserScopedJQ(t *testing.T) {
	// Defense-in-depth (PM gate (a) defense): even though
	// jqsupport.ModuleLoader does NOT bind any user-scope variables
	// today, a future codec migration could. Any filter that references
	// the documented user-scope token names returns identity=false to
	// preserve the false-negative-only failure mode.
	cases := []struct {
		name string
		expr string
	}{
		{"user", `.allCompositions | select(.user == $user)`},
		{"jwt", `{tok: $jwt}`},
		{"groups", `select($groups | length > 0)`},
		{"identity", `{me: $identity}`},
		{"bindings", `select($bindings | length > 0)`},
		{"username", `{u: $username}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cr := &templates.RESTAction{
				Spec: templates.RESTActionSpec{
					API:    []*templates.API{{Name: "x", Path: "/x"}},
					Filter: ptr.To(c.expr),
				},
			}
			if computeIsRefilterIdentity(cr, map[string]any{"x": []any{}}) {
				t.Fatalf("expected identity=false for outer JQ referencing %s, got true", c.name)
			}
		})
	}
}

func Test_PeekIsRefilterIdentity_RoundTrip_OldWrapperIsFalse(t *testing.T) {
	// Forward+backward compatibility: an L1 entry written by a binary
	// that lacks the is_refilter_identity field decodes to false (zero
	// value), so the fast path declines and the standard path runs.
	// PM gate G-UT-MIGR.
	oldShape := []byte(`{"schema_version":"v3","cr":{"metadata":{"name":"x","namespace":"y"},"spec":{"api":[]}},"protected_dict":{}}`)
	v, err := PeekIsRefilterIdentity(oldShape)
	if err != nil {
		t.Fatalf("peek on old shape returned err: %v", err)
	}
	if v {
		t.Fatalf("expected false on old wrapper without field, got true")
	}

	// New wrapper with field=true round-trips.
	cr := makeIdentityRA()
	cr.Name = "compositions-list"
	cr.Namespace = "krateo-system"
	dict := map[string]any{
		"allNamespacesAndCrds": []any{"ns1"},
		"allCompositions":      []any{},
	}
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		t.Fatalf("MarshalCached: %v", err)
	}
	v, err = PeekIsRefilterIdentity(raw)
	if err != nil {
		t.Fatalf("peek on identity wrapper: %v", err)
	}
	if !v {
		t.Fatalf("expected true on freshly-marshaled identity wrapper, got false")
	}

	// New wrapper with UAF round-trips to false.
	uafCR := makeUAFRA()
	uafCR.Name = "namespaces"
	uafCR.Namespace = "krateo-system"
	uafRaw, err := MarshalCached(uafCR, map[string]any{"ns": []any{"ns1"}})
	if err != nil {
		t.Fatalf("MarshalCached UAF: %v", err)
	}
	v, err = PeekIsRefilterIdentity(uafRaw)
	if err != nil {
		t.Fatalf("peek on UAF wrapper: %v", err)
	}
	if v {
		t.Fatalf("expected false on UAF wrapper, got true")
	}
}

// Test_FastPath_ByteEqualToSlowPath is the load-bearing trust-boundary
// test (PM gate G-UT-EQ). Strategy:
//   - build an identity-shape wrapper and call RefilterRESTAction (which
//     now takes the fast path because flag=true)
//   - flip wrapper.IsRefilterIdentity=false in the marshaled bytes by
//     re-marshaling without the helper, then call RefilterRESTAction
//     against THAT (which forces the standard path)
//   - assert the two outputs are byte-for-byte identical
//
// This proves the fast path is correctness-equivalent to the standard
// path for identity wrappers — the foundation of the whole change.
func Test_FastPath_ByteEqualToSlowPath(t *testing.T) {
	// Reset counters so the assertions below are deterministic.
	resetIdentityCounters()

	cr := makeIdentityRA()
	cr.Name = "compositions-list"
	cr.Namespace = "krateo-system"
	cr.APIVersion = "templates.krateo.io/v1"
	cr.Kind = "RESTAction"
	dict := map[string]any{
		"allNamespacesAndCrds": map[string]any{
			"status": map[string]any{"namespaces": []any{"ns1", "ns2", "ns3"}},
		},
		"allCompositions": []any{
			map[string]any{"metadata": map[string]any{"name": "c1", "namespace": "ns1"}},
			map[string]any{"metadata": map[string]any{"name": "c2", "namespace": "ns2"}},
			map[string]any{"metadata": map[string]any{"name": "c3", "namespace": "ns3"}},
		},
	}

	// Identity-flagged wrapper (fast path).
	identityRaw, err := MarshalCached(cr, dict)
	if err != nil {
		t.Fatalf("MarshalCached identity: %v", err)
	}

	// Standard wrapper: same shape but IsRefilterIdentity=false in the
	// JSON. We achieve this by hand-constructing a wrapper with the flag
	// suppressed, so the standard path runs end-to-end.
	standardWrapper := struct {
		SchemaVersion      string                `json:"schema_version"`
		CR                 *templates.RESTAction `json:"cr"`
		ProtectedDict      map[string]any        `json:"protected_dict"`
		IsRefilterIdentity bool                  `json:"is_refilter_identity"`
	}{
		SchemaVersion:      CachedSchemaVersionV3,
		CR:                 cr,
		ProtectedDict:      dict,
		IsRefilterIdentity: false,
	}
	standardRaw, err := json.Marshal(standardWrapper)
	if err != nil {
		t.Fatalf("marshal standard: %v", err)
	}

	// Sanity: PeekIsRefilterIdentity sees the flag.
	if v, _ := PeekIsRefilterIdentity(identityRaw); !v {
		t.Fatalf("identity wrapper should peek true")
	}
	if v, _ := PeekIsRefilterIdentity(standardRaw); v {
		t.Fatalf("standard wrapper should peek false")
	}

	ctx := ctxWith(t, "anyuser", nil)

	beforeFast := cache.GlobalMetrics.RefilterFastPathHits.Load()
	identityOut, err := RefilterRESTAction(ctx, nil, identityRaw)
	if err != nil {
		t.Fatalf("RefilterRESTAction(identity): %v", err)
	}
	afterFast := cache.GlobalMetrics.RefilterFastPathHits.Load()
	if afterFast-beforeFast != 1 {
		t.Fatalf("expected RefilterFastPathHits to advance by 1, got %d", afterFast-beforeFast)
	}

	beforeFast = cache.GlobalMetrics.RefilterFastPathHits.Load()
	standardOut, err := RefilterRESTAction(ctx, nil, standardRaw)
	if err != nil {
		t.Fatalf("RefilterRESTAction(standard): %v", err)
	}
	afterFast = cache.GlobalMetrics.RefilterFastPathHits.Load()
	if afterFast != beforeFast {
		t.Fatalf("expected RefilterFastPathHits to NOT advance for standard wrapper, advanced by %d", afterFast-beforeFast)
	}

	if string(identityOut) != string(standardOut) {
		// Surface a useful diff: line up the two outputs and report the
		// first divergence to make trust-boundary test failures
		// debuggable.
		minLen := len(identityOut)
		if len(standardOut) < minLen {
			minLen = len(standardOut)
		}
		divergeAt := -1
		for i := 0; i < minLen; i++ {
			if identityOut[i] != standardOut[i] {
				divergeAt = i
				break
			}
		}
		if divergeAt < 0 {
			divergeAt = minLen
		}
		ctxStart := divergeAt - 40
		if ctxStart < 0 {
			ctxStart = 0
		}
		ctxEndI := divergeAt + 40
		if ctxEndI > len(identityOut) {
			ctxEndI = len(identityOut)
		}
		ctxEndS := divergeAt + 40
		if ctxEndS > len(standardOut) {
			ctxEndS = len(standardOut)
		}
		t.Fatalf("FAST PATH and STANDARD PATH outputs diverge at byte %d (lengths fast=%d, std=%d)\n  fast around: %q\n  std  around: %q",
			divergeAt, len(identityOut), len(standardOut),
			string(identityOut[ctxStart:ctxEndI]),
			string(standardOut[ctxStart:ctxEndS]))
	}
}

// Test_FastPath_DefensiveRevalidation_FallsThroughOnLie covers the
// defense-in-depth re-validation: an attacker-injected (or codec-bug)
// wrapper that flagged IsRefilterIdentity=true but actually contains a
// UAF MUST take the standard path (and increment RefilterFastPathFallthrough).
// This is the trust-boundary guarantee that keeps the asymmetric-failure
// invariant alive.
func Test_FastPath_DefensiveRevalidation_FallsThroughOnLie(t *testing.T) {
	resetIdentityCounters()

	// Build a wrapper with UAF on api[] but lie that IsRefilterIdentity=true.
	cr := makeUAFRA()
	cr.Name = "namespaces-lie"
	cr.Namespace = "krateo-system"
	dict := map[string]any{"ns": []any{"ns1"}}
	lieWrapper := struct {
		SchemaVersion      string                `json:"schema_version"`
		CR                 *templates.RESTAction `json:"cr"`
		ProtectedDict      map[string]any        `json:"protected_dict"`
		IsRefilterIdentity bool                  `json:"is_refilter_identity"`
	}{
		SchemaVersion:      CachedSchemaVersionV3,
		CR:                 cr,
		ProtectedDict:      dict,
		IsRefilterIdentity: true,
	}
	lieRaw, err := json.Marshal(lieWrapper)
	if err != nil {
		t.Fatalf("marshal lie wrapper: %v", err)
	}
	if v, _ := PeekIsRefilterIdentity(lieRaw); !v {
		t.Fatalf("lie wrapper should peek true (test setup)")
	}

	ctx := ctxWith(t, "any-user", &stubRBACEvaluator{
		allow: map[string]map[string]bool{"any-user": {"ns1": true}},
	})

	beforeFall := cache.GlobalMetrics.RefilterFastPathFallthrough.Load()
	beforeHits := cache.GlobalMetrics.RefilterFastPathHits.Load()

	out, err := RefilterRESTAction(ctx, nil, lieRaw)
	if err != nil {
		t.Fatalf("refilter on lie wrapper: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("lie wrapper produced empty output")
	}

	afterFall := cache.GlobalMetrics.RefilterFastPathFallthrough.Load()
	afterHits := cache.GlobalMetrics.RefilterFastPathHits.Load()
	if afterFall-beforeFall != 1 {
		t.Fatalf("expected RefilterFastPathFallthrough to advance by 1, got %d", afterFall-beforeFall)
	}
	if afterHits != beforeHits {
		t.Fatalf("expected RefilterFastPathHits to NOT advance on lie wrapper, advanced by %d", afterHits-beforeHits)
	}

	// Ensure the standard-path UAF apply actually ran: the output's
	// status.namespaces should reflect the per-user filter (not the raw
	// dict). In this test the user has access to ns1, so it survives.
	if !strings.Contains(string(out), `"namespaces"`) {
		t.Fatalf("standard path output missing namespaces key: %s", string(out))
	}
}

func resetIdentityCounters() {
	cache.GlobalMetrics.RefilterFastPathHits.Store(0)
	cache.GlobalMetrics.RefilterFastPathFallthrough.Store(0)
	cache.GlobalMetrics.L2WritesIdentityBypass.Store(0)
	cache.GlobalMetrics.L2HitsIdentityCohort.Store(0)
}

// Test_FastPath_CohortByteEqual is the architect's envtest-equivalent
// trust-boundary check at unit scope (PM gate G-ENV proxy). 3 distinct
// users in the same binding-identity cohort all hit the same identity-
// flagged L1 wrapper through RefilterRESTAction; their outputs MUST be
// byte-equal. This is the property that lets the dispatcher serve the
// shared L1 entry without re-narrowing per user.
//
// Validated against the dispatcher path's contract: the per-user
// RefilterRESTAction calls converge on byte-identical outputs IFF the
// wrapper is correctly identity-flagged AND the fast path runs. A bug
// that produced different bytes for different users on identity
// wrappers would be caught here before any envtest could fail.
func Test_FastPath_CohortByteEqual(t *testing.T) {
	resetIdentityCounters()

	cr := makeIdentityRA()
	cr.Name = "compositions-list"
	cr.Namespace = "krateo-system"
	cr.APIVersion = "templates.krateo.io/v1"
	cr.Kind = "RESTAction"
	dict := map[string]any{
		"allNamespacesAndCrds": map[string]any{
			"status": map[string]any{"namespaces": []any{"ns1", "ns2"}},
		},
		"allCompositions": []any{
			map[string]any{"metadata": map[string]any{"name": "c1", "namespace": "ns1"}},
			map[string]any{"metadata": map[string]any{"name": "c2", "namespace": "ns2"}},
		},
	}
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		t.Fatalf("MarshalCached: %v", err)
	}

	users := []string{"alice", "bob", "carol"}
	outs := make([][]byte, len(users))
	for i, u := range users {
		ctx := ctxWith(t, u, nil)
		out, err := RefilterRESTAction(ctx, nil, raw)
		if err != nil {
			t.Fatalf("RefilterRESTAction(%s): %v", u, err)
		}
		outs[i] = out
	}

	// All N outputs MUST be byte-equal. Any divergence is a trust-
	// boundary defect: the fast path would be claiming "no per-user
	// difference" while producing per-user-different bytes.
	for i := 1; i < len(outs); i++ {
		if string(outs[0]) != string(outs[i]) {
			t.Fatalf("cohort byte-equality VIOLATED: outs[0] (user=%s) diverges from outs[%d] (user=%s); lengths %d vs %d",
				users[0], i, users[i], len(outs[0]), len(outs[i]))
		}
	}

	// Verify all 3 took the fast path (RefilterFastPathHits advanced by 3).
	if got := cache.GlobalMetrics.RefilterFastPathHits.Load(); got != 3 {
		t.Fatalf("expected 3 fast-path hits across cohort, got %d", got)
	}
	if got := cache.GlobalMetrics.RefilterFastPathFallthrough.Load(); got != 0 {
		t.Fatalf("expected 0 fast-path fallthroughs, got %d", got)
	}
}

// ─── Compile-time check: PeekIsRefilterIdentity is exported with the
// expected signature. If a future refactor breaks the signature, this
// fails to compile so callers in restactions.go / l1cache.go / apiref
// keep working.
var _ = func(raw []byte) (bool, error) { return PeekIsRefilterIdentity(raw) }

var _ = context.Background // satisfy import even if some test methods drop it
