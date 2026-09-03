// widget_content.go — Ship G (0.30.16x): identity-free widget content L1
// layer. This is "F1 one tier up": the F1 apistage class caches per-K8s-
// call envelopes identity-free; Ship G caches the resolved WIDGET envelope
// identity-free, one tier higher.
//
// TWO sites in this file:
//
//   1. populateWidgetContentL1 — the F2 walker's free side-effect of
//      widgets.Resolve. After the walker resolves a navigation widget
//      under the SA identity (withPhase1SAContext, phase1_walk.go:514),
//      this helper Put()s the encoded envelope under the identity-free
//      key (gvr, ns, name, perPage, page, extras=nil). The SA identity
//      sets every status.resourcesRefs.items[].allowed flag to true
//      (the snowplow SA holds */* get/list/watch); the stored body
//      carries those SA-evaluated flags un-stripped. They are NEVER
//      served verbatim — the gate runs on every Get-hit.
//
//   2. gateWidgetEnvelope — the serve-time per-user RBAC gate. On a
//      content-Get-hit (widgets.go), this helper unmarshals the cached
//      envelope, walks status.resourcesRefs.items[], OVERWRITES each
//      `allowed` flag via rbac.UserCan under the REQUEST identity
//      (xcontext.UserInfo), and re-encodes via the SAME helper a cold
//      resolve uses (encodeResolvedJSON — SetIndent("", "  ")). The
//      served body is byte-identical to a cold per-user resolve up to
//      `status.traceId` (which the cache content does NOT carry — the
//      walker never sets it; widgets.go injects traceId only on the
//      cold-resolve path at :128-132).
//
// AC-G.3 cross-user share: admin and cyberjoker hit the SAME L1 key for
// the same (gvr, ns, name, perPage, page). One Put on first request;
// the second request Gets the shell + runs the gate independently. The
// cache content is shared; the body that leaves the pod is per-user.
//
// AC-G.4 byte-equivalence: the gate is a re-run of the SAME function
// (rbac.UserCan -> EvaluateRBAC) over the SAME typed-RBAC snapshot the
// cold-resolve uses at resourcesrefs/resolve.go:88-92. By construction
// the gated body == a cold per-user resolve, modulo `status.traceId`
// (cache content has none; the dispatcher writes the gated body
// directly so no per-request traceId is injected on the hit path
// either — see widgets.go).
//
// Per feedback_l1_per_user_keyed_never_cohort: this layer appears to
// violate the per-user-keyed invariant on first read, but it does NOT
// because the gate runs on EVERY Get-hit. The cache content is the
// SHELL (with SA-evaluated `allowed=true` flags); the body that leaves
// the pod is per-user-narrowed. Same architectural property F1 used
// for the apistage class — the feedback file's invariant prohibits
// serving cached content VERBATIM, not the existence of an identity-
// free shell layer behind a per-user gate.

package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/maps"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/objects"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// widgetContentL1Key returns the identity-free widget content L1 key for
// (gvr, ns, name, perPage, page) and the canonical inputs that hashed
// into it. Returns ("", nil) when the layer or the cache subsystem are
// disabled — callers MUST treat key=="" as "skip the content layer".
//
// Shared by populateWidgetContentL1 (post-Resolve Put) and the F2 walker
// at phase1_walk.go (pre-Resolve L1KeyContext install). Both call sites
// MUST hash to the same cell so the inner-call dep edges the resolver
// records via L1KeyFromContext attach to the SAME L1 entry the walker
// subsequently Put()s.
func widgetContentL1Key(gvr schema.GroupVersionResource, namespace, name string, perPage, page int) (string, *cache.ResolvedKeyInputs) {
	if cache.ResolvedCache() == nil {
		return "", nil
	}
	if !cache.WidgetContentL1Enabled() {
		return "", nil
	}
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassWidgetContent,
		Group:           gvr.Group,
		Version:         gvr.Version,
		Resource:        gvr.Resource,
		Namespace:       namespace,
		Name:            name,
		// Username/Groups intentionally zero — identity-free key.
		PerPage: perPage,
		Page:    page,
		// Extras nil at prewarm — the walker does not receive
		// user-supplied extras; serve-time requests with non-nil
		// extras hash to a different cell and miss the prewarmed
		// entry. Acceptable: extras-bearing requests are the rare
		// per-request shape, the bulk navigation never carries them.
	}
	return cache.ComputeKey(inputs), &inputs
}

