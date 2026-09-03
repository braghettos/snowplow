---
type: Usage
title: extras — per-request context
description: The ?extras= per-request JSON context for RESTAction and Widget resolves — precedence rules, identity injection, inline (author-declared) defaults, and how extras meet the cache key (spec.keyExtras).
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [portal, restaction, widget, cache]
timestamp: 2026-08-06T00:00:00Z
---

# `extras` — per-request context

`extras` is a per-request JSON object you pass on the query string of a `/call`:

```
GET /call?resource=widgets&apiVersion=…&name=…&extras=%7B%22env%22%3A%22prod%22%7D
                                                        └── url-encoded {"env":"prod"}
```

It is parsed once per request by `util.ParseExtras` into a `map[string]any`
(`internal/handlers/util/extras.go`); a malformed value is a 400, an absent
value is the empty map. The same `extras` mechanism works for **both**
[`RESTAction`](restactions.md) and [`Widget`](widgets.md) dispatches.

Use it to parametrise a resolve at request time without minting a new CR — e.g.
pass a selected namespace, an environment, or a row id that the RESTAction's jq
(its `path`/`payload`) or a widget template reads.

---

## What it does, in three rules

### 1. It is the *base dict*; API results overwrite it

On a RESTAction resolve the run dict **starts as a deep copy of `extras`**, and
the API stage outputs are then written on top
(`internal/resolvers/restactions/api/resolve.go`, the dict seed):

```go
dict := map[string]any{}
if opts.Extras != nil {
    dict = maps.DeepCopyJSON(opts.Extras)
}
```

So precedence is **API/apiRef result > extras**: an extras key collides with a
stage output → the stage output wins. The widget side reaches the same ordering
from the opposite direction — `ds` already holds the apiRef result, and
`mergeExtras` only fills keys that are *absent*
(`internal/resolvers/widgets/resolve.go`):

```go
for k, v := range extrasCopy {
    if _, present := ds[k]; !present { ds[k] = v }
}
```

The pagination `slice` triple is treated like an API result — it also wins over
`extras` (`injectSlice` runs before `mergeExtras`).

**Identity injection wins over the client.** For a widget declaring
`spec.identityContext`, the server overwrites the declared keys (`username` /
`groups`) in the request extras with the authenticated JWT's own values
*before* the resolve (`DeclaredIdentity` fold at the top of `widgets.Resolve`)
— so a request carrying `extras.username=SOMEONE_ELSE` cannot spoof a declared
widget's identity. Identity-free widgets (no declaration) are untouched.

#### The per-stage `filter` also sees `extras` as a reserved sibling key

The per-stage RESTAction filter (`spec.api[].filter`) is evaluated against the
wrapped envelope `pig`, which carries the stage response under the stage key
plus two **reserved sibling keys** — `slice` (pagination) and `extras` (the
*pure* per-request extras). So a step filter can read the request extras
directly:

```yaml
spec:
  api:
    - name: things
      path: /apis/.../things
      # read extras directly in the step filter:
      filter: '{items: .things.items, tenant: .extras.tenant}'
```

The `extras` the filter sees is the **pure request extras** (`r.opts.Extras`),
not the accumulated run dict — at later stages the dict has stage outputs and a
synthetic `slice` merged in, so it is no longer the request extras. The key is
present only when the request carried a non-empty `extras` (mirrors the `slice`
guard), so a no-extras resolve is byte-identical to before
(`internal/resolvers/restactions/api/handler.go`, `jsonHandlerCore` —
`pig["extras"]` written under the `len(opts.extras) > 0` guard).

