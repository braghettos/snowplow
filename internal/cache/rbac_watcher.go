package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	rbaclisters "k8s.io/client-go/listers/rbac/v1"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
)

// rbacDebounceWindow is how long we wait to coalesce rapid RBAC change events
// before triggering a single cache invalidation. This prevents a storm of
// events (e.g. from thousands of leftover benchmark ClusterRoleBindings during
// the informer's initial LIST sync) from causing continuous L1 purges.
const rbacDebounceWindow = 2 * time.Second

type RBACWatcher struct {
	cache *RedisCache
	rc    *rest.Config

	mu      sync.Mutex
	pending bool // true when a debounced invalidation is scheduled
	timer   *time.Timer

	// Listers for binding identity computation. Set after Start().
	rbLister  rbaclisters.RoleBindingLister
	crbLister rbaclisters.ClusterRoleBindingLister
	synced    bool

	// identityCache maps username → binding identity hash (from ComputeBindingIdentity).
	// Avoids re-listing all bindings on every HTTP request. Cleared on broad
	// RBAC changes; individual entries deleted on per-user binding invalidation.
	identityCache sync.Map
}

func NewRBACWatcher(c *RedisCache, rc *rest.Config) *RBACWatcher {
	return &RBACWatcher{cache: c, rc: rc}
}

func (rw *RBACWatcher) Start(ctx context.Context) error {
	clientset, err := kubernetes.NewForConfig(rw.rc)
	if err != nil {
		return err
	}
	factory := informers.NewSharedInformerFactory(clientset, 0)

	// Only react to genuine mutations (Update/Delete). Skipping AddFunc avoids
	// the initial-LIST storm where every pre-existing RBAC object fires an ADD
	// event on pod startup — with thousands of resources this would flood the
	// cache with back-to-back invalidations for several minutes.
	rbacHandler := func(obj any) { rw.scheduleInvalidate(ctx) }
	bindingHandler := func(obj any) { rw.scheduleInvalidateFromBinding(ctx, obj) }

	_, _ = factory.Rbac().V1().Roles().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, n any) { rbacHandler(n) }, DeleteFunc: rbacHandler,
	})
	_, _ = factory.Rbac().V1().ClusterRoles().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, n any) { rbacHandler(n) }, DeleteFunc: rbacHandler,
	})
	_, _ = factory.Rbac().V1().RoleBindings().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, n any) { bindingHandler(n) }, DeleteFunc: bindingHandler,
	})
	_, _ = factory.Rbac().V1().ClusterRoleBindings().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, n any) { bindingHandler(n) }, DeleteFunc: bindingHandler,
	})
	// Store listers for binding identity computation.
	rw.rbLister = factory.Rbac().V1().RoleBindings().Lister()
	rw.crbLister = factory.Rbac().V1().ClusterRoleBindings().Lister()

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	rw.synced = true
	slog.Info("rbac-watcher: started informers")
	return nil
}

// scheduleInvalidate debounces RBAC invalidation: multiple events within
// rbacDebounceWindow collapse into a single invalidation call.
func (rw *RBACWatcher) scheduleInvalidate(ctx context.Context) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.pending {
		rw.timer.Reset(rbacDebounceWindow)
		return
	}
	rw.pending = true
	rw.timer = time.AfterFunc(rbacDebounceWindow, func() {
		rw.mu.Lock()
		rw.pending = false
		rw.mu.Unlock()
		rw.invalidate(ctx)
	})
}

// scheduleInvalidateFromBinding debounces binding-specific invalidation.
// When a binding change affects a specific user we still debounce to avoid
// rapid-fire updates, but we pass the object through for targeted invalidation.
func (rw *RBACWatcher) scheduleInvalidateFromBinding(ctx context.Context, obj any) {
	subjects, ok := extractSubjects(obj)
	if !ok || len(subjects) == 0 {
		rw.scheduleInvalidate(ctx)
		return
	}
	// For targeted (per-user) binding invalidations, still debounce but do it
	// immediately via a tiny window so user-specific changes aren't delayed too long.
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.pending {
		rw.timer.Reset(rbacDebounceWindow)
		return
	}
	rw.pending = true
	capturedObj := obj
	rw.timer = time.AfterFunc(rbacDebounceWindow, func() {
		rw.mu.Lock()
		rw.pending = false
		rw.mu.Unlock()
		rw.invalidateFromBinding(ctx, capturedObj)
	})
}

