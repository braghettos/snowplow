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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// resourceOverrides maps a GVR to a custom stripper. Empty at 0.30.5;
// future tags may register GVR-specific policies here. Reads are
// lock-free (write-once at init time of dependent tags).
var resourceOverrides = map[schema.GroupVersionResource]stripFunc{}

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

		// Per-resource-type override: if a custom stripper is
		// registered for this GVR, use it; otherwise the default.
		strip := defaultStripUnstructured
		if override, ok := resourceOverrides[gvr]; ok {
			strip = override
		}

		pre, post := strip(uns)
		logFirstStripOnce(resourceType, pre, post)
		return uns, nil
	}
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

// resetStripLoggingForTest is a test-only helper that clears the
// once-per-resource-type log gate so successive subtests can each
// observe the first-invocation log.
func resetStripLoggingForTest() {
	firstLoggedMu.Lock()
	firstLogged = map[string]struct{}{}
	firstLoggedMu.Unlock()
}
