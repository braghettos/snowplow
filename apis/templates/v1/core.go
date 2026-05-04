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
// +kubebuilder:validation:XValidation:rule="!(has(self.userAccessFilter) && (self.userAccessFilter.verb == '' || self.userAccessFilter.resource == ''))",message="api[].userAccessFilter requires non-empty verb and resource"
// +kubebuilder:validation:XValidation:rule="!(has(self.userAccessFilter) && has(self.verb) && !(self.verb in ['GET','HEAD','get','head']))",message="api[].userAccessFilter is only allowed with read HTTP verbs (GET, HEAD)"
// +kubebuilder:validation:XValidation:rule="!(has(self.userAccessFilter) && has(self.exportJwt) && self.exportJwt == true)",message="api[].userAccessFilter is incompatible with exportJwt=true"
type API struct {
	// Name is a (unique) identifier
	Name string `json:"name"`
	// Path is the request URI path
	Path string `json:"path,omitempty"`
	// Verb is the request method (GET if omitted)
	Verb *string `json:"verb,omitempty"`
	//+listType=atomic
	// Headers is an array of custom request headers
	Headers []string `json:"headers,omitempty"`
	// Payload is the request body
	Payload *string `json:"payload,omitempty"`
	// EndpointRef a reference to an Endpoint
	EndpointRef *Reference `json:"endpointRef,omitempty"`
	// UserAccessFilter declares a per-user RBAC check applied to each item
	// of the call's response array. Snowplow evaluates the check via
	// RBACWatcher.EvaluateRBAC (in-memory, ~10-50 µs per item, zero K8s
	// API roundtrips) using the user identity from xcontext.UserInfo(ctx).
	//
	// Presence of UserAccessFilter on an api[] entry has TWO effects:
	//   1. The HTTP call is dispatched using snowplow's own ServiceAccount
	//      identity (the in-cluster SA token + CA mounted in the pod,
	//      exposed via rest.InClusterConfig() and converted to an
	//      endpoints.Endpoint at request time).
	//   2. The response is filtered per requesting user before being merged
	//      into the resolution dict.
	//
	// If EndpointRef IS set, EndpointRef wins (operator escape hatch);
	// the filter still applies.
	UserAccessFilter *UserAccessFilter `json:"userAccessFilter,omitempty"`
	// DependsOn reference to another API on which this depends
	DependsOn *Dependency `json:"dependsOn,omitempty"`

	Filter *string `json:"filter,omitempty"`

	ContinueOnError *bool `json:"continueOnError,omitempty"`

	ErrorKey *string `json:"errorKey,omitempty"`

	ExportJWT *bool `json:"exportJwt,omitempty"`
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

// UserAccessFilter declares a per-user RBAC filter applied to each item of
// an API call's response array. See API.UserAccessFilter for semantics.
//
// Verb is restricted to read operations: get, list, watch.
// Group is the K8s API group (empty string for core resources, e.g. namespaces).
// Resource is the plural resource name (e.g. namespaces, customresourcedefinitions).
// NamespaceFrom is an optional jq expression evaluated against each item to
// extract the namespace string used in the RBAC check (empty/omitted means
// the resource is cluster-scoped and the check uses namespace="").
type UserAccessFilter struct {
	// Verb is the RBAC verb to evaluate. One of: get, list, watch.
	// +kubebuilder:validation:Enum=get;list;watch
	Verb string `json:"verb"`
	// Group is the K8s API group of the filtered resource. Empty string
	// for core resources (e.g. namespaces, pods).
	// +optional
	Group string `json:"group,omitempty"`
	// Resource is the K8s plural resource name.
	Resource string `json:"resource"`
	// NamespaceFrom is a jq expression evaluated against each item to
	// extract the namespace used in the RBAC check. When empty or omitted
	// the filter treats the resource as cluster-scoped (namespace="").
	NamespaceFrom *string `json:"namespaceFrom,omitempty"`
}
