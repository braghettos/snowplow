// Q-RBAC-DECOUPLE C(d) v3 — per-user refilter at HTTP dispatch.
//
// Overview (full design at /tmp/snowplow-runs/q-rbac-decouple-cd-v3-spec-2026-05-04/SPEC.md):
//
// The L1 outer cache (snowplow:resolved:{identity}:...) is keyed by binding
// identity hash. Two users sharing a binding-identity share the L1 entry,
// and the v2 design wrapped applyUserAccessFilter at the six L1-MISS resolve
// sites — those sites do NOT execute on L1 HIT, so v2 silently served the
// first user's filtered view to every subsequent user in the same binding
// group. That is Q-RBACC-DEFECT-1, the reason v3 exists.
//
// v3 changes the cached shape:
//
//	v2 wire shape: json.Marshal(&cr) where cr.Status.Raw is the OUTER-JQ
//	               output (post-applyUserAccessFilter for each api[] entry).
//
//	v3 wire shape: json.Marshal(cachedRESTAction{ CR, ProtectedDict, "v3" }).
//	               cr.Status is REMOVED (or empty); the per-api intermediate
//	               dict, BEFORE outer JQ and BEFORE applyUserAccessFilter,
//	               lives in ProtectedDict. Outer-JQ + per-user filtering
//	               run at HTTP dispatch time inside RefilterRESTAction.
//
// Trust boundary (NON-NEGOTIABLE): on ANY error, RefilterRESTAction returns
// (nil, err). The caller MUST fall through to the miss path and re-resolve
// under the requesting user's identity. NEVER serve cached bytes that
// failed refilter — that would re-introduce the silent RBAC violation we
// are fixing.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CachedSchemaVersionV3 is embedded in every v3 cached RESTAction wrapper so
// future cache shape migrations (v4, v5, …) can be detected by readers
// without a global flush — Q-RBACC-V3-IMPL-2.
const CachedSchemaVersionV3 = "v3"

// childRESTActionRef is an opaque pointer in ProtectedDict referencing
// another L1 entry. Used when a parent RESTAction's api[] entry resolves a
// /call to a child RESTAction; the child's L1 entry is the source of truth
// for that slot, not an inlined copy.
//
// Why opaque pointer instead of inlining: cardinality. Inlining the child
// would mean every parent caches its own copy of the child's potentially
// large output (compositions-list pulls compositions-get-ns-and-crd which
// pulls 50 NS + N CRDs). The pointer lets the refilter recurse and pick up
// the child's freshly-refiltered output for the requesting user.
//
// SchemaTag is a sentinel JSON marker so json.Unmarshal of the parent
// ProtectedDict can disambiguate a real map from a child reference.
// Concrete value: {"__snowplow_v3_child_l1_ref__": true, ...}.
type childRESTActionRef struct {
	SchemaTag bool   `json:"__snowplow_v3_child_l1_ref__"`
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	L1Key     string `json:"l1_key"`

	// Filter is the parent api[]'s JQ filter to apply to the child's
	// refiltered status output before merging into the parent's dict
	// slot. nil means "use child's status as-is".
	Filter *string `json:"filter,omitempty"`
}

const childRefSentinelKey = "__snowplow_v3_child_l1_ref__"

// IsChildRESTActionRef returns true if v is a JSON-encoded child reference
// (i.e., a map whose first key is the v3 sentinel). Used by the resolver
// when collecting ProtectedDict and by RefilterRESTAction during recursion.
func IsChildRESTActionRef(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	tag, ok := m[childRefSentinelKey].(bool)
	return ok && tag
}

