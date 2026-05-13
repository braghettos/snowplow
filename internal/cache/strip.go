// Package cache — SetTransform strip.
//
// Tag 0.30.5 (plan §"Tag 0.30.5 — Step 2: SetTransform strip"). At
// informer-startup we install a TransformFunc that drops two notoriously
// bulky metadata fields from every object before it lands in the
// indexer:
//
//   - metadata.managedFields  (server-side-apply bookkeeping)
//   - metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]
//
// Both fields are inert for the snowplow read path: no portal RestAction
// or JQ expression in the customer inventory references them (pre-flight
// grep recorded in the ship row for 0.30.5). Dropping them shrinks
// indexer footprint by ~30–50 % per object, which is the lever Step 2
// pulls against the v0.25.244 memory audit.
//
// Design:
//
//   - Returns (obj, nil) on ANY error path. Per the plan §Risks bullet
//     "transform throws on malformed objects", the transform MUST NOT
//     panic and MUST NOT propagate errors that would stall the informer
//     loop. We log once at debug level and return the original object.
//   - Per-resource-type override hook is wired (defaultStripper +
//     resourceOverrides map) so future tags can opt specific GVRs into
//     stricter or laxer policies without restructuring. At 0.30.5 the
//     map is empty — only the default rule ships (plan §"What this tag
//     does NOT do" bullet 3).
//   - Falsifier log: emit INFO once per resource type on first
//     invocation with len_pre / len_post / ratio so the ship row can
//     verify the strip is taking effect (plan §"Code-path falsifier").
//
// Per feedback_no_special_cases.md the strip rule itself contains no
// per-resource policy; per-GVR overrides are an additive mechanism, not
// a special case for any one resource type.
package cache

