package v1

// Reference to a named object.
type Reference struct {
	// Name of the referenced object.
	Name string `json:"name"`
	// Namespace of the referenced object.
	Namespace string `json:"namespace"`
}

// Dependency reference to the identifier of another API on which this depends
type Dependency struct {
	// Name of another API on which this depends
	Name string `json:"name"`
	// Iterator defines a field on which iterate.
	Iterator *string `json:"iterator,omitempty"`
}

// API represents a request to an HTTP service
//
// Stage-level admission guards (Ship S.1 hoist) — the Go markers are the
// SINGLE SOURCE OF TRUTH for ALL CEL on this CRD. These three security
// guards were historically hand-authored in the snowplow CHART CRD; they
// are hoisted here verbatim so a future `scripts/gen.sh` regen can never
// silently drop them. They sit on the API (stage) struct because each
// rule reads sibling stage fields (self.verb / self.exportJwt) alongside
// self.userAccessFilter — placement the UserAccessFilterSpec-level XOR
// rule cannot reach.
//
// +kubebuilder:validation:XValidation:rule="!has(self.userAccessFilter) || !has(self.verb) || self.verb == '' || self.verb in ['GET', 'HEAD', 'get', 'head']",message="userAccessFilter is only allowed on read-verb HTTP stages (GET/HEAD, case-insensitive); CRUD verbs would expose mutation under filter scope."
// +kubebuilder:validation:XValidation:rule="!has(self.userAccessFilter) || !has(self.exportJwt) || !self.exportJwt",message="userAccessFilter stages MUST NOT have exportJwt: true; would leak the raw JWT through the user-facing filtered response."
// +kubebuilder:validation:XValidation:rule="!has(self.userAccessFilter) || ((has(self.userAccessFilter.resource) && size(self.userAccessFilter.resource) > 0) || (has(self.userAccessFilter.resourcesFrom) && size(self.userAccessFilter.resourcesFrom) > 0)) && self.userAccessFilter.verb != ''",message="userAccessFilter must specify a non-empty verb and exactly one of resource or resourcesFrom; a degenerate filter would collapse the SubjectAccessReview check to a wildcard."
//
// A-4 (1.12.3) — THERE IS DELIBERATELY NO FOURTH RULE BOUNDING
// userAccessFilter.verb. Do not add one; it was tried and reverted.
//
// The temptation is obvious: uaf.Verb is threaded verbatim into
// rbac.EvaluateRBAC (refilter.go evalSingle), so `verb: create` on a stage that
// returns objects of the SAME resource keeps the objects the caller may WRITE
// rather than the ones they may read — a scope inversion. But a blanket
// `verb in ['get','list','watch']` rule is too coarse: three LIVE portal
// RESTActions (testdata/portal_uaf_corpus.yaml, portal @ b2a558d) use
// `verb: create` legitimately. Their api-step returns NAMESPACES and the filter
// checks create on a DIFFERENT resource, so it answers "which namespaces may I
// create X in" — a form picker that discloses nothing the caller cannot
// already determine about their own grants. The rule would have failed the
// portal upgrade and silently emptied three namespace pickers.
//
// The real invariant is narrower — SAME-RESOURCE — and it needs the resource
// the api-step RETURNS, which CEL on this struct cannot derive (the path may be
// jq-templated). So it lives at runtime: refilter.go carries uafVerbIsRead as a
// WARN-ONLY signal in 1.12.3, and 1.13.0 flips it to enforce the same-resource
// rule, failing OPEN on templated paths. See the 1.13.0 note there.
//
// TestA4_PortalCorpus_CreateVerbUAFs_Admitted and
// TestA4_UAFVerbBound_NotReintroduced (uaf_verb_cel_test.go) fail if the rule
// comes back, and name the three portal files it would break.
type API struct {
	// Name is a (unique) identifier
	Name string `json:"name"`
	// Path is the request URI path
	Path string `json:"path,omitempty"`
	// Verb is the request method (GET if omitempty)
	Verb *string `json:"verb,omitempty"`
	//+listType=atomic
	// Headers is an array of custom request headers
	Headers []string `json:"headers,omitempty"`
	// Payload is the request body
	Payload *string `json:"payload,omitempty"`
	// EndpointRef a reference to an Endpoint
	EndpointRef *Reference `json:"endpointRef,omitempty"`
	// DependsOn reference to another API on which this depends
	DependsOn *Dependency `json:"dependsOn,omitempty"`

	Filter *string `json:"filter,omitempty"`

	ContinueOnError *bool `json:"continueOnError,omitempty"`

	ErrorKey *string `json:"errorKey,omitempty"`

	ExportJWT *bool `json:"exportJwt,omitempty"`

	// Resolve controls post-fetch behaviour when this api-step fetches a
	// snowplow RESTAction or Widget CR from a DIRECT apiserver path
	// (internal proposal 2026-06-22, Diego-ratified). OPT-IN — default
	// false (contract move 2026-07-02, docs/resolve-default-flip-plan-2026-07-02.md).
	//
	//   - resolve: true  — after fetching the CR from the (cacheable,
	//     dep-tracked) internal apiserver path, snowplow runs the fetched
	//     object through the resolver IN-PROCESS (restactions.Resolve if
	//     it is a RESTAction, widgets.Resolve if a Widget) and substitutes
	//     the resolved envelope for the stage output — "as if /call'd",
	//     with NO outbound /call HTTP round-trip. RBAC- and depth-gated
	//     exactly as the HTTP /call. For any other kind (e.g. a configmap)
	//     it is a harmless no-op (the raw fetched object is fed unchanged).
	//   - resolve: false (or OMITTED) — return the RAW stored CR (the
	//     pre-proposal plain objects.Get / informer-serve behaviour),
	//     unresolved.
	//
	// The default is OMIT→false: aligned with progressive rendering
	// (feedback_progressive_rendering — the frontend descends resourcesRefs
	// level-by-level; the server returns raw refs/CRs by default) and with
	// the #72 inline-rendered-children opt-in (resolve nested refs only where
	// a widget/step explicitly asks). An api-step that CONSUMES a nested
	// RA/widget's RESOLVED data (e.g. reads a child's resolved-only
	// `.status.<field>`) MUST set resolve:true explicitly to preserve its
	// output. With no kubebuilder default the apiserver injects NOTHING on
	// omit → the field arrives NIL → the resolver's ptr.Deref(...Resolve,
	// false) fallback (resolve.go, the single default-fallback site) is
	// LOAD-BEARING for the nil case; the CRD contract and the resolver agree
	// on nil→false. Removing the marker (rather than =false) keeps etcd clean
	// on omit (no persisted false to disambiguate from an authored choice).
	//
	// +optional
	Resolve *bool `json:"resolve,omitempty"`

	// UserAccessFilter declares that this API call dispatches via
	// the snowplow ServiceAccount (cluster-wide read) and that the
	// returned result set MUST be in-process-refiltered through
	// EvaluateRBAC before being returned to the caller. Added at
	// Tag 0.30.9 Sub-scope A — atomic ship: when present, both
	// ServiceAccount-dispatch AND refilter take effect; there is
	// no per-mechanism toggle. Optional — RestActions without this
	// field unchanged from 0.30.8 (per-user-token dispatch).
	//
	// Per Revision 2 (binding): even with UserAccessFilter set,
	// EvaluateRBAC continues to fire on the dispatch CR itself —
	// UserAccessFilter changes WHO dispatches the inner call, NOT
	// whether the outer dispatch is RBAC-gated. The refilter step
	// also calls EvaluateRBAC per object returned by the SA call.
	UserAccessFilter *UserAccessFilterSpec `json:"userAccessFilter,omitempty"`
}