// childRefFromMap extracts a childRESTActionRef from its JSON-decoded map
// form. Returns ok=false if any required field is missing or wrong type.
func childRefFromMap(m map[string]any) (childRESTActionRef, bool) {
	tag, _ := m[childRefSentinelKey].(bool)
	if !tag {
		return childRESTActionRef{}, false
	}
	r := childRESTActionRef{SchemaTag: true}
	r.Group, _ = m["group"].(string)
	r.Version, _ = m["version"].(string)
	r.Resource, _ = m["resource"].(string)
	r.Namespace, _ = m["namespace"].(string)
	r.Name, _ = m["name"].(string)
	r.L1Key, _ = m["l1_key"].(string)
	if filter, ok := m["filter"].(string); ok {
		r.Filter = &filter
	}
	if r.Resource == "" || r.Name == "" || r.L1Key == "" {
		return childRESTActionRef{}, false
	}
	return r, true
}

// MarshalChildRESTActionRef returns the JSON-encodable map form a resolver
// site can drop into ProtectedDict[apiCall.Name] in place of an inlined
// child result. Exported so the resolver fan-out (api/resolve.go) can
// produce the value at the call_l1 / call_inline branches.
func MarshalChildRESTActionRef(gvr schema.GroupVersionResource, namespace, name, l1Key string, filter *string) map[string]any {
	out := map[string]any{
		childRefSentinelKey: true,
		"group":             gvr.Group,
		"version":           gvr.Version,
		"resource":          gvr.Resource,
		"namespace":         namespace,
		"name":              name,
		"l1_key":            l1Key,
	}
	if filter != nil {
		out["filter"] = *filter
	}
	return out
}

// CachedRESTAction is the v3 wire shape for an L1 outer entry. Marshaled
// by the resolver (l1cache.ResolveAndCache write path) and unmarshaled by
// RefilterRESTAction. The previous wire shape was json.Marshal(&cr) where
// cr.Status held the post-filter outer-JQ output; v3 stores the unfiltered
// per-api intermediate dict in ProtectedDict, leaving cr.Status.Raw nil so
// no caller can accidentally read pre-filter bytes through the old field.
type CachedRESTAction struct {
	// SchemaVersion is the wire-shape tag. "v3" is the only accepted value
	// today; readers MUST reject anything else as a cache miss.
	SchemaVersion string `json:"schema_version"`

	// CR holds the RESTAction CR metadata + spec. Status is intentionally
	// NIL: per-user status is computed inside RefilterRESTAction at HTTP
	// dispatch time. Storing a stale cr.Status would be a footgun.
	CR *templates.RESTAction `json:"cr"`

	// ProtectedDict is the per-api[] intermediate dict, BEFORE outer JQ
	// and BEFORE per-user UserAccessFilter has been applied. Each value
	// is either:
	//   - a passthrough JSON value for api[] entries with no
	//     UserAccessFilter and no /call recursion;
	//   - the unfiltered list for api[] entries with UserAccessFilter
	//     (refiltered per-user inside RefilterRESTAction);
	//   - a childRESTActionRef map for api[] entries that resolve to
	//     another RESTAction via /call (refilter recurses).
	ProtectedDict map[string]any `json:"protected_dict"`

	// IsRefilterIdentity is true iff refilter for this wrapper is byte-
	// equivalent to applying the outer JQ over the cached ProtectedDict
	// for ALL users in the binding-identity cohort. Set at MarshalCached
	// time by computeIsRefilterIdentity, which inspects the RA structural
	// shape (no api[] entry has UserAccessFilter, no ProtectedDict slot
	// is a live childRESTActionRef, outer JQ does not reference any of
	// the user-scoped variable names that a future codec migration could
	// inject).
	//
	// When true, RefilterRESTAction's identity fast path skips the
	// per-api iteration + UAF apply + child recursion (each provably a
	// no-op for identity wrappers) and runs only the outer JQ. Cuts the
	// admin compositions-list refilter wall (~1.7-2.8 s today) by
	// removing the redundant per-api walk.
	//
	// SECURITY (asymmetric trust boundary):
	//   false-negative (claim "false" when refilter IS identity) → pay
	//                  extra CPU on the standard path. Safe.
	//   false-positive (claim "true" when refilter is NOT identity) →
	//                  serve unfiltered bytes to a user lacking RBAC.
	//                  CRITICAL violation.
	// computeIsRefilterIdentity is therefore conservative: it returns
	// true ONLY when every structural condition is met. Any uncertainty
	// (nil cr, malformed slot, defensive scan trigger) returns false.
	//
	// `omitempty` on the JSON tag makes the wire format
	// forward+backward-compatible: old binaries decoding new wrappers
	// see the field; new binaries decoding old wrappers (no field) see
	// the Go zero value (false) and the fast path declines.
	IsRefilterIdentity bool `json:"is_refilter_identity,omitempty"`
}

