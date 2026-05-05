package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/kubeutil"
	"github.com/krateoplatformops/plumbing/server/use"
	"github.com/krateoplatformops/plumbing/server/use/cors"
	"github.com/krateoplatformops/plumbing/slogs/pretty"
	_ "github.com/krateoplatformops/snowplow/docs"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers"
	"github.com/krateoplatformops/snowplow/internal/observability"
	"github.com/krateoplatformops/snowplow/internal/handlers/dispatchers"
	apiresolve "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	httpSwagger "github.com/swaggo/http-swagger"
	"k8s.io/client-go/rest"
)

const (
	serviceName = "snowplow"
)

var (
	build string

	// globalInformerReader is set by startBackgroundServices after the
	// ResourceWatcher is created. The HTTP middleware injects it into
	// every request context so resolve.go can read from the informer
	// store instead of Redis.
	globalInformerReader cache.InformerReader

	// globalResourceWatcher is set by startBackgroundServices after the
	// ResourceWatcher is created. Read-only observability hook used by
	// /metrics/runtime to surface HOT/WARM/COLD workqueue depths.
	globalResourceWatcher *cache.ResourceWatcher
)

// workQueueLensAdapter implements handlers.WorkQueueLens by reading the
// package-level globalResourceWatcher at call time. Returns zeros until
// the watcher is wired (handler is registered before startBackgroundServices runs).
type workQueueLensAdapter struct{}

func (workQueueLensAdapter) HotQueueLen() int {
	if globalResourceWatcher == nil {
		return 0
	}
	return globalResourceWatcher.HotQueueLen()
}
func (workQueueLensAdapter) WarmQueueLen() int {
	if globalResourceWatcher == nil {
		return 0
	}
	return globalResourceWatcher.WarmQueueLen()
}
func (workQueueLensAdapter) ColdQueueLen() int {
	if globalResourceWatcher == nil {
		return 0
	}
	return globalResourceWatcher.ColdQueueLen()
}

