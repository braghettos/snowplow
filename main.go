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

	"k8s.io/client-go/dynamic"
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
	if *debugOn {
		log.Debug("environment variables", slog.Any("env", os.Environ()))
	}

	chain := use.NewChain(
		use.TraceId(),
		use.Logger(log),
	)

	// Cache plumbing — present at 0.30.1, dormant by default.
	//
	// When CACHE_ENABLED is unset / false / 0 / no, cache.Disabled()
	// returns true and cache.NewResourceWatcher returns (nil, nil)
	// without instantiating the dynamicinformer factory. No goroutines
	// spawn. Every consumer (objects.Get, dynamic.ListObjects,
	// api.Resolve) checks cache.Disabled() at the top and falls back
	// to apiserver.
	//
	// When CACHE_ENABLED=true the factory is constructed but
	// factory.Start() is NOT called here — that's deferred to 0.30.2
	// which lands consumer routing.
	cacheCtx, cacheCancel := context.WithCancel(context.Background())
	defer cacheCancel()

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
				}
			}
		}
	} else {
		// Disabled() emits the canonical "cache.disabled=true" log line
		// from inside NewResourceWatcher — call it for the side-effect
		// even when we know it returns nil. This is the falsifier the
		// PM gate verifies via `kubectl logs`.
		_, _ = cache.NewResourceWatcher(cacheCtx, nil)
	}
	_ = cacheWatcher // wired to dispatchers at 0.30.2

	mux := http.NewServeMux()

	mux.Handle("GET /swagger/", httpSwagger.WrapHandler)
	//mux.Handle("POST /convert", chain.Then(handlers.Converter()))

	mux.Handle("GET /health", handlers.HealthCheck(serviceName, build, kubeutil.ServiceAccountNamespace))
	mux.Handle("GET /api-info/names", chain.Then(handlers.Plurals()))
	mux.Handle("GET /list", chain.Append(use.UserConfig(*signKey, *authnNS)).Then(handlers.List()))

	mux.Handle("GET /call", chain.Append(
		use.UserConfig(*signKey, *authnNS),
		handlers.Dispatcher(dispatchers.All())).
		Then(handlers.Call()))
	mux.Handle("POST /call", chain.Append(use.UserConfig(*signKey, *authnNS)).Then(handlers.Call()))
	mux.Handle("PUT /call", chain.Append(use.UserConfig(*signKey, *authnNS)).Then(handlers.Call()))
	mux.Handle("PATCH /call", chain.Append(use.UserConfig(*signKey, *authnNS)).Then(handlers.Call()))
	mux.Handle("DELETE /call", chain.Append(use.UserConfig(*signKey, *authnNS)).Then(handlers.Call()))

	mux.Handle("POST /jq", chain.Append(use.UserConfig(*signKey, *authnNS)).Then(handlers.JQ()))

	ctx, stop := signal.NotifyContext(context.Background(), []os.Signal{
		os.Interrupt,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	}...)
	defer stop()

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
			MaxAge:           300, // Maximum value not ignored by any of major browsers
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