// invalidate uses the active-users set for targeted RBAC invalidation.
func (rw *RBACWatcher) invalidate(ctx context.Context) {
	// Clear all cached identities on broad RBAC change so the next
	// request re-computes the binding identity from the updated listers.
	rw.identityCache.Range(func(key, _ any) bool {
		rw.identityCache.Delete(key)
		return true
	})

	usernames, err := rw.cache.SMembers(ctx, ActiveUsersKey)
	if err != nil || len(usernames) == 0 {
		rw.invalidateAllRBAC(ctx)
		return
	}
	total := 0
	for _, username := range usernames {
		total += rw.purgeUserCacheData(ctx, username)
	}
	slog.Debug("rbac-watcher: invalidated RBAC+L1 for active users after role change",
		slog.Int("users", len(usernames)), slog.Int("keys", total))
}

func (rw *RBACWatcher) invalidateAllRBAC(ctx context.Context) {
	// With HASH-based RBAC, each user has one hash key: snowplow:rbac:{username}.
	// SCAN for those hashes and DEL them.
	keys, err := rw.cache.ScanKeys(ctx, "snowplow:rbac:*")
	if err != nil || len(keys) == 0 {
		return
	}
	_ = rw.cache.Delete(ctx, keys...)
	slog.Debug("rbac-watcher: invalidated all RBAC cache entries (fallback scan)",
		slog.Int("count", len(keys)))
}

func (rw *RBACWatcher) invalidateFromBinding(ctx context.Context, obj any) {
	subjects, ok := extractSubjects(obj)
	if !ok || len(subjects) == 0 {
		rw.invalidate(ctx)
		return
	}
	usernames := []string{}
	hasGroups := false
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			usernames = append(usernames, s.Name)
		case rbacv1.GroupKind, rbacv1.ServiceAccountKind:
			hasGroups = true
		}
	}
	if hasGroups {
		rw.invalidate(ctx)
		return
	}
	for _, username := range usernames {
		rw.purgeUserCacheData(ctx, username)
	}
}

// purgeUserCacheData deletes RBAC and L1 (resolved) cache entries for a
// single user. Handles both username-keyed and binding-identity-keyed entries:
//   - Purges RBAC hash and L1 resolved index for the username directly
//   - If the RBAC watcher can compute a binding identity for the user,
//     also purges the binding-identity-keyed RBAC hash and L1 resolved index
//
// Returns the total number of keys deleted.
func (rw *RBACWatcher) purgeUserCacheData(ctx context.Context, username string) int {
	total := 0

	// Load old identity from cache BEFORE clearing it, so we can purge
	// stale entries keyed under the previous binding identity.
	var oldBid string
	if v, ok := rw.identityCache.Load(username); ok {
		oldBid = v.(string)
	}
	rw.identityCache.Delete(username)

	// Purge username-keyed entries (backward compat / migration).
	_ = rw.cache.DeleteUserRBAC(ctx, username)
	total++

	idxKey := UserResolvedIndexKey(username)
	keys, _ := rw.cache.SMembers(ctx, idxKey)
	if len(keys) == 0 {
		keys, _ = rw.cache.ScanKeys(ctx, "snowplow:resolved:"+username+":*")
	}
	if len(keys) > 0 {
		_ = rw.cache.Delete(ctx, append(keys, idxKey)...)
		total += len(keys)
	}

	// Purge old binding identity entries (cached before the RBAC change).
	// The old identity may differ from the new one after a binding update.
	if oldBid != "" && oldBid != username {
		_ = rw.cache.DeleteUserRBAC(ctx, oldBid)
		total++
		oldIdx := UserResolvedIndexKey(oldBid)
		oldKeys, _ := rw.cache.SMembers(ctx, oldIdx)
		if len(oldKeys) == 0 {
			oldKeys, _ = rw.cache.ScanKeys(ctx, "snowplow:resolved:"+oldBid+":*")
		}
		if len(oldKeys) > 0 {
			_ = rw.cache.Delete(ctx, append(oldKeys, oldIdx)...)
			total += len(oldKeys)
		}
	}

	// Also purge NEW binding-identity-keyed entries. After a binding change,
	// the informer has the NEW state, so ComputeBindingIdentity returns
	// the new identity. We purge it because the RBAC decisions cached
	// under the new identity may have been stale (computed before the
	// binding change propagated).
	if newBid := rw.ComputeBindingIdentity(username, nil); newBid != "" && newBid != username && newBid != oldBid {
		_ = rw.cache.DeleteUserRBAC(ctx, newBid)
		total++
		bidIdx := UserResolvedIndexKey(newBid)
		bidKeys, _ := rw.cache.SMembers(ctx, bidIdx)
		if len(bidKeys) == 0 {
			bidKeys, _ = rw.cache.ScanKeys(ctx, "snowplow:resolved:"+newBid+":*")
		}
		if len(bidKeys) > 0 {
			_ = rw.cache.Delete(ctx, append(bidKeys, bidIdx)...)
			total += len(bidKeys)
		}
	}

	return total
}

