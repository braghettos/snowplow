package cache

import (
	"context"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
)

// UserSecretWatcher watches *-clientconfig Secrets in AUTHN_NAMESPACE.
// Tracks active users and purges per-user RBAC cache on re-auth or deletion.
type UserSecretWatcher struct {
	cache   *RedisCache
	rc      *rest.Config
	authnNS string
}

func NewUserSecretWatcher(c *RedisCache, rc *rest.Config, authnNS string) *UserSecretWatcher {
	return &UserSecretWatcher{cache: c, rc: rc, authnNS: authnNS}
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
	slog.Info("user-watcher: started informer", slog.String("namespace", uw.authnNS))
	return nil
}

func (uw *UserSecretWatcher) onAdd(ctx context.Context, secret *corev1.Secret) {
	if !isClientConfig(secret) {
		return
	}
	username := usernameFromSecret(secret)
	_ = uw.cache.SAddUser(ctx, username)
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
