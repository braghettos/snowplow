// resolved.go — Tag 0.30.7 binding: in-process L1 resolved-output cache
// (bounded LRU + byte-budget + time-to-live only).
//
// Per implementation plan §"Tag 0.30.7 — What's implemented":
//
//   - Bounded LRU over `(restaction_path|widget_path, user_identity,
//     query_hash)`. Entry count cap (default 100 000) AND byte-budget
//     cap (default 2 GB). Eviction is single least-recently-used — no
//     complex sweep machinery (Q-L1-BUDGET / audit guidance).
//   - Invalidation in this sub-ship: time-to-live only. DELETE-driven
//     invalidation lands at 0.30.8 per feedback_l1_invalidation_delete_only.md.
//
// Layering rule (project_redis_removal.md): the cache subsystem stays
// removable via CACHE_ENABLED. When `Disabled()` is true the resolver
// cache is never instantiated; dispatchers take the exact 0.30.6 path.
// Even with CACHE_ENABLED=true, RESOLVED_CACHE_ENABLED=false bypasses
// the L1 layer while keeping the rest of cache=on alive (typed-RBAC
// indexer, informer factory, EvaluateRBAC gate).
//
// Sub-ship A (0.30.7) does NOT add:
//   - DELETE-driven eviction (0.30.8).
//   - Dependency tracking (0.30.8).
//   - Refresher (0.30.8).
//   - Per-class queueing (0.30.11).
// Per the plan, none of these are sneaked in here.

package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Resolver-cache env knobs (defaults match chart-0.30.7 spec).
const (
	envResolvedCacheEnabled      = "RESOLVED_CACHE_ENABLED"
	envResolvedCacheMaxEntries   = "RESOLVED_CACHE_MAX_ENTRIES"
	envResolvedCacheMaxBytes     = "RESOLVED_CACHE_MAX_BYTES"
	envResolvedCacheTTLSeconds   = "RESOLVED_CACHE_TTL_SECONDS"
	envResolvedCacheSummaryEvery = "RESOLVED_CACHE_SUMMARY_EVERY_SECONDS"

	// envCatalogUnservableTTLSeconds is the R1 Layer 2 (#36) bounded
	// staleness backstop. When > 0, an apistage entry stored while its
	// underlying GVR informer is NOT servable (registered-but-not-synced /
	// watch-broken / unconfirmed) gets this SHORT per-entry TTL instead of
	// the standard RESOLVED_CACHE_TTL_SECONDS, so a degraded / not-fully-
	// resolved catalog entry self-corrects within the bound EVEN IF a
	// dirty-mark / refresh-ordering gap is ever missed. This is a
	// bounded-staleness floor, NOT a flag-park: the dirty-mark + refresher
	// remain the primary invalidation; this is the safety net that caps the
	// worst case. Default 0 = DISABLED (the per-entry override is purely
	// additive; with it unset, entries use the standard TTL exactly as
	// before). Operators set it via the chart value.
	envCatalogUnservableTTLSeconds = "CATALOG_UNSERVABLE_TTL_SECONDS"

	// envUAFResolvedTTLSeconds — #118 (d) INTERIM stopgap for the
	// userAccessFilter RBAC stale-read (design docs/118-uaf-rbac-stale-read-
	// design-2026-07-22.md). A resolved cell whose RESTAction declares a
	// userAccessFilter stage is stamped this short TTLOverride so its
	// staleness window after an out-of-band RBAC change is CAPPED at this
	// value — even for a hot, data-plane-refreshed cell (the override is
	// re-stamped on the refresher re-Put too, else the CreatedAt-slide would
	// defeat it: #118 C-118-6). This does NOT fix the cache key (a within-TTL
	// RBAC change is still served stale — that is #118 (c)'s job); it only
	// bounds the exposure window. Default 0 = DISABLED (purely additive, the
	// per-entry override is unset → UAF cells use the standard TTL exactly as
	// today). Operators set it via the chart value; cleanly removable per
	// project_caching_is_provisional.
	envUAFResolvedTTLSeconds = "UAF_RESOLVED_TTL_SECONDS"

	// envResolvedCacheMaxResidentBytes is the Ship 4a (0.30.198) byte
	// budget for the PINNED resident region — the eviction-protected cells
	// (expensive prewarmed RAFullList full-list caches). Resident bytes are
	// accounted SEPARATELY from the transient LRU byte budget
	// (RESOLVED_CACHE_MAX_BYTES) and are SKIPPED by the LRU sweep.
	//
	// This is the Ship 4a memory KILL-SWITCH (per the coordinator's
	// single-flag directive — NO new boolean feature flag): it is a TUNABLE
	// byte cap, consistent with RESOLVED_CACHE_MAX_BYTES. Setting it to 0
	// DISABLES pinning — every cell that would otherwise be pinned is stored
	// TRANSIENT instead (LRU-evictable), so the resident region exerts zero
	// memory pressure. A positive value is a hard ceiling: a Put that would
	// push resident bytes past it is stored TRANSIENT (un-pinned) rather
	// than evicting another pinned cell, so a runaway pin set degrades to
	// LRU rather than OOMing the pod.
	//
	// 4a itself is gated entirely under CACHE_ENABLED (ResolvedCacheEnabled);
	// there is NO 4a-specific boolean. Removability comes from the layer's
	// clean code separation (the raFullList class + apiRef Get/Put + this
	// resident region are wholesale-deletable per project_caching_is_provisional),
	// not from a runtime toggle.
	envResolvedCacheMaxResidentBytes = "RESOLVED_CACHE_MAX_RESIDENT_BYTES"

	// envWidgetContentL1Enabled is the Ship G (0.30.16x) opt-in gate for
	// the identity-free widget content L1 layer. Default ON — the layer
	// is the actual zero-cold ship per Diego's 2026-05-21 framing; flag-
	// off it bypasses the upper layer and the dispatcher takes the
	// pre-Ship-G per-user widget L1 path. It is gated UNDER
	// ResolvedCacheEnabled() (the widget content L1 reuses the resolved
	// store + refresher).
	envWidgetContentL1Enabled = "WIDGET_CONTENT_L1_ENABLED"

	defaultResolvedCacheMaxEntries          = 100_000
	defaultResolvedCacheMaxBytes            = int64(2) * 1024 * 1024 * 1024 // 2 GiB
	defaultResolvedCacheTTLSeconds          = 3600
	defaultResolvedCacheSummaryEverySeconds = 300 // 5 min aggregate INFO line

	// defaultResolvedCacheMaxResidentBytes — Ship 4a (0.30.198). The
	// resident region holds the EXPENSIVE prewarmed RAFullList cells
	// (e.g. admin's full compositions-panels list — ~18 MiB at 49K panels,
	// per feedback_zero_cold_navigations_hard_requirement). The expensive
	// cohorts are FEW + stable (broad-RBAC admin-class cohorts), so the
	// resident set is O(few-cohorts × ~tens-of-MiB). 512 MiB holds ~28 such
	// 18 MiB cells with headroom; it is INSIDE the 24 GiB pod / GOMEMLIMIT
	// envelope and is a SEPARATE budget from the 2 GiB transient LRU. The
	// value is the design-time floor; the tester re-derives it empirically
	// from the per-cohort envelope cost × expensive-cohort count under the
	// real cluster (feedback_capacity_caps_empirical_per_entry_cost) before
	// the default is finalised.
	defaultResolvedCacheMaxResidentBytes = int64(512) * 1024 * 1024 // 512 MiB
)

// CacheEntryClassApistage is the ResolvedKeyInputs.CacheEntryClass
// discriminant for a per-api-stage L1 entry (Ship E, 0.30.116). The
// resolved-output store, the dep-tracker, the LRU/TTL machinery, and
// ComputeKey are all reused verbatim — "apistage" is just a third
// granularity of L1 key, not a new cache. The refresher's resolve-once
// seam branches on it to re-run a single stage rather than a whole
// RESTAction.
//
// NOTE the STRING VALUE is unchanged ("apistage"): it is hashed into the
// cache key (ComputeKey) and is the refresher registry key — rotating it
// would invalidate every in-flight entry. The 0.30.118 rename touches the
// Go const IDENTIFIER only.
const CacheEntryClassApistage = "apistage"

// CacheEntryClassWidgetContent is the ResolvedKeyInputs.CacheEntryClass
// discriminant for Ship G (0.30.16x) — the identity-free widget content
// L1 layer. Sibling to CacheEntryClassApistage, one tier UP: caches the
// resolved widget envelope (not the per-K8s-call envelope) keyed on
// (gvr, ns, name, perPage, page, extras) — Username + Groups OMITTED.
//
// The resolved widget envelope is identity-invariant EXCEPT for the
// per-item `status.resourcesRefs.items[].allowed` boolean — set by
// rbac.UserCan under whichever identity resolved it. The walker
// populates this layer under the SA identity (so the stored flags are
// SA-evaluated, typically all-true for navigation widgets); the
// serve-time gate (gateWidgetEnvelope, dispatchers/widgets.go) OVERWRITES
// every `allowed` flag per-request via rbac.UserCan under the request
// identity before serialisation. The cached body is the SHELL; the body
// that leaves the pod is per-user — same architectural property F1's
// apistage class introduced, applied one tier up.
//
// The string VALUE "widgetContent" is load-bearing: it is hashed into
// the cache key (ComputeKey) AND used as the refresher registry key.
// Rotating it would invalidate every in-flight entry.
const CacheEntryClassWidgetContent = "widgetContent"

// CacheEntryClassRAFullList is the ResolvedKeyInputs.CacheEntryClass
// discriminant for Ship 4a (0.30.198) — the page-INDEPENDENT RESTAction
// full-result-list cache. Sibling to apistage / widgetContent. It caches
// the RA's OWN resolved Status map (apiref/resolve.go's ra.Status) resolved
// UNPAGINATED (PerPage=0/Page=0 → no `.slice` injected → the RA's output jq
// `.slice.perPage // ($sorted|length)` returns the FULL sorted set).
//
// The cell is keyed by the RA's identity (its gvr/ns/name), the per-layer
// binding (BindingUID — identity-bound, same as restactions/widgets), and
// the NON-slice Extras, with PerPage/Page FORCED to 0 (page-independent).
// Every paginated /call that matches on non-slice inputs and differs ONLY
// in slice (page/perPage) SHARES this one cell; the per-/call page is then
// applied as a cheap Go-slice at serve time (see ra_full_list_slice.go).
// Widgets that feed the same RA under the same per-layer binding share the
// SAME cell — the chokepoint dedupe across widgets.
//
// IDENTITY-BOUND (NOT identity-free): RA output is RBAC-narrowed (the
// userAccessFilter `namespaces` stage), so two binding-classes can see
// different rows. ComputeKey therefore folds BindingUID for this class
// exactly as it does for restactions/widgets. The per-request RBAC gate
// (UAF narrowing) is UNTOUCHED — this cell sits ABOVE it, keyed
// per-binding, never cross-binding (design §3.3, raFullList row).
//
// The string VALUE "raFullList" is load-bearing: it is hashed into the
// cache key (ComputeKey) AND used as the refresher registry key. Rotating
// it would invalidate every in-flight entry.
const CacheEntryClassRAFullList = "raFullList"

