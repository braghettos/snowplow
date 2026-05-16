// informer_serve.go — Tag 0.30.96 Gap A: serve widget / RESTAction
// entry-CR object GETs from the in-process informer cache.
//
// Background. The 0.30.95 resolver pivot (`internal/resolvers/restactions/
// api/informer_dispatch.go`) routed RESTAction inner-call GET reads to the
// informer. It did NOT touch `objects.Get` — the entry point every widget
// object-read and every RESTAction entry-CR fetch goes through
// (`dispatchers.fetchObject` → `objects.Get`, and the apiref widget →
// RESTAction bridge `widgets/apiref.Resolve`). A re-audit (task
// a50caf240f3e2be19) proved `objects.Get`'s "routed branch" was a
// placeholder stub that unconditionally called `getFromAPIServer`, paying
// a full apiserver round-trip the informer already holds. 0.30.96 fills
// that gap.
//
// Gating. Reuse the SAME `RESOLVER_USE_INFORMER` env flag as the 0.30.95
// pivot. With the flag unset the routed branch is a no-op and the binary
// is byte-identical to 0.30.95 — preserves the architect's R-FALSE-1
// invariant. We deliberately do NOT import the `restactions/api`
// `resolverUseInformer()` helper: although `api` does not import
// `objects` today, adding an `objects → api` edge would be a fragile
// cross-package coupling that a future refactor could turn into an import
// cycle. The flag check is a one-line `os.Getenv`; replicating it here is
// cheaper than the coupling risk (task guidance, explicit).
//
// Byte-equivalence (`feedback_cache_must_not_constrain_jq.md`). The
// informer-served object MUST be stripped identically to
// `getFromAPIServer`'s output so the downstream JQ pipeline sees the same
// bytes regardless of which branch served the read. `getFromAPIServer`
// does exactly two strips: (1) deletes the
// `kubectl.kubernetes.io/last-applied-configuration` annotation, (2)
// `SetManagedFields(nil)`. `stripForServe` below applies precisely those
// two — see the inline cross-reference.
//
// Per `feedback_no_special_cases.md`: the routed branch is uniform across
// every GVR. The gate is cache-mode + informer-state predicates, not a
// per-resource switch.

package objects

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// resolverUseInformerEnv is the env-var key shared with the 0.30.95
// resolver pivot. Keeping the literal identical (not the helper) is
// intentional — see the package-doc rationale on the coupling risk.
const resolverUseInformerEnv = "RESOLVER_USE_INFORMER"

// useInformer reports whether the 0.30.96 routed branch is active.
// Matches the 0.30.95 pivot's `resolverUseInformer() == "true"` check:
// lowercased + trimmed, compared against the exact string "true".
// "shadow" / "" / anything else => OFF (the pivot reserves "shadow" but
// never wired it; we treat it as OFF identically).
//
// Read on every call: env-var reads are sub-microsecond against the Go
// runtime envcache and letting operators flip the gate without a pod
// restart is worth the negligible cost.
func useInformer() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv(resolverUseInformerEnv))) == "true"
}

// Serve-rate counters for the 0.30.96 falsifier. Package-level atomics;
// safe to Add/Load without external locking. The bench could not measure
// the 0.30.95 pivot serve-rate from per-call debug logs at 50K volume —
// these counters surface a STABLE, greppable summary line instead.
var (
	// objectsGetInformerServed counts GETs answered from the informer
	// indexer (cache hit, byte-stripped, returned without apiserver I/O).
	objectsGetInformerServed atomic.Uint64
	// objectsGetApiserverFallthrough counts GETs that took the apiserver
	// branch — flag off, cache disabled, passthrough, metadata-only GVR,
	// not-synced informer, GET-miss, or parse failure.
	objectsGetApiserverFallthrough atomic.Uint64
)

// ObjectsGetStats is an atomic snapshot of the serve-rate counters.
// Numbers may drift between the two fields by a single in-flight call,
// which is fine for log aggregation.
type ObjectsGetStats struct {
	InformerServed       uint64
	ApiserverFallthrough uint64
}

// ObjectsGetStatsSnapshot returns the current counter values. Exported so
// tests can assert increments deterministically.
func ObjectsGetStatsSnapshot() ObjectsGetStats {
	return ObjectsGetStats{
		InformerServed:       objectsGetInformerServed.Load(),
		ApiserverFallthrough: objectsGetApiserverFallthrough.Load(),
	}
}

// objects_get summary goroutine knobs. Mirrors resolved.go's
// `RESOLVED_CACHE_SUMMARY_EVERY_SECONDS` pattern.
const (
	envObjectsGetSummaryEvery       = "OBJECTS_GET_SUMMARY_EVERY_SECONDS"
	defaultObjectsGetSummarySeconds = 60
)

var objectsGetSummaryOnce sync.Once

// startObjectsGetSummary launches a single bounded goroutine that emits
// an `objects_get.summary` INFO line every N seconds. Lifecycle bound: a
// sync.Once guarantees exactly one goroutine for the process lifetime; it
// does constant work per tick and is never stopped (process-scoped, same
// contract as `startResolvedCacheSummary`).
//
// Started lazily on the first routed `Get` call rather than from main.go
// — avoids a startup-wiring change and keeps the goroutine from existing
// at all when the flag is never enabled.
func startObjectsGetSummary() {
	objectsGetSummaryOnce.Do(func() {
		every := time.Duration(objectsGetSummaryEverySeconds()) * time.Second
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for range t.C {
				s := ObjectsGetStatsSnapshot()
				// STABLE single-line falsifier shape (greppable):
				//   objects_get.summary informer_served=N apiserver_fallthrough=M
				slog.Info("objects_get.summary",
					slog.String("subsystem", "cache"),
					slog.Uint64("informer_served", s.InformerServed),
					slog.Uint64("apiserver_fallthrough", s.ApiserverFallthrough),
				)
			}
		}()
	})
}

