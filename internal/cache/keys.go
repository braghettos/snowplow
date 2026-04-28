package cache

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	WatchedGVRsKey   = "snowplow:watched-gvrs"
	ActiveUsersKey   = "snowplow:active-users"
	L1ReadyKey       = "snowplow:l1:ready"
	notFoundSentinel = `{"__snowplow_not_found__":true}`
)

// CacheIdentity returns the binding identity from context if available,
// falling back to the username. This is the cache key component that
// enables sharing L1 entries across users with identical RBAC bindings.
func CacheIdentity(ctx context.Context, username string) string {
	if bid := BindingIdentityFromContext(ctx); bid != "" {
		return bid
	}
	return username
}

// MarkL1Ready writes a Unix-epoch timestamp to the L1 ready sentinel key.
// External consumers (e.g. e2e tests) can poll this key to deterministically
// know when the most recent L1 warmup or refresh cycle completed.
// The key expires after 5 minutes so a stale timestamp from a crashed pod
// does not mislead readiness probes (Bug 13).
func MarkL1Ready(ctx context.Context, c Cache) {
	if c == nil {
		return
	}
	_ = c.SetStringWithTTL(ctx, L1ReadyKey, strconv.FormatInt(time.Now().Unix(), 10), 5*time.Minute)
}

// L1ReadyTimestamp returns the Unix epoch stored in the L1 ready key, or 0.
func L1ReadyTimestamp(ctx context.Context, c Cache) int64 {
	if c == nil {
		return 0
	}
	raw, _, err := c.GetRaw(ctx, L1ReadyKey)
	if err != nil || len(raw) == 0 {
		return 0
	}
	ts, _ := strconv.ParseInt(string(raw), 10, 64)
	return ts
}

func GVRToKey(gvr schema.GroupVersionResource) string {
	g := gvr.Group
	if g == "" {
		g = "core"
	}
	return g + "/" + gvr.Version + "/" + gvr.Resource
}

func ParseGVRKey(s string) schema.GroupVersionResource {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 {
		return schema.GroupVersionResource{}
	}
	g := parts[0]
	if g == "core" {
		g = ""
	}
	return schema.GroupVersionResource{Group: g, Version: parts[1], Resource: parts[2]}
}

// GetKey builds the shared (user-agnostic) cache key for a single-resource GET.
func GetKey(gvr schema.GroupVersionResource, namespace, name string) string {
	return fmt.Sprintf("snowplow:get:%s:%s:%s", GVRToKey(gvr), namespace, name)
}

// ListKey builds the shared cache key for a LIST operation (monolithic blob, legacy).
func ListKey(gvr schema.GroupVersionResource, namespace string) string {
	return fmt.Sprintf("snowplow:list:%s:%s", GVRToKey(gvr), namespace)
}

// ListIndexKey builds the Redis SET key that indexes all item names for a
// GVR+namespace combination. Each member is the resource name (not the full
// GET key) so the index stays compact and we can reconstruct GET keys cheaply.
func ListIndexKey(gvr schema.GroupVersionResource, namespace string) string {
	return fmt.Sprintf("snowplow:list-idx:%s:%s", GVRToKey(gvr), namespace)
}

// ListIndexKeyPattern returns a glob pattern matching all list index keys for a GVR.
func ListIndexKeyPattern(gvr schema.GroupVersionResource) string {
	return fmt.Sprintf("snowplow:list-idx:%s:*", GVRToKey(gvr))
}

// DiscoveryKey builds the shared cache key for an API discovery result.
func DiscoveryKey(category string) string {
	return fmt.Sprintf("snowplow:discovery:%s", category)
}

// RBACHashKey returns the Redis HASH key for a user's RBAC decisions.
// All RBAC results for a user are stored as fields in a single hash,
// reducing per-key overhead from ~70 bytes to near zero per entry.
// Invalidation is a single DEL instead of SCAN + bulk DEL.
func RBACHashKey(username string) string {
	return "snowplow:rbac:" + username
}

