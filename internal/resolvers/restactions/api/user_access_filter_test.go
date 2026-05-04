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
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeRBAC is a deterministic mock for cache.RBACEvaluator. Each entry maps
// a (verb, group, resource, namespace) tuple to an allow/deny decision.
type fakeRBAC struct {
	calls   int
	allowed map[string]bool
}

func newFakeRBAC() *fakeRBAC { return &fakeRBAC{allowed: map[string]bool{}} }

func (f *fakeRBAC) Allow(verb, group, resource, ns string) {
	f.allowed[fakeKey(verb, group, resource, ns)] = true
}

func (f *fakeRBAC) EvaluateRBAC(_ string, _ []string, verb string, gr schema.GroupResource, ns string) bool {
	f.calls++
	return f.allowed[fakeKey(verb, gr.Group, gr.Resource, ns)]
}

func fakeKey(verb, group, resource, ns string) string {
	return verb + "|" + group + "|" + resource + "|" + ns
}

// buildCtx returns a context wired with the test logger, a user identity,
// and (optionally) a mock RBACEvaluator.
func buildCtx(t *testing.T, ev cache.RBACEvaluator, user jwtutil.UserInfo, logBuf *bytes.Buffer) context.Context {
	t.Helper()
	handler := slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(logger),
		xcontext.WithUserInfo(user),
	)
	if ev != nil {
		ctx = cache.WithRBACEvaluator(ctx, ev)
	}
	return ctx
}