// objectsGetSummaryEverySeconds resolves the summary interval from the
// env knob, falling back to the default on unset / non-int / non-positive.
func objectsGetSummaryEverySeconds() int {
	v := os.Getenv(envObjectsGetSummaryEvery)
	if v == "" {
		return defaultObjectsGetSummarySeconds
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultObjectsGetSummarySeconds
	}
	return n
}

// stripForServe applies the EXACT field strips `getFromAPIServer`
// performs, so an informer-served object is byte-equivalent to an
// apiserver-served one for the downstream JQ pipeline.
//
// Cross-reference — getFromAPIServer (get.go) does precisely:
//
//	annotations := uns.GetAnnotations()
//	if annotations != nil {
//	    delete(annotations, lastAppliedConfigAnnotation)
//	    uns.SetAnnotations(annotations)
//	}
//	uns.SetManagedFields(nil)
//
// stripForServe mirrors that 1:1. The caller passes a DeepCopy so the
// strip never mutates the shared informer-store object.
func stripForServe(uns *unstructured.Unstructured) {
	annotations := uns.GetAnnotations()
	if annotations != nil {
		delete(annotations, lastAppliedConfigAnnotation)
		uns.SetAnnotations(annotations)
	}
	uns.SetManagedFields(nil)
}

// filterGetByRBAC — Tag 0.30.101: GET-verb RBAC check for the
// objects.Get informer-served branch. The GET-path sibling of Tag A
// (0.30.100), which fixed the same over-exposure class on the resolver
// pivot's LIST branch (filterListByRBAC, internal/resolvers/restactions/
// api/informer_dispatch_rbac.go).
//
// THE BUG: objects.Get's informer-served branch returned the raw
// informer object (rw.GetObject → stripForServe) with ZERO RBAC check.
// objects.Get's apiserver branch (getFromAPIServer) reads the per-user
// `<username>-clientconfig` endpoint via xcontext.UserConfig and issues
// the GET under the user's own bearer token — so the apiserver applies
// the user's RBAC. The informer branch bypasses that token entirely; a
// narrow-RBAC user GETting a known object name in a namespace they have
// no `get` grant for received the object.
//
// THE FIX: filterGetByRBAC runs the hit object through a `get`-verb
// EvaluateRBAC check against the in-process, tokenless typed-RBAC
// indexer (internal/rbac/evaluate.go) — the same evaluator the
// userAccessFilter refilter and Tag A's LIST filter use.
//
// Return contract:
//   - true  — identity present AND the user has a `get` grant for the
//     object's namespace. Caller serves the informer object.
//   - false — caller MUST NOT serve; fall through to getFromAPIServer
//     (which issues the GET under the user's own token — a denied GET
//     becomes the apiserver's authoritative 403). false covers every
//     fail-closed path: no identity on the context, an EvaluateRBAC
//     error, or an EvaluateRBAC deny. Under no path does an
//     unauthorized informer object reach the caller.
//
// gvr supplies the API group + resource for the EvaluateRBAC tuple; the
// verb is fixed "get". namespace is the object's OWN metadata.namespace
// (cluster-scoped object, namespace=="", evaluates against cluster-wide
// RBAC — the correct apiserver-equivalent semantics) — mirrors Tag A's
// filterListByRBAC, which uses each item's own namespace.
//
// We deliberately replicate the api package's filterGetByRBAC rather
// than import it: `objects` must not gain an `objects → restactions/api`
// import edge (the informer_serve.go package doc spells out the
// import-cycle risk). `internal/rbac` imports only `internal/cache`, so
// `objects → rbac` is cycle-free. The helper body is ~10 lines; the
// duplication is cheaper than the coupling.
//
// Per feedback_no_special_cases.md: uniform over every GVR — no
// per-resource carve-out.
func filterGetByRBAC(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) bool {
	log := xcontext.Logger(ctx)

	if obj == nil {
		// Defensive: the caller only reaches here on an indexer HIT
		// (obj non-nil), but the helper fails closed rather than trust
		// that invariant blindly.
		return false
	}

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		// FAIL-CLOSED: no identity → cannot vouch for the object. Fall
		// through to the apiserver, whose per-user-token gate is the
		// authoritative answer.
		log.Warn("objects.Get.rbac_get_filter.no_identity",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("namespace", obj.GetNamespace()),
			slog.String("name", obj.GetName()),
			slog.Any("err", err),
			slog.String("action", "fallthrough_to_apiserver"),
		)
		return false
	}

	allowed, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  user.Username,
		Groups:    user.Groups,
		Verb:      "get",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: obj.GetNamespace(),
	})
	if err != nil {
		// FAIL-CLOSED: an evaluator hiccup never permits a serve.
		log.Warn("objects.Get.rbac_get_filter.evaluate_error",
			slog.String("subsystem", "cache"),
			slog.String("user", user.Username),
			slog.String("gvr", gvr.String()),
			slog.String("namespace", obj.GetNamespace()),
			slog.String("name", obj.GetName()),
			slog.Any("err", err),
			slog.String("action", "fallthrough_to_apiserver"),
		)
		return false
	}

	log.Debug("objects.Get.rbac_get_filter",
		slog.String("subsystem", "cache"),
		slog.String("user", user.Username),
		slog.String("gvr", gvr.String()),
		slog.String("namespace", obj.GetNamespace()),
		slog.String("name", obj.GetName()),
		slog.Bool("allowed", allowed),
	)
	return allowed
}
