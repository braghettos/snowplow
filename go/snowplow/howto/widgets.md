---
type: Usage
title: Understanding the Widget custom resource
description: How snowplow resolves a Widget CR over /call — the spec contract (apiRef, widgetDataTemplate, resourcesRefs, identityContext, keyExtras), the status the frontend consumes, and how extras reach each template path.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [portal, widget, restaction, resolver]
timestamp: 2026-08-06T00:00:00Z
---

# Understanding the `Widget` Custom Resource

**API Group:** `*.widgets.templates.krateo.io`
**Resource:** `widgets`
**Resolver:** `internal/resolvers/widgets/resolve.go`

A `Widget` is the bridge between a frontend-defined layout and live cluster
data. snowplow resolves a widget on demand over `/call` and writes a render-ready
`status` the Krateo frontend reads directly. The widget *canonicalizes* data into
the shape the frontend renders; the data itself comes from a [`RESTAction`](restactions.md)
(via `apiRef`) and/or from static `spec` fields.

> See also: [`extras.md`](extras.md) (per-request context), the
> [request lifecycle](../docs/architecture/request-lifecycle.md) deep-dive, and
> the [caching](../docs/architecture/caching.md) deep-dive (the widget L1
> keys + per-binding-UID keying).

---

## The `spec` contract

A widget's `spec` carries the resolver-facing fields below plus the
frontend-authored `widgetData`. The resolver runs them in a fixed order
(`Resolve`, `resolve.go`):

| `spec` field | Type | What the resolver does |
|---|---|---|
| `apiRef` | object ref | Fetches a `RESTAction` and resolves it into the widget's **data source** (`ds`). `GetApiRef` defaults `resource: restactions` + `apiVersion: templates.krateo.io/v1` (`widgets.go`). May carry an inline `extras` sub-key (see [`extras.md`](extras.md)). |
| `widgetData` | object | The frontend-authored static base of `status.widgetData`. Read by `GetWidgetData`. |
| `widgetDataTemplate` | `[]{forPath, expression}` | jq expressions evaluated against `ds`; each result is written into `status.widgetData` at `forPath`. Type `WidgetDataTemplate` (`apis/templates/v1/widgetdatatemplate.go`). |
| `resourcesRefs.items` | `[]ResourceRef` | Static action references; each is RBAC-checked and emitted with an `allowed` flag. Type `ResourceRef` (`apis/templates/v1/resourcesrefs.go`). A `GET` ref may set `inline: true` to have its resolved child envelope embedded server-side (below). |
| `resourcesRefsTemplate` | `[]{iterator, template}` | jq-templated action references expanded against `ds`, then merged with the static `resourcesRefs`. Type `ResourceRefTemplate` (`apis/templates/v1/resourcesrefstemplate.go`). A sibling `resourcesRefsTemplateExtras` map scopes inline extras to this jq only. |
| `identityContext` | `[]string` | Declares which authenticated-principal keys (`username`, `groups` — the only honored values) the widget's output depends on. The server injects the *declared* subset of the JWT identity into the resolve extras (injection **wins** over any client-supplied value for those keys) and folds the same material into the cache key. Absent = identity-free (the default). `GetIdentityContext` / `DeclaredIdentity` (`widgets.go`). |
| `keyExtras` | `[]string` | Declares which **request**-`extras` keys the widget's output depends on. Only the declared keys partition the L1 cache key; undeclared request extras still reach the jq dict but do not vary the key (see [caching note](#extras--per-request-context)). `GetKeyExtras` (`widgets.go`). |

### `apiRef`

Points at a `RESTAction`; an empty `name` or `namespace` is a no-op that yields
an empty data source (`apiref/resolve.go`, `Resolve`). The referenced RESTAction
is resolved through the restactions resolver under snowplow's own
ServiceAccount rest config (the dispatcher passes its `saRC`; dispatch of the
RESTAction's own API steps then follows that RESTAction's rules, e.g.
`userAccessFilter`). Its output becomes the top-level `ds` the templates below
evaluate against. Pagination (`?page` / `?perPage`) and `?extras` flow
`widget → apiRef → RESTAction` (`resolveApiRef`, `resolve.go`). An apiRef fetch
error preserves the upstream apiserver status code (a 403 stays a 403 on the
wire, not a 500).

A widget needs no `apiRef`: a static widget (only `widgetData` + `resourcesRefs`)
resolves against an empty `ds` (still seeded with `extras` — see below).

### `widgetDataTemplate`

```yaml
spec:
  widgetData:
    title: ""
  widgetDataTemplate:
    - forPath: title
      expression: ${ .getDeployment.metadata.name }   # jq over ds — MUST be wrapped in ${ }
```

Each entry's `expression` is evaluated against `ds` **only when wrapped in
`${ … }`**: `jqutil.MaybeQuery` looks for the `${` marker and, finding none,
returns the string unchanged (`widgetdatatemplate/resolve.go`, plumbing
`jqutil.MaybeQuery`) — an unwrapped value is then stored **verbatim as a literal**,
not evaluated. The (wrapped) result is set into the static `widgetData` at
`forPath`. A *read* error on `widgetDataTemplate` **fails soft** to the
static-only `widgetData` (`resolveWidgetData`, `resolve.go`) — a load-bearing
invariant kept symmetric with the cache routing predicate so a read error can
never land a ServiceAccount-maximal aggregate in the shared identity-free cell.

### `resourcesRefs` and `resourcesRefsTemplate`