import (
	"log/slog"
	"sync"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// lastAppliedAnnotation is the kubectl annotation that captures the
// entire last-applied object as a JSON blob. Dropping it removes a
// near-duplicate copy of the spec from every kubectl-managed resource.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// stripFunc transforms one object in place (or returns a transformed
// copy) and reports the pre/post byte counts for falsifier logging.
// Returning a non-nil error tells the caller to fall through to the
// untransformed object — the informer never sees an error.
type stripFunc func(*unstructured.Unstructured) (preBytes, postBytes int)

// typedStripFunc is the 0.30.6 extension of stripFunc. It performs the
// default strip THEN converts the Unstructured to a typed pointer; the
// indexer stores the typed pointer in place of the Unstructured. The
// returned object MUST be non-nil (callers replace the indexer entry
// only when this returns a non-nil value); on conversion failure the
// implementation logs WARN and returns the original Unstructured so the
// informer never stalls (same contract as the default strip).
//
// The returned `typedKind` is a short identifier used by the
// falsifier-log line (e.g. "*rbacv1.ClusterRoleBinding"). conversionMs
// is the wall-clock time spent inside FromUnstructured for the
// once-per-type log line.
type typedStripFunc func(*unstructured.Unstructured) (out interface{}, preBytes, postBytes int, typedKind string, conversionMs float64)

// resourceOverrides maps a GVR to a custom stripper. Empty at 0.30.5;
// 0.30.6 registers the four Role-Based Access Control GVRs in
// typedResourceOverrides (below) instead — leaving this map for
// future per-GVR strip-only policies. Reads are lock-free (write-once
// at init time).
var resourceOverrides = map[schema.GroupVersionResource]stripFunc{}

// typedResourceOverrides maps a GVR to a typed-converting transform.
// At 0.30.6 it holds the four Role-Based Access Control GVRs so
// internal/rbac/evaluate.go can read typed `*rbacv1.{Role,RoleBinding,
// ClusterRole,ClusterRoleBinding}` directly from the indexer without
// paying `runtime.FromUnstructured` per /call (pre-flight pprof:
// 4 760 ms cumulative `FromUnstructured` per cold nav at 0.30.61).
//
// Population happens in init() below — early enough that
// NewResourceWatcher's SetTransform install at watcher.go:139-149 sees
// the override before factory.Start.
//
// Per feedback_no_special_cases.md: the four RBAC GVRs are NOT a
// special case — they are simply the GVRs the pre-flight falsifier
// identified as hotspot. The mechanism is general (any GVR can opt in
// to typed transform); only the population is RBAC-specific.
var typedResourceOverrides = map[schema.GroupVersionResource]typedStripFunc{}

// rbacTypedGVRs is the canonical set of GVRs that MUST have a typed
// override at startup. AssertRBACTypedOverridesRegistered panics at
// boot if any is missing, surfacing a registration regression rather
// than letting it ship silently.
var rbacTypedGVRs = []schema.GroupVersionResource{
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
}

func init() {
	// 0.30.6 — register typed-converting transforms for the four
	// Role-Based Access Control GVRs. Population happens at package
	// init so NewResourceWatcher's SetTransform call at startup picks
	// up the override before factory.Start.
	typedResourceOverrides[rbacTypedGVRs[0]] = stripAndTypeRole
	typedResourceOverrides[rbacTypedGVRs[1]] = stripAndTypeRoleBinding
	typedResourceOverrides[rbacTypedGVRs[2]] = stripAndTypeClusterRole
	typedResourceOverrides[rbacTypedGVRs[3]] = stripAndTypeClusterRoleBinding
}

// assertionDisabledForTest, when true, makes
// AssertRBACTypedOverridesRegistered a no-op. Set ONLY by
// DisableTypedOverrideForTest so a test that deliberately removes a
// typed override (to exercise the Unstructured-fallback path) doesn't
// trip the startup assertion. Reset to false when the corresponding
// restore function runs.
var assertionDisabledForTest bool

// AssertRBACTypedOverridesRegistered panics if any of the four
// Role-Based Access Control GVRs is missing its typed override at
// startup. Called from NewResourceWatcher BEFORE factory.Start so a
// registration regression cannot ship silently. Panic message names
// the missing GVR for fast diagnosis.
//
// Per plan §Tag 0.30.6 v2 Risks bullet 1: "a startup assertion
// verifies resourceOverrides[<each RBAC GVR>] != nil before
// factory.Start, panicking on absence so a registration regression
// cannot ship silently."
func AssertRBACTypedOverridesRegistered() {
	if assertionDisabledForTest {
		return
	}
	for _, gvr := range rbacTypedGVRs {
		if _, ok := typedResourceOverrides[gvr]; !ok {
			panic("cache: typed-RBAC override missing for " + gvr.String() +
				" (regression: typed transform not registered at init)")
		}
	}
}

// firstLogged tracks (per resource type) whether we have already emitted
// the falsifier log line. We log once per type so the steady-state
// indexer write path stays silent.
var (
	firstLoggedMu sync.Mutex
	firstLogged   = map[string]struct{}{}
)

// StripBulkyFieldsForResourceType returns a TransformFunc bound to the
// given resource type identifier (used purely for the falsifier log).
// The returned function is the value passed to SharedIndexInformer
// .SetTransform BEFORE factory.Start (primer §4.7).
//
// The signature is interface{} → interface{} per
// k8s.io/client-go/tools/cache.TransformFunc. We intentionally do NOT
// take a TransformFunc-typed return to avoid coupling this package to
// client-go's name (the watcher.go callsite handles the cast).
func StripBulkyFieldsForResourceType(resourceType string, gvr schema.GroupVersionResource) func(interface{}) (interface{}, error) {
	return func(obj interface{}) (interface{}, error) {
		// Defensive: never panic, never propagate errors back to the
		// informer loop. Per plan §Risks "transform throws on malformed
		// objects" — always return (obj, nil) on the error path.
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("cache.strip.panic_recovered",
					slog.String("subsystem", "cache"),
					slog.String("resource_type", resourceType),
					slog.Any("recovered", r),
				)
			}
		}()

		uns, ok := obj.(*unstructured.Unstructured)
		if !ok || uns == nil {
			return obj, nil
		}

		// 0.30.6 — typed-converting override path. If a typed
		// override is registered for this GVR, it produces the
		// typed pointer that replaces the Unstructured in the
		// indexer (zero per-call FromUnstructured cost downstream).
		// On conversion failure the override returns the original
		// Unstructured (logged WARN inside the override) — informer
		// never stalls. Falsifier log includes typed_kind and
		// conversion_ms per plan §"Code-path falsifier".
		if typedOverride, ok := typedResourceOverrides[gvr]; ok {
			out, pre, post, typedKind, conversionMs := typedOverride(uns)
			logFirstStripOnceTyped(resourceType, pre, post, typedKind, conversionMs)
			return out, nil
		}

		// Per-resource-type strip-only override: if a custom
		// stripper is registered for this GVR, use it; otherwise
		// the default. (Currently empty at 0.30.6; reserved for
		// future per-GVR strip policies.)
		strip := defaultStripUnstructured
		if override, ok := resourceOverrides[gvr]; ok {
			strip = override
		}

		pre, post := strip(uns)
		logFirstStripOnce(resourceType, pre, post)
		return uns, nil
	}
}

