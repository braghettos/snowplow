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

	// Sort API by Depends
	names, err := topologicalSort(opts.Items)
	if err != nil {
		log.Error("unable to sorted api by deps", slog.Any("error", err))
		return map[string]any{}
	}
	log.Debug("sorted api by deps", slog.Any("names", names))

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
		runOne := func(call httpcall.RequestOptions, mu *sync.Mutex) (continueOnErr bool) {
			call.Endpoint = &ep
			verb := strings.ToUpper(ptr.Deref(call.Verb, http.MethodGet))

			// Record the GVR dependency for targeted invalidation.
			if pathGVR, _, _ := cache.ParseK8sAPIPath(call.Path); pathGVR.Resource != "" {
				if tracker := cache.TrackerFromContext(ctx); tracker != nil {
					tracker.AddGVR(pathGVR)
				}
			}

			// ── HTTP response cache ────────────────────────────────────────────
			if c != nil && verb == http.MethodGet {
				httpKey := cache.HTTPUserKey(user.Username, verb, call.Path)

			if raw, hit, _ := c.GetRaw(ctx, httpKey); hit {
				handler := jsonHandler(ctx, jsonHandlerOptions{
					key: id, out: dict, filter: apiCall.Filter,
				})
				if tracker := cache.TrackerFromContext(ctx); tracker != nil {
					tracker.AddL2Key(httpKey)
				}
				cache.GlobalMetrics.RawHits.Add(1)
				log.Debug("api: L2 hit", slog.String("name", id), slog.String("path", call.Path))
				if mu != nil {
					mu.Lock()
				}
				herr := handler(io.NopCloser(bytes.NewReader(raw)))
				if mu != nil {
					mu.Unlock()
				}
				if herr != nil {
					log.Warn("api: cached response handler error",
						slog.String("key", httpKey), slog.Any("err", herr))
				}
				return true
			}

			// L2 miss → try L3 promotion: if the path maps to a K8s
			// resource that the warmup or informer already cached in L3,
			// serve from L3 and populate L2 — no API call needed.
			if pathGVR, pathNS, pathName := cache.ParseK8sAPIPath(call.Path); pathGVR.Resource != "" {
				var l3Key string
				if pathName != "" {
					l3Key = cache.GetKey(pathGVR, pathNS, pathName)
				} else {
					l3Key = cache.ListKey(pathGVR, pathNS)
				}
				l3Raw, l3Hit, _ := c.GetRaw(ctx, l3Key)

				// For per-namespace lists: if the per-NS key is missing but the
				// cluster-wide key exists, the GVR was warmed and this namespace
				// simply has no items → synthesize an empty list.
				if !l3Hit && pathName == "" && pathNS != "" {
					if c.Exists(ctx, cache.ListKey(pathGVR, "")) {
						l3Raw = []byte(`{"metadata":{},"items":[]}`)
						l3Hit = true
					}
				}

				if l3Hit && !cache.IsNotFoundRaw(l3Raw) {
					_ = c.SetHTTPRaw(ctx, httpKey, l3Raw)
					gvrKey := cache.GVRToKey(pathGVR)
					_ = c.SAddWithTTL(ctx, cache.L2GVRKey(gvrKey), httpKey, cache.DefaultResourceTTL)
					if tracker := cache.TrackerFromContext(ctx); tracker != nil {
						tracker.AddL2Key(httpKey)
					}
					cache.GlobalMetrics.RawHits.Add(1)
					cache.GlobalMetrics.L3Promotions.Add(1)
					handler := jsonHandler(ctx, jsonHandlerOptions{
						key: id, out: dict, filter: apiCall.Filter,
					})
					if mu != nil {
						mu.Lock()
					}
					herr := handler(io.NopCloser(bytes.NewReader(l3Raw)))
					if mu != nil {
						mu.Unlock()
					}
					if herr != nil {
						log.Warn("api: L3 promoted response handler error",
							slog.String("key", httpKey), slog.Any("err", herr))
					}
					return true
				}
			}

			// True L2+L3 miss → HTTP call.
			baseHandler := jsonHandler(ctx, jsonHandlerOptions{
				key: id, out: dict, filter: apiCall.Filter,
			})
			capturedKey := httpKey
			call.ResponseHandler = func(r io.ReadCloser) error {
				data, rerr := io.ReadAll(r)
				if rerr != nil {
					return rerr
				}
				_ = c.SetHTTPRaw(ctx, capturedKey, data)
				if pathGVR, _, _ := cache.ParseK8sAPIPath(call.Path); pathGVR.Resource != "" {
					_ = c.SAddWithTTL(ctx, cache.L2GVRKey(cache.GVRToKey(pathGVR)), capturedKey, cache.DefaultResourceTTL)
				}
				if tracker := cache.TrackerFromContext(ctx); tracker != nil {
					tracker.AddL2Key(capturedKey)
				}
			cache.GlobalMetrics.RawMisses.Add(1)
			log.Info("api: L2+L3 miss (live HTTP)", slog.String("name", id), slog.String("path", call.Path), slog.String("key", capturedKey))
			if mu != nil {
				mu.Lock()
			}
			werr := baseHandler(io.NopCloser(bytes.NewReader(data)))
			if mu != nil {
				mu.Unlock()
			}
			return werr
		}
		} else {
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
			// ── End HTTP response cache ────────────────────────────────────────

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
				slog.Any("depth", mapDepth(dict)),
			)
			return true
		}

		if len(tmp) == 1 {
			// Single call: run inline to avoid goroutine overhead.
			if !runOne(tmp[0], nil) {
				return dict
			}
		} else {
			// Iterator: N independent calls — run concurrently.
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, call := range tmp {
				wg.Add(1)
				go func(c httpcall.RequestOptions) {
					defer wg.Done()
					runOne(c, &mu)
				}(call)
			}
			wg.Wait()
		}
		// ── End parallel execution ─────────────────────────────────────────────
	}

	removeManagedFields(dict)
	//delete(dict, "slice")

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
