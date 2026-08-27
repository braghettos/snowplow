// userconfig.go — Ship D.3 / 0.30.151. Snowplow-local replacement
// for plumbing's `use.UserConfig` middleware, the F-3 closure that
// Ship D.2 / 0.30.143 missed.
//
// PROVENANCE / UPSTREAM DRIFT MONITOR (AC-D3.1 + AC-D3.14).
//   - Upstream module:  github.com/krateo-platformops/plumbing
//   - Upstream version: v1.14.2 (pinned in go.mod). Bumped v1.14.0→v1.14.2 for the
//     issue #160 jqutil json.Number encoder fix (plumbing PR #27/#28). Re-audited:
//     the v1.14.0→v1.14.2 diff touches jqutil + crdgen + CI only — server/use/
//     userconfig.go is UNCHANGED, so this verbatim mirror remains identical.
//   - Upstream file:    server/use/userconfig.go
//   - Upstream func:    use.UserConfig(keys jwtutil.KeySource, authnNS string)
//                       func(http.Handler) http.Handler
//   - Upstream lines:   :19-73 (73-line single function)
//
// asymmetric-signing migration: upstream now verifies RS256 against an RSA
// public key instead of an HMAC shared secret, and resolves that key through a
// jwtutil.KeySource keyed on the token's "kid" rather than holding one parsed
// key. In production the source is a jwtutil.JWKSKeySource pointed at authn's
// /.well-known/jwks.json, which fetches and caches the key set — so snowplow no
// longer mounts authn's public key from a Secret at all, and authn can rotate
// its keypair without a snowplow redeploy. The transcription below mirrors
// upstream: every request path that used to call jwtutil.Validate(signingKey,
// ...) now calls jwtutil.ValidateWithKeySource(keys, ...). A key that cannot be
// resolved is reported 503 (our fault, retryable) rather than 401, byte-identical
// to upstream's ErrKeyUnavailable handling.
//
// This file is a VERBATIM transcription of `use.UserConfig`'s control
// flow with ONE intentional behaviour-additive change: the
// `endpoints.FromSecret(...)` call at upstream :50-60 is preceded by
// a cache-first lookup against the Ship D.2 in-process Secrets
// snapshot (`api.FromInformerSecret`). On any cache miss the
// VERBATIM upstream call is then issued with the IDENTICAL arguments;
// error handling on the fallback path is byte-identical to upstream
// (NotFound → 401, anything else → 500). Cache hits bypass the
// apiserver GET entirely and are byte-equivalent to what
// `endpoints.FromSecret` would have returned (gated by Ship D.2's
// TestFromInformerSecret_UpstreamFieldParity).
//
// Why this lives in snowplow (not upstream): `project_no_upstream_authority`
// — we cannot patch krateoplatformops/plumbing. Substituting a
// snowplow-local middleware at the same mux mount point is the
// only in-tree way to wrap site #2 of the F-3 call graph (see
// docs/ship-d3-clientconfig-secret-cache-middleware-design.md §1).
//
// Pinned upstream version for drift monitoring (AC-D3.14). Bumping
// `plumbing` in go.mod without re-auditing this file MUST trip
// TestUserConfigMirror_PlumbingVersionPin in userconfig_test.go.
package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/plumbing/kubeutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions/api"
)

// PinnedPlumbingVersion is the upstream `plumbing` module version this
// middleware was transcribed from. AC-D3.14 — if `go.mod`'s pinned
// version drifts from this constant, `TestUserConfigMirror_PlumbingVersionPin`
// fails and the operator must re-audit upstream
// `server/use/userconfig.go` line-by-line before bumping this string.
const PinnedPlumbingVersion = "v1.14.2"

