---
type: Runbook
title: Operating snowplow
description: Operator runbook for the snowplow Helm deployment — probe contract, GOMEMLIMIT/GOGC tuning at 50K scale, cache on/off semantics, observability surface and the incident table.
resource: snowplow
tags:
  - snowplow
  - runbook
  - operations
  - tuning
  - observability
timestamp: 2026-08-06T00:00:00Z
---

# Operating snowplow

The operator runbook: deploy, tune at scale, and diagnose incidents. For the
internals behind these knobs see the architecture deep-dives:
[caching](../docs/architecture/caching.md),
[prewarm](../docs/architecture/prewarm.md),
[observability](../docs/architecture/observability.md).

---

## Deploy

snowplow ships as a Helm chart from this repo (`helm/snowplow`, published as
`oci://ghcr.io/krateo-platformops/charts/snowplow`). The chart's
defaults are pre-sized for cache-enabled operation at 50K-composition scale; the
load-bearing values are below. The container takes **no direct `env:` array** —
every value goes through the chart-managed `snowplow` ConfigMap and is consumed
via `envFrom` (`values.yaml`, `env:` block).

Minimum the chart needs:

- a reachable authn JWKS endpoint (`jwt.jwksUrl`, empty by default → derived from
  `URL_AUTHN`) — the RS256 verification keys are fetched from there and cached, so no
  key Secret is mounted;
- `CACHE_ENABLED=true` (chart default) to run the cache path;
- resources sized for the informer + in-process cache (chart default
  `limits: 8Gi / 4 cpu`, `requests: 4Gi / 2 cpu`).

The chart also picks up an external `snowplow-api-override` ConfigMap via
`extraEnvFrom` (optional) so the portal blueprint can layer config without
re-templating the chart.

### The chart ⇄ binary probe contract

On the **1.0.x ship line the binary serves everything on one port** — `PORT`
(default `8081`, the `--port` flag in `main.go`), including `/health`, `/readyz`,
`/debug/vars`, and `/debug/pprof/*` (all on the same mux — the `/debug/*` block
is registered by `registerDebugRoutes` in `debug_routes.go`).
There is **no `PROBE_PORT`** and no second listener; a prototype probe-port
split lived only on the abandoned 0.25.x line and is not in the current binary.
So all three probes target the single `http` port.