// RBACField builds the HASH field for a specific RBAC decision.
// Format: "{verb}:{group/resource}:{namespace}"
func RBACField(verb string, gr schema.GroupResource, namespace string) string {
	g := gr.Group
	if g == "" {
		g = "core"
	}
	return fmt.Sprintf("%s:%s/%s:%s", verb, g, gr.Resource, namespace)
}

// UserResolvedIndexKey returns the Redis SET key that tracks all resolved (L1)
// cache keys for a given user. Used for O(1) invalidation instead of SCAN.
func UserResolvedIndexKey(username string) string {
	return "snowplow:resolved-idx:" + username
}

// ResolvedKey builds the per-user cache key for a fully-resolved dispatcher
// output (widget or RESTAction). Caching at this level eliminates both the
// HTTP fan-out AND all JQ evaluations for repeated requests.
//
// Pagination is included in the key so paginated requests get isolated entries.
// Pass page=0 and perPage=0 for unpaginated resources (the common case).
func ResolvedKey(username string, gvr schema.GroupVersionResource, namespace, name string, page, perPage int) string {
	base := fmt.Sprintf("snowplow:resolved:%s:%s:%s:%s", username, GVRToKey(gvr), namespace, name)
	if page > 0 || perPage > 0 {
		return fmt.Sprintf("%s:p%d-pp%d", base, page, perPage)
	}
	return base
}

// APIResultKey builds the L1 cache key for a K8s API result within a
// RESTAction resolve pipeline. Cached per-user so RBAC is respected.
// LIST: name="" → snowplow:api-result:{user}:{gvr}:{ns}
// GET:  name!="" → snowplow:api-result:{user}:{gvr}:{ns}:{name}
func APIResultKey(username string, gvr schema.GroupVersionResource, namespace, name string) string {
	base := fmt.Sprintf("snowplow:api-result:%s:%s:%s", username, GVRToKey(gvr), namespace)
	if name != "" {
		return base + ":" + name
	}
	return base
}

// IsAPIResultKey returns true if the key is an API result cache key.
func IsAPIResultKey(key string) bool {
	return strings.HasPrefix(key, "snowplow:api-result:")
}

// ResolvedKeyBase returns the base key without pagination suffix.
// Used to group paginated variants for sequential resolution.
func ResolvedKeyBase(username string, gvr schema.GroupVersionResource, namespace, name string) string {
	return fmt.Sprintf("snowplow:resolved:%s:%s:%s:%s", username, GVRToKey(gvr), namespace, name)
}

// L1ResourceDepKey returns the Redis SET key for a per-resource L1 dependency.
// It maps a specific K8s resource (GVR + namespace + name) to the L1 resolved
// keys that accessed it during resolution.
//
// Three forms encode the access pattern:
//
//	L1ResourceDepKey(gvr, "ns-01", "my-pod")  → GET dependency on specific resource
//	L1ResourceDepKey(gvr, "ns-01", "")         → LIST dependency within a namespace
//	L1ResourceDepKey(gvr, "", "")              → LIST dependency cluster-wide
func L1ResourceDepKey(gvrKey, ns, name string) string {
	return "snowplow:l1dep:" + gvrKey + ":" + ns + ":" + name
}


