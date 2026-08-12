---
type: Usage
title: snowplow — usage
description: How snowplow is installed — via the Krateo installer component pin, or directly from the OCI chart — and its hard install-time dependencies.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [install, helm, installer]
timestamp: 2026-08-06T00:00:00Z
---

# Usage

snowplow ships as two Helm charts from this monorepo, published by CI to GHCR at the
**same version = the repo tag** (see [release](./release.md)):

| Artifact | Where |
|---|---|
| App chart | `oci://ghcr.io/krateo-platformops/charts/snowplow` |
| CRDs chart (`RESTAction`) | `oci://ghcr.io/krateo-platformops/charts/snowplow-crds` |
| Image (multi-arch) | `ghcr.io/krateo-platformops/snowplow` |

## Path 1 — via the Krateo installer (the normal way)

snowplow is a core component of the Krateo installer: the installer umbrella pins the
chart URL + version and reconciles snowplow (and `snowplow-crds`) as Compositions. A
platform install gets snowplow with no extra steps; a version bump is a change to the
installer's pin.

Standalone of the installer, the same mechanism is a raw `CompositionDefinition`
(requires core-provider), as in [`compositiondefinition.yaml`](../compositiondefinition.yaml):

```yaml
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
metadata:
  name: krateo-snowplow
  namespace: krateo-system
spec:
  chart:
    url: oci://ghcr.io/krateo-platformops/charts/snowplow
    version: "1.9.0"
```

## Path 2 — direct `helm install`

```sh
# CRDs first (RESTAction + the authn ServiceAccount CRD the app chart hard-requires):
helm install snowplow-crds oci://ghcr.io/krateo-platformops/charts/snowplow-crds --version 1.9.0
helm install authn-crds oci://ghcr.io/krateo-platformops/charts/authn-crds

# The app chart:
helm install snowplow oci://ghcr.io/krateo-platformops/charts/snowplow \
  --version 1.9.0 --namespace krateo-system
```

### Hard install-time dependencies (fail-loud by design)

The chart renders a `serviceaccount.authn.krateo.io` allowlist CR for the prewarm-seed
loopback token exchange, **unconditionally**. So the install fails fast unless the
cluster has:

- the `serviceaccount.authn.krateo.io` CRD (chart `authn-crds`), and — for the exchange
  to work at runtime — the Krateo **authn** operator (>= 0.24.0).

**No key material is required at install time.** snowplow fetches authn's RSA public
key from authn's JWKS endpoint (`<URL_AUTHN>/.well-known/jwks.json`) on the first token
validation, so there is no public-key Secret to pre-create and nothing to re-sync when
authn rotates its keypair.

The installer sequences authn before snowplow anyway, but snowplow does not depend on
that ordering: because the JWKS fetch is lazy, snowplow starts without authn and
recovers on its own once authn answers. Without a running authn the install still
succeeds (CRD present is enough) and unauthenticated routes serve normally;
authenticated requests get `503` until the key set can be fetched. Details in the
`seedAuthn` and `jwt` blocks of
[`helm/snowplow/values.yaml`](../helm/snowplow/values.yaml).

## Quickstart on Kind

A full single-node walkthrough (namespace, JWT secret, minting a user token, RBAC,
NodePort install, executing your first RESTAction):
[Installing snowplow on Kind](../go/snowplow/howto/install.md). Then run the
[examples](./examples.md).

## Rendering the chart locally

The in-repo `Chart.yaml` carries `CHART_VERSION` / `APP_VERSION` placeholders that CI
substitutes at release time, so substitute them before a local render (exactly what the
lint workflow does):

```sh
sed -i.bak 's/CHART_VERSION/0.0.0/g; s/APP_VERSION/0.0.0/g' helm/snowplow/Chart.yaml
helm template snowplow helm/snowplow
git checkout helm/snowplow/Chart.yaml && rm -f helm/snowplow/Chart.yaml.bak
```

## Upgrading

Reconcile via `helm upgrade` (or bump the installer pin). **Never** `kubectl set image`
or otherwise mutate the running Deployment out of band — the chart is the source of
truth. Rolling deploys are zero-gap by design: `maxUnavailable: 0` + prewarm-gated
`/readyz` keep the old warm pod serving until the new pod is warm
([configuration](./configuration.md)).

## Calling it

Every content read is `GET /call` with a Krateo JWT — see [api](./api.md) and the
[examples](./examples.md).