Both produce `ResourceRef`s that become `status.resourcesRefs.items`. Static
`resourcesRefs.items` are read verbatim (`GetResourcesRefs`, `widgets.go`);
`resourcesRefsTemplate` entries are jq-expanded against `ds` (optionally over an
`iterator`) and appended (`resolveResourceRefs`, `resolve.go`). Every resolved
ref is then turned into a result with:

- a `path` — a `/call` URL for the action (`resourcesrefs/resolve.go`,
  `buildPath`),
- a `verb`, and
- an **`allowed`** flag from `rbac.UserCan` under the requesting identity
  (`resourcesrefs/resolve.go`, `resolveOne`).

The frontend renders only `allowed == true` actions.

**Inline-rendered children.** A `GET` ref carrying `inline: true` is
additionally resolved server-side under the requesting user's identity, and the
resolved child envelope is embedded into `items[i].rendered`
(`embedInlineChildren`, `handlers/dispatchers/widgets.go`). Opt-in and
default-off: a ref without `inline` is byte-identical to before. A non-GET or
not-`allowed` inline ref is not embedded. An inline-embedding parent is always
cached per-user, never in the shared content cell.

**Example — `resourcesRefsTemplate` reading `extras`.** Only the *templated*
variant is jq-expanded against `ds`, so only it can reference `extras`; static
`resourcesRefs.items` are read verbatim and `extras`/jq never apply to them. Here
the `iterator` fans out over an `extras` array, and each element parametrises one
ref (jq via `${…}`):

```yaml
spec:
  resourcesRefsTemplate:
    - iterator: ${ .names }          # extras.names, e.g. ["kube-system","default"]
      template:
        id: ${ "ns-" + . }
        apiVersion: v1
        resource: namespaces
        namespace: ${ . }            # each array element becomes one ref's namespace
        verb: GET
```

Called as `/call?resource=widgets&…&extras={"names":["kube-system","default"]}`
(URL-encoded), this yields one `status.resourcesRefs.items` entry per name. Use
`resourcesRefsTemplate` whenever a ref must depend on `extras`; a static
`resourcesRefs` ref cannot.

---

## The `status` the frontend consumes

`Resolve` writes these `status` keys (`resolve.go`; `traceId` is stamped by the
dispatcher):

| `status` key | Shape | Source |
|---|---|---|
| `status.widgetData` | object | static `widgetData` + `widgetDataTemplate` results |
| `status.resourcesRefs.items[]` | `[]{id, path, verb, payload?, allowed, inline?, rendered?}` | `resourcesRefs` + `resourcesRefsTemplate`; type `ResourceRefResult` (`resourcesrefs.go`) |
| `status.resourcesRefs.slice` | `{perPage, page, continue}` | added only when the request is paginated |
| `status.error` | string | set on any resolve failure |
| `status.traceId` | string | the request's shortid trace id, for frontend log correlation |

The final `status` is validated against the widget's own CRD schema before it is
returned; a schema failure is a 400 `StatusError` (`Resolve`, `resolve.go`).

---

## `extras` — per-request context

`?extras={…}` (URL-encoded JSON) is per-request context usable by **every** path
in a widget resolve (full story in [`extras.md`](extras.md)):

- the `apiRef` RESTAction's own jq (its `path` / `payload`) can reference
  `extras` keys — extras seed the RESTAction resolve dict (the
  `DeepCopyJSON(opts.Extras)` dict seed in `restactions/api/resolve.go`);
- `widgetDataTemplate` **and** `resourcesRefsTemplate` jq can reference `extras`
  keys — `mergeExtras` folds them into `ds` (`resolve.go`);
- **apiRef-less** widgets get `extras` too — `mergeExtras` is the only thing that
  puts them into `ds` when there is no apiRef result;
- for a widget declaring `spec.identityContext`, the server **overwrites** the
  declared keys (`username` / `groups`) in `extras` with the authenticated
  JWT's own values before the resolve — a client cannot spoof another user's
  identity through `extras`.

`extras` is **input-only** (never echoed to `status`) and **non-overwriting**:
any apiRef-result key, or the pagination `slice` triple, wins on a key collision
(`mergeExtras`, `resolve.go`).

**Caching:** for the widget L1 keys, only the request-`extras` keys declared in
`spec.keyExtras` (plus the CR-fixed inline extras and any declared identity)
are folded into the cache key — an undeclared request extra does not create a
new cache cell, and a request carrying undeclared non-identity extras is served
correctly but its result is *not* written to the shared cache
(`effectiveKeyExtras` / `requestExtrasFullyDeclared`,
`internal/handlers/dispatchers/helpers.go`). A widget whose output genuinely
varies on an extras key **must declare it in `spec.keyExtras`**, or a shared
cached body may be served across requests with different extras. A
client-supplied identity extra (`username`, `groups`, `displayName`) is
stripped before the resolve unless the widget declares it in
`spec.identityContext` or `spec.keyExtras`, so identity-dependent output
**must declare `spec.identityContext`**. See
[`extras.md`](extras.md) and the [caching](../docs/architecture/caching.md)
deep-dive.

---

## Resolve order (summary)

declared-identity injection → `apiRef` → `injectSlice` → `mergeExtras` →
`widgetDataTemplate` → fold `resourcesRefsTemplateExtras` →
`resourcesRefs(+Template)` → CRD-schema validate (`Resolve`, `resolve.go`).
Each phase is wall-clocked for the seed-path timing log.

Refer to the [Krateo Widgets documentation](https://github.com/krateo-platformops/frontend/blob/main/docs/docs.md)
for the frontend-side widget catalogue.