// RegisterL1Dependencies registers the L1 resolved key in reverse indexes
// based on dependencies captured by the tracker. Writes:
// - Per-resource deps: L1ResourceDepKey(gvr, ns, name) for each specific resource
// - Cluster-wide deps: L1ResourceDepKey(gvr, "", "") for each GVR accessed
//
// The cluster-wide dep ensures that when ANY resource of a GVR changes in
// ANY namespace, the L1 key is found by triggerL1RefreshBatch. This is critical
// for RESTActions like compositions-list that iterate all namespaces.
func RegisterL1Dependencies(ctx context.Context, c Cache, tracker *DependencyTracker, l1Key string) {
	if c == nil || tracker == nil {
		return
	}
	gvrKeys := tracker.GVRKeys()
	refs := tracker.ResourceRefs()
	if len(gvrKeys) == 0 && len(refs) == 0 {
		return
	}

	seen := make(map[string]bool)

	// Also register the unpaginated base key so that events trigger
	// refresh of both paginated and unpaginated variants. HTTP requests
	// without pagination read the base key; without this registration,
	// it stays stale while paginated keys converge.
	info, isPaginated := ParseResolvedKey(l1Key)
	var baseKey string
	if isPaginated && (info.Page > 0 || info.PerPage > 0) {
		baseKey = ResolvedKeyBase(info.Username, info.GVR, info.NS, info.Name)
	}

	// Per-resource deps (ns + name specific).
	for _, ref := range refs {
		key := L1ResourceDepKey(ref.GVRKey, ref.NS, ref.Name)
		if !seen[key] {
			seen[key] = true
			_ = c.SAddWithTTL(ctx, key, l1Key, ReverseIndexTTL)
			if baseKey != "" {
				_ = c.SAddWithTTL(ctx, key, baseKey, ReverseIndexTTL)
			}
		}
	}

	// Cluster-wide deps: only for GVRs that were LISTed (name="").
	clusterRegistered := 0
	for _, ref := range refs {
		if ref.Name == "" {
			key := L1ResourceDepKey(ref.GVRKey, "", "")
			if !seen[key] {
				seen[key] = true
				_ = c.SAddWithTTL(ctx, key, l1Key, ReverseIndexTTL)
				clusterRegistered++
				if baseKey != "" {
					_ = c.SAddWithTTL(ctx, key, baseKey, ReverseIndexTTL)
				}
			}
		}
	}
	if clusterRegistered > 0 {
		slog.Info("RegisterL1Dependencies: cluster-wide deps registered",
			slog.String("l1Key", l1Key),
			slog.Int("clusterDeps", clusterRegistered),
			slog.Int("perResourceDeps", len(seen)-clusterRegistered))
	}
}


// ExtractAPIGVR extracts the K8s API group, version, and resource from a request path.
// Supports both namespaced and cluster-scoped paths:
//
//	/apis/<group>/<version>/namespaces/<ns>/<resource>[/<name>]
//	/apis/<group>/<version>/<resource>[/<name>]
//	/api/<version>/namespaces/<ns>/<resource>[/<name>]
//	/api/<version>/<resource>[/<name>]
//
// Returns ("", "", "") if the path is not a K8s API path.
func ExtractAPIGVR(path string) (group, version, resource string) {
	// Strip query string
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	path = strings.TrimPrefix(path, "/")
	segs := strings.Split(path, "/")

	switch {
	case len(segs) >= 4 && segs[0] == "apis":
		// /apis/<group>/<version>/...
		group, version = segs[1], segs[2]
		if len(segs) >= 6 && segs[3] == "namespaces" {
			resource = segs[5] // /apis/g/v/namespaces/ns/<resource>
		} else {
			resource = segs[3] // /apis/g/v/<resource>
		}
	case len(segs) >= 3 && segs[0] == "api":
		// /api/<version>/...
		group, version = "core", segs[1]
		if len(segs) >= 5 && segs[2] == "namespaces" {
			resource = segs[4] // /api/v1/namespaces/ns/<resource>
		} else {
			resource = segs[2] // /api/v1/<resource>
		}
	}
	return
}

// UserConfigKey builds the per-user cache key for the Endpoint fetched from
// the user's -clientconfig Secret. Invalidated by UserSecretWatcher when the
// Secret changes.
func UserConfigKey(username string) string {
	return "snowplow:userconfig:" + username
}

// IsNotFoundRaw returns true if raw is the negative-cache sentinel.
func IsNotFoundRaw(raw []byte) bool {
	return string(raw) == notFoundSentinel
}

// ParseGetKey parses a snowplow:get cache key into its components.
func ParseGetKey(key string) (gvr schema.GroupVersionResource, namespace, name string, ok bool) {
	parts := strings.SplitN(key, ":", 5)
	if len(parts) != 5 || parts[0] != "snowplow" || parts[1] != "get" {
		return
	}
	return ParseGVRKey(parts[2]), parts[3], parts[4], true
}

