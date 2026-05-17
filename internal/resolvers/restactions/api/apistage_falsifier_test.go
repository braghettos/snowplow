// apistage_falsifier_test.go — Ship E (0.30.116) pre-flight falsifier
// for the per-api-stage L1 key-swap.
//
// Team rule feedback_falsifier_first_before_ship: this test is written
// to FAIL against the pre-Ship-E resolver (no api-stage L1 — every
// resolve of a stage dispatches the K8s call) and PASS after the
// key-swap lands.
//
//   FE — flag-on, a SECOND resolve of a RESTAction re-running the same
//        stage must be served from the api-stage L1: zero K8s calls for
//        that stage on the second resolve, output byte-identical to the
//        cold (first) resolve, and the resolved-output store reports an
//        apistage hit + store.
//
//   FE-flagoff — flag-OFF, the second resolve dispatches the K8s call
//        AGAIN (no api-stage L1) — the AC-E1 behaviour-neutral control.
//
// Hermetic: the stage dispatches against a local httptest.Server; no
// cluster, no CRD. The stage key is RESTAction-scoped (the architect
// spec folds {group,version,resource,namespace,name} into it), so the
// reuse demonstrated here is two resolves of the SAME RESTAction — the
// realistic "every page reload re-resolves the same RESTActions" case.

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

// apistageTestStage builds a single-stage RESTAction api[] entry that
// LISTs an apiserver-shaped path. The path is a real apiserver shape so
// the dep-recording site can parse it (O4); the stage has no dependsOn
// and no EndpointRef (the endpoint is supplied via the context-carried
// internal endpoint — see apistageResolveOnce).
func apistageTestStage() *templates.API {
	return &templates.API{
		Name:   "compositions",
		Path:   "/apis/composition.krateo.io/v1/namespaces/bench-ns-01/widgets",
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To(".items"),
	}
}

// apistageResolveOnce drives api.Resolve for a one-stage RESTAction
// against the given httptest server. Identity is fixed (one user) so the
// per-user stage key is stable across the two calls in the falsifier.
func apistageResolveOnce(t *testing.T, srvURL string) map[string]any {
	t.Helper()
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "cyberjoker", Groups: []string{"devs"}}),
	)
	// EndpointRef is nil on the stage; resolveOne consults the
	// context-carried internal endpoint first — point it at the test
	// server so the stage dispatches there.
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: srvURL})

	return Resolve(ctx, ResolveOptions{
		// A non-nil RC keeps api.Resolve off its rest.InClusterConfig()
		// early-return (which fails outside a cluster). With a nil
		// EndpointRef + the context-carried internal endpoint, the
		// endpoint mapper never dereferences RC — it returns the
		// internal endpoint first — so a bare *rest.Config is enough.
		RC:                  &rest.Config{},
		Items:               []*templates.API{apistageTestStage()},
		RESTActionNamespace: "bench-ns-01",
		RESTActionName:      "shared-compositions-restaction",
	})
}

