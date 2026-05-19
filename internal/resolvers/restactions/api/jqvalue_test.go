// jqvalue_test.go — Ship A (0.30.137) tests for EvalValue and the three
// migrated call sites.
//
// Coverage map (design §5 + AC-A.3 / AC-A.6 / AC-A.7):
//
//   - TestEvalValue_OutcomeContract — the §1 (value, ok, err) contract:
//     parse / compile / runtime-error / zero-yield / single / multi-yield.
//   - TestEvalValue_OutputEquivalence — §3.1-3.3 representative expression
//     set: integers (incl. > 2^53), floats, negative/zero, unicode/escape
//     strings, bools, nulls, arrays, nested objects, the {key,slice} pig
//     shape. Asserts the int->float64 collapse is invisible post-Marshal.
//   - TestSite1_HandlerFilter_TruthTable     — handler.go:95 (AC-A.3).
//   - TestSite2_ResolveUAFResources_TruthTable — refilter.go resolveUAFResources.
//   - TestSite3_EvalJQString_TruthTable       — refilter.go evalJQString,
//     incl. the §3.4.5 RATIFIED deliberate change (multi-yield -> error).
//   - TestSite3_NamespaceFrom_Consumer       — the evalSingle/NamespaceFrom
//     RBAC consumer (§3.4.6) — multi-yield fails the item closed.
//   - TestEvalValue_AliasingConcurrency_Race — AC-A.6 -race obligation.
//
// No build tag: these run with the default `go test` (and `-race`).

package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"sync"
	"testing"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// jsonEq reports whether a and b serialise to byte-identical JSON — the
// widget-prop boundary equivalence the design proves (the final downstream
// json.Marshal). Used to assert the int->float64 collapse is invisible.
func jsonEq(t *testing.T, a, b any) bool {
	t.Helper()
	aj, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bj, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	return string(aj) == string(bj)
}

// --- EvalValue outcome contract (AC-A.2) ----------------------------------

func TestEvalValue_OutcomeContract(t *testing.T) {
	type want struct {
		value      any
		ok         bool
		errNil     bool // err == nil
		errMulti   bool // errors.Is(err, ErrMultiYield)
		errOther   bool // err != nil && !ErrMultiYield
	}
	cases := []struct {
		name  string
		query string
		data  any
		want  want
	}{
		{
			name:  "single value",
			query: ".foo",
			data:  map[string]any{"foo": "bar"},
			want:  want{value: "bar", ok: true, errNil: true},
		},
		{
			name:  "zero-yield (jq empty)",
			query: "empty",
			data:  map[string]any{"foo": "bar"},
			want:  want{value: nil, ok: false, errNil: true},
		},
		{
			name:  "multi-yield",
			query: ".items[]",
			data:  map[string]any{"items": []any{1.0, 2.0, 3.0}},
			want:  want{value: nil, ok: false, errMulti: true},
		},
		{
			name:  "parse error",
			query: ".foo |",
			data:  map[string]any{"foo": "bar"},
			want:  want{value: nil, ok: false, errOther: true},
		},
		{
			name:  "compile error (unbound function)",
			query: "nonexistent_function_xyz",
			data:  map[string]any{},
			want:  want{value: nil, ok: false, errOther: true},
		},
		{
			name:  "runtime error-value (divide by zero)",
			query: ".a / .b",
			data:  map[string]any{"a": 1.0, "b": 0.0},
			want:  want{value: nil, ok: false, errOther: true},
		},
		{
			name:  "runtime error-value (field of a number)",
			query: ".foo.bar",
			data:  map[string]any{"foo": 5.0},
			want:  want{value: nil, ok: false, errOther: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok, err := EvalValue(context.Background(), tc.query, tc.data, nil)
			if ok != tc.want.ok {
				t.Errorf("ok = %v; want %v", ok, tc.want.ok)
			}
			if tc.want.errNil && err != nil {
				t.Errorf("err = %v; want nil", err)
			}
			if tc.want.errMulti && !errors.Is(err, ErrMultiYield) {
				t.Errorf("err = %v; want ErrMultiYield", err)
			}
			if tc.want.errOther {
				if err == nil {
					t.Errorf("err = nil; want a non-nil non-ErrMultiYield error")
				} else if errors.Is(err, ErrMultiYield) {
					t.Errorf("err = ErrMultiYield; want a parse/compile/runtime error")
				}
			}
			if tc.want.ok && !jsonEq(t, v, tc.want.value) {
				t.Errorf("value = %#v; want %#v", v, tc.want.value)
			}
			if !tc.want.ok && v != nil {
				t.Errorf("value = %#v; want nil on non-ok outcome", v)
			}
		})
	}
}

