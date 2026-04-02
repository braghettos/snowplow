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

	for _, id := range names {
		// Get the api with this identifier
		apiCall, ok := apiMap[id]
		if !ok {
			log.Warn("api not found in apiMap", slog.Any("name", id))
			continue
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
			return dict
		}
		if opts.Verbose {
			ep.Debug = opts.Verbose
		}
		log.Debug("resolved endpoint for api call",
			slog.String("name", id), slog.String("host", ep.ServerURL))

		tmp := createRequestOptions(log, apiCall, dict)

		if len(tmp) == 0 {
			log.Warn("empty request options for http call", slog.Any("name", id))
			continue
		}

		// ── Parallel execution of iterator calls ──────────────────────────────
		// When a single API entry fans out into N calls via an iterator (e.g.
		// one call per namespace), run them concurrently. A mutex serialises
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
				// L3 up-to-date via patchListCache / SetForGVR.
				if c != nil && verb == http.MethodGet {
					var cacheKey string
					if pathName == "" {
						cacheKey = cache.ListKey(pathGVR, pathNS)
					} else {
						cacheKey = cache.GetKey(pathGVR, pathNS, pathName)
					}
					// Check pre-fetched MGET results first (0 round-trips),
					// fall back to individual GetRaw (1 round-trip).
					raw, hit := prefetched[cacheKey]
					if !hit {
						raw, hit, _ = c.GetRaw(ctx, cacheKey)
					}
					if hit {
						log.Debug("L3 cache hit for K8s API path",
							slog.String("name", id),
							slog.String("path", call.Path),
							slog.String("cacheKey", cacheKey))
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
							slog.String("path", call.Path),
							slog.String("cacheKey", cacheKey))
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
					var l3Key string
					if pathName != "" {
						l3Key = cache.GetKey(pathGVR, pathNS, pathName)
					} else {
						l3Key = cache.ListKey(pathGVR, pathNS)
					}
					l3Raw, l3Hit := prefetched[l3Key]
					if !l3Hit {
						l3Raw, l3Hit, _ = c.GetRaw(ctx, l3Key)
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
							if mu != nil {
								mu.Lock()
							}
							_ = handler(io.NopCloser(bytes.NewReader(l3Raw)))
							if mu != nil {
								mu.Unlock()
							}
							return true
						}
					}
				}

				if callGVR, callNS, callName := cache.ParseCallPath(call.Path); callGVR.Resource != "" && callName != "" {
					l1Key := cache.ResolvedKey(user.Username, callGVR, callNS, callName, 0, 0)
					if l1Raw, l1Hit, _ := c.GetRaw(ctx, l1Key); l1Hit {
						cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
						handler := jsonHandler(ctx, jsonHandlerOptions{key: id, out: dict, filter: apiCall.Filter})
						if mu != nil {
							mu.Lock()
						}
						_ = handler(io.NopCloser(bytes.NewReader(l1Raw)))
						if mu != nil {
							mu.Unlock()
						}
						return true
					}
				}
			}
			// ── L3 miss → live HTTP call ───────────────────────────────────
			{
				plain := jsonHandler(ctx, jsonHandlerOptions{
					key: id, out: dict, filter: apiCall.Filter,
				})
				if mu != nil {
					call.ResponseHandler = func(r io.ReadCloser) error {
						data, rerr := io.ReadAll(r)
						if rerr != nil {
							return rerr
						}
						mu.Lock()
						defer mu.Unlock()
						return plain(io.NopCloser(bytes.NewReader(data)))
					}
				} else {
					call.ResponseHandler = plain
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
				if mu != nil {
					mu.Lock()
				}
				if merr == nil && len(errMap) > 0 {
					dict[call.ErrorKey] = errMap
				} else {
					dict[call.ErrorKey] = res.Message
				}
				if mu != nil {
					mu.Unlock()
				}
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
			if !runOne(tmp[0], nil, nil) {
				return dict
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
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, call := range tmp {
				wg.Add(1)
				go func(c httpcall.RequestOptions) {
					defer wg.Done()
					runOne(c, &mu, prefetchedL3)
				}(call)
			}
			wg.Wait()
		}
		// ── End parallel execution ─────────────────────────────────────────────
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
