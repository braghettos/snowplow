// mapdepth_gate_test.go — Ship #6 (0.30.136) hermetic acceptance tests
// for the mapDepth log-level gate.
//
// THE FIX: mapDepth is a recursive full-tree walk of the resolved dict,
// called at 5 resolve.go sites only to fill the `depth` field of the
// "api successfully resolved" log line — ~23% of pod CPU. Ship #6 gates
// it: depthForLog (support.go) runs mapDepth (under dictMu) ONLY when
// the gate is enabled; on the common path it does no work, takes no
// lock, and returns a sentinel.
//
// #222 / 0.30.140 — REGRESSION FIX. The Ship #6 gate originally read
// `log.Enabled(ctx, slog.LevelDebug)`, but xcontext.Logger(ctx)
// fabricates a fresh Debug-enabled handler when ctx lacks the logger
// key — three caller chains (phase1 walker, F2 prewarm, refresher)
// reached Resolve with an un-stamped ctx and the gate fired TRUE under
// configmap DEBUG=false. The gate now reads env.Bool("DEBUG", false)
// directly so the configmap is the single source of truth, immune to
// ctx-logger fabrication. These tests drive the gate via t.Setenv
// accordingly; the logger argument is still passed (signature parity)
// but its level no longer drives the gate.
//
// Coverage of the PM-gate ACs that are hermetically verifiable:
//
//   - TestDepthForLog_AC1_NotComputedOnCommonPath -> AC-1 (mapDepth off
//     the common/non-debug path)
//   - TestDepthForLog_AC2_DeferredNotLost          -> AC-2 (under Debug,
//     depth is exact — byte-identical to pre-#6)
//   - TestDepthForLog_AC3_ConcurrentRaceClean      -> AC-3 (HARD —
//     dictMu discipline under -race, 16+ concurrent at Debug level)
//   - TestDepthForLog_AC6_AllFiveSitesGated        -> AC-6 (exactly 5
//     resolve.go call sites, none ungated)
//
// AC-4 (resolve `dict` / `/call` output byte-unchanged) is covered by
// the existing resolve functional-correctness regression suite —
// mapDepth is read-only, gating it changes nothing observable except
// CPU and the `depth` log field.
package api

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"
)

// discardLoggerForGate returns a *slog.Logger written to io.Discard.
// Its handler level is irrelevant to the #222 env-driven gate — the
// logger is still passed to depthForLog for signature parity, but the
// gate is now driven by t.Setenv("DEBUG", ...).
func discardLoggerForGate() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// nestedDict builds a dict of a known mapDepth — `depth` levels of
// nested map[string]any. mapDepth of an n-deep nest is n.
func nestedDict(depth int) map[string]any {
	if depth <= 1 {
		return map[string]any{"leaf": "value"}
	}
	return map[string]any{"child": nestedDict(depth - 1)}
}

// TestDepthForLog_AC1_NotComputedOnCommonPath — AC-1.
//
// On the common (DEBUG=false) path depthForLog must NOT walk the tree:
// it returns the sentinel depthSentinelNotComputed and does no mapDepth
// work. A nil logger also returns the sentinel.
//
// #222 / 0.30.140: the gate is driven by env.Bool("DEBUG", false), NOT
// the logger level. t.Setenv("DEBUG", "false") models the production
// configmap default.
func TestDepthForLog_AC1_NotComputedOnCommonPath(t *testing.T) {
	t.Setenv("DEBUG", "false")
	var mu sync.Mutex
	dict := nestedDict(7)

	// DEBUG=false -> sentinel, no walk.
	got := depthForLog(context.Background(), discardLoggerForGate(), &mu, dict)
	if got != depthSentinelNotComputed {
		t.Fatalf("AC-1 FAIL: DEBUG=false depthForLog = %d, want sentinel %d — mapDepth ran on "+
			"the common path", got, depthSentinelNotComputed)
	}

	// Nil logger: also the sentinel (no panic, no walk).
	if got := depthForLog(context.Background(), nil, &mu, dict); got != depthSentinelNotComputed {
		t.Fatalf("AC-1 FAIL: nil-logger depthForLog = %d, want sentinel %d", got, depthSentinelNotComputed)
	}

	// #222 falsifier: even with a Debug-level logger, DEBUG=false MUST
	// gate the walk OFF. Pre-#222 a Debug-enabled fabricated logger
	// from xcontext.Logger(ctx) on an un-stamped ctx wrongly enabled
	// the walk on the phase1 walker / F2 prewarm / refresher chains.
	debugLevelLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if got := depthForLog(context.Background(), debugLevelLogger, &mu, dict); got != depthSentinelNotComputed {
		t.Fatalf("#222 REGRESSION: DEBUG=false but Debug-level logger -> depthForLog = %d, "+
			"want sentinel %d. The env gate must dominate the logger level — pre-#222 the "+
			"un-stamped-ctx caller chains (walker/prewarm/refresher) wrongly ran mapDepth.", got, depthSentinelNotComputed)
	}
}