// newApistageCountingServer starts an httptest server that answers the
// stage LIST with a fixed envelope and counts how many requests it
// received. The count is the "K8s call" meter the falsifier asserts on.
func newApistageCountingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"composition.krateo.io/v1","kind":"WidgetList",`+
			`"metadata":{"resourceVersion":"1"},"items":[`+
			`{"apiVersion":"composition.krateo.io/v1","kind":"Widget","metadata":{"name":"w-1"}},`+
			`{"apiVersion":"composition.krateo.io/v1","kind":"Widget","metadata":{"name":"w-2"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestFalsifierFE_ApistageL1HitSkipsKubeCall is the Ship E pre-flight
// falsifier. Flag ON: the first resolve dispatches the stage's K8s LIST
// (1 server hit) and Puts the api-stage L1 entry; the SECOND resolve of
// the same RESTAction is an api-stage L1 hit — ZERO additional server
// hits — and produces byte-identical output.
//
// FAILS pre-Ship-E: there is no api-stage L1, so the second resolve
// dispatches the K8s LIST again (2 server hits) and apistage_store/hit
// counters do not exist / stay 0.
func TestFalsifierFE_ApistageL1HitSkipsKubeCall(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	cache.ResetDepsForTest()
	t.Cleanup(func() {
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	srv, hits := newApistageCountingServer(t)

	// Cold resolve — must dispatch the stage's K8s LIST once.
	first := apistageResolveOnce(t, srv.URL)
	if got := hits.Load(); got != 1 {
		t.Fatalf("FE: cold resolve made %d K8s calls, want 1", got)
	}
	store := cache.ResolvedCache()
	if store == nil {
		t.Fatalf("FE: resolved cache nil under RESOLVED_CACHE_ENABLED=true")
	}
	if s := store.Stats(); s.ApistageStoreTotal == 0 {
		t.Fatalf("FE: cold resolve did not Put an api-stage L1 entry "+
			"(apistage_store_total=0) — the key-swap never ran")
	}

	// Second resolve of the SAME RESTAction — must be an api-stage L1
	// hit: ZERO additional K8s calls.
	second := apistageResolveOnce(t, srv.URL)
	if got := hits.Load(); got != 1 {
		t.Fatalf("FE: second resolve made %d total K8s calls, want 1 "+
			"(the shared stage must be served from the api-stage L1 — "+
			"no api-stage L1 means the LIST is re-dispatched)", got)
	}

	// AC-E2: the L1-hit output is byte-identical to the cold resolve.
	if !apistageDictEqual(first, second) {
		t.Fatalf("FE: api-stage L1 hit output differs from the cold resolve\n"+
			"cold:   %#v\nhit:    %#v", first, second)
	}
}

// TestFalsifierFE_FlagOffBehaviorNeutral is the AC-E1 behaviour-neutral
// control: with RESOLVED_CACHE_APISTAGE_ENABLED unset, the second
// resolve dispatches the K8s LIST AGAIN — the resolver is byte-identical
// to 0.30.115 (no api-stage L1, no key-swap).
func TestFalsifierFE_FlagOffBehaviorNeutral(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	// RESOLVED_CACHE_APISTAGE_ENABLED deliberately NOT set → flag off.
	cache.ResetResolvedCacheForTest()
	cache.ResetDepsForTest()
	t.Cleanup(func() {
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	if cache.ApistageL1Enabled() {
		t.Fatalf("FE-flagoff: ApistageL1Enabled() true with the flag unset")
	}

	srv, hits := newApistageCountingServer(t)

	apistageResolveOnce(t, srv.URL)
	apistageResolveOnce(t, srv.URL)

	// Flag off: NO api-stage L1 — both resolves dispatch the K8s LIST.
	if got := hits.Load(); got != 2 {
		t.Fatalf("FE-flagoff: made %d K8s calls, want 2 (flag-off the api-stage "+
			"key-swap must not run — every resolve dispatches the stage)", got)
	}
	if store := cache.ResolvedCache(); store != nil {
		if s := store.Stats(); s.ApistageStoreTotal != 0 {
			t.Fatalf("FE-flagoff: apistage_store_total=%d, want 0 — the key-swap "+
				"ran despite the flag being off", s.ApistageStoreTotal)
		}
	}
}

// apistageDictEqual reports whether two resolver dicts carry the same
// stage output, compared via their JSON encoding (the resolver dict's
// values are all JSON-shaped, so a marshal comparison is exact and
// stable — json.Marshal sorts map keys).
func apistageDictEqual(a, b map[string]any) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return false
	}
	return string(ab) == string(bb)
}

// --- Ship E (0.30.116) AC coverage — O5 filter-hash + per-user key ----------

// TestAC_E5_FilterHashWhitespaceStable asserts AC-E5: the O5 canonical
// filter-hash is stable across whitespace / render variation. Two
// Helm-rendered forms of the same jq filter that differ only in
// interior whitespace, indentation, or newlines produce the SAME stage
// discriminator — so the stage key does not churn across deployments.
func TestAC_E5_FilterHashWhitespaceStable(t *testing.T) {
	dict := map[string]any{}
	variants := []string{
		`.items[] | select(.metadata.name != null)`,
		`  .items[]  |  select(.metadata.name != null)  `,
		".items[]\n  | select(.metadata.name != null)",
		"\t.items[] |\tselect(.metadata.name != null)\n",
	}
	want := stageDiscriminator(&templates.API{Name: "s", Filter: ptr.To(variants[0])}, dict)
	for i, v := range variants {
		got := stageDiscriminator(&templates.API{Name: "s", Filter: ptr.To(v)}, dict)
		if got != want {
			t.Fatalf("AC-E5: filter variant %d produced a different stage discriminator\n"+
				"variant: %q\nwant disc: %q\ngot disc:  %q", i, v, want, got)
		}
	}
	// A genuinely different filter MUST produce a different discriminator.
	other := stageDiscriminator(&templates.API{
		Name: "s", Filter: ptr.To(".items[] | select(.spec.foo)"),
	}, dict)
	if other == want {
		t.Fatalf("AC-E5: a semantically different filter collided with the base discriminator")
	}
}

// TestAC_E4_StageKeyPerUserNeverCohort asserts AC-E4: the stage key is
// per-user keyed — two distinct identities resolving the same stage of
// the same RESTAction get DISTINCT stage keys. A cohort/groups-only key
// would let user B serve user A's RBAC-narrowed stage output; the
// per-user key makes that structurally impossible
// (feedback_l1_per_user_keyed_never_cohort).
func TestAC_E4_StageKeyPerUserNeverCohort(t *testing.T) {
	stage := apistageTestStage()
	dict := map[string]any{}

	keyA := cache.ComputeKey(stageKeyInputs(
		"ns", "ra", "alice", []string{"devs"}, stage, dict, 0, 0))
	keyB := cache.ComputeKey(stageKeyInputs(
		"ns", "ra", "bob", []string{"devs"}, stage, dict, 0, 0))
	if keyA == keyB {
		t.Fatalf("AC-E4: two distinct usernames produced the SAME stage key — " +
			"the api-stage L1 is not per-user keyed (RBAC cross-user leak risk)")
	}

	// Same user, DIFFERENT groups → still a distinct key (groups are part
	// of identity, RBAC narrowing depends on them).
	keyA2 := cache.ComputeKey(stageKeyInputs(
		"ns", "ra", "alice", []string{"devs", "admins"}, stage, dict, 0, 0))
	if keyA == keyA2 {
		t.Fatalf("AC-E4: a group-set change did not shift the stage key")
	}

	// SAME identity + SAME stage → SAME key (the reuse property — this is
	// what makes the L1 hit possible).
	keyARepeat := cache.ComputeKey(stageKeyInputs(
		"ns", "ra", "alice", []string{"devs"}, stage, dict, 0, 0))
	if keyA != keyARepeat {
		t.Fatalf("AC-E4: the same identity + stage produced different keys — no reuse")
	}
}

// TestAC_StageInputHash_ReflectsDependsOnOutput asserts the stage key
// folds in the effective dict input: when a stage's dependsOn
// predecessor output changes, the stage discriminator changes (so a
// stale predecessor result cannot serve a fresh stage from L1).
func TestAC_StageInputHash_ReflectsDependsOnOutput(t *testing.T) {
	stage := &templates.API{
		Name:      "stage2",
		Filter:    ptr.To(".items"),
		DependsOn: &templates.Dependency{Name: "stage1"},
	}
	discA := stageDiscriminator(stage, map[string]any{
		"stage1": map[string]any{"items": []any{"ns-a", "ns-b"}},
	})
	discB := stageDiscriminator(stage, map[string]any{
		"stage1": map[string]any{"items": []any{"ns-a", "ns-b", "ns-c"}},
	})
	if discA == discB {
		t.Fatalf("stage discriminator did not change when the dependsOn " +
			"predecessor output changed — a stale input could serve a fresh stage")
	}
	// No dependsOn + empty dict → a stable, input-independent discriminator.
	root := &templates.API{Name: "root", Filter: ptr.To(".items")}
	if stageDiscriminator(root, map[string]any{}) != stageDiscriminator(root, map[string]any{"unrelated": 1}) {
		t.Fatalf("a root stage's discriminator must not depend on unrelated dict keys")
	}
}