// CachedBindingIdentity returns a cached binding identity for the user. On
// cache hit it skips the full lister scan. On miss it delegates to
// ComputeBindingIdentity and stores the result. Empty identities (informer
// not synced) are not cached.
func (rw *RBACWatcher) CachedBindingIdentity(username string, groups []string) string {
	if v, ok := rw.identityCache.Load(username); ok {
		return v.(string)
	}
	bid := rw.ComputeBindingIdentity(username, groups)
	if bid != "" {
		rw.identityCache.Store(username, bid)
	}
	return bid
}

// ComputeBindingIdentity returns a short hash identifying the effective RBAC
// permissions for a user. Users with the same set of RoleBindings and
// ClusterRoleBindings produce the same identity, enabling shared L1 cache
// entries across users with identical permissions.
//
// The identity is the first 12 hex chars of SHA-256(sorted binding names).
// Returns "" if the informers have not synced yet.
func (rw *RBACWatcher) ComputeBindingIdentity(username string, groups []string) string {
	if !rw.synced {
		return ""
	}

	subjects := make(map[string]bool, 1+len(groups))
	subjects["User:"+username] = true
	for _, g := range groups {
		subjects["Group:"+g] = true
	}

	var bindingNames []string

	// ClusterRoleBindings (cluster-scoped).
	if crbs, err := rw.crbLister.List(labels.Everything()); err == nil {
		for _, crb := range crbs {
			if matchesSubjects(crb.Subjects, subjects) {
				bindingNames = append(bindingNames, "crb:"+crb.Name)
			}
		}
	}

	// RoleBindings (namespace-scoped).
	if rbs, err := rw.rbLister.List(labels.Everything()); err == nil {
		for _, rb := range rbs {
			if matchesSubjects(rb.Subjects, subjects) {
				bindingNames = append(bindingNames, "rb:"+rb.Namespace+"/"+rb.Name)
			}
		}
	}

	if len(bindingNames) == 0 {
		return "no-bindings"
	}

	sort.Strings(bindingNames)
	h := sha256.New()
	for _, name := range bindingNames {
		h.Write([]byte(name))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// matchesSubjects returns true if any subject in the binding matches the
// user's identity (username or group membership).
func matchesSubjects(bindingSubjects []rbacv1.Subject, userSubjects map[string]bool) bool {
	for _, s := range bindingSubjects {
		switch s.Kind {
		case rbacv1.UserKind:
			if userSubjects["User:"+s.Name] {
				return true
			}
		case rbacv1.GroupKind:
			if userSubjects["Group:"+s.Name] {
				return true
			}
		}
	}
	return false
}

func extractSubjects(obj any) ([]rbacv1.Subject, bool) {
	switch b := obj.(type) {
	case *rbacv1.RoleBinding:
		return b.Subjects, true
	case *rbacv1.ClusterRoleBinding:
		return b.Subjects, true
	case k8scache.DeletedFinalStateUnknown:
		return extractSubjects(b.Obj)
	}
	return nil, false
}
