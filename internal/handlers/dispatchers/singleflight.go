package dispatchers

import "golang.org/x/sync/singleflight"

// Shared singleflight groups ensure that concurrent resolutions of the same
// L1 key — whether triggered by an HTTP request or by the background L1
// refresh — are deduplicated. Only one resolution runs; all other callers
// block and receive the same result.
var (
	widgetFlight     singleflight.Group
	restactionFlight singleflight.Group
	// Background re-resolve groups — separate from HTTP singleflight
	// so background re-resolves don't block HTTP requests.
	widgetBgFlight     singleflight.Group
	restactionBgFlight singleflight.Group
)