// --- §3.1-3.3 output equivalence (AC-A.7) ---------------------------------

func TestEvalValue_OutputEquivalence(t *testing.T) {
	cases := []struct {
		name  string
		query string
		data  any
		// wantMarshal is the expected JSON after the final downstream
		// json.Marshal — the widget-prop boundary the design proves
		// equivalence at (§3.6).
		wantMarshal string
	}{
		{"identity object", ".", map[string]any{"a": 1.0}, `{"a":1}`},
		{"string field", ".s", map[string]any{"s": "hello"}, `"hello"`},
		{"unicode string", ".s", map[string]any{"s": "héllo-☃"}, `"héllo-☃"`},
		{"escape string", ".s", map[string]any{"s": "tab\there\nline"}, `"tab\there\nline"`},
		{"bool true", ".b", map[string]any{"b": true}, `true`},
		{"bool false", ".b", map[string]any{"b": false}, `false`},
		{"null", ".n", map[string]any{"n": nil}, `null`},
		// Integral arithmetic — gojq yields int; json.Marshal of int and
		// of float64 both render the integer identically (§3.1).
		{"integer from length", ".items | length", map[string]any{"items": []any{1.0, 2.0, 3.0}}, `3`},
		{"integer arithmetic", ".a + .b", map[string]any{"a": 2.0, "b": 3.0}, `5`},
		{"negative integer", ".a - .b", map[string]any{"a": 2.0, "b": 5.0}, `-3`},
		{"zero", ".a - .a", map[string]any{"a": 5.0}, `0`},
		// Fractional float — both paths carry float64.
		{"fractional float", ".a / .b", map[string]any{"a": 1.0, "b": 4.0}, `0.25`},
		{"nested object", "{x: .a, y: {z: .b}}", map[string]any{"a": 1.0, "b": "q"}, `{"x":1,"y":{"z":"q"}}`},
		{"array projection", "[.items[].name]", map[string]any{"items": []any{
			map[string]any{"name": "a"}, map[string]any{"name": "b"},
		}}, `["a","b"]`},
		// The {opts.key, "slice"} pig shape (§3.5).
		{"pig shape passthrough", ".result", map[string]any{
			"result": map[string]any{"foo": "bar"}, "slice": "ignored",
		}, `{"foo":"bar"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok, err := EvalValue(context.Background(), tc.query, tc.data, nil)
			if err != nil || !ok {
				t.Fatalf("EvalValue(%q) = (%#v,%v,%v); want a single value", tc.query, v, ok, err)
			}
			got, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if string(got) != tc.wantMarshal {
				t.Errorf("Marshal(EvalValue(%q)) = %s; want %s", tc.query, got, tc.wantMarshal)
			}
		})
	}
}

// TestEvalValue_BigInteger asserts the §3.1 finding: for an integer beyond
// float64's 2^53 exact range, the direct path is strictly MORE correct than
// the round-trip path. The pre-Ship-A round-trip ended with json.Unmarshal
// (no UseNumber) collapsing every JSON number to float64, losing precision;
// gojq's integer arithmetic keeps the result exact (int or *big.Int) and the
// direct path Marshals it exactly.
func TestEvalValue_BigInteger(t *testing.T) {
	// 2^53 + 1 — the smallest integer float64 cannot represent exactly.
	const bigStr = "9007199254740993"

	// Premise check: a float64 round-trip through json.Unmarshal mangles
	// this — exactly the precision loss the round-trip path incurred.
	var asFloat any
	if err := json.Unmarshal([]byte(bigStr), &asFloat); err != nil {
		t.Fatalf("unmarshal premise: %v", err)
	}
	roundTripped, _ := json.Marshal(asFloat)
	if string(roundTripped) == bigStr {
		t.Fatalf("test premise broken: float64 round-trip preserves %s", bigStr)
	}

	// gojq integer arithmetic keeps it exact — the direct path.
	v, ok, err := EvalValue(context.Background(), "9007199254740992 + 1", nil, nil)
	if err != nil || !ok {
		t.Fatalf("EvalValue big-int = (%#v,%v,%v)", v, ok, err)
	}
	// gojq yields an integer type (int or *big.Int) — not float64.
	switch v.(type) {
	case int, *big.Int:
		// expected
	default:
		t.Errorf("big-int result type = %T; want int or *big.Int (not float64)", v)
	}
	got, _ := json.Marshal(v)
	if string(got) != bigStr {
		t.Errorf("big integer Marshal = %s; want %s (direct path must be exact)", got, bigStr)
	}
}

// TestEvalValue_NaNInputBoundary marks the §6 falsifier boundary: JSON has
// no NaN/Inf literal, so an apiserver envelope decode never produces one;
// jq arithmetic over JSON inputs raises an error (caught as err) rather than
// yielding NaN. The encoder's NaN->null mapping is therefore unreachable on
// valid inputs. This test documents that 1/0 is an ERROR, not a NaN yield.
func TestEvalValue_NaNInputBoundary(t *testing.T) {
	_, ok, err := EvalValue(context.Background(), ".a / .b",
		map[string]any{"a": 1.0, "b": 0.0}, nil)
	if ok {
		t.Errorf("1/0 yielded a value; want a jq runtime error")
	}
	if err == nil {
		t.Errorf("1/0 err = nil; want a jq runtime error (no NaN reaches the encoder)")
	}
}

// --- Site #1: handler.go:95 truth table (AC-A.3 / §3.4.2) -----------------
//
// Drives jsonHandlerCore directly. jsonHandlerCore wraps the decoded input
// under opts.key into `pig` (pig := {key: input}) and evaluates the filter
// against `pig` — so every filter expression here is written against the
// pig shape, e.g. `.result.foo` not `.foo`. The truth table asserts the
// migrated caller's (side-effect on out, error) per gojq outcome.

func TestSite1_HandlerFilter_TruthTable(t *testing.T) {
	const key = "result"
	cases := []struct {
		name    string
		filter  string
		input   any
		wantErr bool   // jsonHandlerCore returns non-nil error -> stage FAILS
		wantOut string // JSON of out[key] when no error
	}{
		{
			name:    "single value -> stage continues, tmp updated",
			filter:  ".result.foo",
			input:   map[string]any{"foo": "bar"},
			wantErr: false,
			wantOut: `"bar"`,
		},
		{
			name:    "zero-yield -> stage FAILS",
			filter:  "empty",
			input:   map[string]any{"foo": "bar"},
			wantErr: true,
		},
		{
			name:    "multi-yield -> stage FAILS",
			filter:  ".result.items[]",
			input:   map[string]any{"items": []any{1.0, 2.0}},
			wantErr: true,
		},
		{
			name:    "parse error -> log + stage CONTINUES, tmp unchanged",
			filter:  ".result |",
			input:   map[string]any{"foo": "bar"},
			wantErr: false,
			// tmp unchanged: out[key] is the wrapped original input.
			wantOut: `{"foo":"bar"}`,
		},
		{
			name:    "runtime error -> log + stage CONTINUES, tmp unchanged",
			filter:  ".result.foo.bar",
			input:   map[string]any{"foo": 5.0},
			wantErr: false,
			wantOut: `{"foo":5}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]any{}
			f := tc.filter
			opts := jsonHandlerOptions{key: key, out: out, filter: &f}
			err := jsonHandlerCore(context.Background(), opts, tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("jsonHandlerCore err = %v; wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			got, hasKey := out[key]
			if !hasKey {
				t.Fatalf("out[%q] missing", key)
			}
			gj, _ := json.Marshal(got)
			if string(gj) != tc.wantOut {
				t.Errorf("out[%q] = %s; want %s", key, gj, tc.wantOut)
			}
		})
	}
}

