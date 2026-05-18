// nested_call_testshim_test.go — test-only shim for swapping the api
// package's nested-/call resolver seam (Ship 0.30.123, #155).
//
// The seam var (api.nestedCallResolver) is unexported; api.RegisterNested
// CallResolver is the exported setter. setNestedCallResolverForTest swaps
// it for the duration of a test and returns a restore that reinstalls the
// production wiring (dispatchers.ResolveNestedCall). Production code never
// calls this.

package dispatchers

import (
	"context"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	restactionsapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
)

// setNestedCallResolverForTest installs fn as the nested-/call resolver
// seam and returns a restore function the caller MUST defer/Cleanup. The
// restore reinstalls the production resolver (ResolveNestedCall) so a
// later test that relies on the real wiring is unaffected.
func setNestedCallResolverForTest(
	fn func(ctx context.Context, ref templates.ObjectReference, perPage, page int, extras map[string]any) ([]byte, error),
) func() {
	restactionsapi.RegisterNestedCallResolver(fn)
	return func() { restactionsapi.RegisterNestedCallResolver(ResolveNestedCall) }
}