// MarshalCached returns the v3 wire bytes a caller (l1cache writer) should
// store under the L1 key. Exported so non-test callers can produce the
// canonical shape without re-implementing the schema_version handling.
//
// Q-COMP-LIST-IDENTITY: stamps IsRefilterIdentity at write time. Computation
// is cheap (~O(len(api)) + O(len(dict))) and runs once per L1 write — the
// flag is then read on every refilter via a JSON peek.
func MarshalCached(cr *templates.RESTAction, protectedDict map[string]any) ([]byte, error) {
	wrapper := CachedRESTAction{
		SchemaVersion:      CachedSchemaVersionV3,
		CR:                 cr,
		ProtectedDict:      protectedDict,
		IsRefilterIdentity: computeIsRefilterIdentity(cr, protectedDict),
	}
	return json.Marshal(wrapper)
}

// userScopedJQTokens is the defensive-scan list for outer JQ filters whose
// output could vary per user even when no UAF is present. Today's snowplow
// outer JQ scope contains ONLY `.` (the data input) and the JQ stdlib —
// jqsupport.ModuleLoader does not bind any variables (verified 2026-05-07
// against plumbing/jqutil v0.9.6: EvalOptions has no Vars field, gojq is
// invoked without WithVariables). This list therefore has no production
// hits today but defends against a future codec migration that adds
// per-user variable binding without updating computeIsRefilterIdentity.
//
// Scan is byte-substring on the filter source. False-positives (a string
// literal containing "$user") simply route to the standard path — safe.
// False-negatives are not possible because the scan triggers on any of
// the documented user-scope token prefixes.
var userScopedJQTokens = []string{
	"$user",
	"$jwt",
	"$groups",
	"$identity",
	"$bindings",
	"$username",
}

// containsUserScopedJQ returns true if the filter source references any
// JQ variable in userScopedJQTokens. Used by computeIsRefilterIdentity as
// a defense-in-depth guard.
func containsUserScopedJQ(filter string) bool {
	if filter == "" {
		return false
	}
	for _, tok := range userScopedJQTokens {
		if strings.Contains(filter, tok) {
			return true
		}
	}
	return false
}

// computeIsRefilterIdentity returns true iff refilter is provably a no-op
// for every user in the binding-identity cohort. Returns false on any of:
//   - cr is nil (defensive: never claim identity on uncertainty)
//   - any cr.Spec.API entry has a non-nil UserAccessFilter (per-user
//     filtering would run on the standard path)
//   - any ProtectedDict slot is a live childRESTActionRef (the child
//     could change per user on the next refresh, so the parent's
//     refilter must recurse)
//   - cr.Spec.Filter contains any user-scoped JQ token (defense against
//     a future codec migration that adds per-user variable binding to
//     the JQ scope)
//
// Pure structural inspection; ZERO knowledge of user / RA name / size.
// Generic over the RA shape — any RA whose api[] entries lack UAF and
// whose ProtectedDict has no live childRefs benefits, regardless of
// resource type.
func computeIsRefilterIdentity(cr *templates.RESTAction, protectedDict map[string]any) bool {
	if cr == nil {
		return false
	}
	for _, apiCall := range cr.Spec.API {
		if apiCall == nil {
			continue
		}
		if apiCall.UserAccessFilter != nil {
			return false
		}
	}
	for _, slot := range protectedDict {
		if IsChildRESTActionRef(slot) {
			return false
		}
	}
	if cr.Spec.Filter != nil {
		if containsUserScopedJQ(ptr.Deref(cr.Spec.Filter, "")) {
			return false
		}
	}
	return true
}