func init() {
	// Disable HTTP/2 client globally. K8s clients created by the plumbing
	// library (kubeconfig.NewClientConfig) default to HTTP/2. Under load
	// the K8s API server resets HTTP/2 connections, crashing the Go h2
	// frame reader (exit code 2). The h2c server is unaffected — it uses
	// explicit h2c.NewHandler, not the default transport.
	if v := os.Getenv("GODEBUG"); v != "" {
		os.Setenv("GODEBUG", v+",http2client=0")
	} else {
		os.Setenv("GODEBUG", "http2client=0")
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
	signKey := flag.String("jwt-sign-key", env.String("JWT_SIGN_KEY", ""), "secret key used to sign JWT tokens")
	jqModPath := flag.String("jq-modules-path", env.String(jqsupport.EnvModulesPath, ""),
		"loads JQ custom modules from the filesystem")
	warmupConfigPath := flag.String("warmup-config", env.String("WARMUP_CONFIG", "/etc/snowplow/cache-warmup.yaml"),
		"path to cache warmup YAML config")
	resourceTTL := flag.Duration("resource-ttl", env.Duration("RESOURCE_TTL", cache.DefaultResourceTTL),
		"default TTL for cached Kubernetes resources")
	// l1DiskPath removed — MemCache stores L1 in-process.

	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Flags:")
		flag.PrintDefaults()
	}

	flag.Parse()

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
		lh = pretty.New(&slog.HandlerOptions{
			Level:     logLevel,
			AddSource: false,
		},
			pretty.WithDestinationWriter(os.Stderr),
			pretty.WithColor(),
			pretty.WithOutputEmptyAttrs(),
		)
	} else {
		lh = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:     logLevel,
			AddSource: false,
		})
	}

	log := slog.New(lh)
	slog.SetDefault(log)
	if *debugOn {
		log.Debug("environment variables", slog.Any("env", os.Environ()))
	}

	// Build an in-process cache (replaces Redis sidecar).
	var appCache cache.Cache
	if cache.Disabled() {
		log.Info("caching disabled via CACHE_ENABLED=false")
	} else {
		mc := cache.NewMem(*resourceTTL)
		mc.StartEviction(context.Background())
		appCache = mc
		log.Info("in-process cache enabled")

		// Q-RBAC-DECOUPLE C(d) v3 — flush any stale snowplow:resolved:*
		// entries written under an incompatible schema (e.g. v2-shape
		// CachedRESTAction wrappers from a previous image roll). On the
		// production in-process MemCache this is a no-op because each
		// pod restart starts with an empty cache, but the call is made
		// unconditionally so a future Redis backend would automatically
		// evict pre-v3 entries without manual intervention. Per spec
		// §7.2 step 2: "v3 reader rejects v2-shape entries with an
		// unmarshal error, falls through to miss path, the resolver
		// fills with v3-shape" — the flush is belt-and-suspenders.
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 30*time.Second)
		flushed, ferr := cache.FlushResolvedPrefix(flushCtx, appCache)
		flushCancel()
		if ferr != nil {
			log.Warn("v3 startup: cache flush failed (continuing — v3 reader still rejects v2-shape entries)",
				slog.Any("err", ferr))
		} else if flushed > 0 {
			log.Info("v3 startup: flushed stale L1 outer entries",
				slog.Int("flushed", flushed))
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), []os.Signal{
		os.Interrupt,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	}...)
	defer stop()

	// In-cluster config used by the CachedUserConfig middleware and background
	// services. Created once to avoid re-reading service account files per request.
	sarc, sarcErr := rest.InClusterConfig()
	if sarcErr != nil {
		log.Warn("not running in-cluster; some features disabled", slog.Any("err", sarcErr))
	} else {
		sarc.QPS = 100
		sarc.Burst = 200
		sarc.NextProtos = []string{"http/1.1"} // disable HTTP2 to avoid h2 frame crashes
	}

	// Snowplow-SA endpoint provider, used by api[] entries that declare
	// userAccessFilter (Q-RBAC-DECOUPLE C(d) v2). The closure captures the
	// in-cluster *rest.Config once at startup; api.SnowplowEndpointFromConfig
	// re-reads BearerTokenFile on every call so projected SA token rotation
	// is picked up automatically (~10µs tmpfs read). Returns an error when
	// not in-cluster — userAccessFilter calls will be rejected at runtime
	// with an explicit log line.
	var snowplowEndpointFn func() (*endpoints.Endpoint, error)
	if sarc != nil {
		rc := sarc
		snowplowEndpointFn = func() (*endpoints.Endpoint, error) {
			return apiresolve.SnowplowEndpointFromConfig(rc)
		}
	} else {
		log.Warn("no in-cluster config; userAccessFilter calls will be rejected at runtime")
	}

	// Q-RBAC-DECOUPLE C(d) v6 — Path B (audit 2026-05-04). Build the
	// in-cluster dynamic K8s client once at startup. The SA dispatch path
	// in api.Resolve uses this client (via cache.SnowplowK8sFromContext)
	// instead of httpcall.Do, structurally bypassing plumbing's
	// tlsConfigFor bug that silently dropped CertificateAuthorityData on
	// token-auth endpoints (the v5 D1 defect that left every SA dispatch
	// failing x509: certificate signed by unknown authority).
	//
	// Construction-failure is non-fatal here: the v5 SnowplowEndpoint
	// path stays available as a defense-in-depth fallback (it's broken
	// for SA dispatch, but UAF api[] entries with EndpointRef set still
	// flow through it). Production wiring expects sarc != nil and
	// dynamic.NewClient to succeed; failure here is a strong indicator
	// of a misconfigured pod that warrants explicit operator attention.
	var snowplowK8sClient dynamic.Client
	if sarc != nil {
		var k8sErr error
		snowplowK8sClient, k8sErr = dynamic.NewClient(sarc)
		if k8sErr != nil {
			log.Warn("failed to build snowplow K8s dynamic client; SA dispatch will fall back to httpcall (v5 plumbing TLS bug active)",
				slog.Any("err", k8sErr))
			snowplowK8sClient = nil
		} else {
			log.Info("snowplow K8s dynamic client built (Q-RBAC-DECOUPLE C(d) v6 — Path B SA dispatch active)")
		}
	}

	// No Redis ping or disk store needed — MemCache is always available.

	// Initialize OpenTelemetry SDK (no-op when OTEL_ENABLED != "true").
	// Use an independent context so exporter failures (e.g., collector
	// refusing data due to memory pressure) do not cascade into a
	// server shutdown. The OTel context is cancelled on server stop.
	otelCtx, otelCancel := context.WithCancel(context.Background())
	otelShutdown, otelErr := observability.Init(otelCtx, build)
	if otelErr != nil {
		log.Warn("otel init failed", slog.Any("err", otelErr))
	}
	defer func() {
		otelCancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		otelShutdown(shutdownCtx)
	}()

	// When OTel is enabled, wrap the slog handler to:
	// 1. Inject trace_id/span_id into stderr log output
	// 2. Export logs via OTLP to ClickHouse (for HyperDX correlation)
	if strings.EqualFold(os.Getenv("OTEL_ENABLED"), "true") {
		traceHandler := observability.NewTraceIDHandler(lh)
		otelHandler := observability.NewOTelSlogHandler(traceHandler)
		log = slog.New(otelHandler)
		slog.SetDefault(log)
	}

	// Middleware that injects the cache and informer reader into every
	// request context. globalInformerReader is set by startBackgroundServices
	// after the ResourceWatcher is created.
	withCache := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if appCache != nil {
				ctx = cache.WithCache(ctx, appCache)
			}
			if globalInformerReader != nil {
				ctx = cache.WithInformerReader(ctx, globalInformerReader)
			}
			// Snowplow-SA endpoint provider for elevated userAccessFilter
			// dispatch. Wrapped to the type-erased shape that
			// cache.SnowplowEndpointFromContext returns; api.Resolve
			// re-asserts back to *endpoints.Endpoint.
			if snowplowEndpointFn != nil {
				ctx = cache.WithSnowplowEndpoint(ctx, func() (any, error) {
					return snowplowEndpointFn()
				})
			}
			// Q-RBAC-DECOUPLE C(d) v6 — Path B SA dispatch client.
			if snowplowK8sClient != nil {
				ctx = cache.WithSnowplowK8s(ctx, snowplowK8sClient)
			}
			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)
		})
	}

	chain := use.NewChain(
		use.TraceId(),
		use.Logger(log),
	)

	mux := http.NewServeMux()

	// pprof endpoints for heap/goroutine profiling
	mux.HandleFunc("GET /debug/pprof/", http.DefaultServeMux.ServeHTTP)
	mux.HandleFunc("GET /debug/pprof/heap", http.DefaultServeMux.ServeHTTP)
	mux.HandleFunc("GET /debug/pprof/goroutine", http.DefaultServeMux.ServeHTTP)
	mux.HandleFunc("GET /debug/pprof/profile", http.DefaultServeMux.ServeHTTP)

	// Enable mutex + block profiling for HOT-tier contention analysis.
	// Sampling rates match Go community defaults for production-safe overhead.
	runtime.SetMutexProfileFraction(5) // sample 1-in-5 mutex contention events
	runtime.SetBlockProfileRate(10000) // sample blocking events lasting >10µs

	mux.Handle("GET /swagger/", httpSwagger.WrapHandler)

	mux.Handle("GET /health", handlers.HealthCheck(serviceName, build, kubeutil.ServiceAccountNamespace))
	mux.Handle("GET /ready", handlers.ReadinessCheck())
	mux.Handle("GET /metrics/cache", chain.Then(handlers.CacheMetrics()))
	mux.Handle("GET /metrics/runtime", handlers.RuntimeMetricsHandler(appCache, workQueueLensAdapter{}))
	mux.Handle("GET /api-info/names", chain.Then(handlers.Plurals()))
	// Create RBACWatcher early so the middleware can compute binding identities.
	// Start() is called later in startBackgroundServices.
	var globalRBACWatcher *cache.RBACWatcher
	if appCache != nil {
		globalRBACWatcher = cache.NewRBACWatcher(appCache, sarc)
	}
	userCfg := handlers.CachedUserConfig(*signKey, *authnNS, sarc, appCache, globalRBACWatcher)

	mux.Handle("GET /list", chain.Append(userCfg, withCache).Then(handlers.List()))

	mux.Handle("GET /call", chain.Append(
		userCfg,
		withCache,
		handlers.Dispatcher(dispatchers.All(snowplowEndpointFn, snowplowK8sClient))).
		Then(handlers.Call()))
	mux.Handle("POST /call", chain.Append(userCfg, withCache).Then(handlers.Call()))
	mux.Handle("PUT /call", chain.Append(userCfg, withCache).Then(handlers.Call()))
	mux.Handle("PATCH /call", chain.Append(userCfg, withCache).Then(handlers.Call()))
	mux.Handle("DELETE /call", chain.Append(userCfg, withCache).Then(handlers.Call()))

	mux.Handle("POST /jq", chain.Append(userCfg).Then(handlers.JQ()))

	httpHandler := otelhttp.NewHandler(recoveryMiddleware(handlers.Gzip(use.CORS(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{
			"Accept",
			"Authorization",
			"Content-Type",
			"X-Auth-Code",
			"X-Krateo-TraceId",
		},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	})(mux))), "snowplow")

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      h2c.NewHandler(httpHandler, &http2.Server{}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 50 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server cannot run",
				slog.String("addr", server.Addr),
				slog.Any("err", err))
		}
	}()

	log.Info("server is ready to handle requests", slog.String("addr", server.Addr))

	// Run cache warmup in background so /health responds immediately.
	// Previously this ran before ListenAndServe, causing startup probe failures
	// when informer sync + L2/RBAC warmup exceeded the probe timeout.
	if appCache != nil {
		go startBackgroundServices(ctx, log, appCache, *authnNS, *warmupConfigPath, *signKey, globalRBACWatcher, snowplowEndpointFn, snowplowK8sClient)
	}
	<-ctx.Done()

	stop()
	log.Info("server is shutting down gracefully, press Ctrl+C again to force")

	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	server.SetKeepAlivesEnabled(false)
	if err := server.Shutdown(shutCtx); err != nil {
		log.Error("server forced to shutdown", slog.Any("err", err))
	}

	log.Info("server gracefully stopped")
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				slog.Error("panic recovered",
					slog.Any("error", err),
					slog.String("stack", string(buf[:n])),
					slog.String("path", r.URL.Path))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// startBackgroundServices sets up the four-phase cache startup sequence.
