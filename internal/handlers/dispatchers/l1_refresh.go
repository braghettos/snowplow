package dispatchers

import (
	"context"
	"log/slog"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/observability"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/l1cache"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

var l1RefreshTracer = otel.Tracer("snowplow/l1refresh")

const (
	restactionResource         = "restactions"
	templatesGroup             = "templates.krateo.io"
)

// userContext holds cached credentials for a user. Loaded once,
// shared across all goroutines. Refreshed when the underlying
// secret changes (via UserSecretWatcher).
type userContext struct {
	endpoint    endpoints.Endpoint
	user        jwtutil.UserInfo
	accessToken string
	loadedAt    int64 // unix seconds
}

// MakeL1Refresher returns a cache.L1RefreshFunc that re-resolves L1 keys in
// the background instead of deleting them. Old values keep being served while
// the refresh runs (stale-while-revalidate).
// rbacWatcher may be nil; when non-nil it is injected into the refresh context
// so UserCan uses in-memory RBAC evaluation instead of SSAR API calls.
//
// snowplowEndpointFn is the elevated-call provider for api[] entries that
// declare userAccessFilter (Q-RBAC-DECOUPLE C(d)). May be nil; that disables
// elevated dispatch during background refresh.
func MakeL1Refresher(c cache.Cache, rc *rest.Config, authnNS, signKey string, rbacWatcher *cache.RBACWatcher, snowplowEndpointFn func() (*endpoints.Endpoint, error)) cache.L1RefreshFunc {
	// User context cache: loaded once per user, shared across all goroutines.
	// Avoids 50K K8s Secret GETs when 50K events arrive for the same users.
	var (
		userCtxMu    sync.RWMutex
		userCtxCache = make(map[string]*userContext)
	)

	getUserContext := func(ctx context.Context, identity string) (*userContext, error) {
		// The identity may be a binding identity hash (from Phase C) or
		// a plain username. Resolve to real username for credential lookup.
		username := identity
		if realUser, ok := cache.UsernameForBinding(identity); ok {
			username = realUser
		}

		now := time.Now().Unix()

		// Fast path: read from cache (keyed by real username).
		userCtxMu.RLock()
		uc, ok := userCtxCache[username]
		userCtxMu.RUnlock()
		if ok && (now-uc.loadedAt) < 300 { // 5 min TTL
			return uc, nil
		}

		// Slow path: load from K8s secret.
		ep, err := endpoints.FromSecret(ctx, rc, username+clientConfigSecretSuffix, authnNS)
		if err != nil {
			return nil, err
		}
		groups := extractGroupsFromClientCert(ep.ClientCertificateData)
		user := jwtutil.UserInfo{Username: username, Groups: groups}
		accessToken := mintJWT(user, signKey)

		uc = &userContext{
			endpoint:    ep,
			user:        user,
			accessToken: accessToken,
			loadedAt:    now,
		}
		userCtxMu.Lock()
		userCtxCache[username] = uc
		userCtxMu.Unlock()
		return uc, nil
	}

	return func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string) []string {
		// Inject RBACWatcher for local RBAC evaluation during background refresh.
		// Without this, UserCan falls back to SSAR (K8s API), which saturates
		// the rate limiter with 345K calls at 50K scale.
		if rbacWatcher != nil {
			ctx = cache.WithRBACWatcher(ctx, rbacWatcher)
		}
		// Inject snowplow-SA endpoint provider so api[] entries that
		// declare userAccessFilter resolve correctly during the
		// background refresh path (Q-RBAC-DECOUPLE C(d)).
		if snowplowEndpointFn != nil {
			ctx = cache.WithSnowplowEndpoint(ctx, func() (any, error) {
				return snowplowEndpointFn()
			})
		}

		ctx, span := l1RefreshTracer.Start(ctx, "l1.refresh",
			trace.WithAttributes(
				attribute.String("trigger", triggerGVR.String()),
				attribute.Int("keys", len(l1Keys)),
			),
		)
		defer span.End()

		log := slog.Default()
		refreshStart := time.Now()

		type identityKeys struct {
			info cache.ResolvedKeyInfo
			raw  string
		}
		byIdentity := map[string][]identityKeys{}
		for _, key := range l1Keys {
			info, ok := cache.ParseResolvedKey(key)
			if !ok {
				continue
			}
			byIdentity[info.Username] = append(byIdentity[info.Username], identityKeys{info: info, raw: key})
		}

		// Resolve each key directly. No sub-goroutines, no semaphores.
		// User credentials are cached — no K8s API call per goroutine.
		// The identity may be a binding identity hash or a plain username.
		var totalRefreshed int64
		var allCascade []string

		for identity, keys := range byIdentity {
			uc, err := getUserContext(ctx, identity)
			if err != nil {
				log.Warn("L1 refresh: cannot load user context",
					slog.String("identity", identity), slog.Any("err", err))
				continue
			}

			for _, k := range keys {
				ok, cascade := refreshSingleL1(ctx, c, uc.user, uc.endpoint, uc.accessToken, k.info, k.raw, authnNS, snowplowEndpointFn)
				if ok {
					totalRefreshed++
					allCascade = append(allCascade, cascade...)
				}
			}
		}

		// Cascade keys are returned to the caller (markDirty in watcher.go)
		// which handles ordering: C is resolved before B reads C's fresh L1.

		// Record OTel metrics
		refreshDuration := time.Since(refreshStart)
		if observability.L1RefreshDuration != nil {
			observability.L1RefreshDuration.Record(ctx, refreshDuration.Seconds())
		}
		if observability.L1RefreshUsers != nil {
			observability.L1RefreshUsers.Add(ctx, int64(len(byIdentity)))
		}

		if ctx.Err() != nil {
			log.Error("L1 refresh: context expired before completion",
				slog.String("trigger", triggerGVR.String()),
				slog.Int64("refreshed", totalRefreshed),
				slog.Int("total", len(l1Keys)),
				slog.String("duration", refreshDuration.String()),
				slog.Any("err", ctx.Err()))
			span.SetStatus(2, "context expired") // codes.Error = 2
		}
		log.Info("L1 refresh: done",
			slog.String("trigger", triggerGVR.String()),
			slog.Int64("refreshed", totalRefreshed),
			slog.Int("total", len(l1Keys)),
			slog.String("duration", refreshDuration.String()))
		// Use a fresh background context so the sentinel is always written,
		// even if the refresh context has expired.
		cache.MarkL1Ready(context.Background(), c)

		return allCascade
	}
}