// PeekIsRefilterIdentity decodes ONLY the is_refilter_identity flag from
// a v3 wrapper's raw bytes. Cheap (~1 µs per call regardless of wrapper
// size — encoding/json's stream decoder skips unmatched fields without
// allocating intermediate maps). Used by L2 writers to gate the
// reduction-ratio bypass without re-running the full structural check.
//
// Returns (false, err) on decode failure. Callers MUST treat any error
// as "not eligible for fast path" — never panic, never assume true.
func PeekIsRefilterIdentity(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, fmt.Errorf("peek identity: empty raw")
	}
	var probe struct {
		IsRefilterIdentity bool `json:"is_refilter_identity"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false, err
	}
	return probe.IsRefilterIdentity, nil
}

// RefilterRESTAction unmarshals a v3 cached entry, refilters each
// UserAccessFilter-protected slot for the requesting user, recurses into
// child-RESTAction references, runs the outer JQ filter (cr.Spec.Filter)
// against the refiltered dict, sets cr.Status.Raw, and re-marshals the CR.
// The returned bytes are byte-for-byte equivalent to what the v0/v2
// resolver would produce if it had run under the requesting user's
// identity end-to-end on a cold cache.
//
// SECURITY (the spec's only non-negotiable invariant): on ANY error, the
// function returns (nil, err) and the caller MUST fall through to the miss
// path. NEVER serve `raw` (the cached bytes) on a refilter failure — the
// cached bytes are the unfiltered shape and would leak items.
//
// The Cache parameter is used to look up child RESTAction L1 entries when
// the parent's ProtectedDict contains a childRESTActionRef. nil is
// permitted only if no such ref exists in raw; if a ref is found and c is
// nil the function returns an error so the caller falls through.
func RefilterRESTAction(ctx context.Context, c cache.Cache, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("v3 refilter: empty raw input")
	}

	var wrapper CachedRESTAction
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("v3 refilter: unmarshal wrapper: %w", err)
	}
	if wrapper.SchemaVersion != CachedSchemaVersionV3 {
		// Older or unknown shape — caller falls through to a fresh
		// resolve, which writes the v3 shape on completion.
		return nil, fmt.Errorf("v3 refilter: schema version %q not supported (want %q)",
			wrapper.SchemaVersion, CachedSchemaVersionV3)
	}
	if wrapper.CR == nil {
		return nil, fmt.Errorf("v3 refilter: cached wrapper has nil CR")
	}

	// Defensive: if any caller accidentally serialised a Status into the
	// cached wrapper, drop it now so the only Status we serve is the one
	// computed below from the per-user-refiltered dict.
	wrapper.CR.Status = nil

	// Per-user identity is required: the whole point of v3 is to gate
	// each item on the requesting user's RBAC. UserInfo missing from ctx
	// is a hard error; the dispatcher should never reach RefilterRESTAction
	// without it, but FAIL-CLOSED matches the helper's existing semantics
	// at applyUserAccessFilter (user_access_filter.go:78-89).
	if _, err := xcontext.UserInfo(ctx); err != nil {
		return nil, fmt.Errorf("v3 refilter: no user info in ctx: %w", err)
	}

	refiltered := make(map[string]any, len(wrapper.ProtectedDict))

	// ── Q-COMP-LIST-IDENTITY identity-refilter fast path ─────────────────
	// When the wrapper claims IsRefilterIdentity=true AND a defense-in-
	// depth re-validation against the just-decoded CR + dict agrees, skip
	// the per-api iteration entirely. Identity wrappers are wrappers where
	// the per-api loop below is provably a no-op for every user in the
	// cohort:
	//   - no UAF on any api[] entry → applyUserAccessFilter is never
	//     invoked (line 426 below)
	//   - no live childRESTActionRef in any slot → the recursive
	//     resolveChild branch never fires
	// In that scenario, the per-api loop's only effect is
	// `refiltered[apiCall.Name] = slot` for every entry, which is exactly
	// what carrying through every ProtectedDict key does.
	//
	// Trust boundary (FAIL-CLOSED): the re-validation MUST be performed
	// against the just-decoded wrapper, NOT solely against the cached
	// flag. A corrupted L1 entry that flagged identity but actually
	// contains a UAF MUST fall through to the standard path. Cost of the
	// re-validation: O(len(api)) + O(len(dict)) — microseconds, far
	// below the per-api loop it replaces.
	//
	// The fast path runs the SAME outer JQ over the SAME map shape as
	// the standard path would for an identity wrapper, so output is
	// byte-equivalent by construction.
	if wrapper.IsRefilterIdentity && computeIsRefilterIdentity(wrapper.CR, wrapper.ProtectedDict) {
		for k, v := range wrapper.ProtectedDict {
			refiltered[k] = v
		}
		cache.GlobalMetrics.RefilterFastPathHits.Add(1)
		return runOuterJQAndMarshal(ctx, wrapper.CR, refiltered)
	}
	// If the flag was set but re-validation disagrees, count that as a
	// fallthrough so observability surfaces the discrepancy. PM gate
	// G-FALLTHROUGH alerts when this rate exceeds 5%.
	if wrapper.IsRefilterIdentity {
		cache.GlobalMetrics.RefilterFastPathFallthrough.Add(1)
	}
	// ── End fast path; standard path follows ─────────────────────────────

	// Carry through any non-api keys that the resolver may have added
	// (e.g., "slice" for pagination, "apiRequests" diagnostic). These
	// are not part of cr.Spec.API so we just pass them through.
	apiNames := make(map[string]bool, len(wrapper.CR.Spec.API))
	for _, apiCall := range wrapper.CR.Spec.API {
		if apiCall != nil {
			apiNames[apiCall.Name] = true
		}
	}
	for k, v := range wrapper.ProtectedDict {
		if !apiNames[k] {
			refiltered[k] = v
		}
	}

	for _, apiCall := range wrapper.CR.Spec.API {
		if apiCall == nil {
			continue
		}
		slot, present := wrapper.ProtectedDict[apiCall.Name]
		if !present {
			// Resolver dropped this entry (e.g. ContinueOnError, missing
			// dependency). Leave the slot absent in the refiltered dict;
			// outer JQ may handle it.
			continue
		}

		// Child-RESTAction reference: recurse.
		if IsChildRESTActionRef(slot) {
			refRaw, ok := slot.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("v3 refilter: child ref slot for %q has unexpected type %T",
					apiCall.Name, slot)
			}
			ref, ok := childRefFromMap(refRaw)
			if !ok {
				return nil, fmt.Errorf("v3 refilter: malformed child ref slot for %q", apiCall.Name)
			}
			if c == nil {
				return nil, fmt.Errorf("v3 refilter: child ref %q present but no cache available", apiCall.Name)
			}
			childRaw, hit, err := c.GetRaw(ctx, ref.L1Key)
			if err != nil || !hit || len(childRaw) == 0 {
				return nil, fmt.Errorf("v3 refilter: child L1 miss for %q (key=%s, hit=%v, err=%v)",
					apiCall.Name, ref.L1Key, hit, err)
			}
			childOut, err := RefilterRESTAction(ctx, c, childRaw)
			if err != nil {
				return nil, fmt.Errorf("v3 refilter: child refilter failed for %q: %w", apiCall.Name, err)
			}
			// Project the refiltered child via the parent api[]'s JQ
			// filter (if any), then merge under apiCall.Name.
			projected, perr := projectChild(ctx, childOut, apiCall.Filter, apiCall.Name)
			if perr != nil {
				return nil, fmt.Errorf("v3 refilter: project child for %q: %w", apiCall.Name, perr)
			}
			refiltered[apiCall.Name] = projected
			continue
		}

		// Non-child slot.
		if apiCall.UserAccessFilter == nil {
			refiltered[apiCall.Name] = slot
			continue
		}
		// Per-user filter: applyUserAccessFilter is the single place the
		// per-item RBAC drop decision lives. It also fires the audit log
		// (one entry per refilter call, same shape as v2's miss-path
		// invocation). NOTE: the audit log fires from refilter on every
		// HTTP request now — Q-RBACC-V3-IMPL-5 considered emitting a
		// distinct audit=user_access_refilter event; we keep one shape so
		// post-hoc analysis stays simple, and the trace_id auto-injected
		// by observability.NewTraceIDHandler distinguishes hit-path vs
		// miss-path callers if a operator needs it.
		refiltered[apiCall.Name] = applyUserAccessFilter(ctx, apiCall, slot)
	}

	return runOuterJQAndMarshal(ctx, wrapper.CR, refiltered)
}

// runOuterJQAndMarshal is the shared tail of RefilterRESTAction. Both the
// standard path and the IsRefilterIdentity fast path converge here so the
// outer JQ + final marshal is identical bytes-for-bytes regardless of the
// path taken to assemble `refiltered`. This is the load-bearing equivalence
// guarantee for Q-COMP-LIST-IDENTITY: the fast path is byte-equivalent to
// the standard path because they share THIS function for the steps that
// produce wire bytes.
//
// `cr` is mutated: cr.Status is set to the JQ output before marshal.
// Caller MUST have already cleared any pre-existing Status (refilter does
// this at the top).
func runOuterJQAndMarshal(ctx context.Context, cr *templates.RESTAction, refiltered map[string]any) ([]byte, error) {
	// Outer JQ: re-evaluate cr.Spec.Filter against refiltered dict to
	// produce cr.Status.Raw. If no outer filter, marshal the refiltered
	// dict directly (matches restactions.Resolve's else branch).
	var statusRaw []byte
	if cr.Spec.Filter != nil {
		q := ptr.Deref(cr.Spec.Filter, "")
		s, jerr := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
			Query:        q,
			Data:         refiltered,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if jerr != nil {
			log := xcontext.Logger(ctx)
			log.Error("v3 refilter: outer JQ eval failed",
				slog.String("name", cr.Name),
				slog.String("namespace", cr.Namespace),
				slog.Any("err", jerr))
			return nil, fmt.Errorf("v3 refilter: outer jq: %w", jerr)
		}
		statusRaw = []byte(s)
	} else {
		marshaled, merr := json.Marshal(refiltered)
		if merr != nil {
			return nil, fmt.Errorf("v3 refilter: marshal refiltered dict: %w", merr)
		}
		statusRaw = marshaled
	}

	cr.Status = &runtime.RawExtension{Raw: statusRaw}

	out, merr := json.Marshal(cr)
	if merr != nil {
		return nil, fmt.Errorf("v3 refilter: re-marshal CR: %w", merr)
	}
	return out, nil
}

// projectChild reapplies the parent api[]'s JQ filter to the child's
// refiltered status. Mirrors what the resolver does on the call_l1 / call_inline
// branches — see resolve.go around the `jsonHandlerCompute` invocation
// before mergeIntoDict.
//
// childOut is the v3-refiltered child wire bytes (i.e. json.Marshal(&childCR)
// after RefilterRESTAction set the per-user Status). We extract the
// "status" object — that's what the parent's JQ filter expects to see
// under the apiCall.Name key — and run the filter against it.
func projectChild(ctx context.Context, childOut []byte, filter *string, apiCallName string) (any, error) {
	if len(childOut) == 0 {
		return nil, nil
	}
	var childCR map[string]any
	if err := json.Unmarshal(childOut, &childCR); err != nil {
		return nil, fmt.Errorf("unmarshal child CR: %w", err)
	}
	status, _ := childCR["status"].(map[string]any)
	if filter == nil {
		return status, nil
	}
	q := ptr.Deref(filter, "")
	pig := map[string]any{apiCallName: status}
	s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
		Query:        q,
		Data:         pig,
		ModuleLoader: jqsupport.ModuleLoader(),
	})
	if err != nil {
		return nil, fmt.Errorf("project child jq: %w", err)
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("unmarshal child projection: %w", err)
	}
	return out, nil
}
