---
type: Architecture
title: snowplow — overview
description: What snowplow is (the Krateo content API), how a /call request flows, and where it sits between authn, frontend and sse-proxy.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [portal, content-api, restaction, cache]
timestamp: 2026-08-06T00:00:00Z
---

# Overview

snowplow is the **content API of the Krateo portal**: it resolves `RESTAction` and
frontend `Widget` custom resources into the JSON the Krateo frontend renders, served
over `GET /call`. It is a content bridge, **not a BFF** — it holds no product state and
composes content on demand from Kubernetes CRs, serving any client that can present a
Krateo JWT (the SPA, or `curl`).

The canonical, code-traced map of the internals is
[`go/snowplow/ARCHITECTURE.md`](../go/snowplow/ARCHITECTURE.md); the subsystem deep
dives under [`go/snowplow/docs/architecture/`](../go/snowplow/docs/architecture/) trace
every claim to `file:line`. This page is the distilled platform view — it links, it
does not duplicate.

## What it does

- **Resolves `RESTAction` CRs** ([api](./api.md)): declarative chains of HTTP calls
  (Kubernetes API or external endpoints) with JQ filtering — resolved on demand when
  `/call` is invoked, results returned in the resource's `status`.
- **Resolves frontend `Widget` CRs**: the widget resolver shapes RESTAction output into
  render-ready widget props (the widget CRDs themselves are owned by
  [frontend](https://github.com/krateo-platformops/frontend)).
- **Write passthrough**: `POST/PUT/PATCH/DELETE /call` are a raw apiserver passthrough
  under the caller's identity — never resolved, never cached.

## The request path

```
GET /call ─▶ Dispatcher middleware ─▶ dispatcher (restactions | widgets) ─▶ resolve
                                              │
                                 RBAC gate + serve-time filter ─▶ serialize ─▶ write
```

The load-bearing layering contract: a `RESTAction` emits *unordered* data; the *widget*
canonicalizes and shapes it. Full trace:
[request-lifecycle](../go/snowplow/docs/architecture/request-lifecycle.md).

## The cache (provisional by design)

A three-tier cache (L3 informer → L1 resolved-entry → dispatcher) with per-binding-UID
identity keying and a prewarm engine that replays frontend navigation at boot. Two
invariants matter platform-wide:

- **Per-binding-UID L1 keying, never cohort-only** — cohort-only keying leaks one
  user's resources to another
  ([rbac-uaf](../go/snowplow/docs/architecture/rbac-uaf.md),
  [ADR 0002](../go/snowplow/docs/adr/0002-per-user-l1-keying.md)).
- **All caching is provisional and cleanly removable** — `CACHE_ENABLED=false` is a
  transparent fallback to the direct apiserver: same data, same UI, same RBAC, just
  slower ([ADR 0004](../go/snowplow/docs/adr/0004-cache-is-provisional-and-removable.md),
  [caching](../go/snowplow/docs/architecture/caching.md)).

Prewarm, the boot walker and readiness gating:
[prewarm](../go/snowplow/docs/architecture/prewarm.md). The performance contract and how
it is measured: [north-star](../go/snowplow/docs/architecture/north-star.md).

## Where it sits in the platform

| Peer | Relationship |
|---|---|
| **authn** | Issues the Krateo JWTs snowplow validates — signed asymmetrically (RS256) with authn's private key; snowplow verifies them against authn's **public** key, which it fetches and caches from authn's JWKS endpoint at `/.well-known/jwks.json` (no key material is mounted into snowplow, so authn can rotate its keypair without a snowplow redeploy). The prewarm seed exchanges a projected ServiceAccount token at authn for its loopback JWT — a hard install-time dependency on the `serviceaccount.authn.krateo.io` CRD (see [usage](./usage.md)). |
| **frontend** | The SPA renders what `/call` returns; snowplow reads the frontend nav ConfigMaps (`INIT` / `ROUTES_LOADER`) as prewarm roots. Widget CRDs are frontend-owned. |
| **sse-proxy** | Serves the portal's *event* streams; snowplow serves the portal's *content* (it also has its own `/refreshes` SSE lane for live-refresh nudges). |
| **core-provider / cdc** | Compositions and their CRs are the bulk of what RESTActions read; `GET /rbac` enumerates a RESTAction's read-set so core-provider can pre-generate RBAC. |

## Deployment shape

One Deployment, one container, one port (`8081`) serving content, probes and debug
surfaces alike. All configuration arrives as env vars via a chart-managed ConfigMap
([configuration](./configuration.md)); the operator runbook is
[operating.md](../go/snowplow/howto/operating.md).
