---
type: Configuration
title: snowplow — configuration
description: The whole config surface — Helm values, the env-var ConfigMap contract, probes and runtime tuning — with defaults.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [helm, values, env, tuning]
timestamp: 2026-08-06T00:00:00Z
---

# Configuration

Everything is driven by the `snowplow` Helm chart
([`helm/snowplow/values.yaml`](../helm/snowplow/values.yaml)). The authoritative
machine-readable surface is
[`helm/snowplow/values.schema.json`](../helm/snowplow/values.schema.json) — every value
below is typed and validated there; this page explains, it does not re-enumerate.

## The env contract

The container takes **no direct `env:` array**. Every value that reaches the process
goes through `envFrom` ([`deployment.yaml`](../helm/snowplow/templates/deployment.yaml)):

1. the chart-managed `snowplow` ConfigMap — rendered from `.Values.env`;
2. a small `env:` array for the JWKS knobs (`jwt.*` below) — no key material, just
   the URL and cache tuning;
3. `extraEnvFrom` — by default the optional `snowplow-api-override` ConfigMap, so the
   portal blueprint can layer config without re-templating this chart.

**No key Secret is mounted.** snowplow verifies RS256 JWTs against authn's public key
fetched from authn's JWKS endpoint (`/.well-known/jwks.json`), so it holds no copy of
the key: rotating authn's keypair needs no snowplow redeploy and there is no
public-key Secret to keep in sync. The key set is fetched **lazily on the first token
validation, not at startup** — snowplow starts and serves its unauthenticated routes
even when authn is not up yet, and recovers by itself once authn answers. While the
key set cannot be fetched, authenticated requests get `503` (retryable), not `401`.

A `checksum/configmap` pod annotation rolls the Deployment when the ConfigMap changes.

## Helm values (top level)

| Value | Default | Effect |
|---|---|---|
| `image.registry` / `image.repository` | `ghcr.io` / `krateo-platformops/snowplow` | The app image. `global.imageRegistry` relocates the registry host for mirror/air-gapped installs (repository path preserved). |
| `image.tag` | `""` (= chart `appVersion`) | Pin only to diverge from the released pairing. |
| `service.type` / `service.port` | `LoadBalancer` / `8081` | One port serves everything: content, probes, debug. |
| `replicaCount` | `1` | With `autoscaling.enabled: false` (default). |
| `strategy` | `RollingUpdate`, `maxSurge: 1`, `maxUnavailable: 0` | Zero-gap deploys: the new pod prewarms behind `/readyz` while the old warm pod keeps serving. |
| `progressDeadlineSeconds` | `1200` | Rollout deadline; must exceed informer sync + prewarm seed at scale. |
| `resources` | limits `4 cpu / 8Gi`, requests `2 cpu / 4Gi` | Pre-sized for 50K compositions × 1000 users (informer + in-process L1 cache). |
| `startupProbe` / `livenessProbe` | `/health`, widened thresholds | `/health` binds early and never gates prewarm; liveness must not kill a warming pod. |
| `readinessProbe` | `/readyz` | Flips 503→200 on **prewarm-complete**, not mere informer sync. |
| `ingress` | `enabled: false` | Standard chart ingress if you need it. |
| `jwt.jwksUrl` | `""` (derives from `URL_AUTHN`) | Where the RS256 verification keys come from. Empty derives `<URL_AUTHN>/.well-known/jwks.json` — one URL to get wrong, not two. Set only to point at a different authn. |
| `jwt.cacheTTL` | `5m` | How long a fetched key set is served before refresh. Signing keys rotate rarely, so this keeps authn off the hot path of every validation. |
| `jwt.minRefreshInterval` | `30s` | Floor between two JWKS fetches. Throttles the refetch an unrecognised `kid` triggers, so tokens naming a key authn never published cannot become one fetch per request. |
| `jwt.requestTimeout` | `5s` | Per-fetch timeout. Also bounds how long a validation can block, since concurrent cache misses collapse into one fetch. |
| `seedAuthn.*` | group `krateo:snowplow-seed`, namespace `krateo-system`, audience `authn` | The prewarm-seed loopback-auth artifacts (allowlist CR + ClusterRole/Binding + projected token) — **always rendered**; see [usage](./usage.md) for the hard dependency. |
| `extraEnvFrom` | `snowplow-api-override` ConfigMap (optional) | Extra env sources appended after the defaults. |
| `env.*` | see below | Rendered into the `snowplow` ConfigMap. |

## `env.*` — the runtime knobs (chart defaults)

| Env var | Default | Effect |
|---|---|---|
| `CACHE_ENABLED` | `"true"` | The single master cache gate. `false` = transparent fallback to the direct apiserver — same data, same RBAC, slower ([ADR 0004](../go/snowplow/docs/adr/0004-cache-is-provisional-and-removable.md)). |
| `GOMEMLIMIT` | `7GiB` | **Must stay strictly below the container memory limit** (default 8Gi) so Go GC back-pressures before Linux OOM-kills. Move it with `resources.limits.memory`. |
| `GOGC` | `"50"` | Tighter heap for ~1% CPU. |
| `DEBUG` | `"false"` | Verbose logging. |
| `PRETTY_LOG` | `"false"` | `false` = single-line JSON logs (the pretty handler is a measured CPU/I/O drag in prod). |
| `PHASE1_TIMEOUT_SECONDS` | `"900"` | Outer backstop for the whole boot warmup (informer sync + prewarm walk + cohort seed). |
| `PHASE1_SYNC_PASS_GRACE_SECONDS` | `"45"` | Per-pass grace for one `WaitForCacheSync` pass. |
| `PHASE1_SYNC_QUIESCENCE_SECONDS` | `"10"` | Registered-set stability window before the sync barrier returns. |
| `CATALOG_UNSERVABLE_TTL_SECONDS` | `"300"` | Short TTL for entries cached while their informer wasn't servable (self-correcting safety net); `0` disables. |
| `URL_AUTHN` / `URL_SELF` | authn / snowplow in-cluster service URLs | The prewarm-seed loopback-auth pair: exchange the projected SA token at authn, append the JWT only on exact-`URL_SELF` loopback calls. |
| `SERVICEACCOUNT_TOKEN_PATH` | `/var/run/secrets/krateo.io/serviceaccount/token` | Where the projected (audience=`authn`) token is mounted. |
| `JQ_MODULES_PATH` | `/jq-modules` | Custom jq modules, mounted from the chart's `jq-custom-modules` ConfigMap. |
| `BLIZZARD` | `"false"` | Extra-verbose output dump (`main.go` `-blizzard` flag). |

The binary reads more env vars than the chart sets by default (`PORT` — default `8081`;
`AUTHN_NAMESPACE`; the fine-grained cache back-out knobs; the prewarm-engine gates).
Anything the process should see goes in `env.*`; for the full operator view of what
each knob does at scale (sizing, probe philosophy, incident table) read
[operating.md](../go/snowplow/howto/operating.md), and for the per-subsystem semantics
the deep dives under
[`go/snowplow/docs/architecture/`](../go/snowplow/docs/architecture/).

## The CRDs chart

`snowplow-crds` ([`helm/snowplow-crds/`](../helm/snowplow-crds/)) has no configuration:
it installs the `RESTAction` CRD ([api](./api.md)). It is deliberately **not** a
dependency of the app chart, so CRD ownership stays with its own release.
