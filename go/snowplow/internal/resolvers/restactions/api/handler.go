package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/ptr"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	jqsupport "github.com/krateo-platformops/snowplow/internal/support/jq"
)

// jsonHandlerOptions plumbs per-stage knobs into jsonHandlerCore.
//
// Ship 0.30.235 — `uaf` and `apiCallName` plumb the UserAccessFilter
// spec into jsonHandlerCore so the refilter runs on the RAW envelope
// BEFORE the stage filter applies its projection. The pre-0.30.235
// post-g.Wait() applyUserAccessFilter call ran on the projected shape,
// which broke NamespaceFrom for any filter that dropped .metadata. See
// refilter_layering_test.go for the permanent regression gate.
//
// Ship K / 0.30.245 — `dict` plumbs the resolver's accumulated
// stage-output dict so resolveUAFResources can evaluate
// `userAccessFilter.resourcesFrom` against UPSTREAM stage outputs
// (e.g. `.crds` populated by a prior stage). Pre-Ship-K passed `pig`
// (the per-stage scope) to resolveUAFResources, which never contains
// upstream stage keys → ResourcesFrom always returned []  → fail-closed
// drop every item → 0-count piechart regression on the live
// compositions-list RA. Permanent regression gate at
// refilter_layering_test.go::TestUAFRefilter_ResourcesFromMultiStage_ResolvesAgainstDictNotPig.
// The dict reference is the SAME map jsonHandlerCore writes to as
// `out`; refilter reads it BEFORE the merge, so it sees upstream
// stages but NOT the current stage being filtered.
type jsonHandlerOptions struct {
	key         string
	out         map[string]any
	dict        map[string]any
	filter      *string
	uaf         *templates.UserAccessFilterSpec
	apiCallName string
	// extras is the PURE per-request extras map (r.opts.Extras), exposed to
	// the per-stage filter as the reserved sibling key pig["extras"] so the
	// step filter can read `.extras.*` like every other jq surface. This is
	// NOT opts.dict: dict starts as a DeepCopy of Extras (resolve.go) but then
	// accumulates stage outputs + a synthetic `slice`, so at later stages it
	// is no longer the pure extras. The reference is SHARED, not deep-copied:
	// gojq is the krateo-platformops COW fork, so the filter cannot mutate the input
	// (mirrors the existing pig["slice"] shared-map pattern).
	extras map[string]any
}

// jsonHandler is the HTTP-body-shaped entry point — the form
// httpcall.Do's ResponseHandler contract requires. It io.ReadAll's the
// response body, json.Unmarshal's it, and hands the decoded value to
// jsonHandlerCore. Use this ONLY for a genuine HTTP-body stream
// (httpcall.Do); for an in-memory dispatch result use jsonHandlerBytes
// (raw []byte) or jsonHandlerValue (already-decoded value) — Ship
// 0.30.128 P-CORE-1: those skip the redundant io.ReadAll copy that every
// cache hit otherwise pays.
func jsonHandler(ctx context.Context, opts jsonHandlerOptions) func(io.ReadCloser) error {
	return func(in io.ReadCloser) error {
		dat, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		return jsonHandlerBytesApply(ctx, opts, dat)
	}
}

// jsonHandlerBytes is the []byte-direct entry point — Ship 0.30.128
// P-CORE-1. For an in-memory dispatch result (informer-served bytes, an
// in-process nested /call's Status.Raw, the internal-rest-config
// dispatch) the bytes are ALREADY in memory; wrapping them in an
// io.ReadCloser only so jsonHandler can io.ReadAll them straight back
// out is a redundant full copy paid on every call. jsonHandlerBytes
// json.Unmarshal's the bytes directly and skips that copy.
func jsonHandlerBytes(ctx context.Context, opts jsonHandlerOptions) func([]byte) error {
	return func(dat []byte) error {
		return jsonHandlerBytesApply(ctx, opts, dat)
	}
}

// jsonHandlerValue is the decoded-value-direct entry point — Ship
// 0.30.128 P-CORE-2. For an apistage content-cache hit the gated
// envelope is already a structured value (map[string]any); marshalling
// it to []byte only for jsonHandler to unmarshal it right back is a
// redundant decode paid on every cache hit. jsonHandlerValue feeds the
// already-decoded value straight to jsonHandlerCore — no marshal, no
// unmarshal.
func jsonHandlerValue(ctx context.Context, opts jsonHandlerOptions) func(any) error {
	return func(decoded any) error {
		return jsonHandlerCore(ctx, opts, decoded)
	}
}

// jsonHandlerBytesApply json.Unmarshal's dat then runs jsonHandlerCore.
// The shared body behind jsonHandler (HTTP path) and jsonHandlerBytes
// (in-memory []byte path).
func jsonHandlerBytesApply(ctx context.Context, opts jsonHandlerOptions, dat []byte) error {
	var tmp any
	if err := json.Unmarshal(dat, &tmp); err != nil {
		return err
	}
	return jsonHandlerCore(ctx, opts, tmp)
}