**Known asymmetry — `extras`-stage *wins*, `slice`-stage *loses*.** If a stage is
literally named `extras`, the **stage response wins** the sibling-key collision:
`pig["extras"]` is written *before* `pig[<stageKey>]`, so the stage's own
response clobbers the request extras for that stage's filter. This is intentional
(a stage's own output is the more specific datum). It is the **opposite** of the
pre-existing `slice` behaviour, where a stage named `slice` *loses* to the
synthetic pagination `slice` (written *after* the stage key). The two reserved
keys differ here by history, not by design; the asymmetry is documented and
considered acceptable (declaring a stage `extras` or `slice` is degenerate).

### 2. It is input-only

`extras` seeds the resolve; it is **never written back to `status`**. A widget's
`status.widgetData` / `status.resourcesRefs` carry only resolved data, never the
raw `extras` object. Nothing in the resolvers copies `extras` into the emitted
status.

### 3. It meets the cache key differently per lane

`ComputeKey` folds an extras map last, via `canonicaliseExtras`
(`internal/cache/resolved.go`): a **recursively sorted-key JSON** surrogate.
Sorting means two requests with the same content but different map iteration
order hash to the *same* key (a cache hit), while different content hashes to a
different key. On a marshal failure (cyclic / non-JSON value) it falls back to
a deterministic `fmt.Sprintf("%v", …)` so the key still varies with content.
*Which* extras reach that fold depends on the lane:

- **Direct RESTAction `/call`** — the full request `extras` is part of the L1
  key (`handlers/dispatchers/restactions.go`), so two requests that differ
  only in `extras` land on distinct cache entries.

- **Widget `/call`** — the key folds the *effective key extras*
  (`effectiveKeyExtras`, `handlers/dispatchers/helpers.go`), which is the
  union of:
  - the CR-fixed inline maps (`spec.apiRef.extras` ∪
    `spec.resourcesRefsTemplateExtras`, request wins on collision),
  - **only** the request-extras keys the author declared in
    `spec.keyExtras` (`filterDeclaredKeyExtras`) — an absent/empty
    declaration folds **nothing** from the request extras, so one cached
    cell serves every route (the chrome-widget default),
  - the declared identity (`spec.identityContext` → `username`/`groups`
    from the JWT), and the full request identity for an inline-embedding
    parent.

  Undeclared request extras still reach the resolve's jq dict unchanged —
  they affect the *body*, never the *key*. To keep that from polluting the
  shared per-cohort cell, a request carrying extras the widget did **not**
  declare is served its own correct body but the result is **not written to
  the cache** (`requestExtrasFullyDeclared` — the self-quarantine guard). A
  widget whose output genuinely varies on a request extra **must declare that
  key in `spec.keyExtras`**.

  Identity keys (`username`, `groups`, `displayName`) are handled one step
  earlier and never reach the resolve dict unless the widget declares them.
  A client-supplied identity extra is **stripped** inside `widgets.Resolve`
  (`sanitizeUndeclaredIdentityExtras`), so every caller — the `/call` dispatcher,
  the refresher, the boot seed and nested resolves — gets the same contract.
  It survives only when the widget names that key in
  `spec.identityContext` — where the server overwrites it with the JWT's own
  value — or in `spec.keyExtras`, where it partitions the key. So a widget
  whose output depends on the caller's identity **must declare it in
  `spec.identityContext`**; reading `.username` from the request extras
  yields nothing, because a value the client chose would otherwise shape a
  body written into the cell its whole RBAC cohort reads.

  The same filtered union feeds all four key consumers — dispatch lookup,
  the shared `widgetContent` cell, subscription arming, and the boot/keepwarm
  seed — from the single `effectiveKeyExtras` site, so they cannot drift.

> **Exception — resolves that touch an external endpoint are not cached.** If a
> `RESTAction`'s resolve reaches a genuine **external** endpoint (an
> `endpointRef` to a non-apiserver URL), its result is **never** written to L1 —
> external data has no informer/dependency edge that could invalidate a stale
> entry, so snowplow re-fetches it **live on every `/call`**
> (the external-touched sink Put-gate in `handlers/dispatchers/restactions.go`).
> A `${…}`-templated path that resolves at runtime to an apiserver path is
> treated as **internal** (and cached); only genuinely non-apiserver URLs are
> external.

---

## Author-declared (inline) defaults

Besides the per-request `?extras=`, a **widget CR** may declare *static* extras on
its spec, scoped per surface and merged **under** the request `extras` (the
request always wins on collision):

- `spec.apiRef.extras` — scoped to the widget's `apiRef` RESTAction fetch (so it
  also reaches `ds` transitively). Read by `GetApiRefExtras`
  (`internal/resolvers/widgets/widgets.go`).
- `spec.resourcesRefsTemplateExtras` — scoped to the `resourcesRefsTemplate` jq
  **only**. Read by `GetResourcesRefsExtras`.

The dispatcher folds the union of the two inline maps into the L1 keys
(`unionForKey`, `helpers.go`) — inline maps are CR-fixed, so unlike request
extras they are never filtered by `spec.keyExtras` — and the prewarm seed
applies the same union so the first paint is a hit, not a miss. Precedence is
request-wins via `mergeRequestWins` (`widgets/resolve.go`); both inline maps are
input-only and deep-copied (they never alias the shared CR). A widget that
declares neither is **byte-identical** to before.

> The inline fields (like `identityContext` and `keyExtras`) live on the
> **widget CRD**, which ships from the portal chart — not snowplow. Until that
> CRD declares them the accessors read `{}`/nil (a no-op), so the features are
> latent-but-safe on a snowplow-only upgrade.

---

## The full path

### RESTAction (direct `/call`)

```
/call?resource=restactions&extras={…}
   → restactions dispatcher: util.ParseExtras
   → L1 key includes the full request extras (ComputeKey → canonicaliseExtras)
   → restactions.Resolve → api.Resolve: dict := DeepCopyJSON(extras)
   → API stages overwrite dict; spec.Filter projects; status emitted
```

### Widget (`/call`)

```
/call?resource=widgets&extras={…}
   → widgets dispatcher: util.ParseExtras
   → L1 keys fold effectiveKeyExtras (inline ∪ declared keyExtras ∪ declared identity)
   → widgets.Resolve (receives the RAW request extras):
       declared-identity injection (identityContext keys overwritten from the JWT)
       apiRef → apiref.Resolve → restactions.Resolve (extras seeds the RA dict)
       mergeExtras(ds, extras)  ← non-overwriting; covers apiRef-less widgets too
       widgetDataTemplate jq + resourcesRefsTemplate jq evaluate against ds
```

The prewarm / seed / refresher callers never set request `extras`, so a
nil/empty `extras` is a no-op everywhere (the `if opts.Extras != nil` gate and
the `len(extras) == 0` guard skip the copy), and a resolve without `extras` is
byte-identical to the pre-extras behaviour.

---

## See also

- [`widgets.md`](widgets.md) — how `extras` reaches each widget template path,
  and the `identityContext` / `keyExtras` spec fields.
- [`restactions.md`](restactions.md) — the RESTAction `spec.api` resolve `extras`
  seeds.
- [caching deep-dive](../docs/architecture/caching.md) — `ComputeKey` /
  `canonicaliseExtras` in the full key structure.
