package dispatchers

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/observability"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

var l1RefreshTracer = otel.Tracer("snowplow/l1refresh")

const (
	refreshConcurrency         = 20 // warmup and HTTP-triggered refreshes
	refreshConcurrencyBackground = 8  // l3gen scanner refreshes (fewer keys, less RBAC pressure)
	restactionResource         = "restactions"
	templatesGroup             = "templates.krateo.io"
)

// MakeL1Refresher returns a cache.L1RefreshFunc that re-resolves L1 keys in
// the background instead of deleting them. Old values keep being served while
// the refresh runs (stale-while-revalidate).
func MakeL1Refresher(c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) cache.L1RefreshFunc {
	return func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string) {
		ctx, span := l1RefreshTracer.Start(ctx, "l1.refresh",
			trace.WithAttributes(
				attribute.String("trigger", triggerGVR.String()),
				attribute.Int("keys", len(l1Keys)),
			),
		)
		defer span.End()

		log := slog.Default()

		// Use lower concurrency for background l3gen scans (empty triggerGVR)
		// to reduce RBAC API pressure. Warmup uses full concurrency.
		concurrency := refreshConcurrency
		if triggerGVR.Resource == "" {
			concurrency = refreshConcurrencyBackground
		}

		refreshStart := time.Now()

		log.Info("L1 refresh: starting",
			slog.String("trigger", triggerGVR.String()),
			slog.Int("keys", len(l1Keys)),
			slog.Int("concurrency", concurrency))

		type userKeys struct {
			info cache.ResolvedKeyInfo
			raw  string
		}
		byUser := map[string][]userKeys{}
		for _, key := range l1Keys {
			info, ok := cache.ParseResolvedKey(key)
			if !ok {
				continue
			}
			byUser[info.Username] = append(byUser[info.Username], userKeys{info: info, raw: key})
		}

		// Only refresh L1 for users with a valid -clientconfig secret
		// (i.e., users who logged in while their certificate is still valid).
		// authn removes the secret when the cert expires, so the active-users
		// set tracks exactly who should get eager refresh.
		// Users without a secret are skipped — their L1 expires via TTL and
		// gets re-resolved on next login.
		//
		// FAIL-OPEN: if we cannot read the active-users set (Redis error,
		// context cancelled, empty set), refresh ALL users rather than
		// skipping everyone. Silent skip caused the S7 regression in v0.25.131.
		activeUsers, err := c.SMembers(ctx, cache.ActiveUsersKey)
		if err != nil || len(activeUsers) == 0 {
			if err != nil {
				log.Warn("L1 refresh: cannot read active-users set, refreshing all users",
					slog.Any("err", err))
			}
			// Fall through — byUser is unchanged, all users refreshed
		} else {
			activeSet := make(map[string]bool, len(activeUsers))
			for _, u := range activeUsers {
				activeSet[u] = true
			}
			skippedUsers := 0
			for username := range byUser {
				if !activeSet[username] {
					delete(byUser, username)
					skippedUsers++
				}
			}
			if skippedUsers > 0 {
				log.Info("L1 refresh: skipped inactive users",
					slog.Int("skipped", skippedUsers),
					slog.Int("active", len(byUser)))
			}
		}

		// Classify users by activity and build a priority-ordered refresh list.
		// Hot users first, warm second, cold last. ALL users get refreshed —
		// just in priority order so active users see fresh data sooner.
		now := time.Now().Unix()
		type classifiedUser struct {
			username string
			class    string
		}
		var hotUsers, warmUsers, coldUsers []classifiedUser
		for username := range byUser {
			raw, _, _ := c.GetRaw(ctx, "snowplow:last-seen:"+username)
			if raw == nil {
				coldUsers = append(coldUsers, classifiedUser{username, "cold"})
				continue
			}
			lastSeen, _ := strconv.ParseInt(string(raw), 10, 64)
			age := now - lastSeen
			switch {
			case age < 300: // 5 min
				hotUsers = append(hotUsers, classifiedUser{username, "hot"})
			case age < 3600: // 60 min
				warmUsers = append(warmUsers, classifiedUser{username, "warm"})
			default:
				coldUsers = append(coldUsers, classifiedUser{username, "cold"})
			}
		}

		// Priority-ordered: hot first, warm second, cold last
		orderedUsers := make([]classifiedUser, 0, len(hotUsers)+len(warmUsers)+len(coldUsers))
		orderedUsers = append(orderedUsers, hotUsers...)
		orderedUsers = append(orderedUsers, warmUsers...)
		orderedUsers = append(orderedUsers, coldUsers...)

		log.Info("L1 refresh: user activity classes",
			slog.Int("hot", len(hotUsers)),
			slog.Int("warm", len(warmUsers)),
			slog.Int("cold", len(coldUsers)))

		// Global concurrency semaphore shared across ALL users.
		// Previous attempt (v0.25.135) used nested semaphores (4 outer × 8 inner
		// = 32 goroutines) which caused resource contention. A single global
		// semaphore keeps total concurrent work at `concurrency` regardless of
		// how many users are being refreshed.
		// All L1 refreshes are full re-resolves. Incremental patching was
		// removed because JQ aggregation filters (e.g., piechart counts)
		// require the full pipeline to produce correct results.
		var (
			totalRefreshed int64
			globalSem      = make(chan struct{}, concurrency)
			globalWg       sync.WaitGroup
			globalMu       sync.Mutex
			allCascade     []string
		)

		for _, cu := range orderedUsers {
			username := cu.username
			keys := byUser[username]

			ep, err := endpoints.FromSecret(ctx, rc, username+clientConfigSecretSuffix, authnNS)
			if err != nil {
				log.Warn("L1 refresh: cannot load user endpoint",
					slog.String("user", username), slog.Any("err", err))
				continue
			}
			groups := extractGroupsFromClientCert(ep.ClientCertificateData)
			user := jwtutil.UserInfo{Username: username, Groups: groups}
			accessToken := mintJWT(user, signKey)

			for _, k := range keys {
				globalWg.Add(1)
				globalSem <- struct{}{} // blocks if concurrency limit reached
				go func(u jwtutil.UserInfo, e endpoints.Endpoint, token string, info cache.ResolvedKeyInfo, rawKey string) {
					defer globalWg.Done()
					defer func() { <-globalSem }()
					ok, cascade := refreshSingleL1(ctx, c, u, e, token, info, rawKey, authnNS)
					if ok {
						globalMu.Lock()
						totalRefreshed++
						allCascade = append(allCascade, cascade...)
						globalMu.Unlock()
					}
				}(user, ep, accessToken, k.info, k.raw)
			}
		}

		// Wait for ALL initial refreshes across all users to complete.
		globalWg.Wait()

		// Cascading refresh: iteratively re-resolve L1 keys that depend
		// on refreshed RESTActions. Each round may discover more dependents
		// (e.g. CRD event → compositions-get-ns-and-crd → compositions-list
		// → piechart). Limit depth to avoid infinite loops.
		// Cascade uses per-user credentials, so we re-load endpoints per key.
		refreshed := make(map[string]bool, len(l1Keys))
		for _, key := range l1Keys {
			refreshed[key] = true
		}
		const maxCascadeDepth = 5
		pending := allCascade
		for depth := 0; depth < maxCascadeDepth && len(pending) > 0; depth++ {
			var nextCascade []string
			var cascadeWg sync.WaitGroup
			var cascadeMu sync.Mutex
			for _, ck := range pending {
				if refreshed[ck] {
					continue
				}
				refreshed[ck] = true
				ci, cok := cache.ParseResolvedKey(ck)
				if !cok {
					continue
				}
				// Skip if user is not in our active set
				if _, ok := byUser[ci.Username]; !ok {
					continue
				}
				ep, err := endpoints.FromSecret(ctx, rc, ci.Username+clientConfigSecretSuffix, authnNS)
				if err != nil {
					continue
				}
				groups := extractGroupsFromClientCert(ep.ClientCertificateData)
				user := jwtutil.UserInfo{Username: ci.Username, Groups: groups}
				accessToken := mintJWT(user, signKey)

				cascadeWg.Add(1)
				globalSem <- struct{}{}
				go func(cInfo cache.ResolvedKeyInfo, cRawKey string, u jwtutil.UserInfo, e endpoints.Endpoint, token string) {
					defer cascadeWg.Done()
					defer func() { <-globalSem }()
					ok, cascade := refreshSingleL1(ctx, c, u, e, token, cInfo, cRawKey, authnNS)
					if ok {
						cascadeMu.Lock()
						totalRefreshed++
						nextCascade = append(nextCascade, cascade...)
						cascadeMu.Unlock()
					}
				}(ci, ck, user, ep, accessToken)
			}
			cascadeWg.Wait()
			pending = nextCascade
		}

		// Record OTel metrics
		refreshDuration := time.Since(refreshStart)
		if observability.L1RefreshDuration != nil {
			observability.L1RefreshDuration.Record(ctx, refreshDuration.Seconds())
		}
		if observability.L1RefreshUsers != nil {
			observability.L1RefreshUsers.Add(ctx, int64(len(byUser)))
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
	}
}

