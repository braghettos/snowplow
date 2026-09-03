// build_info_expvar_test.go — 1.12.4 §7a acceptance for the build stamp.
//
// Symptom (iii) of the release is "the build stamp is unobservable". The
// design delivers it by two independent routes because the OTLP route
// depends on a k8sattributes precedence question that could only be
// settled after deploy: if the collector's processor wins, ServiceVersion
// carries the chart appVersion and only this expvar route delivers the
// commit. So this surface is not a nicety — on one of the two possible
// outcomes it is the whole answer.
package main

import (
	"encoding/json"
	"expvar"
	"testing"
)

// TestBuildInfoExpvar_PublishesVersionAndConstantOne asserts the shape a
// dashboard reads: a constant 1 carrying the identity as a label.
func TestBuildInfoExpvar_PublishesVersionAndConstantOne(t *testing.T) {
	registerBuildInfoExpvar("deadbeef")

	v := expvar.Get("snowplow_build_info")
	if v == nil {
		t.Fatal("snowplow_build_info is not published — the build stamp stays unobservable " +
			"on any pod with OTEL_ENABLED=false, which is every pod before 1.12.4")
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("snowplow_build_info is not valid JSON (%q): %v", v.String(), err)
	}
	if got["version"] != "deadbeef" {
		t.Errorf("version = %#v; want \"deadbeef\"", got["version"])
	}
	// JSON numbers decode as float64. The constant 1 is what makes this the
	// standard build-info idiom rather than a bare string variable: a
	// dashboard sums or joins on it.
	if n, ok := got["value"].(float64); !ok || n != 1 {
		t.Errorf("value = %#v; want the constant 1", got["value"])
	}
}

// TestBuildInfoExpvar_EmptyBuildReportsUnknown covers the local `go
// build` case, where the linker flag is absent and build is "". Publishing
// an empty label would render as a blank panel that reads like a scrape
// failure; "unknown" is honest and visibly distinct.
func TestBuildInfoExpvar_EmptyBuildReportsUnknown(t *testing.T) {
	registerBuildInfoExpvar("")
	if got := BuildInfoVersion(); got != "unknown" {
		t.Errorf("BuildInfoVersion() with an empty build = %q; want \"unknown\"", got)
	}

	// Re-registering with a real value must still take effect: the Once
	// guards only the Publish, not the value, so a second call updates
	// what the closure reports rather than being silently ignored.
	registerBuildInfoExpvar("cafe1234")
	if got := BuildInfoVersion(); got != "cafe1234" {
		t.Errorf("BuildInfoVersion() after re-register = %q; want \"cafe1234\" — the value must "+
			"not be frozen by the Publish guard", got)
	}
}

// TestBuildInfoExpvar_RegistrationIsIdempotent — expvar.Publish panics on
// a duplicate key, and main() plus any test may both register.
func TestBuildInfoExpvar_RegistrationIsIdempotent(t *testing.T) {
	registerBuildInfoExpvar("v1")
	registerBuildInfoExpvar("v2") // must not panic
}
