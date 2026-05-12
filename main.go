package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
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

	"k8s.io/apimachinery/pkg/runtime/schema"
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

					// Tag 0.30.6 binding: walk every RestAction in the
					// cluster, derive the GVR set referenced by
					// spec.api[*].path, and eager-register the lot.
					// Bound by STARTUP_TIMEOUT_SECONDS (default 120);
					// fan-in by STARTUP_INFORMER_FANIN (default 8).
					// Soft failure: log + continue; the lazy fallback
					// in AddResourceType handles the gap.
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
	defer func() {
		if cacheWatcher != nil {
			cacheWatcher.Stop()
			cache.SetGlobal(nil)
		}
	}()

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

	// /debug/pprof/* — registered on the custom mux (server does NOT use http.DefaultServeMux).
	// Exposes goroutine, heap, profile, allocs, mutex, block, cmdline, symbol, threadcreate, trace.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

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
