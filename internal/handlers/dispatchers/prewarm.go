package dispatchers

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	preWarmConcurrency      = 10
	preWarmTimeout          = 30 * time.Second
	widgetGroup             = "widgets.templates.krateo.io"
	clientConfigSecretSuffix = "-clientconfig"
)

// ---------------------------------------------------------------------------
// Request-time child pre-warming
// ---------------------------------------------------------------------------

// preWarmChildWidgets resolves child widget references discovered during parent
// widget resolution and stores them in L1 cache. This eliminates the cold-start
// fan-out where the frontend issues N individual requests (e.g. 42 Routes after
// a RoutesLoader) that would each be an L1 miss.
func preWarmChildWidgets(parentCtx context.Context, c *cache.RedisCache, resolved *unstructured.Unstructured, authnNS string) {
	items, found, err := unstructured.NestedSlice(resolved.Object, "status", "resourcesRefs", "items")
	if err != nil || !found || len(items) == 0 {
		return
	}

	user, uerr := xcontext.UserInfo(parentCtx)
	if uerr != nil {
		return
	}
	ep, eerr := xcontext.UserConfig(parentCtx)
	if eerr != nil {
		return
	}
	accessToken, _ := xcontext.AccessToken(parentCtx)

	refs := extractChildWidgetRefs(parentCtx, c, items, user.Username)
	if len(refs) == 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), preWarmTimeout)
		defer cancel()
		resolveL1RefsForUser(ctx, user, ep, accessToken, c, refs, authnNS)
		slog.Default().Info("L1 pre-warm completed",
			slog.String("user", user.Username),
			slog.Int("candidates", len(refs)),
		)
	}()
}

// ---------------------------------------------------------------------------
// Startup L1 warmup for all users
// ---------------------------------------------------------------------------

// WarmL1ForAllUsers discovers every user from their -clientconfig Secrets, then
// resolves every instance of every widget GVR for each user, populating the
// per-user L1 cache. Only widgets are warmed here because they resolve locally
// (read from L3 cache, no HTTP callbacks). RESTActions are excluded: they
// involve deep API fan-out (compositions-list alone triggers 200+ K8s calls)
// and internal HTTP callbacks to snowplow, making startup warmup too slow.
// RESTActions will be fast on first user request thanks to L3+L2 being warm.
func WarmL1ForAllUsers(ctx context.Context, c *cache.RedisCache, rc *rest.Config, authnNS string, widgetGVRs []schema.GroupVersionResource) {
	log := slog.Default()
	if len(widgetGVRs) == 0 || authnNS == "" {
		log.Info("L1 warmup: skipped (no widget GVRs or authn namespace)")
		return
	}

	users, err := discoverUsers(ctx, rc, authnNS)
	if err != nil {
		log.Warn("L1 warmup: failed to discover users", slog.Any("err", err))
		return
	}
	if len(users) == 0 {
		log.Info("L1 warmup: no users found")
		return
	}

	log.Info("L1 warmup: starting",
		slog.Int("users", len(users)),
		slog.Int("widgetGVRs", len(widgetGVRs)),
	)

	var totalWarmed int64
	for _, u := range users {
		warmed := warmL1ForUser(ctx, c, u.userInfo, u.endpoint, "", widgetGVRs, authnNS)
		totalWarmed += warmed
	}

	log.Info("L1 warmup: completed",
		slog.Int("users", len(users)),
		slog.Int64("totalWarmed", totalWarmed),
	)
}

// FilterWidgetGVRs returns only the GVRs from the warmup config whose group
// is widgets.templates.krateo.io (i.e., widget resources that have L1 output).
func FilterWidgetGVRs(cfg *cache.WarmupConfig) []schema.GroupVersionResource {
	if cfg == nil {
		return nil
	}
	var out []schema.GroupVersionResource
	for _, entry := range cfg.Warmup.GVRs {
		if entry.Group == widgetGroup {
			out = append(out, schema.GroupVersionResource{
				Group:    entry.Group,
				Version:  entry.Version,
				Resource: entry.Resource,
			})
		}
	}
	return out
}


// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type discoveredUser struct {
	userInfo jwtutil.UserInfo
	endpoint endpoints.Endpoint
}