// --- Site #2: resolveUAFResources truth table (AC-A.3 / §3.4.3) -----------
//
// Fail-closed security path: every non-single-array outcome -> (nil,false).

func TestSite2_ResolveUAFResources_TruthTable(t *testing.T) {
	log := discardLogger()
	cases := []struct {
		name      string
		expr      string
		dict      map[string]any
		wantOK    bool
		wantSet   []string
	}{
		{
			name:    "single value, []any of strings -> set",
			expr:    ".plurals",
			dict:    map[string]any{"plurals": []any{"compaaa", "compbbb"}},
			wantOK:  true,
			wantSet: []string{"compaaa", "compbbb"},
		},
		{
			name:   "single value, not []any -> fail-closed",
			expr:   ".plurals",
			dict:   map[string]any{"plurals": "compaaa"},
			wantOK: false,
		},
		{
			name:   "zero-yield -> fail-closed",
			expr:   "empty",
			dict:   map[string]any{"plurals": []any{"compaaa"}},
			wantOK: false,
		},
		{
			name:   "multi-yield -> fail-closed",
			expr:   ".plurals[]",
			dict:   map[string]any{"plurals": []any{"compaaa", "compbbb"}},
			wantOK: false,
		},
		{
			name:   "parse error -> fail-closed",
			expr:   ".plurals |",
			dict:   map[string]any{"plurals": []any{"compaaa"}},
			wantOK: false,
		},
		{
			name:   "runtime error -> fail-closed",
			expr:   ".plurals.deep.deeper",
			dict:   map[string]any{"plurals": 5.0},
			wantOK: false,
		},
		{
			name:   "non-string element -> fail-closed",
			expr:   ".plurals",
			dict:   map[string]any{"plurals": []any{"compaaa", 7.0}},
			wantOK: false,
		},
		{
			name:    "empty array -> ([], true) — valid 'no resources'",
			expr:    ".plurals",
			dict:    map[string]any{"plurals": []any{}},
			wantOK:  true,
			wantSet: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uaf := &templates.UserAccessFilterSpec{ResourcesFrom: tc.expr}
			got, ok := resolveUAFResources(context.Background(), log, uaf, tc.dict)
			if ok != tc.wantOK {
				t.Fatalf("resolveUAFResources ok = %v; want %v (set=%v)", ok, tc.wantOK, got)
			}
			if !ok {
				if got != nil {
					t.Errorf("fail-closed must return nil slice; got %v", got)
				}
				return
			}
			if !jsonEq(t, got, tc.wantSet) {
				t.Errorf("set = %v; want %v", got, tc.wantSet)
			}
		})
	}
}

