package dispatchers

import "golang.org/x/sync/singleflight"

// widgetFlight dedups concurrent HTTP-path resolutions of the same widget
// L1 key. widgetBgFlight is the background equivalent used by the L1
// refresh loop so long re-resolves don't block foreground HTTP traffic.
//
// The RESTAction equivalents live in
// internal/resolvers/restactions/l1cache (one foreground group shared
// with widget.apiref callers, one background group for L1 refresh),
// hoisted there to eliminate drift and dedup widget and HTTP paths
// against each other.
var (
	widgetFlight   singleflight.Group
	widgetBgFlight singleflight.Group
)
