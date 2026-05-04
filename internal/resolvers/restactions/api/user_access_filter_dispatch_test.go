package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

// TestEndpointDispatchFork — §9.3 (6 cases).
//
// Drives api.Resolve end-to-end against a mock HTTP server that records the
// inbound Authorization header, then asserts:
//   - JWT suppression behaviour at every fork branch.
//   - Endpoint selection (snowplow callback vs mapper) at every fork branch.
//   - Filter still applies on responses.
type capturedReq struct {
	authHeader string
	path       string
	count      int32
}

func newMockK8s(t *testing.T, body string, cap *capturedReq) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cap.count, 1)
		cap.authHeader = r.Header.Get("Authorization")
		cap.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// build a minimal context with logger + user + access token + evaluator.
func dispatchCtx(t *testing.T, ev cache.RBACEvaluator) context.Context {
	t.Helper()
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "cyberjoker", Groups: []string{"devs"}}),
		xcontext.WithAccessToken("USER-JWT"),
	)
	if ev != nil {
		ctx = cache.WithRBACEvaluator(ctx, ev)
	}
	return ctx
}

func TestEndpointDispatchFork(t *testing.T) {
	const responseBody = `{"items":["demo-system","tenant-a","tenant-b"]}`

	// Helper: a snowplow-endpoint callback pointing at the same server but
	// with a known SA token so we can detect which header path was used.
	saEndpointFor := func(srv *httptest.Server) func() (*endpoints.Endpoint, error) {
		return func() (*endpoints.Endpoint, error) {
			return &endpoints.Endpoint{
				ServerURL: srv.URL,
				Token:     "SNOWPLOW-SA",
			}, nil
		}
	}

	t.Run("UAF_set_NoEndpointRef_UsesSAEndpoint_NoUserJWT", func(t *testing.T) {
		cap := &capturedReq{}
		srv := newMockK8s(t, responseBody, cap)
		defer srv.Close()
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "demo-system")
		ctx := dispatchCtx(t, fr)

		api := &templates.API{
			Name: "ns",
			Path: "/api/v1/namespaces",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
			Filter: ptr.To(`[.ns.items[]]`),
		}

		// We don't actually need K8s rest.Config (we point ServerURL at the test
		// server via SnowplowEndpoint callback), but Resolve needs an RC.
		dict := Resolve(ctx, ResolveOptions{
			RC:               &rest.Config{Host: srv.URL},
			Items:            []*templates.API{api},
			SnowplowEndpoint: saEndpointFor(srv),
		})

		if cap.count == 0 {
			t.Fatal("expected at least 1 HTTP call to mock server")
		}
		if !strings.HasSuffix(cap.authHeader, "SNOWPLOW-SA") {
			t.Errorf("expected snowplow-SA token, got auth=%q", cap.authHeader)
		}
		if strings.Contains(cap.authHeader, "USER-JWT") {
			t.Errorf("user JWT must be suppressed for UAF dispatch, got %q", cap.authHeader)
		}
		got, ok := dict["ns"].([]any)
		if !ok {
			t.Fatalf("expected dict[ns] = []any, got %T %v", dict["ns"], dict["ns"])
		}
		if len(got) != 1 || got[0] != "demo-system" {
			t.Errorf("expected only demo-system kept, got %v", got)
		}
	})

	t.Run("UAF_set_NoEndpointRef_NilCallback_Rejected", func(t *testing.T) {
		cap := &capturedReq{}
		srv := newMockK8s(t, responseBody, cap)
		defer srv.Close()
		ctx := dispatchCtx(t, newFakeRBAC())
		api := &templates.API{
			Name: "ns",
			Path: "/api/v1/namespaces",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
			},
		}
		dict := Resolve(ctx, ResolveOptions{
			RC:    &rest.Config{Host: srv.URL},
			Items: []*templates.API{api},
			// SnowplowEndpoint deliberately nil.
		})
		// Call must be rejected before any HTTP roundtrip.
		if cap.count != 0 {
			t.Errorf("expected 0 HTTP calls when SA endpoint missing, got %d", cap.count)
		}
		// dict must not contain the api result key.
		if _, ok := dict["ns"]; ok {
			t.Errorf("dict should not contain api key on rejection, got %v", dict)
		}
	})

	t.Run("JWT_Suppression_TruthTable", func(t *testing.T) {
		// Direct unit test of shouldInjectUserJWT — covers all 4 corners
		// of the (UAF, EndpointRef, ExportJWT) decision matrix and the
		// ExportJWT override. The Resolve-level integration above
		// covers the consumer wiring; this gates the policy itself.
		ref := &templates.Reference{Name: "ext", Namespace: "tenants"}
		uaf := &templates.UserAccessFilter{Verb: "list", Resource: "namespaces"}
		cases := []struct {
			name string
			api  *templates.API
			want bool
		}{
			{"nilUAF_nilRef_legacyInject", &templates.API{}, true},
			{"nilUAF_setRef_suppress", &templates.API{EndpointRef: ref}, false},
			{"setUAF_nilRef_suppress", &templates.API{UserAccessFilter: uaf}, false},
			{"setUAF_setRef_suppress", &templates.API{UserAccessFilter: uaf, EndpointRef: ref}, false},
			{"exportJWT_overrides_inject", &templates.API{
				UserAccessFilter: uaf, EndpointRef: ref, ExportJWT: ptr.To(true),
			}, true},
			{"exportJWT_alone_inject", &templates.API{ExportJWT: ptr.To(true)}, true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := shouldInjectUserJWT(tc.api); got != tc.want {
					t.Errorf("shouldInjectUserJWT=%v want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("UAF_set_NoCallback_FallsBackToContextProvider", func(t *testing.T) {
		// Verify the fallback path: when opts.SnowplowEndpoint is nil
		// but cache.WithSnowplowEndpoint is installed in ctx, Resolve
		// uses the context provider.
		cap := &capturedReq{}
		srv := newMockK8s(t, responseBody, cap)
		defer srv.Close()
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "demo-system")
		ctx := dispatchCtx(t, fr)
		ctx = cache.WithSnowplowEndpoint(ctx, func() (any, error) {
			return &endpoints.Endpoint{
				ServerURL: srv.URL,
				Token:     "SNOWPLOW-SA-CTX",
			}, nil
		})
		api := &templates.API{
			Name: "ns",
			Path: "/api/v1/namespaces",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
			Filter: ptr.To(`[.ns.items[]]`),
		}
		dict := Resolve(ctx, ResolveOptions{
			RC:    &rest.Config{Host: srv.URL},
			Items: []*templates.API{api},
			// SnowplowEndpoint deliberately nil — exercises fallback.
		})
		if cap.count == 0 {
			t.Fatal("expected HTTP call via ctx fallback")
		}
		if !strings.HasSuffix(cap.authHeader, "SNOWPLOW-SA-CTX") {
			t.Errorf("expected ctx SA token, got %q", cap.authHeader)
		}
		if got := dict["ns"]; got == nil {
			t.Errorf("expected filtered dict[ns]")
		}
	})

	t.Run("ValidateRejects_DegenerateFilter_HardFails", func(t *testing.T) {
		cap := &capturedReq{}
		srv := newMockK8s(t, responseBody, cap)
		defer srv.Close()
		ctx := dispatchCtx(t, newFakeRBAC())
		api := &templates.API{
			Name: "ns",
			Path: "/api/v1/namespaces",
			UserAccessFilter: &templates.UserAccessFilter{
				// Empty resource → degenerate.
				Verb: "list", Resource: "",
			},
		}
		dict := Resolve(ctx, ResolveOptions{
			RC:               &rest.Config{Host: srv.URL},
			Items:            []*templates.API{api},
			SnowplowEndpoint: saEndpointFor(srv),
		})
		if cap.count != 0 {
			t.Errorf("validate should hard-fail before HTTP, got %d calls", cap.count)
		}
		if _, ok := dict["ns"]; ok {
			t.Errorf("dict should not contain rejected api, got %v", dict)
		}
	})

	t.Run("Filter_StillAppliesOnResponse", func(t *testing.T) {
		// A list of 3 items, only "demo-system" is allowed.
		body := `{"items":["demo-system","tenant-a","tenant-b"]}`
		cap := &capturedReq{}
		srv := newMockK8s(t, body, cap)
		defer srv.Close()
		fr := newFakeRBAC()
		fr.Allow("list", "", "namespaces", "demo-system")
		ctx := dispatchCtx(t, fr)
		api := &templates.API{
			Name: "ns",
			Path: "/api/v1/namespaces",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb: "list", Resource: "namespaces",
				NamespaceFrom: ptr.To("."),
			},
			Filter: ptr.To(`[.ns.items[]]`),
		}
		dict := Resolve(ctx, ResolveOptions{
			RC:               &rest.Config{Host: srv.URL},
			Items:            []*templates.API{api},
			SnowplowEndpoint: saEndpointFor(srv),
		})
		got, ok := dict["ns"].([]any)
		if !ok {
			b, _ := json.Marshal(dict)
			t.Fatalf("expected []any in dict[ns], got %s", string(b))
		}
		if len(got) != 1 || got[0] != "demo-system" {
			t.Errorf("expected only demo-system, got %v", got)
		}
	})
}
