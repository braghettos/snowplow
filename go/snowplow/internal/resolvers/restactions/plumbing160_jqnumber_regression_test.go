package restactions

import (
	"context"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/jqutil"
	jqsupport "github.com/krateo-platformops/snowplow/internal/support/jq"
)

// TestPlumbing160_FromJSONNumber_NoPanic guards the issue #160 durable fix that
// snowplow consumes via the plumbing v1.14.2 bump.
//
// A RESTAction whose `filter` uses gojq `fromjson` returns the parsed object,
// and gojq parses with UseNumber() — so numeric literals inside are json.Number.
// plumbing's jqutil encoder had NO `case json.Number` before v1.14.2, so it hit
// the `default` branch and PANICKED (`invalid type: json.Number`). net/http
// recovers per-connection, so the pod stays up (restartCount 0) but the socket
// closes with zero bytes written → browser "Failed to fetch" / curl exit 52.
// It bit the portal composition Edit form on essentially every real blueprint
// (first observed value: .properties.service.properties.nodePort.maximum=32767).
//
// This exercises the exact call shape of restactions.Resolve (restactions.go:65):
// jqutil.Eval over a dict with a fromjson filter. If plumbing is ever downgraded
// below v1.14.2 (missing the encoder's `case json.Number`), this reds/panics.
func TestPlumbing160_FromJSONNumber_NoPanic(t *testing.T) {
	// A top-level OBJECT from fromjson: InferType only normalizes a top-level
	// SCALAR json.Number, so the nested numbers below reach the encoder unchanged
	// — the exact bug path.
	data := map[string]any{
		"raw": `{"maximum":32767,"nested":{"nodePort":30080,"ratio":1.5},"name":"x"}`,
	}

	out, err := jqutil.Eval(context.Background(), jqutil.EvalOptions{
		Query:        `.raw | fromjson`,
		Data:         data,
		ModuleLoader: jqsupport.ModuleLoader(),
	})
	if err != nil {
		t.Fatalf("fromjson-with-numbers must resolve cleanly (plumbing >= v1.14.2); got err: %v", err)
	}

	for _, want := range []string{"32767", "30080", "1.5", `"name"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("resolved output missing %q — encoder dropped/mangled a json.Number; got: %s", want, out)
		}
	}
}
