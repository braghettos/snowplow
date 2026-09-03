// Package widgets is the top-level frontend Widget resolver. Resolve takes a
// Widget CR and fills its status by orchestrating the sub-resolvers: apiRef
// (bridge to a RESTAction), resourcesRefs and resourcesRefsTemplate
// (resource reference expansion), and widgetDataTemplate (jq templating of
// widgetData). It canonicalises the unordered data the RESTAction emits into
// the shape the frontend renders, and validates the result against the
// widget's CRD schema.
package widgets

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/maps"
	v1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	crdschema "github.com/krateo-platformops/snowplow/internal/resolvers/crds/schema"

	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets/apiref"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets/resourcesrefs"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets/resourcesrefstemplate"

	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets/widgetdatatemplate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

type Widget = unstructured.Unstructured

type ResolveOptions struct {
	In      *Widget
	RC      *rest.Config
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

// resolveApiRefFn and validateObjectStatusFn are the two cross-boundary
// hops Resolve makes that require a live apiserver — the apiRef RESTAction
// fetch (apiref.Resolve, reached via resolveApiRef) and the CRD-status schema
// validation (crdschema.ValidateObjectStatus → cache.GVRFor discovery). They
// are exposed as package-level function VARIABLES bound to the real
// implementations so a HERMETIC unit test can drive the orchestration of
// Resolve (error propagation to status.error, the 400 StatusError wrap, and
// the resourcesRefsTemplateExtras scope-isolation ordering) WITHOUT a cluster,
// by transiently swapping the var and restoring it.
//
// Production behaviour is BYTE-IDENTICAL: the defaults are the exact functions
// Resolve called before (resolveApiRef and crdschema.ValidateObjectStatus),
// invoked with the same arguments in the same order — this is an indirection
// seam only, not a behaviour change (test-hook per the coverage-audit plan;
// no env flag, no special-case). Tests MUST restore the original value in a
// defer so package-level state never leaks across tests.
var (
	resolveApiRefFn        = resolveApiRef
	validateObjectStatusFn = crdschema.ValidateObjectStatus
)

func Resolve(ctx context.Context, opts ResolveOptions) (*Widget, error) {
	log := xcontext.Logger(ctx).With(loggerAttr(opts.In.Object))

	// A2 contract: server-trusted identity injection into the RESOLVE INPUT
	// (definitive-cache-identity-architecture §1.3 + §2.4). For a widget whose CR
	// declares spec.identityContext, fold the declared subset of the
	// authenticated principal (DeclaredIdentity — the SAME derivation the cache
	// key path uses via effectiveKeyExtras) into opts.Extras, so the declared
	// identity reaches BOTH the widgetDataTemplate/resourcesRefsTemplate jq
	// (mergeExtras(ds, opts.Extras) below) AND the apiRef fetch (resolveApiRef).
	// INJECTION WINS over any client-supplied value for the declared keys — this
	// is the quarantine that closes the spoofability hole (§1.3, F-ARCH-3): a
	// request carrying extras.username=SOMEONE_ELSE is overwritten by the JWT's
	// own username. CACHE-OFF TRANSPARENT (§2.4, F-ARCH-6): this is part of the
	// widget's INPUT contract, not a cache feature — it runs here in the resolve
	// path whether or not the cache is on, so declared-widget output is identical
	// cache-on vs cache-off for the same JWT. INERT for the ~99% identity-free
	// corpus: DeclaredIdentity returns nil when no identityContext is declared →
	// opts.Extras is untouched → byte-identical to pre-A2.
	// A-3 (1.12.3) — STRIP client-supplied identity extras this widget has not
	// declared, BEFORE the injection merge below and before mergeExtras puts the
	// extras in the jq dict.
	//
	// THE DEFECT. Identity keys are dropped from the widget CACHE KEY unless
	// declared, which is correct — but nothing stopped them reaching the RESOLVE
	// INPUT. For a widget declaring neither spec.identityContext nor
	// spec.keyExtras, DeclaredIdentity below returns nil (no injection fires), so
	// a request sending ?extras={"username":"evil"} had its own value merged into
	// the dict by mergeExtras and rendered into the body. Because the key ignores
	// identity, that body is then written into the per-BindingUID cell EVERY
	// co-cohort member reads. The identity dimension partitions the KEY; it never
	// sanitised the BODY, and the two were conflated.
	//
	// WHY HERE AND NOT AT THE DISPATCHER. Every caller must get the same
	// contract: the /call dispatcher, the refresher's re-resolve, the boot/keepwarm
	// seed and nested resolves all reach this function, and a fix at one call site
	// leaves the others holding the raw map. This is the same single-derivation
	// discipline DeclaredIdentity itself follows (the #64 anti-drift principle) —
	// identity meets the resolve input in exactly one place.
	//
	// WHAT SURVIVES, and why each is safe:
	//   - declared in spec.identityContext — DeclaredIdentity OVERWRITES it
	//     server-side a few lines below (injection-wins, F-ARCH-3), so the client
	//     value is discarded anyway and the body is trustworthy.
	//   - declared in spec.keyExtras — the author asked for it to PARTITION the
	//     key, so a spoofed value SELF-QUARANTINES: it lands in a cell that only
	//     requests carrying that same value can ever reach, and no co-cohort user
	//     sending a different value (or none) is exposed to it. This is the
	//     pre-F6 self-quarantine, still intact for declared keys. It also keeps
	//     the live widgets that declare username in keyExtras working unchanged.
	//
	// Note the asymmetry the two accessors give us, which is deliberate:
	// GetIdentityContext is enum-filtered to {username, groups}, so displayName
	// can never be declared THERE; GetKeyExtras is NOT enum-filtered, so a widget
	// CAN declare displayName as a key extra and have it survive. That is the
	// hidden dependency between the two accessors, pinned by
	// TestA3_DisplayNameInKeyExtras_SurvivesAndPartitions.
	//
	// Non-identity extras are untouched: an undeclared `foo` still reaches the
	// resolve input (the deliberate KEY-ONLY split, dispatchers
	// f6_resolve_input_reach_test.go L12) and is still quarantined at the Put by
	// requestExtrasFullyDeclared.
	//
	// KEY-INERT. This cannot move any cache key: the dispatcher folds request
	// extras through filterDeclaredKeyExtras, which keeps ONLY
	// spec.keyExtras-declared names, and every key stripped here is by
	// construction outside that set.
	opts.Extras = sanitizeUndeclaredIdentityExtras(opts.In.Object, opts.Extras)

	if inj := DeclaredIdentity(ctx, opts.In.Object); len(inj) > 0 {
		merged := make(map[string]any, len(opts.Extras)+len(inj))
		for k, v := range opts.Extras {
			merged[k] = v
		}
		for k, v := range inj { // injection wins on collision (quarantine)
			merged[k] = v
		}
		opts.Extras = merged
	}

	// Ship 0.30.193 Checkpoint 1 — widget-resolve phase wall-clocks.
	// Captured per-phase so seedOneWidget's deferred log can attribute
	// total widget-resolve cost across apiref / widgetData / resrefs /
	// validate. The sink is checked once to skip overhead on the
	// production /call path (no sink installed → no log emitted by
	// caller); phase locals are still computed (4 × time.Now → ~120ns)
	// to keep the code path uniform per feedback_no_special_cases.
	pipSink := cache.PIPStageTimingSinkFrom(ctx)
	phaseApirefMs := int64(0)
	phaseWidgetDataMs := int64(0)
	phaseResRefsMs := int64(0)
	phaseValidateMs := int64(0)
	defer func() {
		// Only emit the structured phase-timing log when a sink is
		// wired (PIP seed path). Production /call has no sink → no log
		// → zero additional log overhead.
		if pipSink == nil {
			return
		}
		log.Info("phase1.seed.widget.phase.timing",
			slog.String("subsystem", "cache"),
			slog.Int64("apiref_ms", phaseApirefMs),
			slog.Int64("widget_data_ms", phaseWidgetDataMs),
			slog.Int64("resrefs_ms", phaseResRefsMs),
			slog.Int64("validate_ms", phaseValidateMs),
		)
	}()

	apirefStart := time.Now()
	ds, err := resolveApiRefFn(ctx, opts)
	phaseApirefMs = time.Since(apirefStart).Milliseconds()
	if err != nil {
		log.Error("unable to resolve api reference", slog.Any("err", err))
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	// Ship H7 (0.30.161): re-inject the pagination triple into the
	// widget data source. The api-resolver constructed `dict["slice"]`
	// at internal/resolvers/restactions/api/resolve.go:211-215, but the
	// RESTAction's spec.Filter projection (e.g. compositions-list emits
	// `{list: (.allCompositions // [])}`) strips `.slice` from the dict
	// before it arrives here as `ds`. The widget jq expects `.slice`
	// (see e.g. table.dashboard-compositions-panel-row-table.yaml:32 —
	// `if .slice then $sorted[0 : (.slice.page * .slice.perPage)] else …`)
	// and silently falls through to "return all rows" when absent —
	// materialising the unbounded list at the resolver layer.
	injectSlice(ds, opts.PerPage, opts.Page)

	// extras-widgets parity (step 2): merge the per-request extras into
	// `ds` so the widget's widgetDataTemplate AND resourcesRefsTemplate jq
	// can reference extras keys, and so apiRef-less (static) widgets get
	// them too. Non-overwriting (any apiRef-result key or the injectSlice
	// triple above WINS on collision). See mergeExtras for the full
	// rationale + precedence.
	mergeExtras(ds, opts.Extras)

	widgetDataStart := time.Now()
	widgetData, err := resolveWidgetData(ctx, opts.In, ds)
	phaseWidgetDataMs = time.Since(widgetDataStart).Milliseconds()
	if err != nil {
		log.Error("unable to resolve widget data", slog.Any("err", err))
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	err = maps.SetNestedField(opts.In.Object, widgetData, "status", widgetDataKey)
	if err != nil {
		log.Error("unable to set status as unstructured.NestedMap",
			slog.Any("err", err))
		return opts.In, err
	}

	// inline-extras design P §4.2 — resourcesRefsTemplate surface. Fold the
	// author-declared spec.resourcesRefsTemplateExtras into `ds` HERE — AFTER
	// resolveWidgetData has fully returned (above) and BEFORE
	// resolveResourceRefs (below). This mutation timing IS the scope
	// isolation (design §2.1): resolveWidgetData never re-reads `ds`, so this
	// fold is invisible to the widgetDataTemplate jq and reaches ONLY the
	// resourcesRefsTemplate jq (which evaluates against this same `ds`).
	// Falsifier #5 guards this boundary.
	//
	// REUSE mergeExtras verbatim — its non-overwriting / ds-wins semantics are
	// EXACTLY right: the apiRef-result (from resolveApiRef) and the per-request
	// extras (the mergeExtras(ds, opts.Extras) fold above) are ALREADY in `ds`,
	// so they win; the rrt-inline map only fills keys nobody else set. Net for
	// this surface: apiRef-result > request > rrt-inline. Empty/absent block ⇒
	// GetResourcesRefsExtras returns {} ⇒ mergeExtras len-guard no-ops ⇒
	// byte-identical to pre-inline-extras.
	mergeExtras(ds, GetResourcesRefsExtras(opts.In.Object))

	resRefsStart := time.Now()
	resourcesRefsResults, err := resolveResourceRefs(ctx, opts.In, ds)
	phaseResRefsMs = time.Since(resRefsStart).Milliseconds()
	if err != nil {
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	if tot := len(resourcesRefsResults); tot > 0 {
		tmp, err := maps.StructSliceToMapSlice(resourcesRefsResults)
		if err != nil {
			return opts.In, err
		}

		pig := map[string]any{
			"items": tmp,
		}
		if opts.PerPage > 0 && opts.Page > 0 {
			hasNext := (tot >= opts.PerPage)
			page := opts.Page
			if hasNext {
				page = page + 1
			}
			pig["slice"] = map[string]any{
				"perPage":  opts.PerPage,
				"page":     page,
				"continue": hasNext,
			}
		}

		err = maps.SetNestedField(opts.In.Object, pig, "status", resourcesRefsKey)
		if err != nil {
			return opts.In, err
		}
	}

	validateStart := time.Now()
	// Ship 2 (production-aim cleanup 2026-06-01) — pass opts.RC in
	// BOTH paths. The previous TestMode-gated branch passed nil to
	// ValidateObjectStatus on the production path, forcing the
	// validator to recover the rc via rest.InClusterConfig() —
	// useless work AND a latent defect for non-in-cluster
	// deployments (the seed path runs with a context-injected
	// internal-dispatch rc, NOT in-cluster). The call site KNOWS
	// the rc; the helper shouldn't recover it.
	err = validateObjectStatusFn(ctx, opts.RC, opts.In.Object)
	phaseValidateMs = time.Since(validateStart).Milliseconds()
	if err != nil {
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, &apierrors.StatusError{
			ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusBadRequest,
				Reason:  metav1.StatusReasonBadRequest,
				Message: err.Error(),
			}}
	}

	return opts.In, nil
}

func resolveApiRef(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	apiRef, err := GetApiRef(opts.In.Object)
	if err != nil {
		return nil, err
	}

	// inline-extras design P §4.1 — apiRef surface. Fold the author-declared
	// spec.apiRef.extras UNDER the per-request extras (request wins) into the
	// effective map threaded to the apiRef fetch. Net precedence on this
	// surface: apiRef-RESULT > request > apiRef-inline (the apiRef RA result
	// overwrites the dict on collision inside api.Resolve, so the result still
	// wins). The inline map reads the `extras` sub-key off opts.In.Object — the
	// SAME widget CR the dispatcher reads (got.Unstructured), so key (the
	// dispatcher's union, §1) and body stay consistent. nil/empty inline +
	// nil/empty request ⇒ a fresh empty effective map ⇒ byte-identical to the
	// pre-inline-extras "request-only" thread (mergeRequestWins of two empties
	// is empty; api.Resolve no-ops on an empty dict). The seed path benefits
	// automatically — it reads opts.In.Object too (the seeded CR), so the seed
	// body folds apiRef-inline with no new ResolveOptions field (design §5).
	apiRefInline := GetApiRefExtras(opts.In.Object)
	apiRefEff := mergeRequestWins(apiRefInline, opts.Extras)

	return apiref.Resolve(ctx, apiref.ResolveOptions{
		RC:      opts.RC,
		ApiRef:  apiRef,
		AuthnNS: opts.AuthnNS,
		PerPage: opts.PerPage,
		Page:    opts.Page,
		// extras-widgets parity (step 1): thread the EFFECTIVE extras
		// (apiRef-inline folded under request) into the apiRef fetch.
		// extras flows widget → apiref → restactions.Resolve → api.Resolve,
		// where api.Resolve seeds the resolve dict via
		// maps.DeepCopyJSON(opts.Extras) (restactions/api/resolve.go:228-230).
		// Effect: the apiRef RESTAction's OWN jq (its `path` / `payload`)
		// can reference extras keys (parametrise the fetch), and those
		// extras keys land top-level in the resolved data source `ds` the
		// widget templates evaluate against transitively. An empty effective
		// map deep-copies to an empty dict (no-op) — the prewarm/seed/refresher
		// callers (no request extras, no inline) stay byte-identical.
		Extras: apiRefEff,
	})
}

func resolveWidgetData(ctx context.Context, obj *Widget, ds map[string]any) (map[string]any, error) {
	log := xcontext.Logger(ctx)

	src := GetWidgetData(obj.Object)

	// LOAD-BEARING INVARIANT (task #69 / arch-rev-70) — error-direction
	// symmetry with the cache routing predicate. A widgetDataTemplate read
	// error here MUST fail-soft to the STATIC-ONLY widgetData (`src`, no
	// aggregate built). The cache predicate
	// (dispatchers.isRBACSensitiveApiRefWidget) reads the SAME accessor
	// (GetWidgetDataTemplate) and, on the same error, DE-CLASSIFIES the
	// widget → routes it to the identity-free widgetContent cell. These two
	// sites MUST stay symmetric: if a read error here ever started building
	// the cross-namespace aggregate while the predicate still de-classified,
	// the SA-maximal aggregate would land in the shared identity-free cell —
	// reopening the cross-user leak task #69 closed. Keep both fail-soft.
	wdt, err := GetWidgetDataTemplate(obj.Object)
	if err != nil {
		log.Warn("unable to get widgetDataTemplate", slog.Any("err", err))
		return src, nil
	}

	evals, err := widgetdatatemplate.Resolve(ctx, widgetdatatemplate.ResolveOptions{
		Items:      wdt,
		DataSource: ds,
	})
	if err != nil {
		log.Error("unable to resolve widgetDataTemplate", slog.Any("err", err))
		return src, err
	}
	log.Debug("widgetDataTemplate JQ evaluation results", slog.Any("evals", evals))

	for _, el := range evals {
		fields := maps.ParsePath(el.Path)
		if len(fields) == 0 {
			continue
		}

		log.Debug("widgetDataTemplate setting nested value",
			slog.Any("fields", fields),
			slog.String("path", el.Path),
			slog.Any("value", el.Value),
			slog.Any("type", reflect.TypeOf(el.Value)),
		)

		err = maps.SetNestedValue(src, fields, el.Value)
		if err != nil {
			log.Error("unable to set nested value",
				slog.Any("fields", fields),
				slog.Any("value", el.Value),
				slog.Any("valueType", reflect.TypeOf(el.Value)),
				slog.Any("err", err))
			return src, err
		}
	}

	return src, nil
}

func resolveResourceRefs(ctx context.Context, obj *Widget, ds map[string]any) ([]v1.ResourceRefResult, error) {
	log := xcontext.Logger(ctx)

	all := []v1.ResourceRef{}

	resrefs, err := GetResourcesRefs(obj.Object)
	if err != nil {
		log.Warn("unable to get resources references", slog.Any("err", err))
	} else {
		all = append(all, resrefs...)
	}

	resrefstpl, err := GetResourcesRefsTemplate(obj.Object)
	if err != nil {
		log.Warn("unable to get resource references template", slog.Any("err", err))
	}
	if len(resrefstpl) > 0 {
		resrefsExtra, err := resourcesrefstemplate.Resolve(ctx, resrefstpl, ds)
		if err != nil {
			return nil, err
		} else {
			all = append(all, resrefsExtra...)
		}
	}

	return resourcesrefs.Resolve(ctx, all)
}

// injectSlice writes `ds["slice"] = {page, perPage, offset}` IFF
// the request carried pagination (`perPage > 0 && page > 0`) AND
// `ds` does not already contain a `slice` entry.
//
// Mechanism-uniform per feedback_no_special_cases: no widget-name
// table, no GVR allowlist. The shape mirrors the triple computed at
// internal/resolvers/restactions/api/resolve.go:211-215; this hop
// merely propagates that triple through the RA-filter projection
// (which would otherwise strip it — see the function's call site
// for the TRACED defect chain).
//
// Branch A per design §3: pre-existing `slice` is preserved so an
// RA author who deliberately emits `.slice` via spec.Filter wins.
func injectSlice(ds map[string]any, perPage, page int) {
	if ds == nil {
		return
	}
	if perPage <= 0 || page <= 0 {
		return
	}
	if _, present := ds["slice"]; present {
		return
	}
	ds["slice"] = map[string]any{
		"page":    page,
		"perPage": perPage,
		"offset":  (page - 1) * perPage,
	}
}

// sanitizeUndeclaredIdentityExtras returns the request extras with
// identity-dimension keys the widget has NOT declared removed. See the A-3 block
// in Resolve for the full rationale.
//
// identityDimensionKeys is the closed set the frontend can put on the wire as
// caller identity: buildExtrasParam sends username + displayName when
// SNOWPLOW_IDENTITY_INJECTION is off, and groups is the other identityContext
// enum value. It is a fixed, mechanism-level identity vocabulary — not a
// widget-name/route table (feedback_no_special_cases) — and it is the same axis
// DeclaredIdentity honours.
//
// Returns the input map UNCHANGED (no copy) when nothing needs stripping — the
// overwhelmingly common path, so the identity-free and fully-declared corpora
// pay one map walk and no allocation.
var identityDimensionKeys = map[string]struct{}{
	"username":    {},
	"groups":      {},
	"displayName": {},
}

func sanitizeUndeclaredIdentityExtras(cr map[string]any, requestExtras map[string]any) map[string]any {
	if len(requestExtras) == 0 {
		return requestExtras
	}
	// authoritative reports whether the widget has declared this key on either
	// axis, i.e. whether its value is server-controlled (identityContext) or
	// self-quarantining (keyExtras).
	authoritative := func(k string) bool {
		for _, d := range GetIdentityContext(cr) {
			if d == k {
				return true
			}
		}
		for _, d := range GetKeyExtras(cr) {
			if d == k {
				return true
			}
		}
		return false
	}
	strip := false
	for k := range requestExtras {
		if _, isIdentity := identityDimensionKeys[k]; isIdentity && !authoritative(k) {
			strip = true
			break
		}
	}
	if !strip {
		return requestExtras
	}
	out := make(map[string]any, len(requestExtras))
	for k, v := range requestExtras {
		if _, isIdentity := identityDimensionKeys[k]; isIdentity && !authoritative(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// mergeExtras folds the per-request extras into the resolved data source
// `ds`, NON-OVERWRITING: extras is the base; any key already present in
// `ds` (an apiRef-RA result key, or the injectSlice pagination triple)
// WINS on collision. This is the widget-side analogue of the RESTAction
// precedence at internal/resolvers/restactions/api/resolve.go:228-230,
// where the resolve dict STARTS as maps.DeepCopyJSON(opts.Extras) and the
// API results then overwrite it — same net ordering (API/apiRef result >
// extras), reached from the opposite direction because here `ds` already
// holds the apiRef result.
//
// WHY it lives here (not only in step 1's apiref thread): widgetDataTemplate
// (resolveWidgetData) AND resourcesRefsTemplate (resolveResourceRefs) both
// evaluate their jq against this SAME `ds`, so one merge makes extras
// referenceable in BOTH template paths. It is ALSO the only thing that puts
// extras into `ds` for a widget with NO apiRef (step 1 never runs there, so
// `ds` comes back as the empty apiref result) — covering apiRef-less / static
// widgets.
//
// Each value is deep-copied via plumbing/maps.DeepCopyJSON (the same util the
// RESTAction path uses) so `ds` — a per-call map — never aliases the
// per-request extras map. nil/empty extras is a no-op (the len-guard skips
// the copy + range), so a widget resolved without an extras param is
// byte-identical to pre-change behaviour. nil `ds` is a no-op (defensive,
// symmetric with injectSlice).
//
// Mechanism-uniform per feedback_no_special_cases: no widget-name table, no
// key allowlist — every extras key is merged the same way for every widget.
func mergeExtras(ds map[string]any, extras map[string]any) {
	if ds == nil || len(extras) == 0 {
		return
	}
	extrasCopy := maps.DeepCopyJSON(extras)
	for k, v := range extrasCopy {
		if _, present := ds[k]; !present {
			ds[k] = v
		}
	}
}

// mergeRequestWins folds the inline (author-declared) extras UNDER the request
// (route/query/login) extras, REQUEST WINNING on every key collision, and
// returns a FRESH map. This is the inline-extras design P per-surface
// precedence (§4): the route is the more-specific intent, so a request param
// MUST override an inline default (an inline default shadowing a route param
// would strand a detail page on the default).
//
// It is the INVERSE direction of mergeExtras (which is non-overwriting / ds-
// wins) and is used at the EARLIER merge — before the effective map ever
// reaches mergeExtras / the apiRef RA dict. Keeping the two helpers separate
// is deliberate (design §4): request-wins-over-inline is achieved HERE;
// apiRef-result-wins-over-everything stays in mergeExtras's ds-wins fold. Do
// NOT collapse them — flipping mergeExtras's direction would also flip the
// apiRef-result-vs-extras precedence and break falsifier #3.
//
// Both inputs are deep-copied into the result via maps.DeepCopyJSON (the same
// util mergeExtras + the RESTAction dict seed use), so the returned map never
// aliases the per-request extras map NOR the inline map read off the shared
// widget CR — no shared-vs-copy concurrency hazard
// (feedback_shared_vs_copy_is_a_concurrency_change). Both empty ⇒ a fresh
// empty map (a no-op effective fold downstream — backward-compat).
//
// Mechanism-uniform (feedback_no_special_cases): every key is folded the same
// way for every widget — no widget-name table, no key allowlist.
func mergeRequestWins(inline, request map[string]any) map[string]any {
	out := make(map[string]any, len(inline)+len(request))
	for k, v := range maps.DeepCopyJSON(inline) {
		out[k] = v
	}
	// Request overwrites any colliding inline key (request wins).
	for k, v := range maps.DeepCopyJSON(request) {
		out[k] = v
	}
	return out
}
