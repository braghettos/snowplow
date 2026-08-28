// Package api executes the individual API stages of a RESTAction. Resolve
// runs each stage (HTTP endpoint calls and in-process Kubernetes GET/LIST
// loopbacks, routed through the informer cache where possible), applies the
// stage's jq, and accumulates results into the shared dict consumed by later
// stages and the RESTAction filter. It handles per-stage inner-call
// fan-out, RBAC narrowing/refiltering, sorting, and endpoint resolution.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/env"
	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/plumbing/jqutil"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/plumbing/kubeutil"
	"github.com/krateo-platformops/plumbing/maps"
	"github.com/krateo-platformops/plumbing/ptr"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/dynamic"
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
//
// Ship F2 (0.30.125): a resolve under cache.WithPrewarmIterSerial — the
// SA content-population pass — gets parallelism 1. The content pass
// uncaps the iterator (full per-namespace fan-out, the #159 OOM
// territory) but runs behind the 503 readiness gate with no latency
// budget, so it forces the fan-out serial to hold peak RSS down. This
// is CONTEXT-SCOPED: every real /call carries no marker and keeps the
// GOMAXPROCS/env width.
func iterParallelism(ctx context.Context) int {
	if cache.PrewarmIterSerialFromContext(ctx) {
		return 1
	}
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
	// headerAcceptJSON is the default per-stage Accept header. Since the
	// feat/restaction-yaml-response ship (snowplow-side JSON-or-YAML
	// external-GET relaxation) it advertises YAML media types in addition
	// to JSON — strictly MORE permissive. This only affects the EXTERNAL
	// HTTP-fetch path (httpFetchAllowingNonJSON); the internal-rest-config
	// and informer dispatch paths bypass the HTTP client entirely and do
	// not honour this header, so the no-regression invariant (AC4) holds.
	// Helm repositories ignore Accept anyway and serve index.yaml as
	// text/plain or text/yaml regardless — the relaxation is what lets
	// that body through (the prior plumbing path 406'd it before the body
	// was read).
	headerAcceptJSON = "Accept: application/json, application/x-yaml, text/yaml"

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

// accumulateErrorKey implements Ship 0.30.257 (#313) Option W-A: the shared
// dict[errorKey] is an ACCUMULATING slice rather than last-wins, so an
// iterator stage with K failing items surfaces ALL K errors (not just the
// last). All iterator items of a stage share ONE errorKey (setup.go:59), so
// the pre-0.30.257 `dict[errorKey] = value` clobbered earlier item errors —
// the long-standing ContinueOnError=true last-wins bug (trace §1.2 / §2.3).
//
//   - absent          → []any{value}
//   - already []any   → append(existing, value)
//   - a non-slice     → []any{existing, value}  (defensive promotion: a prior
//     scalar from any non-iterator path is preserved, never
//     dropped)
//
// CONCURRENCY: `dict` is the shared serve map written by parallel iterator
// workers; the caller MUST hold dictMu across this call (the three worker
// error branches do — resolve.go error sites). The append is NOT internally
// locked — keeping the lock at the call site mirrors the existing
// dict[ErrorKey]-write pattern and keeps the whole error-record + (in the
// httpcall branch) the asMap decision inside one critical section. Exercised
// concurrently by TestResolve_IteratorErrorCollection_Race under -race.
func accumulateErrorKey(dict map[string]any, key string, value any) {
	existing, present := dict[key]
	if !present {
		dict[key] = []any{value}
		return
	}
	if slice, ok := existing.([]any); ok {
		dict[key] = append(slice, value)
		return
	}
	// A present non-slice value (scalar/map from any path) — promote to a
	// slice so neither the prior value nor the new one is silently dropped.
	dict[key] = []any{existing, value}
}

// resolveRun bundles the resolve-INVARIANT state for a single Resolve call:
// values written ONCE by newResolveRun and constant for the whole resolve.
// It is unexported, built one-per-call, and NEVER shared across goroutines
// (each concurrent Resolve builds its own). Mirrors the newPhase1Walker
// constructor-with-positional-required-args precedent (design §1 / §3.1).
//
// CONCURRENCY (feedback_shared_vs_copy_is_a_concurrency_change): the per-STAGE
// mutable primitives (dictMu, g, gctx, itemErrs, iterStart, wireVerbose) are
// DELIBERATELY NOT fields here — they are re-created each stage and would
// either outlive their stage or alias across stages if promoted to fields.
// They stay method-/loop-local. The only mutated field is `dict`, which is
// already the shared-across-goroutines serve map today, guarded by the
// stage-local dictMu (unchanged). Net: zero new sharing — PROVEN by the
// unchanged -race corpus (TestResolve_ConcurrentRequestsDoNotCrossPollinate,
// TestShip2a_FullResolve_ManagedFields_Race, etc.).
type resolveRun struct {
	ctx             context.Context
	log             *slog.Logger
	opts            ResolveOptions            // verbatim caller opts (RC already non-nil)
	dict            map[string]any            // the shared serve map (guarded per-stage by dictMu)
	mapper          endpointReferenceMapper   // per-user endpoint reference resolver
	apistageEnabled bool                      // F1 content-keyed api-stage L1 gate (read once)
	apistageStore   *cache.ResolvedCacheStore // nil when apistage disabled / store unavailable
	stageErrSink    *cache.StageErrorSink     // nil on the request path (refresher-only)
	pipTimingSink   *cache.PIPStageTimingSink // nil on the request path (PIP-seed-only)
}

// newResolveRun builds the per-call resolveRun. user is already resolved by
// the caller (Resolve owns the R-pre3 UserInfo-error early-exit, so the
// constructor never has to surface that error). The constructor performs
// P2(mapper)/P3(dict)/P4(gate+sinks) of the original inline flow verbatim —
// including the "base dict for api resolver" Info log at the end of dict
// construction, which MUST fire after the caller's "created api map" Debug
// log (slog event order is gated).
func newResolveRun(ctx context.Context, opts ResolveOptions, log *slog.Logger, user jwtutil.UserInfo) *resolveRun {
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

	log.Debug("base dict for api resolver", slog.Any("dict", dict))

	// Ship F1 (0.30.119): the content-keyed api-stage L1 is active
	// whenever the resolved-output store is on (ApistageL1Enabled folded
	// under the cache master gate per #57). Read the gate + the store
	// handle ONCE before the loop; with the store off both stay inert and
	// every call runs the byte-identical 0.30.118 path.
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

	// Ship 0.30.192 — pure-additive per-stage timing sink. Installed by
	// phase1_pip_seed.go's seedOneRestaction so cost-attribution data
	// flows into a structured slog line. Production /call requests
	// install no sink; pipTimingSink is nil and every recorder branch
	// below is a nil-receiver no-op (no behavioural change).
	pipTimingSink := cache.PIPStageTimingSinkFrom(ctx)

	return &resolveRun{
		ctx:             ctx,
		log:             log,
		opts:            opts,
		dict:            dict,
		mapper:          mapper,
		apistageEnabled: apistageEnabled,
		apistageStore:   apistageStore,
		stageErrSink:    stageErrSink,
		pipTimingSink:   pipTimingSink,
	}
}

// recordItemError is the Ship 0.30.257 (#313) per-item hard-error triad,
// deduplicated from the three worker error branches (in-process-nested-call /
// internal-rest-config / httpcall.Do). It performs, in order:
//
//  1. W-A: accumulate accumVal under the shared dict[errKey] (the per-item
//     errors accumulating-slice), holding mu across the append — the caller's
//     stage-local dictMu (the lock is taken WITHOUT defer, matching the
//     explicit Lock()/Unlock() the three inline error blocks used).
//  2. Layer (b): Bump the stage-error sink with (id, bumpMsg) so the error-
//     aware Put-gate / request-path Cache-A guard see the failure. Nil-
//     receiver-safe (no sink on the request path).
//  3. Option C-A: record itemErr into this item's disjoint slot itemErrs[i].
//     itemErr is nil when the caller's call.ContinueOnError was true (the
//     caller builds it ONLY inside its `!ContinueOnError` gate, preserving
//     the lazy fmt.Errorf and — critically — the per-site %w (nested /
//     internal-rest-config, wrapping an error) vs %s (httpcall, formatting
//     res.Message string) wrapping verbatim).
//
// accumVal and bumpMsg are SEPARATE params because the httpcall branch
// accumulates the asMap (a map, when AsMap succeeded) while it Bumps the
// res.Message string — the two are not always the same value (the other two
// branches pass <err>.Error() for both).
//
// CONCURRENCY: runs inside the iterator errgroup worker; mu serialises the
// dict[errKey] append against peer workers, and itemErrs[i] is a disjoint
// pre-sized slot (no lock needed — same property the inline code relied on).
// Exercised under -race by TestResolve_IteratorErrorCollection_Race.
func (r *resolveRun) recordItemError(mu *sync.Mutex, itemErrs []error, id string, i int, errKey string, accumVal any, bumpMsg string, itemErr error) {
	mu.Lock()
	accumulateErrorKey(r.dict, errKey, accumVal)
	mu.Unlock()
	r.stageErrSink.Bump(id, bumpMsg)
	if itemErr != nil {
		itemErrs[i] = itemErr
	}
}

// stageAction encodes what the per-stage endpoint resolution tells the
// orchestrator to do next. The helper that resolves the endpoint is pure-ish
// (it logs + writes its own C-2 empty result) and returns one of these; the
// ORCHESTRATOR owns the recordStageTiming()-before-exit invariant and the
// loop control-flow (continue / return dict). This keeps every stage-exit's
// recordStageTiming() in ONE place (design §3.2 / §2.4).
type stageAction int

const (
	stageProceed  stageAction = iota // endpoint resolved; continue the stage
	stageContinue                    // skip to next stage (C-2: UAF SA-endpoint err; dict[id] already set)
	stageReturn                      // truncate the resolve (R-2: non-UAF resolveOne err)
)

// resolveStageEndpoint resolves the dispatch Endpoint for one stage. UAF
// stages use the snowplow-SA endpoint (cluster-wide read); non-UAF stages go
// through the per-user clientconfig (or the named EndpointRef). It replaces
// the inline P5e block verbatim, INCLUDING the two slog.Error sites and the
// C-2 empty-result write — but NOT the recordStageTiming()/continue/return,
// which the orchestrator runs based on the returned stageAction.
//
//   - UAF + SA-endpoint err → log; dict[id]={items:[]}; return (zero, stageContinue)
//     (C-2: fail-closed-but-respond, the stage produces an empty result and
//     the loop continues — downstream stages still run).
//   - non-UAF + resolveOne err → log; return (zero, stageReturn)
//     (R-2: the resolve truncates — the orchestrator returns r.dict).
//   - success → return (ep, stageProceed).
func (r *resolveRun) resolveStageEndpoint(id string, apiCall *templates.API, uafActive bool) (endpoints.Endpoint, stageAction) {
	if uafActive {
		saEP, saErr := serviceAccountEndpointFn()
		if saErr != nil {
			r.log.Error("userAccessFilter: cannot acquire ServiceAccount endpoint; falling through to per-user dispatch (degraded mode)",
				slog.String("name", id), slog.Any("err", saErr))
			// Fail-closed-but-respond: per Revision 5 atomic
			// ship there is no toggle to fall back to the
			// per-user path correctly (we'd leak the user
			// bearer token to a SA-marked stage). Returning
			// an empty result for this stage and continuing.
			r.dict[id] = map[string]any{"items": []any{}}
			return endpoints.Endpoint{}, stageContinue
		}
		return *saEP, stageProceed
	}
	// #113 — template endpointRef.name through the SAME jq/extras evaluator that
	// already renders this stage's path/payload/headers (evalJQ over r.dict, the
	// exact ds createRequestOptions is handed at :405), so `.name` resolves
	// identically to how `path` resolves it. ONLY the templated (MaybeQuery-shaped)
	// named-ref path is touched; a nil ref (internal/clientconfig synthesis) and a
	// literal ref pass through byte-identically (§1.3, guardrail (a): namespace
	// stays literal, never templated). Returns templated=true so resolveOne applies
	// the guardrail (b) defense-in-depth layer.
	ref, templated, guardErr := r.evalEndpointRef(apiCall.EndpointRef)
	if guardErr != nil {
		// Guardrail (b) refusal at the eval site (fail-fast, before any Secret
		// lookup): a templated name resolved to the reserved `-clientconfig`
		// internal-identity class. Same truncating posture as any endpoint-resolve
		// failure (R-2 stageReturn) — the Secret is never dialed.
		r.log.Error("templated endpoint reference refused",
			slog.String("name", id), slog.Any("ref", apiCall.EndpointRef), slog.Any("error", guardErr))
		return endpoints.Endpoint{}, stageReturn
	}
	resolved, err := r.mapper.resolveOne(r.ctx, ref, templated)
	if err != nil {
		r.log.Error("unable to resolve api endpoint reference",
			slog.String("name", id), slog.Any("ref", ref), slog.Any("error", err))
		return endpoints.Endpoint{}, stageReturn
	}
	return resolved, stageProceed
}

// evalEndpointRef renders a templated endpointRef.name (#113). For a nil ref or
// a non-templated (literal) name it returns the ref UNCHANGED with templated=false
// (byte-identical to pre-#113 — the internal nil-ref synthesis and static author
// refs are untouched). For a MaybeQuery-shaped name it evaluates the name through
// evalJQ against r.dict (the SAME per-stage dict path/payload/headers see, so
// `.name` resolves identically), RFC1123-sanitizes the result, applies guardrail
// (a) (namespace stays the author-literal — NEVER templated) and guardrail (b)
// (a resolved `-clientconfig` name is REFUSED here, fail-fast, so the Secret
// lookup never fires), and returns templated=true so resolveOne applies its
// defense-in-depth reserved-suffix refusal too. A jq error yields evalJQ's
// error-string, which is sanitized like any other miss and resolves to a Secret
// miss downstream (§1.2 honest-error posture) — not a new error class.
func (r *resolveRun) evalEndpointRef(ref *templates.Reference) (*templates.Reference, bool, error) {
	if ref == nil {
		return ref, false, nil
	}
	if _, isTemplate := jqutil.MaybeQuery(ref.Name); !isTemplate {
		// Static/literal name (incl a literal `-clientconfig` internal ref, which
		// is the author's own business): pass through, NOT templated, NOT gated.
		return ref, false, nil
	}
	name := kubeutil.MakeDNS1123Compatible(evalJQ(ref.Name, r.dict))
	// Guardrail (b), layer 1 (fail-fast): a request-templated name may never
	// resolve to the reserved `<user>-clientconfig` internal-identity class.
	if strings.HasSuffix(name, clientConfigSuffix) {
		return nil, true, fmt.Errorf(
			"templated endpointRef.name %q resolved to %q, which carries the reserved internal-identity suffix %q; refusing at the template-eval site (#113 guardrail b) — user query-extras may not select a per-user credential Secret",
			ref.Name, name, clientConfigSuffix)
	}
	// Guardrail (a): namespace is NEVER templated in V1 — it stays the
	// author-authored literal, so a templated name can only ever select WITHIN
	// the namespace the RA author already chose (the credential-store trust
	// boundary). templated=true so resolveOne applies guardrail (b) again (§2
	// two-layer defense).
	return &templates.Reference{Name: name, Namespace: ref.Namespace}, true, nil
}

// collapseOrFanoutPlan builds the per-stage RequestOptions slice (the call
// plan). It replaces the inline P5f+P5g+P5h block verbatim: createRequestOptions
// (empty → the "empty request options" Warn + an empty return, which the
// orchestrator treats as C-3), then the Ship D.5 cluster-list collapse
// (rebinding tmp + setting the st timing fields), then the 0.30.92 lazy
// informer register. st is the per-stage timing struct (a loop-local value,
// passed by pointer) — its ClusterList* fields are written here exactly as
// before. Returns tmp (possibly empty for C-3).
func (r *resolveRun) collapseOrFanoutPlan(id string, apiCall *templates.API, ep endpoints.Endpoint, st *cache.PIPStageTiming) []httpcall.RequestOptions {
	tmp := createRequestOptions(r.ctx, r.log, apiCall, r.dict)
	if len(tmp) == 0 {
		// C-3 (a normal "continue" condition): an iterator stage evaluated to
		// zero items, so there is nothing to call. Benign and per-resolve —
		// DEBUG, not the WARN that dominates the healthy-cluster firehose.
		r.log.Debug("empty request options for http call", slog.Any("name", id))
		return tmp
	}

	// Ship D.5 (0.30.152) — cluster-list-when-allowed iterator
	// collapse. When the RA opts in AND the requester holds
	// cluster-scope `list` on the target GVR AND
	// cache + Ship B snapshot are ready, attemptClusterListCollapse
	// pre-dispatches a SINGLE cluster-scope LIST, validates its
	// shape (AC-D5.14), Puts the envelope under the identity-free
	// apistage key, and returns a 1-element tmp slice. The existing
	// worker loop then runs apistageContentServe which Get-hits the
	// cache + applies the per-user gateContentEnvelope narrowing —
	// no double-dispatch, no special-case in the worker.
	//
	// Default-off + RBAC-deny + shape-fail all yield
	// useClusterList=false; tmp keeps its iterator fan-out and the
	// path is byte-identical to pre-D.5 (AC-D5.6).
	//
	// Ship 0.30.192 — capture cluster_list outcome + deny gate for
	// per-stage timing instrumentation. The third return value is
	// the deny-gate number (0 = no deny, 2-7 = which gate); see
	// cache/pip_stage_timing.go PIPStageTiming.ClusterListDenyGate
	// for the value table.
	//
	// Ship S.1 — the per-RA opt-in field (ClusterListWhenAllowed) was
	// removed. attemptClusterListCollapse is now reached for every
	// iterator stage (its own gates 2-5 decide servability), so
	// ClusterListAttempted is true whenever the resolver evaluates the
	// gate. The old `ptr.Deref(apiCall.ClusterListWhenAllowed, false)`
	// gate-attempt signal is gone with the field.
	st.ClusterListAttempted = true
	if newTmp, useClusterList, denyGate := attemptClusterListCollapse(
		r.ctx, r.log, apiCall, r.dict, ep, r.apistageStore, r.apistageEnabled); useClusterList {
		tmp = newTmp
		st.ClusterListUsed = true
		st.ClusterListDenyGate = denyGate // 0 on success
	} else {
		st.ClusterListUsed = false
		st.ClusterListDenyGate = denyGate
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
	lazyRegisterInnerCallPaths(r.ctx, r.log, tmp)
	return tmp
}

// stageCtx is the per-STAGE mutable bundle: the values that are re-created
// each stage iteration and passed to the iterator workers. It is built inside
// the stage loop and discarded at the end of that iteration — it is
// DELIBERATELY NOT a resolveRun field (the concurrency flag, design §3 /
// PM CONDITION 2): dictMu/g/gctx/itemErrs/iterStart/wireVerbose are per-stage
// and would alias across stages or outlive their stage if promoted to fields.
// id/apiCall/ep/calls are the per-stage data the worker reads. A pointer to
// one stageCtx is shared by all of THIS stage's workers (the same sharing the
// inline closure had — read-only for ep/calls/id/apiCall; dictMu guards dict;
// itemErrs[i] is a disjoint pre-sized slot).
type stageCtx struct {
	id          string
	apiCall     *templates.API
	dictMu      *sync.Mutex
	g           *errgroup.Group
	gctx        context.Context
	itemErrs    []error
	iterStart   time.Time
	wireVerbose bool
	ep          endpoints.Endpoint
	calls       []httpcall.RequestOptions
}

// dispatchOneCall is one iterator item's worker — the body the stage loop
// hands to sc.g.Go. It moves the P5j per-call setup (the R1-a verbose Endpoint
// copy + the three dictMu-protected feed closures) AND the 4-branch dispatch
// cascade (in-process nested /call → informer pivot / apistage-content →
// internal-rest-config → httpcall.Do) verbatim. i is passed explicitly (NOT
// captured) so itemErrs[i] is the item's disjoint slot (design Risk R1).
//
// ctx vs gctx (design Risk R2, TRACED): the dispatchers + handlers receive
// sc.gctx (the errgroup ctx, so a cancelled peer aborts in-flight calls);
// depthForLog receives the OUTER r.ctx (it only reads the DEBUG env gate, and
// the inline worker passed the outer ctx at every site — preserved exactly).
//
// Returns error ONLY for a response-HANDLER failure (feed* returning a
// decode/jq err) — per-item hard errors are recorded into sc.itemErrs[i] via
// recordItemError and return nil (Option C-A), so g.Wait() does not cancel the
// stage on them.
func (r *resolveRun) dispatchOneCall(sc *stageCtx, i int) error {
	id := sc.id
	apiCall := sc.apiCall
	dictMu := sc.dictMu
	gctx := sc.gctx
	itemErrs := sc.itemErrs
	dict := r.dict

	call := sc.calls[i]
	// Ship 0.30.121 R1-a — per-call verbose decision. `sc.ep` is shared
	// across every call of this stage; setting Debug in place would race the
	// concurrent workers. When this call wants the wire-dump, take a SHALLOW
	// COPY of the Endpoint value and flip Debug on the copy, so peers keep the
	// un-Debugged shared Endpoint. A K8s collection LIST never wants it
	// (callWantsWireDump returns false) — that suppresses the ~1.94 GiB
	// DumpResponse alloc_space line.
	if callWantsWireDump(sc.wireVerbose, call.Path) {
		epCopy := sc.ep
		epCopy.Debug = true
		call.Endpoint = &epCopy
	} else {
		call.Endpoint = &sc.ep
	}
	// Wrap jsonHandler so every dict[id] mutation goes through
	// dictMu. Three forms — Ship 0.30.128 P-CORE-1/2:
	//   * inner (io.ReadCloser) — the genuine httpcall.Do HTTP-body
	//     path; set as call.ResponseHandler.
	//   * innerBytes ([]byte) — an in-memory dispatch result whose
	//     bytes are already materialised (informer-served, nested
	//     /call, internal-rest-config); skips the redundant
	//     io.ReadAll copy.
	//   * innerValue (any) — an apistage content-cache hit whose
	//     gated envelope is already a decoded structured value;
	//     skips both the marshal and the unmarshal.
	// Ship 0.30.235 — UAF spec + stage name plumbed in so
	// jsonHandlerCore can run the refilter on the RAW envelope
	// BEFORE the stage filter projects items. The pre-0.30.235
	// post-g.Wait() applyUserAccessFilter call is DELETED.
	// Ship K / 0.30.245 — `dict` plumbs the resolver's
	// accumulated stage-output dict into jsonHandlerCore so the
	// UAF refilter's resolveUAFResources evaluates
	// resourcesFrom against UPSTREAM stage outputs (e.g.
	// `.crds` from a prior stage). Same map as `out`; refilter
	// reads BEFORE the merge so the current stage is absent
	// (correct — resourcesFrom never references the stage it
	// gates). Pre-Ship-K passed only `pig` (per-stage scope) →
	// resourcesFrom returned [] → 0-count regression on the
	// multi-stage compositions-list RA.
	hOpts := jsonHandlerOptions{
		key:         id,
		out:         dict,
		dict:        dict,
		filter:      apiCall.Filter,
		uaf:         apiCall.UserAccessFilter,
		apiCallName: apiCall.Name,
		// PURE request extras (NOT dict — dict has accumulated upstream stage
		// outputs + a synthetic slice). Exposed to the step filter as the
		// reserved sibling key pig["extras"]. Shared reference: gojq COW fork.
		extras: r.opts.Extras,
	}
	inner := jsonHandler(gctx, hOpts)
	innerBytesFn := jsonHandlerBytes(gctx, hOpts)
	innerValueFn := jsonHandlerValue(gctx, hOpts)
	call.ResponseHandler = func(r io.ReadCloser) error {
		dictMu.Lock()
		defer dictMu.Unlock()
		return inner(r)
	}
	// feedBytes / feedValue are the dictMu-protected in-memory
	// equivalents the readerFromBytes call sites below use instead
	// of call.ResponseHandler(readerFromBytes(...)).
	feedBytes := func(b []byte) error {
		dictMu.Lock()
		defer dictMu.Unlock()
		return innerBytesFn(b)
	}
	feedValue := func(v any) error {
		dictMu.Lock()
		defer dictMu.Unlock()
		return innerValueFn(v)
	}

	r.log.Debug("calling api", slog.String("name", id),
		slog.String("host", call.Endpoint.ServerURL),
		slog.String("path", call.Path),
	)

	// RETIRED (2026-06-22 unified ship): the /call?resource=...&apiVersion=...
	// LOOPBACK dispatch branch (Ship 0.30.123 #155) was removed. The corpus
	// audit (docs/corpus-audit-call-loopback-2026-06-22.md §1/§2) confirmed
	// ZERO live RAs carry a /call api-step path, so the loopback resolve
	// branch fired on no path today — dead code. The resolve LOGIC it called
	// (dispatchers.ResolveNestedCall) SURVIVES, repurposed as the in-process
	// resolver behind the new DIRECT-APISERVER-PATH + `resolve: true`
	// mechanism (maybeResolveInProcess below). The shared `/call`-URL parser
	// objects.ParseCallPathToObjectRef + buildPath emission STAY — they are
	// the SPA/F2-walker navigation contract (mechanism A), independent of this
	// resolve branch (audit §5 "the shared-parser trap").
	//
	// In-process resolve substitution (the `resolve: true` mechanism). A step
	// whose `path` is a direct apiserver path to a RESTAction/Widget CR is
	// fetched (cacheably, dep-tracked) by the informer-pivot / apistage /
	// internal-rest-config branches below, which yield the RAW CR envelope.
	// With resolve:true (OPT-IN) we then run that CR through the resolver
	// IN-PROCESS via maybeResolveInProcess and feed the RESOLVED envelope
	// instead of the raw CR — byte-identical to an HTTP /call of that CR, no
	// outbound HTTP. resolve:false, OMITTED (nil pointer), or a non-RA/widget
	// path / a LIST / cache off feeds the raw bytes unchanged. Computed once
	// per call.
	//
	// The 2nd arg (nil fallback) is LOAD-BEARING as of the 2026-07-02 contract
	// flip (docs/resolve-default-flip-plan-2026-07-02.md §1): the CRD no longer
	// carries +kubebuilder:default=true, so an omitted `resolve` arrives here as
	// a NIL pointer and this fallback decides its meaning. It MUST be false to
	// agree with the CRD contract (omit→false). This is coupled to the CRD
	// change: removing the CRD default (nil reachable) + this false fallback
	// (nil means false) must land together, or omitted-resolve behaviour would
	// diverge from what the schema advertises. (Before the flip, the apiserver
	// injected non-nil true on every omit, so this fallback was inert.)
	resolve := ptr.Deref(apiCall.Resolve, false)

	// feedRawOrResolved feeds the dispatched RAW envelope bytes downstream,
	// UNLESS the step is a resolve:true direct-path RA/widget GET — in which
	// case it substitutes the in-process resolved envelope. dispatch labels
	// the calling branch for the success/error log. A resolve error is routed
	// through the SAME recordItemError triad an HTTP dispatch error uses (so a
	// denied/depth-capped resolve surfaces a 403-class error, NOT empty
	// content, and — under #313 Option C-A — does not truncate downstream
	// stages). Returns the dispatch label to log (suffixed "-resolved" /
	// "-resolve-error" when the in-process resolve fired).
	feedRawOrResolved := func(raw []byte, dispatch string) string {
		substituted, did, rerr := r.maybeResolveInProcess(gctx, call, resolve)
		if rerr != nil {
			r.log.Error("in-process resolve failed",
				slog.String("name", id),
				slog.String("path", call.Path),
				slog.String("dispatch", dispatch+"-resolve"),
				slog.String("error", rerr.Error()))
			var itemErr error
			if !call.ContinueOnError {
				itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, rerr)
			}
			r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, rerr.Error(), rerr.Error(), itemErr)
			return dispatch + "-resolve-error"
		}
		bytesToFeed := raw
		if did {
			bytesToFeed = substituted
			dispatch += "-resolved"
		}
		if ferr := feedBytes(bytesToFeed); ferr != nil {
			r.log.Error("response handler failed",
				slog.String("name", id),
				slog.String("path", call.Path),
				slog.String("dispatch", dispatch),
				slog.String("error", ferr.Error()))
			var itemErr error
			if !call.ContinueOnError {
				itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, ferr)
			}
			r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, ferr.Error(), ferr.Error(), itemErr)
		}
		return dispatch
	}

	// 0.30.95 resolver pivot — dispatch GET reads to the
	// informer cache when the cache subsystem is on. #57:
	// implicit-on-cache — resolverUseInformer() now folds to
	// !cache.Disabled() (the standalone RESOLVER_USE_INFORMER
	// flag was retired). Cache OFF: this branch is byte-
	// identical to the apiserver path (R-FALSE-1 invariant).
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
	if resolverUseInformer() {
		if r.apistageEnabled {
			if gatedVal, served, ok := apistageContentServe(gctx, r.apistageStore, call, apiCall.UserAccessFilter != nil); ok {
				if served {
					// Ship 0.30.128 P-CORE-2: the gated envelope is
					// already a decoded structured value — feed it
					// direct (no marshal, no unmarshal).
					//
					// In-process resolve (resolve:true direct RA/widget
					// GET): the apistage content layer ALSO serves single-
					// CR GET-by-name (apistage.go isList==false path), so a
					// resolve:true direct-path RA/widget GET can land here.
					// When maybeResolveInProcess substitutes, feed the
					// RESOLVED bytes instead of the gated raw value (it
					// re-fetches + resolves via the seam under the outer L1
					// key — the gated value was the raw CR). Otherwise feed
					// the gated value unchanged (LIST, non-RA/widget,
					// resolve:false). dispatch is labelled accordingly.
					//
					// Build backlog #6 — a feed* error (the per-item stage
					// FILTER zero-/multi-yield, handler.go:142/154) is a
					// per-ITEM hard error: route it through the SAME
					// recordItemError triad, NOT a raw `return err` (which
					// truncated the stage + ALL downstream stages — the
					// #313 C-A violation). The feed error IS an error → %w
					// wrap. Fall through to the success-log + return nil so
					// downstream items + stages still run.
					dispatch := "apistage-content"
					substituted, did, rerr := r.maybeResolveInProcess(gctx, call, resolve)
					if rerr != nil {
						r.log.Error("in-process resolve failed",
							slog.String("name", id),
							slog.String("path", call.Path),
							slog.String("dispatch", "apistage-content-resolve"),
							slog.String("error", rerr.Error()))
						var itemErr error
						if !call.ContinueOnError {
							itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, rerr)
						}
						r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, rerr.Error(), rerr.Error(), itemErr)
						dispatch = "apistage-content-resolve-error"
					} else {
						var ferr error
						if did {
							ferr = feedBytes(substituted)
							dispatch = "apistage-content-resolved"
						} else {
							ferr = feedValue(gatedVal)
						}
						if ferr != nil {
							r.log.Error("apistage-content response handler failed",
								slog.String("name", id),
								slog.String("path", call.Path),
								slog.String("dispatch", dispatch),
								slog.String("error", ferr.Error()))
							var itemErr error
							if !call.ContinueOnError {
								itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, ferr)
							}
							r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, ferr.Error(), ferr.Error(), itemErr)
						}
					}
					// Ship #6 — see depthForLog (support.go).
					depth := depthForLog(r.ctx, r.log, dictMu, dict)
					r.log.Debug("api successfully resolved",
						slog.String("name", id),
						slog.String("host", call.Endpoint.ServerURL),
						slog.String("path", call.Path),
						slog.Int("depth", depth),
						slog.String("dispatch", dispatch),
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
			// Informer-served bytes are in memory — feed direct
			// (Ship 0.30.128 P-CORE-1 — no io.ReadAll copy). For a
			// resolve:true direct-path RA/widget GET, feedRawOrResolved
			// substitutes the in-process resolved envelope; otherwise it
			// feeds raw unchanged. Feed/resolve errors are routed through
			// recordItemError (no truncation — #313 C-A); the returned
			// dispatch label captures whether the in-process resolve fired.
			dispatch := feedRawOrResolved(raw, "informer")
			// Ship #6 — see depthForLog (support.go).
			depth := depthForLog(r.ctx, r.log, dictMu, dict)
			r.log.Debug("api successfully resolved",
				slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL),
				slog.String("path", call.Path),
				slog.Int("depth", depth),
				slog.String("dispatch", dispatch),
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
		dispatch := "internal-rest-config"
		if ierr != nil {
			r.log.Error("api call response failure", slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL),
				slog.String("path", call.Path),
				slog.String("dispatch", "internal-rest-config"),
				slog.String("error", ierr.Error()))
			// Ship 0.30.257 (#313) W-A + layer (b) + Option C-A,
			// deduplicated into recordItemError. The %w-wrapped cause
			// moves from g.Wait() to itemErrs[i], built lazily only
			// when !ContinueOnError. We STILL must NOT fall through to
			// httpcall.Do here (the internal dispatcher OWNED this
			// call; re-hitting plumbing's broken TLS path would mask
			// the real error behind a second x509 failure) — the
			// success-log line + return below covers both the error
			// and the fed-bytes case.
			var itemErr error
			if !call.ContinueOnError {
				itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, ierr)
			}
			r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, ierr.Error(), ierr.Error(), itemErr)
			dispatch = "internal-rest-config-error"
		} else {
			// internal-rest-config dispatch result is in memory — feed
			// direct (Ship 0.30.128 P-CORE-1). For a resolve:true
			// direct-path RA/widget GET, feedRawOrResolved substitutes the
			// in-process resolved envelope; otherwise it feeds raw
			// unchanged. Feed/resolve errors route through recordItemError
			// (no truncation — #313 C-A); the returned dispatch label
			// captures whether the in-process resolve fired.
			dispatch = feedRawOrResolved(raw, "internal-rest-config")
		}
		// Ship #6 — see depthForLog (support.go).
		depth := depthForLog(r.ctx, r.log, dictMu, dict)
		r.log.Debug("api successfully resolved",
			slog.String("name", id),
			slog.String("host", call.Endpoint.ServerURL),
			slog.String("path", call.Path),
			slog.Int("depth", depth),
			slog.String("dispatch", dispatch),
		)
		return nil
	}

	// Fix A1 — BARE group-discovery dispatch branch (CA-bearing SA
	// transport). A composition-resources RA issues an api-step GET against
	// a bare group-discovery URL /apis/<g>/<v> (no resource segment, no
	// endpointRef) to enumerate a managed apiVersion's served resources.
	// That 2-segment path parse-fails ParseAPIServerPathToDep, so it fell
	// through every CA-bearing branch above to the external fetch, which
	// builds a plumbing client from the per-user <user>-clientconfig TOKEN-
	// auth Endpoint — plumbing's tlsConfigFor drops the cluster caData for a
	// token-auth endpoint (HasCertAuth()-only CA install) → x509: certificate
	// signed by unknown authority. TRACED:
	// docs/troubleshoot-discovery-url-apistep-x509-2026-06-23.md. Same
	// plumbing TLS defect internal_dispatch.go documents for the Phase-1 SA
	// path (0.30.104).
	//
	// dispatchViaDiscovery serves the discovery SHAPE only (a resource path
	// is NEVER routed here — ParseAPIServerDiscoveryPath returns false for
	// ≥3-segment paths — so it keeps its per-user-token branch byte-
	// unchanged; the SA-serve exemption is sound because group discovery is
	// anonymous-readable and carries NO tenant data). The branch fires
	// regardless of CACHE_ENABLED (the SA rc is the process-wide
	// dynamic.ServiceAccountRESTConfig() singleton, present cache-on AND
	// cache-off — AC5 transparent fallback) and BEFORE the external fetch,
	// so the bare-discovery GET never reaches the broken plumbing TLS path.
	//
	// The served body is the marshalled *metav1.APIResourceList. It is fed
	// through the SAME dictMu-protected handler chain (feedBytes) the other
	// dispatch branches use — it is NOT a resolve:true RA/widget single-CR
	// GET, so we do NOT run maybeResolveInProcess on it. A non-nil err is
	// the REAL discovery error; like the internal-rest-config branch we do
	// NOT fall through to the external fetch (that would re-hit the broken
	// plumbing TLS path and mask the error behind a second x509 failure) —
	// we surface it via recordItemError exactly as an httpcall.Do
	// StatusFailure (ContinueOnError / ErrorKey honoured).
	if raw, served, derr := dispatchViaDiscovery(gctx, call); served || derr != nil {
		dispatch := "discovery"
		if derr != nil {
			r.log.Error("api call response failure", slog.String("name", id),
				slog.String("host", call.Endpoint.ServerURL),
				slog.String("path", call.Path),
				slog.String("dispatch", "discovery"),
				slog.String("error", derr.Error()))
			var itemErr error
			if !call.ContinueOnError {
				itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, derr)
			}
			r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, derr.Error(), derr.Error(), itemErr)
			dispatch = "discovery-error"
		} else if ferr := feedBytes(raw); ferr != nil {
			r.log.Error("discovery response handler failed",
				slog.String("name", id),
				slog.String("path", call.Path),
				slog.String("dispatch", dispatch),
				slog.String("error", ferr.Error()))
			var itemErr error
			if !call.ContinueOnError {
				itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, ferr)
			}
			r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, ferr.Error(), ferr.Error(), itemErr)
		}
		depth := depthForLog(r.ctx, r.log, dictMu, dict)
		r.log.Debug("api successfully resolved",
			slog.String("name", id),
			slog.String("host", call.Endpoint.ServerURL),
			slog.String("path", call.Path),
			slog.Int("depth", depth),
			slog.String("dispatch", dispatch),
		)
		return nil
	}

	// EXTERNAL branch (feat/restaction-yaml-response): the snowplow-owned
	// fetch replaces plumbing's httpcall.Do. It transcribes request.Do
	// MINUS the 406 JSON content-type gate, so a 2xx YAML body (e.g. a
	// Helm repo index.yaml served text/plain or text/yaml) is read,
	// converted to JSON, and fed onward; a 2xx JSON body passes through
	// value-identical (AC3). It does NOT invoke call.ResponseHandler —
	// instead it returns the converted JSON bytes, which we feed via
	// feedBytes (the dictMu-protected jsonHandlerBytes path) so the EXISTING
	// handler chain (jq filter + UAF refilter + merge) is unchanged. The
	// returned *response.Status keeps the StatusFailure shaping below
	// byte-identical, so recordItemError honours ContinueOnError/ErrorKey
	// exactly as it did for httpcall.Do (AC5). See external_fetch.go.
	//
	// In-process resolve on the CACHE-OFF path (Diego ruling 2026-06-22,
	// Option (ii) / project_cache_off_is_transparent_fallback). Under cache-off
	// the informer-pivot / apistage / internal-rest-config branches above are
	// all skipped (resolverUseInformer()==false; no internal rc on a per-user
	// request), so a direct-apiserver-path RA/widget GET with resolve:true
	// reaches THIS external fall-through. To keep resolve:true a TRANSPARENT
	// fallback — identical resolved data cache-on AND cache-off — we run the
	// in-process resolve substitution HERE too, BEFORE the external fetch +
	// the external-touched bump. maybeResolveInProcess only fires for a
	// resolve:true single-CR apiserver-path GET of a RESTAction/Widget (Gate 4
	// rejects external/templated/LIST/non-RA-widget paths), and under cache-off
	// the seam's objects.Get uses the USER's token (getFromAPIServer) — the
	// authoritative cache-off RBAC gate — and ResolveNestedCall's in-process
	// checkDispatchRBAC is itself !cache.Disabled()-gated, so the user-token
	// model is honoured, not the SA model. On a substitution: feed the resolved
	// envelope, do NOT external-fetch, do NOT bump the external sink (this is an
	// internal apiserver path served in-process, NOT a genuine external
	// endpoint), and return. A genuine external path (parseOK=false) does NOT
	// substitute → falls through to the external fetch + bump unchanged.
	if substituted, did, rerr := r.maybeResolveInProcess(gctx, call, resolve); did || rerr != nil {
		dispatch := "cache-off-inprocess-resolve"
		if rerr != nil {
			r.log.Error("in-process resolve failed (cache-off path)",
				slog.String("name", id),
				slog.String("path", call.Path),
				slog.String("dispatch", dispatch),
				slog.String("error", rerr.Error()))
			var itemErr error
			if !call.ContinueOnError {
				itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, rerr)
			}
			r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, rerr.Error(), rerr.Error(), itemErr)
			dispatch = "cache-off-inprocess-resolve-error"
		} else if ferr := feedBytes(substituted); ferr != nil {
			r.log.Error("in-process resolve response handler failed (cache-off path)",
				slog.String("name", id),
				slog.String("path", call.Path),
				slog.String("dispatch", dispatch),
				slog.String("error", ferr.Error()))
			var itemErr error
			if !call.ContinueOnError {
				itemErr = fmt.Errorf("api %s item %d failed: %w", id, i, ferr)
			}
			r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, ferr.Error(), ferr.Error(), itemErr)
		}
		depth := depthForLog(r.ctx, r.log, dictMu, dict)
		r.log.Debug("api successfully resolved",
			slog.String("name", id),
			slog.String("host", call.Endpoint.ServerURL),
			slog.String("path", call.Path),
			slog.Int("depth", depth),
			slog.String("dispatch", dispatch),
		)
		return nil
	}

	// External-no-cache (proposal 2026-06-22) — THE external-touched bump.
	// Reaching this line is the AUTHORITATIVE signal that this stage touched
	// a genuine EXTERNAL endpoint: the loopback (retired) / informer-pivot /
	// apistage / internal-rest-config branches all `return nil` BEFORE here,
	// AND the cache-off in-process resolve above did not fire (a genuine
	// external path, or resolve:false, or a non-RA/widget path), so control
	// reaches httpFetchAllowingNonJSON only on the external fall-through
	// (proposal §"Detection" — mis-classifying an internal branch as external
	// is structurally impossible because the signal is the dispatch SITE, not
	// the Endpoint shape). The sink is shared across this stage's iterator
	// errgroup workers; Bump is atomic + nil-safe (no sink installed → no-op).
	// Each of the 5 L1 Put surfaces reads Count()>0 and declines the Put (the
	// result is still SERVED — identical to the #313 partial posture), so
	// external data is re-fetched LIVE every /call and never persisted under a
	// TTL it has no dep edge to invalidate.
	cache.ExternalTouchedSinkFromContext(gctx).Bump()
	// Boot-readiness external-fetch wall-clock bound (external_seed_bound.go).
	// The Bump() STAYS before the wrap so a timed-out fetch still records the
	// external touch → the L1 Put is still declined → uniform with the
	// refresher. On a boot readiness-critical scope (seed / discovery walk /
	// content-prewarm pass) fctx carries a per-fetch deadline that truncates
	// the WHOLE retry envelope (util.RetryClient honors req.Context() at every
	// retry/backoff boundary); on /call + refresher scopes it is the parent
	// gctx + a no-op cancel, so the fetch is byte-identical to the pre-fix
	// path.
	fctx, fcancel := externalSeedFetchCtx(gctx)
	defer fcancel()
	res, jsonBytes, _, fetchErr := httpFetchAllowingNonJSON(fctx, call)
	if fetchErr != nil {
		// A non-nil go error mirrors httpcall.Do's response.New(500, err)
		// transport/build faults. res already carries the same 500 Failure
		// envelope; fall through to the StatusFailure shaping below so the
		// behaviour matches the pre-ship httpcall.Do path (which surfaced
		// these as res.Status == Failure with the error message).
		_ = fetchErr
	}

	// SUCCESS (feat/restaction-yaml-response): on a non-Failure fetch the
	// owned fetch does NOT invoke call.ResponseHandler (unlike httpcall.Do);
	// it returned the converted JSON bytes. Feed them through the SAME
	// dictMu-protected handler chain the in-memory dispatch paths use
	// (feedBytes → jsonHandlerBytes → jsonHandlerCore: jq filter + UAF
	// refilter + merge), so the populated dict[id] is byte-identical to the
	// pre-ship httpcall.Do ResponseHandler result for a JSON body (AC3).
	//
	// B-REGRESSION FIX: a feedBytes (handler/jq decode) error MUST NOT be
	// returned raw to the errgroup — pre-ship that error was the return
	// value of call.ResponseHandler, which httpcall.Do wrapped as
	// `response.New(http.StatusInternalServerError, err)` (request.go:121-126)
	// → a StatusFailure → the recordItemError fall-through below → under #313
	// C-A NO truncation (downstream stages still run). Returning the raw error
	// truncated the whole resolve where pre-ship continued (empirically
	// proven: oracle TestOracle_PreShip dict keys [badErr good] vs new []).
	// So we shape the feedBytes error into the SAME 500 StatusFailure
	// envelope and route it through the identical recordItemError handling —
	// byte-identical to the pre-ship ResponseHandler-error path. (Falsifier
	// TestFalsifierB_SuccessBranchDecodeFailure_NoTruncate.)
	if res.Status != response.StatusFailure {
		if err := feedBytes(jsonBytes); err != nil {
			res = response.New(http.StatusInternalServerError, err)
		}
	}

	if res.Status == response.StatusFailure {
		r.log.Error("api call response failure", slog.String("name", id),
			slog.String("host", call.Endpoint.ServerURL),
			slog.String("path", call.Path),
			slog.String("error", res.Message))

		asMap, mapErr := response.AsMap(res)
		if mapErr != nil {
			r.log.Warn("unable to encode status as dict", slog.Any("err", mapErr))
		}

		// Ship 0.30.257 (#313) W-A + layer (b) + Option C-A,
		// deduplicated into recordItemError. The accumulated VALUE
		// is the asMap (when AsMap succeeded) else the raw message
		// string — distinct from the Bump message (always
		// res.Message), so the two are passed separately. res.Message
		// is a string (not an error), so the item error wraps it
		// with %s (mirrors the pre-0.30.257 g.Wait()-returned shape),
		// built lazily only when !ContinueOnError; it nonetheless
		// aggregates via errors.Join at post-g.Wait().
		var accumVal any = res.Message
		if len(asMap) > 0 {
			accumVal = asMap
		}
		var itemErr error
		if !call.ContinueOnError {
			itemErr = fmt.Errorf("api %s item %d failed: %s", id, i, res.Message)
		}
		r.recordItemError(dictMu, itemErrs, id, i, call.ErrorKey, accumVal, res.Message, itemErr)
		// Fall through to the success-log line (preserves
		// pre-0.30.95 behaviour where the
		// "successfully resolved" line emitted on every
		// non-hard-error call).
	}

	// Ship #6 — depthForLog runs mapDepth (the full-tree walk)
	// under dictMu ONLY when Debug is enabled — serialising the
	// read against concurrent jsonHandler writes. On the common
	// (Info) path it does no work and takes no lock.
	depth := depthForLog(r.ctx, r.log, dictMu, dict)
	r.log.Debug("api successfully resolved",
		slog.String("name", id),
		slog.String("host", call.Endpoint.ServerURL),
		slog.String("path", call.Path),
		slog.Int("depth", depth),
	)
	return nil
}

