package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// iterParallelism returns the per-stage inner-call fan-out width.
//
// Default is GOMAXPROCS(0); env override RESOLVER_ITER_PARALLELISM
// (positive integer) takes precedence when set. The value is clamped
// to [1, 32] — the upper bound caps tail-latency damage from a
// pathological stage (e.g. 5000 inner calls × 100ms apiserver latency
// at unbounded width would saturate the apiserver, the snowplow pod's
// HTTP client pool, AND the Go scheduler simultaneously).
//
// Per architect's design 0.30.95: bound is hard-capped at the resolver
// boundary, NOT per-resource — no per-GVR carve-outs (feedback_no_special_cases.md).
func iterParallelism() int {
	n := runtime.GOMAXPROCS(0)
	if s := os.Getenv("RESOLVER_ITER_PARALLELISM"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			n = v
		}
	}
	if n > 32 {
		n = 32
	}
	if n < 1 {
		n = 1
	}
	return n
}

// lazyRegisterSlowThreshold is the ceiling above which EnsureResourceType
// emits a WARN log line. The call itself is just `rw.mu.Lock()` plus a
// `factory.ForResource` + goroutine spawn — sub-millisecond expected.
// Anything above this threshold means rw.mu is heavily contended or
// factory.ForResource is doing real work (rare client-go discovery
// path). The threshold is intentionally aggressive so the falsifier
// fires before 8s-class first-read symptoms accumulate.
const lazyRegisterSlowThreshold = 250 * time.Millisecond

const (
	//annotationKeyVerboseAPI = "krateo.io/verbose"
	headerAcceptJSON = "Accept: application/json"

	// envVerboseWireDump is the Ship 0.30.121 R1-b operator kill-switch
	// for httpcall's DumpResponse verbose wire-dump. When false (the
	// default) endpoint.Debug is NEVER set regardless of the per-RESTAction
	// krateo.io/verbose annotation — so a stray annotation on a heavy
	// RESTAction cannot OOM the pod. Only when this is explicitly "true"
	// does the per-call verbose decision (R1-a) get a chance to flip Debug.
	envVerboseWireDump = "RESOLVER_VERBOSE_WIRE_DUMP"
)

// verboseWireDumpEnabled reports whether the R1-b operator kill-switch
// permits the httpcall verbose wire-dump at all. Default false.
func verboseWireDumpEnabled() bool {
	return env.Bool(envVerboseWireDump, false)
}

// callWantsWireDump implements the Ship 0.30.121 R1-a per-call verbose
// decision. The verbose wire-dump (httpcall DumpResponse) stringifies the
// entire HTTP response body — for a K8s collection LIST that is the
// multi-MB compositions envelope, the dominant transient-memory cost. A
// K8s collection LIST is identified by ParseAPIServerPathToDep yielding a
// parseable apiserver path with an EMPTY object name. For every other
// call shape (a GET-by-name, an external endpoint, a nested /call) the
// body is small and the wire-dump is cheap — verbose stays available
// there for debugging. The decision is keyed on the parsed call shape
// ONLY — no resource/name literal (feedback_no_special_cases).
func callWantsWireDump(verbose bool, callPath string) bool {
	if !verbose {
		return false
	}
	if gvr, _, name, ok := cache.ParseAPIServerPathToDep(callPath); ok && name == "" {
		// A K8s collection LIST — suppress the wire-dump (this is the
		// ~1.94 GiB alloc_space line). gvr is parsed only to confirm the
		// apiserver-path shape; its value is not otherwise needed.
		_ = gvr
		return false
	}
	return true
}

