package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	httpcall "github.com/krateoplatformops/snowplow/internal/httpcall"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// instrumentedDictWrite wraps a dictMu critical section with a
// "restaction.api.dict_write" OTel span recording wait time, hold time,
// and dict size before lock acquisition. Per the design 1-pager at
// /tmp/snowplow-runs/dictmu-otel-span-design-2026-05-03.md the span is
// emitted at every critical-section entry; attribute work is guarded by
// span.IsRecording() so the OTEL_ENABLED=false path stays at ~15 ns/site
// (no-op Start/End plus a predicted-false branch).
//
// fn runs inside mu.Lock()/mu.Unlock() (or unlocked if mu == nil — the
// "single" fan-out shape at the informer-direct mu==nil branch). Callers
// that need to capture an error or other state from the locked region do
// so via closure variables in fn — fn returns nothing here on purpose to
// keep the helper signature unaware of caller-specific result types.
//
// extra is appended to the standard attribute set; pass nil at sites that
// do not need additional attributes.
func instrumentedDictWrite(
	ctx context.Context,
	mu *sync.Mutex,
	dict map[string]any,
	site, intent string,
	extra []attribute.KeyValue,
	fn func(),
) {
	_, span := apiHandlerTracer.Start(ctx, "restaction.api.dict_write")
	recording := span.IsRecording()
	var waitStart, holdStart time.Time
	if recording {
		waitStart = time.Now()
	}
	if mu != nil {
		mu.Lock()
	}
	var sizeBefore int
	if recording {
		holdStart = time.Now()
		sizeBefore = len(dict)
	}
	fn()
	if mu != nil {
		mu.Unlock()
	}
	if recording {
		done := time.Now()
		fanout := "single"
		if mu != nil {
			fanout = "iterator"
		}
		attrs := make([]attribute.KeyValue, 0, 6+len(extra))
		attrs = append(attrs,
			attribute.String("dict.site", site),
			attribute.String("dict.intent", intent),
			attribute.String("fanout.kind", fanout),
			attribute.Int("dict.size_before", sizeBefore),
			attribute.Int64("dict.wait_ns", holdStart.Sub(waitStart).Nanoseconds()),
			attribute.Int64("dict.hold_ns", done.Sub(holdStart).Nanoseconds()),
		)
		attrs = append(attrs, extra...)
		span.SetAttributes(attrs...)
	}
	span.End()
}

const (
	//annotationKeyVerboseAPI = "krateo.io/verbose"
	headerAcceptJSON = "Accept: application/json"
)

type ResolveOptions struct {
	RC      *rest.Config
	AuthnNS string
	Verbose bool
	Items   []*templates.API
	PerPage int
	Page    int
	Extras  map[string]any

	// SnowplowEndpoint, if non-nil, is invoked once per api[] entry that
	// declares UserAccessFilter to obtain the snowplow ServiceAccount
	// endpoint used to dispatch the elevated read. Implementations should
	// re-read BearerTokenFile per call (~10µs tmpfs read) so projected SA
	// token rotation is picked up automatically (Q-RBACC-IMPL-8).
	//
	// nil is permitted; in that case any UserAccessFilter call is rejected
	// with a runtime error log (the migration sequence is: ship snowplow
	// image with this wired BEFORE migrating any RESTAction YAML to use
	// userAccessFilter).
	SnowplowEndpoint func() (*endpoints.Endpoint, error)
}

