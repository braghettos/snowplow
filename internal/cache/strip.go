package cache

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

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