// ParseListKey parses a snowplow:list cache key into its components.
func ParseListKey(key string) (gvr schema.GroupVersionResource, namespace string, ok bool) {
	parts := strings.SplitN(key, ":", 4)
	if len(parts) != 4 || parts[0] != "snowplow" || parts[1] != "list" {
		return
	}
	return ParseGVRKey(parts[2]), parts[3], true
}

// ParseListIndexKey parses a snowplow:list-idx cache key into its components.
func ParseListIndexKey(key string) (gvr schema.GroupVersionResource, namespace string, ok bool) {
	parts := strings.SplitN(key, ":", 4)
	if len(parts) != 4 || parts[0] != "snowplow" || parts[1] != "list-idx" {
		return
	}
	return ParseGVRKey(parts[2]), parts[3], true
}

// ParseCallPath extracts GVR, namespace, and name from a snowplow /call URL
// (e.g. /call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=foo&namespace=bar).
func ParseCallPath(rawPath string) (gvr schema.GroupVersionResource, namespace, name string) {
	idx := strings.Index(rawPath, "?")
	if idx < 0 {
		return
	}
	prefix := rawPath[:idx]
	if prefix != "/call" && prefix != "call" {
		return
	}
	q, err := url.ParseQuery(rawPath[idx+1:])
	if err != nil {
		return
	}
	apiVersion := q.Get("apiVersion")
	resource := q.Get("resource")
	if apiVersion == "" || resource == "" {
		return
	}
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 2 {
		gvr.Group = parts[0]
		gvr.Version = parts[1]
	} else {
		gvr.Version = parts[0]
	}
	gvr.Resource = resource
	namespace = q.Get("namespace")
	name = q.Get("name")
	return
}

// ResolvedKeyInfo holds the parsed components of a resolved (L1) cache key.
type ResolvedKeyInfo struct {
	Username string
	GVR      schema.GroupVersionResource
	NS       string
	Name     string
	Page     int
	PerPage  int
}

// ParseResolvedKey parses an L1 resolved cache key into its components.
// Key format: snowplow:resolved:{user}:{group/version/resource}:{ns}:{name}[:p{page}-pp{perPage}]
func ParseResolvedKey(key string) (ResolvedKeyInfo, bool) {
	parts := strings.SplitN(key, ":", 7)
	if len(parts) < 6 || parts[0] != "snowplow" || parts[1] != "resolved" {
		return ResolvedKeyInfo{}, false
	}
	info := ResolvedKeyInfo{
		Username: parts[2],
		GVR:      ParseGVRKey(parts[3]),
		NS:       parts[4],
		Name:     parts[5],
		Page:     0,
		PerPage:  0,
	}
	if len(parts) == 7 {
		fmt.Sscanf(parts[6], "p%d-pp%d", &info.Page, &info.PerPage)
	}
	if info.GVR.Resource == "" || info.Name == "" {
		return ResolvedKeyInfo{}, false
	}
	return info, true
}

// ParseK8sAPIPath parses a Kubernetes API server path into GVR, namespace, name.
func ParseK8sAPIPath(path string) (gvr schema.GroupVersionResource, namespace, name string) {
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	path = strings.TrimPrefix(path, "/")
	segs := strings.Split(path, "/")

	switch {
	case len(segs) >= 2 && segs[0] == "api":
		gvr.Version = segs[1]
		if len(segs) >= 5 && segs[2] == "namespaces" {
			namespace = segs[3]
			gvr.Resource = segs[4]
			if len(segs) >= 6 {
				name = segs[5]
			}
		} else if len(segs) >= 3 {
			gvr.Resource = segs[2]
			if len(segs) >= 4 {
				name = segs[3]
			}
		}
	case len(segs) >= 3 && segs[0] == "apis":
		gvr.Group = segs[1]
		gvr.Version = segs[2]
		if len(segs) >= 6 && segs[3] == "namespaces" {
			namespace = segs[4]
			gvr.Resource = segs[5]
			if len(segs) >= 7 {
				name = segs[6]
			}
		} else if len(segs) >= 4 {
			gvr.Resource = segs[3]
			if len(segs) >= 5 {
				name = segs[4]
			}
		}
	}
	return
}

