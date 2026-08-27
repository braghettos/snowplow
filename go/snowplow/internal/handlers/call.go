// Package handlers holds snowplow's top-level HTTP handlers. It exposes the
// primary /call endpoint (which dispatches resource requests to the
// per-GVR resolvers via the dispatchers proxy), the /health and /readyz
// probes, and supporting endpoints for listing, jq evaluation, conversion,
// and plural lookups.
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

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/plumbing/ptr"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/dynamic"
	"github.com/krateo-platformops/snowplow/internal/handlers/util"
	"github.com/krateo-platformops/snowplow/internal/support/audit"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func Call() http.Handler {
	return &callHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
		// Issue #156: scope resolution is consulted ONLY when a /call
		// request omits `namespace` (a 400 today). The default backs the
		// resolver with the process-wide SA discovery singleton
		// (dynamic.SharedSAScopeForGVR) — no rc threading through main.go's
		// ~6 Call() mounts. Tests inject a fake via CallWithScopeResolver /
		// the export_test seam so hermetic cases never touch a real mapper.
		scopeResolver: dynamic.SharedSAScopeForGVR,
	}
}

var _ http.Handler = (*callHandler)(nil)

// scopeResolverFn resolves whether a GVR is namespace-scoped. It returns
// namespaced=true for a namespaced resource, false for a cluster-scoped
// one, and a non-nil error when scope cannot be determined (unknown GVR,
// mapper not synced). The /call handler FAILS-CLOSED (400) on error — it
// NEVER guesses a scope on a write path.
type scopeResolverFn func(gvr schema.GroupVersionResource) (namespaced bool, err error)

