package tracing

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// F3 (chart half) — the SHIPPED sampler values (1.12.4 design §2.3 / §8,
// gate condition C1).
//
// The code-level F3 arms in sampler_env_test.go prove that the sampler env
// knob is live and that traceidratio ignores a sampled parent. They cannot
// prove what the chart SHIPS — and the C1 defect was a chart value. This
// arm reads helm/snowplow/values.yaml from the repo and asserts the
// contract the release notes make: OTEL_ENABLED ships together with a
// root-deciding sampler family and an explicit ratio, and OTLP logs are
// explicitly off (the collector has no OTLP logs receiver).
//
// RED on main (8de5295): values.yaml sets no OTEL_* key at all — with
// OTEL_ENABLED flipped on by an operator the SDK default would be
// ParentBased(AlwaysSample), i.e. 100% of customer /call behind the
// 100%-sampling agent gateway.
func TestF3_ChartShipsRootDecidingSampler(t *testing.T) {
	// go test runs with cwd = the package dir: go/snowplow/internal/tracing.
	path := filepath.Join("..", "..", "..", "..", "helm", "snowplow", "values.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chart values %s: %v", path, err)
	}
	var values struct {
		Env map[string]string `json:"env"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse chart values: %v", err)
	}
	env := values.Env

	if env["OTEL_ENABLED"] != "true" {
		t.Fatalf("chart env.OTEL_ENABLED = %q, want \"true\" (1.12.4 ships OTLP export on)", env["OTEL_ENABLED"])
	}
	// The sampler family is the C1 fix. parentbased_* would honour the
	// gateway's sampled=1 and span 100% of customer traffic.
	if got := env["OTEL_TRACES_SAMPLER"]; got != "traceidratio" {
		t.Fatalf("chart env.OTEL_TRACES_SAMPLER = %q, want \"traceidratio\" (NOT parentbased_*: the agent gateway samples 100%% and propagates sampled=1)", got)
	}
	// The ratio must ship WITH the enable flag: the SDK default is 100%.
	if got := env["OTEL_TRACES_SAMPLER_ARG"]; got == "" {
		t.Fatal("chart env.OTEL_TRACES_SAMPLER_ARG is unset — with no ratio the SDK samples 100%")
	}
	// No OTLP logs receiver on the collector agent: exporting into a void.
	if got := env["OTEL_LOGS_ENABLED"]; got != "false" {
		t.Fatalf("chart env.OTEL_LOGS_ENABLED = %q, want \"false\" (the collector's logs pipeline is filelog-only)", got)
	}
	// The endpoint must be the node-local agent, expanded by the Deployment.
	if got := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://$(HOST_IP):4318" {
		t.Fatalf("chart env.OTEL_EXPORTER_OTLP_ENDPOINT = %q, want the node-local agent http://$(HOST_IP):4318", got)
	}
	// Every value must be a string (installer plumbing is string-only) —
	// guaranteed by the map[string]string decode above: a YAML boolean or
	// number would have failed to unmarshal.
}
