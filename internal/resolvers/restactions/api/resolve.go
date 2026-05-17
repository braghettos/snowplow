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
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
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
)

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

	// Ship E (0.30.116): the per-api-stage L1 key-swap is active only
	// when RESOLVED_CACHE_APISTAGE_ENABLED=true (default off). Read the
	// gate + the store handle ONCE before the loop; flag-off both stay
	// inert and every stage runs the byte-identical 0.30.115 path.
	//
	// The api-stage L1 caches resolved STAGE OUTPUT in the
	// ResolvedCacheStore — it does not depend on the informer pivot, so
	// opts.Watcher is not consulted here (ApistageL1Enabled already
	// gates CACHE_ENABLED + RESOLVED_CACHE_ENABLED). RESTActionName
	// being empty means the caller did not thread the owning
	// RESTAction's identity — without it the stage key cannot be scoped,
	// so the key-swap no-ops (degrades to the 0.30.115 path).
	apistageEnabled := cache.ApistageL1Enabled() && opts.RESTActionName != ""
	var apistageStore *cache.ResolvedCacheStore
	if apistageEnabled {
		apistageStore = cache.ResolvedCache()
		if apistageStore == nil {
			apistageEnabled = false
		}
	}

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

		// Ship E (0.30.116) — per-api-stage L1 key-swap. Gated by
		// apistageEnabled (RESOLVED_CACHE_APISTAGE_ENABLED=true). The
		// stage key is per-user-keyed (Username+Groups — never cohort),
		// scoped to the owning RESTAction GVR/namespace/name, and folds
		// in the O5 canonical filter-hash + the stage's effective dict
		// input. A hit serves dict[id] from L1 and skips the stage's
		// K8s call(s) entirely; a miss runs the stage under the stage
		// key's WithL1KeyContext so the inner-call dep-recording
		// attributes this stage's LIST/GET to the stage entry (O4), then
		// stores dict[id] under the stage key.
		//
		// stageCtx is the context the stage body resolves under: on the
		// apistage path it carries WithL1KeyContext(stageKey); flag-off
		// it is the unchanged request ctx.
		stageCtx := ctx
		var stageKey string
		var stageInputs cache.ResolvedKeyInputs
		if apistageEnabled {
			stageInputs = stageKeyInputs(
				opts.RESTActionNamespace, opts.RESTActionName,
				user.Username, user.Groups, apiCall, dict, opts.PerPage, opts.Page)
			stageKey = cache.ComputeKey(stageInputs)

			// Ship 0.30.118 — refresh self-hit fix. When this resolve is a
			// REFRESH-driven re-resolve (WithRefreshBypass set by the
			// refresher) AND this stage's key is EXACTLY the refresh's
			// target stage key, SKIP the Get: the whole point of the
			// refresh is to recompute THIS stage from K8s. Without the
			// skip the stage would self-hit the dirty-but-resident entry
			// it is refreshing (Get honors only TTL, not the dirty flag),
			// the K8s call would be skipped, and no fresh value re-Put.
			// The skip is scoped to the EXACT target stage key — sibling
			// stages of the same refresh still Get-hit normally (they are
			// not being refreshed). Request-path resolves never carry the
			// marker, so this is byte-identical to 0.30.117 for them.
			bypassThisStage := cache.RefreshBypassFromContext(ctx) &&
				stageKey == cache.L1KeyFromContext(ctx)

			if !bypassThisStage {
				if entry, hit := apistageStore.Get(stageKey); hit && entry != nil {
					if v, decoded := decodeStageValue(entry.RawJSON); decoded {
						dict[id] = v
						log.Info("apistage.l1_hit",
							slog.String("subsystem", "cache"),
							slog.String("name", id),
							slog.String("key_hash", stageKey),
							slog.String("hint", "stage served from api-stage L1 — K8s call skipped"),
						)
						continue
					}
				}
			} else {
				log.Info("apistage.refresh_bypass",
					slog.String("subsystem", "cache"),
					slog.String("name", id),
					slog.String("key_hash", stageKey),
					slog.String("hint", "refresh-driven re-resolve of this exact stage — Get skipped, recomputing from K8s"),
				)
			}
			// Miss (or bypass) — resolve the stage under the stage key so
			// the existing inner-call dep-recording (resolve.go ~line 300)
			// attributes this stage's K8s LIST/GET to the stage entry.
			stageCtx = cache.WithL1KeyContext(ctx, stageKey)
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
		if opts.Verbose {
			ep.Debug = opts.Verbose
		}
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
		// Ship E (0.30.116): errgroup derives from stageCtx — on the
		// apistage path that carries WithL1KeyContext(stageKey), so the
		// inner-call dep-recording below attributes this stage's K8s
		// LIST/GET to the stage entry (O4). Flag-off stageCtx == ctx,
		// so this is byte-identical to 0.30.115.
		g, gctx := errgroup.WithContext(stageCtx)
		g.SetLimit(iterParallelism())

		for i := range tmp {
			call := tmp[i]
			call.Endpoint = &ep
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
			// Ship E (0.30.116): read the L1 key from stageCtx. Flag-on,
			// that is the api-stage stageKey — so the dep edge attaches
			// to the STAGE entry (O4: an informer event on this GVR
			// dirty-marks the stage entry and the refresher re-resolves
			// it). Flag-off stageCtx == ctx, so the edge attaches to
			// whatever L1 key the request path threaded — byte-identical
			// to 0.30.115.
			if l1Key := cache.L1KeyFromContext(stageCtx); l1Key != "" && !cache.Disabled() {
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
				// When served=true the call.ResponseHandler lambda
				// (constructed above, line ~291) is invoked with the
				// cache-served bytes. That lambda holds dictMu before
				// delegating to jsonHandler, so the dict mutation
				// contract is identical between pivot and apiserver
				// branches — no second mutex path.
				if resolverUseInformer() == "true" {
					if raw, served := dispatchViaInformer(gctx, call); served {
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

		// Ship E (0.30.116): store the resolved stage output under the
		// stage key. Reached ONLY on the apistage path and ONLY after
		// g.Wait() succeeded + the UAF refilter ran — a stage that
		// short-circuited on a hard error returned `dict` above and
		// never reaches here, so a failed stage is never cached. The
		// stored value is the post-refilter dict[id] — byte-identical
		// to what a subsequent hit serves. The entry's Inputs identify
		// it as an "apistage" entry of the owning RESTAction so the
		// refresher's resolve-once seam can re-run this single stage.
		if apistageEnabled {
			if encoded, encErr := encodeStageValue(dict[id]); encErr == nil {
				inputsCopy := stageInputs
				apistageStore.Put(stageKey, &cache.ResolvedEntry{
					RawJSON: encoded,
					Inputs:  &inputsCopy,
				})
				log.Info("apistage.l1_store",
					slog.String("subsystem", "cache"),
					slog.String("name", id),
					slog.String("key_hash", stageKey),
				)
			} else {
				log.Warn("apistage.encode_failed",
					slog.String("subsystem", "cache"),
					slog.String("name", id),
					slog.Any("err", encErr),
				)
			}
		}
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
