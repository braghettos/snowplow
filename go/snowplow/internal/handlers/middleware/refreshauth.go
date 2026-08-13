// refreshauth.go — Ship 1 (live-refresh-coherence, option A).
//
// RefreshAuth is the SSE-specific authentication shim for GET /refreshes. It
// is a SIBLING of UserConfig (userconfig.go), deliberately NOT a modification
// of it — UserConfig is a verbatim upstream-mirror transcription with a
// drift-pin test (feedback_claim_vs_code_identity_at_diff_review), and must
// not be perturbed.
//
// WHY A SIBLING IS NEEDED (TRACED, design §4):
//   - UserConfig reads the JWT from an `Authorization: Bearer` header ONLY
//     (userconfig.go:134). A browser EventSource cannot set that header — the
//     only native credential channel is a cookie (withCredentials:true). So
//     mounting /refreshes behind UserConfig as-is would 401 every browser
//     connection.
//   - jwtutil.ValidateWithKeySource is a STATELESS RS256 signature
//     verification: UserInfo{Username,Groups} is embedded in the signed token
//     (jwtutil/create.go), with NO session store and NO server-side lookup.
//     So the identical UserInfo is recoverable from the SAME JWT regardless of
//     transport. RefreshAuth extracts the token from a header OR a cookie and
//     calls the byte-identical jwtutil.ValidateWithKeySource(keys, token).
//     (The verification key itself is fetched from authn's JWKS and cached by
//     the shared KeySource, so this stays off the per-request I/O path.)
//
// WHAT IT DELIBERATELY SKIPS:
//   - The `<user>-clientconfig` Secret / WithUserConfig lookup that UserConfig
//     performs (userconfig.go:166-232). /refreshes issues ZERO apiserver reads
//     — it never resolves a widget; it only validates the subscription
//     key-set against the connection's identity (refreshes.go) and streams
//     signals. UserInfo{Username,Groups} alone is sufficient for that
//     re-derivation. Skipping the Secret GET keeps /refreshes entirely off the
//     F-3 apiserver path (no fall-through counter, no apiserver hit), which is
//     exactly the cache-respecting invariant the feature exists to honour.
//
// NO TOKEN-IN-URL: the token is read from the header or the cookie only,
// never the query string (which would leak in logs/referrer).

package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/plumbing/jwtutil"
)

// envRefreshSessionCookie names the cookie RefreshAuth reads the JWT from on
// the browser EventSource path. The actual portal session-cookie name is a
// cross-team / portal-chart fact (design §11, OPEN) — it is therefore
// config-driven (env), NOT a hardcoded Go literal, so the frontend owner can
// set it at deploy time without a code change (feedback_no_special_cases).
// Default below is a sensible placeholder; the header path works regardless.
const envRefreshSessionCookie = "REFRESH_SESSION_COOKIE"

// defaultRefreshSessionCookie is the cookie name used when REFRESH_SESSION_COOKIE
// is unset. Confirm against the portal's actual session cookie before relying
// on the browser path (the header path is unconditional and used by curl
// falsifiers + non-browser clients).
const defaultRefreshSessionCookie = "krateo-session"

// refreshSessionCookieName returns the configured session-cookie name.
func refreshSessionCookieName() string {
	if v := os.Getenv(envRefreshSessionCookie); v != "" {
		return v
	}
	return defaultRefreshSessionCookie
}

// refreshTokenFromRequest extracts the bearer JWT from, in order:
//
//	(a) the `Authorization: Bearer <jwt>` header — so curl falsifiers,
//	    polyfill clients, and non-browser callers work; then
//	(b) the session cookie (the native browser EventSource path).
//
// Returns ("", false) when neither carries a usable token.
func refreshTokenFromRequest(req *http.Request) (string, bool) {
	if authHeader := req.Header.Get("Authorization"); authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" && parts[1] != "" {
			return parts[1], true
		}
	}
	if ck, err := req.Cookie(refreshSessionCookieName()); err == nil && ck.Value != "" {
		return ck.Value, true
	}
	return "", false
}

// RefreshAuth is the cookie-or-header JWT auth middleware for GET /refreshes.
// On success it places UserInfo on the request context via WithUserInfo —
// the IDENTICAL placement UserConfig uses (userconfig.go:231) — so the
// downstream handler reads identity exactly as the /call path does. On any
// failure it responds 401 (Unauthorized) and does not chain.
//
// Signature mirrors UserConfig's single-key-source shape minus authnNS (no
// Secret lookup), so the mux mount is a one-line Append. As of the
// asymmetric-signing migration the "key" is resolved through a
// jwtutil.KeySource — in production authn's JWKS endpoint, fetched and cached
// (see main.go) rather than mounted from a Secret.
func RefreshAuth(keys jwtutil.KeySource) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(wri http.ResponseWriter, req *http.Request) {
			if keys == nil {
				response.InternalError(wri, fmt.Errorf("no JWT key source configured"))
				return
			}

			token, ok := refreshTokenFromRequest(req)
			if !ok {
				response.Unauthorized(wri, fmt.Errorf("missing credentials: no bearer header or session cookie"))
				return
			}

			// The IDENTICAL stateless validation UserConfig performs at
			// userconfig.go:151. Both expired + generic-invalid map to 401
			// (jwtutil.Validate returns ErrTokenExpired / ErrTokenInvalid);
			// a key we cannot fetch is OUR fault, so it maps to 503 instead.
			userInfo, err := jwtutil.ValidateWithKeySource(keys, token)
			if err != nil {
				if errors.Is(err, jwtutil.ErrKeyUnavailable) {
					response.ServiceUnavailable(wri, err)
					return
				}
				response.Unauthorized(wri, err)
				return
			}

			// IDENTICAL identity placement to UserConfig (userconfig.go:231).
			// We intentionally do NOT call WithUserConfig / the Secret lookup —
			// /refreshes issues zero apiserver reads (see file header).
			ctx := xcontext.BuildContext(req.Context(),
				xcontext.WithUserInfo(userInfo),
			)
			next.ServeHTTP(wri, req.WithContext(ctx))
		}
		return http.HandlerFunc(fn)
	}
}
