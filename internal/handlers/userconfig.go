package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/kubeutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/profile"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

// CachedUserConfig is a drop-in replacement for plumbing's use.UserConfig that
// caches the user's Endpoint (fetched from the -clientconfig Secret) in Redis.
// This eliminates the K8s API call on every request (~150-200ms), falling back
// to the live lookup on cache miss. The UserSecretWatcher invalidates the cache
// entry when the Secret changes.
func CachedUserConfig(signingKey, authnNS string, rc *rest.Config, c *cache.RedisCache, rbacWatcher *cache.RBACWatcher) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
			ctx := profile.Start(req.Context(), req.URL.Path)

			authHeader := req.Header.Get("Authorization")
			if authHeader == "" {
				response.Unauthorized(wri, fmt.Errorf("missing authorization header"))
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
				response.Unauthorized(wri, fmt.Errorf("invalid authorization header format"))
				return
			}

			userInfo, err := jwtutil.Validate(signingKey, parts[1])
			if err != nil {
				response.Unauthorized(wri, err)
				return
			}
			profile.Mark(ctx, "jwt")

			// Track last-request timestamp for user activity classification.
			// Fire-and-forget: failure here is non-critical.
			if c != nil {
				go c.SetStringWithTTL(context.Background(), "snowplow:last-seen:"+userInfo.Username,
					strconv.FormatInt(time.Now().Unix(), 10), 60*time.Minute)
			}

			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.AddEvent("user.authenticated", trace.WithAttributes(
					attribute.String("username", userInfo.Username),
				))
			}

			ep, err := cachedEndpointLookup(ctx, c, rc, userInfo.Username, authnNS)
			if err != nil {
				if apierrors.IsNotFound(err) {
					response.Unauthorized(wri, err)
					return
				}
				response.InternalError(wri, err)
				return
			}
			profile.Mark(ctx, "usercfg")

			ctx = xcontext.BuildContext(ctx,
				xcontext.WithAccessToken(parts[1]),
				xcontext.WithUserInfo(userInfo),
				xcontext.WithUserConfig(ep),
				xcontext.WithLogger(slog.Default()),
			)

			// Inject RBACWatcher for local RBAC evaluation (zero K8s API calls).
			// Without this, every UserCan call falls back to SSAR which
			// saturates the K8s API rate limiter at scale.
			if rbacWatcher != nil {
				ctx = cache.WithRBACWatcher(ctx, rbacWatcher)
			}

			// Compute binding identity for RBAC-based L1 cache sharing.
			// Users with identical RBAC bindings share the same L1 entries.
			if rbacWatcher != nil {
				if bid := rbacWatcher.CachedBindingIdentity(userInfo.Username, userInfo.Groups); bid != "" {
					ctx = cache.WithBindingIdentity(ctx, bid)
					// Register the mapping so L1 refresh can find credentials
					// for keys that use the binding identity instead of username.
					cache.RegisterBindingUser(bid, userInfo.Username)
				}
			}

			next.ServeHTTP(wri, req.WithContext(ctx))
		})
	}
}

func cachedEndpointLookup(ctx context.Context, c *cache.RedisCache, rc *rest.Config, username, authnNS string) (endpoints.Endpoint, error) {
	cacheKey := cache.UserConfigKey(username)

	if c != nil {
		if raw, hit, _ := c.GetRaw(ctx, cacheKey); hit {
			ep, err := unmarshalEndpoint(raw)
			if err == nil {
				if span := trace.SpanFromContext(ctx); span.IsRecording() {
					span.AddEvent("user.config.cache_hit", trace.WithAttributes(
						attribute.String("username", username),
					))
				}
				slog.Debug("userconfig: cache hit", slog.String("user", username))
				return ep, nil
			}
		}
	}

	secretName := fmt.Sprintf("%s-clientconfig", kubeutil.MakeDNS1123Compatible(username))
	ep, err := endpoints.FromSecret(ctx, rc, secretName, authnNS)
	if err != nil {
		return endpoints.Endpoint{}, err
	}

	if c != nil {
		if data, merr := marshalEndpoint(ep); merr == nil {
			_ = c.SetRaw(ctx, cacheKey, data)
		}
	}

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.AddEvent("user.config.cache_miss", trace.WithAttributes(
			attribute.String("username", username),
		))
	}
	slog.Debug("userconfig: cache miss, fetched from API", slog.String("user", username))
	return ep, nil
}

// marshalEndpoint serializes an Endpoint to JSON using a map to include fields
// that the Endpoint struct excludes via json:"-" (ClientCertificateData, ClientKeyData).
func marshalEndpoint(ep endpoints.Endpoint) ([]byte, error) {
	m := map[string]string{
		"serverURL":  ep.ServerURL,
		"proxyURL":   ep.ProxyURL,
		"caData":     ep.CertificateAuthorityData,
		"clientCert": ep.ClientCertificateData,
		"clientKey":  ep.ClientKeyData,
		"token":      ep.Token,
		"username":   ep.Username,
		"password":   ep.Password,
		"awsAccess":  ep.AwsAccessKey,
		"awsSecret":  ep.AwsSecretKey,
		"awsRegion":  ep.AwsRegion,
		"awsService": ep.AwsService,
	}
	if ep.Debug {
		m["debug"] = "true"
	}
	if ep.Insecure {
		m["insecure"] = "true"
	}
	return json.Marshal(m)
}

func unmarshalEndpoint(data []byte) (endpoints.Endpoint, error) {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return endpoints.Endpoint{}, err
	}
	return endpoints.Endpoint{
		ServerURL:                m["serverURL"],
		ProxyURL:                 m["proxyURL"],
		CertificateAuthorityData: m["caData"],
		ClientCertificateData:    m["clientCert"],
		ClientKeyData:            m["clientKey"],
		Token:                    m["token"],
		Username:                 m["username"],
		Password:                 m["password"],
		Debug:                    m["debug"] == "true",
		Insecure:                 m["insecure"] == "true",
		AwsAccessKey:             m["awsAccess"],
		AwsSecretKey:             m["awsSecret"],
		AwsRegion:                m["awsRegion"],
		AwsService:               m["awsService"],
	}, nil
}
