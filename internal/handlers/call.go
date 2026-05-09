package handlers

import (
	"bytes"
	"context"
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
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/httpcall"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	// Q-CAUSAL-COST (0.25.323) — wrap wri so the deferred block can classify
	// status=0 reports as client-gone-before-WriteHeader vs after vs Write
	// error. Sits above gzip middleware so bytesWritten is the uncompressed
	// payload the handler emitted. No behavior change.
	rec := newStatusRecorder(wri)
	wri = rec
	handlerStart := time.Now()
	defer logCallDone(req, rec, handlerStart)

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

	// Register every accessed GVR for dynamic informer watching as early as
	// possible. This ensures the informer is started before the K8s API call
	// returns, minimizing the window where a mutation could be missed.
	if c != nil {
		_ = c.SAddGVR(req.Context(), opts.gvr)
	}

	// Only cache GET (read-only) requests.
	if strings.ToUpper(opts.verb) == http.MethodGet {
		cacheKey := callCacheKey(opts)
		if c != nil {
			// Negative cache check: sentinel stored by SetNotFound means a prior
			// K8s lookup returned 404. Serve the 404 from cache and track it.
			if c.GetNotFound(req.Context(), cacheKey) {
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.NegativeHits, "negative_hits")
				response.NotFound(wri, fmt.Errorf("resource not found (cached)"))
				return
			}
			// Q-MIRROR-REMOVAL (0.25.316): the snowplow:get:* mirror is gone.
			// Serve the GET cache hit directly from the informer's in-memory
			// store. ListObjects already returns a stripped, transform-applied
			// view of every watched object — exactly what the legacy mirror
			// held — so we marshal once at request time. Marshal cost is on
			// the order of microseconds for a single object; the savings (no
			// per-event 1:1 mirror write) are 5-6 GiB at bench.
			if raw, hit := callTryServeFromInformer(req.Context(), opts, log); hit {
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.CallHits, "call_hits")
				wri.Header().Set("Content-Type", "application/json")
				wri.WriteHeader(http.StatusOK)
				_, _ = wri.Write(raw)
				return
			}
			// Positive cache check (LIST path or first-fetch GET fall-through).
			// The LIST path (no name) still uses AssembleListFromIndex which
			// itself reads from the informer post-mirror-removal. Single-name
			// GETs that miss the informer (informer not yet synced for this
			// GVR, or RBAC redirected to a different namespace) fall through
			// to the K8s API call below, then write SetRaw on success.
			if raw, hit, rerr := c.GetRaw(req.Context(), cacheKey); hit && rerr == nil {
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.CallHits, "call_hits")
				log.Debug("call: cache hit (kv)", slog.String("key", cacheKey))
				wri.Header().Set("Content-Type", "application/json")
				wri.WriteHeader(http.StatusOK)
				_, _ = wri.Write(raw)
				return
			}
			cache.GlobalMetrics.Inc(&cache.GlobalMetrics.CallMisses, "call_misses")
			log.Info("call: cache miss", slog.String("key", cacheKey), slog.String("verb", opts.verb), slog.String("gvr", cache.GVRToKey(opts.gvr)))
		}
	}

	dict := map[string]any{}
	callOpts := httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{
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

	rt := httpcall.Do(req.Context(), callOpts)
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
	if strings.ToUpper(opts.verb) == http.MethodGet && c != nil && len(dict) > 0 {
		ckey := callCacheKey(opts)
		if !c.Exists(req.Context(), ckey) {
			if raw, merr := json.Marshal(dict); merr == nil {
				_ = c.SetRaw(req.Context(), ckey, raw)
			}
		}
	}

	// Invalidate cache for mutating operations using targeted GVR-based
	// invalidation instead of bulk-wiping all resolved/http entries.
	if has([]string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, opts.verb) && c != nil {
		getKey := cache.GetKey(opts.gvr, opts.nsn.Namespace, opts.nsn.Name)
		listKey := cache.ListKey(opts.gvr, opts.nsn.Namespace)
		listIdxKey := cache.ListIndexKey(opts.gvr, opts.nsn.Namespace)
		_ = c.Delete(req.Context(), getKey, listKey, cache.ListKey(opts.gvr, ""), listIdxKey, cache.ListIndexKey(opts.gvr, ""))

		// L1 resolved keys are NOT deleted here. The informer DELETE event
		// marks dependencies dirty via the background refresh pipeline,
		// which re-resolves affected widgets with the updated informer
		// snapshot (stale-while-refresh for the cascade chain).

		slog.Debug("cache invalidated after mutation",
			slog.String("verb", opts.verb), slog.String("gvr", cache.GVRToKey(opts.gvr)))
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

// callTryServeFromInformer attempts to satisfy a GET (named resource) or LIST
// (no name) directly from the informer's in-memory store, marshaling once at
// request time. Returns (bytes, true) on hit. Returns (nil, false) when:
//   - no InformerReader is in ctx (e.g. unit tests without ResourceWatcher),
//   - the GVR has no registered informer yet (registered just-in-time via
//     SAddGVR above; first request races sync — fall through to K8s API),
//   - the named object is absent (callHandler issues the K8s GET so the
//     negative-cache sentinel fires correctly on a real 404).
//
// Q-MIRROR-REMOVAL (0.25.316): replaces the snowplow:get:* mirror reads.
func callTryServeFromInformer(ctx context.Context, opts callOptions, log *slog.Logger) ([]byte, bool) {
	ir := cache.InformerReaderFromContext(ctx)
	if ir == nil {
		return nil, false
	}

	// LIST path (no name): assemble UnstructuredList from informer store.
	if opts.nsn.Name == "" {
		objs, ok := ir.ListObjects(opts.gvr, opts.nsn.Namespace)
		if !ok {
			return nil, false
		}
		raw, err := marshalUnstructuredList(opts.gvr, objs)
		if err != nil {
			log.Debug("call: informer list marshal failed",
				slog.String("gvr", cache.GVRToKey(opts.gvr)),
				slog.Any("err", err))
			return nil, false
		}
		log.Debug("call: cache hit (informer-list)",
			slog.String("gvr", cache.GVRToKey(opts.gvr)),
			slog.Int("items", len(objs)))
		return raw, true
	}

	// GET path: single named object.
	uns, ok := ir.GetObject(opts.gvr, opts.nsn.Namespace, opts.nsn.Name)
	if !ok || uns == nil {
		return nil, false
	}
	raw, err := json.Marshal(uns.Object)
	if err != nil {
		log.Debug("call: informer object marshal failed",
			slog.String("gvr", cache.GVRToKey(opts.gvr)),
			slog.String("ns", opts.nsn.Namespace),
			slog.String("name", opts.nsn.Name),
			slog.Any("err", err))
		return nil, false
	}
	log.Debug("call: cache hit (informer-get)",
		slog.String("gvr", cache.GVRToKey(opts.gvr)),
		slog.String("ns", opts.nsn.Namespace),
		slog.String("name", opts.nsn.Name))
	return raw, true
}

// marshalUnstructuredList builds a Kubernetes-shaped UnstructuredList JSON
// from a slice of *unstructured.Unstructured. Mirrors the wire format that
// memcache.AssembleListFromIndex used to produce, so existing list consumers
// (frontend, RESTAction iterators) see no shape change.
func marshalUnstructuredList(gvr schema.GroupVersionResource, items []*unstructured.Unstructured) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"apiVersion":"`)
	if gvr.Group == "" {
		buf.WriteString(gvr.Version)
	} else {
		buf.WriteString(gvr.Group)
		buf.WriteByte('/')
		buf.WriteString(gvr.Version)
	}
	buf.WriteString(`","kind":"List","metadata":{"resourceVersion":""},"items":[`)
	for i, uns := range items {
		if uns == nil {
			continue
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		raw, err := json.Marshal(uns.Object)
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
	}
	buf.WriteString(`]}`)
	return buf.Bytes(), nil
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

// logCallDone is the deferred Q-CAUSAL-COST exit hook for callHandler. It
// reads ctx.Err / context.Cause once, increments the three call_events
// counters on the matching edges, and emits the per-call audit line at INFO.
func logCallDone(req *http.Request, rec *statusRecorder, start time.Time) {
	ctx := req.Context()
	ctxErr := ctx.Err()
	var causeStr, ctxErrStr, writeErrStr string
	if ctxErr != nil {
		ctxErrStr = ctxErr.Error()
		if cause := context.Cause(ctx); cause != nil && cause != ctxErr {
			causeStr = cause.Error()
		}
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.CallClientGone, "call_client_gone")
		if rec.headerStatus != 0 {
			cache.GlobalMetrics.Inc(&cache.GlobalMetrics.CallClientGoneAfterWriteHeader, "call_client_gone_after_write_header")
		}
	}
	if rec.writeErr != nil {
		writeErrStr = rec.writeErr.Error()
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.CallWriteError, "call_write_error")
	}
	slog.Info("call.done",
		slog.String("url", req.URL.String()),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.Int("status", rec.headerStatus),
		slog.Int64("bytes", rec.bytesWritten),
		slog.String("write_err", writeErrStr),
		slog.String("ctx_err", ctxErrStr),
		slog.String("cause", causeStr),
	)
}

// statusRecorder is a transparent http.ResponseWriter wrapper. Captures
// status code, cumulative bytes written, and the first non-nil Write error.
// Pass-through only — no behavior change. On implicit-WriteHeader (Write
// without prior WriteHeader) sets headerStatus=200 to mirror net/http.
type statusRecorder struct {
	http.ResponseWriter
	headerStatus int
	bytesWritten int64
	writeErr     error
	wroteHeader  bool
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w}
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.headerStatus = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// Mirror net/http's implicit 200-on-first-Write contract.
		s.headerStatus = http.StatusOK
		s.wroteHeader = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytesWritten += int64(n)
	if err != nil && s.writeErr == nil {
		s.writeErr = err
	}
	return n, err
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
