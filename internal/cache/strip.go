package cache

import (
	"encoding/json"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// envCacheBulkyStripEnabled controls whether StripBulkyFieldsForGVR
// applies the per-GVR strip set in addition to the universal
// StripAnnotationsFromUnstructured base. Default true; set to "false"
// to disable for debugging.
//
// Q-OOM-COMPLETION (v0.25.315) Patch 3.
const envCacheBulkyStripEnabled = "CACHE_BULKY_STRIP_ENABLED"

// compositionGroup is the api group prefix for composition CRs (e.g.
// "fireworksapp.composition.krateo.io"). The per-GVR strip applies
// only to objects in this group hierarchy.
const compositionGroupSuffix = "composition.krateo.io"

// bulkyStripEnabled reads the env on each call. The strip path runs at
// most once per object enter-cache event, so the env-read overhead is
// negligible (millions of objects per minute would still be sub-µs).
func bulkyStripEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envCacheBulkyStripEnabled)))
	if v == "" {
		return true
	}
	return v != "false" && v != "0" && v != "no"
}

// IsCompositionGVR returns true for GVRs under composition.krateo.io.
// Exposed so callers (informer transform, warmup loop) can decide
// whether to apply the per-GVR strip without re-implementing the
// suffix match.
func IsCompositionGVR(gvr schema.GroupVersionResource) bool {
	return strings.HasSuffix(gvr.Group, compositionGroupSuffix)
}

// StripBulkyFields removes kubectl.kubernetes.io/last-applied-configuration
// and metadata.managedFields from every object found recursively in the JSON
// payload. These fields are K8s internal bookkeeping and can account for
// 25-50% of the response size for list-type RESTActions.
// On any error the original bytes are returned unchanged.
func StripBulkyAnnotations(raw []byte) []byte {
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if !stripBulkyFieldsRecursive(obj) {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

const lastAppliedKey = "kubectl.kubernetes.io/last-applied-configuration"

// StripAnnotationsFromUnstructured removes bulky metadata from an
// Unstructured object in-place:
//   - metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]
//   - metadata.managedFields
//
// Returns true if any field was removed. The caller must DeepCopy the object
// first if the informer's shared copy must not be mutated.
func StripAnnotationsFromUnstructured(uns *unstructured.Unstructured) bool {
	changed := false

	// Strip last-applied-configuration annotation.
	ann := uns.GetAnnotations()
	if ann != nil {
		if _, ok := ann[lastAppliedKey]; ok {
			delete(ann, lastAppliedKey)
			if len(ann) == 0 {
				ann = nil
			}
			uns.SetAnnotations(ann)
			changed = true
		}
	}

	// Strip managedFields.
	if mf := uns.GetManagedFields(); len(mf) > 0 {
		uns.SetManagedFields(nil)
		changed = true
	}

	return changed
}

// StripBulkyFieldsForGVR applies StripAnnotationsFromUnstructured plus,
// for composition CRs (under composition.krateo.io), removes a small
// NARROW set of bulky fields that are confirmed UNUSED by every known
// widget JQ filter path. Returns true if any field was removed.
//
// Q-OOM-COMPLETION (v0.25.315) Patch 3 — NARROW SCOPE per widget-JQ
// validation 2026-05-07:
//
//   ✓ STRIP (confirmed unused):
//     - metadata.resourceVersion (informer holds its own; cached object
//       never needs to return this verbatim)
//     - metadata.generation
//     - status.observedGeneration
//
//   ✗ DO NOT STRIP (production widgets reference these):
//     - metadata.uid                — compositions-list filter (line 31)
//     - metadata.creationTimestamp  — compositions-list filter +
//                                      compositions-panels sort_by +
//                                      blueprints-panels sort_by
//     - status.conditions[]         — compositions-list filter (line 31)
//
// The architect's broader strip plan would have broken these production
// widgets. This narrowed scope preserves the per-object size win
// (~2-5% per composition) without touching any field a frontend
// resolves through.
//
// Idempotent. Safe to call multiple times. The caller must DeepCopy
// the object first if the informer's shared copy must not be mutated.
func StripBulkyFieldsForGVR(uns *unstructured.Unstructured, gvr schema.GroupVersionResource) bool {
	changed := StripAnnotationsFromUnstructured(uns)

	if !bulkyStripEnabled() {
		return changed
	}
	if !IsCompositionGVR(gvr) {
		return changed
	}

	obj := uns.Object

	// metadata.resourceVersion / metadata.generation
	if md, ok := obj["metadata"].(map[string]any); ok {
		if _, exists := md["resourceVersion"]; exists {
			delete(md, "resourceVersion")
			changed = true
		}
		if _, exists := md["generation"]; exists {
			delete(md, "generation")
			changed = true
		}
	}

	// status.observedGeneration
	if st, ok := obj["status"].(map[string]any); ok {
		if _, exists := st["observedGeneration"]; exists {
			delete(st, "observedGeneration")
			changed = true
		}
	}

	return changed
}

// stripBulkyFieldsRecursive walks maps and slices, removing
// last-applied-configuration annotations and managedFields from any
// metadata map found recursively. Returns true if any deletion was made.
func stripBulkyFieldsRecursive(v any) bool {
	changed := false
	switch val := v.(type) {
	case map[string]any:
		if metadata, ok := val["metadata"].(map[string]any); ok {
			// Strip last-applied-configuration annotation.
			if annotations, ok := metadata["annotations"].(map[string]any); ok {
				if _, exists := annotations[lastAppliedKey]; exists {
					delete(annotations, lastAppliedKey)
					changed = true
				}
			}
			// Strip managedFields.
			if _, exists := metadata["managedFields"]; exists {
				delete(metadata, "managedFields")
				changed = true
			}
		}
		for _, child := range val {
			if stripBulkyFieldsRecursive(child) {
				changed = true
			}
		}
	case []any:
		for _, item := range val {
			if stripBulkyFieldsRecursive(item) {
				changed = true
			}
		}
	}
	return changed
}
