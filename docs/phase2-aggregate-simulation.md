# Phase 2 Aggregate Simulation

Generated 2026-04-11. Based on full analysis of all 35 portal chart templates.

## 1. Widget Dependency Tree

```
NavMenu: sidebar-nav-menu
  apiRef: sidebar-nav-menu-items (RESTAction)
  children (dynamic via resourcesRefsTemplate):
    NavMenuItem: nav-menu-item-dashboard
      child: Page: dashboard-page
    NavMenuItem: nav-menu-item-blueprints
      child: Page: blueprints-page
    NavMenuItem: nav-menu-item-compositions
      child: Page: compositions-page

RoutesLoader: routes-loader
  apiRef: all-routes (RESTAction)
  children (dynamic): Route objects

Page: dashboard-page
  children:
    Panel: dashboard-blueprints-panel
      child: Row: dashboard-blueprints-panel-row
        children:
          PieChart: dashboard-blueprints-panel-row-piechart    ** apiRef: blueprints-list **
          Table: dashboard-blueprints-panel-row-table          ** apiRef: blueprints-list **
    Panel: dashboard-compositions-panel
      child: Row: dashboard-compositions-panel-row
        children:
          PieChart: dashboard-compositions-panel-row-piechart  ** apiRef: compositions-list **
          Table: dashboard-compositions-panel-row-table        ** apiRef: compositions-list **

Page: compositions-page
  children:
    Button: compositions-page-button-drawer-filters
      child: Filters: compositions-page-button-drawer-filters
    DataGrid: compositions-page-datagrid                       ** apiRef: compositions-panels **
      children (dynamic): Panel objects with label krateo.io/portal-page=compositions

Page: blueprints-page
  children:
    Button: blueprints-page-button-drawer-filters
      child: Filters: blueprints-page-button-drawer-filters
    DataGrid: blueprints-page-datagrid                         ** apiRef: blueprints-panels **
      children (dynamic): Panel objects with label krateo.io/portal-page=blueprints
```

## 2. Complete Widget Inventory

### Widgets WITHOUT apiRef (pure layout, no aggregate needed)

| Widget | Kind | Reason no aggregate needed |
|--------|------|---------------------------|
| dashboard-page | Page | Static layout: references 2 panels via resourcesRefs |
| compositions-page | Page | Static layout: references button + datagrid |
| blueprints-page | Page | Static layout: references button + datagrid |
| dashboard-compositions-panel | Panel | Static layout: title + row ref |
| dashboard-blueprints-panel | Panel | Static layout: title + row ref |
| dashboard-compositions-panel-row | Row | Static layout: references piechart + table |
| dashboard-blueprints-panel-row | Row | Static layout: references piechart + table |
| nav-menu-item-dashboard | NavMenuItem | Static: label, icon, path |
| nav-menu-item-compositions | NavMenuItem | Static: label, icon, path |
| nav-menu-item-blueprints | NavMenuItem | Static: label, icon, path |
| compositions-page-button-drawer-filters | Button | Static: opens drawer |
| blueprints-page-button-drawer-filters | Button | Static: opens drawer |
| compositions-page-button-drawer-filters | Filters | Static: field definitions |
| blueprints-page-button-drawer-filters | Filters | Static: field definitions |
| demo-system-composition-route | Route | Static: path mapping |
| demo-system-compositions-route | Route | Static: path mapping |
| namespace.demo-system | Namespace | Infrastructure |

### Widgets WITH apiRef (resolve data, aggregate candidates)