Because that one port is what the chart publishes (`service.type: LoadBalancer`),
**as of 1.12.3 every `/debug/*` route requires a JWT** — see
[the debug surface is authenticated](#the-debug-surface-is-authenticated) below.
`/health` and `/readyz` stay anonymous: the kubelet presents no credentials, so
gating them would fail every probe.

| Probe | Path | Meaning |
|---|---|---|
| `startupProbe` | `/health` | binary is up and serving (binds early, before prewarm) |
| `livenessProbe` | `/health` | always `200 {"status":"alive"}` — a still-warming pod is alive and must NOT be restarted (`internal/handlers/health.go`) |
| `readinessProbe` | `/readyz` | `200` once `Phase1Done` flips — informer sync **plus** the per-cohort prewarm seed complete (or its ~8min backstop; 1.5.29 prewarm-gated readiness, see `helm/snowplow/values.yaml`); `503 {"status":"warming"}` until then (`internal/handlers/readyz.go`) |

The chart thresholds are **deliberately widened** beyond the k8s defaults
(`timeout 1s / failure 3 / period 10s` ≈ a 30s window). Defaults are too tight for
a 50K-scale cold start, so the chart sets:

- `startupProbe`: `failureThreshold 36 × periodSeconds 10` = up to 6 minutes for
  image pull / scheduler latency before the binary must answer `/health`.
- `livenessProbe`: `failureThreshold 6 × periodSeconds 10` = ~60s window (vs 30s)
  so a transient hiccup doesn't restart a healthy pod.
- `progressDeadlineSeconds: 1200` — a safety net for the cold first-LIST at 50K
  (informer initial LIST/WATCH can take minutes under apiserver load).

---

## Tuning at scale

### `GOMEMLIMIT` — set it BELOW the container memory limit

This is the single most important tuning rule. `GOMEMLIMIT` makes the Go runtime
back-pressure via aggressive GC *before* it grows past the limit. If
`GOMEMLIMIT ≥ container memory limit`, the runtime never sees pressure and Linux
**OOM-kills the pod** instead — the documented cause of past 8Gi OOM incidents.

Chart default: `GOMEMLIMIT: 7GiB` under an `8Gi` container limit (~1GiB headroom).
If you change the memory limit, move `GOMEMLIMIT` with it, always strictly below.

### `GOGC`

Chart default `GOGC: "50"` (vs the Go default 100): trades ~1% CPU for a tighter
heap. Lower it for more memory headroom, raise it to spend less CPU on GC.

### CPU / memory sizing

Right-sized from 50K compositions × 1000 users stress data: peak heap ~3.9GB
(cold start), steady ~3.3GB, in-process L1 ~2–3GB, peak RSS ~6GB. The `8Gi` limit
is contingent on `GOMEMLIMIT` being set correctly (above). Treat a new lower limit
as unproven until validated under Phase-6 load.

### Probe thresholds

Already widened by default (above). If you scale further or run on a slower
apiserver, raise `startupProbe.failureThreshold` and `progressDeadlineSeconds`
rather than narrowing liveness.

### `CACHE_ENABLED` — a transparent fallback, not a degraded mode

`CACHE_ENABLED=false` (`cache.Disabled()`, `internal/cache/cache.go` — only
`"true"`, `"1"`, `"yes"` enable; anything else, including unset, disables) turns off
all three cache tiers. The result is **the same data, same UI, same RBAC — only
slower**: every read goes straight to the apiserver under the user's own token,
and RBAC is enforced inline by the apiserver. It is a correctness-equivalent
fallback, safe to flip if you suspect a cache bug; it is not a reduced-capability
mode. (`CACHE_ENABLED` is the single master gate; the fine-grained back-out knobs
`RESOLVED_CACHE_ENABLED` and `WIDGET_CONTENT_L1_ENABLED` exist only for narrow
rollbacks — see the flag notes at the top of `internal/cache/cache.go`.
`RESOLVED_CACHE_APISTAGE_ENABLED` is **retired**: the api-stage L1 is implicit
under `RESOLVED_CACHE_ENABLED`, setting the old name changes nothing and the
binary warns once about it — see `internal/cache/retired_flags.go` and
[ADR 0004](../docs/adr/0004-cache-is-provisional-and-removable.md).)

Under cache-off, most `snowplow_*` expvars are **absent** from `/debug/vars`
(registered behind an `if cache.Disabled() { return }` init) — absent is expected,
not a defect. See the [observability](../docs/architecture/observability.md)
cache-off contract.

---

## Observability

Everything is on the single `http` port (mux registrations in `debug_routes.go`):

- **`GET /debug/vars`** — expvars (lazy `expvar.Func`, zero per-`/call` cost).
  Full enumeration in [observability.md](../docs/architecture/observability.md).
- **structured `slog`** events on stdout — stable dotted message strings.
- **`GET /debug/pprof/*`** — heap / CPU / goroutine profiles.
- **`GET /debug/servable`** — per-GVR servability snapshot.
- **`GET /debug/apistage`** — resolved-cache entry metadata (never bodies).
- **`GET /debug/refreshes`** — aggregate live-refresh broadcaster counters.

### The debug surface is authenticated

**Since 1.12.3 every route above returns `401` without a valid Krateo JWT.**
Before then only `/debug/refreshes` was gated (#69) and the rest were readable
by anyone who could reach the Service — which, with the chart's default
`service.type: LoadBalancer`, means anyone who could reach the load balancer.

The gate is `middleware.RefreshAuth`: a stateless RS256 validation of any
authn-issued token, read from an `Authorization: Bearer` header **or** the
session cookie, with no apiserver read of its own. Any authenticated identity is
accepted — the endpoints return process-wide diagnostics, not per-user data.

```bash
TOKEN=$(curl -su "$USER:$PASS" http://authn:8082/basic/login | jq -r .accessToken)

curl -H "Authorization: Bearer $TOKEN" http://pod:8081/debug/vars
curl -H "Authorization: Bearer $TOKEN" http://pod:8081/debug/servable

# go tool pprof cannot set a request header, so fetch the profile first.
curl -H "Authorization: Bearer $TOKEN" http://pod:8081/debug/pprof/heap -o heap.pprof
go tool pprof -http=: heap.pprof
```

Expired or missing token → `401`; a token in the query string is **not**
accepted (it would leak into logs and referrers).

#### Known limitation — the debug surface is unavailable when authn is down

`RefreshAuth` maps `jwtutil.ErrKeyUnavailable` to **`503`**
(`internal/handlers/middleware/refreshauth.go:124-127`). So if authn is down or
its JWKS endpoint is unreachable, snowplow cannot verify any token and the
**entire** `/debug/*` surface returns `503` — including `/debug/pprof/goroutine`,
which is exactly what you want during that incident. `/health` and `/readyz` are
unaffected (still anonymous), so the probes and the pod's lifecycle are fine; you
simply lose the diagnostics while the outage lasts.

A `503` here means "snowplow could not fetch the verification key", not "your
credentials are wrong" — do not go hunting for a token problem.

There is no break-glass path today, and a `kubectl exec` into the pod is **not**
one: the gate sits on the mux, not on the network path, so a loopback `curl` from
inside the container gets the same `503`. The follow-up under consideration is to
bind the debug mux to localhost so `kubectl port-forward` always reaches it
regardless of authn's state, or failing that a static operator token.

The Phase 6 bench harness presents this token automatically: `login_all()`
parks the first JWT it obtains and the expvar readers send it. For a
probe-only invocation that never logs in, export `SNOWPLOW_DEBUG_TOKEN`.

What "healthy and warm" looks like:

- `snowplow_prewarm_complete.done == 1`; `/readyz` is `200`.
- `snowplow_dispatch_l1_lookups` shows a high `hit_total/(hit+miss)` ratio.
- `snowplow_apiserver_fallthrough_total` has plateaued (not steadily climbing).
- `snowplow_assertion_violations_total == 0`,
  `snowplow_phase1_seed_operational_fail_total == 0`,
  `snowplow_bindings_by_gvr_delta_skipped_non_typed == 0`.
- `snowplow_refresher_queue_depth` and `snowplow_prewarm_engine_pending_depth`
  near 0.

---

## Incident runbook

| Symptom | Check this first | Likely cause |
|---|---|---|
| **OOM-kill** (pod `OOMKilled`, restart) | container memory limit vs `GOMEMLIMIT` env; `curl -H "Authorization: Bearer $TOKEN" http://pod:8081/debug/pprof/heap -o heap.pprof && go tool pprof heap.pprof` | `GOMEMLIMIT ≥ container limit` so the runtime never back-pressured (set it below); or genuine working-set growth → raise the limit *and* `GOMEMLIMIT` together. |
| **Restart loop** | which probe is failing (`kubectl describe pod`); `/health` 200? `/readyz`? | liveness killing a *warming* pod = probe too tight (widen `startupProbe`/`progressDeadlineSeconds`); `/health` not answering at all = process wedged → goroutine dump `curl -H "Authorization: Bearer $TOKEN" '/debug/pprof/goroutine?debug=2'`. |
| **Stale content** (an object changed, UI didn't) | `snowplow_refresher_queue_depth` + `snowplow_refresher_completed_total`; `snowplow_refresher_dropped_total` / `..._failed_total` | dirty-mark only *enqueues* a re-resolve (stale-while-revalidate by design); a wedged/back-pressured refresher leaves stale content until TTL. A climbing `queue_depth` with flat `completed_total` = workers stuck. Dep-cap drop (`deps.record.cap_reached`) → entries rely on TTL. |
| **Convergence timeout** (a `/call` hangs, client gets HTTP 0) | cache on/off?; `snowplow_apiserver_fallthrough_total` climbing? CPU profile `/debug/pprof/profile` (bearer required) | under cache-OFF, heavy compute can approach the 300s `WriteTimeout` at 50K (`writeTimeout`, `main.go`) — turn cache on. Under cache-ON, sustained fallthrough = prewarm not covering the live mix (check `snowplow_dispatch_l1_lookups` hit ratio + `snowplow_prewarm_engine_pending_depth`). |
| **Everything hits the apiserver** (no cache serving) | `CACHE_ENABLED`; `snowplow_plurals_registered_gvrs.count`; `snowplow_apiserver_fallthrough_cells` | cache disabled, or an informer not yet `HasSynced`, or a specific GVR/reason — the per-cell breakdown attributes it. |
| **403 for a resource the user expects** | `snowplow_rbac_publish_seq` (did a snapshot publish?); cache on/off | cache-on RBAC degrades-to-deny if the in-process snapshot isn't built yet — `seq == 0` means no snapshot (pre-readiness or cache-off). Confirm the RoleBinding exists and a snapshot has published. |
| **Upstream looks broken, not snowplow** | `snowplow_upstream_controller_health` | an auto-discovered controller crash-looping / zero-ready endpoints, or a `Fail`-policy webhook on it (`snowplow_upstream_webhook_failurepolicy`) explaining apiserver write hangs. |

Pair each expvar with its `slog` companion event (e.g.
`snowplow_apiserver_fallthrough_total` ↔ `apiserver_fallthrough`;
`snowplow_assertion_violations_total` ↔ `cache.read_paths_scoped.violation`) — see
[observability.md](../docs/architecture/observability.md) for the full table.