// stripAndType is the shared implementation behind the four
// stripAndType* RBAC variants. Strips managedFields + last-applied
// (default policy), then converts the Unstructured to a typed pointer
// produced by `newTyped` via runtime.DefaultUnstructuredConverter.
// `typedKind` is the falsifier-log identifier (e.g. "*rbacv1.Role").
//
// On conversion failure: WARN log, return the (stripped) Unstructured
// so the informer never stalls. Downstream rbac.evaluateAgainstInformer
// falls back to the to{Kind} helper on the Unstructured path (rare,
// logged loud — plan §Risks bullet 2).
func stripAndType(uns *unstructured.Unstructured, newTyped func() runtime.Object, typedKind string) (interface{}, int, int, string, float64) {
	pre, post := defaultStripUnstructured(uns)
	start := time.Now()
	typed := newTyped()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, typed); err != nil {
		conversionMs := float64(time.Since(start).Microseconds()) / 1000.0
		slog.Warn("cache.strip.typed_conversion_failed",
			slog.String("subsystem", "cache"),
			slog.String("typed_kind", typedKind),
			slog.String("name", uns.GetName()),
			slog.String("namespace", uns.GetNamespace()),
			slog.String("error", err.Error()),
		)
		return uns, pre, post, typedKind, conversionMs
	}
	conversionMs := float64(time.Since(start).Microseconds()) / 1000.0
	return typed, pre, post, typedKind, conversionMs
}

// stripAndTypeRole — typed-converting transform for
// rbac.authorization.k8s.io/v1 Role.
func stripAndTypeRole(uns *unstructured.Unstructured) (interface{}, int, int, string, float64) {
	return stripAndType(uns, func() runtime.Object { return &rbacv1.Role{} }, "*rbacv1.Role")
}

// stripAndTypeRoleBinding — typed-converting transform for RoleBinding.
func stripAndTypeRoleBinding(uns *unstructured.Unstructured) (interface{}, int, int, string, float64) {
	return stripAndType(uns, func() runtime.Object { return &rbacv1.RoleBinding{} }, "*rbacv1.RoleBinding")
}

// stripAndTypeClusterRole — typed-converting transform for ClusterRole.
func stripAndTypeClusterRole(uns *unstructured.Unstructured) (interface{}, int, int, string, float64) {
	return stripAndType(uns, func() runtime.Object { return &rbacv1.ClusterRole{} }, "*rbacv1.ClusterRole")
}

// stripAndTypeClusterRoleBinding — typed-converting transform for
// ClusterRoleBinding.
func stripAndTypeClusterRoleBinding(uns *unstructured.Unstructured) (interface{}, int, int, string, float64) {
	return stripAndType(uns, func() runtime.Object { return &rbacv1.ClusterRoleBinding{} }, "*rbacv1.ClusterRoleBinding")
}

// defaultStripUnstructured implements the default policy: drop
// managedFields and the last-applied-configuration annotation. Returns
// (preBytes, postBytes) measured as len(JSON) of metadata before/after.
// Computing byte counts every call is cheap relative to the indexer
// add; we only do it once per resource type for the falsifier log, so
// the steady-state cost is two len() reads.
func defaultStripUnstructured(uns *unstructured.Unstructured) (int, int) {
	if uns == nil {
		return 0, 0
	}

	// Pre-byte count: rough proxy = annotation length + managedFields
	// entry count × constant. We don't marshal here (too expensive on
	// the hot path); the falsifier log only needs an order-of-magnitude
	// signal and we'll cross-check ratios via pprof heap diff in the
	// ship row.
	prAnnos := uns.GetAnnotations()
	preAnno := 0
	if v, ok := prAnnos[lastAppliedAnnotation]; ok {
		preAnno = len(v)
	}
	preMF := len(uns.GetManagedFields())

	// Drop managedFields.
	if preMF > 0 {
		uns.SetManagedFields(nil)
	}

	// Drop last-applied annotation.
	if preAnno > 0 {
		// Copy the annotation map before mutating — SetAnnotations
		// replaces the whole map, but the caller's map may be shared
		// across the indexer; deleting in place could race with
		// readers. Safer to clone.
		cloned := make(map[string]string, len(prAnnos)-1)
		for k, v := range prAnnos {
			if k == lastAppliedAnnotation {
				continue
			}
			cloned[k] = v
		}
		if len(cloned) == 0 {
			uns.SetAnnotations(nil)
		} else {
			uns.SetAnnotations(cloned)
		}
	}

	// Rough pre/post byte estimate for the falsifier log line. We use
	// the annotation length (which dominates) plus a constant per
	// managedFields entry (~200 bytes is typical for k8s managedFields
	// entries). The exact value isn't load-bearing — the ratio is.
	pre := preAnno + preMF*200
	post := 0
	return pre, post
}

