// Package cache provides the snowplow informer-backed cache subsystem.
//
// At 0.30.1 the package is "plumbing only": types and constructors are
// compiled in but every consumer takes the apiserver branch because
// Disabled() defaults to true. Routing flips on at 0.30.2.
//
// Per project_redis_removal.md the cache subsystem MUST stay removable
// via the CACHE_ENABLED env toggle. Disabled() is the single read of
// that toggle.
package cache

import "os"

// Disabled reports whether the cache subsystem is disabled. When true,
// every consumer takes the apiserver branch; the informer factory is
// never instantiated; no goroutines start.
//
// Default is disabled — CACHE_ENABLED must be explicitly set to a
// truthy value ("true", "1", "yes") to enable cache plumbing. Any
// other value (including unset, empty, "false", "0", "no") is treated
// as disabled.
func Disabled() bool {
	switch os.Getenv("CACHE_ENABLED") {
	case "true", "1", "yes":
		return false
	default:
		return true
	}
}
