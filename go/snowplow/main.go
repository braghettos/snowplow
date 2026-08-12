// Command snowplow is the snowplow server: it resolves Krateo RESTAction and
// frontend Widget custom resources into the JSON the Krateo frontend
// renders, served over /call. It wires the HTTP mux (/call, /health,
// /readyz, /debug/vars, pprof and swagger), the per-GVR dispatchers, the
// informer/L1 caches and prewarm engine, RBAC, and the in-cluster
// Kubernetes clients, then runs the server until signalled to shut down.
package main

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/plumbing/kubeutil"
	"github.com/krateo-platformops/plumbing/server/use"
	"github.com/krateo-platformops/plumbing/server/use/cors"
	_ "github.com/krateo-platformops/snowplow/docs"
	"github.com/krateo-platformops/snowplow/internal/authn"
	"github.com/krateo-platformops/snowplow/internal/cache"
	idynamic "github.com/krateo-platformops/snowplow/internal/dynamic"
	"github.com/krateo-platformops/snowplow/internal/handlers"
	"github.com/krateo-platformops/snowplow/internal/handlers/dispatchers"
	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
	"github.com/krateo-platformops/snowplow/internal/logging"
	"github.com/krateo-platformops/snowplow/internal/metrics"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	crdschema "github.com/krateo-platformops/snowplow/internal/resolvers/crds/schema"
	restactionsapi "github.com/krateo-platformops/snowplow/internal/resolvers/restactions/api"
	"github.com/krateo-platformops/snowplow/internal/support/audit"
	jqsupport "github.com/krateo-platformops/snowplow/internal/support/jq"
	"github.com/krateo-platformops/snowplow/internal/tracing"
	httpSwagger "github.com/swaggo/http-swagger"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

const (
	serviceName = "snowplow"

	// writeTimeout is the http.Server WriteTimeout. Go anchors the write
	// deadline to request-read time (t0), NOT to first-byte time
	// (net/http/server.go: deferred SetWriteDeadline(now+d) at request
	// read). So this t0-anchored deadline must clear the cache-OFF
	// heavy-path compute (~159s measured at 50K: full 50K paged LIST +
	// 50K per-item RBAC; #351/C2) before the handler's single buffered
	// Write, or the Write fails and the client gets HTTP 0. Cache-ON is
	// sub-4s so this longer deadline is never approached on the hot path
	// (zero warm blast radius). 300s = ~2x the measured worst-case
	// headroom. The LB is L4 passthrough (no HTTP timeout) so nothing
	// competes. See docs/c2-cacheoff-deliverability-trace-2026-06-13.md.
	writeTimeout = 300 * time.Second
)

var (
	build string
)

// snowplowCORSOptions returns the cors.Options the server's use.CORS wrapper is
// built from (main's http.Server construction). It is extracted from the inline
// literal into a named constructor so the CORS contract — in particular the
// ExposedHeaders that let the browser frontend READ the live-refresh signalling
// headers (X-Snowplow-Refresh-Key / X-Snowplow-Refresh-Class) plus Link — is
// unit-testable without standing up the full server (H4). Returning a freshly
// built value per call (no shared package-level struct) keeps the seam free of
// mutable shared state. The returned options are byte-identical to the previous
// inline literal.
func snowplowCORSOptions() cors.Options {
	return cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{
			"Accept",
			"Authorization",
			"Content-Type",
			"X-Auth-Code",
			"X-Krateo-TraceId",
			// W3C trace-context + baggage headers — REQUIRED so the
			// browser frontend can propagate traceparent/tracestate/
			// baggage into snowplow (cross-repo CORS dependency). D19a:
			// the audit session correlation id now rides `baggage`
			// (session.id), REPLACING the old X-Krateo-Correlation-Id
			// header, which is removed here.
			"traceparent",
			"tracestate",
			"baggage",
		},
		ExposedHeaders:   []string{"Link", "X-Snowplow-Refresh-Key", "X-Snowplow-Refresh-Class"},
		AllowCredentials: true,
		MaxAge:           300, // Maximum value not ignored by any of major browsers
	}
}