// runStage orchestrates ONE api stage (P5a-P5k of the original inline loop).
// It returns stop=true when the resolve must terminate and the orchestrator
// should `return r.dict` (the three truncating exits: R-1 caller-cancel, R-2
// endpoint-resolve err, R-3 g.Wait() hard error); stop=false means "move to
// the next stage" (normal completion + the three continues: C-1 apiMap miss,
// C-2 UAF SA-endpoint err, C-3 empty request options).
//
// CENTRALISED INVARIANT (design §2.4): every stage-exit that is not the
// pre-timing C-1 (apiMap miss, which returns before BeginStage so it commits
// no PIP stage — preserved exactly) calls recordStageTiming() exactly once
// before returning, so pipTimingSink.EndStage commits the in-flight stage.
// Keeping all of runStage's returns in one method makes that invariant
// auditable in a single place (and immune to "forgot recordStageTiming on a
// new exit path"). recordStageTiming stays a per-stage closure (it closes over
// the per-stage stageStart/stageTiming — NOT lifted to a method/field, which
// would re-introduce per-stage state as struct state).
//
// The per-stage mutable primitives (dictMu, g, gctx, itemErrs, iterStart,
// wireVerbose) are method-locals here, bundled into the method-local stageCtx
// — never resolveRun fields (the concurrency flag / PM CONDITION 2).
func (r *resolveRun) runStage(id string, apiMap map[string]*templates.API) (stop bool) {
	// Get the api with this identifier
	apiCall, ok := apiMap[id]
	if !ok {
		r.log.Warn("api not found in apiMap", slog.Any("name", id))
		return false
	}
	if apiCall.Headers == nil {
		apiCall.Headers = []string{headerAcceptJSON}
	}

	// Ship 0.30.192 — per-stage timing recorder. stageTiming is a
	// stack-local value; recordStageTiming closes over it and the
	// outer pipTimingSink. On a nil sink (production /call path)
	// recordStageTiming is still called (one nil-receiver no-op);
	// no behavioural change. Stage early-exits (continue / return
	// dict) MUST call recordStageTiming() first so the failed
	// stage's wall-clock is captured for cost attribution.
	//
	// Ship 0.30.193 — register the stage with the sink so workers'
	// AccumulateContentServe / AccumulateMemoPopulate /
	// AccumulateDefensive calls (called from concurrent goroutines
	// inside the iterator errgroup) can accumulate into THIS
	// stage's struct. BeginStage takes a *PIPStageTiming pointer;
	// recordStageTiming calls EndStage which COMMITS the in-flight
	// stage to the sink's stages slice. A nil sink makes both
	// BeginStage and EndStage no-ops.
	stageStart := time.Now()
	stageTiming := cache.PIPStageTiming{StageID: id}
	r.pipTimingSink.BeginStage(&stageTiming)
	recordStageTiming := func() {
		stageTiming.ElapsedMs = time.Since(stageStart).Milliseconds()
		if r.pipTimingSink != nil {
			// Ship 0.30.193 — sink-aware finalisation. EndStage
			// commits the in-flight stage; Append is no longer
			// invoked because the stage is already tracked under
			// the sink's current pointer. The previously-set
			// ElapsedMs / IteratorElapsedMs / ClusterListUsed /
			// ClusterListDenyGate fields are captured by the
			// EndStage copy.
			r.pipTimingSink.EndStage()
		}
	}

	// Ship 0.30.257 (#313) — genuine-cancellation guard (design
	// §2.1.1 Option C-A). The iterator errgroup no longer cancels
	// gctx on a per-ITEM hard error (workers record into itemErrs and
	// return nil — see the worker branches + post-g.Wait() join
	// below), so the ONLY thing that aborts the resolve early is a
	// GENUINE caller cancellation (client disconnect / deadline)
	// propagating through the request ctx. We removed the WORKER as a
	// cancellation SOURCE, not the parent-ctx machinery: a cancelled
	// caller still cancels gctx (errgroup.WithContext derives gctx from
	// ctx) so each in-flight dispatch fails fast, and THIS guard at the
	// stage-loop top makes the cancellation abort downstream STAGES
	// promptly instead of resolving them against a dead ctx. Cheap (one
	// atomic load) and inert on the happy path.
	if err := r.ctx.Err(); err != nil {
		r.log.Debug("api.Resolve: caller context cancelled; aborting remaining stages",
			slog.String("name", id), slog.Any("err", err))
		recordStageTiming()
		return true
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

	// Resolve the endpoint. UAF stages use the snowplow-SA
	// endpoint; non-UAF stages go through the per-user
	// clientconfig (or the named EndpointRef) as before. The
	// orchestrator owns the recordStageTiming()-before-exit on the
	// two early-exit actions (C-2 stageContinue / R-2 stageReturn).
	//
	// #57 (C-b): the user-bearer-token append moved to AFTER endpoint
	// resolution (was before) so the self-loopback arm can compare the
	// RESOLVED endpoint host against the configured self-host.
	ep, epAction := r.resolveStageEndpoint(id, apiCall, uafActive)
	switch epAction {
	case stageContinue:
		recordStageTiming()
		return false
	case stageReturn:
		recordStageTiming()
		return true
	}

	// User-bearer-token append: only for non-UAF stages. When UAF is active
	// the SA endpoint carries the SA token (no user-bearer override needed);
	// appending the user token here would route the call through the user's
	// credentials instead of the SA's — breaking the entire UAF mechanism.
	// (UAF stages resolve to the SA endpoint at resolveStageEndpoint:385,
	// never an external/loopback ref, so the `!uafActive` guard is preserved
	// EXACTLY — UAF is untouched by the #57 self-loopback arm.)
	if !uafActive {
		if accessToken, _ := xcontext.AccessToken(r.ctx); accessToken != "" {
			// The append fires when EITHER:
			//   - the original gate: no named endpoint, or exportJWT:true
			//     (a named endpoint is assumed to carry its own auth); OR
			//   - #57 (C-b) self-loopback arm (ADDITIVE): the resolved
			//     endpoint host EXACTLY equals the configured self-host
			//     (URL_SELF) — i.e. the step is a /call loopback back at
			//     snowplow's OWN JWT-gated endpoint, which DOES need the
			//     bearer even though it has a named EndpointRef
			//     (snowplow-endpoint). Exact scheme+host+port equality
			//     (parsedHostEqualsSelf) — NEVER a substring/Contains match,
			//     so an EXTERNAL endpoint whose host merely CONTAINS
			//     "snowplow" (e.g. snowplow-foo.example.com) does NOT match
			//     and the bearer stays OFF it (the leak guard).
			if bearerAppendForStage(apiCall, ep) {
				// 0.30.164: stage-local Headers — never write the user
				// bearer back into the shared CR slice (the CR is marshaled
				// into the /call response body at restactions.go:149; an
				// in-place append leaked the JWT to the wire — see
				// /tmp/snowplow-runs/ship-307/before/.../call-namespaces.json).
				stageHeaders := make([]string, len(apiCall.Headers), len(apiCall.Headers)+3)
				copy(stageHeaders, apiCall.Headers)
				stageHeaders = append(stageHeaders,
					fmt.Sprintf("Authorization: Bearer %s", accessToken))

				// R (composition-resources loopback guard): propagate the
				// in-process recursion guards ACROSS the HTTP self-loopback
				// boundary. The depth-8 counter + #79 ancestor-set are
				// context.Value seams consumed only inside the IN-PROCESS
				// ResolveNestedCall (nested_call.go:98,154); they do NOT survive
				// an HTTP hop, so a composition-resources RA that self-dispatches
				// as a `/call?resource=restactions&name=<self>` loopback re-enters
				// restActionHandler.ServeHTTP as a FRESH request with NO depth,
				// NO ancestor set — the guards never fire → unbounded HTTP
				// self-recursion (the 3121-hop storm, RCA §0).
				//
				// Emit the two guards as advisory headers ONLY on the self-loopback
				// arm — the SAME parsedHostEqualsSelf(ep.ServerURL) predicate the
				// bearer-append already gated on (bearerAppendForStage), so an
				// EXTERNAL endpoint never receives them (leak/forge surface stays
				// off the wire for non-self hosts). The ingest side
				// (restactions.go / widgets.go) re-seeds ctx from these headers
				// ONLY when isTrustedSelfLoopback (C1) — they are advisory, never
				// trusted from an untrusted caller.
				//
				// Node identity is NOT re-derived here: r.ctx already carries the
				// CURRENT descent's ancestor set (the #83 Option-A handler seed +
				// any prior HTTP-hop re-seed), so AncestorsHeaderValue(r.ctx)
				// serializes exactly {ancestors ∪ this node}. The depth is
				// this hop's depth + 1 (the next hop is one deeper), mirroring the
				// in-process WithNestedCallDepth(ctx, depth+1) at nested_call.go:191.
				// Emitting only on the self-host arm keeps external calls
				// byte-identical to pre-R.
				if parsedHostEqualsSelf(ep.ServerURL) {
					stageHeaders = append(stageHeaders,
						fmt.Sprintf("%s: %d", cache.NestedDepthHeader,
							cache.NestedCallDepthFromContext(r.ctx)+1))
					if anc := cache.AncestorsHeaderValue(r.ctx); anc != "" {
						stageHeaders = append(stageHeaders,
							fmt.Sprintf("%s: %s", cache.ResolveAncestorsHeader, anc))
					}
				}

				local := *apiCall
				local.Headers = stageHeaders
				apiCall = &local
			}
		}
	}
	// Ship 0.30.121 R1 — the verbose wire-dump (httpcall's DumpResponse)
	// is the single largest transient-memory consumer (~1.94 GiB
	// alloc_space on the 50K bench: it stringifies every HTTP response
	// body, including the multi-MB compositions LIST). The blanket
	// `ep.Debug = opts.Verbose` set here is REMOVED — Debug is now
	// decided PER-CALL inside the g.Go worker (R1-a: never for a K8s
	// collection LIST) and additionally gated on RESOLVER_VERBOSE_WIRE_DUMP
	// (R1-b: an operator kill-switch, default off). See the worker below.
	r.log.Debug("resolved endpoint for api call",
		slog.String("name", id), slog.String("host", ep.ServerURL),
		slog.Bool("uaf", uafActive))

	// Build the per-stage call plan: request options + Ship D.5
	// cluster-list collapse + 0.30.92 lazy informer register (the
	// stageTiming.ClusterList* fields are written inside). An empty
	// tmp is C-3 (the "empty request options" Warn fired inside) —
	// the orchestrator owns the recordStageTiming()+continue.
	tmp := r.collapseOrFanoutPlan(id, apiCall, ep, &stageTiming)
	if len(tmp) == 0 {
		recordStageTiming()
		return false
	}

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
	// The success-branch log's `depth` field is computed by
	// depthForLog (support.go) — a mapDepth full-tree walk of dict.
	// Ship #6 gates that walk behind the Debug level; when it DOES
	// run (Debug on) it reads dict under dictMu, serialising against
	// concurrent jsonHandler writes. On the common (Info) path
	// neither the walk nor the lock runs.
	var dictMu sync.Mutex
	g, gctx := errgroup.WithContext(r.ctx)
	g.SetLimit(iterParallelism(r.ctx))

	// Ship 0.30.257 (#313) — per-item error slots (design §2.1 / §3.2).
	// One slot per iterator item, index-aligned with tmp. Each worker
	// writes ONLY its own itemErrs[i] (disjoint indices) and returns nil
	// on a per-item hard error, so the errgroup ctx is never cancelled by
	// a per-item failure → the remaining items proceed (Option C-A; the
	// SetLimit bound is unchanged). The slice backing array is allocated
	// HERE, before any g.Go, and never grown — disjoint-index writes to a
	// pre-sized slice are data-race-free without a lock (same property as
	// a pre-sized array; proven by TestResolve_IteratorErrorCollection_Race
	// under -race). The shared dict[errorKey] accumulation (W-A) is a
	// SEPARATE concern and stays under dictMu.
	itemErrs := make([]error, len(tmp))

	// Ship 0.30.192 — record iterator fan-out cost. tmp slice
	// length is the call count (1 after cluster-list collapse,
	// N per-NS after the iterator path); iterStart anchors the
	// wall-clock that g.Wait() closes below.
	stageTiming.IteratorCalls = len(tmp)
	iterStart := time.Now()

	// Ship 0.30.121 R1-b — the operator kill-switch. Compute once per
	// stage: verbose is permitted ONLY when the RESTAction asked for it
	// (opts.Verbose) AND the env flag explicitly enables the wire-dump.
	// Default off => wireVerbose is false => no call ever gets Debug.
	wireVerbose := r.opts.Verbose && verboseWireDumpEnabled()

	// The per-stage mutable bundle handed to each iterator worker. Built
	// here, discarded at the end of this stage iteration — NEVER a
	// resolveRun field (the concurrency flag / PM CONDITION 2). A pointer
	// to it is shared by all of THIS stage's workers (the same read-only
	// sharing the inline closure had; dictMu guards dict).
	sc := &stageCtx{
		id:          id,
		apiCall:     apiCall,
		dictMu:      &dictMu,
		g:           g,
		gctx:        gctx,
		itemErrs:    itemErrs,
		iterStart:   iterStart,
		wireVerbose: wireVerbose,
		ep:          ep,
		calls:       tmp,
	}

	for i := range tmp {
		call := tmp[i]

		// Edge type 3 dep recording — see 0.30.94 ship for full
		// rationale. cache.Deps() is sync.Map-backed; idempotent
		// LoadOrStore; safe from this (parent-goroutine) site — kept
		// INLINE here (NOT inside the worker) so its dep.recorded Debug
		// line preserves its pre-extraction emit order.
		//
		// Ship F1 (0.30.119): the dep edge attaches to whatever L1
		// key the request path threaded (the per-user resolved-output
		// key). The CONTENT-keyed api-stage entry records its OWN dep
		// edge inside the worker, keyed by the per-call content key
		// (gvr,ns,[name]) — so an informer event on a K8s call's GVR
		// dirty-marks the matching content entry and the refresher
		// re-dispatches that one call.
		if l1Key := cache.L1KeyFromContext(r.ctx); l1Key != "" && !cache.Disabled() {
			if ptr.Deref(call.Verb, http.MethodGet) == http.MethodGet {
				if gvr, ns, name, parseOK := cache.ParseAPIServerPathToDep(call.Path); parseOK {
					if name == "" {
						cache.Deps().RecordList(l1Key, gvr, ns)
					} else {
						cache.Deps().Record(l1Key, gvr, ns, name)
					}
					r.log.Debug("dep.recorded",
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

		// i is captured explicitly (per-iteration under Go 1.22+ loop
		// semantics) and passed as dispatchOneCall's arg → itemErrs[i] is
		// this item's disjoint slot (design Risk R1).
		i := i
		g.Go(func() error {
			return r.dispatchOneCall(sc, i)
		})
	}

	// Ship 0.30.257 (#313) — post-fan-out handling. With Option C-A
	// the three per-ITEM hard-error branches record into itemErrs[i]
	// and return nil, so g.Wait() NO LONGER returns non-nil for a
	// per-item dispatch failure (the stage runs all items + downstream
	// stages run — the central #313 behaviour change). g.Wait() can
	// still return non-nil for a NON-per-item worker error: a response
	// HANDLER failure (feedBytes / feedValue / inner returning a decode
	// error — NOT in #313's scope) or a genuine ctx-cancellation
	// surfacing through such a handler. For THOSE we preserve the
	// pre-0.30.257 short-circuit + truncated-dict return verbatim.
	if err := g.Wait(); err != nil {
		r.log.Debug("api stage short-circuited on hard error",
			slog.String("name", id), slog.Any("err", err))
		stageTiming.IteratorElapsedMs = time.Since(iterStart).Milliseconds()
		recordStageTiming()
		return true
	}
	stageTiming.IteratorElapsedMs = time.Since(iterStart).Milliseconds()

	// Ship 0.30.257 (#313) — per-item errors recorded by the worker
	// branches above do NOT cancel the stage; they were collected into
	// itemErrs (disjoint indices) and the matching dict[errorKey]
	// accumulating-slice entries (W-A) already carry them on the wire.
	// errors.Join folds them into a single errors.Is-inspectable
	// aggregate (preserving the %w wraps the worker branches set) for a
	// Debug diagnostic line ONLY — api.Resolve still returns a map, never
	// an error (trace §2.2). The wire result is "all successful items +
	// the accumulated per-item errors under errorKey". The refresher /
	// request-path Cache-A guards (keyed on stageErrSink.Count(), bumped
	// above) decide whether the partial-with-errors result is PERSISTED.
	if joined := errors.Join(itemErrs...); joined != nil {
		r.log.Debug("api stage had per-item errors (continuing)",
			slog.String("name", id), slog.Any("err", joined))
	}

	// Ship 0.30.235 — the pre-0.30.235 post-g.Wait()
	// applyUserAccessFilter call was DELETED; UAF now runs per-worker
	// inside jsonHandlerCore on the raw envelope. See
	// refilter_layering_test.go for the permanent regression gate.

	// Ship F1 (0.30.119): the api-stage L1 is now CONTENT-keyed —
	// the per-K8s-call Put happens inside the g.Go worker (each call
	// stores its own raw envelope under its (gvr,ns,[name]) content
	// key). There is NO per-stage Put here — the Ship E per-stage
	// entry is gone; an iterator stage produces N content entries,
	// one per call, assembled into dict[id] by the N jsonHandler
	// merges exactly as before.

	// Ship 0.30.192 — emit per-stage timing on normal completion.
	// Records ElapsedMs (stage-total wall-clock) + IteratorElapsedMs
	// (g.Wait()-bounded fan-out cost) for the cohort timing log line.
	recordStageTiming()
	return false
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

	// Build the per-call resolve-invariant state (P2 mapper / P3 dict / P4
	// gate+sinks). The per-stage loop below reads these via r.*; the
	// per-stage mutable primitives stay loop-local (see the resolveRun
	// concurrency note).
	r := newResolveRun(ctx, opts, log, user)
	// Each api stage runs through runStage in topological order. runStage
	// returns stop=true on a truncating exit (R-1 caller-cancel, R-2 endpoint
	// err, R-3 g.Wait hard error) — the orchestrator then returns the
	// (truncated) dict; stop=false advances to the next stage. runStage owns
	// the recordStageTiming()-before-every-exit invariant internally.
	for _, id := range names {
		if r.runStage(id, apiMap) {
			return r.dict
		}
	}

	// Ship 2a (0.30.209) — the per-serve removeManagedFields(dict) walk is
	// GONE. managedFields is now stripped ONCE at the four item-
	// materialisation sites (parseListEnvelope, validateClusterListShape,
	// gateGetEnvelope, gateListEnvelope→parseListEnvelope), where the item
	// map is still private. With the Ship 2a SHALLOW envelope, this walk
	// delete(v, "managedFields") wrote the SHARED entry.Items maps in place,
	// racing concurrent serves reads (the -race in
	// TestResolve_ConcurrentRequestsDoNotCrossPollinate). Stripping at
	// load means dict only carries the fresh per-serve outer envelope +
	// jq-constructed objects (never managedFields), so the walk is both
	// unsafe (shared write) and unnecessary. Dropping it also removes a
	// per-serve O(nodes) full-tree traversal (perf win).
	//delete(dict, "slice")

	return r.dict
}

// discoverGroupResourcesFn is the package-private indirection over
// cache.DiscoverGroupResources so the Fix A1 per-group-dedup falsifier
// (lazy_register_storm_test.go) can count invocations per distinct group
// without standing up a real discovery client. Production path is
// unchanged. Mirrors cache.discoveryClientBuilder (discovery_lookup.go:151).
var discoverGroupResourcesFn = cache.DiscoverGroupResources

// serviceAccountEndpointFn is the package-private indirection over
// dynamic.ServiceAccountEndpoint so the L13(b) fail-closed falsifier
// (refilter_concurrency_test.go) can drive the UAF SA-endpoint ACQUIRE-FAILURE
// branch of resolveStageEndpoint deterministically — without depending on the
// ambient /var/run/secrets SA files being absent (a fragile, non-hermetic
// trigger). Production path is UNCHANGED: the default is the real
// dynamic.ServiceAccountEndpoint and production never reassigns it. Mirrors
// discoverGroupResourcesFn (above) and cache.discoveryClientBuilder
// (discovery_lookup.go:151) — the established resolveOnceFn/nestedCallResolver
// var-seam idiom.
var serviceAccountEndpointFn = dynamic.ServiceAccountEndpoint

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
func lazyRegisterInnerCallPaths(ctx context.Context, log *slog.Logger, opts []httpcall.RequestOptions) {
	rw := cache.Global()
	if rw == nil {
		return
	}
	seen := map[schema.GroupVersionResource]struct{}{}
	// 2026-06-22 Fix A1 (discovery storm) — per-group dedup. A
	// composition-detail stage emits ONE RequestOptions entry per
	// iterator dispatch (28 composition kinds for composition.krateo.io),
	// every one carrying the SAME static group. Without this gate the
	// AddNavigationDiscoveredGroup+DiscoverGroupResources block below
	// fires once PER entry → 28× the same synchronous discovery hop on
	// the hot resolve path (each hop = ServerGroups + a
	// ServerResourcesForGroupVersion loop over the group's full version
	// union, all returning already-known state). seenGroups collapses
	// that to once per DISTINCT group per resolve. It is a SIBLING of the
	// GVR `seen` map (which only guards EnsureResourceType below) — NOT a
	// bare sync.Once: a 2nd DISTINCT group still registers + discovers.
	seenGroups := map[string]struct{}{}
	for i := range opts {
		path := opts[i].Path

		// 0.30.102 Tag B Part 2 — composition-group walker feed.
		// Composition apiserver paths are JQ-templated
		// (`/apis/<group>/${.v}/...`), so ParseAPIServerPathToGVR
		// (which rejects any `${`) cannot derive their GVR here. The
		// GROUP segment is static, though — extract it and:
		//
		//  1. Record it in the navigation-discovered set (so the
		//     watcher's removable-discriminator at watcher.go:749 +
		//     :1064 routes composition GVRs to the standalone-informer
		//     branch — the only branch RemoveResourceType can ever
		//     tear down).
		//
		//  2. Ship 0.5 / 0.30.223 (v6): invoke cache.DiscoverGroup-
		//     Resources synchronously. One-shot apiserver discovery
		//     enumerates every CRD-backed resource in `grp`, calls
		//     EnsureResourceType for each (spawning composition
		//     informers), and fires the FD1 dirty-mark chain via
		//     Deps().OnResourceTypeAvailable for genuinely-new GVRs.
		//     Replaces the deleted CRD-informer event-driven backplane
		//     (Ship 0 walker-spawn + the pre-v6 CRD-watch file).
		//     Soft-fails: a
		//     discovery error is logged and the walker continues
		//     (subsequent walks retry); the dispatch through the
		//     unregistered composition GVR will hit apiserver via
		//     fall-through.
		//
		// Gated by PrewarmEnabled() — #57 implicit-on-cache, so a
		// cache-OFF process is byte-identical (the nav-discovered set
		// stays empty and the discovery hop never runs). Non-templated
		// paths also flow through here harmlessly — their group is added
		// too (it IS navigation-reached).
		if cache.PrewarmEnabled() {
			if grp, grpOK := cache.ExtractAPIServerGroupFromTemplatedPath(path); grpOK {
				// Fix A1 — gate on first-sight of this DISTINCT group in
				// the current opts slice. A 2nd distinct group still
				// passes this gate (registers + discovers); a repeat of an
				// already-seen group is skipped, killing the storm.
				if _, dupGrp := seenGroups[grp]; !dupGrp {
					seenGroups[grp] = struct{}{}
					cache.AddNavigationDiscoveredGroup(grp)
					// The walker's *rest.Config is wired through
					// cache.WithInternalRESTConfig during Phase 1 (SA-
					// credentialed walk). When absent (e.g. a non-Phase-1
					// /call), the discovery hop is skipped — the only
					// caller pattern that requires it is the SA-credentialed
					// walker, which DOES set it.
					if rcAny, ok := cache.InternalRESTConfigFromContext(ctx); ok {
						if cfg, ok := rcAny.(*rest.Config); ok && cfg != nil {
							if _, err := discoverGroupResourcesFn(ctx, cfg, grp); err != nil {
								// Soft-fail: discovery is an optimization; the
								// dispatch falls through to the apiserver and a
								// subsequent walk retries. No operator action —
								// cache-effectiveness is tracked by the
								// apiserver_fallthrough metrics. DEBUG, not WARN.
								log.Debug("cache.discovery.group_resources_fetch_failed",
									slog.String("subsystem", "cache"),
									slog.String("group", grp),
									slog.Any("err", err),
								)
							}
						}
					}
				}
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

// removeManagedFields was the per-serve recursive managedFields stripper
// called at the end of Resolve. Ship 2a (0.30.209) removed it: with the
// shallow envelope it wrote the shared entry.Items in place (a data
// race), and managedFields is now stripped once at the item-
// materialisation sites (stripManagedFields in apistage.go /
// cluster_list.go). The function is intentionally deleted rather than
// parked — feedback_no_park_broken_behind_flag.
