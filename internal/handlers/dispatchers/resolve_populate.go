// resolve_populate.go — Ship C (0.30.112): the single resolve-and-store
// path for the L1 resolved-output cache.
//
// resolveAndPopulateL1 re-resolves an L1 entry from its own
// ResolvedKeyInputs and writes the fresh bytes back under the canonical
// key. It is the body the runtime refresher's RefreshFunc invokes on a
// dirty-mark (Ship C) and the body Ship F's prewarm will reuse — one
// resolve path, no duplication.
//
// IDENTITY (PM directive, AC-C7): the re-resolve runs under the entry's
// OWN Inputs identity — Username + Groups from the ResolvedKeyInputs.
// A refresh of user U's entry resolves as U, so RBAC narrowing and the
// resolved content stay user-correct. The re-resolve context also carries
// WithL1KeyContext(key) so the resolver re-records dep edges (the inner
// object set may have changed since the original resolve).
//
// SA TRANSPORT (Ship 0.30.113 Part B): a background refresh has no live
// per-user bearer token — the original request's Endpoint is long gone.
// The widget resolver (widgets.Resolve) reads xcontext.UserConfig(ctx)
// directly and fails "user *Endpoint not found in context" if only the
// identity (WithUserInfo) is supplied. With the informer pivot ON
// (RESOLVER_USE_INFORMER=true) every K8s read is informer-served and
// RBAC-narrowed IN-PROCESS from WithUserInfo — never from the user's
// token — so the user's Endpoint is needed ONLY as a transport. We
// therefore supply the snowplow ServiceAccount endpoint + *rest.Config
// as that transport (WithUserConfig + WithInternalEndpoint +
// WithInternalRESTConfig) while keeping WithUserInfo{Username,Groups}
// for per-user correctness. No per-user token is stored. This is the
// EXACT pattern Phase 1 uses (withPhase1SAContext, phase1_walk.go) — the
// load-bearing 0.30.103 SA-CA seam. When saEP/saRC are nil (no SA creds
// — unit test / outside-cluster) the context is identity-only as before.
//
// Per feedback_l1_invalidation_delete_only.md: this path only ever
// Put()s — it never evicts. A refresh that lands after the entry was
// evicted must not resurrect it (see the post-resolve liveness re-check).

package dispatchers

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/apis"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	restactionsapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

// resolveOnceFn is the resolve-and-encode seam. It re-fetches the CR
// named by inputs (under ctx's identity) and returns the encoded
// resolver output. Production wires it to resolveOnceProd; tests stub
// it to exercise resolveAndPopulateL1's queue/identity/Put plumbing
// without a live cluster.
//
// A package var rather than a parameter so the refresher's RefreshFunc
// signature (cache.RefreshFunc) is untouched. Swapped only by the
// _test.go shim; production never reassigns it.
var resolveOnceFn = resolveOnceProd