// @title SnowPlow API
// @version 0.1.0
// @description This the total new Krateo backend.
// @BasePath /
func main() {
	debugOn := flag.Bool("debug", env.Bool("DEBUG", false), "enable or disable debug logs")
	blizzardOn := flag.Bool("blizzard", env.Bool("BLIZZARD", false), "dump verbose output")
	prettyLog := flag.Bool("pretty-log", env.Bool("PRETTY_LOG", true), "print a nice JSON formatted log")
	port := flag.Int("port", env.ServicePort("PORT", 8081), "port to listen on")
	authnNS := flag.String("authn-namespace", env.String("AUTHN_NAMESPACE", ""),
		"krateo authn service clientconfig secrets namespace")
	// JWT verification keys come from authn's JWKS endpoint, not from a mounted
	// Secret: snowplow holds no copy of authn's public key, and authn can rotate
	// its keypair without a snowplow redeploy. Empty URL → derived from
	// --url-authn below, so the common case needs no extra configuration.
	jwksURL := flag.String("jwks-url", env.String("JWT_JWKS_URL", ""),
		"authn JWKS URL supplying the RS256 verification keys (empty → <url-authn>"+jwtutil.DefaultJWKSPath+")")
	jwksCacheTTL := flag.Duration("jwks-cache-ttl", env.Duration("JWT_JWKS_CACHE_TTL", 5*time.Minute),
		"how long a fetched JWKS key set is served before it is refreshed")
	jwksMinRefresh := flag.Duration("jwks-min-refresh-interval",
		env.Duration("JWT_JWKS_MIN_REFRESH_INTERVAL", 30*time.Second),
		"minimum gap between two JWKS fetch attempts; throttles refetches triggered by an unknown kid")
	jwksTimeout := flag.Duration("jwks-request-timeout", env.Duration("JWT_JWKS_REQUEST_TIMEOUT", 5*time.Second),
		"timeout for a single JWKS fetch; also bounds how long a token validation can block")
	jqModPath := flag.String("jq-modules-path", env.String(jqsupport.EnvModulesPath, ""),
		"loads JQ custom modules from the filesystem")
	// Ship Resilience-1 (0.30.162) — comma-separated list of
	// namespaces whose Deployment + Endpoints are watched for the
	// snowplow_upstream_controller_health gauge. Default
	// "krateo-system". Empty string → per-namespace Deployment +
	// Endpoints watches inert; cluster-scoped webhook-config
	// watches still run (so webhook gauge is populated). Discovery
	// of WHICH Deployments to watch is automatic via
	// MutatingWebhookConfiguration / ValidatingWebhookConfiguration
	// introspection — no hardcoded controller-name list
	// (feedback_no_special_cases).
	controllerHealthNS := flag.String("controller-health-namespaces",
		env.String("CONTROLLER_HEALTH_NAMESPACES", "krateo-system"),
		"comma-separated list of namespaces whose controllers are watched for Resilience-1 expvar gauges")

	// #57 (seed→authn→loopback token). The prewarm seed authenticates its
	// projected (audience-bound) SA token to authn for a real authn-issued
	// JWT, then propagates it through the nested loopback /call so the
	// composition-resources RAs warm (rca-prewarm-nested-loopback-jwt §5).
	urlAuthn := flag.String("url-authn", env.String("URL_AUTHN", "http://authn.krateo-system.svc.cluster.local:8082"),
		"krateo authn base URL for the seed SA→JWT token exchange (#57)")
	saTokenPath := flag.String("serviceaccount-token-path", env.String("SERVICEACCOUNT_TOKEN_PATH", ""),
		"path to the projected (audience=authn) ServiceAccount token (#57; empty → authn.DefaultTokenPath)")
	// #57 (C-b) self-loopback host: the bearer-append self-loopback arm
	// compares a resolved endpoint against this (exact scheme+host+port). A
	// /call api-step that resolves HERE is snowplow's own JWT-gated endpoint
	// and needs the seed bearer. PORT-anchored to the listen port.
	urlSelf := flag.String("url-self", env.String("URL_SELF", "http://snowplow.krateo-system.svc.cluster.local:8081"),
		"snowplow's own service URL — the self-loopback host for the #57 bearer-append arm (exact scheme+host+port match)")

	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Flags:")
		flag.PrintDefaults()
	}

	flag.Parse()

	// Ship 0.30.170-debug — enable Go runtime off-CPU profilers at boot
	// so /debug/pprof/mutex + /debug/pprof/block return non-empty data
	// under Chrome MCP cold-nav load. Required for the parallelism
	// regression investigation after Ship 0.30.169 RBAC index proved the
	// 2× ceiling is OFF-CPU (pod CPU utilization 13.43% during burst).
	// fraction=1 / rate=1 = sample every event; overhead is noise at
	// today's utilization. DEBUG BUILD — roll back to 0.30.169 once
	// profiles are captured.
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)

	os.Setenv("DEBUG", strconv.FormatBool(*debugOn))
	os.Setenv("TRACE", strconv.FormatBool(*blizzardOn))
	os.Setenv("AUTHN_NAMESPACE", *authnNS)
	os.Setenv(jqsupport.EnvModulesPath, *jqModPath)

	logLevel := slog.LevelInfo
	if *debugOn {
		logLevel = slog.LevelDebug
	}

	var lh slog.Handler
	if *prettyLog {
		lh = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:     logLevel,
			AddSource: false,
		})
	} else {
		lh = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:     logLevel,
			AddSource: false,
		})
	}

	log := slog.New(lh)
	// 0.30.172: route package-level slog calls (slog.InfoContext / .Info /
	// .DebugContext etc.) through the configured handler. Without this, any
	// emission via slog.* package-level functions goes to Go's uninitialised
	// default handler (text format to stderr) and is silently filtered out
	// by the JSON-only stdout log pipeline. Specifically caught the
	// dispatcher.call.complete log from per_call_log.go (0.30.171-debug)
	// that uses slog.InfoContext(ctx, ...) and never appeared in pod logs.
	slog.SetDefault(log)
	if *debugOn {
		log.Debug("environment variables", slog.Any("env", os.Environ()))
	}

	// authn signs tokens asymmetrically (RS256); snowplow verifies them against
	// the public key authn publishes at its JWKS endpoint. The key set is
	// fetched lazily (first validation, NOT at boot) and cached for
	// --jwks-cache-ttl, so:
	//   - snowplow starts and serves its unauthenticated routes even when authn
	//     is not up yet, and recovers on its own once authn answers — no startup
	//     ordering dependency and no crash loop on an authn restart;
	//   - a rotated kid triggers exactly one refetch (throttled by
	//     --jwks-min-refresh-interval), so no redeploy is needed to follow a
	//     keypair rotation;
	//   - authn stays off the hot path of every token validation.
	// Absent an explicit --jwks-url, derive it from the authn base URL already
	// configured for the #57 token exchange — one URL to get wrong, not two.
	resolvedJWKSURL := strings.TrimSpace(*jwksURL)
	if resolvedJWKSURL == "" {
		resolvedJWKSURL = jwtutil.JWKSURL(*urlAuthn)
	}
	if resolvedJWKSURL == "" {
		log.Error("no JWKS URL: set --jwks-url/JWT_JWKS_URL or --url-authn/URL_AUTHN")
		os.Exit(1)
	}
	jwtKeys := jwtutil.NewJWKSKeySource(resolvedJWKSURL,
		jwtutil.WithJWKSCacheTTL(*jwksCacheTTL),
		jwtutil.WithJWKSMinRefreshInterval(*jwksMinRefresh),
		jwtutil.WithJWKSRequestTimeout(*jwksTimeout))
	log.Info("JWT verification keys sourced from authn JWKS",
		slog.String("jwks_url", resolvedJWKSURL),
		slog.Duration("cache_ttl", *jwksCacheTTL),
		slog.Duration("min_refresh_interval", *jwksMinRefresh),
		slog.Duration("request_timeout", *jwksTimeout))

	// #57 — wire the prewarm seed's authn token source + the self-loopback
	// host once at startup. The authn.Client exchanges snowplow's projected
	// (audience=authn) SA token for an authn-issued JWT; the seed installs it
	// on the prewarm ctx so a nested loopback /call carries a bearer
	// snowplow's own middleware accepts (rca-prewarm-nested-loopback-jwt §5).
	// Both are additive + degrade-safe: if URL_SELF is empty the self-loopback
	// bearer arm never fires (pre-#57 gate); if the token exchange fails the
	// seed runs token-less (WARN+expvar, not fatal — phase1_walk C-a).
	authnClient := authn.New(*urlAuthn, *saTokenPath)
	dispatchers.SetSeedLoopbackTokenProvider(authnClient.Token)
	// R (composition-resources loopback guard) — install the SAME URL_SELF host
	// on the dispatcher-side ingest belt-and-suspenders check, mirroring
	// api.SetSelfHost below (both derive from the one URL_SELF value).
	dispatchers.SetSelfLoopbackHost(*urlSelf)
	if restactionsapi.SetSelfHost(*urlSelf) {
		log.Info("prewarm seed loopback auth wired (#57)",
			slog.String("url_authn", *urlAuthn),
			slog.String("url_self", *urlSelf),
			slog.String("hint", "seed exchanges its audience=authn SA token for a JWT + appends it on self-loopback /call steps (exact host match)"))
	} else {
		log.Info("prewarm seed self-loopback bearer arm DISABLED (#57)",
			slog.String("url_self", *urlSelf),
			slog.String("hint", "URL_SELF empty/unparseable — the self-loopback bearer-append arm is off; named-endpoint steps follow the pre-#57 gate"))
	}

	// OTel observability (ADDITIVE + default-OFF). Both Setup calls are
	// no-ops unless tracing/metrics resolve to enabled: the OTEL_ENABLED
	// master switch turns both on, and OTEL_TRACING_ENABLED /
	// OTEL_METRICS_ENABLED (each defaulting to OTEL_ENABLED) gate the
	// per-signal pipelines. With all unset they are false; in the off-path
	// they register nothing (no TracerProvider/MeterProvider,
	// no propagator, no exporter), so the global providers stay no-op and
	// every instrumentation site degrades to zero cost. This does NOT touch
	// the shortid X-Krateo-TraceId correlation id, the stdout->otel_logs log
	// pipeline, or the use.TraceId()/use.Logger() chain below — OTel
	// coexists with all three. The deferred shutdowns flush the batch
	// span/metric pipelines on graceful exit; they are no-ops when disabled.
	otelCtx := context.Background()
	traceShutdown, err := tracing.Setup(otelCtx, build)
	if err != nil {
		log.Error("otel tracing setup failed", slog.Any("err", err))
	}
	defer func() { _ = traceShutdown(context.Background()) }()

	metricShutdown, err := metrics.Setup(otelCtx, build)
	if err != nil {
		log.Error("otel metrics setup failed", slog.Any("err", err))
	}
	defer func() { _ = metricShutdown(context.Background()) }()

	// D19a — audit logs pipeline. Build the OTLP/HTTP LoggerProvider (same
	// OTEL_ENABLED gate + endpoint contract, same service.name=snowplow
	// Resource as the TracerProvider) so every AuditEvent is a first-class,
	// trace-correlated OTLP LogRecord on the shared otel_logs plane — NOT a
	// stdout JSON blob and NOT a bespoke correlation header. When the gate is
	// off, logging.Setup returns a nil provider and the audit emitter stays a
	// no-op (off-path byte-identical). This is a SEPARATE signal from the
	// existing stdout->otel-daemonset per-call diagnostic log, which is
	// untouched.
	logProvider, logShutdown, err := logging.Setup(otelCtx, build)
	if err != nil {
		log.Error("otel logs setup failed", slog.Any("err", err))
	}
	defer func() { _ = logShutdown(context.Background()) }()
	if logProvider != nil {
		audit.SetDefault(audit.New(
			logProvider.Logger("github.com/krateo-platformops/snowplow/audit"),
		))
	}

	chain := use.NewChain(
		use.TraceId(),
		use.Logger(log),
		// Audit correlation (D19a): mint/accept the business session id and
		// write it into W3C baggage (session.id), carried on the request
		// context and serialized downstream by the Baggage propagator. The
		// AuditEvent itself is emitted as a trace-correlated OTLP LogRecord.
		// ADDITIVE — coexists with the shortid X-Krateo-TraceId and the OTel
		// traceparent; no bespoke correlation header any more.
		audit.Middleware(),
	)

	// Wire the in-process RA/widget resolver seam (introduced Ship 0.30.123
	// #155 for the /call loopback; the loopback DISPATCH BRANCH was retired
	// 2026-06-22 and the seam repurposed as the in-process resolver behind the
	// DIRECT-APISERVER-PATH + `resolve: true` mechanism). When an api-step's
	// path is a direct apiserver path to a RESTAction/Widget CR with
	// resolve:true, the api resolver invokes this IN-PROCESS instead of issuing
	// an HTTP /call — so a JWT-less / SA-credentialed resolve can complete a
	// referenced-CR resolve the HTTP edge could not.
	//
	// Wired UNCONDITIONALLY at startup (NOT cache-gated): the seam is
	// cache-agnostic, so resolve:true resolves in-process under cache-ON AND
	// cache-OFF alike (Diego Option (ii) — transparent fallback). Under
	// cache-off the seam's objects.Get uses the user's own token and
	// ResolveNestedCall's in-process checkDispatchRBAC self-skips via
	// !cache.Disabled() — the user-token apiserver GET is the RBAC gate. A nil
	// resolver (this wiring skipped — tests only) is the structural fallback:
	// maybeResolveInProcess gate 2 then no-ops to the raw CR. The
	// RESOLVER_INPROCESS_NESTED_CALL flag was retired with the loopback branch;
	// the per-step `resolve` property (default true) is the only gate now.
	// Mirrors the api.resolveOnceFn seam pattern.
	restactionsapi.RegisterNestedCallResolver(dispatchers.ResolveNestedCall)

	// #57 — retired-flag startup audit. PREWARM_ENABLED and
	// RESOLVER_USE_INFORMER were folded into the single CACHE_ENABLED
	// master gate (prewarm + informer-pivot are now implicit-on-cache).
	// A stale value of either name in the env is functionally ignored
	// (the helpers no longer read them); this emits one audit line per
	// retired flag still present — Warn when set to "false" (a silent
	// behavior change: the operator asked for OFF and now gets ON) and
	// Info otherwise (a harmless no-op). Absent keys emit nothing. Placed
	// before the cache banner so the audit reads alongside the cache-mode
	// determination. See cache.AuditRetiredFlags (retired_flags.go).
	cache.AuditRetiredFlags(log)

	// Cache subsystem — Tag 0.30.4 (cache=on activation).
	//
	// When CACHE_ENABLED is unset / false / 0 / no, cache.Disabled()
	// returns true and cache.NewResourceWatcher returns (nil, nil)
	// without instantiating the dynamicinformer factory. No goroutines
	// spawn. Every consumer (objects.Get, dynamic.ListObjects,
	// rbac.UserCan, EvaluateRBAC) checks cache.Disabled() at the top
	// and falls back to the apiserver / SubjectAccessReview path.
	//
	// When CACHE_ENABLED=true:
	//   - the dynamic informer factory is constructed,
	//   - the four Role-Based Access Control GVRs are eagerly
	//     registered (Role, RoleBinding, ClusterRole, ClusterRoleBinding),
	//   - factory.Start() is invoked inside NewResourceWatcher,
	//   - cache.SetGlobal(rw) publishes the watcher for EvaluateRBAC.
	//
	// We then block (with a timeout) on WaitForCacheSync so the first
	// /call dispatched at startup does not race informer LISTs.
	cacheCtx, cacheCancel := context.WithCancel(context.Background())
	defer cacheCancel()
	// 0.30.207 — honor DEBUG on the background path. cacheCtx is a bare
	// context.Background() derivative; unlike the HTTP request path (which
	// runs use.Logger(log) → xcontext.WithLogger), it carries NO logger.
	// Every background-resolve site (refresher → resolveAndPopulateL1 →
	// resolver, prewarm walk, discovery refresher) calls
	// xcontext.Logger(ctx), which on a logger-less context falls back to a
	// HARDCODED slog.LevelDebug handler (plumbing/context.Logger) that
	// ignores the DEBUG env var entirely — so the hot L1-refresher loop
	// emits full-dict DEBUG lines even with DEBUG=false. Inject the
	// already-level-configured logger (Info when DEBUG=false, Debug when
	// DEBUG=true) so the background path obeys the flag. WithLogger wraps a
	// non-nil root verbatim (preserving its level), so this changes log
	// LEVEL gating only — never log content.
	cacheCtx = xcontext.BuildContext(cacheCtx, xcontext.WithLogger(log))

	var cacheWatcher *cache.ResourceWatcher
	if !cache.Disabled() {
		rc, rcErr := rest.InClusterConfig()
		if rcErr != nil {
			log.Warn("cache: rest.InClusterConfig failed; staying on apiserver branch",
				slog.Any("err", rcErr))
		} else {
			dynCli, dynErr := dynamic.NewForConfig(rc)
			if dynErr != nil {
				log.Warn("cache: dynamic.NewForConfig failed; staying on apiserver branch",
					slog.Any("err", dynErr))
			} else {
				w, wErr := cache.NewResourceWatcher(cacheCtx, dynCli)
				if wErr != nil {
					log.Warn("cache: NewResourceWatcher failed; staying on apiserver branch",
						slog.Any("err", wErr))
				} else {
					cacheWatcher = w
					cache.SetGlobal(w)

					// Ship 0.30.233 — wire the SA *rest.Config as a
					// process singleton so the CRD-ADD discovery
					// side-effect (crd_discovery_side_effect.go) can
					// invoke DiscoverGroupResources without a per-call
					// ctx. The informer processor goroutine has no
					// InternalRESTConfigFromContext attached; the
					// singleton is the bridge between the informer
					// event surface and the discovery API. Mirrors the
					// SetGlobal pattern above — single set at startup,
					// idempotent, soft-fails to walker-only discovery
					// if ever unset.
					cache.SetProcessSARestConfig(rc)

					// Task #322 (#318-R2) Commit 1 — wire the SA-singleton
					// cached-discovery invalidation callback so the
					// CRD-lifecycle bridge (crd_discovery_side_effect.go)
					// can invalidate the dynamic-package discovery
					// singleton AFTER DiscoverGroupResources / teardown
					// without an import cycle (cache is below dynamic).
					// Same set-once-at-startup lifecycle as
					// SetProcessSARestConfig above; soft no-op if unset.
					cache.SetSADiscoveryInvalidator(idynamic.InvalidateSADiscovery)

					// Task #323 (#318-R2 Commit 2-B) — wire the per-GVR
					// compiled-CRD-schema memo reset callback so the SAME
					// CRD-lifecycle bridge resets the schema memo in lockstep
					// with the SA-discovery cache (fired right after
					// invalidateSADiscovery in triggerCRDDiscovery/Delete).
					// Same set-once-at-startup lifecycle; soft no-op if unset.
					// crds/schema is above cache in the import graph, so the
					// trampoline lives in cache and main.go injects the impl.
					cache.SetCRDSchemaInvalidator(crdschema.InvalidateCRDSchemaMemo)

					// Ship 0.30.122 R4 Lever 1: wire the in-cluster
					// *rest.Config so the composition GVR's streaming
					// ListWatch can issue raw paged-LIST HTTP requests and
					// stream the response body item-by-item (the dynamic
					// client only returns a fully-materialised list). Same
					// post-construction wiring pattern as SetMetadataClient.
					// When unset the composition GVR falls back to the
					// standard NewFilteredDynamicInformer.
					w.SetRESTConfig(rc)

					// §0.30.93 (Revision 18): wire the metadata client
					// for the metadata-only informer routing path.
					// Composition GVRs (~50K objects at production
					// scale) take this path to stay within the 1.8 GiB
					// RSS budget; RBAC GVRs and customer CRDs without
					// the `krateo.io/cache-mode: metadata` annotation
					// continue on the dynamic full-informer path.
					//
					// Failure to construct the metadata client leaves
					// rw.metaClient == nil — `EnsureResourceType` then
					// emits `cache.lazy_register.metadata_only_unwired`
					// for every Composition GVR touch (loud SRE signal
					// without crash-looping the pod).
					metaCli, metaErr := metadata.NewForConfig(rc)
					if metaErr != nil {
						log.Warn("cache: metadata.NewForConfig failed; metadata-only routing offline (full-informer fallback)",
							slog.Any("err", metaErr))
					} else {
						w.SetMetadataClient(metaCli)
					}

					// 0.30.98 Tag A: wire the discovery client for the
					// four-conjunct servability gate's conjunct 4
					// (resourceTypeConfirmed — the S4 fix). We use a RAW
					// (uncached) discovery.DiscoveryClient deliberately:
					// the discovery-refresh ticker MUST observe a
					// post-startup CRD's group/version transitioning from
					// un-served to served, and a memcache-backed client
					// would mask that transition until an explicit
					// Invalidate(). The ticker calls
					// ServerResourcesForGroupVersion once per ~30s per
					// registered group/version (deduped) — negligible
					// apiserver load.
					//
					// Failure to construct the discovery client leaves
					// rw.disco == nil; resourceTypeConfirmedLocked then
					// defaults to true (the pivot keeps its pre-0.30.98
					// HasSynced-only behaviour). The S4 fix is degraded
					// but not crash-looping — a loud WARN flags it.
					discoCli, discoErr := discovery.NewDiscoveryClientForConfig(rc)
					if discoErr != nil {
						log.Warn("cache: discovery.NewDiscoveryClientForConfig failed; resource-type confirmation offline (S4 gate degrades to HasSynced-only)",
							slog.Any("err", discoErr))
					} else {
						w.SetDiscoveryClient(discoCli)
						// Launch the ~30s discovery-refresh ticker. It
						// primes confirmation once immediately, then
						// re-confirms every registered GVR's resource
						// type on each tick — flipping post-startup CRDs
						// unconfirmed->confirmed and clearing watchBroken
						// on a successful relist. Bound by cacheCtx +
						// rw.stopCh.
						w.StartDiscoveryRefresher(cacheCtx)
					}

					// §0.30.93 annotation discovery: one apiextensions
					// LIST at startup to find CRDs carrying
					// `krateo.io/cache-mode: metadata`. Bounded by
					// 30 s; soft-fail (annotation set stays empty, the
					// static seed in `internal/cache/cache_mode.go`
					// still routes Composition GVRs to metadata-only).
					discoCtx, discoCancel := context.WithTimeout(cacheCtx, 30*time.Second)
					cache.DiscoverMetadataOnlyAnnotations(discoCtx, rc)
					discoCancel()

					// 0.30.8: wire the L1 resolved-output cache refresher.
					// Order matters: register dispatcher handlers BEFORE
					// StartRefresher so the worker pool sees them on
					// first dequeue, and BEFORE the watcher starts
					// emitting UPDATE events (which it already may be
					// doing — NewResourceWatcher calls factory.Start
					// internally for the RBAC GVRs). Idempotent on
					// duplicate calls.
					//
					// 0.30.113 Part B: pass the in-cluster *rest.Config as
					// the background-refresh SA transport — a refresh has
					// no live per-user token; the widget resolver needs an
					// apiserver client-config on the context.
					dispatchers.RegisterRefreshHandlers(rc)
					// Ship 2 Stage 2.5 / 0.30.248 (Fix v2 engine ctx
					// decoupling). The prewarm engine worker reads this
					// ctx as its long-lived runtime — it stops only on
					// process shutdown, so post-boot
					// scopeKindGVRDiscovered enqueues (from
					// cache.DiscoverGroupResources's `if added` branch)
					// are actually consumed. Pre-Fix-v2 the worker
					// inherited the boot-seed goroutine's bounded ctx
					// and died at boot-done, leaving post-boot enqueues
					// unprocessed (Trace v2 §1.5).
					//
					// MUST be wired BEFORE Phase1Warmup runs (line 594
					// below — Phase1Warmup's engineSeed closure reads
					// engineProcessCtx). Mirrors cache.StartRefresher's
					// cacheCtx-as-process-lifetime pattern (line 320).
					dispatchers.SetEngineProcessContext(cacheCtx)
					// BOOT-RACE-TOLERANT prewarm (shape A,
					// docs/prewarm-boot-race-tolerant-2026-07-03.md §2.2).
					// Register the single-object config-vars ConfigMap
					// informer on the PROCESS-LIFETIME cacheCtx (NOT p1Ctx),
					// coherent with SetEngineProcessContext just above: its
					// AddFunc/UpdateFunc enqueue a scopeKindBoot re-drive on
					// the SAME engine singleton the boot seed starts, so a
					// frontend that creates the *-config-vars ConfigMap AFTER
					// snowplow booted (fresh install, inverted order) drives
					// the prewarm walk the instant the ConfigMap lands —
					// before OR after the readiness backstop, zero restart.
					// enqueueScope is safe before the worker starts (it
					// populates the bounded dedup queue; the worker drains it
					// on StartPrewarmEngine inside Phase1Warmup's engineSeed).
					// Gated on the engine posture (#341: PREWARM_ENGINE_ENABLED
					// =true is the production posture) — the enqueue is
					// meaningless without the engine that processes boot scopes.
					if cache.PrewarmEnabled() && dispatchers.PrewarmEngineEnabled() {
						dispatchers.StartConfigVarsWatch(cacheCtx, rc, *authnNS)
						// #102 c1 — the TTL-cadenced quiet-page keep-warm sweep.
						// Anchored to the PROCESS-LIFETIME cacheCtx (same as the
						// config-vars watch + engine worker) so the ticker fires
						// for the pod lifetime. It enqueues a scopeKindKeepwarm on
						// the SAME engine singleton at TTL×3/4, re-seeding rank-1's
						// cells before they lazy-expire. Gated identically (the
						// sweep is meaningless without the engine that processes it).
						dispatchers.StartKeepwarmSweep(cacheCtx)
					}
					// Ship #98 / 0.30.215 — wire the customer-priority
					// yield hook BEFORE StartRefresher so the worker pool
					// sees a populated hook on its first processNext.
					// Mirrors the prewarm engine's customer-inflight signal
					// (prewarm_engine.go:88-105) — one atomic-int64 read per
					// refresher yield-poll tick. Cache-off is a no-op
					// (StartRefresher returns early). The hook is the ONE
					// seam between cache and dispatchers; cache cannot
					// import dispatchers (cycle), so dispatchers injects
					// its predicate via cache.SetCustomerInflightHook.
					cache.SetCustomerInflightHook(dispatchers.CustomerInFlight)
					cache.StartRefresher(cacheCtx)
					// Ship #91 / 0.30.211 — Lever C async invalidator worker.
					// Bounded queue, drop-on-full. Receives raKey enqueues
					// from the deps refresh hook for stuck-false memo
					// invalidation. NOT on the refresher workqueue
					// (feedback_refresher_populate_amplification). Cache-off
					// is a no-op inside the Start fn.
					cache.StartSliceabilityReverifier(cacheCtx)

					// Block until RBAC informer LISTs complete so the
					// first dispatch is not racing the initial sync.
					// Bounded at 60s — soft failure (log + continue),
					// not fatal.
					syncCtx, syncCancel := context.WithTimeout(cacheCtx, 60*time.Second)
					if err := w.WaitForCacheSync(syncCtx, 60*time.Second); err != nil {
						log.Warn("cache: initial WaitForCacheSync incomplete; first dispatches may evaluate against partial RBAC index",
							slog.Any("err", err))
					} else {
						log.Info("cache: RBAC informers fully synced")
					}
					syncCancel()

					// Ship 0.30.189 — sentinel-collision sanity check.
					// `groupOnlyCohortSentinel` ("system:cohort:group-only:v1")
					// is the synthetic identity used to normalise the
					// authenticated-group-only cohort so the PIP seed and
					// the request-time dispatcher hash to the same L1
					// cell. Invariant: CRBsByUser[sentinel] == ∅ and
					// RBsByUserByNS[ns][sentinel] == ∅ for all ns. A real
					// User-kind subject literally named that string would
					// break the invariant (the sentinel would gain real
					// bindings → group-only cohort would leak admin-like
					// access at hash time). Standard k8s admission
					// rejects `system:*` user names; this check guards
					// against misconfigured clusters / test fixtures by
					// failing loud at boot rather than silently at
					// request time.
					if snap := cache.LiveRBACSnapshot(); snap != nil {
						const sentinel = "system:cohort:group-only:v1"
						if n := len(snap.CRBsByUser[sentinel]); n > 0 {
							log.Error("cache: sentinel collision — a ClusterRoleBinding has subject Name="+sentinel,
								slog.Int("crb_count", n),
								slog.String("hint", "rename the offending subject or change groupOnlyCohortSentinel; sentinel must be unique"),
							)
							os.Exit(1)
						}
						for ns, byUser := range snap.RBsByUserByNS {
							if n := len(byUser[sentinel]); n > 0 {
								log.Error("cache: sentinel collision — a RoleBinding has subject Name="+sentinel,
									slog.String("namespace", ns),
									slog.Int("rb_count", n),
									slog.String("hint", "rename the offending subject or change groupOnlyCohortSentinel; sentinel must be unique"),
								)
								os.Exit(1)
							}
						}
					}

					// Ship D.2 (0.30.143) — F-3 cache. Start the
					// AUTHN_NAMESPACE-scoped Secrets informer so the
					// per-user `<user>-clientconfig` lookup the
					// resolver mapper runs (endpoints.go:67) is
					// served from in-process instead of the
					// apiserver. Soft-fail by design — a wiring error
					// leaves the cache inert and the resolver mapper
					// falls back to plumbing's endpoints.FromSecret
					// (the pre-D.2 path).
					//
					// AUTHN_NAMESPACE empty → no-op (production sets
					// it via the chart values; dev/test may leave
					// blank). AssertSecretsInformerWired runs after
					// to surface the misconfiguration.
					if *authnNS != "" {
						if err := cache.StartSecretsInformer(cacheCtx, rc, *authnNS); err != nil {
							log.Warn("cache: StartSecretsInformer failed; F-3 cache offline (upstream fallback)",
								slog.String("authn_ns", *authnNS),
								slog.Any("err", err))
						}
					}
					cache.AssertSecretsInformerWired()

					// Ship Resilience-1 (0.30.162) — upstream-controller
					// health watch surface. Default scope is
					// krateo-system; multi-namespace via comma-
					// separated chart override. Soft-fail by design —
					// wiring errors leave the gauges publishing empty
					// maps and the rest of snowplow runs byte-
					// identical to pre-Resilience-1. CACHE_ENABLED=false
					// has already short-circuited inside Start (no
					// goroutine, no client built) — this whole block
					// only runs when cache is on.
					controllerHealthNamespaces := splitCommaList(*controllerHealthNS)
					if err := cache.StartControllerHealthInformer(cacheCtx, rc, controllerHealthNamespaces); err != nil {
						log.Warn("cache: StartControllerHealthInformer failed; Resilience-1 gauges offline",
							slog.Any("err", err))
					}

					// Tag 0.30.6 binding: walk every RestAction in the
					// cluster, derive the GVR set referenced by
					// spec.api[*].path, and eager-register the lot.
					// Bound by STARTUP_TIMEOUT_SECONDS (default 120);
					// fan-in by STARTUP_INFORMER_FANIN (default 8).
					// Soft failure: log + continue; the lazy fallback
					// in AddResourceType handles the gap.
					//
					// As of 2026-05-13 post-mortem (tag 0.30.61): gated off
					// by default because no consumer reads from the
					// eagerly-registered informers at this tag — the
					// resolver still calls apiserver directly, so eager
					// registration is pure apiserver pressure. Bench at
					// 0.30.6 showed 3× S7/S8 convergence regression + new
					// S6b VERIFY TIMEOUT vs 0.30.5. Re-enable when
					// resolver-cache wiring lands. Set
					// EAGER_REGISTER_ENABLED=true to opt-in (will cause
					// apiserver pressure at 50K scale per bench data;
					// see project_regression_journal.md 2026-05-13).
					//
					// The inventory walker (inventory.go), eager.go, and
					// the watcher.go eagerSet plumbing are intentionally
					// preserved as dormant library code.
					if os.Getenv("EAGER_REGISTER_ENABLED") == "true" {
						log.Info("eager-register: enabled via EAGER_REGISTER_ENABLED=true",
							slog.String("subsystem", "cache"))
						fanin := env.Int("STARTUP_INFORMER_FANIN", 8)
						startupTimeout := time.Duration(env.Int("STARTUP_TIMEOUT_SECONDS", 120)) * time.Second
						invCtx, invCancel := context.WithTimeout(cacheCtx, startupTimeout)
						inv, invErr := cache.CollectResourceTypesFromRestActions(invCtx, dynCli)
						if invErr != nil {
							log.Warn("cache: RestAction inventory walk failed; falling through to lazy registration",
								slog.Any("err", invErr))
							// Mark eager-done with an empty set so any
							// post-startup AddResourceType is treated as
							// expected-lazy (no WARN spam).
							w.MarkEagerSet([]schema.GroupVersionResource{})
						} else {
							n, regErr := cache.EagerRegisterAll(invCtx, w, inv, fanin, startupTimeout)
							if regErr != nil {
								log.Warn("cache: eager registration WaitForCacheSync incomplete",
									slog.Int("resource_types", n),
									slog.Any("err", regErr))
							}
							w.MarkEagerSet(inv)
						}
						invCancel()
					} else {
						// 0.30.9 Sub-scope B (Revision 17): lazy
						// registration of resolver-touched GVRs is
						// the production default for DELETE-evict.
						// The dispatcher hot path calls
						// cache.Global().EnsureResourceType(gvr) on
						// every dep-edge record, so the informer
						// (and the watcher.go UpdateFunc/DeleteFunc
						// handlers) comes online on first touch.
						// EAGER_REGISTER_ENABLED=true remains
						// available as an OPTIONAL warm-start knob
						// for bench/large-customer scenarios that
						// want cold-zero on the first request; the
						// cost is a startup memory burst (OOM'd at
						// 50K bench scale on 0.30.8 — see
						// project_regression_journal.md 2026-05-13).
						log.Info("eager-register: disabled (default); lazy-register-on-resolver-touch provides DELETE-evict",
							slog.String("subsystem", "cache"),
							slog.String("rationale", "0.30.9 Sub-scope B: informers wired on first dep-record via EnsureResourceType; bounded memory at production scale"),
							slog.String("override_hint", "set EAGER_REGISTER_ENABLED=true for warm-start (bench-only; costs startup memory)"),
						)
						// Mark eager-done with an empty set so any
						// post-startup AddResourceType is treated as
						// expected-lazy (no WARN spam from the eagerSet
						// gate in watcher.AddResourceType).
						w.MarkEagerSet([]schema.GroupVersionResource{})

						// 0.30.99 Tag B — startup navigation GVR-walk.
						//
						// #130 1.7.5 — IMPLICIT-ON under CACHE_ENABLED. We are
						// inside the !cache.Disabled() branch, so the cache
						// subsystem is on and the walk now runs by DEFAULT here;
						// only an explicit PREWARM_REGISTER_ENABLED=false opts
						// out (emergency lever). This INVERTS the pre-1.7.5
						// default-OFF gate. resolvePrewarmRegisterDefault encodes
						// the contract (unset/"true"/other => ON, "false" => OFF)
						// and is unit-tested. Not in the chart configmap: absent
						// now means ON (was OFF).
						//
						// Why implicit-on is safe (TRACED — supersedes the
						// obsolete default-OFF "no-consumer QPS / OOM" rationale,
						// which was falsified; the walk is NOT the "same 58 GVRs"):
						//
						//   - The walk registers ONLY the STATIC (non-JQ-templated)
						//     inventory GVR subset. Every composition.krateo.io path
						//     is JQ-templated and is REJECTED by the ${-guard in
						//     ParseAPIServerPathToGVR (inventory.go:171) — so the
						//     walk CANNOT register composition GVRs. Its incremental
						//     footprint is the trivial static/CRD-meta GVR class.
						//   - The composition informers (the OOM-relevant mass at
						//     50K) are registered LAZILY via the
						//     DiscoverGroupResources / EnsureResourceType hook on the
						//     resolver inner-call path (resolve.go) REGARDLESS of
						//     this flag. Flipping it neither adds nor removes them,
						//     so it does not touch the 0.30.8/0.30.92 OOM surface.
						//   - A consumer now EXISTS for the walk-registered
						//     informers: the #130 F1 confirm-prime + the
						//     informer-serve branch + the Phase 1 seed read them.
						//     The 0.30.61 "no consumer reads the eagerly-registered
						//     informers" regression the default-OFF gate guarded
						//     against no longer applies (pivot is implicit-on-cache,
						//     #57).
						if resolvePrewarmRegisterDefault(os.Getenv("PREWARM_REGISTER_ENABLED")) {
							log.Info("prewarm-register: enabled (implicit-on under CACHE_ENABLED; set PREWARM_REGISTER_ENABLED=false to opt out)",
								slog.String("subsystem", "cache"),
								slog.String("hint", "startup navigation GVR-walk active; registers the STATIC (non-${-templated) inventory GVR subset only — composition GVRs arrive lazily via the resolver DiscoverGroupResources hook regardless of this flag"),
							)
							// Soft failure: a LIST error is logged +
							// ignored — the lazy register-on-navigation
							// fallback still covers every GVR on first
							// request. Bound by a fresh timeout so a
							// stalled apiserver cannot wedge boot.
							pwCtx, pwCancel := context.WithTimeout(cacheCtx,
								time.Duration(env.Int("STARTUP_TIMEOUT_SECONDS", 120))*time.Second)
							reg, present, pwErr := w.PrewarmRegisterFromNavigation(pwCtx, dynCli)
							pwCancel()
							if pwErr != nil {
								log.Warn("cache: startup navigation GVR-walk incomplete; lazy register-on-navigation covers the gap",
									slog.Any("err", pwErr))
							} else {
								log.Info("cache: startup navigation GVR-walk done",
									slog.String("subsystem", "cache"),
									slog.Int("registered", reg),
									slog.Int("already_present", present))
							}
						} else {
							log.Info("prewarm-register: disabled via explicit PREWARM_REGISTER_ENABLED=false opt-out",
								slog.String("subsystem", "cache"),
								slog.String("rationale", "explicit emergency opt-out (PREWARM_REGISTER_ENABLED=false); the walk registers only the static non-${-templated inventory GVR subset — composition informers still arrive lazily via the resolver DiscoverGroupResources hook, so this opt-out does NOT change the composition-informer footprint"),
							)
						}

						// 0.30.102 Tag B — Phase 1 SA-credentialed
						// resolution walk + CRD-watch + probe-gated
						// readiness. #57: implicit-on-cache —
						// cache.PrewarmEnabled() now folds to
						// !cache.Disabled(), so reaching here (inside the
						// !cache.Disabled() block) it is always true; the
						// standalone PREWARM_ENABLED flag was retired.
						// Distinct from the 0.30.99
						// PREWARM_REGISTER_ENABLED GVR-walk above: Tag B
						// resolves the routesloaders navigation roots
						// under SA identity (discovering GVRs by
						// resolution, not from a configured list) and
						// BLOCKS readiness on every navigated informer
						// reaching HasSynced.
						//
						// Phase1Warmup BLOCKS on the informer sync
						// barrier, so it runs on its own goroutine — the
						// HTTP server (incl. /readyz) must come up while
						// Phase 1 is still warming so the readinessProbe
						// observes 503 during warmup. The goroutine's
						// lifecycle is bounded by both the derived timeout
						// context AND cacheCtx; it terminates when
						// Phase1Warmup returns.
						//
						// When the cache subsystem is OFF, prewarm is
						// implicit-off: this branch is unreachable (the
						// enclosing !cache.Disabled() guard) and the
						// readiness safety-net below calls
						// cache.MarkPhase1Done() immediately — /readyz
						// then returns 200 from the first probe (no-op
						// gate; transparent cache-off fallback).
						if cache.PrewarmEnabled() {
							log.Info("prewarm: Phase 1 startup warmup enabled (implicit-on-cache, #57)",
								slog.String("subsystem", "cache"),
								slog.String("hint", "SA-credentialed routesloaders resolution walk + CRD-watch; /readyz gates on Phase1Done"),
							)
							// PHASE1_TIMEOUT_SECONDS bounds the whole walk +
							// sync barrier. Default 900s — aligned with the
							// chart's startupProbe budget (failureThreshold
							// 90 * periodSeconds 10). A separate knob from
							// STARTUP_TIMEOUT_SECONDS (120s) because Phase 1
							// resolves the full navigation surface and may
							// legitimately take minutes at production scale.
							phase1Timeout := time.Duration(env.Int("PHASE1_TIMEOUT_SECONDS", 900)) * time.Second
							go func() {
								p1Ctx, p1Cancel := context.WithTimeout(cacheCtx, phase1Timeout)
								defer p1Cancel()
								if err := dispatchers.Phase1Warmup(p1Ctx, rc, *authnNS); err != nil {
									log.Warn("cache: Phase 1 startup warmup incomplete",
										slog.String("subsystem", "cache"),
										slog.Any("err", err))
								}
								// Phase1Warmup always calls
								// cache.MarkPhase1Done internally before it
								// returns — /readyz is now 200.
							}()
						} else {
							// #57: PrewarmEnabled() is implicit-on-cache, so
							// inside this !cache.Disabled() block it is always
							// true — this else is defensively retained (the
							// gate symbol is preserved) but unreachable under
							// cache-on. The cache-off no-op path lives in the
							// readiness safety-net below.
							log.Info("prewarm: Phase 1 startup warmup not scheduled",
								slog.String("subsystem", "cache"),
								slog.String("rationale", "prewarm is implicit-on-cache (#57); the cache-off no-op flips Phase1Done via the readiness safety-net"),
							)
							// Nothing to warm — flip the readiness gate now
							// so /readyz is an immediate-200 no-op.
							cache.MarkPhase1Done()
						}
					}
				}
			}
		}
	} else {
		// 0.30.71 — "true cache-off" diagnostic mode.
		//
		// CACHE_ENABLED=false unconditionally disables the L1
		// resolved-output cache, the typed-RBAC indexer, and the
		// informer factory. We still construct a passthrough
		// ResourceWatcher (when in-cluster config is available) so
		// the watcher API stays callable; every Get/List call routes
		// to apiserver via the dynamic client. NewResourceWatcher
		// emits the loud WARN diagnostic-mode banner so operators
		// see immediately that ALL caching is off.
		//
		// When in-cluster config or dynamic.NewForConfig fails (e.g.
		// running outside a cluster for unit tests), we fall back to
		// the pre-0.30.71 nil-watcher shape — consumers nil-check
		// cache.Global() and take their own apiserver branch.
		rc, rcErr := rest.InClusterConfig()
		if rcErr != nil {
			log.Info("cache: rest.InClusterConfig unavailable in disabled mode; watcher will be nil",
				slog.Any("err", rcErr))
			_, _ = cache.NewResourceWatcher(cacheCtx, nil)
		} else {
			dynCli, dynErr := dynamic.NewForConfig(rc)
			if dynErr != nil {
				log.Warn("cache: dynamic.NewForConfig failed in disabled mode; watcher will be nil",
					slog.Any("err", dynErr))
				_, _ = cache.NewResourceWatcher(cacheCtx, nil)
			} else {
				w, wErr := cache.NewResourceWatcher(cacheCtx, dynCli)
				if wErr != nil {
					log.Warn("cache: NewResourceWatcher failed in disabled mode; watcher will be nil",
						slog.Any("err", wErr))
				} else if w != nil {
					cacheWatcher = w
					cache.SetGlobal(w)
				}
			}
		}
	}
	defer func() {
		if cacheWatcher != nil {
			cacheWatcher.Stop()
			cache.SetGlobal(nil)
		}
	}()

	// 0.30.102 Tag B — readiness-gate safety net. The Tag B block above
	// flips the Phase1Done gate (immediately when prewarm is off, or
	// asynchronously at the tail of Phase1Warmup when on). #57: prewarm is
	// implicit-on-cache, so "prewarm off" == cache off. Several startup
	// paths bypass that block entirely: CACHE_ENABLED=false (diagnostic
	// passthrough) or a cache-setup failure (nil watcher). On any such
	// path there is nothing to warm, so /readyz must still return 200.
	// When the cache subsystem is ON AND a watcher exists, the Phase1Warmup
	// goroutine owns the flip — do NOT pre-flip here, or the
	// premature-Ready invariant breaks.
	//
	// 0.30.153 — the three-disjunct invariant is encoded in
	// cache.ShouldFlipPhase1DoneOnStartup so the cache-off case
	// (CACHE_ENABLED=false + non-nil passthrough watcher) is covered. The
	// prior 2-disjunct condition missed that case; pod was stuck
	// `{"status":"warming","phase1Done":false}` forever, Service endpoints
	// empty, snowplow LB unroutable. #57 preserves the 3-arg signature
	// (cache.PrewarmEnabled() folds to !Disabled(), so the middle disjunct
	// now equals the first) — the named helper is the regression's encoded
	// falsifier.
	if cache.ShouldFlipPhase1DoneOnStartup(
		!cache.Disabled(),
		cache.PrewarmEnabled(),
		cacheWatcher == nil,
	) {
		cache.MarkPhase1Done()
	}

	mux := http.NewServeMux()

	mux.Handle("GET /swagger/", httpSwagger.WrapHandler)
	//mux.Handle("POST /convert", chain.Then(handlers.Converter()))

	mux.Handle("GET /health", handlers.HealthCheck(serviceName, build, kubeutil.ServiceAccountNamespace))
	mux.Handle("GET /readyz", handlers.ReadyCheck())
	// Ship D (0.30.141, architectural-consistency invariant) —
	// /api-info/names is `/call`-class read; scope as `plurals` so the
	// invariant counter classifies any apiserver fall-through from this
	// route. cache.FallthroughScopeMiddleware is a no-op when
	// CACHE_ENABLED=false (project_caching_is_provisional).
	mux.Handle("GET /api-info/names", chain.Append(
		cache.FallthroughScopeMiddleware(cache.ScopePlurals)).
		Then(handlers.Plurals()))
	cache.RegisterScopedRoute("GET /api-info/names", cache.ScopePlurals)

	mux.Handle("GET /list", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeList)).
		Then(handlers.List()))
	cache.RegisterScopedRoute("GET /list", cache.ScopeList)

	// Ship D — GET /call is the canonical `/call` read path. Dispatcher
	// routes into the per-group restactions / widgets handlers; for the
	// scope label we use call-generic (the dispatcher's fallthrough lane
	// is the F-1 site). The restactions / widgets dispatcher entries
	// inherit the scope via the same ctx.
	mux.Handle("GET /call", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallGeneric),
		handlers.Dispatcher(dispatchers.All())).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("GET /call", cache.ScopeCallGeneric)

	// GET /export — GENERIC export: any /call-resolvable list
	// (RESTAction or list/table Widget) serialized as a CSV/JSON
	// attachment. The handler re-dispatches IN-PROCESS through the same
	// dispatcher lane the GET /call route uses (identical auth, RBAC gate
	// and serve-time user-aware filtering — export can never see more
	// than the caller's own /call), then serializes the extracted rows.
	// The route shares the /call middleware chain, so the scope
	// classification and the read-path invariant hold; a scheduled
	// export is just a CronJob curling this route (see howto/export.md).
	mux.Handle("GET /export", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallGeneric)).
		Then(handlers.Export(
			handlers.Dispatcher(dispatchers.All())(handlers.Call()))))
	cache.RegisterScopedRoute("GET /export", cache.ScopeCallGeneric)

	// Ship 1 (live-refresh-coherence, option A) — GET /refreshes is the
	// per-subject live-refresh SSE stream. It uses middleware.RefreshAuth
	// (cookie-or-header JWT -> UserInfo) instead of middleware.UserConfig,
	// because a browser EventSource cannot set the Authorization header and
	// /refreshes needs no <user>-clientconfig Secret lookup (it issues ZERO
	// apiserver reads). It is DELIBERATELY NOT cache.RegisterScopedRoute'd:
	// with no apiserver reads it is outside the read-path-scoped invariant
	// (AssertReadPathsScoped, main.go below), and requiredScopedRoutes
	// (fallthrough_assert.go) does not list it. Under CACHE_ENABLED=false or
	// REFRESH_SSE_ENABLED=false the handler serves a clean idle stream
	// (transparent fallback, project_cache_off_is_transparent_fallback).
	mux.Handle("GET /refreshes", chain.Append(
		middleware.RefreshAuth(jwtKeys)).
		Then(handlers.Refreshes()))

	// GET /rbac — RESTAction read-set enumeration for core-provider RBAC
	// pre-generation (design docs/restaction-rbac-endpoint-design.md). It
	// resolves a referenced RESTAction's api[] stages to the (group, version,
	// resource, verb) tuples a /call WOULD read, WITHOUT dispatching.
	// Authenticated with the SAME JWT+clientconfig as /call (middleware.UserConfig):
	// the JWT authenticates the caller, but the enumeration uses NONE of the
	// caller's RBAC perms (it runs under the SA), so the read-set is computable
	// before any binding exists. DELIBERATELY NOT cache.RegisterScopedRoute'd:
	// like /refreshes above, it issues ZERO per-user apiserver reads, so it sits
	// outside the read-path-scoped invariant (fallthrough_assert.go).
	mux.Handle("GET /rbac", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS)).
		Then(handlers.RBAC()))

	// Ship D — write-verb `/call` routes also get the middleware (PM
	// explicit). Write verbs are out of the read-path invariant (F-11
	// in the design), but centralizing classification prevents silent
	// escapes: a future GET-class route mistakenly registered under
	// `call-write-*` would still trip the counter on the wrong cell.
	mux.Handle("POST /call", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWritePost)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("POST /call", cache.ScopeCallWritePost)

	mux.Handle("PUT /call", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWritePut)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("PUT /call", cache.ScopeCallWritePut)

	mux.Handle("PATCH /call", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWritePatch)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("PATCH /call", cache.ScopeCallWritePatch)

	mux.Handle("DELETE /call", chain.Append(
		middleware.UserConfig(jwtKeys, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWriteDelete)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("DELETE /call", cache.ScopeCallWriteDelete)

	mux.Handle("POST /jq", chain.Append(middleware.UserConfig(jwtKeys, *authnNS)).Then(handlers.JQ()))

	// /debug/pprof/* — registered on the custom mux (server does NOT use http.DefaultServeMux).
	// Exposes goroutine, heap, profile, allocs, mutex, block, cmdline, symbol, threadcreate, trace.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	// Ship D.1 (0.30.142) — mount expvar.Handler on the snowplow mux so
	// Ship D's counters (snowplow_apiserver_fallthrough_total,
	// snowplow_apiserver_fallthrough_cells,
	// snowplow_assertion_violations_total) are reachable over HTTP.
	// Without this mount the counters published in
	// internal/cache/fallthrough_meter_expvar.go are visible only via
	// the process expvar registry — not on the production pod's HTTP
	// surface — which is why Ship D's tester gate appeared to fail
	// observability (the architect's audit confirmed the wrappers WERE
	// firing; the counter simply wasn't reachable). One-liner alongside
	// the existing pprof registrations.
	//
	// Ship 0.30.249 / Task #250 Block 2a — register
	// snowplow_rbac_publish_seq (RBAC snapshot publish-sequence counter)
	// here BEFORE the mux mount accepts scrapes. Unconditionally wired
	// (cache mode-agnostic) so the Phase 6 #250 bench S8/S9 inner gate
	// can poll the value under both CACHE_ENABLED=true and =false;
	// under cache-off the value remains 0 and the bench's
	// _wait_rbac_propagation_to_snowplow times out as expected.
	cache.RegisterRBACSnapshotExpvar()
	// Ship L2 (0.30.253) / Task #291 — register the snapshot authz memo
	// counters (hits/misses/swaps/refused/entries) next to the publish-seq
	// expvar, BEFORE the mux mount accepts scrapes, so the F1/F5 falsifiers
	// can read hit-rate + entry count over /debug/vars. Cache mode-agnostic
	// registration; the memo itself is only populated on the cache=on path.
	rbac.RegisterAuthzMemoExpvar()
	mux.Handle("GET /debug/vars", expvar.Handler())

	// Fix #1 / stale-delete diagnostic — read-only per-GVR servability
	// snapshot {HasSynced, watchBroken, confirmed, servable}
	// (docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md).
	// Mutates no state; available in both cache-on and cache-off so the
	// stale-delete latch (registered-but-unconfirmed / watch-broken GVR) is
	// diagnosable without a kubectl exec. Mounted next to /debug/vars.
	mux.HandleFunc("GET /debug/servable", handlers.DebugServable())

	// R1 diagnostic — read-only METADATA-ONLY snapshot of resolved-output
	// cache entries (class/path/gvr/age/ttl/items_count), for diagnosing a
	// degraded apistage entry (stale getComposition / cluster-scoped
	// allCompositionResources) without a kubectl exec. NEVER returns
	// resolved bodies (per-identity RBAC-sensitive) — the structural leak
	// guard is cache.ResolvedEntryMeta's type shape. Mounted next to
	// /debug/servable (docs/design-r1-allcompositionresources-invalidation-2026-06-26.md §6).
	mux.HandleFunc("GET /debug/apistage", handlers.DebugApistage())

	// #61 diagnostic — read-only AGGREGATE-ONLY refresh-broadcaster counters
	// (published/delivered/dropped/coalesced + subscriber count), the
	// on-cluster instrument for verifying live-refresh delivery
	// (refreshDeliveredTotal>0 for an armed key under churn) without a kubectl
	// exec. NO per-subscription-key/identity enumeration — totals only (a
	// per-key dump would be a cross-user signal). Mounted next to
	// /debug/servable + /debug/apistage (docs/rca-refreshes-zero-delivery-2026-06-26.md §5).
	//
	// #69 — AUTH-GATED for prod. Wrapped in middleware.RefreshAuth (the SAME
	// cookie-or-header JWT gate /refreshes uses), so a bare unauthenticated GET
	// now 401s instead of returning the counters. The body stays aggregate-only
	// (uint64 totals + int subscriber count + bool — structurally cannot carry
	// per-key/per-user data), so even past the gate it is leak-safe; the gate
	// is defence-in-depth so the diagnostic is not world-readable in prod. Uses
	// the same RefreshAuth chain form as /refreshes (no apiserver read, no
	// UserConfig Secret lookup).
	mux.Handle("GET /debug/refreshes", chain.Append(
		middleware.RefreshAuth(jwtKeys)).
		Then(handlers.DebugRefreshes()))

	ctx, stop := signal.NotifyContext(context.Background(), []os.Signal{
		os.Interrupt,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	}...)
	defer stop()

	// Track 2 (transport gzip) — wrap the mux with Accept-Encoding-gated gzip
	// compression BEFORE otelhttp so gzip is the INNERMOST wrapper (closest to
	// the mux): it compresses the handler's actual response bytes, and the
	// otelhttp span still measures the full request incl. the compressed
	// write. text/event-stream (GET /refreshes) is excluded so the SSE stream
	// keeps its per-event incremental flush (middleware.Gzip / T2-2). Non-gzip
	// clients + curl diagnostics are byte-identical (gzhttp only compresses on
	// Accept-Encoding: gzip). See middleware/compression.go.
	var muxHandler http.Handler = middleware.Gzip(mux)

	// OTel HTTP server instrumentation (default-OFF gated). otelhttp wraps
	// the mux to create a server span per request and EXTRACT inbound W3C
	// traceparent/baggage (the global propagator installed by
	// tracing.Setup). When tracing is disabled the global TracerProvider is
	// the no-op default, so otelhttp.NewHandler produces no spans and adds
	// negligible overhead — the off-path stays effectively byte-identical.
	//
	// The wrap sits INSIDE use.CORS so CORS remains OUTERMOST and answers
	// the OPTIONS preflight before otelhttp ever runs (otelhttp would
	// otherwise span the preflight; CORS must own it). WithFilter skips
	// span creation for the high-frequency health/readiness/debug probes.
	var rootHandler http.Handler = otelhttp.NewHandler(muxHandler, serviceName,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			switch r.URL.Path {
			case "/health", "/readyz":
				return false
			}
			return !strings.HasPrefix(r.URL.Path, "/debug/")
		}),
	)

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      use.CORS(snowplowCORSOptions())(rootHandler),
		ReadTimeout:  10 * time.Second, // read is fast; unchanged (#351/C2)
		WriteTimeout: writeTimeout,     // 300s — clears cache-OFF heavy path (#351/C2)
		IdleTimeout:  30 * time.Second, // keep-alive; unchanged (#351/C2)
	}

	// Ship D (0.30.141) — architectural-consistency invariant boot
	// assert. Verifies every /call-class route is wrapped with
	// FallthroughScopeMiddleware. Test mode panics on missing routes;
	// prod logs ERROR + bumps snowplow_assertion_violations_total. The
	// invariant has no kill-switch in prod — a missing route degrades
	// invariant visibility, not service correctness.
	cache.AssertReadPathsScoped()

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server cannot run",
				slog.String("addr", server.Addr),
				slog.Any("err", err))
		}
	}()

	// Listen for the interrupt signal.
	log.Info("server is ready to handle requests", slog.String("addr", server.Addr))
	<-ctx.Done()

	// Restore default behavior on the interrupt signal and notify user of shutdown.
	stop()
	log.Info("server is shutting down gracefully, press Ctrl+C again to force")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	server.SetKeepAlivesEnabled(false)
	if err := server.Shutdown(ctx); err != nil {
		log.Error("server forced to shutdown", slog.Any("err", err))
	}

	log.Info("server gracefully stopped")
}

