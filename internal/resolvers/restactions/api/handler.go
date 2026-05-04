package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var apiHandlerTracer = otel.Tracer("snowplow/resolvers/restactions/api")

type jsonHandlerOptions struct {
	key    string
	out    map[string]any
	filter *string
}

// ---------------------------------------------------------------------------
// V0_HOIST split: Compute (no lock) + Merge (under lock)
// ---------------------------------------------------------------------------
//
// Per architect's hoist design v2.1 § JQ hoist (file
// /tmp/snowplow-runs/dictmu-microbench-design-2026-05-03.md), the
// json.Unmarshal + jq.Eval + final-unmarshal steps are pure functions of
// the input bytes/data and the optional `slice` value previously read
// from the dict. They are hoisted out of the dictMu critical section.
// Only the read-modify-write into out[opts.key] stays under the lock.
//
// gojq purity discipline (audit at /tmp/snowplow-runs/gojq-purity-audit-2026-05-03.md):
// every Compute that originates from informer storage MUST be preceded
// by safeCopyJSON at the call site. The Direct entry point assumes data
// is already a JQ-safe independent copy. The hoist preserves that
// invariant — see the `// gojq-purity-required` markers in resolve.go.

// jsonHandlerCompute parses raw bytes, runs the optional JQ filter, and
// returns the value to merge into the dict. It is safe to call WITHOUT
// holding dictMu; it does NOT read from or write to opts.out.
//
// gojq-purity-required: callers passing tree input must ensure
// safeCopyJSON has been invoked at the call site (raw byte parsing is
// always safe; JQ on []byte produces a fresh tree).
func jsonHandlerCompute(ctx context.Context, opts jsonHandlerOptions, raw []byte, slice any) (any, error) {
	log := xcontext.Logger(ctx)

	_, unmarshalSpan := apiHandlerTracer.Start(ctx, "restaction.api.cache_unmarshal",
		trace.WithAttributes(
			attribute.String("key", opts.key),
			attribute.Int("bytes", len(raw)),
		))
	var tmp any
	if err := json.Unmarshal(raw, &tmp); err != nil {
		unmarshalSpan.End()
		return nil, err
	}
	unmarshalSpan.End()

	if opts.filter == nil {
		return tmp, nil
	}

	pig := map[string]any{opts.key: tmp}
	if slice != nil {
		pig["slice"] = slice
	}

	q := ptr.Deref(opts.filter, "")
	log.Debug("found local filter on api result", slog.String("filter", q))
	_, jqSpan := apiHandlerTracer.Start(ctx, "restaction.api.jq_eval",
		trace.WithAttributes(
			attribute.String("api.call.key", opts.key),
			attribute.Int("api.call.input_bytes", len(raw)),
			attribute.Int("api.call.filter_len", len(q)),
		))
	// gojq-purity-required: raw bytes were just unmarshaled into tmp;
	// tmp is owned by this goroutine and gojq is free to mutate it.
	s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
		Query: q, Data: pig,
		ModuleLoader: jqsupport.ModuleLoader(),
	})
	if err == nil {
		jqSpan.SetAttributes(attribute.Int("api.call.output_bytes", len(s)))
	}
	jqSpan.End()
	if err != nil {
		log.Error("unable to evaluate JQ filter",
			slog.String("filter", q), slog.Any("error", err))
		return tmp, nil
	}
	if err := json.Unmarshal([]byte(s), &tmp); err != nil {
		return nil, err
	}
	return tmp, nil
}

