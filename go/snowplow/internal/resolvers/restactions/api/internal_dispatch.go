// internal_dispatch.go — 0.30.104 Phase-1 TLS-CA fix +
// 0.30.250 Task #268 paged-LIST fix.
//
// 0.30.250 Task #268 — paged LIST in the internal-REST-config dispatcher.
//
//   Symptom (TRACED 2026-06-09 — docs/task-267-s6-admin-2-widget-silent-skip-trace-2026-06-09.md):
//   under bench S6 admin Dashboard the 2 compositions-panel widgets
//   (`dashboard-compositions-panel-row-piechart` and `-table`) stayed in
//   `isLoading` indefinitely. Pod logs: `nri.List(ctx, metav1.ListOptions{})`
//   at the load-bearing site below issued an UNPAGINATED cluster-wide LIST
//   of 50K composition CRs, which took 2.9-162 s per /call. Browser nav
//   cadence (~5 s) cancelled the in-flight body read; client-go returned
//   `context canceled`; the dispatcher returned HTTP 500; React Query kept
//   the widget skeleton visible while retrying.
//
//   The fix wraps the LIST call in a continue-token paged walk mirroring
//   the prior art at internal/cache/streaming_list.go (the streaming
//   ListWatch used by the informer factory since R4/H2a). Per-page
//   limit = listPageLimit (500) — matches every other LIST in snowplow
//   and client-go's defaultPageSize. The walk accumulates items into a
//   single *unstructured.UnstructuredList; the served bytes go through
//   the SAME `list.UnstructuredContent()` marshal as the pre-0.30.250
//   path so JQ downstream is byte-equivalent
//   (feedback_cache_must_not_constrain_jq.md).
//
//   Behaviour-neutral for the no-internal-config branch (still served by
//   plumbing's httpcall.Do unchanged) AND for GET-by-name (the `if name
//   != ""` branch is untouched — pager.go's "1 page short-circuit"
//   semantics are already covered by a non-paged Get).
//
//   A WARN-level slog event `internal_dispatch.paged_list.completed` is
//   emitted once per LIST that ran the paged loop, carrying gvr / pages /
//   items / page_limit / total_ms — the falsifier event required by
//   feedback_falsifier_first_before_ship.md.
//
// 0.30.104 Phase-1 TLS-CA fix (unchanged):
//
// THE BUG (0.30.103 flag-ON re-bench, reproduced on every boot):
//
//   Phase 1's SA-credentialed startup walk resolves the `routesloaders`
//   navigation root under the snowplow service-account identity. The
//   walk's FIRST step after resolving the root is an inner api[] stage
//   that LISTs `/api/v1/namespaces` — the resolver dispatches it through
//   plumbing's httpcall.Do, which builds the HTTP client from the
//   plumbing Endpoint shape via HTTPClientForEndpoint -> tlsConfigFor.
//
//   plumbing's tlsConfigFor (http/request/transport.go) installs a custom
//   CA pool into RootCAs ONLY inside the `HasCertAuth()` branch. The
//   snowplow SA endpoint is TOKEN-auth (bearer JWT, no client cert), so
//   `HasCertAuth()` is false and tlsConfigFor returns at the
//   `!ep.HasCertAuth()` early-exit — the SA endpoint's
//   CertificateAuthorityData (the raw-PEM cluster CA) is NEVER applied.
//   The client then verifies the apiserver cert against the system root
//   store, which does not contain the cluster's self-signed CA:
//
//     ERROR api call response failure name=namespaces
//       path=/api/v1/namespaces
//       error="tls: failed to verify certificate: x509: certificate
//              signed by unknown authority"
//
//   The namespace LIST fails -> the walk never discovers the composition
//   GVR -> Phase 1 registers only the 8 infrastructure informers, not the
//   composition informer -> Phase 1 pre-warms nothing at scale.
//
//   plumbing is upstream (project_no_upstream_authority.md) — it cannot
//   be patched. plumbing's tlsConfigFor structurally has only two TLS
//   outcomes: Insecure (skip-verify) or HasCertAuth (build CA pool +
//   client cert). A token-auth endpoint carrying a custom CA cannot be
//   served by it at all.
//
// THE FIX (snowplow-side, behavior-neutral):
//
//   Phase 1 already attaches its SA *rest.Config — the value
//   rest.InClusterConfig() returns, which carries the cluster CA and the
//   SA bearer token with the correct in-cluster TLS semantics — to the
//   context via cache.WithInternalRESTConfig (used since 0.30.103 by the
//   objects.Get / resourcesrefs.Resolve fetch sites). 0.30.104 extends
//   that same context-carried *rest.Config to the api-stage K8s GET/LIST
//   dispatch: dispatchViaInternalRESTConfig is a sibling of
//   dispatchViaInformer wired into resolve.go's inner-call worker BEFORE
//   httpcall.Do. When the context carries an internal *rest.Config and
//   the call is a GET against an apiserver-shaped path, the dispatch goes
//   through a client-go dynamic client built from that *rest.Config —
//   client-go's transport installs the CA correctly. Every other call
//   shape (no internal config, non-GET verb, external/subresource path)
//   falls through to the unchanged httpcall.Do path.
//
//   BEHAVIOR-NEUTRAL: ordinary per-user requests NEVER set
//   cache.WithInternalRESTConfig, so dispatchViaInternalRESTConfig
//   immediately returns served=false for them and the path is byte-
//   identical to pre-0.30.104. The mechanism is keyed only on context
//   state — uniform across GVRs, no per-resource carve-out
//   (feedback_no_special_cases.md).
//
// CRITICAL — this path is validated ON-CLUSTER. Two prior Phase-1-SA
// fixes (0.30.102 base64, 0.30.103) passed unit tests and failed on the
// real cluster; a unit test cannot exercise the real cluster CA or a real
// apiserver TLS handshake. The falsifier in internal_dispatch_tls_test.go
// is necessary but not sufficient.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/plumbing/ptr"
	"github.com/krateo-platformops/snowplow/internal/cache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// internalDispatchListPageLimit is the per-page limit for the paged