// resolveAndPopulateL1 is the single resolve-and-store path. It:
//
//  1. computes the canonical L1 key from inputs (must equal the key the
//     entry is filed under — ComputeKey is deterministic);
//  2. builds the re-resolve context: the entry's own Username+Groups
//     identity, the SA transport (saEP/saRC) so the resolver's
//     UserConfig/object-fetch sites have an apiserver client-config,
//     plus WithL1KeyContext(key) so dep edges are re-recorded;
//  3. re-resolves + encodes via the resolveOnce seam;
//  4. re-checks the entry is still live (a DELETE-evict may have raced
//     the refresh) and, if so, Put()s the fresh bytes.
//
// saEP/saRC are the process-singleton snowplow ServiceAccount endpoint +
// *rest.Config — supplied as TRANSPORT only (see the file header). When
// nil (no SA creds) the context is identity-only and a widget re-resolve
// that reads xcontext.UserConfig fails; the refresher's bounded retry
// then drops the key to TTL.
//
// Returns an error on resolve failure so the refresher can retry with
// backoff; returns nil (no-op) for an Inputs the path cannot drive.
func resolveAndPopulateL1(ctx context.Context, inputs cache.ResolvedKeyInputs, saEP *endpoints.Endpoint, saRC *rest.Config) error {
	log := xcontext.Logger(ctx)

	c := cache.ResolvedCache()
	if c == nil {
		// L1 disabled — nothing to populate. Not an error.
		return nil
	}

	key := cache.ComputeKey(inputs)

	// AC-C7: re-resolve under the entry's OWN identity. Username+Groups
	// come straight off the cached Inputs — that is what makes refresh #N
	// resolve as user U (RBAC narrowing stays user-correct).
	opts := []xcontext.WithContextFunc{
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: inputs.Username,
			Groups:   inputs.Groups,
		}),
	}
	// Ship 0.30.113 Part B — SA transport. A background refresh has no
	// per-user token; the widget resolver reads xcontext.UserConfig
	// directly. Supply the SA endpoint as the transport so that read
	// succeeds. Under the informer pivot every K8s read is RBAC-narrowed
	// in-process from WithUserInfo above, never from this endpoint's
	// token — so this is transport-only, no per-user-token storage.
	// Mirrors withPhase1SAContext (phase1_walk.go).
	if saEP != nil {
		opts = append(opts, xcontext.WithUserConfig(*saEP))
	}
	rctx := xcontext.BuildContext(ctx, opts...)
	// WithInternalEndpoint / WithInternalRESTConfig make cache.ClientConfigFor
	// (internal_client.go) return the pre-built SA *rest.Config verbatim
	// for the objects.Get apiserver fall-through and resourcesrefs.Resolve
	// — the load-bearing 0.30.103 SA-CA seam (the SA's raw-PEM CA cannot
	// survive the base64/cert-only kubeconfig path).
	if saEP != nil {
		rctx = cache.WithInternalEndpoint(rctx, saEP)
	}
	if saRC != nil {
		rctx = cache.WithInternalRESTConfig(rctx, saRC)
	}
	// WithL1KeyContext threads the L1 key so the resolver's inner-call
	// recording site re-records dep edges for this refresh.
	rctx = cache.WithL1KeyContext(rctx, key)

	// Ship 0.30.120 layer (b) — error-aware Put-gate. Install a
	// stage-error sink on the re-resolve context. The api resolver bumps
	// it whenever it writes dict[call.ErrorKey] (a swallowed,
	// continueOnError'd stage failure — e.g. the 401 from an exportJwt
	// loopback that has no per-user JWT in a background refresh). After
	// the re-resolve we read stageErrSink.Load(): a non-zero value means
	// the fresh bytes were produced under a stage error and MUST NOT
	// overwrite the prior good entry. The sink is request-path-inert:
	// only the refresher installs it, so a cold dispatch is unaffected.
	rctx, stageErrSink := cache.WithStageErrorSink(rctx)

	encoded, err := resolveOnceFn(rctx, inputs)
	if err != nil {
		return fmt.Errorf("resolveAndPopulateL1 %s/%s: %w",
			inputs.CacheEntryClass, inputs.Name, err)
	}
	if encoded == nil {
		// The seam declined to resolve (e.g. unknown handler kind) —
		// skip-to-TTL, not an error.
		return nil
	}

	// A refresh that lands AFTER the entry was DELETE-evicted must not
	// resurrect it. Re-Get under the key: if it is gone, drop the fresh
	// bytes on the floor (the eviction is authoritative).
	if _, alive := c.Get(key); !alive {
		log.Debug("resolveAndPopulateL1: entry evicted during refresh; not resurrecting",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
		)
		return nil
	}

	// Ship 0.30.120 layer (b) — error-aware Put-gate. If the re-resolve
	// observed ANY stage error (a swallowed, continueOnError'd inner-call
	// failure — e.g. the 401 from an exportJwt loopback that the SA-
	// transport refresher cannot satisfy), the fresh bytes are an
	// under-served result: they MUST NOT overwrite the user's prior good
	// entry. Decline the Put and return nil — NOT an error. A deterministic
	// stage failure must not drive AddRateLimited / burn the retry budget;
	// the prior good entry stays and the outer TTL is the safety net.
	//
	// The gate keys on STAGE-ERROR PRESENCE, never on result emptiness — a
	// user who legitimately has 0 compositions produces no stage error
	// (sink == 0) and their empty result IS stored.
	if stageErrSink.Load() > 0 {
		log.Warn("resolveAndPopulateL1: stage error during refresh; declining to overwrite good entry",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
			slog.String("handler", inputs.CacheEntryClass),
			slog.String("user", inputs.Username),
			slog.Int64("stage_errors", stageErrSink.Load()),
			slog.String("effect", "prior good entry kept; TTL is the outer net"),
		)
		cache.BumpRefresherSkippedStageError()
		return nil
	}

	c.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  &inputs,
	})
	log.Debug("resolveAndPopulateL1: re-resolved + stored",
		slog.String("subsystem", "cache"),
		slog.String("key_hash", key),
		slog.String("handler", inputs.CacheEntryClass),
		slog.String("user", inputs.Username),
	)
	return nil
}

