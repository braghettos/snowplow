// apistage.go — Ship E (0.30.116): per-api-stage L1 key-swap.
//
// The RESTAction resolver runs a dependsOn-sorted chain of stages, each
// dispatching K8s call(s) and writing dict[id]. Pre-Ship-E the whole
// RESTAction output is the single L1 entry — so re-resolving the SAME
// RESTAction (a second /call, a refresh, a different page) re-dispatches
// every stage's K8s call from scratch. Ship E inserts a per-stage
// key-swap: before a stage dispatches, look it up in the resolved-output
// L1 under a stage key; a hit serves dict[id] from L1 and skips the K8s
// call.
//
// SCOPE — within-RESTAction stage reuse. The stage key folds in the
// owning RESTAction's identity ({group,version,resource,namespace,name})
// AND the author-chosen stage.Name, so it is scoped to one RESTAction.
// The win is: the same RESTAction re-resolved (2nd /call, refresh,
// pagination, another request by the same identity) reuses its own
// stage entries. It deliberately does NOT dedup a stage across DIFFERENT
// RESTActions — the discriminator keys on stage.Name, not the rendered
// K8s call signature, so two RESTActions whose stages happen to share a
// name but render different calls must NOT collide. Cross-RESTAction
// stage sharing is a possible future ship IF the stage key hashes the
// full rendered call signature (path+verb+headers+endpoint) instead —
// out of Ship E scope.
//
// The api-stage entry is just a third granularity of cache.ResolvedKeyInputs
// (HandlerKind=="apistage", Stage set) on the SAME ResolvedCacheStore —
// no parallel cache. The live Ship-A dep-tracker + Ship-C refresher
// handle it with no new code: resolving the stage under
// WithL1KeyContext(stageKey) makes the existing inner-call dep-recording
// attribute the stage's LIST/GET to the stage key (O4).
//
// EVERYTHING here is gated by cache.ApistageL1Enabled() — default OFF.
// Flag-off, none of this runs and the resolver is byte-identical to
// 0.30.115 (AC-E1).

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// restActionGVR is the FIXED GroupVersionResource of a RESTAction CR.
// A RESTAction is always templates.krateo.io/v1 restactions — this is
// not a per-resource special-case, it is the single type whose stages
// the api-stage key-swap applies to (the resolver only ever resolves
// RESTAction stages). The api-stage key folds it in so a stage id is
// scoped to its owning RESTAction's GVR.
var restActionGVR = struct{ Group, Version, Resource string }{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// canonicalizeFilter implements the O5 canonical filter-hash input. The
// stage `filter` jq string is Helm-rendered and whitespace-variable —
// two deployments of the same chart can render the same filter with
// different interior whitespace / indentation. Canonicalize so the
// stage key is stable across that render variation (AC-E5):
//
//   - trim leading/trailing whitespace;
//   - collapse every run of interior whitespace (spaces, tabs,
//     newlines) to a single space.
//
// We canonicalize the POST-RENDER string captured at resolve time (the
// architect's preferred, deterministic choice) — not the pre-render
// template. An empty / nil filter canonicalizes to "".
func canonicalizeFilter(filter string) string {
	return strings.Join(strings.Fields(filter), " ")
}

// stageInputHash hashes the stage's effective dict input — the subset of
// the resolver `dict` the stage actually reads. A stage declares its
// dependency via dependsOn.Name; that predecessor's output
// (dict[dependsOn.Name]) is the input that, when changed, changes the
// stage's result. Pagination (dict["slice"]) is also folded in because
// an iterator stage's per-page offset is part of its effective input.
//
// When the stage has no dependsOn, only the pagination slice (if any)
// contributes — a root stage's input is just its own static path.
//
// The hash is over a deterministic JSON encoding; a marshal failure
// (cyclic / non-JSON value — not expected for resolver dict values)
// falls back to the empty hash, which is conservative: it makes the
// stage key less specific, never wrong (a stale key just misses).
func stageInputHash(dict map[string]any, dep *templates.Dependency) string {
	input := map[string]any{}
	if dep != nil && dep.Name != "" {
		if v, ok := dict[dep.Name]; ok {
			input["dep"] = v
		}
		if dep.Iterator != nil {
			input["iter"] = *dep.Iterator
		}
	}
	if slice, ok := dict["slice"]; ok {
		input["slice"] = slice
	}
	if len(input) == 0 {
		return ""
	}
	buf, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// stageDiscriminator builds the ResolvedKeyInputs.Stage string for one
// RESTAction stage: the stage id, the O5 canonical filter-hash, and the
// effective-dict-input hash. ComputeKey folds this — together with the
// RESTAction GVR/namespace/name and the identity (Username+Groups) — into
// the final stage key. The format is fixed-field, '\x1f'-separated so two
// distinct (id, filter, input) triples can never alias.
func stageDiscriminator(stage *templates.API, dict map[string]any) string {
	filterHash := ""
	if stage.Filter != nil {
		canon := canonicalizeFilter(*stage.Filter)
		if canon != "" {
			sum := sha256.Sum256([]byte(canon))
			filterHash = hex.EncodeToString(sum[:])
		}
	}
	inputHash := stageInputHash(dict, stage.DependsOn)
	return strings.Join([]string{"stage", stage.Name, filterHash, inputHash}, "\x1f")
}

// stageKeyInputs assembles the cache.ResolvedKeyInputs for one
// RESTAction stage. Identity is Username+Groups — per-user keyed, NEVER
// cohort (feedback_l1_per_user_keyed_never_cohort, AC-E4): two distinct
// identities resolving the same stage get distinct keys, so an RBAC
// cross-user leak is structurally impossible. The GVR/namespace/name are
// the OWNING RESTAction's — so the api-stage entry's Inputs identify it
// as a stage of that RESTAction, and the refresher's resolve-once seam
// can re-fetch the RESTAction and re-run the single stage.
func stageKeyInputs(raNamespace, raName, username string, groups []string, stage *templates.API, dict map[string]any, perPage, page int) cache.ResolvedKeyInputs {
	return cache.ResolvedKeyInputs{
		HandlerKind: cache.HandlerKindApistage,
		Group:       restActionGVR.Group,
		Version:     restActionGVR.Version,
		Resource:    restActionGVR.Resource,
		Namespace:   raNamespace,
		Name:        raName,
		Username:    username,
		Groups:      groups,
		PerPage:     perPage,
		Page:        page,
		Stage:       stageDiscriminator(stage, dict),
	}
}

// apistageHitValue is the JSON shape an api-stage L1 entry stores: the
// single stage's output value (what the stage writes to dict[id]). On a
// hit the resolver decodes this and assigns it back to dict[id].
type apistageHitValue struct {
	Value any `json:"value"`
}

// encodeStageValue marshals a stage's dict[id] output for storage in the
// api-stage L1 entry. Wrapping in apistageHitValue keeps the top-level
// shape an object regardless of the stage value's type (slice, map,
// scalar) so decode is uniform.
func encodeStageValue(v any) ([]byte, error) {
	return json.Marshal(apistageHitValue{Value: v})
}

// decodeStageValue unmarshals an api-stage L1 entry's bytes back to the
// stage output value.
func decodeStageValue(raw []byte) (any, bool) {
	var hv apistageHitValue
	if err := json.Unmarshal(raw, &hv); err != nil {
		return nil, false
	}
	return hv.Value, true
}
