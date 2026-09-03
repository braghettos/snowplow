package dispatchers

import (
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
)

// perCallState carries the per-/call timing + observability state that
// gets emitted into the snowplow stdout -> otel-daemonset filelog ->
// ClickHouse otel_logs pipeline at dispatcher exit. Created once per
// ServeHTTP and mutated as the call progresses (l1Hit, gvr) before the
// deferred emit() runs.
//
// Ship 0.30.171-debug only. The OTel SDK isn't wired on the shipping
// branch; the existing stdout->otel-daemonset pipeline IS (~28k rows/hr
// empirically). This emission is a single slog.InfoContext per /call
// (overhead well below 1ms) — used to identify the slow /call class in
// the 8-cycle parallelism diagnostic (slowest_call_ms ~470ms, chain
// ~3.65 => par=2.0 vs anchor par=4.3).
type perCallState struct {
	start   time.Time
	path    string
	method  string
	handler string // "restactions" | "widgets"
	l1Hit   string // "hit" | "miss" | "content-hit" | "n/a"
	gvr     string // group/version/resource — set once fetchObject succeeds
	user    string // captured at emit() from xcontext.UserInfo(ctx)
}

// beginPerCall is called as the FIRST line of each ServeHTTP body. It
// returns the live state pointer so the dispatcher can update l1Hit /
// gvr as the call progresses, and a deferred emit() closure that should
// be invoked at function exit via `defer beginPerCall(...)()`-style
// usage. Default l1Hit is "n/a" so an error before lookup still emits a
// well-formed row.
func beginPerCall(r *http.Request, handler string) (*perCallState, func()) {
	st := &perCallState{
		start:   time.Now(),
		path:    r.URL.Path,
		method:  r.Method,
		handler: handler,
		l1Hit:   "n/a",
	}
	ctx := r.Context()
	return st, func() {
		user := ""
		if ui, err := xcontext.UserInfo(ctx); err == nil {
			user = ui.Username
		}
		attrs := []any{
			slog.String("handler", handler),
			slog.String("path", st.path),
			slog.String("method", st.method),
			slog.String("user", user),
			slog.String("l1_hit", st.l1Hit),
			slog.String("gvr", st.gvr),
			slog.Int64("total_ms", time.Since(st.start).Milliseconds()),
		}
		// OTel log-correlation: since 1.12.4 the trace_id/span_id pair is
		// attached to EVERY *Context record by the trace-correlation
		// handler installed in main (log_handler.go), so this record gets
		// it for free through slog.InfoContext(ctx, ...). The per-site
		// injection that used to live here was the ONLY correlated record
		// in the process — and INFO, which LOG_LEVEL=warn suppresses — so
		// production correlation coverage was effectively zero. One site
		// became all sites; do not re-add it here.
		slog.InfoContext(ctx, "dispatcher.call.complete", attrs...)
	}
}