// --- Site #3: evalJQString truth table (AC-A.3 / §3.4.4 / §3.4.5) ---------

func TestSite3_EvalJQString_TruthTable(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		data     any
		want     string
		wantErr  bool
		// deliberateChange tags the §3.4.5 RATIFIED multi-yield row.
		deliberateChange bool
	}{
		{
			name: "single value, string -> verbatim string",
			expr: ".metadata.name",
			data: map[string]any{"metadata": map[string]any{"name": "ns-01"}},
			want: "ns-01",
		},
		{
			name: "single value, null -> empty string",
			expr: ".missing",
			data: map[string]any{"metadata": map[string]any{"name": "ns-01"}},
			want: "",
		},
		{
			name: "single value, non-string (number) -> JSON serialisation verbatim",
			expr: ".count",
			data: map[string]any{"count": 5.0},
			want: "5",
		},
		{
			name: "single value, non-string (bool) -> JSON serialisation verbatim",
			expr: ".flag",
			data: map[string]any{"flag": true},
			want: "true",
		},
		{
			name: "zero-yield -> empty string, no error",
			expr: "empty",
			data: map[string]any{"metadata": map[string]any{"name": "ns-01"}},
			want: "",
		},
		{
			name:             "multi-yield -> ERROR (RATIFIED deliberate change §3.4.5)",
			expr:             ".items[]",
			data:             map[string]any{"items": []any{"a", "b"}},
			wantErr:          true,
			deliberateChange: true,
		},
		{
			name:    "parse error -> error",
			expr:    ".metadata.name |",
			data:    map[string]any{"metadata": map[string]any{"name": "ns-01"}},
			wantErr: true,
		},
		{
			name:    "runtime error -> error",
			expr:    ".metadata.name.deeper",
			data:    map[string]any{"metadata": map[string]any{"name": "ns-01"}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalJQString(context.Background(), tc.expr, tc.data)
			if (err != nil) != tc.wantErr {
				t.Fatalf("evalJQString err = %v; wantErr %v", err, tc.wantErr)
			}
			if tc.deliberateChange && !errors.Is(err, ErrMultiYield) {
				t.Errorf("§3.4.5 deliberate change: err = %v; want ErrMultiYield", err)
			}
			if tc.wantErr {
				if got != "" {
					t.Errorf("evalJQString on error returned %q; want \"\"", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("evalJQString = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestSite3_NamespaceFrom_Consumer covers §3.4.6 — the SECOND evalJQString
// consumer, evalSingle's UAF NamespaceFrom RBAC path. A multi-yield
// NamespaceFrom expression now fails the item CLOSED (returns false) instead
// of computing a namespace from concatenated garbage — strictly safer.
//
// evalSingle returns false on a NamespaceFrom error before any EvaluateRBAC
// call, so this needs no RBAC fixture.
func TestSite3_NamespaceFrom_Consumer(t *testing.T) {
	log := discardLogger()
	item := map[string]any{"items": []any{"a", "b"}}

	t.Run("multi-yield NamespaceFrom -> item denied (fail-closed)", func(t *testing.T) {
		uaf := &templates.UserAccessFilterSpec{
			NamespaceFrom: ".items[]", // multi-yield
			Verb:          "list",
			Resource:      "compositions",
		}
		permitted := evalSingle(context.Background(), log, "alice", nil, uaf,
			[]string{"compositions"}, item)
		if permitted {
			t.Errorf("multi-yield NamespaceFrom permitted the item; want denied (fail-closed)")
		}
	})

	t.Run("parse-error NamespaceFrom -> item denied (fail-closed)", func(t *testing.T) {
		uaf := &templates.UserAccessFilterSpec{
			NamespaceFrom: ".name |",
			Verb:          "list",
			Resource:      "compositions",
		}
		permitted := evalSingle(context.Background(), log, "alice", nil, uaf,
			[]string{"compositions"}, item)
		if permitted {
			t.Errorf("parse-error NamespaceFrom permitted the item; want denied")
		}
	})
}

// --- AC-A.6: aliasing / concurrency obligation ----------------------------
//
// THE FINDING (verified by this test + the gojq source read):
//
// gojq's Code.RunWithContext calls normalizeNumbers(input)
// (gojq@v0.12.17/compiler.go:52 -> normalize.go:71-80), which walks the
// input tree and WRITES BACK into every map/slice IN PLACE — `v[k] =
// normalizeNumbers(x)`. So EvalValue, exactly like jqutil.Eval before it
// (jqutil.go:40 RunWithContext — the SAME call), MUTATES its input tree.
// This is NOT a Ship A regression: the round-trip Ship A removes was on the
// OUTPUT (encode->Unmarshal); the input-mutation predates Ship A and is
// byte-identical pre/post. It is already documented as the cause of the
// 0.30.128/0.30.129 crash and fixed by apistage.go:227's DeepCopyJSON.
//
// AC-A.6 DISCHARGE — the input is per-call-PRIVATE at every migrated site:
//
//   Site #1 handler.go: `pig` is a fresh map built per call (handler.go
//     pig := map[string]any{...}); its `tmp` value comes from
//     listEnvelopeValue -> maps.DeepCopyJSON (apistage.go:227) — a private
//     deep copy. EvalValue mutating `pig`/`tmp` touches only this call's
//     private tree.
//   Sites #2/#3 refilter.go: `dict` is a fresh map per Resolve call
//     (resolve.go:205 `dict := map[string]any{}`, or DeepCopyJSON of Extras
//     at :207). applyUserAccessFilter — hence resolveUAFResources and
//     evalSingle->evalJQString — runs in the SEQUENTIAL `for _, id := range
//     names` stage loop (resolve.go:248, call at :785), AFTER the stage's
//     parallel g.Go workers have completed (g.Wait). No second goroutine
//     shares `dict` at refilter time.
//
// AC-A.6 DISCHARGE — the OUTPUT (the genuinely-new Ship A surface) is
// consumed strictly READ-ONLY and never enters a shared cache:
//
//   Site #1 handler.go: `tmp = v` — a pointer assignment into this call's
//     private `pig`/`out`. `out` is dict, per-call-private. No mutation of
//     v; no shared cache. (The apistage CONTENT cache is keyed and Put
//     upstream of the filter, on the raw envelope — not the filter output.)
//   Site #2 refilter.go: `v.([]any)` then `for _, e := range arr` — a
//     read-only range; the loop only reads `e.(string)`. arr is never
//     mutated, never cached.
//   Site #3 refilter.go: `switch s := v.(type)` — read-only type switch;
//     returns the string / "" / encodeValueCompact(s). No mutation, no cache.
//
// Conclusion: the input-mutation hazard is real but pre-existing and
// confined to per-call-private trees at all 3 sites; the new output
// aliasing is consumed read-only. Ship A introduces no new concurrency
// hazard. The test below CONFIRMS the discharge under `go test -race`:
// each goroutine evaluates over its OWN private input copy (mirroring
// apistage.go:227) and consumes the result read-only — modelling the
// production invariant. It must pass clean under -race.
func TestEvalValue_AliasingConcurrency_Race(t *testing.T) {
	// The shared TEMPLATE — never passed to EvalValue directly; each
	// goroutine deep-copies it, exactly as apistage.go:227 hands each
	// resolver a private DeepCopyJSON envelope.
	template := map[string]any{
		"metadata": map[string]any{
			"name":      "shared-obj",
			"namespace": "krateo-system",
			"labels":    map[string]any{"app": "snowplow", "tier": "cache"},
		},
		"items": []any{
			map[string]any{"name": "a", "n": 1.0},
			map[string]any{"name": "b", "n": 2.0},
			map[string]any{"name": "c", "n": 3.0},
		},
		"slice": map[string]any{"limit": 50.0},
	}
	queries := []string{
		".",                    // identity — aliases the WHOLE private input
		".metadata",            // aliases a sub-map of the private input
		".metadata.name",       // leaf string
		".items",               // aliases the items slice
		"[.items[].name]",      // builds a fresh array (no alias)
		".metadata.labels.app", // deep leaf
		".items | length",      // integer result
	}

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				// Per-call PRIVATE input copy — the apistage.go:227 /
				// resolve.go:205 invariant. gojq's normalizeNumbers may
				// mutate this, but it is owned solely by this goroutine.
				priv := deepCopyAny(template).(map[string]any)
				q := queries[(g+i)%len(queries)]
				v, ok, err := EvalValue(context.Background(), q, priv, nil)
				if err != nil {
					t.Errorf("EvalValue(%q) err = %v", q, err)
					return
				}
				if !ok {
					t.Errorf("EvalValue(%q) ok = false", q)
					return
				}
				// Read-only consumption of the (possibly aliasing) result —
				// exactly what the 3 migrated callers do: assign it
				// (handler.go tmp=v), type-assert + range-read it
				// (resolveUAFResources), or type-switch it (evalJQString).
				// A read-only json.Marshal models that read-only use.
				_, _ = json.Marshal(v)
			}
		}(g)
	}
	wg.Wait()
}

// deepCopyAny recursively copies a JSON-decoded value tree (map/slice/
// scalars) — the test-local equivalent of maps.DeepCopyJSON, used to model
// the per-call-private input each migrated caller actually feeds EvalValue.
func deepCopyAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		c := make(map[string]any, len(t))
		for k, x := range t {
			c[k] = deepCopyAny(x)
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, x := range t {
			c[i] = deepCopyAny(x)
		}
		return c
	default:
		return v
	}
}