// ResolvedEntry is the L1 cache value. The pre-encoded JSON bytes are
// what we hand back on a hit; storing the encoded form (rather than the
// runtime *RESTAction / *Widget object) avoids racey shared-state on
// the hit path — readers get an immutable []byte slice.
//
// Sub-ship B (0.30.8) populates the Inputs field so the refresher can
// re-invoke the resolver on UPDATE/PATCH events. RawJSON + CreatedAt
// remain unchanged from sub-ship A.
type ResolvedEntry struct {
	RawJSON   []byte    // pre-encoded resolver output, ready to write
	CreatedAt time.Time // for TTL eviction

	// SeededAtBoot marks an entry written by the Phase-1 boot seed
	// (seedOneWidget / seedOneRestaction Put), as opposed to a real
	// user-/call resolve. #130 F3 seed-attribution observable: the
	// resolved_cache.lookup hit path reads this to tag hit_source
	// "seed" vs "traffic" and increment hits_seed_attributable, so
	// "did the boot seed warm this cell" is answerable without forensics.
	// Boolean provenance ONLY — carries no per-user data, so it is
	// leak-safe. A refresher/traffic re-Put of the same key overwrites the
	// entry with SeededAtBoot=false (the natural zero value), correctly
	// re-classifying the cell as traffic-warmed once real traffic replaces
	// the seed. Purely additive: legacy entries default false = "traffic".
	SeededAtBoot bool

	// Inputs is the canonical key-input bundle the entry was resolved
	// from. The refresher uses it to drive a re-resolve when an
	// UPDATE/PATCH event fires for any of this entry's dep tuples.
	// Nil-safe: a missing Inputs (e.g., legacy 0.30.7 entries during a
	// rolling restart) skips refresh but still serves TTL+LRU correctly.
	Inputs *ResolvedKeyInputs

	// Items / ItemsAPIVersion / ItemsKind — Ship 0.30.121 R3 — the
	// pre-parsed LIST envelope for a CacheEntryClassApistage CONTENT
	// entry. F1's content-gate (gateListEnvelope) re-unmarshalled the
	// stored RawJSON envelope on EVERY content-Get-hit to run
	// filterListByRBAC over the items — the ~1.73 GiB double-unmarshal.
	// R3 parses the envelope's items ONCE at the content-entry Put site
	// and stores them here; the content-gate then runs filterListByRBAC
	// directly over Items and skips the unmarshal. Output is byte-
	// identical by construction (same parse -> filter -> marshalAsList
	// pipeline; only the unmarshal TIMING moves from per-hit to per-Put).
	//
	// Populated ONLY for CacheEntryClassApistage LIST content entries
	// (name=="" — a collection). Nil for restactions/widgets entries and
	// for apistage GET-by-name entries (gateGetEnvelope is left as-is).
	// A nil Items means "no pre-parse — gate via the RawJSON unmarshal
	// path" so the field is purely additive and back-compatible.
	Items           []*unstructured.Unstructured
	ItemsAPIVersion string
	ItemsKind       string

	// Ship 0.30.242 H.c-layered Phase 2 step 2a (commit subsequent to
	// 1d93d02): the previous `CohortGates atomic.Pointer[CohortGateMemoStore]`
	// field — Ship GMC / 0.30.174's per-(content-entry × cohort) memo of
	// filterListByRBAC's kept-name set — is REMOVED. Under H.c-layered the
	// apistage cell is RBAC-narrowed AT POPULATE TIME by the specific
	// binding that authorised it (design §3.4); the per-cohort gate-memo
	// is no longer needed because every cohort sharing this binding sees
	// the SAME items. The CohortGateMemoStore type, NewCohortGateMemoStore
	// constructor, and CohortGateMemoStoreLoadOrInit accessor were deleted
	// alongside the field source file (cohort_gate_memo_store.go) in commit
	// 1d93d02. The serve-time `apistage` filtering call site in
	// internal/resolvers/restactions/api/apistage.go:586 is migrated in
	// Phase 2b to serveParsedListEnvelope (a direct cell-items serve — no
	// per-cohort filter).

	// Pinned — Ship 4a (0.30.198) — marks the entry as RESIDENT: it lives
	// in a separate byte budget (maxResidentBytes) and is SKIPPED by the
	// transient LRU eviction sweep (evictUntilUnderCapsLocked). Set true
	// ONLY for EXPENSIVE prewarmed RAFullList cells (measured-cost
	// predicate — resolve wall-ms or envelope bytes over a threshold), so
	// admin's first compositions-page visit hits a warm cell rather than a
	// thrash-evicted seed (the prior cold-nav failure mode —
	// feedback_zero_cold_navigations_hard_requirement). A pinned entry is
	// still TTL- and DELETE-evictable; only LRU pressure spares it. The
	// refresher re-pins (Pinned=true) on every re-resolve so a dirty-mark
	// never demotes a pinned cell to transient.
	//
	// Read/written ONLY under ResolvedCacheStore.mu (it participates in the
	// resident byte accounting). The field is set on the *ResolvedEntry
	// before Put; Put reads it under mu to decide resident vs transient
	// accounting.
	Pinned bool

	// TTLOverride — R1 Layer 2 (#36) bounded-staleness backstop. When > 0,
	// THIS entry expires after TTLOverride instead of the store's standard
	// ttl. Set to the short CATALOG_UNSERVABLE_TTL_SECONDS when an apistage
	// entry is stored while its GVR informer is not servable, so a degraded
	// catalog entry self-evicts within the bound even if a dirty-mark is
	// missed. Zero means "use the store's standard ttl" (the default for
	// every healthy entry) — purely additive. Read in Get's TTL check;
	// also folded into the metadata ttl-remaining projection. Set before
	// Put; not mutated after.
	TTLOverride time.Duration

	// ExternalTTL — external-widget bounded-TTL cache (Option A,
	// 2026-07-10). Set true ONLY on the external-TTL Put branch
	// (widgets.go, the `krateo.io/external-cache-ttl-seconds` opt-in).
	// Marks a bounded-staleness external entry that has NO dep edges and
	// therefore MUST NOT arm a /refreshes subscription. It is read on the
	// HIT-serve path to SUPPRESS the X-Snowplow-Refresh-Key header (the
	// browser-side arming trigger) — the one runtime signal the HIT path
	// has that the entry is external-TTL, since the context-scoped
	// ExternalTouchedSink is gone by then (it lives only for the duration
	// of a resolve, and a HIT does no resolve).
	//
	// Zero value (false) = a normal entry → today's behavior byte-
	// identical (C4): the header stamps exactly as before. In-memory cache
	// state ONLY — never wire-encoded (not part of RawJSON), so a rolling
	// restart or feature-off deploy simply loses the marker and the entry
	// TTL-evicts / re-resolves as a plain entry. Set before Put; not
	// mutated after.
	ExternalTTL bool
}