// populateWidgetContentL1 is the F2 walker's free side-effect of
// widgets.Resolve — Ship G (0.30.16x) §2.3. After the walker resolves a
// navigation widget under the SA identity, this helper Puts the encoded
// envelope under the identity-free key AND records dep edges into the
// DepTracker so an informer event on any K8s object the widget transitively
// depends on dirty-marks the entry and the refresher re-resolves it.
//
// Args:
//   - ctx — the SA-credentialed walker context (withPhase1SAContext).
//     The Put itself is identity-independent; the ctx is unused here
//     beyond a defensive nil-cache guard.
//   - gvr — the widget CR's GVR. Threaded from the caller — either
//     got.GVR at the recursive call site or schema.ParseGroupVersion of
//     the resolved ObjectReference at the root site.
//   - in — the widget CR (the resolver's input). namespace/name read
//     from its metadata via in.GetNamespace/GetName.
//   - perPage, page — the pagination the walker resolved THIS widget
//     under. Part of the content key so prewarm and serve-time agree
//     on the same key when perPage matches.
//   - res — the resolved widget envelope (resolver output). Encoded
//     verbatim — no strip of `allowed` flags; the gate overwrites
//     them per-request.
//
// Dep edges recorded (mirrors widgets.go:195 per-user path exactly):
//   - Self-dep: (gvr, ns, name) -> key. DELETE on the widget CR evicts
//     the content entry via DepTracker.RemoveL1.
//   - apiRef -> RestAction (when spec.apiRef present).
//   - Each render-eligible resourcesRefs item -> key (action-only refs
//     filtered out per Revision 14).
//   - Inner-call deps (edge type 3, recorded inside the resolver via
//     L1KeyFromContext) are wired by the walker installing
//     cache.WithL1KeyContext(ctx, key) BEFORE widgets.Resolve — see
//     phase1_walk.go around the widgets.Resolve call.
//
// No return value: a Put failure (cache nil, encode failure, etc.) is
// logged at debug. The walker's primary purpose (informer discovery)
// continues unaffected.
//
// Concurrency: ResolvedCache().Put is mutex-guarded; concurrent walker
// invocations on different widgets are safe. Same-widget concurrent
// Puts (a no-op in production — the walker is single-threaded per root)
// would replace-in-place via the LRU's index lookup.
func populateWidgetContentL1(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	in *unstructured.Unstructured,
	perPage, page int,
	res *unstructured.Unstructured,
) {
	if in == nil || res == nil {
		return
	}
	key, inputs := widgetContentL1Key(gvr, in.GetNamespace(), in.GetName(), perPage, page)
	if key == "" {
		return
	}
	c := cache.ResolvedCache()
	if c == nil {
		return
	}
	log := xcontext.Logger(ctx)
	if log == nil {
		log = slog.Default()
	}

	// Ship 1.3 (lever 1) — defense-in-depth empty-store guard. Do NOT Put
	// a transient-empty poison SHELL. The identity-free content cell is
	// hit by the frontend at the SAME (perPage, page) the walker seeds;
	// once an EMPTY shell lands there it is served as a permanent stale
	// HIT (the >3,100-cycle defect, project_prewarm_page_offset_bug). Lever
	// 2 makes the refresher CORRECT such a cell, but a cell that is never
	// poisoned at boot needs no correction — so we refuse to store the
	// poison shape in the first place.
	//
	// The poison SHAPE (mechanism-uniform, no widget-name/GVR special-case
	// per feedback_no_special_cases): the widget DECLARES an apiRef AND a
	// resourcesRefsTemplate (so its entire status.resourcesRefs.items list
	// is BUILT by fanning the template over the apiRef RA's data), yet the
	// resolved status.resourcesRefs.items came back EMPTY. Under the SA-
	// maximal walk identity (withPhase1SAContext — Ship 1.1 CohortNSACL
	// `*/*` permitAll=true) an empty result for an apiRef+template-driven
	// widget means the apiRef RA was TRANSIENTLY empty at walk time (the
	// boot data-availability window), NOT a genuine zero. We skip the Put;
	// the cell stays unseeded → the serve-time path falls through to the
	// per-user/cohort L1 (apiRef-RA-narrowed, correct) until lever 2's
	// refresher or a later walk pass stores a populated shell.
	//
	// Diego directive (Ship 1.3): the empty-check keys ONLY on
	// status.resourcesRefs.items — the authoritative per-user-narrowed data
	// path — NEVER on status.widgetData.items (not a data signal here).
	//
	// A widget with NO apiRef, or with ONLY a static spec.resourcesRefs
	// (no template), or one that genuinely yields zero template items is
	// PRESERVED: we guard exclusively the apiRef+template-driven shape that
	// resolved empty. recordWidgetDeps is also skipped on the guarded path
	// — there is no entry to dep-track.
	// Ship (task #69) — RBAC-sensitivity routing guard. An apiRef-driven
	// render-template widget (piechart/table over an aggregating apiRef RA)
	// renders from status.widgetData, which the serve-time gate NEVER
	// narrows per-user — so the identity-free cell would serve every user
	// the SA-maximal aggregate (a cross-user leak). NEVER write the
	// identity-free cell for such a widget; the serve path routes it to the
	// per-cohort `widgets` L1 (RBAC-narrowed at resolve under each cohort's
	// own identity). recordWidgetDeps is also skipped — there is no
	// identity-free entry to dep-track. Checked BEFORE the empty-shell guard
	// because classification supersedes it (a classified widget never
	// touches this cell at all, empty or populated).
	if isRBACSensitiveApiRefWidget(res.Object) {
		log.Debug("widget_content.populate.skip_rbac_sensitive",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Int("perPage", perPage),
			slog.Int("page", page),
			slog.String("reason", "apiRef+render-template widget renders from status.widgetData (not narrowed by the serve-gate) — routed to the per-cohort widgets L1; not seeding identity-free cell"),
		)
		bumpWidgetContentSkippedRBACSensitive()
		return
	}

	if shouldSkipEmptyWidgetShell(res) {
		log.Debug("widget_content.populate.skip_empty_shell",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Int("perPage", perPage),
			slog.Int("page", page),
			slog.String("reason", "apiRef+resourcesRefsTemplate widget resolved with empty status.resourcesRefs.items — transient-empty poison shell; not seeding identity-free cell"),
		)
		bumpWidgetContentSkippedEmptyShell()
		return
	}

	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		// A failed encode at prewarm is non-fatal — the serve-time
		// resolve still works. Log at debug; the walker continues.
		log.Debug("widget_content.populate.encode_failed",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Any("err", err),
		)
		return
	}
	// Ship 0.30.257 (#313) Cache-A — error-aware Put-gate for the
	// identity-free widget-content cell. The walker resolves the widget's
	// apiRef RESTAction transitively; the api resolver bumps a stage-error
	// sink installed on the resolve ctx (phase1_walk.go) whenever it writes
	// dict[errorKey] for a per-item iterator hard error. After #313 a
	// per-item failure no longer truncates the result — so without this gate
	// a partial-with-errors SHELL could be Put into the identity-free cell
	// and served (gated per-user) for the TTL. Symmetric with the refresher
	// Put-gate (resolve_populate.go:242), the request-path RESTAction/widget
	// gates (restactions.go / widgets.go), and the 0.30.254 "never cache an
	// under-served result" posture. sink==nil (no sink threaded on this ctx
	// — e.g. a caller that did not install one) is nil-receiver-safe
	// (Count()==0) and Puts as before; recordWidgetDeps is ALSO skipped on
	// the declined path — there is no entry to dep-track.
	if sink := cache.StageErrorSinkFromContext(ctx); sink.Count() > 0 {
		log.Debug("widget_content.populate.skip_stage_error",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Int("perPage", perPage),
			slog.Int("page", page),
			slog.Int64("stage_errors", sink.Count()),
			slog.String("reason", "apiRef RESTAction resolved with per-item stage error(s) — partial-with-errors shell; not seeding identity-free cell (Cache-A)"),
		)
		return
	}

	// External-no-cache (proposal 2026-06-22) — decline to seed the
	// identity-free content cell when the widget's apiRef resolve touched a
	// genuine external endpoint (no informer/dep edge can invalidate it). The
	// F2 walker installs the external-touched sink on resolveCtx
	// (phase1_walk.go); recordWidgetDeps is ALSO skipped on this declined path
	// (the return below) — there is no entry to dep-track.
	if extSink := cache.ExternalTouchedSinkFromContext(ctx); extSink.Count() > 0 {
		cache.BumpExternalSkippedPut()
		log.Debug("widget_content.populate.skip_external_touched",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Int("perPage", perPage),
			slog.Int("page", page),
			slog.Int64("external_touches", extSink.Count()),
			slog.String("reason", "apiRef RESTAction touched an external endpoint — no dep edge to invalidate; not seeding identity-free cell, served live per /call"),
		)
		return
	}

	// 1.12.3 A-1 / R-1 (SECURITY, cross-tenant) — decline to seed the identity-free
	// content cell when the widget's resolve ran a userAccessFilter refilter. Third
	// sibling of the stage-error and external-touched gates above, same shape, same
	// early return (which also skips recordWidgetDeps — there is no entry to
	// dep-track).
	//
	// THIS CELL IS THE WORST PLACE A NARROWED BODY COULD LAND: it carries NO
	// identity fold at all, so it is shared across every user, and the serve-time
	// gate re-derives only status.resourcesRefs.items[].allowed — it never narrows
	// status.widgetData. isRBACSensitiveApiRefWidget is meant to route apiRef+
	// template widgets to the per-cohort layer before they ever get here, but it
	// is a declaration-shaped heuristic that de-classifies on accessor error. This
	// gate makes the property hold by OBSERVATION rather than by that argument.
	if uafSink := cache.UAFTouchedSinkFromContext(ctx); uafSink.Count() > 0 {
		cache.BumpWidgetsUAFPutDeclined()
		log.Debug("widget_content.populate.skip_user_access_filter",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", in.GetNamespace()),
			slog.String("name", in.GetName()),
			slog.Int("perPage", perPage),
			slog.Int("page", page),
			slog.Int64("uaf_touches", uafSink.Count()),
			slog.String("reason", "apiRef RESTAction ran a userAccessFilter refilter — the envelope is narrowed for the resolving identity, and this cell is identity-FREE and shared; not seeding it (1.12.3 A-1/R-1)"),
		)
		return
	}

	// scope-waiver:TTLOverride: widgetContent-class cell — identity-free shared envelope, per-user serve-time filter (gateWidgetEnvelope). 1.12.3 A-1/R-1: it holds no per-user UAF refilter output because a refilter-touched resolve can no longer REACH this Put (the UAFTouchedSink gate immediately above declines it) — previously this rested on isRBACSensitiveApiRefWidget's routing argument alone, which the R-1 finding showed is not a safe basis for a UAF claim (uaf_shortttl.go R-d-4 SITE MAP).
	c.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  inputs,
	})

	// Record dep edges so K8s informer events dirty-mark this entry and
	// the refresher re-resolves it. Without this, the entry is TTL-only
	// stale-forever — the AC-G.5 defect the architect's diff-review caught.
	// Mirrors the per-user widgets.go:195 path exactly: same arguments,
	// same call site (after Put). Use `res` (the resolved envelope) — it
	// carries spec.apiRef and spec/status.resourcesRefs.items[] that
	// recordWidgetDeps reads (the walker's `in` and `res` are the same CR
	// shape; recordWidgetDeps is robust to either, but we mirror widgets.go).
	recordWidgetDeps(log, key, gvr, res)
}