func Resolve(ctx context.Context, opts ResolveOptions) map[string]any {
	if len(opts.Items) == 0 {
		return map[string]any{}
	}

	if opts.RC == nil {
		var err error
		opts.RC, err = rest.InClusterConfig()
		if err != nil {
			return map[string]any{}
		}
	}

	log := xcontext.Logger(ctx)
	log.Debug("pagination options", slog.Int("page", opts.Page), slog.Int("perPage", opts.PerPage))

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("unable to fetch user info from context", slog.Any("err", err))
		return map[string]any{}
	}

	// Use binding identity for cache keys when available (shared L1 entries).
	identity := cache.CacheIdentity(ctx, user.Username)

	// Extract Redis cache from context (nil-safe: all cache ops are no-ops on nil).
	c := cache.FromContext(ctx)

	// Sort API by Depends into parallel levels
	levels, err := topologicalLevels(opts.Items)
	if err != nil {
		log.Error("unable to sort api by deps", slog.Any("error", err))
		return map[string]any{}
	}
	// Flatten for backward compat logging
	var names []string
	for _, lvl := range levels {
		names = append(names, lvl...)
	}
	log.Debug("sorted api by deps", slog.Any("names", names), slog.Int("levels", len(levels)))

	apiMap := make(map[string]*templates.API, len(opts.Items))
	for _, id := range names {
		for _, el := range opts.Items {
			if el.Name == id {
				apiMap[id] = el
				break
			}
		}
	}
	log.Debug("created api map", slog.Int("total", len(apiMap)))

	// Endpoints reference mapper
	mapper := endpointReferenceMapper{
		authnNS:  opts.AuthnNS,
		username: user.Username,
		rc:       opts.RC,
	}

	dict := map[string]any{}
	if opts.Extras != nil {
		dict = maps.DeepCopyJSON(opts.Extras)
	}

	if opts.PerPage > 0 && opts.Page > 0 {
		dict["slice"] = map[string]any{
			"page":    opts.Page,
			"perPage": opts.PerPage,
			"offset":  (opts.Page - 1) * opts.PerPage,
		}
	}

	log.Debug("base dict for api resolver", slog.Int("dict_keys", len(dict)))

	// Collect all API request paths executed during resolution.
	// These are the actual expanded paths (not JQ templates) and are
	// stored in the resolved output so callers can extract K8s API
	// group dependencies deterministically.
	var apiRequestsMu sync.Mutex
	var apiRequests []string

	// ── Parallel topological levels ──────────────────────────────────────
	// APIs within the same dependency level are independent and can be
	// resolved in parallel. A bounded semaphore (20) prevents overwhelming
	// the K8s API server. Levels are processed sequentially so that
	// dependent APIs see the results of their dependencies in dict.
	//
	// A single mutex (dictMu) serialises ALL writes to the shared dict
	// across all API calls within a level.

	// resolveAPI resolves a single API entry (which may fan out into N
	// iterator calls). It returns false if the resolution failed and the
	// API does not have continueOnError set.
	resolveAPI := func(id string, dictMu *sync.Mutex) bool {
		// Get the api with this identifier
		apiCall, ok := apiMap[id]
		if !ok {
			log.Warn("api not found in apiMap", slog.Any("name", id))
			return true
		}

		// Validate the userAccessFilter shape early. CEL admission already
		// enforces this at apply time, but RESTActions admitted under an
		// older CRD (no CEL rules) need the same guard at runtime. Errors
		// HARD-FAIL the api[] entry — a degenerate filter would silently
		// leak items to the requesting user (Q-RBACC-IMPL-5).
		if err := validateUserAccessFilter(apiCall); err != nil {
			log.Error("userAccessFilter rejected at runtime",
				slog.String("api", id), slog.Any("err", err))
			return false
		}

		if apiCall.Headers == nil {
			apiCall.Headers = []string{headerAcceptJSON}
		}

		if accessToken, _ := xcontext.AccessToken(ctx); accessToken != "" {
			if shouldInjectUserJWT(apiCall) {
				apiCall.Headers = append(apiCall.Headers,
					fmt.Sprintf("Authorization: Bearer %s", accessToken))
			}
		}

		// Endpoint dispatch fork.
		// - userAccessFilter set & no EndpointRef → snowplow-SA dispatch
		//   (the migration target: cyberjoker compositions discovery
		//   without cluster-wide RBAC). Endpoint comes from a callback so
		//   the projected SA token is re-read on every call.
		// - EndpointRef wins (operator escape hatch). The filter still
		//   applies to the response if userAccessFilter is set.
		// - Otherwise: the original mapper handles the user-secret /
		//   internal-clientconfig lookup unchanged.
		var ep endpoints.Endpoint
		if apiCall.UserAccessFilter != nil && apiCall.EndpointRef == nil {
			// Resolve the snowplow-SA endpoint provider. opts.SnowplowEndpoint
			// (set explicitly by the dispatcher constructors per Q-RBACC-IMPL-7)
			// wins; otherwise fall back to the context-stored provider that
			// main.go installs once via the withSnowplowEndpoint middleware.
			// The fallback path keeps the widget→apiref→l1cache→restactions
			// chain working without per-dispatcher threading.
			snowplowEndpointFn := opts.SnowplowEndpoint
			if snowplowEndpointFn == nil {
				if anyFn := cache.SnowplowEndpointFromContext(ctx); anyFn != nil {
					snowplowEndpointFn = func() (*endpoints.Endpoint, error) {
						v, err := anyFn()
						if err != nil {
							return nil, err
						}
						ep, ok := v.(*endpoints.Endpoint)
						if !ok || ep == nil {
							return nil, fmt.Errorf("snowplow endpoint context value type %T is not *endpoints.Endpoint", v)
						}
						return ep, nil
					}
				}
			}
			if snowplowEndpointFn == nil {
				log.Error("userAccessFilter set but no snowplow endpoint configured",
					slog.String("name", id))
				return false
			}
			snEp, snErr := snowplowEndpointFn()
			if snErr != nil || snEp == nil {
				log.Error("failed to obtain snowplow endpoint",
					slog.String("name", id), slog.Any("err", snErr))
				return false
			}
			ep = *snEp
		} else {
			var err error
			ep, err = mapper.resolveOne(ctx, apiCall.EndpointRef)
			if err != nil {
				log.Error("unable to resolve api endpoint reference",
					slog.String("name", id), slog.Any("ref", apiCall.EndpointRef), slog.Any("error", err))
				return false
			}
		}
		if opts.Verbose {
			ep.Debug = opts.Verbose
		}
		log.Debug("resolved endpoint for api call",
			slog.String("name", id), slog.String("host", ep.ServerURL))

		// createRequestOptions reads from dict — take a snapshot under lock
		// so concurrent writers in this level don't cause a data race.
		var tmp []httpcall.RequestOptions
		instrumentedDictWrite(ctx, dictMu, dict, "snapshot", "read",
			[]attribute.KeyValue{
				attribute.String("restaction.api.name", id),
			},
			func() {
				tmp = createRequestOptions(log, apiCall, dict)
			})

		if len(tmp) == 0 {
			log.Warn("empty request options for http call", slog.Any("name", id))
			return true
		}

		// ── Parallel execution of iterator calls ──────────────────────────────
		// When a single API entry fans out into N calls via an iterator (e.g.
		// one call per namespace), run them concurrently. dictMu serialises
		// all writes to the shared dict so that JQ handlers can safely append.
		//
		// For single-call entries (the common case) we skip goroutine overhead.
		//
		// V0_HOIST: snapshot dict["slice"] once at start of runOne under
		// brief lock so jsonHandler*Compute can be called outside the lock
		// without racing with sibling iterator writers (which may grow the
		// map and trigger concurrent-map-read panics on go runtime). Slice
		// is set exactly once at the top of Resolve and is never mutated by
		// the fanout, so a single snapshot is correct.
		runOne := func(call httpcall.RequestOptions, mu *sync.Mutex) (continueOnErr bool) {
			var sliceSnap any
			if mu != nil {
				mu.Lock()
				if si, ok := dict["slice"]; ok {
					sliceSnap = si
				}
				mu.Unlock()
			} else if si, ok := dict["slice"]; ok {
				sliceSnap = si
			}
			call.Endpoint = &ep
			verb := strings.ToUpper(ptr.Deref(call.Verb, http.MethodGet))

			// Record the GVR and per-resource dependency for targeted invalidation
			// and register the GVR for informer watching so the store gets populated.
			// RESTAction paths can be either K8s API paths (/apis/group/version/...)
			// or snowplow /call paths (/call?resource=...&apiVersion=...).
			// Record the path for the apiRequests list in the resolved output.
			apiRequestsMu.Lock()
			apiRequests = append(apiRequests, call.Path)
			apiRequestsMu.Unlock()

			if pathGVR, pathNS, pathName := cache.ParseK8sAPIPath(call.Path); pathGVR.Resource != "" {
				if tracker := cache.TrackerFromContext(ctx); tracker != nil {
					tracker.AddGVR(pathGVR)
					tracker.AddResource(pathGVR, pathNS, pathName)
				}
				if c != nil {
					_ = c.SAddGVR(ctx, pathGVR)
				}

				// ── Cache intercept for K8s API GET calls ─────────────────
				// Read from informer store (in-memory, zero I/O, zero-copy).
				if c != nil && verb == http.MethodGet {
					// L1 cache for K8s API results. Each API call within a
					// RESTAction is cached per-user so subsequent pages (or
					// resolves) reuse the cached result. Only the changed
					// namespace re-resolves from the informer.
					dirtySet := cache.DirtySetFromContext(ctx)
					gvrKey := cache.GVRToKey(pathGVR)
					if c != nil && (dirtySet == nil || !dirtySet.ShouldBypassAPIResult(gvrKey, pathNS)) {
						apiCacheKey := cache.APIResultKey(identity, pathGVR, pathNS, pathName)
						if raw, hit, _ := c.GetRaw(ctx, apiCacheKey); hit {
							rbacVerb := "list"
							if pathName != "" {
								rbacVerb = "get"
							}
							if rbac.UserCan(ctx, rbac.UserCanOptions{
								Verb:          rbacVerb,
								GroupResource: schema.GroupResource{Group: pathGVR.Group, Resource: pathGVR.Resource},
								Namespace:     pathNS,
							}) {
								// V0_HOIST: at api_l1, raw bytes are already in
								// hand. Compute (Unmarshal + JQ) runs WITHOUT
								// dictMu; only the merge-into-dict step takes
								// the lock. gojq-purity-required: raw bytes
								// were just fetched from L1 — no informer
								// alias, safeCopyJSON not needed for raw byte
								// path (parsing into a fresh tree).
								handler := call.ResponseHandler
								if handler == nil {
									apiL1Extra := []attribute.KeyValue{
										attribute.String("restaction.api.name", id),
										attribute.String("api.gvr", pathGVR.String()),
										attribute.String("api.namespace", pathNS),
										attribute.Int("payload.bytes", len(raw)),
									}
									if pathName != "" {
										apiL1Extra = append(apiL1Extra,
											attribute.String("api.name", pathName))
									}
									handlerOpts := jsonHandlerOptions{
										key: id, out: dict, filter: apiCall.Filter,
									}
									// gojq-purity-required: raw is fresh bytes; safeCopyJSON not required for this path.
									computed, cerr := jsonHandlerCompute(ctx, handlerOpts, raw, sliceSnap)
									if cerr == nil {
										// Per-user RBAC drop on list-shaped responses.
										// No-op when apiCall.UserAccessFilter == nil.
										computed = applyUserAccessFilter(ctx, apiCall, computed)
										instrumentedDictWrite(ctx, mu, dict, "api_l1", "append",
											apiL1Extra,
											func() {
												mergeIntoDict(dict, id, computed)
											})
										return true // L1 API cache hit
									}
								} else {
									if herr := handler(io.NopCloser(bytes.NewReader(raw))); herr == nil {
										return true // L1 API cache hit
									}
								}
							}
						}
					}

					// L1 miss: read from informer, normalize, resolve, then
					// write to L1 API cache for subsequent pages/resolves.
					if ir := cache.InformerReaderFromContext(ctx); ir != nil {
						var directData any
						var directHit bool

						if pathName == "" {
							_, listSpan := apiHandlerTracer.Start(ctx, "restaction.api.informer_list",
								trace.WithAttributes(
									attribute.String("gvr", pathGVR.String()),
									attribute.String("ns", pathNS),
								))
							if items, ok := ir.ListObjects(pathGVR, pathNS); ok && len(items) > 0 {
								listSpan.SetAttributes(attribute.Int("items", len(items)))
								listSpan.End()
								// Informer objects are shared pointers (client-go contract:
								// read-only). Safe copy is made below via marshal+unmarshal
								// before passing to JQ which may mutate maps in-place.
								itemsList := make([]any, len(items))
								for i, item := range items {
									itemsList[i] = item.Object
								}
								directData = map[string]any{
									"apiVersion": "v1",
									"kind":       "List",
									"metadata":   map[string]any{},
									"items":      itemsList,
								}
								directHit = true
							} else if ok {
								listSpan.SetAttributes(attribute.Int("items", 0))
								listSpan.End()
								directData = map[string]any{
									"apiVersion": "v1",
									"kind":       "List",
									"metadata":   map[string]any{},
									"items":      []any{},
								}
								directHit = true
							} else {
								listSpan.End()
							}
						} else {
							if obj, ok := ir.GetObject(pathGVR, pathNS, pathName); ok {
								directData = obj.Object
								directHit = true
							}
						}

						if directHit {
							rbacVerb := "list"
							if pathName != "" {
								rbacVerb = "get"
							}
							if !rbac.UserCan(ctx, rbac.UserCanOptions{
								Verb:          rbacVerb,
								GroupResource: schema.GroupResource{Group: pathGVR.Group, Resource: pathGVR.Resource},
								Namespace:     pathNS,
							}) {
								directHit = false
							}
						}

						if directHit {
							// Marshal the informer data to bytes for the L1 cache
							// entry (raw bytes are the cache value below).
							raw, merr := json.Marshal(directData)
							if merr != nil {
								return false
							}
							// Independent copy for JQ which mutates maps in-place
							// (gojq's normalizeNumbers / deleteEmpty). safeCopyJSON
							// walks the tree directly — pprof at 50K identified the
							// previous json.Unmarshal round-trip as the dominant
							// allocator (~25% of total alloc, ~21 GB/s sustained,
							// ~32% gcAssistAlloc CPU). The walk also coerces every
							// numeric leaf to float64, which is required because
							// gojq.normalizeNumbers panics on int64 (the v0.25.283
							// regression). Unstructured trees from K8s informers
							// contain int64 fields (metadata.generation,
							// status.observedGeneration, etc.) so the coercion is
							// load-bearing, not cosmetic.
							safeData := safeCopyJSON(directData)

							handlerOpts := jsonHandlerOptions{
								key: id, out: dict, filter: apiCall.Filter,
							}
							directExtra := []attribute.KeyValue{
								attribute.String("restaction.api.name", id),
								attribute.String("api.gvr", pathGVR.String()),
								attribute.String("api.namespace", pathNS),
								attribute.Int("payload.bytes", len(raw)),
							}
							if pathName != "" {
								directExtra = append(directExtra,
									attribute.String("api.name", pathName))
							}
							// V0_HOIST: at informer_direct, JQ runs on safeData
							// (already safeCopyJSON-wrapped above) BEFORE
							// taking dictMu. Only mergeIntoDict is locked.
							// gojq-purity-required: safeData is the
							// safeCopyJSON output — independent of informer
							// storage, gojq is free to mutate it.
							if mu != nil {
								computed, cerr := jsonHandlerDirectCompute(ctx, handlerOpts, safeData, sliceSnap)
								if cerr == nil {
									// Per-user RBAC drop on list-shaped responses.
									// No-op when apiCall.UserAccessFilter == nil.
									computed = applyUserAccessFilter(ctx, apiCall, computed)
									instrumentedDictWrite(ctx, mu, dict, "informer_direct", "append",
										directExtra,
										func() {
											mergeIntoDict(dict, id, computed)
										})
									if c != nil {
										apiCacheKey := cache.APIResultKey(identity, pathGVR, pathNS, pathName)
										_ = c.SetAPIResultRaw(ctx, apiCacheKey, raw)
										gvrKey := cache.GVRToKey(pathGVR)
										depKey := cache.L1ResourceDepKey(gvrKey, pathNS, pathName)
										_ = c.SAddWithTTL(ctx, depKey, apiCacheKey, cache.ReverseIndexTTL)
										if pathName == "" {
											clusterDep := cache.L1ResourceDepKey(gvrKey, "", "")
											// Instrumentation: cluster-wide dep writer (resolve, mu!=nil).
											cache.GlobalMetrics.ClusterDepSAddByResolve.Add(1)
											if pathNS != "" {
												cache.GlobalMetrics.ClusterDepSAddByResolveNSList.Add(1)
											}
											cache.SAddClusterDepInstrumented(ctx, c, clusterDep, apiCacheKey, cache.ReverseIndexTTL)
										}
									}
									return true
								}
							} else {
								// mu==nil: single-fanout shape, already
								// lock-free. Hoist still applies for code
								// symmetry — no contention but identical
								// gojq-purity discipline.
								var herr error
								computed, cerr := jsonHandlerDirectCompute(ctx, handlerOpts, safeData, sliceSnap)
								if cerr == nil {
									// Per-user RBAC drop on list-shaped responses.
									// No-op when apiCall.UserAccessFilter == nil.
									computed = applyUserAccessFilter(ctx, apiCall, computed)
									instrumentedDictWrite(ctx, nil, dict, "informer_direct", "append",
										directExtra,
										func() {
											mergeIntoDict(dict, id, computed)
										})
								} else {
									herr = cerr
								}
								if herr == nil {
									if c != nil {
										apiCacheKey := cache.APIResultKey(identity, pathGVR, pathNS, pathName)
										_ = c.SetAPIResultRaw(ctx, apiCacheKey, raw)
										gvrKey := cache.GVRToKey(pathGVR)
										depKey := cache.L1ResourceDepKey(gvrKey, pathNS, pathName)
										_ = c.SAddWithTTL(ctx, depKey, apiCacheKey, cache.ReverseIndexTTL)
										if pathName == "" {
											clusterDep := cache.L1ResourceDepKey(gvrKey, "", "")
											// Instrumentation: cluster-wide dep writer (resolve, mu==nil).
											cache.GlobalMetrics.ClusterDepSAddByResolve.Add(1)
											if pathNS != "" {
												cache.GlobalMetrics.ClusterDepSAddByResolveNSList.Add(1)
											}
											cache.SAddClusterDepInstrumented(ctx, c, clusterDep, apiCacheKey, cache.ReverseIndexTTL)
										}
									}
									return true
								}
							}
						}
					}
				}
			} else if callGVR, callNS, callName := cache.ParseCallPath(call.Path); callGVR.Resource != "" {
				if tracker := cache.TrackerFromContext(ctx); tracker != nil {
					tracker.AddGVR(callGVR)
					tracker.AddResource(callGVR, callNS, callName)
				}
				if c != nil {
					_ = c.SAddGVR(ctx, callGVR)
				}
			}

			// ── /call path L1 lookup ────────────────────────────────────────
			// For nested /call paths (e.g. compositions-list calling
			// compositions-get-ns-and-crd), try L1 first. If L1 misses
			// and we're in a background refresh (DirtySet present), resolve
			// inline via CallResolver instead of HTTP round-trip back to
			// snowplow, which would timeout under high load.
			if c != nil && verb == http.MethodGet {
				if callGVR, callNS, callName := cache.ParseCallPath(call.Path); callGVR.Resource != "" && callName != "" {
					l1Key := cache.ResolvedKey(identity, callGVR, callNS, callName, 0, 0)
					callExtra := []attribute.KeyValue{
						attribute.String("restaction.api.name", id),
						attribute.String("api.gvr", callGVR.String()),
						attribute.String("api.namespace", callNS),
						attribute.String("api.name", callName),
					}
					if l1Raw, l1Hit, _ := c.GetRaw(ctx, l1Key); l1Hit {
						cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
						// V0_HOIST: gojq-purity-required: l1Raw is fresh
						// L1 bytes — parsed into a fresh tree, not aliasing informer.
						handlerOpts := jsonHandlerOptions{key: id, out: dict, filter: apiCall.Filter}
						computed, cerr := jsonHandlerCompute(ctx, handlerOpts, l1Raw, sliceSnap)
						if cerr == nil {
							// Per-user RBAC drop on list-shaped responses.
							// No-op when apiCall.UserAccessFilter == nil.
							computed = applyUserAccessFilter(ctx, apiCall, computed)
							instrumentedDictWrite(ctx, mu, dict, "call_l1", "append",
								callExtra,
								func() {
									mergeIntoDict(dict, id, computed)
								})
						}
						return true
					}

					// L1 miss: try inline resolution during background refresh.
					// This avoids the HTTP self-call that causes context timeout
					// at 50K scale when snowplow is under heavy load.
					if callResolver := cache.CallResolverFromContext(ctx); callResolver != nil {
						if ir := cache.InformerReaderFromContext(ctx); ir != nil {
							if obj, ok := ir.GetObject(callGVR, callNS, callName); ok {
								raw, rerr := callResolver(ctx, obj.Object, l1Key, opts.AuthnNS)
								if rerr == nil && len(raw) > 0 {
									// V0_HOIST: gojq-purity-required:
									// raw is freshly produced bytes from
									// callResolver (independent tree).
									handlerOpts := jsonHandlerOptions{key: id, out: dict, filter: apiCall.Filter}
									computed, cerr := jsonHandlerCompute(ctx, handlerOpts, raw, sliceSnap)
									if cerr == nil {
										// Per-user RBAC drop on list-shaped responses.
										// No-op when apiCall.UserAccessFilter == nil.
										computed = applyUserAccessFilter(ctx, apiCall, computed)
										instrumentedDictWrite(ctx, mu, dict, "call_inline", "append",
											callExtra,
											func() {
												mergeIntoDict(dict, id, computed)
											})
									}
									return true
								}
							}
						}
					}
				}
			}
			// ── Skip malformed K8s API paths ───────────────────────────────
			// No endpointRef means the call goes directly to the K8s API server.
			// If the resource type is missing (empty .plural from stale CRD
			// data), skip instead of hitting a guaranteed 404.
			if apiCall.EndpointRef == nil {
				if _, _, r := cache.ExtractAPIGVR(call.Path); r == "" {
					log.Warn("skipping K8s API call with missing resource type",
						slog.String("name", id), slog.String("path", call.Path))
					return call.ContinueOnError
				}
			}

			// ── Fallback → live HTTP call ──────────────────────────────────
			//
			// Re-parse call.Path to recover GVR / NS / Name for the dictMu
			// span attributes at the http_fallback and err_write sites below.
			// pathGVR / callGVR computed at the top of runOne are scoped to
			// their respective branches and not visible here. Try K8s-API
			// shape first; if that doesn't match, try the /call shape. Both
			// parsers are pure string ops and return empty Resource on
			// mismatch — non-Snowplow paths simply emit empty GVR attrs.
			fbGVR, fbNS, fbName := cache.ParseK8sAPIPath(call.Path)
			if fbGVR.Resource == "" {
				fbGVR, fbNS, fbName = cache.ParseCallPath(call.Path)
			}
			fallbackBaseExtra := func() []attribute.KeyValue {
				attrs := []attribute.KeyValue{
					attribute.String("restaction.api.name", id),
					attribute.String("api.gvr", fbGVR.String()),
					attribute.String("api.namespace", fbNS),
				}
				if fbName != "" {
					attrs = append(attrs, attribute.String("api.name", fbName))
				}
				return attrs
			}
			{
				handlerOpts := jsonHandlerOptions{
					key: id, out: dict, filter: apiCall.Filter,
				}
				call.ResponseHandler = func(r io.ReadCloser) error {
					data, rerr := io.ReadAll(r)
					if rerr != nil {
						return rerr
					}
					httpExtra := append(fallbackBaseExtra(),
						attribute.Int("payload.bytes", len(data)))
					// V0_HOIST: gojq-purity-required: data is freshly read
					// HTTP response bytes — parsed into a fresh tree.
					computed, cerr := jsonHandlerCompute(ctx, handlerOpts, data, sliceSnap)
					if cerr != nil {
						return cerr
					}
					// Per-user RBAC drop on list-shaped responses.
					// No-op when apiCall.UserAccessFilter == nil.
					computed = applyUserAccessFilter(ctx, apiCall, computed)
					instrumentedDictWrite(ctx, mu, dict, "http_fallback", "append",
						httpExtra,
						func() {
							mergeIntoDict(dict, id, computed)
						})
					return nil
				}
			}

			log.Debug("calling api", slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL), slog.String("path", call.Path))

			res := httpcall.Do(ctx, call)
			if res.Status == response.StatusFailure {
				log.Error("api call response failure", slog.String("name", id),
					slog.String("host", call.Endpoint.ServerURL), slog.String("path", call.Path),
					slog.String("error", res.Message))

				errMap, merr := response.AsMap(res)
				instrumentedDictWrite(ctx, mu, dict, "err_write", "error",
					fallbackBaseExtra(),
					func() {
						if merr == nil && len(errMap) > 0 {
							dict[call.ErrorKey] = errMap
						} else {
							dict[call.ErrorKey] = res.Message
						}
					})
				return call.ContinueOnError
			}

			log.Debug("api successfully resolved",
				slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL), slog.String("path", call.Path),
			)
			return true
		}

		if len(tmp) == 1 {
			// Single call: run inline to avoid goroutine overhead.
			// Still pass dictMu since we're inside a parallel level.
			if !runOne(tmp[0], dictMu) {
				return false
			}
		} else {
			// Iterator: N independent calls — run concurrently with bounded
			// fan-out. Without this cap, a single API that expands to N
			// per-namespace calls (e.g., 50 namespaces) spawned N goroutines.
			// At 50K compositions × 3 users prewarming, this contributed to
			// the 24K goroutine peak measured in v0.25.280. Mirrors the
			// pattern used at the level fan-out below.
			iterSem := make(chan struct{}, runtime.GOMAXPROCS(0))
			var wg sync.WaitGroup
			for _, call := range tmp {
				wg.Add(1)
				iterSem <- struct{}{}
				go func(c httpcall.RequestOptions) {
					defer wg.Done()
					defer func() { <-iterSem }()
					runOne(c, dictMu)
				}(call)
			}
			wg.Wait()
		}
		// ── End parallel execution ─────────────────────────────────────────────
		return true
	}

	// Bounded semaphore: concurrent API resolutions per level.
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))

	for levelIdx, level := range levels {
		if ctx.Err() != nil {
			log.Warn("context cancelled, aborting remaining levels",
				slog.Int("remaining", len(levels)-levelIdx))
			break
		}

		if len(level) == 1 {
			// Single API in this level: run inline, no goroutine overhead.
			var dictMu sync.Mutex
			if !resolveAPI(level[0], &dictMu) {
				break // fatal error — stop processing further levels
			}
			continue
		}

		// Multiple APIs in this level: run in parallel with bounded concurrency.
		var dictMu sync.Mutex
		var levelWg sync.WaitGroup
		// levelFailed tracks whether any non-continueOnError API failed.
		// We collect errors but don't stop sibling goroutines.
		var levelFailed int32 // atomic: 0 = ok, 1 = at least one fatal failure

		for _, id := range level {
			levelWg.Add(1)
			sem <- struct{}{} // acquire semaphore slot
			go func(apiID string) {
				defer levelWg.Done()
				defer func() { <-sem }() // release semaphore slot
				defer func() {
					if r := recover(); r != nil {
						log.Error("panic in parallel API resolution",
							slog.String("name", apiID), slog.Any("panic", r))
						atomic.StoreInt32(&levelFailed, 1)
					}
				}()
				if !resolveAPI(apiID, &dictMu) {
					atomic.StoreInt32(&levelFailed, 1)
				}
			}(id)
		}
		levelWg.Wait()

		if atomic.LoadInt32(&levelFailed) != 0 {
			log.Warn("at least one API in level failed, stopping resolution",
				slog.Int("level", levelIdx), slog.Any("apis", level))
			break
		}
	}

	// Store the collected API request paths in the resolved output.
	// Deduplicate to keep the list compact (iterator calls often share
	// the same path pattern across namespaces — we only need unique paths).
	if len(apiRequests) > 0 {
		seen := make(map[string]bool, len(apiRequests))
		unique := make([]any, 0, len(apiRequests))
		for _, p := range apiRequests {
			if !seen[p] {
				seen[p] = true
				unique = append(unique, p)
			}
		}
		dict["apiRequests"] = unique
	}

	return dict
}