// ResolvedKeyInputs is the canonical key-input bundle. The exact set
// of fields is binding: any change shifts the key space and instantly
// invalidates every in-flight cached entry — bump the constant
// resolvedKeyVersion below as part of any such change so the salt
// guarantees clean separation across rolling restarts.
//
// Ship A.3 / 0.30.179 — Username + Groups REMOVED. The identity-bound
// classes (restactions, widgets) keyed on BindingSetHash, a uint64 hash
// of the cohort's RBAC binding-pointer-set.
//
// Ship 0.30.242 H.c-layered (Phase 2 step 2a) — BindingSetHash REPLACED
// with BindingUID string. The L1 cell key now carries the metadata.uid
// of the FIRST-MATCH binding that authorised THIS layer's access for
// the request's identity. Per-binding sharing (finer granularity than
// per-cohort): two users granted by the SAME binding share the cell;
// the same user with different bindings authorising different layers
// gets different cells per layer (the per-binding granularity advantage
// over v3, design §1.2).
//
// HG-178.6 falsifier: no `Username string` + `Groups []string` literal
// columns survive in ResolvedKeyInputs for restactions/widgets.
type ResolvedKeyInputs struct {
	// CacheEntryClass is the entry-class discriminant — one of the string
	// values "restactions", "widgets", "apistage", "widgetContent", or
	// "raFullList". (Renamed from HandlerKind in 0.30.118; the string
	// VALUES are unchanged — they are hashed into the key and used as
	// refresher registry keys.)
	CacheEntryClass string
	Group           string // dispatched CR's GVR Group
	Version         string // dispatched CR's GVR Version
	Resource        string // dispatched CR's GVR Resource
	Namespace       string // dispatched CR namespace
	Name            string // dispatched CR name

	// BindingUID — Ship 0.30.242 H.c-layered — the per-layer first-match
	// binding identity that authorised this cell's access for the
	// request's identity. Replaces the v3 BindingSetHash uint64 cohort
	// hash with a finer-grained per-binding string identity:
	//
	//   ""            — anonymous / no permit / no snapshot (cache=off / pre-ready)
	//   "C:<uid>"     — ClusterRoleBinding match
	//   "R:<ns>/<uid>" — RoleBinding match (ns prefix carries scope)
	//
	// Folded into ComputeKey for every class EXCEPT widgetContent. The
	// "C:" / "R:" prefixes keep CRB and RB UIDs from aliasing; the
	// "R:<ns>/" prefix on RBs also carries namespace scope into the
	// identifier directly (defensive — apiserver does not reuse UIDs
	// across namespaces, but the prefix shape makes that invariant
	// machine-readable from the BindingUID alone).
	//
	// SOT for derivation: cache.BindingUIDFromCRB / BindingUIDFromRB in
	// internal/cache/match_subject.go (the lint-allowlisted derivation
	// site — design §4.3).
	BindingUID string

	// RepresentativeUsername + RepresentativeGroups — Ship A.3 / 0.30.179
	// Option A; KEPT in H.c-layered. The L1 cell is per-binding (keyed by
	// BindingUID), but the REFRESHER must re-resolve under a CONCRETE
	// identity (a request runs as a single user; objects.Get + RBAC
	// narrowing need a username + groups). The first writer's identity is
	// recorded here as the representative tuple for re-resolve.
	//
	// CORRECTNESS: every member of the equivalence class authorised by
	// the same BindingUID resolves to BYTE-IDENTICAL output (per-binding
	// sharing is the equivalence-class invariant — feedback_l1_per_user_
	// keyed_never_cohort.md compliant because per-binding sharing IS the
	// equivalence class for users granted by the same binding). The
	// representative is therefore EQUIVALENT to any other binding-class
	// member at resolve time. If the binding is deleted, the cell is
	// dirty-marked by BindingUID; the next /call MISSes, the seed
	// reseeds under a fresh representative — no stale-identity risk.
	//
	// EXCLUDED FROM COMPUTEKEY. These fields are bookkeeping carried on
	// ResolvedEntry.Inputs, NOT key material. Two members writing the
	// same cell must NOT shift the cell's identity by name; ComputeKey
	// skips them entirely.
	RepresentativeUsername string
	RepresentativeGroups   []string

	PerPage int
	Page    int
	Extras  map[string]any

	// Stage is set ONLY for CacheEntryClass=="apistage" entries (Ship E,
	// 0.30.116). It carries the per-stage discriminator string —
	// stage id + O5 canonical filter-hash + a hash of the stage's
	// effective dict input (its dependsOn predecessor output). Empty
	// for "restactions"/"widgets" entries, so ComputeKey is
	// byte-identical to 0.30.115 for every non-apistage key (a
	// pre-existing entry's key does not shift). The api-stage resolver
	// builds the Stage value; ComputeKey only folds it into the hash.
	Stage string

	// HasUAF — #118 (d) interim: true when the resolved cell's RESTAction
	// declares a userAccessFilter stage. Set by the customer dispatch (which
	// has the resolved CR in hand) into the cacheInputs before Put, and CARRIED
	// on ResolvedEntry.Inputs so the refresher re-Put (which only has the stored
	// Inputs, not the CR) can re-stamp the short UAF TTLOverride too — the
	// #118 C-118-6 both-Put-sites requirement (else the CreatedAt-slide on a hot
	// refreshed cell defeats the cap).
	//
	// EXCLUDED FROM COMPUTEKEY. Like RepresentativeUsername/Groups above, this
	// is bookkeeping carried on Inputs, NOT key material — ComputeKey does not
	// hash it, so adding it does NOT shift the key space (no resolvedKeyVersion
	// bump) and a UAF cell keeps the SAME key as before, only a shorter TTL.
	HasUAF bool

	// RBACSubGen — #118 (c) DURABLE fix. The requesting identity's EFFECTIVE
	// per-subject RBAC sub-generation (RBACSubGenForSubject over the user +
	// groups (+ SA) counters), FOLDED INTO ComputeKey for every identity-bound
	// class (like BindingUID; EXCLUDED for widgetContent, which is identity-free).
	// A grant/revoke that touches this user's OWN bindings bumps a subject
	// counter → this term changes → new key → cold miss → fresh resolve → fresh
	// UAF refilter. Blast radius = only users whose own bindings changed (herd-
	// proportional; survives the 50K install storm that global RBACGen dies in).
	// Stamped on the DISPATCH path (dispatchCacheLookupKey → helpers.go, for
	// dispatch/subscription) from the RBACSubGenForSubject reader. The SEED does
	// NOT stamp it (the identity-bound seed Put writes RBACSubGen==0): #118
	// (c)-v2 GAP-3 de-scoped this as a #42-class seed-reachability perf gap
	// (dispatch key stays correct — a warm-miss for a moved-sub-gen subject, not
	// an authz-staleness bug), ticketed separately. UNLIKE HasUAF this IS folded
	// into ComputeKey → resolvedKeyVersion (v4→v5→v6 across (c) and (c)-v2).
	RBACSubGen uint64
}

// resolvedKeyVersion is folded into every key hash so a key-schema
// change forces a clean break across rolling pods. Bump on any change
// to ResolvedKeyInputs fields or the key-encoding logic.
//
// NOT bumped for Ship E's Stage field: ComputeKey folds Stage in only
// when it is non-empty (see ComputeKey), so every "restactions" /
// "widgets" key — Stage=="" — hashes byte-identically to v1. A version
// bump would needlessly rotate the whole key space on the 0.30.116
// rolling restart for zero correctness gain.
//
// Ship A.3 / 0.30.179 — BUMPED v1 → v2. The identity field shape
// changed (Username + Groups removed; BindingSetHash added) so every
// pre-0.30.179 key is structurally different from a fresh key for
// the SAME cohort. The salt rotation forces a clean break across the
// rolling restart: pre-v2 entries never serve as v2 hits (AC-178.3).
//
// Ship 1 / 0.30.195 — BUMPED v2 → v3. BindingSetHash now hashes the
// binding's immutable metadata.uid instead of its pointer address
// (rbac_cohort_gen.go — collectCohortBindingIDs / fnv64aIdentities). The
// hash VALUE for the same logical cohort differs from the v2 pointer-set
// value, so a pre-0.30.195 (pointer-keyed) L1 entry MUST NOT be served as
// the new UID-keyed entry for the same cohort. The salt rotation forces a
// clean rolling key break: pre-v3 entries never serve as v3 hits.
//
// Ship 0.30.242 H.c-layered (Phase 2 step 2a) — BUMPED v3 → v4. The
// identity field changed from BindingSetHash uint64 (cohort hash over
// the matched binding-set) to BindingUID string (first-match per-layer
// binding identity). The ComputeKey body fold rotates from a uint64
// LittleEndian encoding to a UTF-8 string write — pre-v4 keys are
// structurally different from v4 keys for the SAME logical access. The
// salt rotation forces a clean rolling key break: pre-v4 (cohort-keyed)
// entries never serve as v4 (per-binding-keyed) hits.
//
// Additionally, the v3-v4 fold also drops the apistage exclusion: under
// v3 the apistage cell was identity-free at the ComputeKey level (the
// per-cohort gate-memo filtered items at serve time); under v4 the
// apistage cell folds BindingUID like every other identity-bound class.
// The cohort-gate-memo apparatus is deleted (design §3.4). Pre-v4
// apistage entries CANNOT serve as v4 hits even under the same identity
// because the v3 key did not encode identity.
// Ship #118 (c) — BUMPED v4 → v5. ComputeKey now folds RBACSubGen (the
// requesting identity's per-subject RBAC sub-generation) alongside BindingUID
// for every identity-bound class. A pre-v5 cell's key did not encode the
// sub-gen, so it is structurally different from a v5 key for the SAME access;
// the salt rotation forces a clean rolling key break — no pre-fix (RBAC-blind)
// entry serves as a post-fix hit across the restart (C-118-7).
//
// Ship #118 (c)-v2 — BUMPED v5 → v6. The RBACSubGen FIELD SHAPE is unchanged
// (still uint64), but its TIMELINE changed: (c) v1 (v5) bumped the sub-gen
// SYNCHRONOUSLY on the RBAC delta event; (c)-v2 defers the bump to
// snapshot-publish (rbac_subgen_pending.go, GAP-2). For a subject mid-
// transition at the rolling-restart boundary, the SAME logical access can map
// to a different sub-gen under the two regimes → a v5 cell could be served as
// a v6 hit and re-pin the very staleness this fix removes. The salt rotation
// forces every pod to treat pre-v6 cells as non-hits — a clean cross-regime
// break, identical rationale to v3→v4 and v4→v5.
const resolvedKeyVersion = "v6"

// ResolvedCacheStore is the L1 resolved-output cache: a bounded LRU
// guarded by a single mutex with a per-entry byte budget. Constructed
// lazily by ResolvedCache(); never read or written without holding mu.
//
// Exported only so dispatchers and tests can take a handle; production
// code MUST go through cache.ResolvedCache() rather than instantiating
// stores directly.
type ResolvedCacheStore struct {
	mu sync.Mutex

	// LRU eviction order: front = most-recently-used.
	order *list.List
	// Lookup index. Value is *list.Element whose Value is *lruItem.
	index map[string]*list.Element

	maxEntries int
	maxBytes   int64
	ttl        time.Duration

	curBytes int64

	// Ship 4a (0.30.198) — PINNED resident region. maxResidentBytes is the
	// separate byte budget for entries with ResolvedEntry.Pinned==true;
	// curResidentBytes tracks the live resident weight. Resident entries are
	// SKIPPED by evictUntilUnderCapsLocked (the transient LRU sweep) and are
	// NOT counted in curBytes — the two budgets are independent. A
	// maxResidentBytes of 0 DISABLES pinning (Put stores everything
	// transient). residentEntries is the live resident entry count, surfaced
	// in Stats for the prewarm-coverage falsifier.
	maxResidentBytes int64
	curResidentBytes int64
	residentEntries  int

	// Falsifier counters (atomic; safe to read without mu).
	hitTotal         atomic.Uint64
	missTotal        atomic.Uint64
	evictLRUTotal    atomic.Uint64
	evictTTLTotal    atomic.Uint64
	evictDeleteTotal atomic.Uint64 // 0.30.8: DELETE-event-driven evictions
	storeTotal       atomic.Uint64

	// Ship E (0.30.116) api-stage counters. apistageStoreTotal counts
	// Put()s of an "apistage"-kind entry; apistageEvictTotal counts
	// evictions (LRU/TTL/DELETE) of one. apistage_evict_pressure in the
	// summary line is the evict/store ratio — the O6 budget signal: a
	// high ratio means the maxEntries/maxBytes budget is too small for
	// the N-identities × M-stages cardinality and the api-stage entries
	// are churning rather than being reused. The store classifies via
	// entry.Inputs.CacheEntryClass, so the opaque key string never needs a
	// per-kind tag.
	apistageStoreTotal atomic.Uint64
	apistageEvictTotal atomic.Uint64

	// Ship G (0.30.16x) widget-content counters. widgetContentStoreTotal
	// counts Put()s of a "widgetContent"-kind entry (the identity-free
	// widget envelope cached by Phase 1's F2 walker as a free side-effect
	// of widgets.Resolve); widgetContentEvictTotal counts evictions
	// (LRU/TTL/DELETE) of one. widget_content_evict_pressure in the
	// summary line is the evict/store ratio — same shape as the apistage
	// counters. Classified off entry.Inputs.CacheEntryClass.
	widgetContentStoreTotal atomic.Uint64
	widgetContentEvictTotal atomic.Uint64

	// Ship 4a (0.30.198) raFullList + resident-region counters.
	// raFullListStoreTotal counts Put()s of a "raFullList"-kind entry;
	// raFullListEvictTotal counts evictions of one (TTL/DELETE — a pinned
	// raFullList is never LRU-evicted; an un-pinned one can be).
	// residentPinTotal counts Put()s that landed in the resident region
	// (Pinned honoured); residentDemoteTotal counts Put()s that REQUESTED a
	// pin but were stored transient instead (maxResidentBytes==0 or the
	// resident budget would overflow) — the kill-switch / degrade signal.
	raFullListStoreTotal atomic.Uint64
	raFullListEvictTotal atomic.Uint64
	residentPinTotal     atomic.Uint64
	residentDemoteTotal  atomic.Uint64
}

