// build_info_expvar.go — 1.12.4 §7a. The build stamp as an observable.
//
// THE GAP. `main.build` is stamped correctly as of 1.12.3 (Dockerfile
// `-X main.build=`, matching the lowercase symbol main.go actually
// declares; .ko.yaml already had it). But the value reaches nothing
// observable: handlers.HealthCheck takes it and discards it — its
// parameters are explicitly kept "for API stability … intentionally
// unused" — and the only other consumers are the three default-OFF OTel
// Setup calls. On an OTel-off pod, which is every pod before this
// release, there is no way to ask a running snowplow which commit it is.
//
// THE SHAPE. `snowplow_build_info` is the standard build-info idiom: a
// constant 1 carrying the identity as a label. The expvar value is a
// map so /debug/vars and the OTLP gauge agree, and so a future stamp
// (branch, build date) is additive rather than a new key.
//
// Lives in package main because `build` is a package-main var; keeping
// the publisher next to it means the value cannot go stale relative to
// the linker flag that sets it.
package main

import (
	"expvar"
	"sync"
)

// buildInfoExpvarOnce guards the Publish against expvar's duplicate-key
// panic, matching every other registrar in the tree. main() calls the
// registrar exactly once, but tests call it too.
var buildInfoExpvarOnce sync.Once

// buildInfoValue is the last registered build string. Read by the
// published closure, so a test can register and then assert without
// racing the Once.
var buildInfoValue struct {
	sync.RWMutex
	v string
}

// registerBuildInfoExpvar publishes snowplow_build_info as
// {"value":1,"version":"<build>"}.
//
// Registered UNCONDITIONALLY — NOT gated on cache.Disabled(). The build
// identity is not a cache concept: an operator debugging a
// CACHE_ENABLED=false pod needs the commit at least as much as one
// debugging a cache-on pod. This is the same posture as
// RegisterRBACSnapshotExpvar and RegisterAuthzMemoExpvar.
//
// An empty build string (a `go build` without the linker flag, i.e. a
// local dev binary) publishes version "unknown" rather than "", so a
// dashboard panel never renders a blank label that reads as a scrape
// failure.
func registerBuildInfoExpvar(build string) {
	buildInfoValue.Lock()
	if build == "" {
		build = "unknown"
	}
	buildInfoValue.v = build
	buildInfoValue.Unlock()

	buildInfoExpvarOnce.Do(func() {
		expvar.Publish("snowplow_build_info", expvar.Func(func() any {
			buildInfoValue.RLock()
			defer buildInfoValue.RUnlock()
			return map[string]any{
				"value":   1,
				"version": buildInfoValue.v,
			}
		}))
	})
}

// BuildInfoVersion returns the build string the expvar closure reports,
// after the empty-string normalisation. Test-facing; the OTLP mirror
// does NOT read it (internal/metrics cannot import package main) — it
// gets the same string as the `build` argument to metrics.Setup, which
// is the identical main.build value, so the two surfaces agree by
// construction rather than by a cross-package call.
func BuildInfoVersion() string {
	buildInfoValue.RLock()
	defer buildInfoValue.RUnlock()
	if buildInfoValue.v == "" {
		return "unknown"
	}
	return buildInfoValue.v
}