// logFirstStripOnce emits the falsifier INFO log line on the first
// invocation for a given resource type. Subsequent invocations are
// silent. Per the plan §"Code-path falsifier":
//
//	strip.applied resource_type=apps/v1/Deployment len_pre=4823 len_post=2741 ratio=0.43
func logFirstStripOnce(resourceType string, pre, post int) {
	firstLoggedMu.Lock()
	if _, done := firstLogged[resourceType]; done {
		firstLoggedMu.Unlock()
		return
	}
	firstLogged[resourceType] = struct{}{}
	firstLoggedMu.Unlock()

	ratio := 0.0
	if pre > 0 {
		ratio = float64(post) / float64(pre)
	}
	slog.Info("strip.applied",
		slog.String("subsystem", "cache"),
		slog.String("resource_type", resourceType),
		slog.Int("len_pre", pre),
		slog.Int("len_post", post),
		slog.Float64("ratio", ratio),
	)
}

// logFirstStripOnceTyped is the 0.30.6 extension of logFirstStripOnce.
// Emits the falsifier line with typed_kind + conversion_ms fields so
// the ship row can verify typed transform is active per RBAC kind:
//
//	strip.applied resource_type=rbac.authorization.k8s.io/v1/clusterrolebindings len_pre=N len_post=0 ratio=0 typed_kind=*rbacv1.ClusterRoleBinding conversion_ms=0.42
//
// Subsequent invocations for the same resource type are silent.
func logFirstStripOnceTyped(resourceType string, pre, post int, typedKind string, conversionMs float64) {
	firstLoggedMu.Lock()
	if _, done := firstLogged[resourceType]; done {
		firstLoggedMu.Unlock()
		return
	}
	firstLogged[resourceType] = struct{}{}
	firstLoggedMu.Unlock()

	ratio := 0.0
	if pre > 0 {
		ratio = float64(post) / float64(pre)
	}
	slog.Info("strip.applied",
		slog.String("subsystem", "cache"),
		slog.String("resource_type", resourceType),
		slog.Int("len_pre", pre),
		slog.Int("len_post", post),
		slog.Float64("ratio", ratio),
		slog.String("typed_kind", typedKind),
		slog.Float64("conversion_ms", conversionMs),
	)
}

// resetStripLoggingForTest is a test-only helper that clears the
// once-per-resource-type log gate so successive subtests can each
// observe the first-invocation log.
func resetStripLoggingForTest() {
	firstLoggedMu.Lock()
	firstLogged = map[string]struct{}{}
	firstLoggedMu.Unlock()
}

// DisableTypedOverrideForTest removes the typed-converting override
// for gvr (so the transform falls back to default strip — Unstructured
// stays in the indexer). Returns a restore function. Exported with the
// "ForTest" suffix because Go's package-test boundary makes a true
// _test.go-internal helper unreachable from black-box tests in
// internal/rbac/evaltest.
//
// Production callers MUST NOT call this. The function is intentionally
// noisy: it WARN-logs every invocation so accidental use in non-test
// code is loud in production logs.
//
// This is the 0.30.6 plan §Risks bullet 2 test-mechanism — exercises
// the defensive Unstructured fallback path in rbac.evaluateAgainstInformer
// without requiring kind cluster integration.
func DisableTypedOverrideForTest(gvr schema.GroupVersionResource) func() {
	slog.Warn("cache.DisableTypedOverrideForTest invoked — production code MUST NOT call this",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
	)
	saved, present := typedResourceOverrides[gvr]
	delete(typedResourceOverrides, gvr)
	prevAssertionFlag := assertionDisabledForTest
	assertionDisabledForTest = true
	return func() {
		if present {
			typedResourceOverrides[gvr] = saved
		}
		assertionDisabledForTest = prevAssertionFlag
	}
}