type lruItem struct {
	key   string
	entry *ResolvedEntry
	bytes int64
}

var (
	resolvedCacheInstance *ResolvedCacheStore
	resolvedCacheOnce     sync.Once
	resolvedCacheStarted  atomic.Bool

	// resolvedCachePublished mirrors resolvedCacheInstance as a
	// race-free, NON-CONSTRUCTING handle (1.12.4 §7b). Observability
	// readers — the expvar closure and the OTLP observable callback —
	// must not be able to bring the singleton into existence, because
	// ResolvedCache() also wires the dep tracker and starts the summary
	// goroutine. A metrics scrape creating a cache is a side effect no
	// telemetry surface should have.
	//
	// Written exactly once, inside resolvedCacheOnce.Do, immediately
	// after the instance is fully configured; read with Load() from any
	// goroutine. Reading resolvedCacheInstance directly instead would be
	// a data race against that same Do.
	resolvedCachePublished atomic.Pointer[ResolvedCacheStore]
)

// ResolvedCacheEnabled reports whether the L1 resolved-output cache is
// active. Two gates must both be true:
//  1. CACHE_ENABLED=true (entire cache subsystem). Anything else and we
//     are in pure 0.25.x parity mode; the resolver runs on every call.
//  2. RESOLVED_CACHE_ENABLED!=false (per-feature toggle). Defaults to
//     true when CACHE_ENABLED=true; explicit "false"/"0"/"no" disables.
//
// This split lets cache=on serve EvaluateRBAC + the typed-RBAC indexer
// while leaving L1 disabled for back-out scenarios.
func ResolvedCacheEnabled() bool {
	if Disabled() {
		return false
	}
	switch os.Getenv(envResolvedCacheEnabled) {
	case "false", "0", "no":
		return false
	default:
		return true
	}
}

// ApistageL1Enabled reports whether the Ship E (0.30.116) per-api-stage
// L1 key-swap is active. On iff ResolvedCacheEnabled(), i.e. both master
// gates hold:
//  1. CACHE_ENABLED=true        — the whole cache subsystem (Disabled()).
//  2. RESOLVED_CACHE_ENABLED!=false — the resolved-output L1 store +
//     refresher, which the api-stage entry reuses verbatim.
//
// Folded into the master gate per #57 (project_single_cache_flag_direction)
// — the api-stage L1 is a working, load-bearing, RBAC-sensitive identity-
// free cache that ran =true everywhere in production, so it is now in the
// same class as PrewarmEnabled: implicit-on under the cache subsystem with
// no per-feature env flag. With RESOLVED_CACHE_ENABLED=false (or the whole
// subsystem off via CACHE_ENABLED) the RESTAction resolver runs byte-
// identical to 0.30.115 — no per-stage Get/Put, no api-stage L1 key
// (AC-E1, now re-anchored to RESOLVED_CACHE_ENABLED=false).
func ApistageL1Enabled() bool {
	return ResolvedCacheEnabled()
}

// WidgetContentL1Enabled reports whether the Ship G (0.30.16x) identity-
// free widget content L1 layer is opted in. TWO gates, all must hold:
//  1. CACHE_ENABLED=true            — the whole cache subsystem
//     (Disabled()).
//  2. RESOLVED_CACHE_ENABLED!=false — the resolved-output L1 store +
//     refresher, which the widget content entry reuses verbatim.
//  3. WIDGET_CONTENT_L1_ENABLED!="false" — the per-feature toggle.
//     Defaults to true; explicit "false"/"0"/"no" disables.
//
// Default ON when the cache subsystem itself is on, mirroring
// ResolvedCacheEnabled. When CACHE_ENABLED=false the entire path is
// skipped (cleanly removable per project_caching_is_provisional).
// WIDGET_CONTENT_L1_ENABLED=false bypasses ONLY this upper layer; the
// per-user widget L1 + apistage L1 (if enabled) keep serving — same
// "AC-G.6" fine-grained toggle pattern as ApistageL1Enabled.
func WidgetContentL1Enabled() bool {
	if !ResolvedCacheEnabled() {
		return false
	}
	switch os.Getenv(envWidgetContentL1Enabled) {
	case "false", "0", "no":
		return false
	default:
		return true
	}
}

// ResolvedCache returns the singleton resolved-output cache, lazily
// initialising it on first use. Returns nil when ResolvedCacheEnabled()
// is false — callers MUST nil-check.
func ResolvedCache() *ResolvedCacheStore {
	if !ResolvedCacheEnabled() {
		return nil
	}
	resolvedCacheOnce.Do(func() {
		resolvedCacheInstance = newResolvedCache(
			intFromEnv(envResolvedCacheMaxEntries, defaultResolvedCacheMaxEntries),
			int64BytesFromEnv(envResolvedCacheMaxBytes, defaultResolvedCacheMaxBytes),
			time.Duration(intFromEnv(envResolvedCacheTTLSeconds, defaultResolvedCacheTTLSeconds))*time.Second,
		)
		// Ship 4a (0.30.198) — wire the resident-region budget. A 0 value is
		// VALID and explicitly DISABLES pinning (kill-switch), so it is read
		// directly rather than through newResolvedCache's positive-default
		// guard. int64BytesFromEnv (#278-C) returns the default only when the
		// var is unset, unparseable, or NEGATIVE; an explicit "0" disables
		// pinning and is preserved (its range contract accepts 0). Parse-
		// reject and negative now emit a WARN — the #154 silent-512MiB-
		// truncation case is now operator-visible, and scientific notation
		// (e.g. "5e8") parses via the ParseFloat fallback.
		resolvedCacheInstance.maxResidentBytes = int64BytesFromEnv(
			envResolvedCacheMaxResidentBytes, defaultResolvedCacheMaxResidentBytes)
		// 0.30.8: wire the cache into the dep tracker so OnDelete can
		// evict and so any eviction path (LRU/TTL/DELETE) calls
		// Deps().RemoveL1Key to keep dep records and L1 entries
		// in lock-step.
		Deps().SetStore(resolvedCacheInstance)
		startResolvedCacheSummary(resolvedCacheInstance)
		// 1.12.4 §7b — publish the non-constructing observability handle
		// LAST, so a reader that sees it also sees a fully-configured
		// store (maxResidentBytes set, dep tracker wired).
		resolvedCachePublished.Store(resolvedCacheInstance)
	})
	return resolvedCacheInstance
}

// newResolvedCache constructs a fresh cache. Exported for tests; in
// production the singleton path goes through ResolvedCache().
func newResolvedCache(maxEntries int, maxBytes int64, ttl time.Duration) *ResolvedCacheStore {
	if maxEntries <= 0 {
		maxEntries = defaultResolvedCacheMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = defaultResolvedCacheMaxBytes
	}
	if ttl <= 0 {
		ttl = time.Duration(defaultResolvedCacheTTLSeconds) * time.Second
	}
	return &ResolvedCacheStore{
		order:      list.New(),
		index:      map[string]*list.Element{},
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		ttl:        ttl,
		// Ship 4a (0.30.198) — default the resident budget so test
		// construction has pinning available. The production singleton path
		// OVERWRITES this from RESOLVED_CACHE_MAX_RESIDENT_BYTES (where an
		// explicit 0 disables pinning). Tests that exercise pin behaviour set
		// the field directly.
		maxResidentBytes: defaultResolvedCacheMaxResidentBytes,
	}
}