// refreshSingleL1 re-resolves one L1 entry and updates the cache in-place.
// For widget entries, it also calls preWarmChildWidgets so that child widgets
// discovered during resolution (e.g. composition-panels) are pre-warmed into L1.
//
// Returns (ok, cascadeKeys): ok indicates success, cascadeKeys contains L1 keys
// that depend on the refreshed resource and should be enqueued for refresh too
// (cascading invalidation for RESTAction → widget dependency chains).
//
// snowplowEndpointFn is forwarded to ResolveRESTActionBackground so api[]
// entries that declare userAccessFilter resolve correctly during the
// background refresh path.
func refreshSingleL1(ctx context.Context, c cache.Cache, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, info cache.ResolvedKeyInfo, rawKey, authnNS string, snowplowEndpointFn func() (*endpoints.Endpoint, error)) (bool, []string) {
	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(accessToken),
		xcontext.WithLogger(slog.Default()),
	)
	rctx = cache.WithCache(rctx, c)

	// Propagate RBACWatcher from parent context for local RBAC evaluation.
	// The closure in MakeL1Refresher injects it before calling us.
	if rw := cache.RBACWatcherFromContext(ctx); rw != nil {
		rctx = cache.WithRBACWatcher(rctx, rw)
	}

	// Propagate InformerReader so nested resolution can read from informer.
	if ir := cache.InformerReaderFromContext(ctx); ir != nil {
		rctx = cache.WithInformerReader(rctx, ir)
	}

	// Propagate snowplow-SA endpoint provider (Q-RBAC-DECOUPLE C(d)) so
	// nested resolutions of api[] entries with userAccessFilter can dispatch
	// elevated calls. Refresh closure here also wraps snowplowEndpointFn
	// directly into ctx for parity (see MakeL1Refresher.refreshFn).
	if snEp := cache.SnowplowEndpointFromContext(ctx); snEp != nil {
		rctx = cache.WithSnowplowEndpoint(rctx, snEp)
	}

	// Propagate DirtySet so nested resolution bypasses stale API result cache.
	if ds := cache.DirtySetFromContext(ctx); ds != nil {
		rctx = cache.WithDirtySet(rctx, ds)
	}

	// Inject inline /call resolver so nested RESTAction calls (e.g.
	// compositions-list → compositions-get-ns-and-crd) resolve in-process
	// from the informer instead of making an HTTP round-trip back to
	// snowplow, which times out under high load at 50K scale.
	//
	// Q-RBAC-DECOUPLE C(d) — nested userAccessFilter dispatch contract:
	// the l1cache.ResolveAndCache call below does NOT thread snowplowEndpointFn
	// through l1cache.Input on purpose. The nested resolver inherits
	// cache.WithSnowplowEndpoint via callCtx — installed at lines 107-111
	// above and propagated into rctx at lines 229-231. If a future refactor
	// removes the context fallback in
	// `internal/resolvers/restactions/api/resolve.go` (the
	// `cache.SnowplowEndpointFromContext(ctx)` branch around the dispatch
	// fork, currently ~line 263), audit THIS site first or thread
	// snowplowEndpointFn into l1cache.Input — otherwise nested /call
	// dispatch under userAccessFilter will silently break (architect
	// review concern #2, 2026-05-04).
	if cache.InformerReaderFromContext(rctx) != nil {
		rctx = cache.WithCallResolver(rctx, func(callCtx context.Context, obj map[string]any, resolvedKey, callAuthnNS string) ([]byte, error) {
			result, err := l1cache.ResolveAndCache(callCtx, l1cache.Input{
				Cache:       c,
				Obj:         obj,
				ResolvedKey: resolvedKey,
				AuthnNS:     callAuthnNS,
			})
			if err != nil {
				return nil, err
			}
			if result == nil {
				return nil, nil
			}
			return result.Raw, nil
		})
	}

	// If the key's identity differs from the real username, it's a binding
	// identity hash. Set it in context so CacheIdentity returns the hash
	// during inner resolution, keeping keys consistent.
	if info.Username != user.Username {
		rctx = cache.WithBindingIdentity(rctx, info.Username)
	}

	got := objects.Get(rctx, templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name: info.Name, Namespace: info.NS,
		},
		APIVersion: info.GVR.GroupVersion().String(),
		Resource:   info.GVR.Resource,
	})
	if got.Err != nil {
		return false, nil
	}

	switch {
	case info.GVR.Group == widgetGroup:
		// resolveWidgetFromObject (called inside) registers deps via
		// RegisterL1Dependencies.
		result, err := ResolveWidget(rctx, c, got, rawKey, authnNS, info.PerPage, info.Page)
		if err != nil {
			return false, nil
		}
		_ = result
		slog.Info("refreshSingleL1: widget resolved",
			slog.String("key", rawKey),
			slog.String("name", info.Name),
			slog.String("ns", info.NS))
		// Do NOT TouchKey here: background refresh is system activity, not
		// user access. Temperature must reflect user visits only.
		return true, nil

	case info.GVR.Group == templatesGroup && info.GVR.Resource == restactionResource:
		_, err := ResolveRESTActionBackground(rctx, c, got.Unstructured.Object, rawKey, authnNS, info.PerPage, info.Page, snowplowEndpointFn)
		if err != nil {
			slog.Info("refreshSingleL1: RESTAction resolve failed",
				slog.String("key", rawKey), slog.Any("err", err))
			return false, nil
		}

		slog.Info("refreshSingleL1: RESTAction resolved",
			slog.String("key", rawKey),
			slog.String("name", info.Name),
			slog.String("ns", info.NS))

		// Cascading refresh: find L1 keys that depend on this RESTAction
		// as a resource (e.g. piechart depends on compositions-list).
		depKey := cache.L1ResourceDepKey(
			cache.GVRToKey(info.GVR), info.NS, info.Name,
		)
		cascade, _ := c.SMembers(rctx, depKey)
		if len(cascade) > 0 {
			slog.Info("refreshSingleL1: cascade",
				slog.String("from", info.Name),
				slog.Int("targets", len(cascade)))
		}
		return true, cascade

	default:
		return false, nil
	}
}

// filterDeleteChanges extracts changes whose net effect is a delete.
// K8s informers fire UPDATE before DELETE (adding deletionTimestamp),
// so we track the last operation per namespace/name. Items whose last