// ctx is the signal context used for long-running watchers (cancelled on SIGTERM).
// Warmup and informer sync use a separate background context so that a SIGTERM
// received during startup (e.g. from a failing liveness probe) does not abort
// the warmup — the server will start with a fully warm cache regardless.
func startBackgroundServices(ctx context.Context, log *slog.Logger, c cache.Cache, authnNS, warmupConfigPath, signKey string, rbacWatcher *cache.RBACWatcher, snowplowEndpointFn func() (*endpoints.Endpoint, error), snowplowK8sClient dynamic.Client) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		log.Warn("not running in-cluster; background cache services disabled", slog.Any("err", err))
		return
	}
	rc.QPS = 100
	rc.Burst = 200
	// HTTP/2 re-enabled for informers (client-go v0.35 + Go 1.25).
	// Multiplexes all WATCH streams over 1-2 connections, saving ~240MB
	// in bufio/TLS overhead vs HTTP/1.1 (confirmed via pprof).

	// Phase 1: Start long-running watchers bound to the process lifetime (signal ctx).
	resourceWatcher, err := cache.NewResourceWatcher(c, rc)
	if err != nil {
		log.Error("failed to create resource watcher", slog.Any("err", err))
		return
	}

	if cc, ok := c.(*cache.MemCache); ok {
		cc.SetGVRNotifier(resourceWatcher.AddGVR)
	}
	globalInformerReader = resourceWatcher // InformerReader interface — reads from informer store
	globalResourceWatcher = resourceWatcher // exposes HOT/WARM/COLD queue depths to /metrics/runtime
	resourceWatcher.SetL1Refresher(dispatchers.MakeL1Refresher(c, rc, authnNS, signKey, rbacWatcher, snowplowEndpointFn, snowplowK8sClient))

	// Load warmup config early to get autoDiscoverGroups for CRD informer filtering.
	warmupCfg, err := cache.LoadWarmupConfig(warmupConfigPath)
	if err != nil {
		log.Warn("failed to load warmup config", slog.Any("err", err))
	}
	if warmupCfg != nil {
		resourceWatcher.SetAutoDiscoverGroups(warmupCfg.Warmup.AutoDiscoverGroups)
	}

	resourceWatcher.Start(ctx)

	if rbacWatcher != nil {
		if err := rbacWatcher.Start(ctx); err != nil {
			log.Warn("failed to start RBAC watcher", slog.Any("err", err))
		}
	}

	if authnNS != "" {
		userWatcher := cache.NewUserSecretWatcher(c, rc, authnNS)
		userWatcher.SetOnUserReady(dispatchers.MakeRBACPreWarmer(c, rc, authnNS, signKey))
		if err := userWatcher.Start(ctx); err != nil {
			log.Warn("failed to start user secret watcher", slog.Any("err", err))
		}
	}

	// Phase 2: Load warmup config and pre-register GVRs (triggers informer startup).
	// Uses a background context so a signal during startup does not abort registration.
	warmupCtx, warmupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer warmupCancel()

	warmer := cache.NewWarmer(c, rc)
	if warmupCfg == nil {
		// Retry loading if it failed earlier (shouldn't happen but be safe).
		warmupCfg, err = cache.LoadWarmupConfig(warmupConfigPath)
		if err != nil {
			log.Warn("warmup config not loaded; using empty config",
				slog.String("path", warmupConfigPath), slog.Any("err", err))
		}
	}
	if warmupCfg != nil {
		warmer.SetWarmupConfig(warmupCfg)
		warmer.DiscoverCompositionGVRs(warmupCtx)
		warmer.PreRegisterGVRs(warmupCtx)
	}

	// Phase 3: Wait for informers to complete their initial list-and-watch sync.
	log.Info("waiting for informer caches to sync...")
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer syncCancel()
	if resourceWatcher.WaitForSync(syncCtx) {
		log.Info("informer caches synced")
	} else {
		log.Warn("informer caches did not sync within timeout; proceeding with warmup anyway")
	}

	// Phase 4: Run L2 warmup using the background context.
	log.Info("starting cache warmup")
	warmer.Run(warmupCtx)

	// Phase 4a: Reconcile L2 from informer stores -- fixes stale data from pod restart.
	// Informers have an active WATCH, so their in-memory store is authoritative.
	// Any ghost objects (missed DELETEs during downtime) are removed, and missing
	// or stale objects are patched.
	reconcileStats := resourceWatcher.Reconcile(warmupCtx)
	log.Info("L2 reconciliation completed",
		slog.Int("gvrs", reconcileStats.GVRs),
		slog.Int("added", reconcileStats.Added),
		slog.Int("removed", reconcileStats.Removed),
		slog.Int("updated", reconcileStats.Updated),
		slog.Int("errors", reconcileStats.Errors),
		slog.Duration("duration", reconcileStats.Duration))

	// Phase 4b: RBAC decisions are now populated organically during
	// resolution rather than pre-warmed via the GVR x NS x verb cartesian
	// product. Inject informer reader into warmup contexts so resolve.go
	if globalInformerReader != nil {
		warmupCtx = cache.WithInformerReader(warmupCtx, globalInformerReader)
	}
	// Inject snowplow-SA endpoint provider so warmup-driven resolutions
	// of api[] entries with userAccessFilter use the in-cluster SA.
	if snowplowEndpointFn != nil {
		warmupCtx = cache.WithSnowplowEndpoint(warmupCtx, func() (any, error) {
			return snowplowEndpointFn()
		})
	}
	if snowplowK8sClient != nil {
		warmupCtx = cache.WithSnowplowK8s(warmupCtx, snowplowK8sClient)
	}

	// Phase 5: Pre-warm L1 (resolved widget + RESTAction output) for every
	// known user. Uses its own context because warmupCtx is cancelled when
	// this function returns, but the L1 warmup continues in the background.
	//
	// When a frontend config is available, WarmL1FromEntryPoints walks the
	// widget tree starting from the INIT/ROUTES_LOADER entry points. This
	// warms exactly the paths the frontend will request. Falls back to the
	// GVR-based WarmL1ForAllUsers when the config is missing.
	frontendConfigPath := env.String("FRONTEND_CONFIG", "/etc/frontend-config/config.json")
	go func() {
		l1Ctx, l1Cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer l1Cancel()
		if globalInformerReader != nil {
			l1Ctx = cache.WithInformerReader(l1Ctx, globalInformerReader)
		}
		if snowplowEndpointFn != nil {
			l1Ctx = cache.WithSnowplowEndpoint(l1Ctx, func() (any, error) {
				return snowplowEndpointFn()
			})
		}
		if snowplowK8sClient != nil {
			l1Ctx = cache.WithSnowplowK8s(l1Ctx, snowplowK8sClient)
		}

		feCfg, feErr := cache.LoadFrontendConfig(frontendConfigPath)
		if feErr != nil {
			log.Warn("L1 warmup: frontend config not available — skipping L1 prewarm",
				slog.String("path", frontendConfigPath), slog.Any("err", feErr))
			return
		}
		eps := feCfg.EntryPoints()
		if len(eps) == 0 {
			log.Warn("L1 warmup: no entry points in frontend config — skipping L1 prewarm")
			return
		}
		log.Info("L1 warmup: using frontend entry points",
			slog.Int("entryPoints", len(eps)),
			slog.String("configPath", frontendConfigPath))
		dispatchers.WarmL1FromEntryPoints(l1Ctx, c, rc, authnNS, signKey, eps, rbacWatcher, snowplowEndpointFn, snowplowK8sClient)
	}()
}