// ComputeKey produces the canonical cache key for the supplied inputs.
// The output is a hex-encoded SHA-256 over a versioned, sorted byte
// representation of every field; tests cover stability + sensitivity.
func ComputeKey(in ResolvedKeyInputs) string {
	h := sha256.New()
	// version prefix — any future schema bump rotates the entire key
	// space on rolling restart.
	h.Write([]byte(resolvedKeyVersion))
	h.Write([]byte{0})
	h.Write([]byte(in.CacheEntryClass))
	h.Write([]byte{0})
	h.Write([]byte(in.Group))
	h.Write([]byte{0})
	h.Write([]byte(in.Version))
	h.Write([]byte{0})
	h.Write([]byte(in.Resource))
	h.Write([]byte{0})
	h.Write([]byte(in.Namespace))
	h.Write([]byte{0})
	h.Write([]byte(in.Name))
	h.Write([]byte{0})

	// Identity. Ship G (0.30.16x): widgetContent is identity-free — the
	// widget envelope is shared, the per-user `allowed` flag is re-derived
	// at serve time.
	//
	// Ship 0.30.242 H.c-layered (Phase 2 step 2a): identity-bound classes
	// (restactions, widgets, apistage, raFullList) fold in `BindingUID` —
	// the first-match per-layer binding identity from the cache snapshot
	// (cache.BindingUIDFromCRB / FromRB applied to whichever CRB or RB
	// granted THIS layer's access for the request's identity). Two users
	// granted by the SAME binding land on the SAME cell — finer-grained
	// sharing than v3's per-cohort hash (per design §3.3 + §3.4).
	//
	// apistage flipped from identity-free (v3) to identity-bound (v4):
	// under v3 the apistage cell held SA-populated raw items filtered
	// per-cohort at serve time by gateListItemsWithMemo. Under v4 the
	// apistage cell is RBAC-narrowed AT POPULATE TIME by the specific
	// binding that authorised it; the cohort-gate-memo apparatus is
	// deleted (design §3.4). widgetContent stays identity-free.
	//
	// This is a per-CLASS key shape, NOT a per-resource switch
	// (feedback_no_special_cases): the discriminant is the entry class,
	// uniform for every entry of every GVR. widgetContent skips the
	// identity fold entirely; all other classes fold the BindingUID
	// string. The v3 → v4 resolvedKeyVersion bump rotates the key space
	// cleanly on the rolling restart so no v3 entry serves as a v4 hit.
	if in.CacheEntryClass != CacheEntryClassWidgetContent {
		h.Write([]byte(in.BindingUID))
		h.Write([]byte{0xff}) // identity terminator
		// #118 (c) v4→v5: fold the requesting identity's per-subject RBAC
		// sub-generation alongside BindingUID for every identity-bound class.
		// The BindingUID captures WHICH binding authorised THIS layer's GET;
		// RBACSubGen captures "did anything about this user's effective RBAC
		// change" (incl a per-namespace grant/revoke the single dispatch-GET
		// BindingUID is blind to — the userAccessFilter refilter dependency,
		// #118 defect 1). A change to the user's own bindings bumps a subject
		// counter → this fold changes → new key → cold miss → fresh refilter.
		// widgetContent is excluded (identity-free shared envelope) exactly as
		// for BindingUID — folding identity there would break the shared-content
		// invariant (design §key-parity-surface). uint64 LE, then a terminator.
		var subgen [8]byte
		binary.LittleEndian.PutUint64(subgen[:], in.RBACSubGen)
		h.Write(subgen[:])
		h.Write([]byte{0xfe}) // sub-gen terminator (distinct from the 0xff identity terminator)
	}

	h.Write([]byte(strconv.Itoa(in.PerPage)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(in.Page)))
	h.Write([]byte{0})

	// Stage (Ship E, 0.30.116): folded in ONLY when non-empty. An empty
	// Stage writes nothing — so a "restactions"/"widgets" key (Stage=="")
	// hashes byte-identically to the pre-0.30.116 encoding and no
	// in-flight entry's key shifts. The non-empty branch writes a
	// sentinel byte (0x01) before the value so an api-stage key can
	// never collide with a hypothetical extras-only key that happened to
	// produce the same trailing bytes.
	if in.Stage != "" {
		h.Write([]byte{0x01})
		h.Write([]byte(in.Stage))
		h.Write([]byte{0})
	}

	// Extras: canonicalise via sorted-key JSON. We deliberately use
	// json.Marshal on a SORTED-KEY surrogate instead of MarshalIndent
	// to keep the byte count tight; the surrogate is built by
	// canonicaliseExtras below.
	if len(in.Extras) > 0 {
		if buf, err := canonicaliseExtras(in.Extras); err == nil {
			h.Write(buf)
		} else {
			// On marshal failure (cyclic / non-JSON value), fall
			// back to a deterministic-but-pessimistic dump of
			// fmt.Sprintf so the key still varies with content.
			h.Write([]byte(fmt.Sprintf("%v", in.Extras)))
		}
	}
	h.Write([]byte{0})

	return hex.EncodeToString(h.Sum(nil))
}

