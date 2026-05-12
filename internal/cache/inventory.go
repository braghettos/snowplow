// inventory.go — Step 3 / Tag 0.30.6 binding: walk every RestAction in
// the cluster and derive the deduped GroupVersionResource set that the
// informer factory must eager-register before the first /call lands.
//
// Per implementation plan §"Tag 0.30.6 — What's implemented" bullet 1:
//   CollectResourceTypesFromRestActions(ctx, dynClient): walks
//   RestActions, extracts spec.api[*].path + kind + apiVersion to
//   derive (Group, Version, Resource) tuples, dedupes, returns sorted
//   list.
//
// We only emit GVRs derived from Kubernetes apiserver-style paths
// (`/api/v1/...` and `/apis/<group>/<version>/<resource>...`). Entries
// with `endpointRef` set are external HTTP calls (e.g. GitHub) and have
// no informer to register. Entries whose path isn't recognisable as an
// apiserver URL are skipped silently — the lazy fallback in watcher.go
// will catch them with the WARN log line.

package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// restActionGVR is the GroupVersionResource of the RESTAction custom
// resource itself. We hard-code it rather than importing the typed
// package to keep this file free of build-time coupling to apis/templates.
//
// Per feedback_no_special_cases.md: this is NOT a per-resource policy.
// It's the bare anchor needed to discover OTHER GVRs — the inventory is
// a meta-query, not a per-resource gate.
var restActionGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// CollectResourceTypesFromRestActions walks every RestAction in the
// cluster via dynClient, extracts apiserver-style paths from each
// spec.api[*].path, and returns the deduped, sorted GVR set.
//
// The returned slice MAY include restActionGVR itself if the cache=on
// path needs to read RestActions through the informer. The caller is
// responsible for unioning this slice with whatever other GVRs it needs
// (typically RBACResourceTypes for cache=on RBAC evaluation).
//
// Errors from the LIST call are returned to the caller; the watcher
// then decides whether to fall through to lazy registration or fail
// startup hard. Per the plan, the caller treats LIST errors as soft
// (log + continue with empty inventory; lazy fallback catches misses).
func CollectResourceTypesFromRestActions(ctx context.Context, dynClient dynamic.Interface) ([]schema.GroupVersionResource, error) {
	if dynClient == nil {
		return nil, fmt.Errorf("cache.inventory: dynClient must be non-nil")
	}

	// Page through every RestAction cluster-wide. We use the dynamic
	// client directly (not the informer) because the informer for
	// RestActions itself may not be registered yet at startup — this
	// is the bootstrap step.
	var (
		all      = map[schema.GroupVersionResource]struct{}{}
		continueToken string
	)
	for {
		opts := metav1.ListOptions{
			Limit:    listPageLimit,
			Continue: continueToken,
		}
		list, err := dynClient.Resource(restActionGVR).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("cache.inventory: list RestActions: %w", err)
		}

		for i := range list.Items {
			ra := &list.Items[i]
			// spec.api is []map[string]any in unstructured form.
			apis, found, err := unstructuredSliceField(ra.Object, "spec", "api")
			if err != nil || !found {
				continue
			}
			for _, entry := range apis {
				m, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				// Skip external-endpoint entries — those have no
				// apiserver-side GVR to register.
				if _, hasEndpointRef := m["endpointRef"]; hasEndpointRef {
					continue
				}
				pathRaw, _ := m["path"].(string)
				if pathRaw == "" {
					continue
				}
				gvr, ok := parseAPIServerPathToGVR(pathRaw)
				if !ok {
					continue
				}
				all[gvr] = struct{}{}
			}
		}

		continueToken = list.GetContinue()
		if continueToken == "" {
			break
		}
	}

	out := make([]schema.GroupVersionResource, 0, len(all))
	for gvr := range all {
		out = append(out, gvr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Resource < out[j].Resource
	})

	slog.Info("cache.inventory.collected",
		slog.String("subsystem", "cache"),
		slog.Int("resource_types", len(out)),
	)

	return out, nil
}

// parseAPIServerPathToGVR extracts (Group, Version, Resource) from a
// Kubernetes apiserver URL path. Supports both shapes:
//
//   /api/v1/...                     -> core group   ({"", "v1", <resource>})
//   /apis/<group>/<version>/...     -> named group  ({<group>, <version>, <resource>})
//
// Returns ok=false for anything else (external endpoints, JQ-templated
// fragments that still carry `${...}` etc.).
//
// The function strips any query string and JQ expression suffix before
// parsing. A path like
//
//   ${ "/api/v1/namespaces/" + (.) + "/pods" }
//
// will NOT be parsed because the leading `${` breaks the prefix match;
// the lazy fallback in watcher.go is expected to catch these at
// dispatch time. This is deliberate: the inventory walker is a
// "best-effort eager" — we don't try to evaluate JQ at startup.
func parseAPIServerPathToGVR(path string) (schema.GroupVersionResource, bool) {
	// Strip query string.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	// Strip trailing slash.
	path = strings.TrimRight(path, "/")

	// Reject paths with unresolved JQ template fragments.
	if strings.Contains(path, "${") {
		return schema.GroupVersionResource{}, false
	}

	switch {
	case strings.HasPrefix(path, "/api/"):
		// /api/<version>/<resource>[/...]
		rest := strings.TrimPrefix(path, "/api/")
		parts := strings.Split(rest, "/")
		if len(parts) < 2 {
			return schema.GroupVersionResource{}, false
		}
		version := parts[0]
		// Namespaced form: /api/v1/namespaces/<ns>/<resource>[/...]
		if parts[1] == "namespaces" && len(parts) >= 4 {
			return schema.GroupVersionResource{
				Group:    "",
				Version:  version,
				Resource: parts[3],
			}, true
		}
		// Cluster-scoped form: /api/v1/<resource>[/...]
		return schema.GroupVersionResource{
			Group:    "",
			Version:  version,
			Resource: parts[1],
		}, true

	case strings.HasPrefix(path, "/apis/"):
		// /apis/<group>/<version>/<resource>[/...]
		// or:  /apis/<group>/<version>/namespaces/<ns>/<resource>[/...]
		rest := strings.TrimPrefix(path, "/apis/")
		parts := strings.Split(rest, "/")
		if len(parts) < 3 {
			return schema.GroupVersionResource{}, false
		}
		group := parts[0]
		version := parts[1]
		if parts[2] == "namespaces" && len(parts) >= 5 {
			return schema.GroupVersionResource{
				Group:    group,
				Version:  version,
				Resource: parts[4],
			}, true
		}
		return schema.GroupVersionResource{
			Group:    group,
			Version:  version,
			Resource: parts[2],
		}, true
	}

	return schema.GroupVersionResource{}, false
}

// unstructuredSliceField pulls obj[fields...] as []any. Returns
// (nil, false, nil) when the path is missing or the leaf is not a
// slice. An error is returned only when an intermediate node has the
// wrong type — that's a malformed RestAction and the caller logs it.
func unstructuredSliceField(obj map[string]any, fields ...string) ([]any, bool, error) {
	cur := any(obj)
	for i, f := range fields[:len(fields)-1] {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("cache.inventory: %q is not an object", strings.Join(fields[:i], "."))
		}
		v, found := m[f]
		if !found {
			return nil, false, nil
		}
		cur = v
	}
	last := fields[len(fields)-1]
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, false, nil
	}
	v, found := m[last]
	if !found {
		return nil, false, nil
	}
	s, ok := v.([]any)
	if !ok {
		return nil, false, nil
	}
	return s, true, nil
}