// refreshSingleL1 re-resolves one L1 entry and updates the cache in-place.
// For widget entries, it also calls preWarmChildWidgets so that child widgets
// discovered during resolution (e.g. composition-panels) are pre-warmed into L1.
//
// Returns (ok, cascadeKeys): ok indicates success, cascadeKeys contains L1 keys
// that depend on the refreshed resource and should be enqueued for refresh too
// (cascading invalidation for RESTAction → widget dependency chains).
func refreshSingleL1(ctx context.Context, c *cache.RedisCache, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, info cache.ResolvedKeyInfo, rawKey, authnNS string) (bool, []string) {
	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(accessToken),
	)
	rctx = cache.WithCache(rctx, c)

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
		// Use ResolveWidgetBackground to avoid blocking HTTP requests that
		// resolve the same key via singleflight.
		//
		// IMPORTANT: resolveWidgetFromObject (called inside) registers deps
		// via RegisterL1Dependencies. But if the singleflight dedup skips
		// the resolve fn (another caller already running), deps are NOT
		// re-registered. To guarantee the dep chain survives across L1
		// refresh cycles, we explicitly re-register the apiRef GVR dep
		// AFTER the resolve completes, using a fresh tracker.
		tracker := cache.NewDependencyTracker()
		tctx := cache.WithDependencyTracker(rctx, tracker)
		result, err := ResolveWidgetBackground(tctx, c, got, rawKey, authnNS, info.PerPage, info.Page)
		if err != nil {
			return false, nil
		}
		_ = result
		// Re-register deps unconditionally. The tracker may have been
		// populated by resolveWidgetFromObject (singleflight winner) or
		// by apiref.Resolve's L1-hit dep tracking (0.25.178 fix). Either
		// way, SADD is idempotent — re-adding the same dep is a no-op.
		cache.RegisterL1Dependencies(rctx, c, tracker, rawKey)
		registerApiRefGVRDeps(rctx, c, got.Unstructured, rawKey, tracker)
		return true, nil

	case info.GVR.Group == templatesGroup && info.GVR.Resource == restactionResource:
		// Use ResolveRESTActionBackground to avoid blocking HTTP requests.
		_, err := ResolveRESTActionBackground(rctx, c, got.Unstructured.Object, rawKey, authnNS, info.PerPage, info.Page)
		if err != nil {
			return false, nil
		}
		registerApiRefGVRDeps(rctx, c, got.Unstructured, rawKey, nil)

		// Cascading refresh: find L1 keys that depend on this RESTAction
		// as a resource (e.g. piechart depends on compositions-list).
		depKey := cache.L1ResourceDepKey(
			cache.GVRToKey(info.GVR), info.NS, info.Name,
		)
		cascade, _ := c.SMembers(rctx, depKey)
		return true, cascade

	default:
		return false, nil
	}
}

// filterDeleteChanges extracts changes whose net effect is a delete.
// K8s informers fire UPDATE before DELETE (adding deletionTimestamp),
// so we track the last operation per namespace/name. Items whose last