// isRBACSensitiveApiRefWidget reports whether `obj` (a widget CR's
// `.Object` map — either the fetched CR at serve time or the resolved
// envelope at populate time, both of which carry the original `spec.*`)
// is an apiRef-driven widget whose RENDERED OUTPUT is RBAC-sensitive and
// therefore MUST NOT be served from the identity-free `widgetContent`
// cell.
//
// WHY THIS EXISTS (the leak class this closes). The identity-free
// `widgetContent` cell is shared across users (keyed by widget+pagination,
// NOT identity). The serve-time gate (gateWidgetEnvelope) only re-derives
// `status.resourcesRefs.items[].allowed` per requester — it NEVER narrows
// `status.widgetData`. So a piechart/table that renders ENTIRELY from
// `status.widgetData` (series.total, data=${.list}) computed by an apiRef
// RA that aggregates cross-namespace would serve EVERY user the SA-maximal
// full aggregate → cross-user leak. The fix routes these widgets to the
// per-cohort `widgets` L1.
//
// CORRECTED 1.12.3 (A-1/R-1) — the destination is NOT unconditionally safe.
// This comment used to end "...the per-cohort `widgets` L1, which is
// RBAC-correct by construction (each cohort resolves the apiRef RA under its
// OWN identity → narrowed at resolve; no shared cell, no serve-gate, no
// leak)". The premise is wrong in one specific and now-confirmed case. The
// `widgets` cell is keyed per BINDING, not per USER (BindingUID + RBACSubGen;
// username/groups are excluded from the key by design — resolved.go). "Each
// cohort resolves under its own identity" is true, but a COHORT IS NOT A USER:
// when the apiRef'd RA carries a userAccessFilter, two users who share the
// first-match binding but have different per-object grants are one cohort with
// two different correct bodies, and the first one to arrive writes the cell the
// second reads. That is A-1, and this routing decision is what makes the
// `widgets` cell its highest-volume carrier.
//
// The routing itself is still RIGHT — the shared identity-free cell would be
// strictly worse. What changed is that the destination now carries its own
// guard: a refilter-touched resolve declines its Put at every widgets-class
// site (the UAFTouchedSink gate; uaf_shortttl.go R-d-4 SITE MAP). Until 1.13.0
// folds the UAF scope into the key (v7), such widgets are simply not cached —
// so "no leak" holds again, by a decline rather than by construction.
//
// SHAPE-BASED, no widget-name/GVR literal (feedback_no_special_cases).
// True IFF the widget DECLARES a non-empty spec.apiRef AND declares at
// least one render template (spec.widgetDataTemplate OR
// spec.resourcesRefsTemplate) — i.e. its rendered output is BUILT from the
// apiRef RA's data.
//
// Over-classifying is BENIGN: a misclassified widget simply takes the
// per-cohort path, which is never a leak (just one extra per-cohort cell).
// An identity-invariant widget without an apiRef OR without any template
// stays false → keeps using the identity-free layer.
//
// Accessor errors are treated as "absent" (the accessor returns a zero
// value alongside the error). A read failure on the apiRef name yields
// false (no apiRef ⇒ not classified); a read failure on a template yields
// a zero-length slice for that template (so it does not contribute to the
// OR). This is the conservative direction for the predicate: a transient
// read failure de-classifies, falling back to the identity-free layer —
// which is the unchanged pre-fix posture, never a new leak path (the gate
// still runs on that layer).
//
// LOAD-BEARING INVARIANT (arch-rev-70) — error-direction symmetry with the
// resolver. The de-classify-on-error direction is SAFE ONLY because the
// resolver's resolveWidgetData (internal/resolvers/widgets/resolve.go:184-188)
// reads the SAME accessor (widgets.GetWidgetDataTemplate) and, on the same
// read error, FAILS SOFT to static-only widgetData — it builds NO
// cross-namespace aggregate. So a widget the predicate de-classifies (and
// therefore routes to the shared identity-free cell) cannot, on that same
// error, contain a leak-bearing aggregate. If the resolver ever stopped
// failing soft on this error while the predicate still de-classified, the
// SA-maximal aggregate would land in the identity-free cell → reopening the
// cross-user leak. These two sites MUST stay symmetric.
func isRBACSensitiveApiRefWidget(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	// #72 / C-INLINE-1 (HARD) — independent OR-clause AT THE TOP, BEFORE the
	// apiRef short-circuit below. A widget bearing any inline+GET resourcesRefs
	// item embeds a server-resolved child `rendered` body that is narrowed per
	// requesting-user identity; it MUST route to the per-user `widgets` L1, NEVER
	// the shared identity-free `widgetContent` cell (or the SA-maximal child
	// render would leak cross-user — the task #69 leak class). This MUST run
	// before the `apiRef.Name == ""` return at the next stmt: an inline widget
	// with NO apiRef would otherwise fall through to `return false` → shared
	// cell → leak. Gates symmetrically at the serve-READ (widgets.go) and the
	// populate-WRITE (widget_content.go) sites that both call this predicate.
	if hasInlineGETRef(obj) {
		return true
	}
	apiRef, err := widgets.GetApiRef(obj)
	if err != nil || apiRef.Name == "" {
		return false
	}
	wdt, _ := widgets.GetWidgetDataTemplate(obj)
	rrt, _ := widgets.GetResourcesRefsTemplate(obj)
	return len(wdt) > 0 || len(rrt) > 0
}

