package cache

import (
	"context"
	"log/slog"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
)

type RBACWatcher struct {
	cache *RedisCache
	rc    *rest.Config
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

	rbacHandler := func(obj any) { rw.invalidate(ctx, obj) }
	bindingHandler := func(obj any) { rw.invalidateFromBinding(ctx, obj) }

	_, _ = factory.Rbac().V1().Roles().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: rbacHandler, UpdateFunc: func(_, n any) { rbacHandler(n) }, DeleteFunc: rbacHandler,
	})
	_, _ = factory.Rbac().V1().ClusterRoles().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: rbacHandler, UpdateFunc: func(_, n any) { rbacHandler(n) }, DeleteFunc: rbacHandler,
	})
	_, _ = factory.Rbac().V1().RoleBindings().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: bindingHandler, UpdateFunc: func(_, n any) { bindingHandler(n) }, DeleteFunc: bindingHandler,
	})
	_, _ = factory.Rbac().V1().ClusterRoleBindings().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: bindingHandler, UpdateFunc: func(_, n any) { bindingHandler(n) }, DeleteFunc: bindingHandler,
	})
	factory.Start(ctx.Done())
	slog.Info("rbac-watcher: started informers")
	return nil
}

// invalidate uses the active-users set for targeted RBAC invalidation.
func (rw *RBACWatcher) invalidate(ctx context.Context, _ any) {
	usernames, err := rw.cache.SMembers(ctx, ActiveUsersKey)
	if err != nil || len(usernames) == 0 {
		rw.invalidateAllRBAC(ctx)
		return
	}
	total := 0
	for _, username := range usernames {
		total += rw.purgeUserCacheData(ctx, username)
	}
	slog.Debug("rbac-watcher: invalidated RBAC+L1+L2 for active users after role change",
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
		rw.invalidate(ctx, obj)
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
		rw.invalidate(ctx, obj)
		return
	}
	for _, username := range usernames {
		rw.purgeUserCacheData(ctx, username)
	}
}

// purgeUserCacheData deletes RBAC, L1 (resolved), and L2 (http) cache entries
// for a single user. Returns the total number of keys deleted.
func (rw *RBACWatcher) purgeUserCacheData(ctx context.Context, username string) int {
	total := 0
	patterns := []string{
		RBACKeyPattern(username),
		"snowplow:resolved:" + username + ":*",
		"snowplow:http:" + username + ":*",
	}
	for _, p := range patterns {
		keys, _ := rw.cache.ScanKeys(ctx, p)
		if len(keys) > 0 {
			_ = rw.cache.Delete(ctx, keys...)
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