// LIST in dispatchViaInternalRESTConfig. 500 matches client-go's
// defaultPageSize AND snowplow's global informer-LIST limit
// (internal/cache/watcher.go:24-28, `listPageLimit`). Centralised so
// the falsifier test pins the value and a future bench-driven retune
// is a single-site change.
//
// Task #268 / 0.30.250 — picked from the architect's recommendation
// (docs/task-267-s6-admin-2-widget-silent-skip-trace-2026-06-09.md
// §10): at 50K compositions / ~500 items per page → ~100 round-trips
// of ~50-100 ms each → total wall-clock ~8 s (bounded, no `context
// canceled` body-read failure mode). The bench's nav-cadence is ~5 s
// so the LIST still spills into the next nav; the downstream cache /
// prewarm work is what brings the warm /call below the nav budget.
// Bounding the LIST is the necessary first step (it is no longer a
// 162 s body-read that the browser cancels).
const internalDispatchListPageLimit int64 = 500

// internalDispatchServeSyncWait bounds the per-GVR sync-wait the #121 1a
// informer-serve branch performs before falling through to the live paged
// LIST. SMALL by design (C1 "never worse"): the measured boot race has the
// composition informer synced ~24s BEFORE the walk's LIST dispatch, so in the
// common case WaitForGVRSync returns TRUE immediately (already synced) and
// this bound is never consumed. It exists only for the narrow window where a
// GVR lazy-registered mid-walk hasn't quite finished its initial LIST — a few
// seconds of wait then trades a 27.5s live LIST for an informer read. If the
// wait expires the branch falls through to the live LIST exactly as today
// (the ~136s headroom 1a frees dwarfs this bound, so a never-syncing GVR
// cannot re-create the seed deadline-cut). A const, not a CRD field: it is an
// internal timeout with no per-resource semantics (feedback_no_special_cases
// is about path/resource carve-outs, not tunables); centralised so the C1
// falsifier pins it.
const internalDispatchServeSyncWait = 5 * time.Second