type callHandler struct {
	authnNS string
	verbose bool
	// scopeResolver is consulted ONLY on the namespace-absent branch of
	// validateRequest (Issue #156). The namespace-present path is
	// byte-identical to pre-#156 behaviour and NEVER calls this — so a
	// cold/erroring resolver can never regress a currently-working
	// namespaced request.
	scopeResolver scopeResolverFn
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

	// Ship D (0.30.141) — F-1: handlers.Call() is the dispatcher's
	// fallthrough lane for GVR groups not in the
	// dispatchers.All() map (every "raw apiserver passthrough" /call).
	// Record BEFORE request.Do so a panicking plumbing call still
	// counts (AC-D.3 ordering).
	cache.RecordApiserverFallthrough(req.Context(), cache.ReasonClientBuild, "")
	rt := request.Do(req.Context(), callOpts)

	// Audit correlation: every WRITE through /call emits a
	// normalized AuditEvent carrying the request correlation id (see
	// internal/support/audit) so a portal action is linkable to the
	// object it mutated. Reads are deliberately not audited here (volume;
	// they are already covered by the request log + trace id).
	if has([]string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, opts.verb) {
		outcome, code, msg := "success", http.StatusOK, ""
		if rt.Status == response.StatusFailure {
			outcome, code, msg = "failure", rt.Code, rt.Message
		}
		audit.Emit(req.Context(), audit.Event{
			Action:    "call",
			Verb:      strings.ToUpper(opts.verb),
			Group:     opts.gvr.Group,
			Version:   opts.gvr.Version,
			Resource:  opts.gvr.Resource,
			Name:      opts.nsn.Name,
			Namespace: opts.nsn.Namespace,
			Outcome:   outcome,
			Code:      code,
			Message:   msg,
		})
	}

	if rt.Status == response.StatusFailure {
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

	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(wri)
	enc.SetIndent("", "  ")
	if err := enc.Encode(dict); err != nil {
		log.Error("unable to serve api call response", slog.Any("err", err))
	}
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

	// Issue #156 — scope-aware validation. We must NOT call the shared
	// util.ParseNamespacedName here: it hard-rejects an empty `namespace`
	// before scope is known (nsn.go:18), which is exactly the
	// cluster-scoped write we now want to serve. Read name/namespace
	// directly and apply the /call-LOCAL rule below. (util.ParseNamespacedName
	// stays byte-unchanged for its other prod caller, dispatchers/helpers.go:38.)
	name := req.URL.Query().Get("name")
	namespace := req.URL.Query().Get("namespace")

	if namespace != "" {
		// namespace PRESENT → today's namespaced path, BYTE-IDENTICAL, and
		// we deliberately do NOT consult the scope mapper: a namespaced
		// write must not gain a boot-window / discovery-lag failure mode.
		// Preserve the exact pre-#156 name-required check (nsn.go:12-14):
		// util.ParseNamespacedName required a non-empty name for EVERY
		// verb, so keep that (POST-without-name 400s today → still 400s).
		if name == "" {
			err = fmt.Errorf("missing 'name' query parameter")
			return
		}
		opts.namespaced = true
		opts.nsn = types.NamespacedName{Name: name, Namespace: namespace}
	} else {
		// namespace ABSENT → this 400s today (util.ParseNamespacedName
		// rejects the empty namespace). NOW resolve scope: only take the
		// new cluster-scoped path on a POSITIVE cluster-scope. Everything
		// else (namespaced GVR, mapper miss/error/unknown) FAILS-CLOSED
		// with a 400 — exactly as /call returns today — never a silent
		// namespaced fallback and never a panic.
		var namespaced bool
		namespaced, err = r.resolveScope(opts.gvr)
		if err != nil {
			// Scope unknown (CRD not yet discovered, mapper not synced,
			// ambiguous resource). Fail-closed: 400, same family as today's
			// missing-namespace 400. Forces a retry once discovery settles.
			err = fmt.Errorf("unable to resolve scope for resource %q (namespace omitted): %w", opts.gvr.Resource, err)
			return
		}
		if namespaced {
			// Genuinely namespaced GVR but no namespace supplied →
			// backward-compatible 400 (byte-identical intent to today's
			// missing-'namespace' rejection).
			err = fmt.Errorf("missing 'namespace' query parameter")
			return
		}
		// cluster-scoped GVR → the new capability. name is required for
		// by-name verbs (GET/PUT/PATCH/DELETE); POST create may omit it
		// (apiserver assigns / uses metadata.name in the body). buildURIPath
		// omits the namespaces/<ns> segment when namespaced==false.
		if name == "" && has([]string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}, opts.verb) {
			err = fmt.Errorf("missing 'name' query parameter")
			return
		}
		opts.namespaced = false
		opts.nsn = types.NamespacedName{Name: name} // Namespace deliberately empty
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

// resolveScope reports whether opts.gvr is namespace-scoped. It is called
// ONLY on the namespace-absent branch of validateRequest (Issue #156). A
// nil scopeResolver (a mis-constructed handler) is treated as scope-unknown
// and fails-closed, never as "cluster" — so a wiring bug cannot silently
// widen the URI.
func (r *callHandler) resolveScope(gvr schema.GroupVersionResource) (namespaced bool, err error) {
	if r.scopeResolver == nil {
		return false, fmt.Errorf("scope resolver not configured")
	}
	return r.scopeResolver(gvr)
}

type callOptions struct {
	gvr         schema.GroupVersionResource
	nsn         types.NamespacedName
	verb        string
	contentType string
	perPage     int
	page        int
	dat         []byte
	// namespaced records the resolved cluster scope of gvr (Issue #156).
	// true  → emit the namespaces/<ns> URI segment (byte-identical to
	//         pre-#156 behaviour; always the case when a namespace was
	//         supplied).
	// false → cluster-scoped: OMIT the namespaces/<ns> segment.
	namespaced bool
}

func buildURIPath(opts callOptions) (string, error) {
	base := path.Join("/apis", opts.gvr.Group, opts.gvr.Version)
	if len(opts.gvr.Group) == 0 {
		base = path.Join("/api", opts.gvr.Version)
	}

	// Issue #156 — the ONLY scope-dependent difference is the
	// namespaces/<ns> segment. namespaced==true reproduces today's path
	// byte-for-byte; namespaced==false (a POSITIVE cluster scope resolved
	// upstream) omits the segment → base/resource. The name-append block
	// below is scope-independent and unchanged.
	var uriPath string
	if opts.namespaced {
		uriPath = path.Join(base, "namespaces", opts.nsn.Namespace, opts.gvr.Resource)
	} else {
		uriPath = path.Join(base, opts.gvr.Resource)
	}
	if strings.EqualFold("namespaces", opts.gvr.Resource) {
		// namespaces is itself cluster-scoped; either branch above yields
		// base/resource for it, but keep this explicit special case for
		// zero churn / backward-compat (harmless with the new branch).
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

	// Aggiunta dei query parametri, se necessario
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