// shouldSkipEmptyWidgetShell reports whether the resolved widget `res` is
// a TRANSIENT-EMPTY POISON SHELL that must NOT be stored into the
// identity-free content cell (Ship 1.3 lever 1).
//
// True IFF ALL hold:
//   - the widget DECLARES a non-empty spec.apiRef (its data source is an
//     external RESTAction), AND
//   - the widget DECLARES a non-empty spec.resourcesRefsTemplate (so its
//     ENTIRE status.resourcesRefs.items list is built by fanning the
//     template over the apiRef RA's resolved data — there is no static
//     resourcesRefs floor that would survive an empty apiRef), AND
//   - the resolved status.resourcesRefs.items came back EMPTY.
//
// Under the SA-maximal walk identity (Ship 1.1 CohortNSACL `*/*`
// permitAll=true) an empty result for such a widget can only be a
// transient apiRef-RA emptiness at boot — never a genuine RBAC-narrowed
// zero (the SA sees everything). Storing it would poison the cell the
// frontend hits. A widget without an apiRef, without a template, or with
// a non-empty resolved list is NOT a poison shell and is stored normally.
//
// Per the Ship 1.3 directive the emptiness check keys ONLY on
// status.resourcesRefs.items — never status.widgetData.items.
func shouldSkipEmptyWidgetShell(res *unstructured.Unstructured) bool {
	if res == nil {
		return false
	}
	obj := res.Object

	// Declares a non-empty apiRef? (GetApiRef defaults Resource/APIVersion
	// even for an absent block, so gate on the apiRef NAME being present.)
	apiRef, err := widgets.GetApiRef(obj)
	if err != nil || apiRef.Name == "" {
		return false
	}

	// Declares a resourcesRefsTemplate? (If the list is built only from a
	// static spec.resourcesRefs, an empty result is authoritative — not a
	// poison shape — so we do not guard it.)
	tpl, err := widgets.GetResourcesRefsTemplate(obj)
	if err != nil || len(tpl) == 0 {
		return false
	}

	// Resolved status.resourcesRefs.items empty?
	items, ok, err := maps.NestedSlice(obj, "status", "resourcesRefs", "items")
	if err != nil {
		// Read failure — be conservative and let the Put proceed (the
		// serve-time gate still narrows per-user; a false-negative here is
		// the unchanged pre-1.3 behaviour, never a leak).
		return false
	}
	if ok && len(items) > 0 {
		return false
	}
	// items absent or empty AND the widget is apiRef+template-driven →
	// transient-empty poison shell.
	return true
}

