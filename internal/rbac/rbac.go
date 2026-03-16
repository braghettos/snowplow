package rbac

import (
	"context"
	"log/slog"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	"github.com/krateoplatformops/snowplow/internal/cache"
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

const rbacCacheTTL = 24 * time.Hour

// resolveUsername extracts the best available username: JWT > ep.Username.
func resolveUsername(ctx context.Context) string {
	if ui, err := xcontext.UserInfo(ctx); err == nil && ui.Username != "" {
		return ui.Username
	}
	if ep, err := xcontext.UserConfig(ctx); err == nil && ep.Username != "" {
		return ep.Username
	}
	return ""
}

type UserCanOptions struct {
	Verb          string
	GroupResource schema.GroupResource
	Namespace     string
}

func UserCan(ctx context.Context, opts UserCanOptions) (ok bool) {
	log := xcontext.Logger(ctx)

	ep, err := xcontext.UserConfig(ctx)
	if err != nil {
		log.Error("unable to get user endpoint", slog.Any("err", err))
		return false
	}

	// Attempt cache lookup.
	c := cache.FromContext(ctx)
	cacheKey := ""
	username := resolveUsername(ctx)
	if c != nil && username != "" {
		cacheKey = cache.RBACKey(username, opts.Verb, opts.GroupResource, opts.Namespace)
		var allowed bool
		if hit, rerr := c.Get(ctx, cacheKey, &allowed); hit && rerr == nil {
			cache.GlobalMetrics.RBACHits.Add(1)
			log.Debug("RBAC cache hit", slog.String("key", cacheKey))
			return allowed
		}
		cache.GlobalMetrics.RBACMisses.Add(1)
	}

	rc, err := kubeconfig.NewClientConfig(ctx, ep)
	if err != nil {
		log.Error("unable to create user client config", slog.Any("err", err))
		return false
	}

	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		log.Error("unable to create kubernetes clientset", slog.Any("err", err))
		return false
	}

	selfCheck := authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Group:     opts.GroupResource.Group,
				Resource:  opts.GroupResource.Resource,
				Namespace: opts.Namespace,
				Verb:      opts.Verb,
			},
		},
	}

	resp, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().
		Create(ctx, &selfCheck, metav1.CreateOptions{})
	if err != nil {
		log.Error("unable to perform SelfSubjectAccessReviews",
			slog.Any("selfCheck", selfCheck), slog.Any("err", err))
		return false
	}

	log.Debug("SelfSubjectAccessReviews result", slog.Any("response", resp))

	if c != nil && cacheKey != "" {
		_ = c.SetWithTTL(ctx, cacheKey, resp.Status.Allowed, rbacCacheTTL)
	}

	return resp.Status.Allowed
}
