package api

import (
	"fmt"
	"net/http"
	"strings"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// validateUserAccessFilter is the runtime guard that mirrors the CRD CEL
// admission rules. CEL only fires at apply/update time; this function
// catches RESTActions that were admitted under an older CRD version (no
// CEL rules) AND any future drift between the CRD schema and the runtime
// expectations.
//
// Per Q-RBACC-IMPL-5 (architect 2026-05-04) errors here HARD-FAIL the
// api[] entry — they do NOT honour ContinueOnError. A degenerate filter
// is a security-relevant misconfiguration: silently continuing with no
// filter would leak data to the requesting user.
//
// Returns nil when the filter is absent (the no-op path for old YAMLs)
// or correctly populated.
func validateUserAccessFilter(apiCall *templates.API) error {
	if apiCall == nil || apiCall.UserAccessFilter == nil {
		return nil
	}
	f := apiCall.UserAccessFilter

	if strings.TrimSpace(f.Verb) == "" || strings.TrimSpace(f.Resource) == "" {
		return fmt.Errorf("api %q: userAccessFilter degenerate (empty verb=%q or resource=%q)",
			apiCall.Name, f.Verb, f.Resource)
	}

	switch f.Verb {
	case "get", "list", "watch":
	default:
		return fmt.Errorf("api %q: userAccessFilter.verb=%q not in {get,list,watch}",
			apiCall.Name, f.Verb)
	}

	if apiCall.Verb != nil {
		v := strings.ToUpper(*apiCall.Verb)
		if v != "" && v != http.MethodGet && v != http.MethodHead {
			return fmt.Errorf("api %q: userAccessFilter incompatible with HTTP verb %q",
				apiCall.Name, v)
		}
	}

	if apiCall.ExportJWT != nil && *apiCall.ExportJWT {
		return fmt.Errorf("api %q: userAccessFilter incompatible with exportJwt=true",
			apiCall.Name)
	}

	return nil
}
