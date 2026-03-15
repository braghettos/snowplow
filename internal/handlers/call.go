package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func Call() http.Handler {
	return &callHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
	}
}

var _ http.Handler = (*callHandler)(nil)

type callHandler struct {
	authnNS string
	verbose bool
}

// @Summary Call Endpoint
// @Description Handle Resources
// @ID call
// @Param  apiVersion       query   string  true  "Resource API Group and Version"
// @Param  resource         query   string  true  "Resource Plural"
// @Param  name             query   string  true  "Resource name"
// @Param  namespace        query   string  true  "Resource namespace"
// @Param  page             query   string  false "Pagination desired page"
// @Param  perPage          query   string  false "Pagination desired per page items"
// @Param  extras           query   string  false "JSON encoded map of extra params"
// @Param data body string false "Object"
// @Produce  json
// @Success 200 {object} map[string]any
// @Failure 400 {object} response.Status
// @Failure 401 {object} response.Status
// @Failure 404 {object} response.Status
// @Failure 500 {object} response.Status
// @Router /call [get]
// @Router /call [post]
// @Router /call [put]
// @Router /call [patch]
// @Router /call [delete]
func (r *callHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	opts, err := r.validateRequest(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}

	uri, err := buildURIPath(opts)
	if err != nil {
		response.InternalError(wri, err)
		return
	}

	log := xcontext.Logger(req.Context())
	start := time.Now()

	ep, err := xcontext.UserConfig(req.Context())
	if err != nil {
		log.Error("unable to get user endpoint", slog.Any("err", err))
		response.Unauthorized(wri, err)
		return
	}
	ep.Debug = r.verbose

	log.Debug("user config succesfully loaded", slog.Any("endpoint", ep))

	c := cache.FromContext(req.Context())

	// Only cache GET (read-only) requests.
	if strings.ToUpper(opts.verb) == http.MethodGet {
		cacheKey := callCacheKey(opts)
		if c != nil {
			// Negative cache check: sentinel stored by SetNotFound means a prior
			// K8s lookup returned 404. Serve the 404 from cache and track it.
			if c.GetNotFound(req.Context(), cacheKey) {
				cache.GlobalMetrics.NegativeHits.Add(1)
				response.NotFound(wri, fmt.Errorf("resource not found (cached)"))
				return
			}
			// Positive cache check.
			if raw, hit, rerr := c.GetRaw(req.Context(), cacheKey); hit && rerr == nil {
				cache.GlobalMetrics.RawHits.Add(1)
				log.Debug("cache hit", slog.String("key", cacheKey))
				wri.Header().Set("Content-Type", "application/json")
				wri.WriteHeader(http.StatusOK)
				_, _ = wri.Write(raw)
				return
			}
			cache.GlobalMetrics.RawMisses.Add(1)
		}
	}

	dict := map[string]any{}
	callOpts := request.RequestOptions{
		RequestInfo: request.RequestInfo{
			Path: uri,
			Verb: ptr.To(strings.ToUpper(opts.verb)),
			Headers: []string{
				"Accept: application/json",
			},
		},
		Endpoint:        &ep,
		ResponseHandler: callResponseHandler(dict),
	}
	if opts.dat != nil && has([]string{http.MethodPost, http.MethodPut, http.MethodPatch}, opts.verb) {
		callOpts.Headers = append(callOpts.Headers,
			fmt.Sprintf("Content-Type: %s", opts.contentType),
		)
		callOpts.Payload = ptr.To(string(opts.dat))
	}

	rt := request.Do(req.Context(), callOpts)
	if rt.Status == response.StatusFailure {
		if rt.Code == http.StatusNotFound && strings.ToUpper(opts.verb) == http.MethodGet && c != nil {
			_ = c.SetNotFound(req.Context(), callCacheKey(opts))
		}
		log.Error("unable to call endpoint",
			slog.String("verb", strings.ToUpper(opts.verb)),
			slog.String("uri", uri),
			slog.String("err", rt.Message))
		response.Encode(wri, rt)
		return
	}

	log.Info("endpoint call done",
		slog.String("verb", strings.ToUpper(opts.verb)),
		slog.String("uri", uri),
		slog.String("duration", util.ETA(start)),
	)

	// Cache successful GET responses, but only if the key is not already present.
	// The ResourceWatcher may have populated (or freshly updated) the key while
	// we were waiting for the K8s API response, so we must not overwrite it with
	// potentially stale bytes fetched before the mutation was visible.
	// Additionally, auto-register every accessed GVR for dynamic informer
	// watching so that no GVR needs to be manually listed in cache-warmup.yaml
	// just to benefit from cache invalidation on mutations.
	if strings.ToUpper(opts.verb) == http.MethodGet && c != nil && len(dict) > 0 {
		ckey := callCacheKey(opts)
		if !c.Exists(req.Context(), ckey) {
			if raw, merr := json.Marshal(dict); merr == nil {
				_ = c.SetRaw(req.Context(), ckey, raw)
			}
		}
		// SAddGVR is a no-op if the GVR is already registered; the onNewGVR
		// callback starts a dynamic informer only on first registration.
		_ = c.SAddGVR(req.Context(), opts.gvr)
	}

	// Invalidate cache for mutating operations.
	if has([]string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, opts.verb) && c != nil {
		getKey := cache.GetKey(opts.gvr, opts.nsn.Namespace, opts.nsn.Name)
		listKey := cache.ListKey(opts.gvr, opts.nsn.Namespace)
		_ = c.Delete(req.Context(), getKey, listKey, cache.ListKey(opts.gvr, ""))
		slog.Debug("cache invalidated after mutation",
			slog.String("verb", opts.verb), slog.String("getKey", getKey))
	}

	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(wri)
	enc.SetIndent("", "  ")
	if err := enc.Encode(dict); err != nil {
		log.Error("unable to serve api call response", slog.Any("err", err))
	}
}

