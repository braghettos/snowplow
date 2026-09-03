---
type: API
title: snowplow — API
description: The RESTAction CRD (templates.krateo.io) and the HTTP surface snowplow serves on its single port.
resource: restactions.templates.krateo.io
tags: [crd, restaction, http, openapi]
timestamp: 2026-08-06T00:00:00Z
---

# API

snowplow exposes two contracts: the **`RESTAction` CRD** it owns, and the **HTTP
surface** it serves (which also resolves the frontend-owned `Widget` CRDs).

## The `RESTAction` CRD

`restactions.templates.krateo.io` — group `templates.krateo.io`, version `v1`, kind
`RESTAction`, namespaced, short name `ra`. Shipped by the
[`snowplow-crds` chart](../helm/snowplow-crds/templates/templates.krateo.io_restactions.yaml);
Go types under [`go/snowplow/apis/templates/`](../go/snowplow/apis/templates/) are the
source of truth (the CRD is generated, drift-gated in CI).

A `RESTAction` declaratively defines one or more HTTP calls (`spec.api[]`) that may
depend on each other. Each call's JSON (or YAML — converted transparently,
[ADR 0006](../go/snowplow/docs/adr/0006-snowplow-owned-external-fetch.md)) response
joins a shared context that later calls and JQ expressions can reference.

| `spec.api[]` field | Meaning |
|---|---|
| `name` | Stage identifier — and the key its filtered result lands under in the returned `status`. |
| `verb`, `path`, `headers`, `payload` | The HTTP request (verb defaults to `GET`). |
| `endpointRef` | Reference to an [`Endpoint`](../go/snowplow/howto/endpoints.md) Secret (server URL, auth, TLS, proxy). Absent = the Kubernetes API server. |
| `dependsOn` | Chains this stage after another; its output is referenceable via JQ. |
| `filter` | JQ expression applied to the response ([custom modules](./configuration.md) available). |
| `continueOnError`, `errorKey` | Keep going on failure; where the error lands in `status`. |
| `exportJwt` | Export a JWT obtained by this stage for later stages. |
| `resolve` | When `path` points at a `RESTAction`/`Widget` CR, resolve it in-process (default `true`). |
| `userAccessFilter` | Dispatch the read via snowplow's ServiceAccount, then RBAC-refilter the result per item against the requesting user. CEL rules on the CRD enforce read-verb-only, no `exportJwt`, and the `resource`/`resourcesFrom` XOR. |

A top-level `spec.filter` shapes the overall output.

**Lifecycle — resolved on read, no controller.** Nothing reconciles a `RESTAction`.
Applying one stores inert spec; the calls execute when the CR is read **through
`GET /call`**, and the resolved output is returned in the response's `status`. Reading
the CR via `kubectl` shows it as-is; snowplow does not persist resolved results back to
the cluster (the CRD's status subresource is free-form for compatibility). Caching and
prewarm may resolve ahead of time, but that is transparent
([overview](./overview.md)).

Authoring references: [restactions.md](../go/snowplow/howto/restactions.md) (full field
reference), [endpoints.md](../go/snowplow/howto/endpoints.md),
[widgets.md](../go/snowplow/howto/widgets.md), and the worked
[examples](./examples.md).

## The HTTP surface

Everything is served on the single `http` port (default `8081`). Authenticated routes
take a Krateo JWT (`Authorization: Bearer …`), validated as RS256 against authn's
public key, fetched and cached from authn's JWKS endpoint
(`/.well-known/jwks.json`; see [configuration](./configuration.md)). The
authoritative machine-readable spec is
[`go/snowplow/docs/swagger.json`](../go/snowplow/docs/swagger.json) (served live at
`GET /swagger/`).

| Route | What it does |
|---|---|
| `GET /call` | **The content endpoint.** Resolves the referenced `RESTAction` or `Widget` (`apiVersion`, `resource`, `name`, `namespace` query params; optional [`extras`](../go/snowplow/howto/extras.md)) — dispatched, RBAC-gated, cached. |
| `POST/PUT/PATCH/DELETE /call` | Write passthrough to the apiserver under the caller's identity — never resolved, never cached. |
| `GET /export` | Any `/call`-resolvable list as a CSV/JSON attachment; re-dispatches in-process with identical auth/RBAC ([export.md](../go/snowplow/howto/export.md)). |
| `GET /list` | List resources by category in a namespace. |
| `GET /api-info/names` | API names/plurals discovery helper. |
| `GET /rbac` | Enumerates the (group, version, resource, verb) read-set a RESTAction *would* perform, without dispatching — for RBAC pre-generation. |
| `GET /refreshes` | Per-subject live-refresh SSE stream (cookie-or-header JWT). |
| `POST /jq` | Evaluate a JQ expression against a JSON input. |
| `GET /health`, `GET /readyz` | Liveness (always 200 once serving) / readiness (200 on prewarm-complete). |
| `GET /swagger/` | The OpenAPI UI + spec. |
| `GET /debug/vars`, `/debug/pprof/*`, `/debug/servable`, `/debug/apistage`, `/debug/refreshes` | Operator diagnostics ([observability](../go/snowplow/docs/architecture/observability.md)); `/debug/refreshes` is JWT-gated. |

Note: the spec also describes `POST /convert` (YAML↔JSON), but that route is currently
not wired in `main.go` — the spec is a superset on this one endpoint.