// TestApplyUserAccessFilter — §9.1 (12 cases).
func TestApplyUserAccessFilter(t *testing.T) {
	user := jwtutil.UserInfo{Username: "cyberjoker", Groups: []string{"devs"}}

	t.Run("NoFilter_Passthrough", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := buildCtx(t, nil, user, &buf)
		api := &templates.API{Name: "ns"}
		in := []any{"a", "b"}
		got := applyUserAccessFilter(ctx, api, in)
		if len(got.([]any)) != 2 {
			t.Fatalf("expected passthrough, got %v", got)
		}
	})

	t.Run("NonArray_WarnAndPassthrough", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC()
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "x",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
			},
		}
		in := map[string]any{"some": "object"}
		got := applyUserAccessFilter(ctx, api, in)
		if _, ok := got.(map[string]any); !ok {
			t.Fatalf("expected passthrough of map, got %T", got)
		}
		if !strings.Contains(buf.String(), "response not an array") {
			t.Errorf("expected warn log, got %q", buf.String())
		}
	})

	t.Run("RBACPass_All", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "demo-system")
		fr.Allow("list", "", "namespaces", "tenant-a")
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
		}
		in := []any{"demo-system", "tenant-a"}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 2 {
			t.Fatalf("expected both kept, got %v", got)
		}
	})

	t.Run("RBACDeny_All", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC() // empty allowlist → all deny
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
		}
		in := []any{"demo-system", "tenant-a"}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 0 {
			t.Fatalf("expected all denied, got %v", got)
		}
	})

	t.Run("RBACMixed_50_to_1", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC()
		// Only "demo-system" allowed.
		fr.Allow("list", "", "namespaces", "demo-system")
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
		}
		in := make([]any, 50)
		for i := range in {
			in[i] = "tenant-" + string(rune('a'+i))
		}
		in[7] = "demo-system"
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 1 || got[0] != "demo-system" {
			t.Fatalf("expected only demo-system, got %v", got)
		}
	})

	t.Run("NamespaceFromJQString", func(t *testing.T) {
		// Q-RBACC-IMPL-6 verification: jqsupport.Eval(".", "demo-system")
		// must return "demo-system" (string).
		out, err := jqsupport.Eval(context.Background(), ".", "demo-system")
		if err != nil {
			t.Fatalf("jqsupport.Eval: %v", err)
		}
		s, ok := out.(string)
		if !ok || s != "demo-system" {
			t.Fatalf("expected \"demo-system\" string, got %T %v", out, out)
		}

		var buf bytes.Buffer
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "demo-system")
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
		}
		in := []any{"demo-system", "blocked"}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 1 || got[0] != "demo-system" {
			t.Fatalf("expected demo-system only, got %v", got)
		}
	})

	t.Run("NamespaceFromObjectField", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC()
		fr.Allow("list", "core.krateo.io", "compositions", "tenant-a")
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "compositions",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Group: "core.krateo.io", Resource: "compositions",
				NamespaceFrom: ptr.To(".metadata.namespace"),
			},
		}
		in := []any{
			map[string]any{"metadata": map[string]any{"namespace": "tenant-a"}},
			map[string]any{"metadata": map[string]any{"namespace": "tenant-b"}},
		}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 1 {
			t.Fatalf("expected 1 kept, got %d", len(got))
		}
		obj := got[0].(map[string]any)
		if obj["metadata"].(map[string]any)["namespace"] != "tenant-a" {
			t.Fatalf("kept wrong item: %v", obj)
		}
	})

	t.Run("JQError_DropsItem", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "x")
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				// Invalid jq expression triggers compilation error.
				NamespaceFrom: ptr.To("(((not valid jq"),
			},
		}
		in := []any{"x", "y", "z"}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 0 {
			t.Fatalf("expected all items dropped on jq error, got %d", len(got))
		}
	})

	t.Run("JQNonString_DropsItem", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "x")
		ctx := buildCtx(t, fr, user, &buf)
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To(".count"),
			},
		}
		// .count is a number — non-string, should be dropped.
		in := []any{
			map[string]any{"count": 5.0},
			map[string]any{"count": 7.0},
		}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 0 {
			t.Fatalf("expected drops on non-string, got %d", len(got))
		}
	})

	t.Run("NoUserInfo_FailClosed", func(t *testing.T) {
		var buf bytes.Buffer
		// Explicitly do NOT install user info.
		handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
		logger := slog.New(handler)
		ctx := xcontext.BuildContext(context.Background(), xcontext.WithLogger(logger))
		ctx = cache.WithRBACEvaluator(ctx, newFakeRBAC())

		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
			},
		}
		in := []any{"a", "b"}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 0 {
			t.Fatalf("expected fail-closed empty, got %v", got)
		}
		if !strings.Contains(buf.String(), "cannot read user identity") {
			t.Errorf("expected error log about user identity, got %q", buf.String())
		}
	})

	t.Run("NoRBACEvaluator_FailClosed", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := buildCtx(t, nil, user, &buf) // no evaluator installed
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
			},
		}
		in := []any{"a", "b"}
		got := applyUserAccessFilter(ctx, api, in).([]any)
		if len(got) != 0 {
			t.Fatalf("expected fail-closed empty, got %v", got)
		}
		if !strings.Contains(buf.String(), "RBACWatcher not in context") {
			t.Errorf("expected error log about evaluator missing, got %q", buf.String())
		}
	})

	t.Run("AuditLogShape_FieldsPresent", func(t *testing.T) {
		var buf bytes.Buffer
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "demo-system")
		ctx := buildCtx(t, fr, user, &buf)
		ctx = cache.WithRESTActionName(ctx, "compositions-get-ns-and-crd")
		api := &templates.API{
			Name: "ns",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
		}
		in := []any{"demo-system", "blocked"}
		_ = applyUserAccessFilter(ctx, api, in)

		got := buf.String()
		expectedFields := []string{
			"audit=user_access_filter",
			"restaction=compositions-get-ns-and-crd",
			"api_call=ns",
			"user=cyberjoker",
			"filter_verb=list",
			"filter_resource=namespaces",
			"items_in=2",
			"items_out=1",
			"denied=1",
			"jq_errors=0",
			"duration=",
		}
		for _, f := range expectedFields {
			if !strings.Contains(got, f) {
				t.Errorf("audit log missing %q\nfull log:\n%s", f, got)
			}
		}
	})
}