// HashExtras returns a stable, order-independent hash of a JSON-native extras
// map — the SAME canonicalisation ComputeKey folds into the L1 key (single
// derivation site via canonicaliseExtras, so the F4 seed-resolve memo's notion
// of "same effective extras" cannot drift from the L1 key's). Empty/nil map →
// the fixed sentinel "e0" (never empty, so an empty-extras key segment is
// unambiguous). Used by the apiref seam to build the SeedResolveMemo key's
// extras component; keeping the canonicalisation here means a future extras
// schema change updates memo + L1 key together.
func HashExtras(m map[string]any) string {
	if len(m) == 0 {
		return "e0"
	}
	buf, err := canonicaliseExtras(m)
	if err != nil {
		// Deterministic-but-pessimistic fallback — mirrors ComputeKey's
		// marshal-failure branch so a cyclic/non-JSON value still varies the
		// key rather than colliding.
		buf = []byte(fmt.Sprintf("%v", m))
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// canonicaliseExtras emits a sorted-key JSON encoding of m. Nested
// maps are recursively canonicalised; everything else round-trips
// through json.Marshal as-is.
func canonicaliseExtras(m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []byte
	out = append(out, '{')
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		out = append(out, kb...)
		out = append(out, ':')
		v := m[k]
		if nested, ok := v.(map[string]any); ok {
			vb, err := canonicaliseExtras(nested)
			if err != nil {
				return nil, err
			}
			out = append(out, vb...)
			continue
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out = append(out, vb...)
	}
	out = append(out, '}')
	return out, nil
}

// effectiveTTLLocked returns the TTL governing this entry's expiry: the
// per-entry TTLOverride when positive (R1 Layer 2 #36 bounded-staleness
// backstop), else the store's standard ttl. Callers MUST hold c.mu.
func (c *ResolvedCacheStore) effectiveTTLLocked(entry *ResolvedEntry) time.Duration {
	if entry != nil && entry.TTLOverride > 0 {
		// Honour the shorter of the two so the backstop can only TIGHTEN the
		// bound, never extend an entry past the store's standard ttl.
		if c.ttl <= 0 || entry.TTLOverride < c.ttl {
			return entry.TTLOverride
		}
	}
	return c.ttl
}

// Get returns the cached entry for key, or (nil, false). A TTL-expired
// entry is treated as a miss and is dropped during the same call so
// memory pressure is bounded. Increments hit/miss counters atomically.
func (c *ResolvedCacheStore) Get(key string) (*ResolvedEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[key]
	if !ok {
		c.missTotal.Add(1)
		return nil, false
	}
	item := el.Value.(*lruItem)
	// R1 Layer 2 (#36): a per-entry TTLOverride (the short
	// CATALOG_UNSERVABLE_TTL_SECONDS set on entries stored while their GVR
	// was not servable) takes precedence over the store's standard ttl, so a
	// degraded catalog entry self-evicts within the bound. Zero override =
	// the standard ttl (every healthy entry).
	if eff := c.effectiveTTLLocked(item.entry); eff > 0 && time.Since(item.entry.CreatedAt) > eff {
		c.removeElementLocked(el)
		c.evictTTLTotal.Add(1)
		c.missTotal.Add(1)
		return nil, false
	}
	// LRU touch: move to front.
	c.order.MoveToFront(el)
	c.hitTotal.Add(1)
	return item.entry, true
}

// Put stores entry under key, evicting LRU tail entries until both
// entry-count and byte-budget caps are satisfied. The entry's CreatedAt
// is set to time.Now() if zero. Putting under a key that already exists
// replaces the entry and adjusts curBytes accordingly.
func (c *ResolvedCacheStore) Put(key string, entry *ResolvedEntry) {
	if c == nil || entry == nil {
		return
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	bytes := entryBytes(entry)

	c.mu.Lock()
	defer c.mu.Unlock()

	apistage := isApistageEntry(entry)
	widgetContent := isWidgetContentEntry(entry)
	raFullList := isRAFullListEntry(entry)

	// Ship 4a (0.30.198) — resolve the entry's FINAL pin status. A pin is
	// requested via entry.Pinned (set by an expensive prewarm/refresher
	// Put). It is HONOURED only when the resident region is enabled
	// (maxResidentBytes > 0) AND the new resident weight fits under the
	// resident budget. Otherwise the entry is DEMOTED to transient
	// (Pinned=false) — the kill-switch (budget 0) and the overflow-guard
	// both degrade to LRU rather than evicting another pinned cell or
	// OOMing. The decision is taken HERE, under mu, after subtracting any
	// prior resident weight for this key (replace-in-place may flip status).
	priorResident := int64(0)
	if el, ok := c.index[key]; ok {
		old := el.Value.(*lruItem)
		if old.entry != nil && old.entry.Pinned {
			priorResident = old.bytes
		}
	}
	if entry.Pinned {
		if c.maxResidentBytes <= 0 {
			// Kill-switch: pinning disabled. Store transient.
			entry.Pinned = false
			c.residentDemoteTotal.Add(1)
		} else if c.curResidentBytes-priorResident+bytes > c.maxResidentBytes {
			// Resident budget would overflow. Degrade to transient rather
			// than evict another pinned cell (the prewarm-coverage contract
			// — a pinned cell is never sacrificed for another pin).
			entry.Pinned = false
			c.residentDemoteTotal.Add(1)
		}
	}

	// Replace-in-place semantics if key already present. The prior entry's
	// status (transient vs resident) may differ from the new one, so adjust
	// BOTH budgets symmetrically.
	if el, ok := c.index[key]; ok {
		old := el.Value.(*lruItem)
		if old.entry != nil && old.entry.Pinned {
			c.curResidentBytes -= old.bytes
			c.residentEntries--
		} else {
			c.curBytes -= old.bytes
		}
		old.entry = entry
		old.bytes = bytes
		if entry.Pinned {
			c.curResidentBytes += bytes
			c.residentEntries++
			c.residentPinTotal.Add(1)
		} else {
			c.curBytes += bytes
		}
		c.order.MoveToFront(el)
		c.storeTotal.Add(1)
		c.bumpClassStoreLocked(apistage, widgetContent, raFullList)
		c.evictUntilUnderCapsLocked()
		return
	}

	item := &lruItem{key: key, entry: entry, bytes: bytes}
	el := c.order.PushFront(item)
	c.index[key] = el
	if entry.Pinned {
		c.curResidentBytes += bytes
		c.residentEntries++
		c.residentPinTotal.Add(1)
	} else {
		c.curBytes += bytes
	}
	c.storeTotal.Add(1)
	c.bumpClassStoreLocked(apistage, widgetContent, raFullList)

	c.evictUntilUnderCapsLocked()
}

// bumpClassStoreLocked increments the per-class store counters. Must be
// called with mu held (or from a context where atomic adds are fine — the
// counters are atomic, but keeping the call under mu groups it with the
// byte accounting). Ship 4a factored this out of Put's two branches.
func (c *ResolvedCacheStore) bumpClassStoreLocked(apistage, widgetContent, raFullList bool) {
	if apistage {
		c.apistageStoreTotal.Add(1)
	}
	if widgetContent {
		c.widgetContentStoreTotal.Add(1)
	}
	if raFullList {
		c.raFullListStoreTotal.Add(1)
	}
}

// isApistageEntry reports whether entry is a Ship E api-stage L1 entry —
// classified by its Inputs.CacheEntryClass. Nil-safe.
func isApistageEntry(entry *ResolvedEntry) bool {
	return entry != nil && entry.Inputs != nil &&
		entry.Inputs.CacheEntryClass == CacheEntryClassApistage
}

// isWidgetContentEntry reports whether entry is a Ship G widget-content
// L1 entry — classified by its Inputs.CacheEntryClass. Nil-safe.
func isWidgetContentEntry(entry *ResolvedEntry) bool {
	return entry != nil && entry.Inputs != nil &&
		entry.Inputs.CacheEntryClass == CacheEntryClassWidgetContent
}

// isRAFullListEntry reports whether entry is a Ship 4a raFullList L1
// entry — classified by its Inputs.CacheEntryClass. Nil-safe.
func isRAFullListEntry(entry *ResolvedEntry) bool {
	return entry != nil && entry.Inputs != nil &&
		entry.Inputs.CacheEntryClass == CacheEntryClassRAFullList
}

// itemsTreeOverheadFactor estimates the in-memory footprint of a parsed
// []*unstructured.Unstructured tree relative to the JSON text it was
// parsed from. A Go map[string]any / []any interface tree carries
// per-node header + boxing overhead well above the compact JSON byte
// length; 3x is a deliberately conservative floor so the LRU byte cap
// does not silently under-count the R3 pre-parsed Items (Ship 0.30.121).
const itemsTreeOverheadFactor = 3

// entryBytes is the LRU byte-accounting weight of an L1 entry — Ship
// 0.30.121 R3. It counts the pre-encoded RawJSON envelope AND, when the
// entry carries the R3 pre-parsed Items (an apistage LIST content
// entry), the estimated in-memory footprint of that parsed tree. Without
// the Items term the byte cap silently under-counts every content entry
// by roughly its own envelope size, letting curBytes drift far past
// maxBytes. Items is parsed from RawJSON, so its tree size is estimated
// as itemsTreeOverheadFactor * len(RawJSON) rather than re-serialising
// each item (which would re-introduce the very marshal R3 removes).
// A nil/empty Items contributes nothing — restactions/widgets entries
// and apistage GET entries are accounted exactly as pre-0.30.121.
func entryBytes(entry *ResolvedEntry) int64 {
	if entry == nil {
		return 0
	}
	b := int64(len(entry.RawJSON))
	if len(entry.Items) > 0 {
		b += int64(len(entry.RawJSON)) * itemsTreeOverheadFactor
	}
	return b
}

// Len returns the number of entries currently held. Safe to call
// without external locking; takes the internal mutex.
func (c *ResolvedCacheStore) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Bytes returns the current byte usage. Safe under concurrent traffic.
func (c *ResolvedCacheStore) Bytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes
}

// Stats returns a snapshot of the falsifier counters. Numbers are
// atomic and may drift between fields by a single call, which is fine
// for log aggregation.
type ResolvedCacheStats struct {
	Entries          int
	Bytes            int64
	MaxEntries       int
	MaxBytes         int64
	HitTotal         uint64
	MissTotal        uint64
	StoreTotal       uint64
	EvictLRUTotal    uint64
	EvictTTLTotal    uint64
	EvictDeleteTotal uint64 // 0.30.8: DELETE-event-driven evictions

	// Ship E (0.30.116) api-stage counters.
	ApistageStoreTotal uint64
	ApistageEvictTotal uint64

	// Ship G (0.30.16x) widget-content counters.
	WidgetContentStoreTotal uint64
	WidgetContentEvictTotal uint64

	// Ship 4a (0.30.198) raFullList + resident-region counters.
	RAFullListStoreTotal uint64
	RAFullListEvictTotal uint64
	ResidentEntries      int
	ResidentBytes        int64
	MaxResidentBytes     int64
	ResidentPinTotal     uint64
	ResidentDemoteTotal  uint64
}

func (c *ResolvedCacheStore) Stats() ResolvedCacheStats {
	if c == nil {
		return ResolvedCacheStats{}
	}
	c.mu.Lock()
	entries := c.order.Len()
	bytes := c.curBytes
	residentEntries := c.residentEntries
	residentBytes := c.curResidentBytes
	maxResidentBytes := c.maxResidentBytes
	c.mu.Unlock()
	return ResolvedCacheStats{
		Entries:                 entries,
		Bytes:                   bytes,
		MaxEntries:              c.maxEntries,
		MaxBytes:                c.maxBytes,
		HitTotal:                c.hitTotal.Load(),
		MissTotal:               c.missTotal.Load(),
		StoreTotal:              c.storeTotal.Load(),
		EvictLRUTotal:           c.evictLRUTotal.Load(),
		EvictTTLTotal:           c.evictTTLTotal.Load(),
		EvictDeleteTotal:        c.evictDeleteTotal.Load(),
		ApistageStoreTotal:      c.apistageStoreTotal.Load(),
		ApistageEvictTotal:      c.apistageEvictTotal.Load(),
		WidgetContentStoreTotal: c.widgetContentStoreTotal.Load(),
		WidgetContentEvictTotal: c.widgetContentEvictTotal.Load(),
		RAFullListStoreTotal:    c.raFullListStoreTotal.Load(),
		RAFullListEvictTotal:    c.raFullListEvictTotal.Load(),
		ResidentEntries:         residentEntries,
		ResidentBytes:           residentBytes,
		MaxResidentBytes:        maxResidentBytes,
		ResidentPinTotal:        c.residentPinTotal.Load(),
		ResidentDemoteTotal:     c.residentDemoteTotal.Load(),
	}
}

// ResolvedEntryMeta is the METADATA-ONLY projection of one cached
// resolved-output entry, returned by RangeMetadata for the /debug/apistage
// diagnostic (R1 design §6 Mode 1).
//
// STRUCTURAL LEAK GUARD (PM F-2): this struct contains ONLY scalar metadata
// — class, key hash, GVR coordinates, age/TTL, and the LENGTH of the body.
// It deliberately has NO []byte, NO []*unstructured.Unstructured, and NO
// map field, so it is STRUCTURALLY INCAPABLE of carrying RawJSON, the parsed
// Items, or the Extras key-inputs. Resolved output is per-identity
// RBAC-sensitive; a content dump would be a cross-user leak. The leak guard
// is the type itself (and RangeMetadata's field-by-field copy), not a
// comment — see TestRangeMetadata_StructurallyCannotLeakContent.
type ResolvedEntryMeta struct {
	CacheEntryClass string `json:"cacheEntryClass"`
	// KeyHash is the opaque ComputeKey string (a hash) — not reversible to
	// inputs, safe to expose.
	KeyHash string `json:"keyHash"`
	// Path is a derived apiserver-style path from the GVR coordinates
	// (group/version/resource[/namespaces/ns][/name]) — built from the key
	// inputs, NOT from any resolved body.
	Path      string `json:"path"`
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Stage is the apistage per-stage discriminator (itself a hash of the
	// stage id + filter + dependsOn-output) — opaque, not content.
	Stage       string `json:"stage,omitempty"`
	AgeSeconds  int64  `json:"ageSeconds"`
	TTLRemainingSeconds int64 `json:"ttlRemainingSeconds"`
	Pinned      bool   `json:"pinned"`
	// ItemsCount is the LENGTH of the pre-parsed LIST envelope (0 when not a
	// parsed-list apistage entry). A count only — never the items themselves.
	ItemsCount int `json:"itemsCount"`
	// RawJSONBytes is the LENGTH of the encoded body (for size diagnostics) —
	// never the body.
	RawJSONBytes int `json:"rawJSONBytes"`
}

// RangeMetadata walks every live cached entry under c.mu (read-consistent
// snapshot semantics: the lock is held for the whole walk) and invokes fn
// with the METADATA-ONLY projection of each. Iteration stops early if fn
// returns false. Returns immediately on a nil receiver.
//
// STRUCTURAL LEAK GUARD (PM F-2): fn receives a ResolvedEntryMeta, which by
// construction cannot carry RawJSON / Items / Extras (see the type doc). This
// method reads those fields ONLY to compute scalar projections (len(), an age
// duration, a path string from the GVR) — it never copies a body, a parsed
// item, or the extras map into the emitted value. A future field added to
// ResolvedEntryMeta that could carry content would break
// TestRangeMetadata_StructurallyCannotLeakContent.
func (c *ResolvedCacheStore) RangeMetadata(fn func(ResolvedEntryMeta) bool) {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.order.Front(); el != nil; el = el.Next() {
		item := el.Value.(*lruItem)
		entry := item.entry
		meta := ResolvedEntryMeta{
			KeyHash:      item.key,
			AgeSeconds:   int64(now.Sub(entry.CreatedAt).Seconds()),
			Pinned:       entry.Pinned,
			ItemsCount:   len(entry.Items),
			RawJSONBytes: len(entry.RawJSON),
		}
		if eff := c.effectiveTTLLocked(entry); eff > 0 {
			rem := eff - now.Sub(entry.CreatedAt)
			if rem < 0 {
				rem = 0
			}
			meta.TTLRemainingSeconds = int64(rem.Seconds())
		}
		if in := entry.Inputs; in != nil {
			meta.CacheEntryClass = in.CacheEntryClass
			meta.Group = in.Group
			meta.Version = in.Version
			meta.Resource = in.Resource
			meta.Namespace = in.Namespace
			meta.Name = in.Name
			meta.Stage = in.Stage
			meta.Path = metaPathFromCoords(in.Group, in.Version, in.Resource, in.Namespace, in.Name)
		}
		if !fn(meta) {
			return
		}
	}
}

// metaPathFromCoords builds an apiserver-style path string from GVR
// coordinates for the /debug metadata projection. Pure string composition
// from the key inputs — touches no resolved body.
func metaPathFromCoords(group, version, resource, namespace, name string) string {
	var b strings.Builder
	if group == "" {
		b.WriteString("/api/")
		b.WriteString(version)
	} else {
		b.WriteString("/apis/")
		b.WriteString(group)
		b.WriteString("/")
		b.WriteString(version)
	}
	if namespace != "" {
		b.WriteString("/namespaces/")
		b.WriteString(namespace)
	}
	b.WriteString("/")
	b.WriteString(resource)
	if name != "" {
		b.WriteString("/")
		b.WriteString(name)
	}
	return b.String()
}

// ApistageEvictPressure is the Ship E (0.30.116) O6 budget signal: the
// ratio of api-stage entry evictions to api-stage entry stores. 0 means
// no api-stage churn (every stored stage entry is still resident or was
// never stored). A ratio approaching 1 means the maxEntries/maxBytes
// budget is too small for the N-identities × M-stages cardinality — the
// api-stage entries are being evicted as fast as they are written, so
// the key-swap buys nothing. The tester's 50K bench reads this to set
// the budget; the feature ships default-off until it is green.
func (s ResolvedCacheStats) ApistageEvictPressure() float64 {
	if s.ApistageStoreTotal == 0 {
		return 0
	}
	return float64(s.ApistageEvictTotal) / float64(s.ApistageStoreTotal)
}

// WidgetContentEvictPressure is the Ship G (0.30.16x) per-class budget
// signal — same shape as ApistageEvictPressure but for the widget
// content layer. A ratio approaching 1 means the LRU budget is too
// small for the navigation-tree-width entries the F2 walker populates.
func (s ResolvedCacheStats) WidgetContentEvictPressure() float64 {
	if s.WidgetContentStoreTotal == 0 {
		return 0
	}
	return float64(s.WidgetContentEvictTotal) / float64(s.WidgetContentStoreTotal)
}

// RAFullListEvictPressure is the Ship 4a (0.30.198) per-class budget signal —
// same shape as ApistageEvictPressure/WidgetContentEvictPressure but for the
// raFullList (RA full-list slice) layer: the ratio of raFullList entry
// evictions to raFullList entry stores. 0 means no raFullList churn (nothing
// stored yet, or every stored slice is still resident). A ratio approaching 1
// means the LRU/resident budget is too small for the full-list cardinality —
// the pinned-slice contract is being pushed out as fast as it is written.
func (s ResolvedCacheStats) RAFullListEvictPressure() float64 {
	if s.RAFullListStoreTotal == 0 {
		return 0
	}
	return float64(s.RAFullListEvictTotal) / float64(s.RAFullListStoreTotal)
}

// HitRate computes a simple cumulative hit rate. Returns 0 when there
// has been no traffic. Useful for the 5-min summary line and for the
// post-deploy falsifier (<50% hit rate = STOP per plan).
func (s ResolvedCacheStats) HitRate() float64 {
	total := s.HitTotal + s.MissTotal
	if total == 0 {
		return 0
	}
	return float64(s.HitTotal) / float64(total)
}

// evictUntilUnderCapsLocked drops tail entries (least recently used)
// until BOTH caps are satisfied. Must be called with mu held.
//
// Ship 4a (0.30.198) — PINNED (resident) entries are SKIPPED: the
// transient entry-count cap (maxEntries) and the transient byte cap
// (maxBytes) apply to TRANSIENT entries only, and the sweep walks from the
// LRU tail toward the front, skipping any pinned element, evicting the
// first TRANSIENT element it finds. Resident bytes/count have their OWN
// budget (maxResidentBytes), enforced at Put time by demote-to-transient —
// the sweep never evicts a pinned cell (the prewarm-coverage contract,
// feedback_zero_cold_navigations_hard_requirement). If EVERY remaining
// entry is pinned the sweep terminates (no transient victim) — bounded, no
// spin.
func (c *ResolvedCacheStore) evictUntilUnderCapsLocked() {
	transientEntries := c.order.Len() - c.residentEntries
	for transientEntries > c.maxEntries || c.curBytes > c.maxBytes {
		// Walk from the LRU tail toward the front to find the first
		// non-pinned victim. A pinned entry is skipped (it lives in the
		// resident region; LRU pressure must not touch it).
		var victim *list.Element
		for el := c.order.Back(); el != nil; el = el.Prev() {
			item := el.Value.(*lruItem)
			if item.entry != nil && item.entry.Pinned {
				continue
			}
			victim = el
			break
		}
		if victim == nil {
			// No transient victim left — every entry is pinned. Stop; the
			// transient caps cannot be satisfied further without touching
			// resident cells, which is forbidden.
			return
		}
		c.removeElementLocked(victim)
		c.evictLRUTotal.Add(1)
		transientEntries--
	}
}

// removeElementLocked drops el from order + index and adjusts the byte
// counter. Must be called with mu held.
//
// 0.30.8: also clears the dep-tracker reverse index for this key so
// dep records don't outlive the L1 entry. RemoveL1Key is itself
// lock-free (sync.Map ops) so calling it while holding c.mu is safe;
// the reverse path never re-enters the store.
func (c *ResolvedCacheStore) removeElementLocked(el *list.Element) {
	item := el.Value.(*lruItem)
	delete(c.index, item.key)
	c.order.Remove(el)
	// Ship 4a (0.30.198) — debit the correct budget. A pinned entry's bytes
	// live in the resident region; a transient entry's in curBytes.
	if item.entry != nil && item.entry.Pinned {
		c.curResidentBytes -= item.bytes
		if c.curResidentBytes < 0 {
			c.curResidentBytes = 0
		}
		c.residentEntries--
		if c.residentEntries < 0 {
			c.residentEntries = 0
		}
	} else {
		c.curBytes -= item.bytes
		if c.curBytes < 0 {
			// Defensive — should never happen with non-negative bytes.
			c.curBytes = 0
		}
	}
	// Ship E (0.30.116): count an api-stage eviction for the O6 pressure
	// metric. Classified off the dropped entry's CacheEntryClass.
	if isApistageEntry(item.entry) {
		c.apistageEvictTotal.Add(1)
	}
	// Ship G (0.30.16x): count a widget-content eviction for the same
	// per-class pressure signal.
	if isWidgetContentEntry(item.entry) {
		c.widgetContentEvictTotal.Add(1)
	}
	// Ship 4a (0.30.198): count a raFullList eviction (TTL/DELETE — a pinned
	// raFullList is skipped by the LRU sweep; an un-pinned one can be
	// LRU-evicted here).
	if isRAFullListEntry(item.entry) {
		c.raFullListEvictTotal.Add(1)
	}
	// Dep-tracker cleanup. Safe even when L1 is the only consumer
	// (Deps() is always non-nil); a no-op when no edges were ever
	// recorded for this key.
	Deps().RemoveL1Key(item.key)
}

// deleteForDep removes the entry under key, returning true if a live
// entry was found and dropped. Increments the DELETE-eviction counter.
// Used by DepTracker.OnDelete; production code MUST NOT call this
// path directly (DELETE eviction must flow through the dep tracker so
// the dep-record cleanup runs alongside the L1 drop).
//
// Performs a separate lock acquisition from any in-flight Get/Put —
// holds c.mu only for the duration of the index lookup + LRU detach.
// The dep tracker calls RemoveL1Key AFTER deleteForDep returns; since
// the entry is already gone from index/order, the second cleanup pass
// is a cheap no-op on the L1 side and does the actual dep-record
// removal on the dep side.
func (c *ResolvedCacheStore) deleteForDep(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	el, ok := c.index[key]
	if !ok {
		c.mu.Unlock()
		return false
	}
	// removeElementLocked also calls Deps().RemoveL1Key — but in this
	// path the dep tracker is mid-iteration over the reverse index
	// for THIS key, and LoadAndDelete inside RemoveL1Key is a no-op
	// the second time. We accept the trivial double-call rather than
	// branching the eviction body.
	item := el.Value.(*lruItem)
	delete(c.index, item.key)
	c.order.Remove(el)
	// Ship 4a (0.30.198) — debit the correct budget (a DELETE can evict a
	// pinned cell; the resident region is TTL/DELETE-evictable, only LRU-
	// pressure spares it).
	if item.entry != nil && item.entry.Pinned {
		c.curResidentBytes -= item.bytes
		if c.curResidentBytes < 0 {
			c.curResidentBytes = 0
		}
		c.residentEntries--
		if c.residentEntries < 0 {
			c.residentEntries = 0
		}
	} else {
		c.curBytes -= item.bytes
		if c.curBytes < 0 {
			c.curBytes = 0
		}
	}
	// Ship E (0.30.116): count an api-stage DELETE-eviction for the O6
	// pressure metric — same classification as removeElementLocked.
	apistage := isApistageEntry(item.entry)
	// Ship G (0.30.16x): same per-class DELETE classification for the
	// widget-content layer.
	widgetContent := isWidgetContentEntry(item.entry)
	// Ship 4a (0.30.198): same per-class DELETE classification for raFullList.
	raFullList := isRAFullListEntry(item.entry)
	c.mu.Unlock()
	c.evictDeleteTotal.Add(1)
	if apistage {
		c.apistageEvictTotal.Add(1)
	}
	if widgetContent {
		c.widgetContentEvictTotal.Add(1)
	}
	if raFullList {
		c.raFullListEvictTotal.Add(1)
	}
	return true
}

// startResolvedCacheSummary launches a single bounded goroutine that
// emits a `resolved_cache.summary` INFO line every N seconds. The
// goroutine self-suppresses on duplicate starts via resolvedCacheStarted.
// We never expose a stop method: the goroutine's lifetime is the
// process's lifetime and it does only constant work per tick.
func startResolvedCacheSummary(c *ResolvedCacheStore) {
	if c == nil {
		return
	}
	if !resolvedCacheStarted.CompareAndSwap(false, true) {
		return
	}
	every := time.Duration(intFromEnv(envResolvedCacheSummaryEvery, defaultResolvedCacheSummaryEverySeconds)) * time.Second
	if every <= 0 {
		every = time.Duration(defaultResolvedCacheSummaryEverySeconds) * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			s := c.Stats()
			d := Deps().Stats()
			r := refresherStatsSnapshot()
			dw := DepWatchStatsSnapshot()
			// Falsifier shape per plan §"Code-path falsifier" (0.30.8):
			//   resolved_cache.summary entries=N bytes=B hit_rate=0.NN
			//   evict_lru=X evict_delete=Y refresh_enqueued=M refresh_completed=K
			//   dep_map_size=D
			slog.Info("resolved_cache.summary",
				slog.String("subsystem", "cache"),
				slog.Int("entries", s.Entries),
				slog.Int64("bytes", s.Bytes),
				slog.Float64("hit_rate", s.HitRate()),
				slog.Uint64("evict_lru", s.EvictLRUTotal),
				slog.Uint64("evict_ttl", s.EvictTTLTotal),
				slog.Uint64("evict_delete", s.EvictDeleteTotal),
				slog.Uint64("refresh_enqueued", d.EnqueueUpdateTotal),
				slog.Uint64("refresh_completed", r.completed),
				slog.Uint64("refresh_failed", r.failed),
				slog.Uint64("refresh_retried", r.retried),
				slog.Uint64("refresh_dropped", r.dropped),
				slog.Uint64("refresh_skipped_stage_error", r.skippedStageError),
				slog.Int64("dep_map_size", d.TotalRecords),
				slog.Uint64("dep_record_total", d.RecordTotal),
				slog.Uint64("dep_record_dropped_cap", d.RecordDroppedCap),
				slog.Uint64("dep_record_dropped_no_key", d.RecordDroppedNoKey),
				slog.Uint64("dep_dirty_mark_total", d.DirtyMarkTotal),
				slog.Uint64("dep_add_dropped_pre_sync", dw.AddDroppedPreSync),
				slog.Uint64("dep_add_propagated", dw.AddPropagated),
				slog.Uint64("hit_total", s.HitTotal),
				slog.Uint64("miss_total", s.MissTotal),
				slog.Uint64("store_total", s.StoreTotal),
				slog.Int("max_entries", s.MaxEntries),
				slog.Int64("max_bytes", s.MaxBytes),
				// Ship E (0.30.116) O6 budget signal — AC-E7.
				slog.Uint64("apistage_store_total", s.ApistageStoreTotal),
				slog.Uint64("apistage_evict_total", s.ApistageEvictTotal),
				slog.Float64("apistage_evict_pressure", s.ApistageEvictPressure()),
				slog.Bool("apistage_enabled", ApistageL1Enabled()),
				// Ship G (0.30.16x) — AC-G.1 / AC-G.12 / AC-G.14 surface.
				slog.Uint64("widget_content_store_total", s.WidgetContentStoreTotal),
				slog.Uint64("widget_content_evict_total", s.WidgetContentEvictTotal),
				slog.Float64("widget_content_evict_pressure", s.WidgetContentEvictPressure()),
				slog.Bool("widget_content_enabled", WidgetContentL1Enabled()),
				// Ship 4a (0.30.198) — raFullList + resident-region surface
				// (per feedback_measurement_use_expvar_not_log_tails — also
				// in /debug/vars via the same Stats snapshot). resident_entries
				// is the prewarm-coverage signal; resident_demote_total > 0
				// means the resident budget overflowed or pinning is disabled.
				slog.Uint64("ra_full_list_store_total", s.RAFullListStoreTotal),
				slog.Uint64("ra_full_list_evict_total", s.RAFullListEvictTotal),
				slog.Float64("ra_full_list_evict_pressure", s.RAFullListEvictPressure()),
				slog.Int("resident_entries", s.ResidentEntries),
				slog.Int64("resident_bytes", s.ResidentBytes),
				slog.Int64("max_resident_bytes", s.MaxResidentBytes),
				slog.Uint64("resident_pin_total", s.ResidentPinTotal),
				slog.Uint64("resident_demote_total", s.ResidentDemoteTotal),
			)
		}
	}()
}

// resetResolvedCacheForTest tears the singleton down so each test sees
// a clean cache. Exported only via the *_test.go shim — production
// code MUST NOT call this.
func resetResolvedCacheForTest() {
	resolvedCacheInstance = nil
	resolvedCacheOnce = sync.Once{}
	resolvedCacheStarted.Store(false)
	// 1.12.4 §7b — drop the observability handle too, else a test that
	// tears the singleton down still reports the previous store's stats
	// through /debug/vars and OTLP.
	resolvedCachePublished.Store(nil)
}

// ResetResolvedCacheForTest is the exported variant for cross-package
// tests (e.g. internal/handlers/dispatchers' Ship C falsifier).
// Production code MUST NOT call it.
func ResetResolvedCacheForTest() {
	resetResolvedCacheForTest()
}

// DeleteForTest removes key from the resolved cache. Cross-package
// test-only seam — Ship C's resurrect-guard test emulates a DELETE-evict
// landing mid-refresh. Production eviction MUST flow through the dep
// tracker (deleteForDep) so dep records are cleaned alongside; this
// helper deliberately bypasses that and is therefore TEST-ONLY.
func (c *ResolvedCacheStore) DeleteForTest(key string) {
	if c == nil {
		return
	}
	c.deleteForDep(key)
}

// intFromEnv parses an env var as int with a default fallback. We
// intentionally accept any non-int value as "use default" with no
// logging — env-knob misconfiguration is a deploy issue and the test
// suite covers correct parses.
func intFromEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// CatalogUnservableTTL returns the configured R1 Layer 2 (#36) bounded-
// staleness backstop duration, or 0 when DISABLED (the default — the env
// var unset/0/invalid). When > 0, an apistage entry stored while its GVR is
// not servable should carry this as ResolvedEntry.TTLOverride. Read at Put
// time (cheap os.Getenv + Atoi); kept here next to the resolved-cache TTL
// knobs since it governs per-entry expiry in this store.
func CatalogUnservableTTL() time.Duration {
	s := intFromEnv(envCatalogUnservableTTLSeconds, 0)
	if s <= 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// UAFResolvedTTL returns the #118 (d) interim short-TTL stamped on resolved
// cells whose RESTAction declares a userAccessFilter stage, or 0 when DISABLED
// (the default — env unset/0/invalid). When > 0, a UAF-bearing entry should
// carry this as ResolvedEntry.TTLOverride at BOTH Put sites (customer dispatch
// AND refresher re-Put — #118 C-118-6) so its RBAC-staleness window is capped
// even under CreatedAt-sliding data-plane refresh churn. Read at Put time
// (cheap os.Getenv + Atoi), mirroring CatalogUnservableTTL. effectiveTTLLocked
// already honours the SHORTER of TTLOverride and the store ttl, so a UAF TTL
// only ever TIGHTENS the bound. INTERIM ONLY: caps the window, does not fix the
// key (#118 (c) is the durable per-user-RBAC-subgen key fix).
func UAFResolvedTTL() time.Duration {
	s := intFromEnv(envUAFResolvedTTLSeconds, 0)
	if s <= 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// ResolvedCacheTTL returns the effective standard resolved-cache entry TTL —
// the SAME value newResolvedCache is constructed with (RESOLVED_CACHE_TTL_SECONDS
// env, default defaultResolvedCacheTTLSeconds=3600s). Exported so the #102 c1
// keep-warm sweep can derive its cadence (TTL×3/4) from the SAME source that
// governs expiry — no separate cadence knob (feedback_no_special_cases). A
// non-positive env value falls back to the default (matching newResolvedCache's
// own <=0 guard at :605), so the accessor never returns 0 and the sweep cadence
// is always well-defined.
func ResolvedCacheTTL() time.Duration {
	s := intFromEnv(envResolvedCacheTTLSeconds, defaultResolvedCacheTTLSeconds)
	if s <= 0 {
		s = defaultResolvedCacheTTLSeconds
	}
	return time.Duration(s) * time.Second
}

func int64FromEnv(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// boolFromEnv parses an env var as a bool with a default fallback.
// Recognises the canonical false set ("false", "0", "no") and the
// canonical true set ("true", "1", "yes"); any unset or unrecognised
// value returns def. Used by R4's RESOLVER_COMPOSITION_STREAMING_LIST
// (default true) — env-knob misconfiguration is a deploy issue, so an
// unrecognised value falls back silently to the default.
func boolFromEnv(key string, def bool) bool {
	switch os.Getenv(key) {
	case "false", "0", "no":
		return false
	case "true", "1", "yes":
		return true
	default:
		return def
	}
}

// positiveIntFromEnv parses an env var as an int that MUST be >= 1,
// falling back to def with a VISIBLE WARN slog on either (a) a parse
// failure or (b) an out-of-range (<= 0) value. This is the
// range-validating sibling of intFromEnv (#278-C / generalizes #154):
// intFromEnv silently swallows malformed values, and the historic
// `if n <= 0 { n = def }` clamps at the call sites were silent too — a
// misconfigured RESOLVED_CACHE_REFRESHER_PARALLELISM=-1 or =garbage
// became the default with no operator-visible signal. The absence of
// that log line WAS the #154 failure-mode.
//
// Behaviour preserved: the SAME def applies in the SAME cases the old
// `intFromEnv(...)` + `if n <= 0` clamp applied it. Only the WARN is
// new. Used by the parallelism / queue-len / worker-count knobs, all of
// which require a strictly-positive value.
func positiveIntFromEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("cache.env.parse_rejected",
			slog.String("subsystem", "cache"),
			slog.String("key", key),
			slog.String("value", v),
			slog.Int("default_applied", def),
			slog.String("note", "unparseable as int — falling back to default"),
		)
		return def
	}
	if n <= 0 {
		slog.Warn("cache.env.out_of_range",
			slog.String("subsystem", "cache"),
			slog.String("key", key),
			slog.Int("value", n),
			slog.Int("default_applied", def),
			slog.String("note", "value must be >= 1 — falling back to default"),
		)
		return def
	}
	return n
}

// int64BytesFromEnv parses an env var as an int64 byte-count, falling
// back to def with a VISIBLE WARN slog on either a parse failure or a
// negative value. Used by the byte-cap knobs (RESOLVED_CACHE_MAX_BYTES,
// RESOLVED_CACHE_MAX_RESIDENT_BYTES, DEPS_MAX_RECORDS).
//
// #154 ORIGINAL REPORT: a scientific-notation value such as "5e8" failed
// strconv.ParseInt and silently truncated to the 512MiB default with no
// log. This variant adds a strconv.ParseFloat fallback so "5e8" /
// "1.5e9" parse to their integer byte-count, AND emits a WARN whenever
// the value is rejected outright.
//
// RANGE CONTRACT: zero is ACCEPTED (it is a VALID kill-switch for
// RESOLVED_CACHE_MAX_RESIDENT_BYTES — see ResolvedCache() at the
// maxResidentBytes wire site — and the downstream `<= 0` guards in
// newResolvedCache already map a 0 max-bytes/max-entries onto the
// positive default). Only a NEGATIVE value is out-of-range; it WARNs and
// returns def. This preserves which defaults apply — only rejection is
// now visible.
func int64BytesFromEnv(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			slog.Warn("cache.env.out_of_range",
				slog.String("subsystem", "cache"),
				slog.String("key", key),
				slog.Int64("value", n),
				slog.Int64("default_applied", def),
				slog.String("note", "byte-count must be >= 0 — falling back to default"),
			)
			return def
		}
		return n
	}
	// strconv.ParseInt failed — try scientific / float notation (#154).
	f, ferr := strconv.ParseFloat(v, 64)
	if ferr != nil {
		slog.Warn("cache.env.parse_rejected",
			slog.String("subsystem", "cache"),
			slog.String("key", key),
			slog.String("value", v),
			slog.Int64("default_applied", def),
			slog.String("note", "unparseable as int64 byte-count (incl. scientific notation) — falling back to default"),
		)
		return def
	}
	if f < 0 {
		slog.Warn("cache.env.out_of_range",
			slog.String("subsystem", "cache"),
			slog.String("key", key),
			slog.String("value", v),
			slog.Int64("default_applied", def),
			slog.String("note", "byte-count must be >= 0 — falling back to default"),
		)
		return def
	}
	return int64(f)
}