// callCacheKey returns the cache key for the given call options.
// GET requests for a named resource use the GET key format so they are
// consistent with the keys populated by the warmup and the ResourceWatcher.
// LIST requests (no name) use the LIST key format for the same reason.
func callCacheKey(opts callOptions) string {
	if opts.nsn.Name == "" {
		return cache.ListKey(opts.gvr, opts.nsn.Namespace)
	}
	return cache.GetKey(opts.gvr, opts.nsn.Namespace, opts.nsn.Name)
}

func (r *callHandler) validateRequest(req *http.Request) (opts callOptions, err error) {
	opts.verb = req.Method
	if has([]string{http.MethodPost, http.MethodPut, http.MethodPatch}, opts.verb) {
		opts.contentType = req.Header.Get("Content-type")
		if opts.contentType == "" {
			opts.contentType = "application/json"
		}
	}

	opts.gvr, err = util.ParseGVR(req)
	if err != nil {
		return
	}

	opts.nsn, err = util.ParseNamespacedName(req)
	if err != nil {
		return
	}

	if val := req.URL.Query().Get("perPage"); val != "" {
		opts.perPage, err = strconv.Atoi(val)
		if err != nil {
			return
		}
	}

	if val := req.URL.Query().Get("page"); val != "" {
		opts.page, err = strconv.Atoi(val)
		if err != nil {
			return
		}
	}

	if req.Body != nil {
		opts.dat, err = io.ReadAll(io.LimitReader(req.Body, 1048576))
		if err != nil {
			return
		}
	}

	return
}

type callOptions struct {
	gvr         schema.GroupVersionResource
	nsn         types.NamespacedName
	verb        string
	contentType string
	perPage     int
	page        int
	dat         []byte
}

func buildURIPath(opts callOptions) (string, error) {
	base := path.Join("/apis", opts.gvr.Group, opts.gvr.Version)
	if len(opts.gvr.Group) == 0 {
		base = path.Join("/api", opts.gvr.Version)
	}

	uriPath := path.Join(base, "namespaces", opts.nsn.Namespace, opts.gvr.Resource)
	if strings.EqualFold("namespaces", opts.gvr.Resource) {
		uriPath = path.Join(base, opts.gvr.Resource)
	}

	if has([]string{
		http.MethodDelete,
		http.MethodGet,
		http.MethodPut,
		http.MethodPatch,
	}, opts.verb) {
		uriPath = path.Join(uriPath, opts.nsn.Name)
	}

	query := url.Values{}
	if opts.perPage > 0 {
		query.Set("perPage", strconv.Itoa(opts.perPage))
	}
	if opts.page > 0 {
		query.Set("page", strconv.Itoa(opts.page))
	}

	if len(query) > 0 {
		uriPath += "?" + query.Encode()
	}

	return uriPath, nil
}

func has(s []string, e string) bool {
	for _, a := range s {
		if strings.EqualFold(a, e) {
			return true
		}
	}
	return false
}

func callResponseHandler(out map[string]any) func(io.ReadCloser) error {
	return func(in io.ReadCloser) error {
		dat, err := io.ReadAll(in)
		if err != nil {
			return err
		}

		x := bytes.TrimSpace(dat)
		isArray := len(x) > 0 && x[0] == '['

		if isArray {
			v := []any{}
			err := json.Unmarshal(dat, &v)
			if err != nil {
				return err
			}
			out["items"] = v
			return nil
		}

		return json.Unmarshal(dat, &out)
	}
}
