package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
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
	pending bool     // true when a debounced invalidation is scheduled
	timer   *time.Timer
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
	factory.Start(ctx.Done())
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
// single user. Uses per-user index SETs for O(1) lookup instead of SCAN.
// Falls back to SCAN if the index is empty (e.g., keys created before
// index tracking was added). Returns the total number of keys deleted.
func (rw *RBACWatcher) purgeUserCacheData(ctx context.Context, username string) int {
	total := 0
	for _, idx := range []struct {
		indexKey string
		pattern  string // fallback SCAN pattern
	}{
		{UserRBACIndexKey(username), RBACKeyPattern(username)},
		{UserResolvedIndexKey(username), "snowplow:resolved:" + username + ":*"},
	} {
		keys, _ := rw.cache.SMembers(ctx, idx.indexKey)
		if len(keys) == 0 {
			// Fallback to SCAN for keys created before index tracking.
			keys, _ = rw.cache.ScanKeys(ctx, idx.pattern)
		}
		if len(keys) > 0 {
			_ = rw.cache.Delete(ctx, append(keys, idx.indexKey)...)
			total += len(keys)
		}
	}
	return total
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
