// uaf_touched_sink.go — 1.12.3 A-1 / R-1: the OBSERVED-refilter Put-gate seam.
//
// WHY A SECOND MECHANISM (R-1, the blocker the red-team and the architect both
// found independently). A-1's first cut gated on a DECLARATION: "is the
// RESTAction being dispatched one that declares a userAccessFilter stage?". That
// closes the direct /call on the RA, but the portal does not render through it.
// It renders through WIDGETS: widgets.Resolve → resolveApiRef → apiref.Resolve →
// restactions.Resolve, and the UAF-refiltered rows land in the WIDGET's
// status.widgetData, Put under the widgets-class per-BINDING key. The declaring
// CR is the apiRef'd RESTAction, several frames below the widget the dispatcher
// holds — so the declaration predicate at the widget Put site sees nothing and
// the cell is written. Measured on the live cluster: 66 widgets apiRef a UAF RA,
// and that widgets cell served 298,064 hits against 365 misses over 5d7h. It is
// THE hot carrier; gating only the RA was gating the cold path.
//
// The same argument covers a subtler restactions case the declaration gate also
// misses: a NON-UAF RESTAction that NESTS a UAF one. Its own spec declares no
// userAccessFilter, but its resolved body still contains per-requester-narrowed
// rows.
//
// THE MECHANISM (mirrors StageErrorSink / ExternalTouchedSink exactly — the
// established idiom for "something happened DOWN the resolve that the Put site
// cannot see from its own frame"): the refilter bumps a sink threaded on the
// resolve ctx; every L1 Put site reads Count()>0 and DECLINES. Detection is at
// the SITE where the refilter actually runs, so it is authoritative regardless
// of how many resolver frames separate it from the Put — declaration-blind by
// construction, which is precisely the property the R-1 miss needed.
//
// DECLARATION AND SINK ARE BOTH KEPT, deliberately, and they are not redundant:
//   - the SINK is the general, transitive, always-correct signal, but it is
//     necessarily POST-resolve (you learn the refilter ran by running it);
//   - the DECLARATION is available PRE-resolve, which is what lets the boot seed
//     skip a UAF RA's whole fan-out instead of paying for a resolve whose output
//     it must then throw away.
// So the seed keeps its pre-resolve declaration skip and every Put site consults
// both. Either one firing declines.
//
// THE BUMP SITE lives in internal/resolvers/restactions/api/refilter.go, at the
// top of the LIVE refilter entry point (applyUserAccessFilterOnPig). BumpUAFTouched
// below exists so that site is a single line with no sink plumbing of its own —
// it is a no-op when no sink is installed, so it is safe on every path.

package cache

import (
	"context"
	"sync/atomic"
)

// ctxKeyUAFTouchedSinkType is the typed empty-struct context key used by
// WithUAFTouchedSink / UAFTouchedSinkFromContext. Distinct unexported type — no
// cross-package raw-string-key collision.
type ctxKeyUAFTouchedSinkType struct{}

var ctxKeyUAFTouchedSink = ctxKeyUAFTouchedSinkType{}

// UAFTouchedSink counts how many times a resolve under this context ran a
// userAccessFilter refilter. Count()>0 means "the bytes produced under this ctx
// are narrowed for THIS requester" → they MUST NOT be Put into any cell whose
// key does not separate requesters by their narrowing scope (which, before
// 1.13.0/v7, is every cell). Bumped from the errgroup workers of a multi-stage
// resolve, so all access is atomic.
type UAFTouchedSink struct {
	count atomic.Int64
}

// Bump records one userAccessFilter refilter execution. nil-receiver-safe so the
// bump site can call it unconditionally even when no sink is installed on ctx.
func (s *UAFTouchedSink) Bump() {
	if s == nil {
		return
	}
	s.count.Add(1)
}

// Count returns the number of refilter executions recorded. nil-receiver-safe
// (returns 0) so a Put site that finds no sink reads as "no refilter ran — Put
// as normal", which is the correct default for every non-UAF resolve.
func (s *UAFTouchedSink) Count() int64 {
	if s == nil {
		return 0
	}
	return s.count.Load()
}

// WithUAFTouchedSink returns a child context carrying a fresh *UAFTouchedSink,
// plus the sink itself. Installed by EVERY resolve entry that may Put an L1
// entry, alongside the stage-error and external-touched sinks it mirrors:
// restactions.go, widgets.go, resolve_populate.go (the refresher),
// phase1_pip_seed.go (both seedOneRestaction and seedOneWidget) and
// phase1_walk.go (the F2 walker, which feeds widget_content.go's populate). The
// apiref chokepoint and every nested resolve inherit it through ctx, which is
// exactly why it sees a refilter the Put site's own frame cannot.
//
// A resolve with no sink installed bumps a nil receiver (no-op) and Puts as
// before — byte-identical to pre-1.12.3 for every path not listed above.
func WithUAFTouchedSink(ctx context.Context) (context.Context, *UAFTouchedSink) {
	sink := &UAFTouchedSink{}
	if ctx == nil {
		return ctx, sink
	}
	return context.WithValue(ctx, ctxKeyUAFTouchedSink, sink), sink
}

// UAFTouchedSinkFromContext returns the *UAFTouchedSink attached to ctx by
// WithUAFTouchedSink, or nil when none is attached. A nil return MUST be treated
// by callers as "no refilter observed — Put as normal"; it is not an error. The
// sink's methods are nil-receiver-safe.
func UAFTouchedSinkFromContext(ctx context.Context) *UAFTouchedSink {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKeyUAFTouchedSink).(*UAFTouchedSink)
	return v
}

// BumpUAFTouched records one userAccessFilter refilter execution against
// whatever sink ctx carries. THIS IS THE ONE LINE THE REFILTER CALLS
// (internal/resolvers/restactions/api/refilter.go, at the top of the live
// applyUserAccessFilterOnPig entry point) — it keeps the resolver free of any
// sink plumbing and is a no-op when no sink is installed, so it is safe to call
// on every refilter path including tests and the dead twin.
func BumpUAFTouched(ctx context.Context) {
	UAFTouchedSinkFromContext(ctx).Bump()
}