// UserAccessFilterSpec declares the per-object refilter contract.
// Added at Tag 0.30.9 Sub-scope A.
//
// All fields except NamespaceFrom mirror the SubjectAccessReview
// ResourceAttributes inputs (verb/group/resource); NamespaceFrom is
// a JQ expression evaluated against each returned object to derive
// the per-object namespace the refilter calls EvaluateRBAC with.
//
// Example RestAction stanza:
//
//	api:
//	- name: namespaces
//	  path: /apis/v1/namespaces
//	  endpointRef: { name: krateo-kube, namespace: krateo-system }
//	  userAccessFilter:
//	    verb: get
//	    group: ""
//	    resource: namespaces
//	    namespaceFrom: .metadata.name
//
// At dispatch time:
//   1. snowplow-SA reads the cluster-wide namespace list.
//   2. For each returned namespace, EvaluateRBAC(user, "get", "",
//      "namespaces", .metadata.name) gates whether to keep the entry.
//   3. The filtered result set is returned + cached under a key that
//      includes user_identity (so admin and cyberjoker get distinct
//      L1 entries).
//
// Exactly-one-of(resource, resourcesFrom): the refilter checks EITHER a
// single static plural (Resource) OR a runtime-discovered plural set
// (ResourcesFrom) — never both, never neither. The XOR is enforced at
// admission via the CEL rule below (CEL needs apiextensions/v1 +
// k8s>=1.25 — GKE satisfies this). `resource` is therefore
// conditionally-required THROUGH this rule, NOT via the struct-level
// `required` list (which stays [group, verb]).
//
// +kubebuilder:validation:XValidation:rule="has(self.resource) != has(self.resourcesFrom)",message="exactly one of resource or resourcesFrom must be set"
type UserAccessFilterSpec struct {
	// Verb is the Kubernetes RBAC verb checked per object.
	// Required. Lower-case ("get", "list", "watch", etc.).
	Verb string `json:"verb"`
	// Group is the API group of the checked resource. Empty string
	// = core group. Required (use "" explicitly for core).
	Group string `json:"group"`
	// Resource is the plural resource name (e.g. "namespaces").
	// The STATIC resource. Required UNLESS ResourcesFrom is set — when
	// ResourcesFrom is set the resource plural set is derived at
	// dispatch time and Resource may be left empty.
	Resource string `json:"resource,omitempty"`
	// ResourcesFrom is a JQ expression evaluated ONCE against the full
	// resolve dict, yielding a []string of resource plurals — Ship
	// 0.30.129. Symmetric with NamespaceFrom (which is jq-evaluated
	// per object): ResourcesFrom lets the checked resource set itself
	// be RUNTIME-DISCOVERED rather than a static literal.
	//
	// When set, the refilter keeps a namespace iff the user can perform
	// Verb on ANY plural in the set (OR semantics) in that namespace.
	// Group stays static (a single API group). When unset, behaviour is
	// byte-identical to pre-0.30.129 — the static Resource is checked.
	//
	// Use case: compositions-get-ns-and-crd discovers the composition
	// CRD plurals at runtime in dict["crds"]; resourcesFrom evaluates
	// "[ (.crds // [])[] | .plural ]" so the per-namespace RBAC prune
	// covers exactly the discovered composition CRDs — no hardcoded
	// plural literal.
	ResourcesFrom string `json:"resourcesFrom,omitempty"`
	// NamespaceFrom is a JQ path expression evaluated against each
	// returned object to derive the per-object namespace for the
	// EvaluateRBAC call. Typical values:
	//   - ".metadata.name" when the returned objects ARE namespaces
	//     (cluster-scoped check by name, returns namespace itself).
	//   - ".metadata.namespace" when the returned objects live IN
	//     namespaces (e.g. CustomResourceDefinitions don't, but
	//     compositions do).
	//   - "." when the items are bare namespace-name strings (the
	//     namespaces-stage post-filter shape).
	//
	// Optional with a default of ".metadata.namespace": when the field
	// is ABSENT the refilter evaluates ".metadata.namespace" against
	// each object — the common namespaced-object shape — rather than
	// falling back to a cluster-scope (namespace="") RBAC check. The
	// cluster-scope check is the WRONG default for the dominant
	// namespaced-object case: it would deny a narrow dev who holds the
	// grant only in their own namespace. An explicit "." or
	// ".metadata.name" still overrides the default verbatim; the default
	// only fires when the field is omitted.
	//
	// +optional
	// +kubebuilder:default=".metadata.namespace"
	NamespaceFrom string `json:"namespaceFrom,omitempty"`
	// NameFrom is a JQ path expression evaluated against each returned
	// object to derive the per-object NAME for the EvaluateRBAC call.
	// Symmetric with NamespaceFrom (which derives the per-object
	// namespace): NameFrom derives the per-object name so a
	// resourceNames-scoped RBAC grant (a Role/ClusterRole rule with a
	// non-empty resourceNames, valid only for name-specific verbs —
	// get/update/patch/delete) is honoured. Typical values:
	//   - ".metadata.name" (the default) for a K8s object.
	//   - "." when the items are bare name strings (the namespaces-stage
	//     post-filter shape — the projected item IS the name).
	//
	// Optional with a default of ".metadata.name": when the field is
	// ABSENT the refilter evaluates ".metadata.name" against each object.
	// Without the derived name the EvaluateRBAC call carries an empty
	// Name, and a resourceNames-scoped grant (which requires the request
	// name to appear in rule.resourceNames) matches NOTHING — every named
	// object is silently dropped (issue #123: under-serve / fail-closed).
	// The default only fires when the field is omitted; an explicit "."
	// still overrides it verbatim.
	//
	// Note: NameFrom has NO effect on collection verbs
	// (list/watch/create/deletecollection): the RBAC evaluator scopes
	// resourceNames to name-specific verbs only, so a plain list grant is
	// evaluated identically whether or not a Name is threaded.
	//
	// +optional
	// +kubebuilder:default=".metadata.name"
	NameFrom string `json:"nameFrom,omitempty"`
}

// ObjectReference is a reference to a named object in a specified namespace.
type ObjectReference struct {
	Reference  `json:",inline"`
	Resource   string `json:"resource,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}

// Data is a key value pair.
type Data struct {
	// Name of the data
	Name string `json:"name"`
	// Value of the data. Can be also a JQ expression.
	Value string `json:"value,omitempty"`
	// AsString if true the value will be considered verbatim as string.
	AsString *bool `json:"asString,omitempty"`
}
