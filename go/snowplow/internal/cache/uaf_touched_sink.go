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
// THE TWO BUMP SITES, and their status as of this commit:
//
//  1. DECLARATION-BASED, PRESENT on this branch — apiref.Resolve
//     (internal/resolvers/widgets/apiref/resolve.go, bumpUAFSinkIfDeclared) fires
//     when the apiRef'd RESTAction DECLARES a userAccessFilter. This is the
//     apiRef chokepoint every widget→RESTAction read funnels through, and the
//     first frame holding the typed RA. It closes the R-1 widgets carrier.
//
//  2. EXECUTION-BASED, NOT YET PRESENT HERE — cache.BumpUAFTouched at the top of
//     the live refilter entry point (applyUserAccessFilterOnPig,
//     internal/resolvers/restactions/api/refilter.go). That file is owned by
//     another dev and the bump lands on the sibling branch
//     fix/1.12.3-authz-hardening. IT IS A HARD TAG CONDITION for 1.12.3.
//
// WHY BOTH, AND WHY (2) IS NOT OPTIONAL. (1) inspects the RA the apiRef names.
// It is therefore BLIND to a chain: a parent RA that declares nothing but whose
// inner step consumes a UAF child. Only a bump at the refilter itself sees that.
// Today no live RA forms such a chain (0 of 49 consume the restactions endpoint),
// so on this branch alone the chain is closed by CORPUS ACCIDENT rather than by
// the code — one customer CR away from being false, with no admission rule
// preventing it. TestM1_NestedUAFChild_NoCellPut_RequiresRefilterBump is RED here
// and green once (2) merges; that RED is the evidence the two are not redundant.
//
// Double-bumping is harmless: every gate reads Count()>0, never an exact count.
// BumpUAFTouched exists so each site is a single line with no sink plumbing of
// its own, and it is a no-op when no sink is installed.

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

// BumpUAFTouched records that the resolve under ctx produced a
// userAccessFilter-narrowed body, against whatever sink ctx carries.
//
// THIS IS THE ONE LINE EACH BUMP SITE CALLS. Two call it (see the header):
// apiref.Resolve's declaration bump, present on this branch; and the refilter's
// own execution bump at the top of applyUserAccessFilterOnPig
// (internal/resolvers/restactions/api/refilter.go), which lands on the sibling
// authz branch and is a hard tag condition. Keeping it a one-liner is what lets
// either site adopt it without any sink plumbing of its own.
//
// No-op when no sink is installed, so it is safe on every path — including tests
// and refilter.go's dead twin applyUserAccessFilter.
func BumpUAFTouched(ctx context.Context) {
	UAFTouchedSinkFromContext(ctx).Bump()
}
