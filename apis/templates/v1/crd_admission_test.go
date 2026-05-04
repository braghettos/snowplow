package v1

import (
	"os"
	"strings"
	"testing"
)

// TestCRDUserAccessFilterAdmission — §9.7 (3 cases).
//
// Verifies the rendered crds/templates.krateo.io_restactions.yaml ships
// the three CEL admission rules that gate userAccessFilter:
//   - Degenerate filter (empty verb / empty resource) is rejected.
//   - HTTP=POST + userAccessFilter is rejected.
//   - exportJwt=true + userAccessFilter is rejected.
//
// Runtime hard-fail equivalents of these checks live in
// internal/resolvers/restactions/api/user_access_filter_validate.go and
// are covered by the §9.2 unit test. The CRD-side gate is what stops
// admission of malformed CRs in the first place.
func TestCRDUserAccessFilterAdmission(t *testing.T) {
	const path = "../../../crds/templates.krateo.io_restactions.yaml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	body := string(raw)

	cases := []struct {
		name   string
		needle string
	}{
		{
			name:   "DegenerateFilter_Rejected",
			needle: "userAccessFilter requires non-empty verb and resource",
		},
		{
			name: "HTTP_POST_Rejected",
			// YAML wraps the message across two lines, so we look for the
			// stable prefix.
			needle: "userAccessFilter is only allowed with read HTTP",
		},
		{
			name:   "ExportJWT_True_Rejected",
			needle: "userAccessFilter is incompatible with exportJwt=true",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(body, tc.needle) {
				t.Errorf("CRD must contain CEL message %q", tc.needle)
			}
		})
	}

	// Also assert the verb enum is present so create/update/delete are
	// rejected at admission instead of waiting for the runtime validator.
	if !strings.Contains(body, "- get") || !strings.Contains(body, "- list") || !strings.Contains(body, "- watch") {
		t.Errorf("CRD must enumerate verb values: get, list, watch")
	}
}
