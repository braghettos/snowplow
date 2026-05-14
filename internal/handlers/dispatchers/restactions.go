package dispatchers

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/snowplow/apis"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"k8s.io/apimachinery/pkg/runtime"
)

func RESTAction() http.Handler {
	return &restActionHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
	}
}

type restActionHandler struct {
	authnNS string
	verbose bool
}

var _ http.Handler = (*restActionHandler)(nil)

func (r *restActionHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	log := xcontext.Logger(req.Context())

	start := time.Now()

	extras, err := util.ParseExtras(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}

	got := fetchObject(req)
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	// Revision 2 binding (0.30.4): in cache=on mode every RestAction
	// dispatch is gated by EvaluateRBAC against the CR being dispatched.
	// Cache=off skips this gate — fetchObject already runs per-user
	// against apiserver, which enforces RBAC inline.
	if !cache.Disabled() {
		if !checkDispatchRBAC(req.Context(), got.GVR, got.Unstructured.GetNamespace()) {
			log.Warn("RESTAction dispatch denied by EvaluateRBAC",
				slog.String("name", got.Unstructured.GetName()),
				slog.String("namespace", got.Unstructured.GetNamespace()),
				slog.String("gvr", got.GVR.String()),
			)
			response.Encode(wri, response.New(http.StatusForbidden,
				fmt.Errorf("forbidden: cannot get %s in namespace %q",
					got.GVR.Resource, got.Unstructured.GetNamespace())))
			return
		}
	}

	perPage, page := paginationInfo(log, req)

	// Tag 0.30.7: L1 resolved-output cache lookup. Runs strictly
	// AFTER the EvaluateRBAC gate above (Revision 2 binding) so the
	// permission check is never short-circuited by a cache hit. Cache
	// hits short-circuit the resolver + JSON re-encode; misses fall
	// through to the 0.30.6-equivalent resolve-and-encode path.
	//
	// Per feedback_l1_invalidation_delete_only.md:
	//   * DELETE evicts dependent L1 keys (0.30.8 dep tracker).
	//   * UPDATE/PATCH enqueue refresh via the background refresher
	//     (stale-while-revalidate; never evicts).
	//   * TTL remains the outer safety net.
	cacheKey, cacheHandle, cacheInputs := dispatchCacheLookupKey(req.Context(), "restactions",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		perPage, page, extras)
	if cacheHandle != nil {
		if entry, ok := cacheHandle.Get(cacheKey); ok {
			emitResolvedCacheLookup(log, "restactions", cacheKey, true, len(entry.RawJSON))
			writeResolvedJSON(wri, entry.RawJSON)
			log.Info("RESTAction successfully resolved",
				slog.String("name", got.Unstructured.GetName()),
				slog.String("namespace", got.Unstructured.GetNamespace()),
				slog.String("duration", util.ETA(start)),
				slog.String("l1", "hit"),
			)
			return
		}
		emitResolvedCacheLookup(log, "restactions", cacheKey, false, 0)
	}

	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		log.Error("unable to add apis to scheme",
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	var cr v1.RESTAction
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr)
	if err != nil {
		log.Error("unable to convert unstructured to typed rest action",
			slog.String("name", got.Unstructured.GetName()),
			slog.String("namespace", got.Unstructured.GetNamespace()),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	ctx := xcontext.BuildContext(req.Context())
	// 0.30.94 Edge type 3: attach the L1 key being populated so the
	// resolver can record dep edges for each inner K8s call it makes.
	// Empty cacheKey (L1 disabled, RBAC-skipped) is a no-op inside
	// WithL1KeyContext — the resolver sees an empty key and skips
	// recording.
	if cacheKey != "" {
		ctx = cache.WithL1KeyContext(ctx, cacheKey)
	}
	res, err := restactions.Resolve(ctx, restactions.ResolveOptions{
		In:      &cr,
		AuthnNS: r.authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve rest action",
			slog.String("name", cr.GetName()),
			slog.String("namespace", cr.GetNamespace()),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	// Encode once, write once, and (if L1 is live) store the encoded
	// bytes for the next lookup. Sharing the same []byte between the
	// http.ResponseWriter write path and the cache entry is safe
	// because the cache treats RawJSON as immutable once put.
	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		log.Error("unable to encode rest action response",
			slog.String("name", cr.Name),
			slog.String("namespace", cr.Namespace),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}
	if cacheHandle != nil && cacheKey != "" {
		cacheHandle.Put(cacheKey, &cache.ResolvedEntry{
			RawJSON: encoded,
			Inputs:  cacheInputs,
		})
		// 0.30.8: record the self-dep so a DELETE on this RestAction
		// CR evicts the cached entry, and an UPDATE re-resolves it.
		// Inner-K8s-call deps (edge type 3) are NOT recorded at this
		// tag — that would require a *RecordingDeps context threaded
		// through resolve.go, which is deferred to a future sub-ship.
		// TTL remains the outer safety net for changes the dep
		// tracker cannot see.
		//
		// 0.30.9 Sub-scope B: ensure the informer for got.GVR is
		// registered BEFORE recording the dep. Without this, a
		// previously-unseen RestAction GVR would record a forward
		// edge whose DELETE/UPDATE events the watcher never wires.
		ensureWatcherInformerForGVR(got.GVR)
		cache.Deps().Record(cacheKey, got.GVR, got.Unstructured.GetNamespace(), got.Unstructured.GetName())
	}

	log.Info("RESTAction successfully resolved",
		slog.String("name", cr.Name),
		slog.String("namespace", cr.Namespace),
		slog.String("duration", util.ETA(start)),
		slog.String("l1", "miss"),
	)

	writeResolvedJSON(wri, encoded)
}
