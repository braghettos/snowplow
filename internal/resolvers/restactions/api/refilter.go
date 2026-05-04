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
}

// MarshalCached returns the v3 wire bytes a caller (l1cache writer) should
// store under the L1 key. Exported so non-test callers can produce the
// canonical shape without re-implementing the schema_version handling.
func MarshalCached(cr *templates.RESTAction, protectedDict map[string]any) ([]byte, error) {
	wrapper := CachedRESTAction{
		SchemaVersion: CachedSchemaVersionV3,
		CR:            cr,
		ProtectedDict: protectedDict,
	}
	return json.Marshal(wrapper)
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

	// Outer JQ: re-evaluate cr.Spec.Filter against refiltered dict to
	// produce cr.Status.Raw. If no outer filter, marshal the refiltered
	// dict directly (matches restactions.Resolve's else branch).
	var statusRaw []byte
	if wrapper.CR.Spec.Filter != nil {
		q := ptr.Deref(wrapper.CR.Spec.Filter, "")
		s, jerr := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
			Query:        q,
			Data:         refiltered,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if jerr != nil {
			log := xcontext.Logger(ctx)
			log.Error("v3 refilter: outer JQ eval failed",
				slog.String("name", wrapper.CR.Name),
				slog.String("namespace", wrapper.CR.Namespace),
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

	wrapper.CR.Status = &runtime.RawExtension{Raw: statusRaw}

	out, merr := json.Marshal(wrapper.CR)
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
