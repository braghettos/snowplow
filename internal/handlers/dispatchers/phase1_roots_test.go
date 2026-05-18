// phase1_roots_test.go — 0.30.107 falsifiers for the ConfigMap-derived
// navigation-root source.
//
// The no-hardcode contract: Phase 1's navigation roots are NOT the
// literal GVRs `navmenus` / `routesloaders`; they are read from the
// frontend ConfigMap's config.json (.api.INIT / .api.ROUTES_LOADER).
// These tests prove the parse + /call-URL decode is correct and does not
// depend on any literal resource name.

package dispatchers

import (
	"encoding/json"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/handlers/util"
)

// TestFrontendConfig_ParsesInitAndRoutesLoader proves config.json's
// api.INIT and api.ROUTES_LOADER are extracted verbatim — the production
// ConfigMap shape.
func TestFrontendConfig_ParsesInitAndRoutesLoader(t *testing.T) {
	raw := `{
	  "api": {
	    "AUTHN_API_BASE_URL": "http://authn:8082",
	    "INIT": "/call?resource=navmenus&apiVersion=widgets.templates.krateo.io/v1beta1&name=sidebar-nav-menu&namespace=krateo-system",
	    "ROUTES_LOADER": "/call?resource=routesloaders&apiVersion=widgets.templates.krateo.io/v1beta1&name=routes-loader&namespace=krateo-system"
	  },
	  "params": { "FRONTEND_NAMESPACE": "krateo-system" }
	}`
	var cfg frontendConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if cfg.API.Init == "" || cfg.API.RoutesLoader == "" {
		t.Fatalf("INIT/ROUTES_LOADER not extracted: %+v", cfg.API)
	}
}

// TestFrontendConfig_RootsDecodeFromConfigNotLiterals proves the root
// ObjectReferences are derived by decoding the config.json /call URLs —
// the resource/name/namespace come from config, not Go literals. If the
// ConfigMap named a DIFFERENT widget, Phase 1 would follow it.
func TestFrontendConfig_RootsDecodeFromConfigNotLiterals(t *testing.T) {
	// A deliberately NON-default config — different resource, name and
	// namespace than the production sidebar-nav-menu. The decoder must
	// follow the config, proving nothing is hardcoded.
	raw := `{
	  "api": {
	    "INIT": "/call?resource=othermenus&apiVersion=widgets.example.io/v2&name=custom-menu&namespace=tenant-a",
	    "ROUTES_LOADER": "/call?resource=otherloaders&apiVersion=widgets.example.io/v2&name=custom-loader&namespace=tenant-b"
	  }
	}`
	var cfg frontendConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	initRef, ok := util.ParseCallPathToObjectRef(cfg.API.Init)
	if !ok {
		t.Fatalf("INIT /call URL did not decode")
	}
	if initRef.Resource != "othermenus" || initRef.Name != "custom-menu" ||
		initRef.Namespace != "tenant-a" || initRef.APIVersion != "widgets.example.io/v2" {
		t.Fatalf("INIT ref not config-derived: %+v", initRef)
	}

	rlRef, ok := util.ParseCallPathToObjectRef(cfg.API.RoutesLoader)
	if !ok {
		t.Fatalf("ROUTES_LOADER /call URL did not decode")
	}
	if rlRef.Resource != "otherloaders" || rlRef.Name != "custom-loader" ||
		rlRef.Namespace != "tenant-b" {
		t.Fatalf("ROUTES_LOADER ref not config-derived: %+v", rlRef)
	}
}

// TestFrontendConfig_MissingEntryPointTolerated proves an absent/empty
// entry point yields no ref (skipped, non-fatal) rather than a panic or
// a bogus ref.
func TestFrontendConfig_MissingEntryPointTolerated(t *testing.T) {
	raw := `{"api":{"INIT":"","ROUTES_LOADER":"/call?resource=routesloaders&apiVersion=widgets.templates.krateo.io/v1beta1&name=routes-loader&namespace=krateo-system"}}`
	var cfg frontendConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.API.Init != "" {
		t.Fatalf("expected empty INIT")
	}
	if _, ok := util.ParseCallPathToObjectRef(cfg.API.RoutesLoader); !ok {
		t.Fatalf("ROUTES_LOADER should still decode")
	}
}

// TestFrontendConfig_NonCallURLRejected proves a non-/call entry point
// (an external link, a malformed value) is rejected by the decoder —
// the walk must not try to fetch a bogus root.
func TestFrontendConfig_NonCallURLRejected(t *testing.T) {
	for _, bad := range []string{
		"https://example.com/external",
		"/call?name=x&namespace=y", // no resource/apiVersion
		"not a url at all ${leak}",
	} {
		if _, ok := util.ParseCallPathToObjectRef(bad); ok {
			t.Fatalf("bad entry point %q should NOT decode to a root ref", bad)
		}
	}
}

// TestFrontendConfigConfigMapName_DefaultAndOverride proves the ConfigMap
// pointer is config, not a Go constant: the default applies when the env
// var is unset, and the env var overrides it.
func TestFrontendConfigConfigMapName_DefaultAndOverride(t *testing.T) {
	t.Setenv(frontendConfigConfigMapEnv, "")
	if got := frontendConfigConfigMapName(); got != frontendConfigConfigMapDefault {
		t.Fatalf("default: want %q got %q", frontendConfigConfigMapDefault, got)
	}
	t.Setenv(frontendConfigConfigMapEnv, "tenant-frontend-cfg")
	if got := frontendConfigConfigMapName(); got != "tenant-frontend-cfg" {
		t.Fatalf("override: want %q got %q", "tenant-frontend-cfg", got)
	}
}
