package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	xcontext "github.com/krateoplatformops/plumbing/context"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

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
	log.Info("pagination options", slog.Int("page", opts.Page), slog.Int("perPage", opts.PerPage))

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("unable to fetch user info from context", slog.Any("err", err))
		return map[string]any{}
	}

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

	log.Info("base dict for api resolver", slog.Any("dict", dict))

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
		if apiCall.Headers == nil {
			apiCall.Headers = []string{headerAcceptJSON}
		}

		if accessToken, _ := xcontext.AccessToken(ctx); accessToken != "" {
			if apiCall.EndpointRef == nil || ptr.Deref(apiCall.ExportJWT, false) {
				apiCall.Headers = append(apiCall.Headers,
					fmt.Sprintf("Authorization: Bearer %s", accessToken))
			}
		}

		// Resolve the endpoint
		ep, err := mapper.resolveOne(ctx, apiCall.EndpointRef)
		if err != nil {
			log.Error("unable to resolve api endpoint reference",
				slog.String("name", id), slog.Any("ref", apiCall.EndpointRef), slog.Any("error", err))
			return false
		}
		if opts.Verbose {
			ep.Debug = opts.Verbose
		}
		log.Debug("resolved endpoint for api call",
			slog.String("name", id), slog.String("host", ep.ServerURL))

		// createRequestOptions reads from dict — take a snapshot under lock
		// so concurrent writers in this level don't cause a data race.
		dictMu.Lock()
		tmp := createRequestOptions(log, apiCall, dict)
		dictMu.Unlock()

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
		runOne := func(call httpcall.RequestOptions, mu *sync.Mutex, prefetched map[string][]byte) (continueOnErr bool) {
			call.Endpoint = &ep
			verb := strings.ToUpper(ptr.Deref(call.Verb, http.MethodGet))

			// Record the GVR and per-resource dependency for targeted invalidation
			// and register the GVR for informer watching so L3 gets populated.
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

				// ── L3 cache intercept for K8s API GET calls ──────────────
				// If this is a read (GET) and we have a cache, try to serve
				// from L3 instead of hitting the K8s API. The informer keeps
				// L3 up-to-date via per-item index SETs + SetForGVR.
				if c != nil && verb == http.MethodGet {
					var raw []byte
					var hit bool

					if pathName == "" {
						// LIST operation: try per-item index assembly first (O(1) writes),
						// then fall back to monolithic blob (legacy).
						raw, hit, _ = c.AssembleListFromIndex(ctx, pathGVR, pathNS)
						if !hit {
							cacheKey := cache.ListKey(pathGVR, pathNS)
							raw, hit = prefetched[cacheKey]
							if !hit {
								raw, hit, _ = c.GetRaw(ctx, cacheKey)
							}
						}
					} else {
						// GET operation: direct key lookup.
						cacheKey := cache.GetKey(pathGVR, pathNS, pathName)
						raw, hit = prefetched[cacheKey]
						if !hit {
							raw, hit, _ = c.GetRaw(ctx, cacheKey)
						}
					}

					if hit {
						log.Debug("L3 cache hit for K8s API path",
							slog.String("name", id),
							slog.String("path", call.Path))
						// Feed the cached JSON through the response handler
						// as if it came from the K8s API.
						handler := call.ResponseHandler
						if handler == nil {
							handler = jsonHandler(ctx, jsonHandlerOptions{
								key: id, out: dict, filter: apiCall.Filter,
							})
							if mu != nil {
								origHandler := handler
								handler = func(r io.ReadCloser) error {
									data, rerr := io.ReadAll(r)
									if rerr != nil {
										return rerr
									}
									mu.Lock()
									defer mu.Unlock()
									return origHandler(io.NopCloser(bytes.NewReader(data)))
								}
							}
						}
						if herr := handler(io.NopCloser(bytes.NewReader(raw))); herr == nil {
							return true // L3 cache hit — skip httpcall.Do
						}
						log.Debug("L3 cache hit but handler failed, falling through to API",
							slog.String("name", id),
							slog.String("path", call.Path))
					} else {
						log.Debug("L3 cache miss for K8s API path",
							slog.String("name", id),
							slog.String("path", call.Path))
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

			// ── L3 direct read ──────────────────────────────────────────────
			if c != nil && verb == http.MethodGet {
				pathGVR, pathNS, pathName := cache.ParseK8sAPIPath(call.Path)
				if pathGVR.Resource != "" {
					var l3Raw []byte
					var l3Hit bool
					if pathName != "" {
						l3Key := cache.GetKey(pathGVR, pathNS, pathName)
						l3Raw, l3Hit = prefetched[l3Key]
						if !l3Hit {
							l3Raw, l3Hit, _ = c.GetRaw(ctx, l3Key)
						}
					} else {
						// LIST: try per-item index assembly first, then blob.
						l3Raw, l3Hit, _ = c.AssembleListFromIndex(ctx, pathGVR, pathNS)
						if !l3Hit {
							l3Key := cache.ListKey(pathGVR, pathNS)
							l3Raw, l3Hit = prefetched[l3Key]
							if !l3Hit {
								l3Raw, l3Hit, _ = c.GetRaw(ctx, l3Key)
							}
						}
					}
					if l3Hit && !cache.IsNotFoundRaw(l3Raw) {
						rbacVerb := "list"
						if pathName != "" {
							rbacVerb = "get"
						}
						if rbac.UserCan(ctx, rbac.UserCanOptions{
							Verb:          rbacVerb,
							GroupResource: schema.GroupResource{Group: pathGVR.Group, Resource: pathGVR.Resource},
							Namespace:     pathNS,
						}) {
							cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L3Promotions, "l3_promotions")
							handler := jsonHandler(ctx, jsonHandlerOptions{key: id, out: dict, filter: apiCall.Filter})
							mu.Lock()
							_ = handler(io.NopCloser(bytes.NewReader(l3Raw)))
							mu.Unlock()
							return true
						}
					}
				}

				if callGVR, callNS, callName := cache.ParseCallPath(call.Path); callGVR.Resource != "" && callName != "" {
					l1Key := cache.ResolvedKey(user.Username, callGVR, callNS, callName, 0, 0)
					if l1Raw, l1Hit, _ := c.GetRaw(ctx, l1Key); l1Hit {
						cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
						handler := jsonHandler(ctx, jsonHandlerOptions{key: id, out: dict, filter: apiCall.Filter})
						mu.Lock()
						_ = handler(io.NopCloser(bytes.NewReader(l1Raw)))
						mu.Unlock()
						return true
					}
				}
			}
			// ── L3 miss → live HTTP call ───────────────────────────────────
			{
				plain := jsonHandler(ctx, jsonHandlerOptions{
					key: id, out: dict, filter: apiCall.Filter,
				})
				call.ResponseHandler = func(r io.ReadCloser) error {
					data, rerr := io.ReadAll(r)
					if rerr != nil {
						return rerr
					}
					mu.Lock()
					defer mu.Unlock()
					return plain(io.NopCloser(bytes.NewReader(data)))
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
				mu.Lock()
				if merr == nil && len(errMap) > 0 {
					dict[call.ErrorKey] = errMap
				} else {
					dict[call.ErrorKey] = res.Message
				}
				mu.Unlock()
				return call.ContinueOnError
			}

			log.Info("api successfully resolved",
				slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL), slog.String("path", call.Path),
			)
			return true
		}

		if len(tmp) == 1 {
			// Single call: run inline to avoid goroutine overhead.
			// Still pass dictMu since we're inside a parallel level.
			if !runOne(tmp[0], dictMu, nil) {
				return false
			}
		} else {
			// Iterator: N independent calls — run concurrently.
			// Pre-fetch all L3 cache keys in a single MGET to avoid 120+
			// sequential Redis round-trips (2-5ms each = 240-600ms).
			var prefetchedL3 map[string][]byte
			if c != nil {
				var l3Keys []string
				for _, call := range tmp {
					if pathGVR, pathNS, pathName := cache.ParseK8sAPIPath(call.Path); pathGVR.Resource != "" {
						if pathName == "" {
							l3Keys = append(l3Keys, cache.ListKey(pathGVR, pathNS))
						} else {
							l3Keys = append(l3Keys, cache.GetKey(pathGVR, pathNS, pathName))
						}
					}
				}
				if len(l3Keys) > 0 {
					prefetchedL3 = c.GetRawMulti(ctx, l3Keys)
				}
			}
			var wg sync.WaitGroup
			for _, call := range tmp {
				wg.Add(1)
				go func(c httpcall.RequestOptions) {
					defer wg.Done()
					runOne(c, dictMu, prefetchedL3)
				}(call)
			}
			wg.Wait()
		}
		// ── End parallel execution ─────────────────────────────────────────────
		return true
	}

	// Bounded semaphore: max 20 concurrent API resolutions per level.
	sem := make(chan struct{}, 20)

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

	removeManagedFields(dict)
	//delete(dict, "slice")

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

func removeManagedFields(data any) {
	switch v := data.(type) {
	case map[string]any:
		delete(v, "managedFields")
		// scansiona tutte le altre chiavi
		for _, val := range v {
			removeManagedFields(val)
		}
	case []any:
		for _, elem := range v {
			removeManagedFields(elem)
		}
	// other types (string, int, ecc.) -> do nothing
	default:
		return
	}
}