// splitCommaList parses a comma-separated env var into a string
// slice, dropping empty entries and trimming whitespace. Used by
// the Ship Resilience-1 wiring (0.30.162) to parse
// CONTROLLER_HEALTH_NAMESPACES into the watch-scope slice. Empty
// input → empty slice (subsystem-defined behavior: the per-
// namespace Deployment + Endpoints watches stay inert; cluster-
// scoped webhook watches still run).
// resolvePrewarmRegisterDefault decides whether the startup navigation
// GVR-walk (PrewarmRegisterFromNavigation) runs, from the raw
// PREWARM_REGISTER_ENABLED env value.
//
// #130 1.7.5 — IMPLICIT-ON under CACHE_ENABLED. The walk is now enabled by
// default whenever the cache subsystem is on; only an EXPLICIT
// PREWARM_REGISTER_ENABLED=false opts out (emergency lever). This is the
// inverse of the pre-1.7.5 default-OFF gate (== "true" to enable). Rationale
// (TRACED, supersedes the obsolete "no-consumer QPS" note that justified
// default-OFF):
//
//   - The walk registers ONLY the STATIC (non-JQ-templated) inventory GVR
//     subset. JQ-templated apiserver paths — every composition.krateo.io path
//     is one — are rejected by the `${`-guard in ParseAPIServerPathToGVR
//     (inventory.go:171), so the walk CANNOT register composition GVRs. Its
//     incremental footprint is bounded to the trivial static/CRD-meta class.
//   - The composition informers (the OOM-relevant mass at 50K) are registered
//     lazily via the DiscoverGroupResources / EnsureResourceType hook on the
//     resolver inner-call path (resolve.go) REGARDLESS of this flag — flipping
//     it neither adds nor removes them.
//   - A consumer now DOES exist for the walk-registered informers: the #130 F1
//     confirm-prime + the informer-serve branch + the Phase 1 seed all read
//     them. The "walk registers informers nobody reads" regression the
//     default-OFF gate guarded against no longer applies.
//
// Contract: only the exact string "false" disables. Empty (unset), "true", and
// any other value all enable — an implicit-on lever with a single explicit
// opt-out. CACHE-off unreachability is enforced by the CALLER (this resolves
// only inside the !cache.Disabled() branch), not here.
func resolvePrewarmRegisterDefault(raw string) bool {
	return raw != "false"
}

func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
