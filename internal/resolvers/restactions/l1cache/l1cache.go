// Package l1cache hoists the cache-wrapped RESTAction resolution out of
// internal/handlers/dispatchers/restactions.go so that BOTH the HTTP
// dispatcher path AND the widget apiref path share one implementation.
//
// Why this matters:
//   - Before the hoist there were two writers for the same L1 key
//     (snowplow:resolved:{user}:...:restactions:...:{name}):
//       1. dispatchers/restactions.go  (HTTP /call?resource=restactions)
//       2. resolvers/widgets/apiref    (widget with apiRef to a RA)
//     Drift between the two was a real hazard whenever the key schema,
//     marshal step, or dependency-registration logic changed.
//   - Each path carried its own singleflight.Group so a widget call and
//     a direct /call for the same key would NOT dedup against each
//     other — N parallel resolutions of the same 32 s compositions-list
//     aggregation in the worst case.
//
// Both problems are fixed by keeping the only implementation here and
// sharing one foreground singleflight.Group between all HTTP-triggered
// callers. Background L1 refresh keeps its own separate group so that
// long (10-30 s) re-resolves do not block foreground HTTP requests.
package l1cache

import (
	"context"
	"encoding/json"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/apis"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

var tracer = otel.Tracer("snowplow/resolvers/restactions/l1cache")

// foregroundFlight dedups all HTTP-triggered RESTAction resolutions —
// whether they come from the restaction HTTP dispatcher OR from a
// widget whose apiRef points at the RA. Sharing one group kills the
// cross-path thundering herd.
//
// backgroundFlight is separate so the L1 refresh loop can do long

// Input is everything the resolver + cache layer need from a caller.
// The same struct shape serves both HTTP and widget paths; empty
// ResolvedKey signals a non-cacheable path (skip L1 write AND skip
// singleflight).
type Input struct {
	// Cache is the L1 cache. May be nil (cache disabled); in
	// that case L1 writes are skipped.
	Cache cache.Cache

	// Obj is the unstructured RESTAction CR as fetched from L2 Redis
	// or the informer store. Marshaled shape: map[string]any.
	Obj map[string]any

	// ResolvedKey is the L1 cache key (from cache.ResolvedKey).
	// Empty string disables both L1 write and singleflight dedup —
	// used for the non-cacheable "extras present" path in the HTTP
	// dispatcher.
	ResolvedKey string

	// AuthnNS is the authentication namespace forwarded to the
	// restactions resolver for endpoint lookup.
	AuthnNS string

	// SArc is the service-account rest.Config (only used by the HTTP
	// dispatcher path today; widget apiref leaves it nil and the
	// restactions resolver falls back to UserConfig from ctx).
	SArc *rest.Config

	// PerPage and Page are the pagination hints passed through to
	// the restaction's JQ pipeline. Use 0 / 0 for unpaginated.
	PerPage int
	Page    int

	// Extras are optional key/value pairs that can customise the
	// resolve pipeline. When non-empty the caller must pass an empty
	// ResolvedKey to disable caching, because Extras are NOT part of
	// the key.
	Extras map[string]any

	// SnowplowEndpoint is the elevated-call provider for api[] entries
	// that declare UserAccessFilter. Plumbed through to
	// restactions.ResolveOptions and ultimately api.ResolveOptions. nil
	// is permitted; UserAccessFilter calls will be rejected at runtime.
	SnowplowEndpoint func() (*endpoints.Endpoint, error)
}

// Result is what both HTTP and widget callers need from one resolution.
// Raw is the marshaled + bulky-annotations-stripped CR (written to L1
// and to the HTTP response body). Status is the decoded status subtree
// of the CR, which is what the widget apiref path returns to its own
// callers.
type Result struct {
	Raw    []byte
	Status map[string]any
}

// ResolveAndCache runs the full RESTAction resolution:
// convert → resolve → marshal → strip → L1 write.
// L1 keys are per-user so there is no thundering herd.
func ResolveAndCache(ctx context.Context, in Input) (*Result, error) {
	return resolveAndCacheInner(ctx, in)
}

func resolveAndCacheInner(ctx context.Context, in Input) (*Result, error) {
	ctx, span := tracer.Start(ctx, "restaction.resolve")
	defer span.End()

	log := xcontext.Logger(ctx)

	// Register all core types with a fresh scheme (dispatcher did the
	// same — keeping behaviour bit-for-bit identical).
	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, err
	}

	var cr v1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(in.Obj, &cr); err != nil {
		log.Error("unable to convert unstructured to typed rest action",
			slog.Any("err", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if span.IsRecording() {
		span.SetAttributes(
			attribute.String("restaction.name", cr.GetName()),
			attribute.String("restaction.namespace", cr.GetNamespace()),
		)
	}

	tracker := cache.NewDependencyTracker()
	tctx := cache.WithDependencyTracker(xcontext.BuildContext(ctx), tracker)
	_, dict, err := restactions.Resolve(tctx, restactions.ResolveOptions{
		In:               &cr,
		SArc:             in.SArc,
		AuthnNS:          in.AuthnNS,
		PerPage:          in.PerPage,
		Page:             in.Page,
		Extras:           in.Extras,
		SnowplowEndpoint: in.SnowplowEndpoint,
	})
	if err != nil {
		log.Error("unable to resolve rest action",
			slog.String("name", cr.GetName()),
			slog.String("namespace", cr.GetNamespace()),
			slog.Any("err", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	slog.Debug("RESTAction resolved",
		slog.String("name", cr.Name),
		slog.String("namespace", cr.Namespace))

	// Q-RBAC-DECOUPLE C(d) v3 — write the v3 wire shape so the dispatcher
	// can refilter per-user on every L1 hit. CR.Status (the OUTER-JQ output
	// computed against the UNFILTERED dict by restactions.Resolve above) is
	// retained inside the wrapper for back-compat / debugging visibility,
	// but it is NEVER served verbatim to a user — RefilterRESTAction
	// re-runs the outer JQ against the per-user-refiltered dict and
	// overwrites Status before re-marshaling for the wire.
	_, marshalSpan := tracer.Start(ctx, "http.marshal")
	raw, merr := api.MarshalCached(&cr, dict)
	if merr == nil {
		marshalSpan.SetAttributes(attribute.Int("http.response.body.size", len(raw)))
	}
	marshalSpan.End()
	if merr != nil {
		return nil, merr
	}

	// StripBulkyAnnotations removed: metadata cleanup is now handled by
	// the informer transform (watcher.go:985) for K8s objects and by JQ
	// filters in each RESTAction for the resolved output. Snowplow should
	// not post-process business-level field selection.

	if in.Cache != nil && in.ResolvedKey != "" {
		_ = in.Cache.SetResolvedRaw(tctx, in.ResolvedKey, raw)
		// Only touch on HTTP requests and prewarm, NOT background refresh.
		// Background refresh is system activity — temperature must reflect
		// user access only. DirtySet in context signals background refresh.
		if cache.DirtySetFromContext(tctx) == nil {
			if rki, ok := cache.ParseResolvedKey(in.ResolvedKey); ok {
				cache.TouchKey(cache.ResolvedKeyBase(rki.Username, rki.GVR, rki.NS, rki.Name))
			}
		}
		cache.RegisterL1Dependencies(tctx, in.Cache, tracker, in.ResolvedKey)
	}

	// Q-RBAC-DECOUPLE C(d) v3 — Result.Status is the requesting user's
	// per-user-refiltered status, computed by api.RefilterRESTAction
	// against the bytes we just wrote. This keeps the apiref-path
	// (widgets→apiref→l1cache) trust-boundary invariant: every status
	// returned to a caller has been refiltered for the requesting user.
	//
	// Falling back to the raw-decoded shape (the v0/v2 path) is unsafe in
	// v3: `raw` is the v3 wrapper, so the old `wrapper["status"]` lookup
	// does not exist on the wrapper, and even if it did the value would
	// be the UNFILTERED outer-JQ output computed under whatever user
	// happened to drive the resolver. In v3 the outer JQ is recomputed
	// per-user inside RefilterRESTAction.
	//
	// On any refilter error we return the wrapper's raw bytes with
	// Status=nil so the apiref caller can decide to fall through to a
	// fresh resolve (the same trust-boundary discipline the dispatcher
	// applies on L1 hit).
	refiltered, refErr := api.RefilterRESTAction(ctx, in.Cache, raw)
	if refErr != nil {
		log.Warn("resolveAndCacheInner: refilter failed; returning raw without status",
			slog.String("name", cr.Name),
			slog.String("ns", cr.Namespace),
			slog.Any("err", refErr))
		return &Result{Raw: raw}, nil
	}

	var status map[string]any
	if len(refiltered) > 0 {
		var refWrapper map[string]any
		if uerr := json.Unmarshal(refiltered, &refWrapper); uerr == nil {
			status, _ = refWrapper["status"].(map[string]any)
		}
	}

	// Diagnostic for S7: log item count for list-type RESTActions (e.g.
	// compositions-list). The JQ filter produces {"list": [...items...]},
	// so status["list"] is the array. This lets us see whether a delete
	// event was reflected in the resolved output.
	if status != nil {
		if listRaw, ok := status["list"]; ok {
			if listSlice, ok := listRaw.([]any); ok {
				slog.Info("resolveAndCacheInner: resolved",
					slog.String("name", cr.Name),
					slog.String("ns", cr.Namespace),
					slog.Int("items", len(listSlice)),
					slog.Int("rawBytes", len(raw)))
			}
		}
	}

	return &Result{Raw: raw, Status: status}, nil
}

// extractAPIRequests extracts the "apiRequests" string array from the
// resolved RESTAction JSON output. Used to populate the L1 API-level
