package profile

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Request profiling — enabled by CACHE_PROFILE=true env var.
// Adds per-segment timings on the L1-hit path to identify the 290ms floor.

type profileKey struct{}

type profile struct {
	start time.Time
	last  time.Time
	path  string
	segs  []segment
}

type segment struct {
	name string
	ms   int64
}

var profileEnabled = os.Getenv("CACHE_PROFILE") == "true"

// StartProfile attaches a new profile to the context.
// Returns the original context if profiling is disabled (no-op fast path).
func Start(ctx context.Context, path string) context.Context {
	if !profileEnabled {
		return ctx
	}
	now := time.Now()
	return context.WithValue(ctx, profileKey{}, &profile{
		start: now,
		last:  now,
		path:  path,
	})
}

// Mark records a segment timing since the previous Mark call (or profile start).
// No-op if profiling is disabled or no profile in context.
func Mark(ctx context.Context, name string) {
	if !profileEnabled {
		return
	}
	p, ok := ctx.Value(profileKey{}).(*profile)
	if !ok {
		return
	}
	now := time.Now()
	p.segs = append(p.segs, segment{name, now.Sub(p.last).Milliseconds()})
	p.last = now
}

// EndProfile logs the profile summary as a single structured log line.
// Called at the end of the L1-hit path.
func End(ctx context.Context, outcome string) {
	if !profileEnabled {
		return
	}
	p, ok := ctx.Value(profileKey{}).(*profile)
	if !ok {
		return
	}
	totalMs := time.Since(p.start).Milliseconds()
	attrs := []any{
		slog.String("path", p.path),
		slog.String("outcome", outcome),
		slog.Int64("total_ms", totalMs),
	}
	for _, s := range p.segs {
		attrs = append(attrs, slog.Int64(s.name+"_ms", s.ms))
	}
	slog.Info("cache-profile", attrs...)
}