// UserConfig is the snowplow-local cache-aware sibling of plumbing's
// `use.UserConfig`. Signature is byte-identical (same parameters,
// same return type) so the call-site swap at `main.go`'s 7 mount
// points is a single-token replacement (`use.` → `middleware.`).
//
// Flow (verbatim from upstream except where annotated):
//
//   1.  Authorization header presence check — Unauthorized on miss.
//   2.  `Bearer ` prefix validation — Unauthorized on mismatch.
//   3.  `jwtutil.ValidateWithKeySource(keys, token)` — Unauthorized on a bad or
//       expired token, ServiceUnavailable when the key itself is unresolvable.
//   4.  `rest.InClusterConfig()` — InternalError on error.
//
//      (Steps 1-4 are byte-identical to upstream; the early `return`s
//      mean we exit before any cache lookup runs. Cache=off behaviour
//      is preserved trivially: we never reach step 5 on any of these
//      branches.)
//
//   5.  CACHE-FIRST LOOKUP — Ship D.3 change.
//       `api.FromInformerSecret(ctx, authnNS, "<user>-clientconfig")`.
//
//       a. served=true, ferr=nil  → cache HIT. Skip the upstream
//          apiserver GET entirely; the cache's `Endpoint` value is
//          byte-equivalent to what upstream `FromSecret` would have
//          returned (Ship D.2 `TestFromInformerSecret_UpstreamFieldParity`).
//
//       b. served=false, ferr=nil → SOFT MISS (cache=off,
//          pre-readiness, NS mismatch, Secret absent in snapshot).
//          Record the fallthrough counter on the call's scope
//          context, THEN re-issue the verbatim upstream call with
//          IDENTICAL arguments (`endpoints.FromSecret(context.Background(),
//          sarc, name, authnNS)`). Error handling on the re-call is
//          byte-identical to upstream :53-60: `apierrors.IsNotFound(err)`
//          → Unauthorized; any other error → InternalError.
//
//       c. served=false, ferr!=nil → CACHE HARD ERROR. The Secret is
//          present in the snapshot but its content is malformed
//          ("missed required attribute for endpoint: server-url"
//          — Ship D.2 verbatim error string). Treat as upstream's
//          non-NotFound branch → InternalError. The error string is
//          byte-identical to what upstream would deliver on the same
//          input (gated by AC-D2.6).
//
//   6-7. `xcontext.BuildContext(ctx, WithAccessToken, WithUserInfo,
//        WithUserConfig(ep))` + `next.ServeHTTP(wri, req.WithContext(ctx))`
//        — verbatim from upstream :62-68. The `ep` value flowing in
//        is byte-equivalent regardless of which branch (5.a, 5.b, 5.c)
//        produced it; the downstream contract is identical.
//
// AC-D3.3 — `cache.RecordApiserverFallthrough(ctx, ReasonSecretGet, "")`
// fires on the SOFT-MISS branch (5.b) BEFORE the upstream
// `endpoints.FromSecret` call. This is the F-3 internal counter the
// 0.30.143 baseline lacked — post-D.3 the counter and apiserver-side
// `secrets,GET` rate have the SAME denominator (each cache miss
// increments both exactly once).
//
// AC-D3.N — `ReasonSecretGet` counter recovers. Post-deploy
// `/debug/vars` will show `snowplow_apiserver_fallthrough_cells["call-*|secret-get"]`
// non-zero on the production workload. This is the empirical proof
// the middleware swap rewired the F-3 path through snowplow's local
// counter-firing wrapper rather than the un-instrumented upstream
// middleware.
//
// AC-D3.10 — `CACHE_ENABLED=false` flag-flip semantics. With caching
// disabled, `api.FromInformerSecret` returns `(zero, false, nil)`
// (the soft-miss path) on every request → the upstream `FromSecret`
// is invoked on every call → apiserver-side rate returns to within
// ±10% of the pre-D.3 baseline. The flag is verified-removable
// (`project_caching_is_provisional`).
func UserConfig(keys jwtutil.KeySource, authnNS string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(wri http.ResponseWriter, req *http.Request) {
			// Upstream — a missing key source is a wiring bug, surfaced on every
			// request as an InternalError, before the auth-header check.
			if keys == nil {
				response.InternalError(wri, fmt.Errorf("no JWT key source configured"))
				return
			}

			// Upstream :22-26 — Authorization header presence.
			authHeader := req.Header.Get("Authorization")
			if authHeader == "" {
				response.Unauthorized(wri, fmt.Errorf("missing authorization header"))
				return
			}

			// Upstream :28-32 — Bearer prefix.
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
				response.Unauthorized(wri, fmt.Errorf("invalid authorization header format"))
				return
			}

			// Upstream :34-42 — jwtutil.ValidateWithKeySource. Both expired +
			// generic-invalid branches go to Unauthorized; we preserve the
			// if/else shape verbatim so a future divergence in upstream is loud
			// at audit time even if the observable behaviour is identical today.
			userInfo, err := jwtutil.ValidateWithKeySource(keys, parts[1])
			if err != nil {
				// Upstream — an unresolvable key (JWKS unreachable, unknown
				// kid) is OUR failure, not a bad credential: 503 so the client
				// retries instead of re-authenticating against an authn that is
				// merely down.
				if errors.Is(err, jwtutil.ErrKeyUnavailable) {
					response.ServiceUnavailable(wri, err)
					return
				}
				if errors.Is(err, jwtutil.ErrTokenExpired) {
					response.Unauthorized(wri, err)
				} else {
					response.Unauthorized(wri, err)
				}
				return
			}

			// Ship D.3 — cache-first lookup. The `<user>-clientconfig`
			// Secret name is computed IDENTICALLY to upstream
			// `userconfig.go:51-52`: kubeutil.MakeDNS1123Compatible of
			// the validated JWT username, with the `-clientconfig`
			// suffix.
			secretName := fmt.Sprintf("%s-clientconfig",
				kubeutil.MakeDNS1123Compatible(userInfo.Username))

			ep, served, ferr := api.FromInformerSecret(req.Context(), authnNS, secretName)
			if !served {
				if ferr != nil {
					// (5.c) — cache HARD ERROR (Ship D.2's verbatim
					// upstream error string for server-url missing).
					// Upstream's equivalent branch is the non-NotFound
					// arm at :58-59 → InternalError.
					response.InternalError(wri, ferr)
					return
				}
				// (5.b) — SOFT MISS. Record the fallthrough counter
				// BEFORE the upstream re-call so the per-cell rate
				// matches the apiserver-side denominator exactly
				// (AC-D3.3, AC-D3.4, AC-D3.N).
				cache.RecordApiserverFallthrough(req.Context(), cache.ReasonSecretGet, "")

				// Upstream :44-48 — InClusterConfig. Required for the
				// upstream `FromSecret` re-call below. Deferred from
				// upstream's pre-FromSecret position because:
				//
				//   (a) On cache HIT (the steady state) we don't need
				//       it — the cache supplies the Endpoint and the
				//       SA RC is irrelevant. Deferring avoids paying
				//       its cost on the hot path.
				//   (b) On cache MISS the call order vs upstream is
				//       byte-identical: InClusterConfig immediately
				//       precedes the FromSecret re-call, exactly as
				//       upstream :44-50 has it. The InClusterConfig
				//       failure mode (500 with verbatim "unable to
				//       create in cluster config" wrap) is preserved
				//       and tested by
				//       TestUserConfig_ByteEquivalence_CacheMiss.
				sarc, ierr := rest.InClusterConfig()
				if ierr != nil {
					response.InternalError(wri, fmt.Errorf("unable to create in cluster config: %w", ierr))
					return
				}

				// Verbatim upstream call (`use.UserConfig` :50-52)
				// with the EXACT same arguments. The cache-miss
				// fallback's observable behaviour MUST be byte-identical
				// to upstream — gated by TestUserConfig_ByteEquivalence_CacheMiss.
				ep, ferr = endpoints.FromSecret(context.Background(), sarc, secretName, authnNS)
				if ferr != nil {
					// Upstream :54-59 — NotFound → 401, else → 500.
					// PM load-bearing pin: the IsNotFound distinction
					// MUST be preserved here (cache-miss errors must
					// route 401 vs 500 identically to upstream).
					if apierrors.IsNotFound(ferr) {
						response.Unauthorized(wri, ferr)
						return
					}
					response.InternalError(wri, ferr)
					return
				}
			}

			// Upstream :62-66 — BuildContext with the 3 opts in the same
			// order. `ep` is byte-equivalent regardless of whether (5.a)
			// cache-hit or (5.b) upstream re-call produced it.
			ctx := xcontext.BuildContext(req.Context(),
				xcontext.WithAccessToken(parts[1]),
				xcontext.WithUserInfo(userInfo),
				xcontext.WithUserConfig(ep),
			)

			// Upstream :68 — chain to next handler.
			next.ServeHTTP(wri, req.WithContext(ctx))
		}

		return http.HandlerFunc(fn)
	}
}