// resolveOnceProd is the production resolve-and-encode implementation.
// It re-fetches the dispatch CR named by inputs (objects.Get, under
// ctx's identity) and dispatches the matching resolver, returning the
// encoded output byte-identical to the request-path encode
// (encodeResolvedJSON — same encoder settings as a cold dispatch).
func resolveOnceProd(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error) {
	authnNS := env.String("AUTHN_NAMESPACE", "")

	// Ship F1 (0.30.119): an api-stage entry is a CONTENT-keyed K8s call
	// (gvr, namespace, name-or-empty) — NOT a RESTAction. Its refresh is
	// a single un-gated K8s re-dispatch + re-Put under the same content
	// key; there is no whole-RESTAction re-run, no objects.Get of a CR,
	// no self-hit (a content entry is re-dispatched, never self-Got).
	// resolveContentEntryForRefresh returns the fresh raw envelope, which
	// resolveAndPopulateL1 Puts under the content key.
	if inputs.CacheEntryClass == cache.CacheEntryClassApistage {
		return resolveContentEntryForRefresh(ctx, inputs)
	}

	// restactions / widgets entries identify a CR — re-fetch it, then
	// dispatch the matching resolver.
	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      inputs.Name,
			Namespace: inputs.Namespace,
		},
		APIVersion: schemaGroupVersion(inputs.Group, inputs.Version),
		Resource:   inputs.Resource,
	}
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		return nil, fmt.Errorf("re-fetch %s/%s: %s",
			inputs.Resource, inputs.Name, got.Err.Message)
	}
	if got.Unstructured == nil {
		return nil, fmt.Errorf("re-fetch %s/%s: nil object", inputs.Resource, inputs.Name)
	}

	switch inputs.CacheEntryClass {
	case "restactions":
		return resolveRestActionForRefresh(ctx, got, inputs, authnNS)
	case "widgets":
		return resolveWidgetForRefresh(ctx, got, inputs, authnNS)
	default:
		// Unknown handler kind — skip-to-TTL.
		return nil, nil
	}
}

// resolveContentEntryForRefresh re-dispatches the single K8s call an
// api-stage CONTENT entry caches (Ship F1, 0.30.119). The entry's Inputs
// are a content key (gvr, namespace, name-or-empty); the refresh is one
// un-gated K8s re-dispatch — no whole-RESTAction re-run, no self-hit.
// The fresh raw envelope is returned for resolveAndPopulateL1 to Put
// back under the content key. (nil, nil) — the call is not currently
// pivot-servable — is a skip-to-TTL, not an error.
func resolveContentEntryForRefresh(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error) {
	return restactionsapi.RefreshContentEntry(ctx, inputs)
}

// schemaGroupVersion renders the apiVersion string objects.Get expects.
// Core group ("") renders as just the version.
func schemaGroupVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// resolveRestActionForRefresh converts the re-fetched CR to a typed
// RESTAction and dispatches restactions.Resolve, returning encoded
// output. Mirrors the restActionHandler.ServeHTTP resolve+encode path.
func resolveRestActionForRefresh(ctx context.Context, got objects.Result, inputs cache.ResolvedKeyInputs, authnNS string) ([]byte, error) {
	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add apis to scheme: %w", err)
	}
	var cr templatesv1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); err != nil {
		return nil, fmt.Errorf("unstructured -> RESTAction: %w", err)
	}

	// Ship 0.30.123 (#155): the Ship 0.30.120 layer-(a) exportJwt
	// skip-to-TTL scan was REMOVED here. Layer (a) existed only because a
	// background refresh had no way to complete an exportJwt /call-loopback
	// stage (no per-user JWT) — so it declined to refresh those RESTActions
	// at all. 0.30.123's in-process nested /call resolves a /call-loopback
	// stage WITHOUT an Authorization header (identity carried on ctx), so
	// the refresher CAN now correctly refresh an exportJwt RESTAction with
	// real, non-empty content. The skip-to-TTL net is obsolete.
	//
	// Layer (b) — the error-aware Put-gate in resolveAndPopulateL1 — STAYS
	// as the general backstop: any stage that still errors (a genuine RBAC
	// denial, an apiserver fault) bumps the stage-error sink and the
	// Put-gate declines to overwrite the good entry.

	res, err := restactions.Resolve(ctx, restactions.ResolveOptions{
		In:      &cr,
		AuthnNS: authnNS,
		PerPage: inputs.PerPage,
		Page:    inputs.Page,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve RESTAction: %w", err)
	}
	return encodeResolvedJSON(res)
}

// resolveWidgetForRefresh dispatches widgets.Resolve on the re-fetched
// CR, returning encoded output. Mirrors the widgetsHandler.ServeHTTP
// resolve+encode path.
func resolveWidgetForRefresh(ctx context.Context, got objects.Result, inputs cache.ResolvedKeyInputs, authnNS string) ([]byte, error) {
	res, err := widgets.Resolve(ctx, widgets.ResolveOptions{
		In:      got.Unstructured,
		AuthnNS: authnNS,
		PerPage: inputs.PerPage,
		Page:    inputs.Page,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve widget: %w", err)
	}
	return encodeResolvedJSON(res)
}