// internalClientCache memoises the client-go dynamic client built from a
// given internal *rest.Config. Phase 1's walk fans out many inner api[]
// calls all carrying the SAME SA *rest.Config pointer; rebuilding the
// dynamic client per call would re-create the TLS transport each time.
// Keyed on the *rest.Config pointer identity — Phase 1 builds exactly one
// SA config and attaches it verbatim, so the pointer is a stable cache
// key for the process lifetime.
//
// The dynamic client is used directly with the parsed GVR — NO RESTMapper
// / discovery round-trip. The resolver's inner-call path has already
// produced a fully-qualified apiserver REST path; ParseAPIServerPathToDep
// extracts the exact (group, version, resource), so k8sdynamic's
// Resource(gvr) is sufficient and avoids the apiserver discovery I/O that
// a typed/mapped client would incur.
//
// Concurrency: resolve.go's inner-call worker is a bounded errgroup, so
// dispatchViaInternalRESTConfig is called from parallel goroutines. The
// map is guarded by internalClientMu. The cached dynamic.Interface itself
// is safe for concurrent use.
var (
	internalClientMu    sync.Mutex
	internalClientCache = map[*rest.Config]k8sdynamic.Interface{}
)

// internalClientFor returns a memoised client-go dynamic client for rc,
// building one on first use. rc carries the cluster CA + SA bearer token
// verbatim (it is the rest.InClusterConfig() value) so client-go's
// NewForConfig produces a transport that trusts the cluster CA. This is
// the load-bearing difference from plumbing's httpcall.Do path, whose
// tlsConfigFor drops the CA for token-auth endpoints.
func internalClientFor(rc *rest.Config) (k8sdynamic.Interface, error) {
	internalClientMu.Lock()
	defer internalClientMu.Unlock()
	if cli, ok := internalClientCache[rc]; ok {
		return cli, nil
	}
	cli, err := k8sdynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	internalClientCache[rc] = cli
	return cli, nil
}

// resetInternalClientCacheForTest clears the memoised client map.
// TEST-ONLY — the production cache is set-once-per-config and never
// cleared. Exported within-package for the falsifier test.
func resetInternalClientCacheForTest() {
	internalClientMu.Lock()
	defer internalClientMu.Unlock()
	internalClientCache = map[*rest.Config]k8sdynamic.Interface{}
}