func discoverUsers(ctx context.Context, rc *rest.Config, authnNS string) ([]discoveredUser, error) {
	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	secrets, err := clientset.CoreV1().Secrets(authnNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var users []discoveredUser
	for _, sec := range secrets.Items {
		name := sec.Name
		if !strings.HasSuffix(name, clientConfigSecretSuffix) {
			continue
		}
		username := strings.TrimSuffix(name, clientConfigSecretSuffix)
		ep, err := endpoints.FromSecret(ctx, rc, name, authnNS)
		if err != nil {
			slog.Warn("L1 warmup: failed to load endpoint for user",
				slog.String("user", username), slog.Any("err", err))
			continue
		}
		groups := extractGroupsFromClientCert(ep.ClientCertificateData)
		slog.Debug("L1 warmup: discovered user",
			slog.String("user", username),
			slog.Any("groups", groups))
		users = append(users, discoveredUser{
			userInfo: jwtutil.UserInfo{Username: username, Groups: groups},
			endpoint: ep,
		})
	}
	return users, nil
}


// extractGroupsFromClientCert parses the PEM-encoded client certificate and
// returns the Subject.Organization values, which Kubernetes uses as groups.
func extractGroupsFromClientCert(certPEM string) []string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert.Subject.Organization
}

func warmL1ForUser(ctx context.Context, c *cache.RedisCache, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, gvrs []schema.GroupVersionResource, authnNS string) int64 {
	log := slog.Default()

	var allRefs []l1Ref
	for _, gvr := range gvrs {
		listKey := cache.ListKey(gvr, "")
		var list unstructured.UnstructuredList
		if hit, err := c.Get(ctx, listKey, &list); !hit || err != nil {
			continue
		}
		for _, obj := range list.Items {
			rKey := cache.ResolvedKey(user.Username, gvr, obj.GetNamespace(), obj.GetName(), -1, -1)
			if c.Exists(ctx, rKey) {
				continue
			}
			allRefs = append(allRefs, l1Ref{
				gvr:  gvr,
				ns:   obj.GetNamespace(),
				name: obj.GetName(),
			})
		}
	}

	if len(allRefs) == 0 {
		return 0
	}

	warmed := resolveL1RefsForUser(ctx, user, ep, accessToken, c, allRefs, authnNS)

	log.Info("L1 warmup: user done",
		slog.String("user", user.Username),
		slog.Int("candidates", len(allRefs)),
		slog.Int64("warmed", warmed),
	)
	return warmed
}

type l1Ref struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string
}

func extractChildWidgetRefs(ctx context.Context, c *cache.RedisCache, items []interface{}, username string) []l1Ref {
	var refs []l1Ref
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		path, _ := m["path"].(string)
		if path == "" || !strings.Contains(path, "/call?") {
			continue
		}
		gvr, ns, name := cache.ParseCallPath(path)
		if gvr.Resource == "" || name == "" {
			continue
		}
		if gvr.Group != widgetGroup {
			continue
		}
		key := cache.ResolvedKey(username, gvr, ns, name, -1, -1)
		if c.Exists(ctx, key) {
			continue
		}
		refs = append(refs, l1Ref{gvr: gvr, ns: ns, name: name})
	}
	return refs
}

// resolveL1RefsForUser resolves a batch of L1 refs (widgets or RESTActions) for
// a specific user and stores them in L1 cache. Returns the number successfully
// warmed. The caller provides the context (with its own deadline).
func resolveL1RefsForUser(ctx context.Context, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, c *cache.RedisCache, refs []l1Ref, authnNS string) int64 {
	ctx = xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(accessToken),
	)
	ctx = cache.WithCache(ctx, c)

	var (
		wg     sync.WaitGroup
		sem    = make(chan struct{}, preWarmConcurrency)
		warmed int64
	)

	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(r l1Ref) {
			defer wg.Done()
			defer func() { <-sem }()

			rctx := xcontext.BuildContext(ctx,
				xcontext.WithUserConfig(ep),
				xcontext.WithUserInfo(user),
				xcontext.WithAccessToken(accessToken),
			)
			rctx = cache.WithCache(rctx, c)

			got := objects.Get(rctx, templatesv1.ObjectReference{
				Reference: templatesv1.Reference{
					Name: r.name, Namespace: r.ns,
				},
				APIVersion: r.gvr.GroupVersion().String(),
				Resource:   r.gvr.Resource,
			})
			if got.Err != nil {
				return
			}

			tracker := cache.NewDependencyTracker()
			tctx := cache.WithDependencyTracker(rctx, tracker)

			if r.gvr.Group != widgetGroup {
				return
			}
			res, resolveErr := widgets.Resolve(tctx, widgets.ResolveOptions{
				In:      got.Unstructured,
				AuthnNS: authnNS,
				PerPage: -1,
				Page:    -1,
			})
			if resolveErr != nil {
				return
			}
			raw, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return
			}

			rKey := cache.ResolvedKey(user.Username, r.gvr, r.ns, r.name, -1, -1)
			_ = c.SetResolvedRaw(rctx, rKey, raw)
			for _, gvrKey := range tracker.GVRKeys() {
				_ = c.SAddWithTTL(rctx, cache.L1GVRKey(gvrKey), rKey, cache.DefaultResourceTTL)
			}

			atomic.AddInt64(&warmed, 1)
		}(ref)
	}

	wg.Wait()
	return warmed
}