// jsonHandlerDirectCompute is the zero-copy equivalent of
// jsonHandlerCompute for data already in Go memory (e.g., from the
// informer store). Skips json.Unmarshal of raw bytes; the JQ pass and
// final unmarshal are hoisted exactly as in jsonHandlerCompute.
//
// gojq-purity-required: `data` MUST already be safeCopyJSON-wrapped at
// the call site. Pass an aliased informer tree at your peril.
func jsonHandlerDirectCompute(ctx context.Context, opts jsonHandlerOptions, data any, slice any) (any, error) {
	log := xcontext.Logger(ctx)

	tmp := data
	if opts.filter == nil {
		return tmp, nil
	}

	pig := map[string]any{opts.key: data}
	if slice != nil {
		pig["slice"] = slice
	}

	q := ptr.Deref(opts.filter, "")
	log.Debug("found local filter on api result", slog.String("filter", q))
	_, jqSpan := apiHandlerTracer.Start(ctx, "restaction.api.jq_eval",
		trace.WithAttributes(
			attribute.String("api.call.key", opts.key),
			attribute.Int("api.call.filter_len", len(q)),
		))
	// gojq-purity-required: data must be a safeCopyJSON-wrapped tree.
	s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
		Query: q, Data: pig,
		ModuleLoader: jqsupport.ModuleLoader(),
	})
	if err == nil {
		jqSpan.SetAttributes(attribute.Int("api.call.output_bytes", len(s)))
	}
	jqSpan.End()
	if err != nil {
		log.Error("unable to evaluate JQ filter",
			slog.String("filter", q), slog.Any("error", err))
		return tmp, nil
	}
	if err := json.Unmarshal([]byte(s), &tmp); err != nil {
		return nil, err
	}
	return tmp, nil
}

// mergeIntoDict merges a computed value into out[key] using the
// production semantics of jsonHandler / jsonHandlerDirect. It MUST run
// under the caller's dictMu critical section.
//
// Semantics (preserved from pre-hoist handler.go lines 81–103):
//   - If out[key] is unset: store as-is.
//   - If out[key] is []any: append wrapAsSlice(val).
//   - Otherwise: produce []any{prev, val} (or expanded if val is []any).
func mergeIntoDict(out map[string]any, key string, val any) {
	got, ok := out[key]
	if !ok {
		out[key] = val
		return
	}

	switch existingSlice := got.(type) {
	case []any:
		if v := wrapAsSlice(val); len(v) > 0 {
			out[key] = append(existingSlice, v...)
		}
	default:
		switch v := val.(type) {
		case []any:
			all := []any{got}
			all = append(all, v...)
			out[key] = all
		default:
			out[key] = []any{got, v}
		}
	}
}

// ---------------------------------------------------------------------------
// Legacy under-lock handlers (kept for any non-hoisted call site, e.g.
// the snapshot read at line 236 which is partial-hoist only — see
// resolve.go).
// ---------------------------------------------------------------------------

func jsonHandler(ctx context.Context, opts jsonHandlerOptions) func(io.ReadCloser) error {
	return func(in io.ReadCloser) error {
		dat, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		var slice any
		if si, ok := opts.out["slice"]; ok {
			slice = si
		}
		// gojq-purity-required: raw bytes are owned by this call.
		val, cerr := jsonHandlerCompute(ctx, opts, dat, slice)
		if cerr != nil {
			return cerr
		}
		mergeIntoDict(opts.out, opts.key, val)
		return nil
	}
}

// jsonHandlerDirect is the zero-copy equivalent of jsonHandler for data
// that is already in Go memory (e.g., from the informer store).
//
// gojq-purity-required: `data` MUST already be safeCopyJSON-wrapped.
func jsonHandlerDirect(ctx context.Context, opts jsonHandlerOptions, data any) error {
	var slice any
	if si, ok := opts.out["slice"]; ok {
		slice = si
	}
	val, cerr := jsonHandlerDirectCompute(ctx, opts, data, slice)
	if cerr != nil {
		return cerr
	}
	mergeIntoDict(opts.out, opts.key, val)
	return nil
}

// normalizeForJQ recursively converts types that gojq cannot handle
// (e.g., int64 from K8s unstructured) to JSON-compatible types (float64).
// Returns a new tree — does NOT modify the input (safe for informer data).
func normalizeForJQ(v any) any {
	switch val := v.(type) {
	case map[string]interface{}:
		cp := make(map[string]interface{}, len(val))
		for k, vv := range val {
			cp[k] = normalizeForJQ(vv)
		}
		return cp
	case []interface{}:
		cp := make([]interface{}, len(val))
		for i, vv := range val {
			cp[i] = normalizeForJQ(vv)
		}
		return cp
	case int64:
		return float64(val)
	case int32:
		return float64(val)
	case int:
		return float64(val)
	default:
		return v
	}
}

func wrapAsSlice(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	default:
		return []any{v}
	}
}