// jsonHandlerCore is the post-decode logic: wrap the decoded value under
// opts.key, apply the optional stage filter, and merge into opts.out.
// Byte-identical behaviour to the pre-0.30.128 jsonHandler from the
// decoded value onward (AC-128.4) — P-CORE-1/2 only change WHEN/WHERE
// the decode happens (once at entry-load, not once per cache-hit), never
// the merge/filter result.
func jsonHandlerCore(ctx context.Context, opts jsonHandlerOptions, tmp any) error {
	log := xcontext.Logger(ctx)

	pig := map[string]any{}
	// pig["extras"] is written BEFORE pig[opts.key] so a stage literally named
	// "extras" keeps its OWN response (response wins; collision Option A). The
	// guard mirrors the `slice` guard below: write nothing when extras is
	// absent so the no-extras envelope stays byte-identical to pre-feature.
	// The map is SHARED, not copied — gojq's braghettos COW fork can't mutate
	// the filter input (same contract as pig["slice"]).
	if len(opts.extras) > 0 {
		pig["extras"] = opts.extras
	}
	pig[opts.key] = tmp
	if si, ok := opts.out["slice"]; ok {
		pig["slice"] = si
	}

	// Ship 0.30.235 — UAF refilter runs on the RAW envelope BEFORE the
	// stage filter projects items. The projection can omit .metadata;
	// running UAF first uses the K8s-canonical shape the spec documents.
	// nil-UAF is a single early-return — pre-0.30.235 byte-identical.
	//
	// Ship K / 0.30.245 — opts.dict is threaded into the refilter so
	// resolveUAFResources evaluates `resourcesFrom` against the FULL
	// resolver dict (upstream stage outputs), not the per-stage pig
	// scope. Fixes the 0-count compositions-list regression preserved
	// from 0.30.235.
	if opts.uaf != nil {
		rf := applyUserAccessFilterOnPig(ctx, pig, opts.dict, opts.key, opts.uaf)
		emitRefilterFalsifierFromHandler(ctx, log, opts.apiCallName, opts.uaf, rf)
	}

	if opts.filter != nil {
		q := ptr.Deref(opts.filter, "")
		log.Debug("found local filter on api result", slog.String("filter", q))
		// Ship A (0.30.137): EvalValue returns gojq's result value
		// directly — no jqutil encode-to-string + json.Unmarshal-back
		// round-trip (design §3.4.2). Behaviour byte-identical per the
		// §3.4.2 truth table.
		v, ok, err := EvalValue(context.TODO(), q, pig, jqsupport.ModuleLoader())
		switch {
		case errors.Is(err, ErrMultiYield):
			// Current: multi-yield -> invalid concatenated JSON ->
			// json.Unmarshal errors -> return err -> stage fails.
			return err
		case err != nil:
			// Parse/compile/runtime gojq-error. Current: log.Error, tmp
			// unchanged, stage continues. (jqutil.Eval err branch.)
			log.Error("unable to evaluate JQ filter",
				slog.String("filter", q), slog.Any("error", err))
		case !ok:
			// Zero-yield (jq `empty`). Current: jqutil.Eval returns "",
			// json.Unmarshal("") errors -> return err -> stage fails.
			return fmt.Errorf("jq filter %q yielded no value", q)
		default:
			// Single value. Current: json.Unmarshal(s) -> tmp. Ship A:
			// tmp = v directly (the §3.1-3.3 equivalence proof).
			tmp = v
		}
	}

	got, ok := opts.out[opts.key]
	if !ok {
		opts.out[opts.key] = tmp
		return nil
	}

	// #74 Class 3 R1' — PROVENANCE-DISCRIMINATED flatten. A bare []any element
	// `tmp` is SPLICED into the accumulator (its members appended element-wise)
	// ONLY when it is FILTER-PRODUCED — `opts.filter != nil`, i.e. the per-stage
	// gojq filter constructed the array (e.g. composition-schema's
	// `getCompositionDefinitionNamespace … | map(.metadata.namespace)`, where
	// element-wise concat is intended). A RAW-RESPONSE []any (`opts.filter ==
	// nil`) is appended as a SINGLE element, never spliced.
	//
	// Why: the dispatch never returns a bare []any for a no-filter step — a
	// GET-by-name is an object, a LIST GET is the {items} envelope OBJECT
	// (listEnvelopeValue, apistage.go), discovery is an APIResourceList OBJECT.
	// A no-filter bare []any arises ONLY when a resolve:true nested element
	// resolves to an RA/widget whose top-level value is an array — the Class 3
	// `allCompositionResources` self/nested resolve returning ["v1", …].
	// Splicing THAT poisons the parent list with bare scalars ("v1"), which the
	// downstream portal filter then indexes as `.metadata` → "expected an object
	// but got: string". Corpus-proven safe (docs/errzero-class3-decision §7.2):
	// getCompositionDefinitionNamespace = 10/10 filter-bearing (still flattens),
	// allCompositionResources = 0/34 (stops splicing), no identity-passthrough
	// filter exists, no no-filter LIST-GET returns a bare []any.
	//
	// STRUCTURAL predicate (feedback_no_special_cases) — keyed on
	// `opts.filter != nil`, never on a resource/name/GVR literal. Global, no
	// per-RA config, no CRD field.
	flatten := opts.filter != nil
	switch existingSlice := got.(type) {
	case []any:
		if arr, isArr := tmp.([]any); isArr {
			if flatten && len(arr) > 0 {
				// (A) filter-produced array → splice members (preserves the
				// pre-R1' len>0 empty-slice skip).
				opts.out[opts.key] = append(existingSlice, arr...)
			} else if !flatten {
				// (B) raw-response array → single element, never spliced.
				opts.out[opts.key] = append(existingSlice, tmp)
			}
			// flatten && len==0: empty filtered array contributes nothing
			// (unchanged from the pre-R1' len(v) > 0 guard).
		} else {
			// scalar/object element → single element (wrapAsSlice's old
			// default branch).
			opts.out[opts.key] = append(existingSlice, tmp)
		}
	default:
		// Second element arriving (existing accumulator value is not yet a
		// slice). Same A/B discrimination as above.
		if arr, isArr := tmp.([]any); isArr && flatten {
			opts.out[opts.key] = append([]any{got}, arr...)
		} else {
			opts.out[opts.key] = []any{got, tmp}
		}
	}

	return nil
}