// TestDepthForLog_AC2_DeferredNotLost — AC-2.
//
// Under DEBUG=true, depthForLog runs mapDepth and returns the EXACT
// depth — byte-identical to the pre-#6 unconditional mapDepth(dict).
// Verified against dicts of known depth.
//
// #222 / 0.30.140: the gate is driven by DEBUG=true, NOT the logger
// level. The logger argument is still passed for signature parity.
func TestDepthForLog_AC2_DeferredNotLost(t *testing.T) {
	t.Setenv("DEBUG", "true")
	var mu sync.Mutex
	log := discardLoggerForGate()

	for _, want := range []int{1, 2, 5, 12} {
		dict := nestedDict(want)
		got := depthForLog(context.Background(), log, &mu, dict)
		if got != want {
			t.Fatalf("AC-2 FAIL: Debug depthForLog of a %d-deep dict = %d, want %d", want, got, want)
		}
		// Byte-identical to the raw mapDepth — the diagnostic is the
		// SAME value, only deferred to the Debug level.
		if raw := mapDepth(dict); got != raw {
			t.Fatalf("AC-2 FAIL: depthForLog=%d != mapDepth=%d — the gated value diverges", got, raw)
		}
	}

	// A slice-shaped dict is also walked correctly under Debug.
	sliceDict := map[string]any{"items": []any{nestedDict(4), nestedDict(2)}}
	if got, want := depthForLog(context.Background(), log, &mu, sliceDict), mapDepth(sliceDict); got != want {
		t.Fatalf("AC-2 FAIL: slice-shaped dict depth = %d, want %d", got, want)
	}
}

// TestDepthForLog_AC3_ConcurrentRaceClean — AC-3 (HARD — concurrency).
//
// Gating a lock is a concurrency change. Under DEBUG=true, depthForLog
// runs mapDepth UNDER dictMu — serialising the full-tree read against
// concurrent writers of the same dict. This test runs >=16 goroutines:
// readers calling depthForLog with DEBUG=true + writers mutating the
// shared dict under the SAME mutex (the jsonHandler-write analogue).
// Run under `go test -race`; an unsynchronised read/write trips here.
func TestDepthForLog_AC3_ConcurrentRaceClean(t *testing.T) {
	t.Setenv("DEBUG", "true")
	var mu sync.Mutex
	log := discardLoggerForGate()

	// The shared dict — guarded by mu, exactly as `dict` is guarded by
	// dictMu in Resolve.
	dict := map[string]any{"seed": nestedDict(3)}

	const readers = 16
	const writers = 8
	const iterations = 200

	var wg sync.WaitGroup

	// Readers — depthForLog at Debug level (mapDepth runs under mu).
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = depthForLog(context.Background(), log, &mu, dict)
			}
		}()
	}

	// Writers — mutate the shared dict under the SAME mutex, the
	// jsonHandler-write analogue. Without depthForLog's lock this is a
	// data race the detector would catch.
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				mu.Lock()
				dict["w"] = map[string]any{"by": id, "iter": i}
				mu.Unlock()
			}
		}(w)
	}

	wg.Wait()
}

// TestDepthForLog_AC3_CommonPathTakesNoLock — AC-3, the other half.
//
// On the common (DEBUG=false) path depthForLog must NOT acquire the
// mutex — there is no mapDepth call to protect. Proven by: hold the
// mutex on the test goroutine, then call depthForLog. If depthForLog
// tried to Lock(), it would deadlock; instead it returns the sentinel
// immediately. A watchdog goroutine fails the test on a deadlock
// rather than hanging it.
func TestDepthForLog_AC3_CommonPathTakesNoLock(t *testing.T) {
	t.Setenv("DEBUG", "false")
	var mu sync.Mutex
	dict := nestedDict(5)

	mu.Lock() // held for the whole call — depthForLog must not block on it
	defer mu.Unlock()

	done := make(chan int, 1)
	go func() {
		done <- depthForLog(context.Background(), discardLoggerForGate(), &mu, dict)
	}()

	select {
	case got := <-done:
		if got != depthSentinelNotComputed {
			t.Fatalf("AC-3 FAIL: common-path depthForLog = %d, want sentinel", got)
		}
	case <-time.After(2 * time.Second):
		// 2s is ample: depthForLog's DEBUG=false path is a single
		// env.Bool check + return; it cannot legitimately take 2s.
		t.Fatal("AC-3 FAIL: common-path depthForLog blocked on dictMu — it must NOT acquire " +
			"the lock when mapDepth is gated out")
	}
}

// TestDepthForLog_AC6_AllFiveSitesGated — AC-6.
//
// Exactly 5 mapDepth call sites existed in resolve.go; all 5 are now
// routed through depthForLog. This test greps resolve.go's source and
// asserts: zero ungated `mapDepth(` CALL sites remain, and there are
// exactly 5 depthForLog(ctx, ...) call sites.
func TestDepthForLog_AC6_AllFiveSitesGated(t *testing.T) {
	src, err := os.ReadFile("resolve.go")
	if err != nil {
		t.Fatalf("read resolve.go: %v", err)
	}
	text := string(src)

	// No ungated mapDepth CALL site. `mapDepth(` followed by an
	// identifier char is a call; it may still appear in comments, so we
	// strip line comments first.
	lineComment := regexp.MustCompile(`(?m)//.*$`)
	codeOnly := lineComment.ReplaceAllString(text, "")
	if mapDepthCall := regexp.MustCompile(`\bmapDepth\(`).FindAllString(codeOnly, -1); len(mapDepthCall) != 0 {
		t.Fatalf("AC-6 FAIL: %d ungated mapDepth( call site(s) remain in resolve.go — "+
			"every site must route through depthForLog", len(mapDepthCall))
	}

	// Exactly 5 depthForLog(ctx, ...) call sites.
	calls := regexp.MustCompile(`depthForLog\(ctx,`).FindAllString(codeOnly, -1)
	if len(calls) != 5 {
		t.Fatalf("AC-6 FAIL: found %d depthForLog(ctx, ...) call sites in resolve.go, want exactly 5", len(calls))
	}
}
