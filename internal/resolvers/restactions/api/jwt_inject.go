package api

import (
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// shouldInjectUserJWT decides whether the user's bearer JWT should be
// appended to the outgoing call's Authorization header.
//
// The legacy default (pre-Q-RBAC-DECOUPLE) was: inject IFF EndpointRef is
// nil OR ExportJWT is true. After C(d) v2 the rule is extended:
//
//   - If UserAccessFilter is set, the call is elevated via snowplow's own
//     ServiceAccount identity. Forwarding the user JWT to that endpoint
//     would (a) leak it to the K8s API server unnecessarily and (b) cause
//     RBAC ambiguity (which identity authorises?). Suppress it.
//   - ExportJWT=true remains the explicit opt-in (and is rejected
//     alongside UserAccessFilter by the runtime validator + CEL rule #3,
//     so the combination is unreachable in practice — but the gate is
//     tested anyway as a defence-in-depth check).
//
// Result table (UAF=UserAccessFilter, ER=EndpointRef, JWT=ExportJWT):
//
//	UAF | ER  | JWT  | inject user JWT?
//	----+-----+------+------------------
//	nil | nil | -    | YES (legacy default)
//	nil | set | -    | NO  (user→external endpoint, JWT belongs to internal)
//	set | nil | -    | NO  (snowplow-SA dispatch)
//	set | set | -    | NO  (operator escape hatch — same logic as UAF=nil/ER=set)
//	any | any | true | YES (explicit opt-in; UAF+JWT=true is blocked elsewhere)
func shouldInjectUserJWT(apiCall *templates.API) bool {
	if ptr.Deref(apiCall.ExportJWT, false) {
		return true
	}
	if apiCall.EndpointRef != nil {
		return false
	}
	if apiCall.UserAccessFilter != nil {
		return false
	}
	return true
}
