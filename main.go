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
	restactionsapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	httpSwagger "github.com/swaggo/http-swagger"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
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

	// Ship 0.30.123 (#155) — wire the in-process nested-/call resolver
	// seam. When a RESTAction stage's path is a /call?resource=... loopback
	// into snowplow's own /call endpoint, the api resolver invokes this
	// IN-PROCESS instead of issuing an HTTP request — so a JWT-less /
	// SA-credentialed resolve can complete an exportJwt loopback stage.
	// Wired unconditionally at startup (not cache-gated): the seam itself
	// is cache-agnostic; ResolveNestedCall internally gates its RBAC check
	// on !cache.Disabled(). Mirrors the api.resolveOnceFn seam pattern.
	// RESOLVER_INPROCESS_NESTED_CALL (default true) is the runtime gate;
	// a nil resolver (this wiring skipped) is the structural fallback.
	restactionsapi.RegisterNestedCallResolver(dispatchers.ResolveNestedCall)

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
					cache.StartRefresher(cacheCtx)

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

						// 0.30.99 Tag B — startup navigation GVR-walk,
						// gated behind PREWARM_REGISTER_ENABLED (default
						// OFF, mirrors EAGER_REGISTER_ENABLED). Not in the
						// chart configmap — absent ⇒ OFF, so Tag B is
						// chart-change-free and behavior-neutral for
						// production.
						//
						// Why default OFF (architect REJECT of default-on,
						// adjudicated): with RESOLVER_USE_INFORMER OFF by
						// default the resolver pivot does NOT consume the
						// informers a startup walk would register. A walk
						// would register N informers nobody reads — each
						// EnsureResourceType lands in the post-Start branch
						// and immediately spawns a LIST+WATCH against
						// apiserver. That is the exact "pure apiserver
						// pressure, no consumer" regression the 0.30.6 /
						// 0.30.61 post-mortems reverted (feature journal
						// 0.30.61: "no consumer reads from the eagerly-
						// registered informers ... eager-register = pure
						// apiserver overhead"). The 0.30.8 (rev 104) and
						// 0.30.92 OOM-at-50K modes are also unmitigated:
						// composition GVRs route to the FULL-Unstructured
						// informer because metadataOnlyGVRSeed is empty and
						// customer core-provider CRDs are not annotated
						// krateo.io/cache-mode: metadata.
						//
						// Promotion to ON-by-default requires a
						// PREWARM_REGISTER_ENABLED=true bench at 50K
						// measuring apiserver QPS + RSS-under-load,
						// alongside RESOLVER_USE_INFORMER=true so the pivot
						// consumer is actually present.
						if os.Getenv("PREWARM_REGISTER_ENABLED") == "true" {
							log.Info("prewarm-register: enabled via PREWARM_REGISTER_ENABLED=true",
								slog.String("subsystem", "cache"),
								slog.String("hint", "startup navigation GVR-walk active; opt-in only — costs apiserver QPS while RESOLVER_USE_INFORMER is off"),
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
							log.Info("prewarm-register: disabled (default); set PREWARM_REGISTER_ENABLED=true to opt-in",
								slog.String("subsystem", "cache"),
								slog.String("rationale", "startup walk registers informers the pivot does not consume while RESOLVER_USE_INFORMER is off — re-arms the 0.30.61 no-consumer apiserver-QPS regression + the unmitigated 0.30.8/0.30.92 OOM modes"),
							)
						}

						// 0.30.102 Tag B — Phase 1 SA-credentialed
						// resolution walk + CRD-watch + probe-gated
						// readiness. Gated behind PREWARM_ENABLED
						// (default OFF). Distinct from the 0.30.99
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
						// When PREWARM_ENABLED is OFF the goroutine is not
						// spawned and cache.MarkPhase1Done() is called
						// immediately — /readyz then returns 200 from the
						// first probe (no-op gate; behavior-neutral
						// default). PREWARM_ENABLED is NOT in the chart
						// configmap — absent => OFF.
						if cache.PrewarmEnabled() {
							log.Info("prewarm: Phase 1 startup warmup enabled via PREWARM_ENABLED=true",
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
							log.Info("prewarm: Phase 1 startup warmup disabled (default); set PREWARM_ENABLED=true to opt-in",
								slog.String("subsystem", "cache"),
								slog.String("rationale", "Tag B is behavior-neutral by default — Phase 1 does not run and /readyz returns 200 immediately"),
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
	// flips the Phase1Done gate (immediately when PREWARM_ENABLED is OFF,
	// or asynchronously at the tail of Phase1Warmup when ON). But several
	// startup paths bypass that block entirely: CACHE_ENABLED=false
	// (diagnostic passthrough), or a cache-setup failure (nil watcher).
	// On any such path there is nothing to warm, so /readyz must still
	// return 200. When PREWARM_ENABLED is ON AND a watcher exists, the
	// Phase1Warmup goroutine owns the flip — do NOT pre-flip here, or the
	// premature-Ready invariant breaks. cacheWatcher==nil with
	// PREWARM_ENABLED ON also has nothing to warm (no informer factory),
	// so flip it.
	if !cache.PrewarmEnabled() || cacheWatcher == nil {
		cache.MarkPhase1Done()
	}

	mux := http.NewServeMux()

	mux.Handle("GET /swagger/", httpSwagger.WrapHandler)
	//mux.Handle("POST /convert", chain.Then(handlers.Converter()))

	mux.Handle("GET /health", handlers.HealthCheck(serviceName, build, kubeutil.ServiceAccountNamespace))
	mux.Handle("GET /readyz", handlers.ReadyCheck())
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