type ResolveOptions struct {
	RC      *rest.Config
	AuthnNS string
	Verbose bool
	Items   []*templates.API
	PerPage int
	Page    int
	Extras  map[string]any

	// Watcher is the cluster-wide informer cache. When nil (the
	// default at 0.30.1, since CACHE_ENABLED defaults to false),
	// every API call takes the apiserver branch via httpcall.Do.
	// Routing for K8s-served endpoints flips on at 0.30.2.
	Watcher *cache.ResourceWatcher

	// RESTActionNamespace / RESTActionName identify the owning
	// RESTAction CR (Ship E, 0.30.116). They are folded into the
	// per-api-stage L1 key so a stage id is scoped to its RESTAction.
	// restactions.Resolve threads them from the RESTAction's
	// ObjectMeta. Empty when the caller did not supply them — the
	// api-stage key-swap then no-ops for that resolve (cache miss is
	// always safe), so a caller that forgets to thread them degrades
	// to the 0.30.115 path rather than mis-keying.
	RESTActionNamespace string
	RESTActionName      string
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

	// Cache routing gate. At 0.30.1 cache.Disabled() defaults to true
	// and Watcher is nil — every API call takes the apiserver branch.
	// The 0.30.2 ship lands the cache-served branch keyed off Watcher.
	if cache.Disabled() || opts.Watcher == nil {
		log.Debug("api.Resolve: cache disabled or watcher unset; using apiserver branch",
			slog.Bool("cache_disabled", cache.Disabled()),
			slog.Bool("watcher_nil", opts.Watcher == nil))
	}

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("unable to fetch user info from context", slog.Any("err", err))
		return map[string]any{}
	}

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

	// Ship F1 (0.30.119): the content-keyed api-stage L1 is active only
	// when RESOLVED_CACHE_APISTAGE_ENABLED=true (default off). Read the
	// gate + the store handle ONCE before the loop; flag-off both stay
	// inert and every call runs the byte-identical 0.30.118 path.
	//
	// Unlike Ship E's per-stage, per-user key, the F1 content layer keys
	// each K8s CALL by its (gvr, namespace, name-or-empty) — identity-
	// free, shared. No owning-RESTAction scoping is needed (the call
	// tuple fully identifies the content unit), so RESTActionName is no
	// longer consulted.
	apistageEnabled := cache.ApistageL1Enabled()
	var apistageStore *cache.ResolvedCacheStore
	if apistageEnabled {
		apistageStore = cache.ResolvedCache()
		if apistageStore == nil {
			apistageEnabled = false
		}
	}

	// Ship 0.30.120 layer (b) — error-aware Put-gate sink. The background
	// refresher installs an *atomic.Int64 on ctx via WithStageErrorSink;
	// each dict[call.ErrorKey] write below bumps it so resolveAndPopulateL1
	// can decline to overwrite a good L1 entry with a result produced
	// under a swallowed (continueOnError'd) stage error. On the normal
	// request path no sink is installed — stageErrSink is nil, the bump
	// sites are no-ops, and this resolve is byte-identical to 0.30.119.
	stageErrSink := cache.StageErrorSinkFromContext(ctx)

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

		// Tag 0.30.9 Sub-scope A: detect userAccessFilter.
		// When set, the dispatch uses snowplow's ServiceAccount
		// endpoint (cluster-wide read) — NOT the per-user
		// clientconfig — and the response is in-process-refiltered
		// per object through EvaluateRBAC. When unset, the dispatch
		// path is unchanged from 0.30.8 (per-user-token via the
		// endpointReferenceMapper). Per Revision 5 (binding): atomic
		// ship — no gate flag. Portal RestActions opt in by adding
		// the userAccessFilter stanza; the resolver branches
		// per-stage.
		uafActive := apiCall.UserAccessFilter != nil

		// User-bearer-token append: only for non-UAF stages. When
		// UAF is active the SA endpoint carries the SA token (no
		// user-bearer override needed); appending the user token
		// here would route the call through the user's credentials
		// instead of the SA's — breaking the entire UAF mechanism.
		if !uafActive {
			if accessToken, _ := xcontext.AccessToken(ctx); accessToken != "" {
				if apiCall.EndpointRef == nil || ptr.Deref(apiCall.ExportJWT, false) {
					apiCall.Headers = append(apiCall.Headers,
						fmt.Sprintf("Authorization: Bearer %s", accessToken))
				}
			}
		}

		// Resolve the endpoint. UAF stages use the snowplow-SA
		// endpoint; non-UAF stages go through the per-user
		// clientconfig (or the named EndpointRef) as before.
		var ep endpoints.Endpoint
		if uafActive {
			saEP, saErr := dynamic.ServiceAccountEndpoint()
			if saErr != nil {
				log.Error("userAccessFilter: cannot acquire ServiceAccount endpoint; falling through to per-user dispatch (degraded mode)",
					slog.String("name", id), slog.Any("err", saErr))
				// Fail-closed-but-respond: per Revision 5 atomic
				// ship there is no toggle to fall back to the
				// per-user path correctly (we'd leak the user
				// bearer token to a SA-marked stage). Returning
				// an empty result for this stage and continuing.
				dict[id] = map[string]any{"items": []any{}}
				continue
			}
			ep = *saEP
		} else {
			resolved, err := mapper.resolveOne(ctx, apiCall.EndpointRef)
			if err != nil {
				log.Error("unable to resolve api endpoint reference",
					slog.String("name", id), slog.Any("ref", apiCall.EndpointRef), slog.Any("error", err))
				return dict
			}
			ep = resolved
		}
		// Ship 0.30.121 R1 — the verbose wire-dump (httpcall's DumpResponse)
		// is the single largest transient-memory consumer (~1.94 GiB
		// alloc_space on the 50K bench: it stringifies every HTTP response
		// body, including the multi-MB compositions LIST). The blanket
		// `ep.Debug = opts.Verbose` set here is REMOVED — Debug is now
		// decided PER-CALL inside the g.Go worker (R1-a: never for a K8s
		// collection LIST) and additionally gated on RESOLVER_VERBOSE_WIRE_DUMP
		// (R1-b: an operator kill-switch, default off). See the worker below.
		log.Debug("resolved endpoint for api call",
			slog.String("name", id), slog.String("host", ep.ServerURL),
			slog.Bool("uaf", uafActive))

		tmp := createRequestOptions(ctx, log, apiCall, dict)
		if len(tmp) == 0 {
			log.Warn("empty request options for http call", slog.Any("name", id))
			continue
		}

		// 0.30.92 widening: lazy-register the informer for every
		// downstream apiserver GVR enumerated by this stage's request
		// options. Without this, the 0.30.91 hook only fired for the
		// entry-point RESTAction GVR (recorded in restactions.go's
		// dispatcher) — downstream GVRs the resolver dispatches inner
		// HTTP calls against (e.g. compositions, sidebar widgets,
		// resourcesRefs targets) never received an informer, so the
		// 0.30.8 dep-tracker DeleteFunc never fired and
		// `evict_delete_total` stayed 0 after deliberate DELETE events
		// (probe `/tmp/snowplow-runs/0.30.91/preflight/probe.log`,
		// gates 1 + 4 FAIL).
		//
		// `call.Path` is the JQ-evaluated apiserver REST path
		// (e.g. `/apis/composition.krateo.io/v1/namespaces/<ns>/
		// githubscaffoldingwithcompositionpages`). `ParseAPIServerPathToGVR`
		// extracts (Group, Version, Resource) and skips non-apiserver
		// paths (external endpoints, malformed templated fragments).
		// EnsureResourceType is idempotent + singleflight under rw.mu;
		// duplicate calls within the loop are sub-microsecond no-ops.
		//
		// Timing instrumentation: if EnsureResourceType ever blocks
		// longer than lazyRegisterSlowThreshold we emit a WARN so the
		// 0.30.92 first-read-latency follow-up has a falsifier.
		lazyRegisterInnerCallPaths(log, tmp)

		// 0.30.95 bounded-parallel inner-call iterator.
		//
		// Pre-0.30.95 the inner-call loop was sequential — N inner calls
		// against the apiserver paid N × per-call latency. The architect's
		// 0.30.95 design replaces it with a bounded errgroup whose width
		// is iterParallelism() (GOMAXPROCS default, env-overridable, hard
		// cap 32). dictMu serialises all writes against `dict` from
		// concurrent goroutines (jsonHandler closure + error-branch
		// inline). gctx flows into httpcall.Do so the first hard-error
		// cancels in-flight peers when ContinueOnError=false.
		//
		// Edge type 3 dep-recording stays INLINE before g.Go (per the
		// 0.30.94 contract — sync.Map under the hood, safe to call from
		// the parent goroutine, no need to record from inside the worker).
		//
		// The success-branch log calls mapDepth(dict) which walks dict —
		// that read must be serialised against concurrent jsonHandler
		// writes too, so we take dictMu there as well.
		var dictMu sync.Mutex
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(iterParallelism())

		// Ship 0.30.121 R1-b — the operator kill-switch. Compute once per
		// stage: verbose is permitted ONLY when the RESTAction asked for it
		// (opts.Verbose) AND the env flag explicitly enables the wire-dump.
		// Default off => wireVerbose is false => no call ever gets Debug.
		wireVerbose := opts.Verbose && verboseWireDumpEnabled()

		for i := range tmp {
			call := tmp[i]
			// Ship 0.30.121 R1-a — per-call verbose decision. `ep` is shared
			// across every tmp[] call of this stage; setting ep.Debug in
			// place would race the concurrent g.Go workers. When this call
			// wants the wire-dump, take a SHALLOW COPY of the Endpoint value
			// and flip Debug on the copy, so peers keep the un-Debugged
			// shared Endpoint. A K8s collection LIST never wants it
			// (callWantsWireDump returns false) — that suppresses the
			// ~1.94 GiB DumpResponse alloc_space line.
			if callWantsWireDump(wireVerbose, call.Path) {
				epCopy := ep
				epCopy.Debug = true
				call.Endpoint = &epCopy
			} else {
				call.Endpoint = &ep
			}
			// Wrap jsonHandler so every dict[id] mutation goes through
			// dictMu. The inner closure is the per-call ResponseHandler
			// that httpcall.Do invokes from inside its goroutine.
			inner := jsonHandler(gctx, jsonHandlerOptions{
				key: id, out: dict, filter: apiCall.Filter,
			})
			call.ResponseHandler = func(r io.ReadCloser) error {
				dictMu.Lock()
				defer dictMu.Unlock()
				return inner(r)
			}

			// Edge type 3 dep recording — see 0.30.94 ship for full
			// rationale. cache.Deps() is sync.Map-backed; idempotent
			// LoadOrStore; safe from this (parent-goroutine) site.
			//
			// Ship F1 (0.30.119): the dep edge attaches to whatever L1
			// key the request path threaded (the per-user resolved-output
			// key). The CONTENT-keyed api-stage entry records its OWN dep
			// edge inside the worker, keyed by the per-call content key
			// (gvr,ns,[name]) — so an informer event on a K8s call's GVR
			// dirty-marks the matching content entry and the refresher
			// re-dispatches that one call.
			if l1Key := cache.L1KeyFromContext(ctx); l1Key != "" && !cache.Disabled() {
				if ptr.Deref(call.Verb, http.MethodGet) == http.MethodGet {
					if gvr, ns, name, parseOK := cache.ParseAPIServerPathToDep(call.Path); parseOK {
						if name == "" {
							cache.Deps().RecordList(l1Key, gvr, ns)
						} else {
							cache.Deps().Record(l1Key, gvr, ns, name)
						}
						log.Debug("dep.recorded",
							slog.String("subsystem", "cache"),
							slog.String("edge_type", "innerCall"),
							slog.String("gvr", gvr.String()),
							slog.String("ns", ns),
							slog.String("name", name),
							slog.String("l1_key", l1Key),
						)
					}
				}
			}

			g.Go(func() error {
				log.Debug("calling api", slog.String("name", id),
					slog.String("host", call.Endpoint.ServerURL),
					slog.String("path", call.Path),
				)

				// Ship 0.30.123 (#155) — in-process nested /call. This is
				// the FIRST dispatch branch (before the informer pivot,
				// before httpcall.Do). When the stage's `path` is a
				// /call?resource=...&apiVersion=... loopback into snowplow's
				// OWN /call endpoint, resolve the referenced RESTAction
				// IN-PROCESS — no HTTP request, no Authorization header,
				// identity carried by the WithUserInfo already on ctx. This
				// lets a JWT-less / SA-credentialed resolve complete an
				// exportJwt loopback stage (the 0.30.120 poison) and is the
				// hard prerequisite for F2's startup SA-prewarm.
				//
				// Three structural gates, ALL must hold or the branch is
				// skipped and the call falls through to the informer pivot
				// / httpcall.Do exactly as 0.30.121:
				//   1. RESOLVER_INPROCESS_NESTED_CALL enabled (default true);
				//   2. the resolver seam is wired (nestedCallResolver != nil
				//      — the second structural fallback);
				//   3. the call is a GET whose path parses as a /call
				//      loopback (util.ParseCallPathToObjectRef — SHAPE only,
				//      no resource/name/host literal).
				// On a nested error: honour ContinueOnError / ErrorKey
				// exactly as the HTTP path, AND bump the 0.30.120 stage-error
				// sink so layer (b)'s Put-gate still sees the failure.
				if inprocessNestedCallEnabled() && nestedCallResolver != nil &&
					ptr.Deref(call.Verb, http.MethodGet) == http.MethodGet {
					if ref, isLoopback := util.ParseCallPathToObjectRef(call.Path); isLoopback {
						statusRaw, nerr := nestedCallResolver(gctx, ref,
							opts.PerPage, opts.Page, opts.Extras)
						if nerr != nil {
							log.Error("nested /call resolution failed",
								slog.String("name", id),
								slog.String("path", call.Path),
								slog.String("dispatch", "in-process-nested-call"),
								slog.String("error", nerr.Error()))
							dictMu.Lock()
							dict[call.ErrorKey] = nerr.Error()
							dictMu.Unlock()
							// Layer (b) backstop (0.30.120): record the stage
							// error on the refresher's sink (nil on the
							// request path) so the error-aware Put-gate still
							// sees a nested-/call failure.
							if stageErrSink != nil {
								stageErrSink.Add(1)
							}
							if !call.ContinueOnError {
								return fmt.Errorf("api %s failed: %s", id, nerr.Error())
							}
							// ContinueOnError: fall through to the success-log
							// line, mirroring the httpcall.Do ContinueOnError
							// contract.
						} else {
							// The in-process result IS the referenced
							// RESTAction's Status.Raw — byte-identical to the
							// HTTP /call response body. Feed it to the stage's
							// ResponseHandler exactly as the HTTP return is fed.
							if err := call.ResponseHandler(readerFromBytes(statusRaw)); err != nil {
								return err
							}
						}
						dictMu.Lock()
						depth := mapDepth(dict)
						dictMu.Unlock()
						dispatch := "in-process-nested-call"
						if nerr != nil {
							dispatch = "in-process-nested-call-error"
						}
						log.Info("api successfully resolved",
							slog.String("name", id),
							slog.String("host", call.Endpoint.ServerURL),
							slog.String("path", call.Path),
							slog.Int("depth", depth),
							slog.String("dispatch", dispatch),
						)
						return nil
					}
				}

				// 0.30.95 resolver pivot — dispatch GET reads to the
				// informer cache when RESOLVER_USE_INFORMER=true.
				// Flag default OFF: this branch is byte-identical to
				// 0.30.94 with the flag unset (R-FALSE-1 invariant).
				//
				// The pivot returns served=true ONLY when the call is
				// safely cache-servable (GET, parseable apiserver path,
				// cache=on, full-Unstructured informer, synced). All
				// other shapes (write verbs, subresources, external
				// URLs, metadata-only GVRs, pre-sync, 404) fall through
				// to the apiserver branch below unchanged.
				//
				// Ship F1 (0.30.119) — content-keyed api-stage L1. When
				// apistageEnabled, the pivot-served raw envelope is
				// cached identity-free under the per-call content key
				// (gvr, namespace, name-or-empty):
				//
				//   1. content Get(contentKey) — HIT: use the stored raw
				//      envelope, skip the dispatch entirely. MISS:
				//      dispatch UN-GATED (WithApistageContentResolve makes
				//      dispatchViaInformer skip its inline RBAC gate) and
				//      Put the raw envelope under contentKey.
				//   2. GATE the raw envelope (hit OR miss) with the
				//      REQUEST identity — gateContentEnvelope runs
				//      filterListByRBAC/filterGetByRBAC, the single F1
				//      gate site. served=false here is fail-closed (no
				//      identity / GET denied) → fall through to apiserver.
				//   3. feed the GATED envelope to call.ResponseHandler →
				//      jsonHandler/apiCall.Filter → dict[id], unchanged.
				//
				// The content entry holds only un-gated content; the
				// per-user narrowing is the fresh per-request gate at
				// step 2 — no cross-user leak, the hit path is gated too.
				// Flag-off (apistageEnabled false) this is byte-identical
				// to the 0.30.118 pivot path.
				if resolverUseInformer() == "true" {
					if apistageEnabled {
						if raw, served, ok := apistageContentServe(gctx, apistageStore, call); ok {
							if served {
								if err := call.ResponseHandler(readerFromBytes(raw)); err != nil {
									return err
								}
								dictMu.Lock()
								depth := mapDepth(dict)
								dictMu.Unlock()
								log.Info("api successfully resolved",
									slog.String("name", id),
									slog.String("host", call.Endpoint.ServerURL),
									slog.String("path", call.Path),
									slog.Int("depth", depth),
									slog.String("dispatch", "apistage-content"),
								)
								return nil
							}
							// served=false — fail-closed (no identity / GET
							// denied): fall through to the apiserver branch,
							// whose per-user token narrows correctly.
						}
						// ok=false — the content layer could not serve this
						// call (not pivot-servable: write verb, external URL,
						// metadata-only GVR, pre-sync). Fall through.
					} else if raw, served := dispatchViaInformer(gctx, call); served {
						if err := call.ResponseHandler(readerFromBytes(raw)); err != nil {
							return err
						}
						dictMu.Lock()
						depth := mapDepth(dict)
						dictMu.Unlock()
						log.Info("api successfully resolved",
							slog.String("name", id),
							slog.String("host", call.Endpoint.ServerURL),
							slog.String("path", call.Path),
							slog.Int("depth", depth),
							slog.String("dispatch", "informer"),
						)
						return nil
					}
				}

				// 0.30.104 Phase-1 TLS-CA fix — when an internal-dispatch
				// *rest.Config is on the context (Phase 1's SA-credentialed
				// startup walk attaches its rest.InClusterConfig() config
				// via cache.WithInternalRESTConfig), route apiserver-path
				// GET/LIST calls through a client-go dynamic client built
				// from that *rest.Config instead of plumbing's httpcall.Do.
				//
				// WHY: plumbing's httpcall.Do builds the HTTP client from
				// the Endpoint shape; its tlsConfigFor installs a custom CA
				// pool ONLY in the HasCertAuth() branch. The snowplow SA
				// endpoint is TOKEN-auth, so the SA's cluster CA is dropped
				// and the apiserver TLS handshake fails with
				// "x509: certificate signed by unknown authority" — Phase 1
				// never discovers the composition GVR. The context-carried
				// *rest.Config is the rest.InClusterConfig() value, which
				// carries the cluster CA verbatim; client-go's transport
				// installs it correctly. See internal_dispatch.go.
				//
				// BEHAVIOR-NEUTRAL: ordinary per-user requests never set
				// cache.WithInternalRESTConfig, so dispatchViaInternalRESTConfig
				// returns served=false for them and this block is a no-op —
				// the path is byte-identical to pre-0.30.104.
				//
				// A non-nil err here is the REAL apiserver error (a 403, a
				// genuine connectivity fault). We do NOT fall through to
				// httpcall.Do on error — that would just re-hit the broken
				// plumbing TLS path and mask the real error behind a second
				// x509 failure. We surface it exactly as an httpcall.Do
				// StatusFailure: write call.ErrorKey under dictMu, then
				// honour ContinueOnError.
				if raw, served, ierr := dispatchViaInternalRESTConfig(gctx, call); served || ierr != nil {
					if ierr != nil {
						log.Error("api call response failure", slog.String("name", id),
							slog.String("host", call.Endpoint.ServerURL),
							slog.String("path", call.Path),
							slog.String("dispatch", "internal-rest-config"),
							slog.String("error", ierr.Error()))
						dictMu.Lock()
						dict[call.ErrorKey] = ierr.Error()
						dictMu.Unlock()
						// Ship 0.30.120 layer (b): record the stage error on
						// the refresher's sink (nil on the request path).
						if stageErrSink != nil {
							stageErrSink.Add(1)
						}
						if !call.ContinueOnError {
							// Cancel gctx so in-flight peers short-circuit —
							// same contract as the httpcall.Do StatusFailure
							// hard-error branch below.
							return fmt.Errorf("api %s failed: %s", id, ierr.Error())
						}
						// ContinueOnError: the internal dispatcher OWNED this
						// call (an internal *rest.Config is on the context).
						// We must NOT fall through to httpcall.Do — that would
						// re-hit the broken plumbing TLS path and mask the
						// real error behind a second x509 failure. Emit the
						// success-log line and return, mirroring the
						// httpcall.Do ContinueOnError fall-through.
					} else {
						if err := call.ResponseHandler(readerFromBytes(raw)); err != nil {
							return err
						}
					}
					dictMu.Lock()
					depth := mapDepth(dict)
					dictMu.Unlock()
					dispatch := "internal-rest-config"
					if ierr != nil {
						dispatch = "internal-rest-config-error"
					}
					log.Info("api successfully resolved",
						slog.String("name", id),
						slog.String("host", call.Endpoint.ServerURL),
						slog.String("path", call.Path),
						slog.Int("depth", depth),
						slog.String("dispatch", dispatch),
					)
					return nil
				}

				res := httpcall.Do(gctx, call)
				if res.Status == response.StatusFailure {
					log.Error("api call response failure", slog.String("name", id),
						slog.String("host", call.Endpoint.ServerURL),
						slog.String("path", call.Path),
						slog.String("error", res.Message))

					asMap, mapErr := response.AsMap(res)
					if mapErr != nil {
						log.Warn("unable to encode status as dict", slog.Any("err", mapErr))
					}

					dictMu.Lock()
					if len(asMap) > 0 {
						dict[call.ErrorKey] = asMap
					} else {
						dict[call.ErrorKey] = res.Message
					}
					dictMu.Unlock()
					// Ship 0.30.120 layer (b): record the stage error on the
					// refresher's sink (nil on the request path) — covers both
					// the asMap and res.Message ErrorKey-write branches above.
					if stageErrSink != nil {
						stageErrSink.Add(1)
					}

					if !call.ContinueOnError {
						// Returning a non-nil error cancels gctx so
						// in-flight peers short-circuit; g.Wait() picks
						// up the first such error. dict already carries
						// call.ErrorKey from the lock-protected write
						// above, matching the pre-0.30.95 sequential
						// early-return contract.
						return fmt.Errorf("api %s failed: %s", id, res.Message)
					}
					// ContinueOnError: fall through to the success-log
					// line (preserves pre-0.30.95 behaviour where the
					// "successfully resolved" line emitted on every
					// non-hard-error call).
				}

				// mapDepth walks dict — serialise against concurrent
				// jsonHandler writes by reading under dictMu.
				dictMu.Lock()
				depth := mapDepth(dict)
				dictMu.Unlock()
				log.Info("api successfully resolved",
					slog.String("name", id),
					slog.String("host", call.Endpoint.ServerURL),
					slog.String("path", call.Path),
					slog.Int("depth", depth),
				)
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			log.Debug("api stage short-circuited on hard error",
				slog.String("name", id), slog.Any("err", err))
			return dict
		}

		// Tag 0.30.9 Sub-scope A: refilter the SA-dispatched result
		// in-process. Runs AFTER all dispatched calls for this API
		// stage complete + their jsonHandler has populated dict[id].
		// Per Revision 2 binding: EvaluateRBAC fires per object —
		// this is the load-bearing security gate that turns cluster-
		// wide-read into user-scoped result sets.
		if uafActive {
			rf := applyUserAccessFilter(ctx, dict, apiCall)
			emitRefilterFalsifier(log, apiCall, user.Username, rf)
		}

		// Ship F1 (0.30.119): the api-stage L1 is now CONTENT-keyed —
		// the per-K8s-call Put happens inside the g.Go worker (each call
		// stores its own raw envelope under its (gvr,ns,[name]) content
		// key). There is NO per-stage Put here — the Ship E per-stage
		// entry is gone; an iterator stage produces N content entries,
		// one per call, assembled into dict[id] by the N jsonHandler
		// merges exactly as before.
	}

	removeManagedFields(dict)
	//delete(dict, "slice")

	return dict
}

