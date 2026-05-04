package api

import (
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// TestValidateUserAccessFilter — §9.2 (8 cases).
func TestValidateUserAccessFilter(t *testing.T) {
	cases := []struct {
		name      string
		api       *templates.API
		wantErr   bool
		errSubstr string
	}{
		{
			name: "NilFilter",
			api:  &templates.API{Name: "ns"},
		},
		{
			name: "Valid_ListNamespaces",
			api: &templates.API{
				Name: "ns",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:     "list",
					Group:    "",
					Resource: "namespaces",
				},
			},
		},
		{
			name: "EmptyVerb",
			api: &templates.API{
				Name: "ns",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:     "",
					Resource: "namespaces",
				},
			},
			wantErr:   true,
			errSubstr: "degenerate",
		},
		{
			name: "EmptyResource",
			api: &templates.API{
				Name: "ns",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:     "list",
					Resource: "",
				},
			},
			wantErr:   true,
			errSubstr: "degenerate",
		},
		{
			name: "VerbCreateRejected",
			api: &templates.API{
				Name: "ns",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:     "create",
					Resource: "namespaces",
				},
			},
			wantErr:   true,
			errSubstr: "not in {get,list,watch}",
		},
		{
			name: "HTTP_POST_Rejected",
			api: &templates.API{
				Name: "ns",
				Verb: ptr.To("POST"),
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:     "list",
					Resource: "namespaces",
				},
			},
			wantErr:   true,
			errSubstr: "incompatible with HTTP verb",
		},
		{
			name: "HTTP_GET_OK",
			api: &templates.API{
				Name: "ns",
				Verb: ptr.To("GET"),
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:     "list",
					Resource: "namespaces",
				},
			},
		},
		{
			name: "ExportJWT_True_Rejected",
			api: &templates.API{
				Name:      "ns",
				ExportJWT: ptr.To(true),
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:     "list",
					Resource: "namespaces",
				},
			},
			wantErr:   true,
			errSubstr: "exportJwt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUserAccessFilter(tc.api)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
