package cache

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
)

// UserReadyFunc is called when a user's -clientconfig secret is created or
// updated. The watcher calls this in a background goroutine so it must be
// safe for concurrent use. The username is the secret name minus the
// "-clientconfig" suffix.
type UserReadyFunc func(ctx context.Context, username string)

// UserSecretWatcher watches *-clientconfig Secrets in AUTHN_NAMESPACE.
// Tracks active users and purges per-user RBAC cache on re-auth or deletion.
type UserSecretWatcher struct {
	cache   *RedisCache
	rc      *rest.Config
	authnNS string

	// onUserReady is called in a background goroutine when a user's
	// -clientconfig secret is created or updated. Used for RBAC pre-warming.
	onUserReady UserReadyFunc

	// warmingUsers tracks which users have an in-flight RBAC pre-warm
	// goroutine to prevent duplicate concurrent warms for the same user.
	warmingUsers sync.Map // username -> struct{}
}

func NewUserSecretWatcher(c *RedisCache, rc *rest.Config, authnNS string) *UserSecretWatcher {
	return &UserSecretWatcher{cache: c, rc: rc, authnNS: authnNS}
}

// SetOnUserReady registers a callback invoked (in a background goroutine)
// when a user's -clientconfig secret is created or updated. Typically wired
// to RBAC pre-warming.
func (uw *UserSecretWatcher) SetOnUserReady(fn UserReadyFunc) {
	uw.onUserReady = fn
}

func (uw *UserSecretWatcher) Start(ctx context.Context) error {
	clientset, err := kubernetes.NewForConfig(uw.rc)
	if err != nil {
		return err
	}
	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset, 0,
		informers.WithNamespace(uw.authnNS),
	)
	_, _ = factory.Core().V1().Secrets().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if s, ok := toSecret(obj); ok {
				uw.onAdd(ctx, s)
			}
		},
		UpdateFunc: func(_, obj any) {
			if s, ok := toSecret(obj); ok {
				uw.onUpdate(ctx, s)
			}
		},
		DeleteFunc: func(obj any) {
			if s, ok := toSecret(obj); ok {
				uw.onDelete(ctx, s)
			}
		},
	})
	factory.Start(ctx.Done())

	// Wait for the initial LIST to complete so that the active-users set
	// is populated before any L1 refresh runs. Without this, the first
	// refresh after pod start finds an empty active-users set and either
	// skips all users (bug) or falls through to refresh everyone (wasteful).
	synced := factory.WaitForCacheSync(ctx.Done())
	for resource, ok := range synced {
		if !ok {
			slog.Warn("user-watcher: informer sync failed", slog.String("resource", resource.String()))
		}
	}
	slog.Info("user-watcher: started and synced informer", slog.String("namespace", uw.authnNS))
	return nil
}

func (uw *UserSecretWatcher) onAdd(ctx context.Context, secret *corev1.Secret) {
	if !isClientConfig(secret) {
		return
	}
	username := usernameFromSecret(secret)
	_ = uw.cache.SAddUser(ctx, username)
	uw.triggerUserReady(ctx, username)
}

func (uw *UserSecretWatcher) onUpdate(ctx context.Context, secret *corev1.Secret) {
	if !isClientConfig(secret) {
		return
	}
	username := usernameFromSecret(secret)
	slog.Info("user-watcher: user secret updated, invalidating cached config",
		slog.String("username", username))
	_ = uw.cache.SAddUser(ctx, username)
	_ = uw.cache.Delete(ctx, UserConfigKey(username))
	uw.triggerUserReady(ctx, username)
}

// triggerUserReady fires the onUserReady callback in a background goroutine.
// Uses a per-user tryLock (sync.Map) to skip if a pre-warm is already running
// for this user -- avoids unbounded goroutine creation from rapid secret updates.
func (uw *UserSecretWatcher) triggerUserReady(ctx context.Context, username string) {
	if uw.onUserReady == nil {
		return
	}
	// Per-user tryLock: if another goroutine is already warming this user, skip.
	if _, loaded := uw.warmingUsers.LoadOrStore(username, struct{}{}); loaded {
		slog.Debug("user-watcher: RBAC pre-warm already running, skipping",
			slog.String("username", username))
		return
	}
	fn := uw.onUserReady
	go func() {
		defer uw.warmingUsers.Delete(username)
		fn(ctx, username)
	}()
}

func (uw *UserSecretWatcher) onDelete(ctx context.Context, secret *corev1.Secret) {
	if !isClientConfig(secret) {
		return
	}
	username := usernameFromSecret(secret)
	slog.Info("user-watcher: user secret deleted, purging caches",
		slog.String("username", username))
	_ = uw.cache.SRemUser(ctx, username)
	_ = uw.cache.Delete(ctx, UserConfigKey(username))
	uw.purgeRBACKeys(ctx, username)
}

func (uw *UserSecretWatcher) purgeRBACKeys(ctx context.Context, username string) {
	keys, err := uw.cache.ScanKeys(ctx, RBACKeyPattern(username))
	if err != nil || len(keys) == 0 {
		return
	}
	_ = uw.cache.Delete(ctx, keys...)
}

func isClientConfig(secret *corev1.Secret) bool {
	return strings.HasSuffix(secret.Name, "-clientconfig")
}

func usernameFromSecret(secret *corev1.Secret) string {
	return strings.TrimSuffix(secret.Name, "-clientconfig")
}

func toSecret(obj any) (*corev1.Secret, bool) {
	switch v := obj.(type) {
	case *corev1.Secret:
		return v, true
	case k8scache.DeletedFinalStateUnknown:
		if s, ok := v.Obj.(*corev1.Secret); ok {
			return s, true
		}
	}
	return nil, false
}