// gateWidgetEnvelope applies the serve-time per-user RBAC gate to a raw
// widget envelope retrieved from the identity-free content layer — the
// Ship G analogue of F1's gateContentEnvelope. It walks the embedded
// status.resourcesRefs.items[] slice and OVERWRITES each `allowed` flag
// via rbac.UserCan under the request identity.
//
// Returns (gatedEnvelope, served):
//   - served==false — fail-closed: no identity on ctx, or a malformed
//     stored envelope. The caller falls through to the existing per-
//     user widget L1 lookup, which ALSO nil-checks UserInfo at
//     dispatchCacheLookupKey and bails on the same condition.
//   - served==true  — gatedEnvelope is the RBAC-narrowed bytes ready
//     to write to the response wire. encodeResolvedJSON uses the SAME
//     encoder settings the cold-resolve path uses (SetIndent("", "  "))
//     so the body is byte-identical to a cold-resolve response for the
//     same request identity, modulo status.traceId (the cache content
//     has none; the dispatcher emits the gated body directly).
//
// AC-G.4 binding: per-item re-derivation is byte-equivalent to a fresh
// resolveOne loop because both call sites invoke rbac.UserCan with the
// SAME (Verb, GroupResource, Namespace) signature over the SAME typed-
// RBAC snapshot (resourcesrefs/resolve.go:88-92 — see verbs.go for the
// REST→kube verb mapping).
//
// Sub-microsecond per item (typed-RBAC snapshot lookup, no apiserver
// round-trip). N items per widget ≈ tens; gate budget per hit ≈ <50µs
// CPU, dominated by the json.Unmarshal of the cached body.
//
// FLAG, NOT DROP — Diego's ACCEPTED tradeoff (Ship 1.3, 2026-05-29). The
// gate re-derives `allowed` per requester but does NOT remove not-allowed
// items from status.resourcesRefs.items. This is the SAME shape a cold
// per-user resolve produces (resourcesrefs/resolve.go:88-115 appends every
// item unconditionally, flagging — never dropping — the not-allowed ones),
// so the gated body is byte-equivalent to a cold resolve. The boundary is
// the FLAG, by design:
//   - the frontend renders ONLY items with allowed==true. When the SA-
//     maximal shell (lever 2 populates the full list under the SA identity)
//     is served to cyberjoker (krateo-system only), every cross-namespace
//     panel re-derives allowed==false → the frontend renders 0; admin
//     re-derives allowed==true → renders the full set.
//   - the per-request RBAC gate at the dispatch entrypoint (widgets.go /
//     restactions.go checkDispatchRBAC, and EvaluateRBAC on every /call)
//     independently DENIES any attempt to FETCH a not-allowed item's
//     `path` — so a not-allowed item is metadata only, never actionable.
//   - ACCEPTED residue: a not-allowed item's metadata (id / path / name /
//     namespace) remains in the served bytes flagged allowed==false. Diego
//     ruled this acceptable (flag, not drop); a drop is intentionally NOT
//     added. status.widgetData.items is NEVER consulted as a data signal
//     (Diego directive) — resourcesRefs.items is the authoritative path.
func gateWidgetEnvelope(
	ctx context.Context,
	raw []byte,
) ([]byte, bool) {
	if _, err := xcontext.UserInfo(ctx); err != nil {
		// FAIL-CLOSED: no identity to gate against. The caller falls
		// through to the existing per-user L1, which dispatch also
		// nil-checks UserInfo and bails — same contract.
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	// Walk status.resourcesRefs.items[] (a []any of map[string]any per
	// the marshalled templatesv1.ResourceRefResult shape). For each
	// item, re-derive `allowed` under THIS user's identity. The other
	// fields (id, path, verb, payload) are identity-invariant by §2.2
	// of the design doc — preserved untouched.
	if items, ok, err := maps.NestedSlice(obj, "status", "resourcesRefs", "items"); ok && err == nil {
		for i, raw := range items {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			it["allowed"] = recomputeAllowedFromRefItem(ctx, it)
			items[i] = it
		}
		// SetNestedField replaces the slice in place — the items slice
		// is the same value we just mutated. The Set is defensive
		// against any internal copy semantics the maps package may
		// introduce in the future.
		_ = maps.SetNestedField(obj, items, "status", "resourcesRefs", "items")
	}
	encoded, err := encodeResolvedJSON(obj)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// recomputeAllowedFromRefItem replays resourcesrefs/resolve.go:88-92 for
// ONE item under the request identity. The item carries:
//   - "path" — the /call?... URL (built via resourcesrefs/resolve.go's
//     buildPath; carries resource/apiVersion/namespace/name).
//   - "verb" — the REST verb (GET/POST/PUT/PATCH/DELETE per the
//     kubeToREST map in resourcesrefs/verbs.go).
//
// Mapping back: the GVR is reconstructed from the item's path via
// util.ParseCallPathToObjectRef (callpath.go:36) + inline
// schema.ParseGroupVersion + GroupVersion.WithResource (the same
// pattern util.ParseGVR at gvr.go:23 uses for HTTP requests). The
// REST verb is mapped back to its kube equivalent (POST→create,
// GET→get, etc. — see verbs.go's restToKube). The (verb, gvr.GroupResource,
// namespace) tuple is then handed to rbac.UserCan — the EXACT same
// signature resourcesrefs/resolveOne uses at resolve.go:88-92.
//
// Returns false (fail-closed) on any parse failure or missing
// path/verb — defensive against a malformed cached item. The caller
// in gateWidgetEnvelope writes the result back to `allowed`, so a
// degraded item shows allowed=false rather than the SA-evaluated
// allowed=true the cache shell may carry.
func recomputeAllowedFromRefItem(ctx context.Context, item map[string]any) bool {
	path, _ := item["path"].(string)
	verb, _ := item["verb"].(string)
	if path == "" || verb == "" {
		return false
	}
	ref, ok := objects.ParseCallPathToObjectRef(path)
	if !ok {
		// Not a /call endpoint — external URL or malformed. The cold-
		// resolve path would not have set allowed=true for this item
		// either (the path would not parse via resourcesrefs's
		// buildPath shape), so false is the correct fail-closed verdict.
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false
	}
	gvr := gv.WithResource(ref.Resource)
	kubeVerb, ok := restVerbToKube(verb)
	if !ok {
		return false
	}
	return rbac.UserCan(ctx, rbac.UserCanOptions{
		Verb:          kubeVerb,
		GroupResource: gvr.GroupResource(),
		Namespace:     ref.Namespace,
	})
}

// restVerbToKube maps an HTTP method back to its kube verb — the inverse
// of resourcesrefs/verbs.go's kubeToREST map. Kept here (not lifted into
// internal/handlers/util) because it is the gate's sole consumer and is
// trivially small; lifting would invert the verbs.go authoring direction
// for no reuse gain.
//
// The map mirrors resourcesrefs/verbs.go's restToKube exactly. Returns
// ok=false for an unknown verb so the gate fails closed rather than
// guessing.
func restVerbToKube(restVerb string) (string, bool) {
	switch strings.ToUpper(restVerb) {
	case http.MethodGet:
		return "get", true
	case http.MethodPost:
		return "create", true
	case http.MethodPut:
		return "update", true
	case http.MethodPatch:
		return "patch", true
	case http.MethodDelete:
		return "delete", true
	default:
		return "", false
	}
}
