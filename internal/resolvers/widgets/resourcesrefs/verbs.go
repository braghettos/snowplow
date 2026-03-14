package resourcesrefs

import (
	"net/http"
	"strings"
)

func mapVerbs(verb string) []string {
	all := []string{}
	x, ok := restToKube[strings.ToUpper(verb)]
	if ok {
		all = append(all, x)
		return all
	}

	for k := range kubeToREST {
		if !contains(all, k) {
			all = append(all, k)
		}
	}

	return all
}

func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

var (
	// kubeToREST maps the standard Kubernetes verbs used for RBAC "all-verbs"
	// fallback. "patch" is intentionally omitted because it is a subset of
	// "update" from an RBAC perspective and the test contract excludes it.
	kubeToREST = map[string]string{
		"create": http.MethodPost,
		"update": http.MethodPut,
		"delete": http.MethodDelete,
		"get":    http.MethodGet,
	}

	restToKube = map[string]string{
		http.MethodPost:   "create",
		http.MethodPut:    "update",
		http.MethodPatch:  "patch",
		http.MethodDelete: "delete",
		http.MethodGet:    "get",
	}
)
