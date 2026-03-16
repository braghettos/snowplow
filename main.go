package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/kubeutil"
	"github.com/krateoplatformops/plumbing/server/use"
	"github.com/krateoplatformops/plumbing/server/use/cors"
	"github.com/krateoplatformops/plumbing/slogs/pretty"
	_ "github.com/krateoplatformops/snowplow/docs"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers"
	"github.com/krateoplatformops/snowplow/internal/handlers/dispatchers"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	httpSwagger "github.com/swaggo/http-swagger"
	"k8s.io/client-go/rest"
)

const (
	serviceName = "snowplow"
)

var (
	build string
)

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

	// Build a Redis cache (sidecar at localhost:6379).
	var redisCache *cache.RedisCache
	if cache.Disabled() {
		log.Info("caching disabled via CACHE_ENABLED=false")
	} else {
		redisCache = cache.New(*resourceTTL)
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
	}

	if redisCache != nil {
		if err := redisCache.Ping(ctx); err != nil {
			log.Warn("redis not available; caching disabled", slog.Any("err", err))
			redisCache = nil
		} else {
			log.Info("redis connected")
			startBackgroundServices(ctx, log, redisCache, *authnNS, *warmupConfigPath)
		}
	}

	// Middleware that injects the cache into every request context.
	withCache := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redisCache != nil {
				r = r.WithContext(cache.WithCache(r.Context(), redisCache))
			}
			next.ServeHTTP(w, r)
		})
	}

	chain := use.NewChain(
		use.TraceId(),
		use.Logger(log),
	)

	mux := http.NewServeMux()

	mux.Handle("GET /swagger/", httpSwagger.WrapHandler)

	mux.Handle("GET /health", handlers.HealthCheck(serviceName, build, kubeutil.ServiceAccountNamespace))
	mux.Handle("GET /metrics/cache", chain.Then(handlers.CacheMetrics()))
	mux.Handle("GET /api-info/names", chain.Then(handlers.Plurals()))
	userCfg := handlers.CachedUserConfig(*signKey, *authnNS, sarc, redisCache)

	mux.Handle("GET /list", chain.Append(userCfg, withCache).Then(handlers.List()))

	mux.Handle("GET /call", chain.Append(
		userCfg,
		withCache,
		handlers.Dispatcher(dispatchers.All())).
		Then(handlers.Call()))
	mux.Handle("POST /call", chain.Append(userCfg, withCache).Then(handlers.Call()))
	mux.Handle("PUT /call", chain.Append(userCfg, withCache).Then(handlers.Call()))
	mux.Handle("PATCH /call", chain.Append(userCfg, withCache).Then(handlers.Call()))
	mux.Handle("DELETE /call", chain.Append(userCfg, withCache).Then(handlers.Call()))

	mux.Handle("POST /jq", chain.Append(userCfg).Then(handlers.JQ()))

	server := &http.Server{
		Addr: fmt.Sprintf(":%d", *port),
		Handler: use.CORS(cors.Options{
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
		})(mux),
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

// startBackgroundServices sets up the four-phase cache startup sequence.
// ctx is the signal context used for long-running watchers (cancelled on SIGTERM).
// Warmup and informer sync use a separate background context so that a SIGTERM
// received during startup (e.g. from a failing liveness probe) does not abort
// the warmup — the server will start with a fully warm cache regardless.
func startBackgroundServices(ctx context.Context, log *slog.Logger, c *cache.RedisCache, authnNS, warmupConfigPath string) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		log.Warn("not running in-cluster; background cache services disabled", slog.Any("err", err))
		return
	}

	// Phase 1: Start long-running watchers bound to the process lifetime (signal ctx).
	resourceWatcher, err := cache.NewResourceWatcher(c, rc)
	if err != nil {
		log.Error("failed to create resource watcher", slog.Any("err", err))
		return
	}

	c.SetGVRNotifier(resourceWatcher.AddGVR)

	resourceWatcher.StartExpiryRefresh(ctx)
	resourceWatcher.Start(ctx)

	rbacWatcher := cache.NewRBACWatcher(c, rc)
	if err := rbacWatcher.Start(ctx); err != nil {
		log.Warn("failed to start RBAC watcher", slog.Any("err", err))
	}

	if authnNS != "" {
		userWatcher := cache.NewUserSecretWatcher(c, rc, authnNS)
		if err := userWatcher.Start(ctx); err != nil {
			log.Warn("failed to start user secret watcher", slog.Any("err", err))
		}
	}

	// Phase 2: Load warmup config and pre-register GVRs (triggers informer startup).
	// Uses a background context so a signal during startup does not abort registration.
	warmupCtx, warmupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer warmupCancel()

	warmer := cache.NewWarmer(c, rc)
	warmupCfg, err := cache.LoadWarmupConfig(warmupConfigPath)
	if err != nil {
		log.Warn("warmup config not loaded; using empty config",
			slog.String("path", warmupConfigPath), slog.Any("err", err))
	} else {
		warmer.SetWarmupConfig(warmupCfg)
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

	// Phase 4: Run L3 warmup using the background context.
	log.Info("starting cache warmup")
	warmer.Run(warmupCtx)

	// Phase 5: Pre-warm L1 (resolved widget output) for every known user.
	// This runs after L3 is populated so widget resolution hits L3 instead of
	// the live K8s API.
	if warmupCfg != nil && authnNS != "" {
		widgetGVRs := dispatchers.FilterWidgetGVRs(warmupCfg)
		go dispatchers.WarmL1ForAllUsers(warmupCtx, c, rc, authnNS, widgetGVRs)
	}
}