| Widget | Kind | apiRef (RESTAction) | GVR(s) read | JQ summary | Aggregate type |
|--------|------|---------------------|-------------|------------|---------------|
| dashboard-compositions-panel-row-piechart | PieChart | compositions-list | composition.krateo.io/v1-2-2/* (all ns) | Count items by Ready condition: True/False/Unknown + total | **countBy** |
| dashboard-compositions-panel-row-table | Table | compositions-list | composition.krateo.io/v1-2-2/* (all ns) | Map all items to rows (name,ns,status,date,kind,av), sort by date desc | **topN** |
| dashboard-blueprints-panel-row-piechart | PieChart | blueprints-list | core.krateo.io/v1alpha1/compositiondefinitions (all ns) | Count items by Ready condition: True/False/Unknown + total | **countBy** |
| dashboard-blueprints-panel-row-table | Table | blueprints-list | core.krateo.io/v1alpha1/compositiondefinitions (all ns) | Map all items to rows, sort by date desc | **countBy** (tiny set, low priority) |
| sidebar-nav-menu | NavMenu | sidebar-nav-menu-items | widgets.templates.krateo.io/v1beta1/navmenuitems (all ns) | Collect navmenuitems, sort by order | **none** (small set, layout-only) |
| routes-loader | RoutesLoader | all-routes | widgets.templates.krateo.io/v1beta1/routes (all ns) | Collect routes | **none** (small set, layout-only) |
| compositions-page-datagrid | DataGrid | compositions-panels | widgets.templates.krateo.io/v1beta1/panels (all ns, label-filtered) | Collect panels with label compositions, paginate | **none** (small set, layout-only) |
| blueprints-page-datagrid | DataGrid | blueprints-panels | widgets.templates.krateo.io/v1beta1/panels (all ns, label-filtered) | Collect panels with label blueprints, paginate | **none** (small set, layout-only) |
| blueprints-row | Row | blueprints-panels | widgets.templates.krateo.io/v1beta1/panels (all ns, label-filtered) | Collect panels with label blueprints, build resourcesRefs | **none** (small set, layout-only) |

## 3. RESTAction Detail

### compositions-list (THE bottleneck)

**API calls chain:**
1. Calls `compositions-get-ns-and-crd` (self-referencing snowplow /call)
   - Which calls: `/apis/apiextensions.k8s.io/v1/customresourcedefinitions` (CRDs)
   - And: `/api/v1/namespaces` (all namespaces)
   - Produces: cross-product of {plural, version} x {namespace}
2. For each {plural, version, namespace} tuple: calls `/apis/composition.krateo.io/{version}/namespaces/{ns}/{plural}`
3. Final filter: flattens all items into `{uid, name, ns, ts, kind, av, conditions}`

**At 48998 compositions across 500 namespaces:** This is 500+ API calls (1 per ns) producing 48998 items, then JQ iterates all 48998 items. This is the O(N) bottleneck.

### blueprints-list

**API calls chain:**
1. `/api/v1/namespaces` -> list of ns names
2. For each ns: `/apis/core.krateo.io/v1alpha1/namespaces/{ns}/compositiondefinitions`
3. Final filter: flattens into `.list`

**At 1 blueprint:** Negligible cost. The ns fan-out is ~500 calls but each returns empty except krateo-system.

### compositions-panels, blueprints-panels, sidebar-nav-menu-items, all-routes

All follow the same pattern: list namespaces, then fan-out to list widgets per namespace. All return small sets (a handful of panel/navmenuitem/route objects). No aggregate needed.

## 4. Aggregate Benefit Analysis

### HIGH value (P0)

| Widget | Aggregate type | Current cost at 49K | Aggregate cost | Benefit |
|--------|---------------|--------------------:|---------------:|---------|
| compositions-piechart | countBy(conditions.Ready) | O(49K) JQ iteration per request per user | O(1) read, O(1) write per event | Eliminates ~5s JQ + ~2.5s Redis fetch |
| compositions-table | topN(creationTimestamp, 50) | O(49K) JQ map+sort per request per user | O(1) ZRANGEBYLEX read, O(log N) write per event | Eliminates ~5s JQ + ~2.5s Redis fetch |

### LOW value (not worth implementing now)

| Widget | Why low value |
|--------|--------------|
| blueprints-piechart | Only 1 item. JQ cost: <1ms. No benefit from aggregate. |
| blueprints-table | Only 1 item. Same. |
| sidebar-nav-menu | ~3 items. Negligible. |
| routes-loader | ~2 items. Negligible. |
| compositions-page-datagrid | Returns panels (widget objects), not compositions. Small set. |
| blueprints-page-datagrid | Returns panels (widget objects). Small set. |
| blueprints-row | Returns panels. Small set. |

## 5. Admin vs Cyberjoker Simulation

### Live data assumptions

**Compositions** (GVR: `composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages`):
- Total: 48998 items across 500 bench-ns-XX namespaces
- Ready=True: ~2450 (5%)
- Ready=False: ~25 (0.05%)
- Unknown (no Ready condition): ~46523 (92% + remainder)
- demo-system: 0 compositions
- Most recent: bench-app-32-985 at 2026-04-11T01:08:28Z

**Blueprints** (GVR: `core.krateo.io/v1alpha1/compositiondefinitions`):
- 1 item: "github-scaffolding-with-composition-page" in krateo-system
- Ready=True

**cyberjoker RBAC** (after bench-composition-admin-binding deletion):
- demo-system: list/get compositions, widgets, restactions, configmaps, compositiondefinitions
- krateo-system: get restactions, list widgets/templates
- Cluster-wide: list namespaces, list/get CRDs
- NO access to bench-ns-XX namespaces
- NO cluster-wide list on compositions

---

### 5.1 compositions-piechart (countBy aggregate)

**Aggregate key:** `snowplow:agg:countBy:composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages:conditions.Ready`

**Stored aggregate (global, pre-RBAC):**
```json
{
  "True": 2450,
  "False": 25,
  "Unknown": 46523,
  "total": 48998,
  "byNamespace": {
    "bench-ns-0": {"True": 5, "False": 0, "Unknown": 93, "total": 98},
    "bench-ns-1": {"True": 5, "False": 0, "Unknown": 93, "total": 98},
    "...": "...(500 namespaces)...",
    "demo-system": {"True": 0, "False": 0, "Unknown": 0, "total": 0}
  }
}
```

**RBAC filtering:** Snowplow must filter the aggregate by the namespaces the user can LIST compositions in.

#### admin (cluster-admin, sees all namespaces)

`status.widgetData` output:
```json
{
  "description": "Compositions",
  "series": {
    "data": [
      {"color": "green",  "value": 2450,  "label": "Ready:True"},
      {"color": "orange", "value": 25,    "label": "Ready:False"},
      {"color": "gray",   "value": 46523, "label": "Unknown"}
    ],
    "total": 48998
  },
  "title": "48998"
}
```

#### cyberjoker (demo-system only for compositions)

cyberjoker can LIST compositions only in demo-system. demo-system has 0 compositions.

`status.widgetData` output:
```json
{
  "description": "Compositions",
  "series": {
    "data": [
      {"color": "green",  "value": 0, "label": "Ready:True"},
      {"color": "orange", "value": 0, "label": "Ready:False"},
      {"color": "gray",   "value": 0, "label": "Unknown"}
    ],
    "total": 0
  },
  "title": "0"
}
```

---

### 5.2 compositions-table (topN aggregate)

**Aggregate key:** `snowplow:agg:topN:composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages:creationTimestamp`

**Stored aggregate:** Redis ZSET with score = Unix timestamp of creationTimestamp.
Members are compact JSON: `{uid, name, ns, ts, kind, av, conditions}`.

At write time: ZADD on each add/update event, ZREM on delete. O(log N) per event.
At read time: ZREVRANGEBYSCORE with RBAC-allowed namespace filter, LIMIT 0 50. O(log N + 50).

**RBAC filtering:** The ZSET stores namespace in each member. At read time, snowplow filters to namespaces the user can LIST.

#### admin (sees all 48998, top 50 by date desc)

`status.widgetData` output (showing first 3 of 50 rows):
```json
{
  "columns": [
    {"title": "Name", "valueKey": "name"},
    {"title": "Namespace", "valueKey": "namespace"},
    {"title": "Status", "valueKey": "status"},
    {"title": "Date", "valueKey": "date"},
    {"title": "Kind", "valueKey": "kind"},
    {"title": "ApiVersion", "valueKey": "apiversion"}
  ],
  "data": [
    [
      {"valueKey": "key",        "kind": "jsonSchemaType", "type": "string", "stringValue": "uid-bench-app-32-985"},
      {"valueKey": "name",       "kind": "jsonSchemaType", "type": "string", "stringValue": "bench-app-32-985"},
      {"valueKey": "namespace",  "kind": "jsonSchemaType", "type": "string", "stringValue": "bench-ns-32"},
      {"valueKey": "date",       "kind": "jsonSchemaType", "type": "string", "stringValue": "2026-04-11T01:08:28Z"},
      {"valueKey": "status",     "kind": "jsonSchemaType", "type": "string", "stringValue": "Ready: Unknown"},
      {"valueKey": "kind",       "kind": "jsonSchemaType", "type": "string", "stringValue": "GithubScaffoldingWithCompositionPage"},
      {"valueKey": "apiversion", "kind": "jsonSchemaType", "type": "string", "stringValue": "composition.krateo.io/v1-2-2"}
    ],
    [
      {"valueKey": "key",        "kind": "jsonSchemaType", "type": "string", "stringValue": "uid-bench-app-32-984"},
      {"valueKey": "name",       "kind": "jsonSchemaType", "type": "string", "stringValue": "bench-app-32-984"},
      {"valueKey": "namespace",  "kind": "jsonSchemaType", "type": "string", "stringValue": "bench-ns-32"},
      {"valueKey": "date",       "kind": "jsonSchemaType", "type": "string", "stringValue": "2026-04-11T01:08:27Z"},
      {"valueKey": "status",     "kind": "jsonSchemaType", "type": "string", "stringValue": "Ready: Unknown"},
      {"valueKey": "kind",       "kind": "jsonSchemaType", "type": "string", "stringValue": "GithubScaffoldingWithCompositionPage"},
      {"valueKey": "apiversion", "kind": "jsonSchemaType", "type": "string", "stringValue": "composition.krateo.io/v1-2-2"}
    ],
    "... (48 more rows, descending by date)"
  ],
  "pageSize": 10
}
```

#### cyberjoker (demo-system only, 0 compositions)

`status.widgetData` output:
```json
{
  "columns": [
    {"title": "Name", "valueKey": "name"},
    {"title": "Namespace", "valueKey": "namespace"},
    {"title": "Status", "valueKey": "status"},
    {"title": "Date", "valueKey": "date"},
    {"title": "Kind", "valueKey": "kind"},
    {"title": "ApiVersion", "valueKey": "apiversion"}
  ],
  "data": [],
  "pageSize": 10
}
```

---

### 5.3 blueprints-piechart (NO aggregate needed)

Only 1 blueprint exists. Both users see the same result (both have access to krateo-system compositiondefinitions -- admin via cluster-admin, cyberjoker via demo-system role, but the blueprint is in krateo-system).

**Key RBAC observation:** cyberjoker has `compositiondefinitions` access only in demo-system (per RBAC template lines 139-148). The single blueprint is in krateo-system. cyberjoker does NOT have a role granting compositiondefinitions access in krateo-system.

However, `blueprints-list` RESTAction lists namespaces then fans out. cyberjoker CAN list namespaces (cluster-wide) and CAN list compositiondefinitions in demo-system. But the single blueprint is in krateo-system, and cyberjoker has NO compositiondefinitions access in krateo-system (only `widgets.templates.krateo.io` get/list and `templates.krateo.io/restactions` get).

#### admin

```json
{
  "description": "Blueprints",
  "series": {
    "data": [
      {"color": "green",  "value": 1, "label": "Ready:True"},
      {"color": "orange", "value": 0, "label": "Ready:False"},
      {"color": "gray",   "value": 0, "label": "Unknown"}
    ],
    "total": 1
  },
  "title": "1"
}
```

#### cyberjoker

The fan-out to krateo-system/compositiondefinitions returns 403 (no RBAC). demo-system has 0 compositiondefinitions. Result:

```json
{
  "description": "Blueprints",
  "series": {
    "data": [
      {"color": "green",  "value": 0, "label": "Ready:True"},
      {"color": "orange", "value": 0, "label": "Ready:False"},
      {"color": "gray",   "value": 0, "label": "Unknown"}
    ],
    "total": 0
  },
  "title": "0"
}
```

**No aggregate needed:** 1 item total, JQ cost < 1ms.

---

### 5.4 blueprints-table (NO aggregate needed)

Same RBAC analysis as blueprints-piechart.

#### admin

```json
{
  "data": [
    [
      {"valueKey": "key",        "stringValue": "uid-github-scaffolding"},
      {"valueKey": "name",       "stringValue": "github-scaffolding-with-composition-page"},
      {"valueKey": "namespace",  "stringValue": "krateo-system"},
      {"valueKey": "date",       "stringValue": "2026-04-09T..."},
      {"valueKey": "status",     "stringValue": "Ready: True"},
      {"valueKey": "kind",       "stringValue": "CompositionDefinition"},
      {"valueKey": "apiversion", "stringValue": "core.krateo.io/v1alpha1"}
    ]
  ],
  "pageSize": 10
}
```

#### cyberjoker

```json
{
  "data": [],
  "pageSize": 10
}
```

**No aggregate needed:** 1 item total.

---

### 5.5 Summary: sidebar-nav-menu, routes-loader, datagrid widgets (NO aggregate)

These all resolve widget-type GVRs (navmenuitems, routes, panels) which are tiny sets (3-5 items). JQ cost is negligible. No aggregate needed.

Both admin and cyberjoker see the same nav items and routes (cyberjoker has widget access in krateo-system and demo-system).

---

## 6. Summary Table

| Widget | Aggregate type | Benefit | admin output (key values) | cyberjoker output (key values) |
|--------|---------------|---------|--------------------------|-------------------------------|
| compositions-piechart | **countBy** | **Eliminates 7.5s O(N) per request** | total=48998, True=2450, False=25, Unknown=46523 | total=0 (no ns access) |
| compositions-table | **topN(50)** | **Eliminates 7.5s O(N) per request** | 50 most recent items, newest=bench-app-32-985 | empty (no ns access) |
| blueprints-piechart | none | <1ms, 1 item | total=1, True=1 | total=0 (no krateo-system access) |
| blueprints-table | none | <1ms, 1 item | 1 row | empty |
| sidebar-nav-menu | none | <1ms, ~3 items | 3 nav items | 3 nav items (has widget access) |
| routes-loader | none | <1ms, ~2 items | 2 routes | 2 routes (has widget access) |
| compositions-page-datagrid | none | <1ms, ~few panels | panel list | panel list |
| blueprints-page-datagrid | none | <1ms, ~few panels | panel list | panel list |
| blueprints-row | none | <1ms, ~few panels | panel list | panel list |

## 7. Implementation Priority

**Only 2 aggregates are worth implementing:**

1. **compositions-piechart countBy** (Phase 2.1, task #23)
   - Aggregate shape: `{byNamespace: {ns: {True: N, False: N, Unknown: N, total: N}}}`
   - Read path: sum counters for RBAC-allowed namespaces, O(allowed_ns) per read
   - Write path: on L3 add/update/delete, increment/decrement the ns bucket, O(1)
   - Eliminates: 48998-item JQ iteration + 2.5s MGET

2. **compositions-table topN** (Phase 2.2, task #24)
   - Aggregate shape: Redis ZSET keyed by creation timestamp, member = compact JSON with ns field
   - Read path: ZREVRANGE, post-filter by allowed namespaces, take 50. O(log N + scan)
   - Write path: ZADD on add, ZREM on delete, O(log N)
   - Eliminates: 48998-item JQ map+sort + 2.5s MGET

**Everything else is not worth aggregating** because the underlying data sets have fewer than 10 items.

## 8. RBAC Design Constraint

Both aggregates MUST support per-user RBAC filtering. The key insight:

- The aggregate stores data **per namespace** (byNamespace map for countBy, ns field in ZSET members for topN)
- At read time, snowplow already knows which namespaces the user can LIST the GVR in (from the RBAC pre-warming cache)
- The read path filters the aggregate to only those namespaces

This means:
- admin (cluster-admin): reads ALL namespace buckets -> full totals
- cyberjoker (demo-system only for compositions): reads only demo-system bucket -> 0 for everything
- A future user with access to bench-ns-0 through bench-ns-9: would see ~980 compositions (10 namespaces x ~98 each)

No per-user aggregate storage needed. One global aggregate + RBAC filter at read time.