// dispatchViaInternalRESTConfig attempts to serve `call` through a
// client-go dynamic client built from the context-carried internal
// *rest.Config (cache.WithInternalRESTConfig). It is the api-stage
// sibling of dispatchViaInformer and is wired into resolve.go's inner-
// call worker BEFORE httpcall.Do.
//
// Returns (rawBytes, true, nil) when the call was served — the caller
// feeds rawBytes to call.ResponseHandler exactly as it does for the
// informer-pivot branch. Returns (nil, false, nil) for every gate that
// must take the unchanged httpcall.Do path:
//
//   - no internal *rest.Config on the context (every ordinary per-user
//     request — the behavior-neutral invariant);
//   - the context value is the wrong type / a nil pointer;
//   - non-GET verb (POST/PUT/PATCH/DELETE — client-go dynamic Get/List
//     here is read-only; writes are not a Phase-1 shape and stay on the
//     unchanged path);
//   - a non-apiserver path (external URL, JQ-leaked `${...}`, malformed
//     shape) — the internal dispatcher only owns apiserver GVR paths;
//   - a subresource path (.../status, .../scale, ...) — no dynamic-Get
//     shape, same gate as the informer pivot.
//
// Returns (nil, false, err) ONLY when the apiserver call itself errored
// after the dispatcher committed to serving (client build failed, or the
// Get/List returned a non-recoverable error). resolve.go treats a non-nil
// err here exactly as it treats an httpcall.Do StatusFailure — it does
// NOT silently fall through to httpcall.Do, because that would just
// re-hit the same broken plumbing TLS path. Surfacing the error keeps
// the failure diagnosable (it is the real apiserver error, e.g. a 403 or
// a genuine connectivity fault) rather than masking it behind a second
// x509 error.
//
// On the served path the LIST output is wrapped in the apiserver LIST
// envelope (apiVersion/kind/items) and a GET-by-name returns the bare
// object — byte-equivalent to what httpcall.Do would have delivered, so
// the downstream JQ pipeline is invariant (feedback_cache_must_not_constrain_jq.md).
func dispatchViaInternalRESTConfig(ctx context.Context, call httpcall.RequestOptions) ([]byte, bool, error) {
	// Gate 1: internal *rest.Config present? Absent => ordinary per-user
	// request => behavior-neutral fall-through to httpcall.Do.
	v, ok := cache.InternalRESTConfigFromContext(ctx)
	if !ok {
		return nil, false, nil
	}
	rc, rcOK := v.(*rest.Config)
	if !rcOK || rc == nil {
		// An internal driver attached the wrong shape (not a *rest.Config,
		// or a nil pointer). Fall through to httpcall.Do — but WARN: a
		// mis-wired internal driver's apiserver dispatches will silently
		// take the plumbing path, which drops the SA CA for a token-auth
		// endpoint and fails with the x509 error. Loud so a future caller
		// passing the wrong shape is diagnosable (mirrors resolveOne's
		// WithInternalEndpoint wrong-type warning).
		xcontext.Logger(ctx).Warn("dispatchViaInternalRESTConfig: internal REST config present but not a usable *rest.Config; falling through to httpcall.Do",
			slog.String("subsystem", "cache"),
			slog.String("got_type", fmt.Sprintf("%T", v)),
			slog.String("hint", "an internal driver (e.g. Phase 1) must pass *rest.Config to cache.WithInternalRESTConfig"),
		)
		return nil, false, nil
	}

	// Gate 2: verb. client-go dynamic Get/List is read-only.
	if verb := ptr.Deref(call.Verb, http.MethodGet); verb != http.MethodGet {
		return nil, false, nil
	}

	// Gate 3: subresource path. Same exclusion as the informer pivot —
	// status/scale/log/exec/binding/proxy have no dynamic-Get shape.
	if hasSubresourceSuffix(call.Path) {
		return nil, false, nil
	}

	// Gate 4: parse path -> GVR + namespace + name. Non-apiserver paths
	// (external URLs, unresolved `${...}`) return ok=false; the internal
	// dispatcher only owns apiserver GVR paths.
	gvr, namespace, name, parseOK := cache.ParseAPIServerPathToDep(call.Path)
	if !parseOK {
		return nil, false, nil
	}

	cli, err := internalClientFor(rc)
	if err != nil {
		return nil, false, err
	}

	// Select the resource interface. namespace=="" => cluster-scoped
	// (the /api/v1/namespaces LIST itself, and any cluster-scoped GVR);
	// a non-empty namespace => the namespaced resource interface.
	ri := cli.Resource(gvr)
	var nri k8sdynamic.ResourceInterface = ri
	if namespace != "" {
		nri = ri.Namespace(namespace)
	}

	// Served path. Two shapes — GET-by-name and LIST (name=="").
	if name != "" {
		obj, getErr := nri.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return nil, false, getErr
		}
		raw, mErr := json.Marshal(obj.Object)
		if mErr != nil {
			return nil, false, mErr
		}
		return raw, true, nil
	}

	// Task #118 — unintended-collapse WARN (diagnostic only, NO behavior
	// change). The by-name GET branch above is taken when the resolved path
	// carries a non-empty terminal name segment; falling through to here
	// means name=="" and we are about to serve a cluster/namespace-wide LIST.
	//
	// That is CORRECT for an intentional LIST step (an RA whose api-step path
	// authored NO name segment, e.g. `.../compositiondefinitions`). But it is
	// ALSO where a by-name step SILENTLY collapses: when an RA authors a name
	// (and/or namespace) path segment via a jq concat template
	// (`... + "/compositiondefinitions/" + .name`) and the fed value is
	// null/absent, gojq's `string + null == string` folds the segment away,
	// leaving a TRAILING SLASH (`.../compositiondefinitions/`) or an EMPTY
	// INTERIOR SEGMENT (`.../namespaces//...`). ParseAPIServerPathToDep strips
	// the trailing slash and parses name=="" — so the by-name GET the author
	// intended becomes an unbounded cluster LIST. Downstream jq that expects a
	// single object then chokes (the blueprint-formdef "split cannot be
	// applied to: null" class traced in task #117). EMPIRICALLY VERIFIED
	// (jqutil.Eval probe): the concat idiom renders exactly these shapes,
	// while an authored nameless LIST and a resolved by-name GET do NOT — so
	// pathHasNullPathSegment discriminates the unintended collapse from the
	// legitimate LIST. (The interpolation idiom `\(.name)` renders the literal
	// "null" and takes the GET branch above instead, so it never lands here.)
	//
	// This is a WARN only. The dispatch still LISTs exactly as before — the
	// served envelope is byte-identical to pre-#118. The single log line lets
	// an operator see that a caller supplied a null where a name was expected,
	// instead of the failure only surfacing as a confusing downstream jq error.
	if pathHasNullPathSegment(call.Path) {
		xcontext.Logger(ctx).Warn("internal_dispatch.list.unintended_collapse: api-step path resolved to a cluster LIST because a name/namespace path segment was null — check the caller's extras",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("namespace", namespace),
			slog.String("resolved_path", call.Path),
			slog.String("note", "Task #118 — a jq-templated name/namespace segment folded to empty (string+null); the intended by-name GET became a cluster-wide LIST"),
		)
	}

	// Task #268 / 0.30.250 — paged LIST walk.
	//
	// The pre-0.30.250 code issued ONE call: `nri.List(ctx, ListOptions{})`
	// — unpaginated. At 50K composition CRs that single call took 2.9-162 s,
	// the browser cancelled the in-flight body, client-go returned
	// `context canceled`, the resolver surfaced HTTP 500, React Query
	// kept the widget skeleton visible (TRACED — task-267 trace doc).
	//
	// The paged walk uses the same continue-token contract as client-go's
	// pager.ListPager (k8s.io/client-go/tools/pager/pager.go:80-167) and
	// snowplow's existing streaming_list.go (`for { ...; if cont == ""
	// break; opts.Continue = cont }`). Each iteration:
	//
	//   1. issues nri.List with opts.Limit = internalDispatchListPageLimit
	//      and opts.Continue = previous page's continue token;
	//   2. accumulates `page.Items` into the result list's Items slice;
	//   3. CAPTURES the FIRST page's envelope identity (apiVersion / kind /
	//      metadata.resourceVersion) onto the result list's Object — these
	//      are RV-pinned by the apiserver at page 1 of a paged walk and
	//      every subsequent page reports the same snapshot RV (the
	//      apiserver contract — streaming_list.go BLOCKER 1);
	//   4. clears opts.ResourceVersion on subsequent pages so the
	//      apiserver's "specifying resource version is not allowed when
	//      using continue" error cannot fire (pager.ListPager line 160-164).
	//
	// The result list's Object map is built from page 1's Object plus the
	// accumulated `Items` slice. `list.UnstructuredContent()` then folds
	// the Items back into an `items` key — IDENTICAL marshal shape to the
	// pre-0.30.250 single-LIST path. The downstream JQ pipeline sees the
	// same envelope; the LIST-envelope guard from the 0.30.104 falsifier
	// (TestInternalRESTConfigDispatch_TrustsClusterCA) still passes.
	//
	// nri.List error semantics: client-go returns ALL apiserver errors
	// (including 410-Gone "Expired") wrapped as `*apierrors.StatusError`.
	// We surface them verbatim — the dispatcher's existing error path
	// (resolve.go:784-833) records them with `dispatch=internal-rest-config-error`
	// and the resolver surfaces an HTTP 500 with the original error. No
	// `pager.FullListIfExpired`-style fallback — at 50K the unpaginated
	// fallback is exactly the failure mode the fix exists to remove. A
	// 410 here is rare (the apiserver compacted between pages) and the
	// caller (or the prewarm refresher) will retry with a fresh RV.
	// #121 1a — INFORMER-SERVE BRANCH. Before paying the live paged LIST,
	// try to serve this cluster/namespace-wide LIST from the (already-synced)
	// informer indexer. The root cause of the boot-walk deadline-cut is that
	// this dispatcher ALWAYS ran the live LIST — even for a GVR whose informer
	// was fully synced — so the 50K-composition boot re-walk paid a 27.5s /
	// 60K-item live LIST ~24s AFTER the benchapps informer had synced (pure
	// waste that ate the boot-scope budget and deadline-cut the seed;
	// docs/boot-walk-deadline-rootcause-2026-07-09.md).
	//
	// SCOPE: gated on a serve-watcher being attached to ctx. ONLY the Phase 1
	// prewarm walk/seed context attaches it (withPhase1SAContext); ordinary
	// per-user /call requests do NOT, so their dispatch is byte-identical to
	// pre-1a — rw==false here → the whole branch is skipped and the live LIST
	// runs exactly as today. NEVER WORSE (C1): a bounded WaitForGVRSync then
	// an IsServable re-check; on any miss (no watcher, unregistered, unsynced
	// past the bound, or not-servable) we fall through to the live paged LIST
	// below unchanged.
	//
	// RBAC: the walk runs under the SA identity and the live LIST it replaces
	// is the SAME cluster/namespace-wide SA read — ListServableEnvelopeJSON
	// returns the full informer set (no per-user narrowing), byte-parity with
	// the SA LIST. The downstream userAccessFilter refilter (if any) runs
	// identically on either envelope. No per-user path reaches here (scope
	// gate above), so there is no cross-user serve.
	if rw, haveRW := cache.ServeWatcherFromContext(ctx); haveRW {
		// Bounded per-GVR sync-wait (returns immediately when already synced —
		// the common boot case), then the SAME four-conjunct IsServable gate
		// ListObjectsServable enforces. Both must hold to serve from cache.
		synced := rw.WaitForGVRSync(ctx, gvr, internalDispatchServeSyncWait)
		if synced && rw.IsServable(gvr) {
			if served, ok := rw.ListServableEnvelopeJSON(gvr, namespace); ok {
				xcontext.Logger(ctx).Info("internal_dispatch.list.informer_served",
					slog.String("subsystem", "cache"),
					slog.String("gvr", gvr.String()),
					slog.String("namespace", namespace),
					slog.Int("bytes", len(served)),
					slog.String("note", "Task #121 1a — served the prewarm-walk LIST from the synced informer instead of a live paged apiserver LIST"),
				)
				return served, true, nil
			}
		}
		// Serve branch NOT taken — capture the four servability conjuncts under
		// one read lock so the boot log discriminates ALL miss reasons (sync
		// timeout, unregistered, watch-broken, or typeConfirmed=false) before
		// we fall through to the live paged LIST. Diagnostic only (Task #130 F1);
		// the dispatch flow below is byte-identical whether or not this fires.
		snap := rw.ServabilitySnapshotFor(gvr)
		xcontext.Logger(ctx).Info("internal_dispatch.list.serve_miss",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.Bool("registered", snap.Registered),
			slog.Bool("hasSynced", snap.HasSynced),
			slog.Bool("watchHealthy", snap.WatchHealthy),
			slog.Bool("typeConfirmed", snap.TypeConfirmed),
		)
		// Fall through to the live paged LIST below (never worse).
	}

	listStart := time.Now()
	resultList := &unstructured.UnstructuredList{}
	opts := metav1.ListOptions{Limit: internalDispatchListPageLimit}
	pages := 0
	totalItems := 0
	for {
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		default:
		}
		page, pageErr := nri.List(ctx, opts)
		if pageErr != nil {
			return nil, false, pageErr
		}
		pages++

		// First page — capture envelope identity (apiVersion / kind /
		// metadata.resourceVersion) onto the result list. Subsequent
		// pages report the SAME envelope (apiserver paged-LIST contract),
		// so we only need page 1. Guard with `== nil` / `== ""` so a
		// future page that drifts (proxy edge) cannot last-write-wins
		// overwrite the page-1 snapshot RV.
		if resultList.Object == nil {
			// Shallow-copy page 1's Object map onto the result list. We
			// rebuild `items` below from accumulated Items via
			// UnstructuredContent(), so dropping page 1's items key (if
			// any) here is fine — but copy every OTHER envelope scalar
			// (apiVersion, kind, metadata sub-map, etc.) verbatim.
			//
			// R3 — OWNERSHIP NOTE (architect 0.30.250 review): page.Object
			// is freshly allocated by client-go's UnstructuredList.Unmarshal
			// on every nri.List call (it is NOT shared with any cache or
			// reused buffer — client-go builds a new map for each LIST
			// response). After the shallow-copy below the *map[string]any
			// values nested under `metadata`/`metadata.continue` are still
			// the page-1 map's children; the subsequent SetContinue("") /
			// SetRemainingItemCount(nil) calls mutate THAT map. This is
			// safe because we own page.Object's tree — page-1 goes out of
			// scope at the next loop iteration and only this resultList
			// retains a reference to it.
			resultList.Object = make(map[string]interface{}, len(page.Object))
			for k, v := range page.Object {
				if k == "items" {
					continue
				}
				resultList.Object[k] = v
			}
		}

		// Append this page's items into the accumulated list. page.Items
		// is the typed []Unstructured slice; appending is O(items-in-page)
		// — at 500-per-page / 50K total, ~100 iterations of 500-element
		// appends.
		resultList.Items = append(resultList.Items, page.Items...)
		totalItems += len(page.Items)

		// Drive the loop on the page's `continue` token. Empty token =>
		// last page; break out of the loop.
		cont := page.GetContinue()
		if cont == "" {
			break
		}
		opts.Continue = cont
		// Clear ResourceVersion on subsequent pages — same rationale as
		// pager.ListPager (line 160-164): a continue token fully
		// determines the page; carrying an RV alongside it errors with
		// "specifying resource version is not allowed when using continue".
		opts.ResourceVersion = ""
		opts.ResourceVersionMatch = ""
	}

	// CRITICAL — clear page 1's `metadata.continue` and
	// `metadata.remainingItemCount` on the accumulated list. Page 1's
	// envelope (copied verbatim into resultList.Object above) carries a
	// non-empty `continue` token (since the paged walk continues past
	// page 1); leaking that token into the served LIST would tell
	// downstream JQ filters and the JS pagination state that the list
	// is PARTIAL — but it is not, we've accumulated every page. The
	// unpaginated pre-0.30.250 path returned `continue=""` and no
	// `remainingItemCount` here; preserve byte-equivalence by clearing
	// both on the materialised list.
	resultList.SetContinue("")
	resultList.SetRemainingItemCount(nil)

	// Falsifier event — proves the paged path executed (per
	// feedback_falsifier_first_before_ship). One event per served LIST,
	// not per page (to keep the log volume bounded at scale).
	//
	// LEVEL — DEBUG (#170). This is a success/completion event, not a
	// fault. It fires once per served paged LIST, i.e. at the /call rate,
	// which on a healthy portal at scale is ~253K/week — a firehose that
	// drowns real warnings now that LOG_LEVEL=warn is the steady-state prod
	// floor (#163). The old R2 note kept it at WARN to survive a floor
	// raise, but LOG_LEVEL is now an explicit knob: this event stays fully
	// visible (with its pages/items/total_ms fields) at LOG_LEVEL=debug when
	// you actually need it, and the per-cell counters/metrics are unaffected.
	xcontext.Logger(ctx).Debug("internal_dispatch.paged_list.completed",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
		slog.String("namespace", namespace),
		slog.Int("pages", pages),
		slog.Int("items", totalItems),
		slog.Int64("page_limit", internalDispatchListPageLimit),
		slog.Int64("total_ms", time.Since(listStart).Milliseconds()),
		slog.String("note", "Task #268 / 0.30.250 — paged LIST walk replaces unpaginated nri.List"),
	)

	// CRITICAL — marshal resultList.UnstructuredContent(), NOT
	// resultList.Object. UnstructuredContent() shallow-copies the
	// envelope scalars (the page-1 Object map without its `items` key)
	// AND folds the accumulated typed Items slice back into an `items`
	// array — the exact apiserver LIST shape (apiVersion / kind /
	// metadata / items) the JQ pipeline expects, byte-equivalent to the
	// pre-0.30.250 unpaginated path and to httpcall.Do
	// (feedback_cache_must_not_constrain_jq.md).
	raw, mErr := json.Marshal(resultList.UnstructuredContent())
	if mErr != nil {
		return nil, false, mErr
	}
	return raw, true, nil
}

