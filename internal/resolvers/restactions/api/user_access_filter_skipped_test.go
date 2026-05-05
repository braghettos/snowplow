package api

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestAuditUserAccessFilterSkipped — Q-RBAC-DECOUPLE C(d) v5 — D3a
// (audit 2026-05-05).
//
// Asserts that auditUserAccessFilterSkipped emits the expected
// structured slog record. This is the direct catcher for the
// resolver-error suppression hole identified in the audit.
func TestAuditUserAccessFilterSkipped(t *testing.T) {
	t.Run("Skipped_OnApiError_EmitsAuditLine", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

		ctx := xcontext.BuildContext(context.Background(),
			xcontext.WithLogger(log),
			xcontext.WithUserInfo(jwtutil.UserInfo{
				Username: "cyberjoker",
				Groups:   []string{"krateo-widgets-reader"},
			}),
		)
		ctx = cache.WithRESTActionName(ctx, "compositions-get-ns-and-crd")

		apiCall := &templates.API{
			Name: "crds",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb:          "list",
				Group:         "apiextensions.k8s.io",
				Resource:      "customresourcedefinitions",
				NamespaceFrom: ptr.To("."),
			},
		}
		auditUserAccessFilterSkipped(ctx, apiCall, "api_error", "TLS x509 verify failed")

		out := buf.String()
		mustContainAll(t, out, []string{
			`"audit":"user_access_filter_skipped"`,
			`"restaction":"compositions-get-ns-and-crd"`,
			`"api_call":"crds"`,
			`"user":"cyberjoker"`,
			`"groups":["krateo-widgets-reader"]`,
			`"filter_verb":"list"`,
			`"filter_group":"apiextensions.k8s.io"`,
			`"filter_resource":"customresourcedefinitions"`,
			`"filter_namespace_from":"."`,
			`"reason":"api_error"`,
			`"error":"TLS x509 verify failed"`,
		})
	})

	t.Run("Skipped_NoUAF_NotEmitted", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

		ctx := xcontext.BuildContext(context.Background(),
			xcontext.WithLogger(log),
			xcontext.WithUserInfo(jwtutil.UserInfo{Username: "anyone"}),
		)

		// API entry without UserAccessFilter — emit must be a no-op.
		apiCall := &templates.API{Name: "plain", UserAccessFilter: nil}
		auditUserAccessFilterSkipped(ctx, apiCall, "api_error", "anything")

		if buf.Len() != 0 {
			t.Fatalf("expected zero log output for non-UAF apiCall; got: %s", buf.String())
		}

		// Nil apiCall — must also be a no-op.
		auditUserAccessFilterSkipped(ctx, nil, "api_error", "anything")
		if buf.Len() != 0 {
			t.Fatalf("expected zero log output for nil apiCall; got: %s", buf.String())
		}
	})

	t.Run("Skipped_FieldShape_MatchesUAFAuditForParserParity", func(t *testing.T) {
		// Both emits must include the same {restaction, api_call, user,
		// groups, filter_verb, filter_group, filter_resource,
		// filter_namespace_from} field set so log parsers can share the
		// schema. The skipped variant ADDs {reason, error} and DROPS the
		// count fields {items_in, items_out, denied, jq_errors, duration}.

		// Capture both emits.
		var skipped, full bytes.Buffer
		skLog := slog.New(slog.NewJSONHandler(&skipped, nil))
		fullLog := slog.New(slog.NewJSONHandler(&full, nil))

		mkCtx := func(log *slog.Logger) context.Context {
			c := xcontext.BuildContext(context.Background(),
				xcontext.WithLogger(log),
				xcontext.WithUserInfo(jwtutil.UserInfo{Username: "u", Groups: []string{"g"}}),
			)
			return cache.WithRESTActionName(c, "ra")
		}

		apiCall := &templates.API{
			Name: "n",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Group: "g", Resource: "r",
			},
		}

		auditUserAccessFilterSkipped(mkCtx(skLog), apiCall, "api_error", "boom")
		auditUserAccessFilter(mkCtx(fullLog), apiCall, 0, 0, 0, 0, 0)

		// Both must contain the shared field set.
		shared := []string{
			`"restaction"`, `"api_call"`, `"user"`, `"groups"`,
			`"filter_verb"`, `"filter_group"`, `"filter_resource"`, `"filter_namespace_from"`,
		}
		mustContainAll(t, skipped.String(), shared)
		mustContainAll(t, full.String(), shared)

		// Skipped variant must include {reason, error}.
		mustContainAll(t, skipped.String(), []string{`"reason"`, `"error"`})

		// Full variant must include the count fields.
		mustContainAll(t, full.String(), []string{`"items_in"`, `"items_out"`, `"denied"`, `"jq_errors"`, `"duration"`})
	})
}

func mustContainAll(t *testing.T, hay string, needles []string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			t.Errorf("output missing fragment %q\nFULL OUTPUT:\n%s", n, hay)
		}
	}
}