// lazyRegisterInnerCallPaths walks the per-stage RequestOptions slice
// (one entry per iterator dispatch — the iterator + non-iterator paths
// share this code) and calls cache.Global().EnsureResourceType for the
// GVR derived from each call.Path. Idempotent across paths that point
// at the same GVR (singleflight under rw.mu).
//
// Cache=off / watcher=nil branch is silently skipped — there is no
// informer to register and the apiserver-fallback path in
// `httpcall.Do` handles the call regardless.
//
// Paths that don't resolve to an apiserver GVR (external endpoints,
// JQ-evaluation failures that leak `${...}` to the final string) are
// also silently skipped — those have no informer counterpart and the
// dispatch will hit the external URL through `httpcall.Do` as before.
//
// Timing: per-call duration is measured; calls slower than
// lazyRegisterSlowThreshold emit a WARN log so a regression in
// rw.mu contention or factory.ForResource cost becomes visible.
func lazyRegisterInnerCallPaths(log *slog.Logger, opts []httpcall.RequestOptions) {
	rw := cache.Global()
	if rw == nil {
		return
	}
	seen := map[schema.GroupVersionResource]struct{}{}
	for i := range opts {
		path := opts[i].Path

		// 0.30.102 Tag B Part 2 — CRD-watch group feed. Composition
		// apiserver paths are JQ-templated (`/apis/<group>/${.v}/...`)
		// so ParseAPIServerPathToGVR (which rejects any `${`) cannot
		// derive their GVR here. The GROUP segment is static, though —
		// extract it and feed the CRD-watch's navigation-derived
		// auto-discover set. Gated by PREWARM_ENABLED so a flag-OFF
		// process is byte-identical (the auto-discover set stays empty
		// and the CRD-watch never runs). Non-templated paths also flow
		// through here harmlessly — their group is added too, which is
		// correct (it IS navigation-reached).
		if cache.PrewarmEnabled() {
			if grp, grpOK := cache.ExtractAPIServerGroupFromTemplatedPath(path); grpOK {
				cache.AddAutoDiscoverGroup(grp)
			}
		}

		gvr, ok := cache.ParseAPIServerPathToGVR(path)
		if !ok {
			continue
		}
		if _, dup := seen[gvr]; dup {
			continue
		}
		seen[gvr] = struct{}{}

		start := time.Now()
		added, _ := rw.EnsureResourceType(gvr)
		elapsed := time.Since(start)

		// Emit a one-shot INFO line on first registration of a GVR so
		// the gate-2 probe can count distinct lazy-registered GVRs.
		// The cache layer emits its own `cache.lazy_register` line —
		// we add a callsite-specific marker so post-mortems can tell
		// whether the entry came from a widget dep edge or an inner
		// resolver call.
		if added {
			log.Info("cache.lazy_register.inner_call",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("path", path),
				slog.Duration("ensure_elapsed", elapsed),
				slog.String("hint", "resolver inner-call first touch — informer registered + dep-tracker handlers wired"),
			)
		}
		if elapsed > lazyRegisterSlowThreshold {
			log.Warn("cache.lazy_register.slow",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.Duration("elapsed", elapsed),
				slog.Duration("threshold", lazyRegisterSlowThreshold),
				slog.String("hint", "EnsureResourceType blocked unexpectedly long — investigate rw.mu contention"),
			)
		}
	}
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
