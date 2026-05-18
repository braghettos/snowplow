// nested_call_depth.go — Ship 0.30.123 (#155): the nested-/call recursion
// depth seam.
//
// In-process nested /call resolution (api/resolve.go's loopback branch ->
// dispatchers.ResolveNestedCall -> restactions.Resolve) can recurse: the
// inner RESTAction may itself have a /call-loopback stage. A RESTAction
// that /call's itself — or a cycle A->B->A — would recurse unboundedly
// and overflow the stack.
//
// WithNestedCallDepth threads a depth counter down the resolve context.
// ResolveNestedCall reads it; at nestedCallMaxDepth it returns a bounded
// ERROR ("nested /call depth limit exceeded") — never empty, never a
// panic — and otherwise increments the depth on the context it passes
// into the nested restactions.Resolve. No visited-set: a legitimate DAG
// may /call the same RESTAction twice on independent branches, so depth
// alone bounds cycles without rejecting valid graphs.
//
// Mirrors WithL1KeyContext / L1KeyFromContext (deps.go): a distinct
// unexported empty-struct key so external packages cannot collide via a
// raw string key.

package cache

import "context"

// nestedCallMaxDepth caps how deep an in-process nested-/call chain may
// recurse before ResolveNestedCall fails the call with a bounded error.
// 8 is well above any legitimate RESTAction /call nesting (the deepest
// observed customer graph is 2-3) and low enough that a self-referential
// cycle terminates near-instantly with no stack pressure.
const nestedCallMaxDepth = 8

// ctxKeyNestedCallDepthType is the typed empty-struct context key used
// by WithNestedCallDepth / NestedCallDepthFromContext. Distinct
// unexported type — no cross-package raw-string-key collision.
type ctxKeyNestedCallDepthType struct{}

var ctxKeyNestedCallDepth = ctxKeyNestedCallDepthType{}

// WithNestedCallDepth returns a child context carrying depth as the
// current nested-/call recursion depth. ResolveNestedCall increments the
// depth read off the inbound context and threads the result into the
// nested restactions.Resolve, so each loopback hop adds 1.
func WithNestedCallDepth(ctx context.Context, depth int) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyNestedCallDepth, depth)
}

// NestedCallDepthFromContext returns the nested-/call recursion depth
// attached to ctx by WithNestedCallDepth. Returns 0 when no depth was
// attached.
//
// Depth semantics (the single source of truth — ResolveNestedCall's
// Step-1 comment matches this):
//   - the outermost request-path resolve carries NO marker, so it reads
//     depth 0;
//   - its FIRST nested /call enters ResolveNestedCall reading depth 0,
//     proceeds, and resolves the inner RESTAction under depth 0+1 = 1;
//   - hop N reads depth N-1;
//   - the guard `depth >= nestedCallMaxDepth` rejects the hop that reads
//     depth == nestedCallMaxDepth. With nestedCallMaxDepth = 8 the hops
//     reading depth 0..7 all proceed — exactly 8 nested hops execute —
//     and the 9th (reading depth 8) is the terminating bounded error.
func NestedCallDepthFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	d, _ := ctx.Value(ctxKeyNestedCallDepth).(int)
	return d
}

// NestedCallMaxDepth exposes the recursion cap so the dispatchers
// package (ResolveNestedCall) and tests can reference the single
// canonical bound rather than re-declaring it.
func NestedCallMaxDepth() int {
	return nestedCallMaxDepth
}
