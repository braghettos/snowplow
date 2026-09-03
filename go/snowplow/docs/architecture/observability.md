---
type: Architecture
title: snowplow — observability
description: The runtime observability surface — expvars at /debug/vars, structured slog events, pprof, the default-off OTel export (traces/metrics/logs + audit), and the probe endpoints.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [observability, expvar, otel, metrics, logging, pprof]
timestamp: 2026-08-06T00:00:00Z
---

# Observability

Snowplow's runtime observability surface is four things, all on the single HTTP
port the server listens on (`main.go` `server.Addr = :<port>`):

1. **expvars** at `GET /debug/vars` — the metric surface.
2. **structured `slog` events** on stdout — the event log.
3. **pprof** at `GET /debug/pprof/*` — runtime profiling.
4. **OTel export** — traces, metrics and logs to an OTLP collector,
   **default-off** behind the `OTEL_ENABLED` master switch (see below).

Plus the diagnostic endpoints `GET /debug/servable`, `GET /debug/apistage` and
`GET /debug/refreshes` (the live-refresh subscription registry), and two probe
endpoints used by the chart.

> **Every `/debug/*` route needs a JWT (since 1.12.3).** The whole surface —
> pprof, vars, servable, apistage, refreshes — is registered by
> `registerDebugRoutes` (`debug_routes.go`) behind `middleware.RefreshAuth`,
> the stateless header-or-cookie RS256 gate `/refreshes` uses. Before 1.12.3
> only `/debug/refreshes` was gated (#69) and the rest were world-readable on
> the chart's LoadBalancer Service. Pass the token explicitly:
>
> ```bash
> curl -H "Authorization: Bearer $TOKEN" http://pod:8081/debug/vars
> ```
>
> No token, an expired token, or a token supplied only in the query string →
> `401`. A JWKS the pod cannot fetch → `503`. `/health` and `/readyz` stay
> anonymous — the kubelet presents no credentials. Operator recipes are in
> [operating.md](../../howto/operating.md).

The probe endpoints:

| Path | Handler | Returns | Meaning |
|---|---|---|---|
| `GET /health` | `internal/handlers/health.go` | always `200 {"status":"alive"}` — a static pre-encoded body, zero allocation, no apiserver read | liveness only — process is up; a still-warming pod is alive and must NOT be restarted |
| `GET /readyz` | `internal/handlers/readyz.go` | `200 {"status":"ready"}` once `cache.IsPhase1Done()`, else `503 {"status":"warming"}` | readiness — safe to receive traffic. Since the prewarm-gated-readiness reversal this flips only after the synchronous boot seed (or its fire-regardless backstop) — a pod can be **Ready-degraded** (flipped via backstop with cold cells); `snowplow_readyz_backstop_fired` distinguishes that from a healthy boot |

The chart wires **livenessProbe → `/health`**, **startupProbe → `/health`**
(long `failureThreshold` budget), and **readinessProbe → `/readyz`**
(`helm/snowplow/values.yaml`).

---

## OTel export (default-off)

`main.go` wires three gated pipelines at boot, all no-ops unless enabled:

| Env | Meaning |
|---|---|
| `OTEL_ENABLED` | master switch (default `false`) |
| `OTEL_TRACING_ENABLED` | gate tracing (defaults to the value of `OTEL_ENABLED`) |
| `OTEL_METRICS_ENABLED` | gate metrics (defaults to the value of `OTEL_ENABLED`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | collector OTLP/HTTP endpoint (standard contract) |

- **Traces** (`internal/tracing/tracing.go` `Setup`): registers a global
  TracerProvider + the W3C trace-context/baggage propagators; the whole mux is
  wrapped in `otelhttp.NewHandler` (no-op spans when disabled). The CORS
  options in `main.go` allow the browser to send
  `traceparent`/`tracestate`/`baggage`.
- **Metrics** (`internal/metrics/metrics.go` `Setup`): an **OTLP mirror of the
  expvar surface** — observable instruments read the same live counter
  snapshots the expvar closures read (families: fallthrough, dispatch L1,
  prewarm/phase-1, refresher, discovery, upstream health, …). The expvar
  surface at `/debug/vars` is unchanged and remains the scrape-free ground
  truth; the OTLP export is additive.
- **Logs + audit** (`internal/logging/logging.go` + `internal/support/audit`):
  an OTLP LogRecord bridge on the shared `otel_logs` plane, plus the audit
  middleware. The audit correlation id (D19a) rides **W3C `baggage`
  (`session.id`)** — minted/accepted by `audit.Middleware()` and serialized
  downstream by the Baggage propagator. It **replaces** the old
  `X-Krateo-Correlation-Id` header entirely; the shortid `X-Krateo-TraceId`
  and the OTel `traceparent` coexist.

---

## Cache-off contract (read before interpreting any expvar)

Under `CACHE_ENABLED=false` the cache subsystem does not exist
(transparent-fallback). Almost every `snowplow_*` expvar is registered in an
`init()` guarded by `if cache.Disabled() { return }`, so under cache-off **the
key is absent from `/debug/vars` entirely** — not present-but-zero. An absent
key under cache-off is expected, not a defect.

The exceptions, **registered unconditionally** in `main.go`'s HTTP bootstrap so
a bench probe gets `0` rather than a missing-key error under cache-off:

- `snowplow_rbac_publish_seq` — `cache.RegisterRBACSnapshotExpvar()`
- `snowplow_authz_memo_*` — `rbac.RegisterAuthzMemoExpvar()`

---

## expvars at `/debug/vars`

Every value is an `expvar.Func` (or counter) evaluated lazily at scrape time
(zero per-`/call` cost). Names are stable for grep/Prometheus tooling. Grouped
by subsystem.

### Fallthrough meter — "is the cache actually serving, or punting to the apiserver?"
Defined in `internal/cache/fallthrough_meter_expvar.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_apiserver_fallthrough_total` | grand-total `uint64` of read requests that bypassed the cache and hit the apiserver directly | climbs during boot/cold; should plateau once warm. A steadily climbing total on a warm pod = cache not covering the live request mix |
| `snowplow_apiserver_fallthrough_cells` | per-cell `map["path\|gvr\|reason"]→uint64` breakdown of the above | use to attribute fallthrough to a specific path/GVR/reason |
| `snowplow_assertion_violations_total` | per-check `map[string]→uint64` of architectural-invariant breaches. Keys: `read_paths_scoped` (a `/call`-class route not wrapped with `FallthroughScopeMiddleware`, asserted at boot by `cache.AssertReadPathsScoped()`), `serve_requires_servable` (an authoritative cache HIT was about to be served from a not-servable informer — asserted per-serve in `internal/cache/serve_assert.go`) | **0** for every key. Non-zero = an invariant is broken in prod (logged ERROR, pod stays up). **`serve_requires_servable` > 0 is P1** — never-serve-from-not-synced is the most load-bearing cache guarantee |

### Dispatch L1 lookups — resolved-output cache hit rate
Defined in `internal/handlers/dispatchers/l1_lookup_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_dispatch_l1_lookups` | `map["<handlerKind>\|<gvr>"→{"hit_total","miss_total"}]`; handlerKind ∈ {restactions, widgets, widgetContent} | high hit ratio on a warm pod. Sustained low hit ratio = prewarm not covering the served mix |
| `snowplow_widget_content_skipped_rbac_sensitive_total` / `snowplow_widget_content_skipped_empty_shell_total` (`widget_content_metrics.go`) | widgetContent fast-path skip counters (RBAC-sensitive widget; empty-shell decline) | informational — attribute content-cell misses |
| `snowplow_widget_skipped_undeclared_extras_put_total` | Puts quarantined by the F6 undeclared-request-extras allowlist | non-zero means a client sends extras the widget author has not declared in `spec.keyExtras` |

### RAFullList serve path — the cheap Go-slice serve for big LISTs
Defined in `internal/cache/bindings_by_gvr_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_ra_full_list_serve` | `map{hit, repopulate, verified_slice, fallback}` serve-outcome counters for the RAFullList cell | admin's first compositions `/call` should drive `hit`+1 over a warm prewarm-pinned cell; rising `fallback` = the cheap path is not engaging |
| `snowplow_ra_full_list_memo` | per-(RA × sliceShape) sliceability verdict snapshot | diagnostic for the RAFullList failure modes |
| `snowplow_sliceability_reverify` | async sliceability-reverify worker counters | evidence the stuck-false reverify path is firing (informer event → re-verify) |
| `snowplow_bindings_by_gvr_delta_skipped_non_typed` | `uint64` — delta-event objects neither typed nor convertible, and DROPPED | **0**. Non-zero = the bindings-by-GVR index is drifting until the next boot rebuild (a silent-data-staleness canary) |

### Prewarm — boot warm-up completion + walk coverage

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_prewarm_complete` (`internal/cache/prewarm_complete_metric.go`) | `map{done:0/1, elapsed_ms}` — `done=1` once `Phase1Done` flips (same atomic `/readyz` reads); `elapsed_ms` = process-start→done, `-1` until flip | `done` reaches `1`; `elapsed_ms` is the cold-start-to-ready time |
| `snowplow_readyz_backstop_fired` (`internal/handlers/dispatchers/readiness_backstop_metrics.go`) | `int` — readiness flipped via a backstop arm (seed error / timeout / panic / boot error) instead of the first-nav happy path; each fire also emits a `readyz.backstop.fired` ERROR with the reason | **0** on a healthy boot; any non-zero = a FAILED-but-serving (Ready-degraded) boot — alert |
| `snowplow_phase1_units_planned` / `snowplow_phase1_units_seeded` / `snowplow_phase1_apiref_pages_total` / `snowplow_phase1_eligible_no_continue_total` (`phase1_walk_pagination_metrics.go`) | apiRef-pagination walk planning/seeding counters (widgetContent cells planned, seeded, extra pages resolved, single-page widgets) | `units_seeded` reconciles with `units_planned` minus skips; pages ≫ page-cap × widget-count confirms the backstop raise |
| `snowplow_phase1_walk_children` / `snowplow_phase1_walk_zero_children_total` / `snowplow_phase1_walk_observations_total` (`phase1_walk_metrics.go`) | per-root boot-walk fan-out + zero-children observations | high zero-children ratio = roots resolving empty (RBAC/data gap, or the pre-barrier pass — the engine re-walk covers it) |
| `snowplow_phase1_configvars_skipped_total` (`phase1_configvars_watch.go`) | config-vars watch events skipped by the data-change gate (metadata-only churn, e.g. CDC traceparent re-stamps) | climbs harmlessly under CR churn; a boot re-drive fires only on real `config.json` change |

### Prewarm engine — the unified walk/seed worker
Defined in `internal/handlers/dispatchers/prewarm_engine_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_prewarm_engine_enqueued_total` | `uint64` — cumulative `enqueueScope` calls (every enqueue, even dedup-coalesced) | climbs during boot/re-walk/keepwarm |
| `snowplow_prewarm_engine_processed_total` | `uint64` — scopes fully processed by the worker | `processed ≈ enqueued − dedups` means the queue drained |
| `snowplow_prewarm_engine_requeued_total` | `uint64` — engine-owned boot-scope requeues (the F.4 deadline-cut resume) | small; unbounded climb = the resume bound regressed |
| `snowplow_prewarm_engine_yield_total` | `uint64` — worker parked because a customer `/call` was in flight (customer-priority yield) | >0 under customer load = the yield hook is working |
| `snowplow_prewarm_engine_pending_depth` | live queue depth | **0** once the worker drains; sustained non-zero across many scrapes = worker dead |

### Phase-1 seed — per-target seed outcomes
Defined in `internal/handlers/dispatchers/phase1_pip_metrics.go` (+ siblings).

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_phase1_bindingset_seed_resolves_total` | `uint64` — seed UNITS resolved + written to per-binding L1 | climbs during seed; `0` after boot means no seed unit was written |
| `snowplow_phase1_bindingset_seed_failures_total` | `uint64` — grand-total seed failures (= rbac_deny + operational; back-compat) | interpret via the split below |
| `snowplow_phase1_seed_rbac_deny_total` | `uint64` — EXPECTED narrow-RBAC denies; cohort genuinely can't read the target | non-zero is **normal**; not re-enqueued |
| `snowplow_phase1_seed_operational_fail_total` | `uint64` — UNEXPECTED failures (ctx timeout/cancel, 5xx, transport, panic) | **0**. Non-zero = a real hole; these ARE re-enqueued |
| `snowplow_phase1_seed_fresh_skip_total` | `uint64` — seed units skipped because the cell is still fresh (boot-resume / keepwarm passes) | evidence the resume/sweep is incremental, not redundant |
| `snowplow_phase1_keepwarm_age_skip_total` | `uint64` — keepwarm sweep age-skips (cell young enough) | climbs with sweeps over a warm store |
| `snowplow_phase1_seed_skipped_stage_error_total` | `uint64` — seed Puts declined by the error-aware Put-gate | low; a climb = a systematically-degraded seed target |
| `snowplow_phase1_widget_seed_failure_total` / `snowplow_phase1_restaction_seed_failure_total` | per-cohort×object failure maps | pinpoint which widget/RA broke which cohort |
| `snowplow_resolved_cache_hits_seed_attributable` | hits on cells the seed wrote (seed attribution) | the seed-is-actually-useful signal |

### Refresher — background re-resolve worker pool
Defined in `internal/cache/refresher_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_refresher_enqueue_total` / `_completed_total` / `_failed_total` / `_retried_total` / `_dropped_total` | task lifecycle counters | completed tracks enqueue under steady state; failed/retried/dropped low |
| `snowplow_refresher_skipped_no_entry_total` / `_skipped_no_handler_total` / `_skipped_stage_error_total` | skip reasons | informational |
| `snowplow_refresher_queue_depth` | live workqueue `Len()` | near 0; climbing depth with stagnant `completed_total` = workers stuck (back-pressure) |
| `snowplow_refresher_yielded_total` | worker yield-parked for a customer `/call` | >0 under customer burst (if 0, hook broken) |
| `snowplow_refresher_capped_total` | yield max-parked cap fired (proceeded anyway) | **near 0**; steady climb = inflight counter leaking or sustained pressure |
| `snowplow_refresher_floored_total` | dequeue rate-floor deferred a key (entry younger than floor) | >0 under install-churn storm = the floor gate is protecting against re-resolve storms |

### Live refresh (SSE)
Defined in `internal/cache/refresh_broadcaster_expvar.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_refresh_broadcaster` | broadcaster counter snapshot (publishes, deliveries, subscriber counts, drops) | drops near 0; publishes track L1 commits under churn |

### RBAC snapshot + authz memo — subject-index freshness and serve-time eval cache

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_rbac_publish_seq` (`internal/cache/rbac_snapshot_expvar.go`) | `uint64` — incremented once per successful RBAC-snapshot publish (also the point per-subject sub-generation bumps land) | bumps within ~30s of a RoleBinding ADD/DELETE; `0` = no snapshot published (cache-off or pre-readiness) |
| `snowplow_authz_memo_hits` / `_misses` / `_swaps` / `_refused` / `_entries` (`internal/rbac/snapshot_authz_memo.go` via `RegisterAuthzMemoExpvar`) | memo hit/miss, generation shard swaps, cap-breach refusals, live entry count | hit rate ≥0.85 warm; swaps bump on snapshot generation change; refused low |
| `snowplow_authz_memo_deny_uncached_total` | `uint64` — denies (never cached, by design) | informational; should be > 0 and rising on a live cluster — a flat 0 with denied traffic would suggest the PERMITS-only rule regressed |

### Informer / discovery surface

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_plurals_registered_gvrs` (`internal/cache/registered_gvrs_expvar.go`) | `{count, gvrs:[…], last_register_unix_ns}` — live set of GVRs with a registered informer | `count` tracks the cluster's served GVR set; two scrapes with identical `last_register_unix_ns` = informer set quiesced |
| `snowplow_crd_discovery` (`internal/cache/crd_discovery_expvar.go`) | `map{events_enqueued, events_dropped, events_processed, discovery_invoked, discovery_skipped_ng, deletes_processed, delete_skipped_ng, panics_recovered}` | `events_dropped`/`*_skipped_ng`/`panics_recovered` should be **0** — a non-zero `discovery_skipped_ng` is a flashing red flag (silent-skip defect class) |
| `snowplow_crd_schema_memo_hits_total` / `_misses_total` / `_stale_dropped_total` / `_invalidations_total` (`internal/resolvers/crds/schema/schema_cache_metrics.go`) | compiled-CRD-schema memo counters | high hit ratio warm; stale-drops expected under concurrent CRD install |
| `snowplow_sa_discovery_builds_total` / `_invalidations_total` / `_fallbacks_total` (`internal/dynamic/cached_client_metrics.go`) | SA-discovery client lifecycle | fallbacks low; climb = discovery degrading |

### Upstream controller health — "is snowplow broken, or is an upstream controller crash-looping?"
Defined in `internal/cache/controller_health_expvar.go` / `controller_health.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_upstream_controller_health` | `map["<ns>/<name>"→{Healthy, Reason, PodRestartCount, EndpointReadyCount, …}]` for auto-discovered controllers | every entry `Healthy=1`, `Reason=""`. `Reason` enum: `pod-restart-within-window`, `endpoints-zero-ready`, `both`, `unwired` |
| `snowplow_upstream_webhook_failurepolicy` | `map["<webhookName>"→{Policy:"Fail"/"Ignore", Configuration, Type}]` | a `Fail`-policy webhook on a crash-looping controller explains apiserver pressure / write hangs |

---

## Key `slog` events

JSON structured logs on stdout. Message strings are stable, dotted, and greppable. The
operator-notable ones:

| Event (message) | Level | Site | What it tells an operator |
|---|---|---|---|
| `phase1.warmup.completed` | Info | `dispatchers/phase1_walk.go` | the prewarm walk finished — the boundary `/readyz` and `snowplow_prewarm_complete` track |
| `phase1.warmup.roots_list_failed` | Warn | `dispatchers/phase1_walk.go` | the frontend config-vars ConfigMap was absent at boot; the config-vars informer will re-drive a boot re-walk when it lands (self-heal, no restart) |
| `readyz.backstop.fired` | Error | `dispatchers/readiness_backstop_metrics.go` | readiness flipped **Ready-degraded** via a backstop arm (reason attached); pairs with `snowplow_readyz_backstop_fired` — alert |
| `phase1.seed.sync_incomplete` / `phase1.seed.panic` | Warn / Error | `dispatchers/phase1_walk.go` | the synchronous boot seed erred/panicked; readiness still flipped (backstop) with cold cells |
| `phase1.seed.cohort.operational_failure` | Warn+ | dispatchers phase1 seed | a cohort hit an UNEXPECTED seed failure — pairs with `snowplow_phase1_seed_operational_fail_total`; actionable |
| `phase1.seed.cohort.expected_deny` | Info | dispatchers phase1 seed | EXPECTED narrow-RBAC deny — normal, pairs with `snowplow_phase1_seed_rbac_deny_total` |
| `phase1.walk.apiref_pagination.backstop_hit` | Warn | `dispatchers/phase1_walk_pagination.go` | a widget's apiRef pagination hit the anti-runaway page ceiling — coverage may be capped |
| `apiserver_fallthrough` | Debug | `internal/cache/fallthrough_meter.go` | a read punted to the apiserver — the log companion to `snowplow_apiserver_fallthrough_total`; includes path/gvr/reason. **Demoted Warn→Debug in 1.12.2** (commit `52eb46d`): a warm pod still punts routinely, so at Warn it drowned the log. Track the counter, not the line; raise `LOG_LEVEL=debug` to see it. |
| `cache.read_paths_scoped.violation` | Error | `internal/cache/fallthrough_assert.go` | architectural invariant breach — a `/call` route is not scope-wrapped |
| `cache.serve_requires_servable.violation` | Error | `internal/cache/serve_assert.go` | **P1** invariant breach — a cache HIT was about to be served from a not-servable informer (names the gvr + serve_path) |
| `cache.bindings_by_gvr.delta_skipped_non_typed` | Warn | `internal/cache/bindings_by_gvr_delta.go` | an index delta event was dropped — index is drifting; pairs with the same-named expvar |
| `cache.crd_discovery.event_dropped` | Warn | `internal/cache/crd_discovery_side_effect.go` | a CRD-discovery event was dropped (queue full / shutdown) |
| `cache.controller_health.watch.broken` | Warn | `internal/cache/controller_health.go` | an upstream controller-health watch broke — the health gauge may go stale |
| `cache.rbac.snapshot.published` | Info | `internal/cache/` rbac snapshot | a new RBAC subject-index snapshot published; pairs with `snowplow_rbac_publish_seq` |
| `config.retired_flag_ignored` | Warn/Info | `internal/cache/retired_flags.go` | a retired prewarm/pivot env var is still set; Warn when the value is `false` (a silent behavior change the operator did not consent to — the flag is implicit-on-cache now) |
| `dispatcher.call.complete` | Info | `dispatchers/per_call_log.go` | per-dispatch structured timing (the per-call diagnostic) |
| `cache.secrets.informer.assertion_violation` | Error | `internal/cache/secrets_informer.go` | secrets-informer invariant breach |

There are many more dotted events in the `phase1.*`, `prewarm.engine.*`,
`cache.crd_discovery.*`, `cache.rbac.snapshot.*`, and `cache.discovery.*`
families (full set is greppable with
`grep -rhoE '"(phase1|prewarm|cache)\.[a-z_.]+"' internal main.go`); the table
above lists the ones that map to an alarm-worthy condition or pair directly
with an expvar.

---

## pprof

Registered on the custom server mux (the server does **not** use
`http.DefaultServeMux`), `main.go`:

| Path | Profile |
|---|---|
| `GET /debug/pprof/` | index (links to goroutine, heap, allocs, threadcreate, block, mutex, …) |
| `GET /debug/pprof/cmdline` | process command line |
| `GET /debug/pprof/profile` | 30s CPU profile |
| `GET /debug/pprof/symbol` | symbol lookup |
| `GET /debug/pprof/trace` | execution trace |

Mutex + block profiling fractions are set at startup so `/debug/pprof/mutex`
and `/debug/pprof/block` return non-empty data.

Typical use:

```
go tool pprof http://<pod>:<port>/debug/pprof/heap          # memory
go tool pprof http://<pod>:<port>/debug/pprof/profile       # CPU (30s)
curl http://<pod>:<port>/debug/pprof/goroutine?debug=2      # goroutine dump (deadlock/leak)
```

Note: responses are gzip-compressed only when the client sends
`Accept-Encoding: gzip` (the `gzhttp` wrapper); plain `curl` diagnostics are
byte-identical to pre-compression behaviour, and the SSE routes are excluded
from compression buffering.

---

## Notes / discrepancies vs. earlier revisions

- Several phase-1 seed expvars referenced in older notes
  (`snowplow_phase1_seed_restactions_total`, `snowplow_phase1_seed_widgets_total`
  and their `_by_cohort` maps; the binding-set classes / powerset-skipped
  counters; the cohort_seed_status gauge) were **deleted** as always-zero or
  orphaned and are intentionally NOT in the code today — they are excluded
  here.
- An earlier revision of this doc claimed the chart wires the startupProbe to
  `/readyz`; the chart actually points **startupProbe → `/health`** (with a
  long failure budget) and only the readinessProbe at `/readyz`.
- The observability surface used to be expvar/slog/pprof only; the OTel export
  (traces + expvar-mirror metrics + log/audit bridge) is additive and
  default-off — enabling it changes no expvar semantics.