// pathHasNullPathSegment reports whether a RESOLVED apiserver path carries the
// fingerprint of a jq-templated segment that folded to empty — i.e. a TRAILING
// SLASH (`.../compositiondefinitions/`) or an EMPTY INTERIOR SEGMENT
// (`.../namespaces//...`). Task #118.
//
// This is the discriminator between the two ways name=="" is reached in the
// LIST branch of dispatchViaInternalRESTConfig:
//
//   - Unintended collapse (returns true): an RA authored a name and/or
//     namespace segment through a jq concat template
//     (`... + "/<resource>/" + .name`) and the fed value was null/absent.
//     gojq evaluates `string + null == string`, so the segment disappears and
//     the path keeps the separator slash that framed it — a trailing '/' when
//     the last segment folded, an empty '//' when an interior segment folded.
//     ParseAPIServerPathToDep then strips the trailing slash and parses
//     name=="", turning the intended by-name GET into a cluster-wide LIST.
//
//   - Intentional LIST (returns false): the RA authored NO name segment
//     (`.../compositiondefinitions`, or `.../namespaces/<ns>/<resource>`), so
//     the resolved path has neither a trailing slash nor an empty interior
//     segment. This is a legitimate "list all of kind X" step and MUST NOT
//     warn — else every legitimate LIST spams the log.
//
// The query string is stripped first (a resolved path may carry `?extras=...`);
// the check is purely on path structure. Note this also flags a null-namespace
// collapse that still carries a name (`.../namespaces//<resource>/foo`) — that
// too is an unintended collapse (an empty interior segment addresses the wrong
// scope), which the caller can log even though such a path takes the GET
// branch; here it is only consulted on the LIST branch, so it fires precisely
// on the null→cluster-LIST case task #118 targets.
func pathHasNullPathSegment(path string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		return false
	}
	// A trailing slash means the terminal templated segment folded to empty.
	if strings.HasSuffix(path, "/") {
		return true
	}
	// An empty interior segment ("//") means an interior templated segment
	// (typically the namespace) folded to empty. TrimPrefix the leading '/'
	// so the mandatory leading separator is not mistaken for an empty segment.
	return strings.Contains(strings.TrimPrefix(path, "/"), "//")
}
