package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	rbaclisters "k8s.io/client-go/listers/rbac/v1"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
)

// rbacDebounceWindow is how long we wait to coalesce rapid RBAC change events
// before triggering a single cache invalidation. This prevents a storm of
// events (e.g. from thousands of leftover benchmark ClusterRoleBindings during
// the informer's initial LIST sync) from causing continuous L1 purges.
const rbacDebounceWindow = 2 * time.Second

// RBACPrewarmer is the minimal contract RBACWatcher needs from the
// dispatchers PrewarmWorkerPool to fan out cohort prewarm on binding-ADD
// events. Kept narrow so internal/cache does not import dispatchers.
//
// Implemented by *dispatchers.PrewarmWorkerPool.
type RBACPrewarmer interface {
	// EnqueueForCohort enqueues prewarm jobs for the given usernames
	// as a single cohort fan-out. Returns (accepted, dropped). Must be
	// non-blocking for the caller (the binding-ADD informer goroutine).
	EnqueueForCohort(ctx context.Context, usernames []string) (accepted, dropped int)
}

type RBACWatcher struct {
	cache Cache
	rc    *rest.Config

	mu      sync.Mutex
	pending bool // true when a debounced invalidation is scheduled
	timer   *time.Timer

	// Cohort-prewarm coalescing state. Distinct from `mu` (invalidation
	// debounce) because ADD events trigger fan-outs that should not
	// reset the invalidation timer or be reset by it. The pendingCohort
	// flag + cohortTimer + accumulated subject sets together collapse a
	// burst of binding-ADD events (helm rollout, initial-LIST) into one
	// debounced fan-out covering the union of affected subjects.
	cohortMu       sync.Mutex
	pendingCohort  bool
	cohortTimer    *time.Timer
	cohortUserSet  map[string]struct{} // accumulated User subjects
	cohortGroupSet map[string]struct{} // accumulated Group subjects (expanded at fire time)
	prewarmer      atomic.Value         // RBACPrewarmer (nil-typed safe via Load)

	// Listers for binding identity computation and local RBAC evaluation.
	// Set after Start().
	rbLister   rbaclisters.RoleBindingLister
	crbLister  rbaclisters.ClusterRoleBindingLister
	roleLister rbaclisters.RoleLister
	crLister   rbaclisters.ClusterRoleLister
	synced     bool

	// identityCache maps username → binding identity hash (from ComputeBindingIdentity).
	// Avoids re-listing all bindings on every HTTP request. Cleared on broad
	// RBAC changes; individual entries deleted on per-user binding invalidation.
	identityCache sync.Map

	// evalCache memoises EvaluateRBAC decisions keyed by
	//   "username|verb|group|resource|namespace" → bool
	//
	// Q-OOM-FIX (v0.25.313 RCA, 2026-05-08) — heap profiling traced 56% of
	// cumulative allocations (~4.15 TB) to crbLister.List(labels.Everything())
	// inside EvaluateRBAC. Each call materialises the entire CRB set into a
	// fresh slice; under the no-op UPDATE storm (~4.5/s sustained) and
	// per-item RBAC checks at compositions-list scale this dominated the heap.
	//
	// Architect re-review (2026-05-08): bounded with LRU eviction at 200K
	// entries to prevent the cache itself becoming a leak. Worst-case
	// production density (1000 users × 10 verbs × 50 GRs × 100 namespaces
	// = 50M keys × ~120 B = ~6 GB) is dramatically larger than typical
	// working sets — 200K covers ~10× normal load while capping the
	// resident bytes at ~24 MB.
	//
	// Invariants:
	//   - Invalidated wholesale from invalidate() (broad RBAC change).
	//   - Invalidated per-user from purgeUserCacheData() (binding-scoped change).
	//   - Both paths fire under the existing rbacDebounceWindow, so cohort-
	//     prewarm and HTTP cache stay coherent with the eval cache.
	evalCache *evalLRU
}

// evalCacheCap bounds the EvaluateRBAC decision cache at 200K entries
// (~24 MB resident at the typical key+bool pair size). See the doc-comment
// on RBACWatcher.evalCache for the sizing rationale.
const evalCacheCap = 200_000

func NewRBACWatcher(c Cache, rc *rest.Config) *RBACWatcher {
	return &RBACWatcher{
		cache:     c,
		rc:        rc,
		evalCache: newEvalLRU(evalCacheCap),
	}
}

func (rw *RBACWatcher) Start(ctx context.Context) error {
	clientset, err := kubernetes.NewForConfig(rw.rc)
	if err != nil {
		return err
	}
	factory := informers.NewSharedInformerFactory(clientset, 0)

	// Roles/ClusterRoles: react only to Update/Delete. Skipping AddFunc avoids
	// the initial-LIST storm where every pre-existing RBAC object fires an ADD
	// event on pod startup — with thousands of resources this would flood the
	// cache with back-to-back invalidations for several minutes.
	rbacHandler := func(obj any) { rw.scheduleInvalidate(ctx) }
	bindingHandler := func(obj any) { rw.scheduleInvalidateFromBinding(ctx, obj) }

	// Q-COHORT-PREWARM (v0.25.312) — RoleBinding/ClusterRoleBinding ADD events
	// trigger COHORT PREWARM (writes only; no invalidation). Per the
	// architect spec, ADD strictly EXPANDS visibility — the user's first
	// request would otherwise pay the cold tail (cyberjoker 22.5s on
	// ledger row 3). The 2s debounce + subject-union coalescing collapses
	// runtime ADDs (kubectl create rolebinding mid-flight) into ONE fan-out.
	//
	// v0.25.313 — gate AddFunc on `rw.synced` so initial-LIST ADDs are
	// IGNORED. R5 startup-prewarm already covers the BIDs visible at
	// startup. The cohort-prewarm hook is for RUNTIME ADDs only — when
	// a user gains visibility AFTER startup. Without this gate, two
	// informer initial-LISTs (RB + CRB) each fire 1004 fan-outs at boot,
	// pushing peak heap > 14 GiB and triggering node-eviction (observed
	// 2026-05-07 22:55 UTC on snowplow 0.25.312 / chart 0.25.314).
	bindingAddHandler := func(obj any) {
		if !rw.synced {
			return
		}
		rw.scheduleCohortPrewarmFromBinding(ctx, obj)
	}

	_, _ = factory.Rbac().V1().Roles().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, n any) { rbacHandler(n) }, DeleteFunc: rbacHandler,
	})
	_, _ = factory.Rbac().V1().ClusterRoles().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, n any) { rbacHandler(n) }, DeleteFunc: rbacHandler,
	})
	_, _ = factory.Rbac().V1().RoleBindings().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    bindingAddHandler,
		UpdateFunc: func(_, n any) { bindingHandler(n) },
		DeleteFunc: bindingHandler,
	})
	_, _ = factory.Rbac().V1().ClusterRoleBindings().Informer().AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    bindingAddHandler,
		UpdateFunc: func(_, n any) { bindingHandler(n) },
		DeleteFunc: bindingHandler,
	})
	// Store listers for binding identity computation and local RBAC evaluation.
	rw.rbLister = factory.Rbac().V1().RoleBindings().Lister()
	rw.crbLister = factory.Rbac().V1().ClusterRoleBindings().Lister()
	rw.roleLister = factory.Rbac().V1().Roles().Lister()
	rw.crLister = factory.Rbac().V1().ClusterRoles().Lister()

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	rw.synced = true
	slog.Info("rbac-watcher: started informers")
	return nil
}

// scheduleInvalidate debounces RBAC invalidation: multiple events within
// rbacDebounceWindow collapse into a single invalidation call.
func (rw *RBACWatcher) scheduleInvalidate(ctx context.Context) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.pending {
		rw.timer.Reset(rbacDebounceWindow)
		return
	}
	rw.pending = true
	rw.timer = time.AfterFunc(rbacDebounceWindow, func() {
		rw.mu.Lock()
		rw.pending = false
		rw.mu.Unlock()
		rw.invalidate(ctx)
	})
}

// scheduleInvalidateFromBinding debounces binding-specific invalidation.
// When a binding change affects a specific user we still debounce to avoid
// rapid-fire updates, but we pass the object through for targeted invalidation.
func (rw *RBACWatcher) scheduleInvalidateFromBinding(ctx context.Context, obj any) {
	subjects, ok := extractSubjects(obj)
	if !ok || len(subjects) == 0 {
		rw.scheduleInvalidate(ctx)
		return
	}
	// For targeted (per-user) binding invalidations, still debounce but do it
	// immediately via a tiny window so user-specific changes aren't delayed too long.
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.pending {
		rw.timer.Reset(rbacDebounceWindow)
		return
	}
	rw.pending = true
	capturedObj := obj
	rw.timer = time.AfterFunc(rbacDebounceWindow, func() {
		rw.mu.Lock()
		rw.pending = false
		rw.mu.Unlock()
		rw.invalidateFromBinding(ctx, capturedObj)
	})
}

// SetPrewarmer registers the RBACPrewarmer used to fan out cohort
// prewarm jobs on binding-ADD events. Wired in main.go after the
// PrewarmWorkerPool is constructed; safe to call from any goroutine and
// safe to be left unset (binding-ADD path becomes a no-op log line).
func (rw *RBACWatcher) SetPrewarmer(p RBACPrewarmer) {
	if p == nil {
		rw.prewarmer.Store((RBACPrewarmer)(nil))
		return
	}
	rw.prewarmer.Store(p)
}

// scheduleCohortPrewarmFromBinding accumulates the User/Group subjects
// of a binding-ADD event and schedules a single debounced cohort
// fan-out. Multiple ADDs within rbacDebounceWindow merge into one
// dispatch keyed by union-of-subjects — this is what makes the initial-
// LIST storm tractable at ~50 RBs + 1004 active users.
//
// The fan-out runs `prewarmer.EnqueueForCohort(union of users)` with
// Group subjects expanded to ALL active users (Option A from architect
// §2.4). The pool's inflightUsers + L1 Exists short-circuits make the
// per-user cost a few hash lookups for users whose BID didn't change,
// and a real prewarm walk for users whose BID did change.
func (rw *RBACWatcher) scheduleCohortPrewarmFromBinding(ctx context.Context, obj any) {
	subjects, ok := extractSubjects(obj)
	if !ok || len(subjects) == 0 {
		return
	}
	rw.cohortMu.Lock()
	if rw.cohortUserSet == nil {
		rw.cohortUserSet = make(map[string]struct{})
	}
	if rw.cohortGroupSet == nil {
		rw.cohortGroupSet = make(map[string]struct{})
	}
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			if s.Name != "" {
				rw.cohortUserSet[s.Name] = struct{}{}
			}
		case rbacv1.GroupKind:
			if s.Name != "" {
				rw.cohortGroupSet[s.Name] = struct{}{}
			}
		}
	}
	if rw.pendingCohort {
		rw.cohortTimer.Reset(rbacDebounceWindow)
		rw.cohortMu.Unlock()
		return
	}
	rw.pendingCohort = true
	rw.cohortTimer = time.AfterFunc(rbacDebounceWindow, func() {
		rw.fireCohortPrewarm(ctx)
	})
	rw.cohortMu.Unlock()
}

// fireCohortPrewarm drains the accumulated subject sets, expands Group
// subjects against the active-users index, and dispatches a single
// EnqueueForCohort call. Runs in the cohortTimer goroutine; non-blocking
// for the informer.
func (rw *RBACWatcher) fireCohortPrewarm(ctx context.Context) {
	rw.cohortMu.Lock()
	users := rw.cohortUserSet
	groups := rw.cohortGroupSet
	rw.cohortUserSet = nil
	rw.cohortGroupSet = nil
	rw.pendingCohort = false
	rw.cohortMu.Unlock()

	prewarmer := rw.loadPrewarmer()
	if prewarmer == nil {
		slog.Debug("rbac-watcher: cohort prewarm skipped (no prewarmer wired)",
			slog.Int("users", len(users)),
			slog.Int("groups", len(groups)),
		)
		return
	}

	// Build the affected-username set:
	//   1. Direct User subjects.
	//   2. If any Group subject was observed, expand to ALL active users
	//      (Option A — architect §2.4). Per-user EnqueueForUser is cheap
	//      for users whose BID didn't change (L1 Exists hits short-circuit
	//      the walk); only users in the new group pay a real prewarm.
	affected := make(map[string]struct{}, len(users))
	for u := range users {
		affected[u] = struct{}{}
	}
	if len(groups) > 0 {
		active, err := rw.cache.SMembers(ctx, ActiveUsersKey)
		if err != nil {
			slog.Warn("rbac-watcher: cohort prewarm — SMembers(ActiveUsersKey) failed; using direct subjects only",
				slog.Any("err", err),
			)
		} else {
			for _, u := range active {
				if u != "" {
					affected[u] = struct{}{}
				}
			}
		}
	}
	if len(affected) == 0 {
		slog.Debug("rbac-watcher: cohort prewarm — no affected users after expansion",
			slog.Int("users", len(users)),
			slog.Int("groups", len(groups)),
		)
		return
	}

	usernames := make([]string, 0, len(affected))
	for u := range affected {
		usernames = append(usernames, u)
	}

	accepted, dropped := prewarmer.EnqueueForCohort(ctx, usernames)
	slog.Info("rbac-watcher: cohort prewarm fanned out on binding ADD",
		slog.Int("users_total", len(usernames)),
		slog.Int("user_subjects", len(users)),
		slog.Int("group_subjects", len(groups)),
		slog.Int("accepted", accepted),
		slog.Int("dropped", dropped),
	)
}

// loadPrewarmer reads the atomic.Value, returning nil if unset or
// stored as a typed-nil interface.
func (rw *RBACWatcher) loadPrewarmer() RBACPrewarmer {
	v := rw.prewarmer.Load()
	if v == nil {
		return nil
	}
	p, ok := v.(RBACPrewarmer)
	if !ok || p == nil {
		return nil
	}
	return p
}

// invalidate uses the active-users set for targeted RBAC invalidation.
func (rw *RBACWatcher) invalidate(ctx context.Context) {
	// Clear all cached identities on broad RBAC change so the next
	// request re-computes the binding identity from the updated listers.
	rw.identityCache.Range(func(key, _ any) bool {
		rw.identityCache.Delete(key)
		return true
	})

	// Q-OOM-FIX — drop the EvaluateRBAC decision cache wholesale on broad
	// RBAC changes. Full purge (vs. per-key delete) is fine because RBAC
	// changes are rare and the cache repopulates lazily on next request.
	if rw.evalCache != nil {
		rw.evalCache.Purge()
	}

	usernames, err := rw.cache.SMembers(ctx, ActiveUsersKey)
	if err != nil || len(usernames) == 0 {
		rw.invalidateAllRBAC(ctx)
		return
	}
	total := 0
	for _, username := range usernames {
		total += rw.purgeUserCacheData(ctx, username)
	}
	slog.Debug("rbac-watcher: invalidated RBAC+L1 for active users after role change",
		slog.Int("users", len(usernames)), slog.Int("keys", total))
}

func (rw *RBACWatcher) invalidateAllRBAC(ctx context.Context) {
	// With HASH-based RBAC, each user has one hash key: snowplow:rbac:{username}.
	// SCAN for those hashes and DEL them.
	keys, err := rw.cache.ScanKeys(ctx, "snowplow:rbac:*")
	if err != nil || len(keys) == 0 {
		return
	}
	_ = rw.cache.Delete(ctx, keys...)
	slog.Debug("rbac-watcher: invalidated all RBAC cache entries (fallback scan)",
		slog.Int("count", len(keys)))
}

func (rw *RBACWatcher) invalidateFromBinding(ctx context.Context, obj any) {
	subjects, ok := extractSubjects(obj)
	if !ok || len(subjects) == 0 {
		rw.invalidate(ctx)
		return
	}
	usernames := []string{}
	hasGroups := false
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			usernames = append(usernames, s.Name)
		case rbacv1.GroupKind, rbacv1.ServiceAccountKind:
			hasGroups = true
		}
	}
	if hasGroups {
		rw.invalidate(ctx)
		return
	}
	for _, username := range usernames {
		rw.purgeUserCacheData(ctx, username)
	}
}

// purgeUserCacheData deletes RBAC and L1 (resolved) cache entries for a
// single user. Handles both username-keyed and binding-identity-keyed entries:
//   - Purges RBAC hash and L1 resolved index for the username directly
//   - If the RBAC watcher can compute a binding identity for the user,
//     also purges the binding-identity-keyed RBAC hash and L1 resolved index
//
// Returns the total number of keys deleted.
//
// Q-RBACC-L2-1 architect-review CONCERN-2 (2026-05-05) — ORDERING is
// load-bearing. apiref reads L2 BEFORE L1 (apiref/resolve.go: L2Get
// runs before c.GetRaw). If we evicted L1 first, a microsecond
// window would open where:
//
//   1. L1 evicted.
//   2. Concurrent apiref read finds STALE L2, serves stale bytes
//      against the OLD binding (G10 violation — cyberjoker post-CRB
//      sees old-binding refiltered output).
//   3. L2 evicted (window closes).
//
// Fix: evict L2 FIRST, then L1. Concurrent reads see L2 miss → fall
// through to L1 → if L1 still present, refilter against (still-cached)
// old wrapper but with the new identity in ctx — which produces the
// CORRECT new-binding refilter, because RefilterRESTAction recomputes
// per-user against the cached unfiltered ProtectedDict. Then L1
// evicts; subsequent reads MISS at L1 too and re-resolve. Either path
// returns new-binding bytes; no stale-L2 window.
func (rw *RBACWatcher) purgeUserCacheData(ctx context.Context, username string) int {
	total := 0

	// Q-OOM-FIX — drop EvaluateRBAC entries for this user. Key shape is
	// "username|verb|group|resource|namespace" so a HasPrefix match on
	// "username|" cleanly scopes the eviction. Concurrent EvaluateRBAC
	// after this returns will just miss the cache and re-populate.
	if rw.evalCache != nil {
		rw.evalCache.RemoveWithPrefix(username + "|")
	}

	// Phase 1: snapshot identities (no cache mutation yet).
	//
	// Load old identity BEFORE clearing the in-memory cache so we can
	// purge stale entries keyed under the previous binding identity.
	// Compute new identity ahead of any cache mutation so the L2 evict
	// step (Phase 2) has the full identity set without ordering hazards.
	var oldBid string
	if v, ok := rw.identityCache.Load(username); ok {
		oldBid = v.(string)
	}
	rw.identityCache.Delete(username)
	newBid := rw.ComputeBindingIdentity(username, nil)

	// Phase 2: evict L2 FIRST (architect CONCERN-2). Order matters
	// because apiref reads L2 before L1 — flushing L1 first would
	// expose a stale-L2 read window.
	//
	// EvictL2ForIdentity is no-op for unknown identities (the byIdentity
	// reverse-index lookup misses, returning 0). Safe to call with
	// empty / unknown values; cheap.
	if oldBid != "" {
		_ = EvictL2ForIdentity(ctx, oldBid)
	}
	if newBid != "" {
		_ = EvictL2ForIdentity(ctx, newBid)
	}
	_ = EvictL2ForIdentity(ctx, username)

	// Phase 3: evict L1 + RBAC hash entries.

	// Purge username-keyed entries (backward compat / migration).
	_ = rw.cache.DeleteUserRBAC(ctx, username)
	total++

	idxKey := UserResolvedIndexKey(username)
	keys, _ := rw.cache.SMembers(ctx, idxKey)
	if len(keys) == 0 {
		keys, _ = rw.cache.ScanKeys(ctx, "snowplow:resolved:"+username+":*")
	}
	if len(keys) > 0 {
		_ = rw.cache.Delete(ctx, append(keys, idxKey)...)
		total += len(keys)
	}

	// Purge old binding identity entries (cached before the RBAC change).
	// The old identity may differ from the new one after a binding update.
	if oldBid != "" && oldBid != username {
		_ = rw.cache.DeleteUserRBAC(ctx, oldBid)
		total++
		oldIdx := UserResolvedIndexKey(oldBid)
		oldKeys, _ := rw.cache.SMembers(ctx, oldIdx)
		if len(oldKeys) == 0 {
			oldKeys, _ = rw.cache.ScanKeys(ctx, "snowplow:resolved:"+oldBid+":*")
		}
		if len(oldKeys) > 0 {
			_ = rw.cache.Delete(ctx, append(oldKeys, oldIdx)...)
			total += len(oldKeys)
		}
	}

	// Also purge NEW binding-identity-keyed entries. After a binding change,
	// the informer has the NEW state, so ComputeBindingIdentity returns
	// the new identity. We purge it because the RBAC decisions cached
	// under the new identity may have been stale (computed before the
	// binding change propagated).
	if newBid != "" && newBid != username && newBid != oldBid {
		_ = rw.cache.DeleteUserRBAC(ctx, newBid)
		total++
		bidIdx := UserResolvedIndexKey(newBid)
		bidKeys, _ := rw.cache.SMembers(ctx, bidIdx)
		if len(bidKeys) == 0 {
			bidKeys, _ = rw.cache.ScanKeys(ctx, "snowplow:resolved:"+newBid+":*")
		}
		if len(bidKeys) > 0 {
			_ = rw.cache.Delete(ctx, append(bidKeys, bidIdx)...)
			total += len(bidKeys)
		}
	}

	return total
}

// CachedBindingIdentity returns a cached binding identity for the user. On
// cache hit it skips the full lister scan. On miss it delegates to
// ComputeBindingIdentity and stores the result. Empty identities (informer
// not synced) are not cached.
//
// The cache key includes groups because the same username can appear with
// different group sets (prewarm extracts groups from X.509 cert, HTTP gets
// them from JWT). Without groups in the key, the first caller poisons the
// cache for all subsequent callers.
func (rw *RBACWatcher) CachedBindingIdentity(username string, groups []string) string {
	sort.Strings(groups)
	cacheKey := username + "\x00" + strings.Join(groups, ",")
	if v, ok := rw.identityCache.Load(cacheKey); ok {
		return v.(string)
	}
	bid := rw.ComputeBindingIdentity(username, groups)
	if bid != "" {
		rw.identityCache.Store(cacheKey, bid)
	}
	return bid
}

// ComputeBindingIdentity returns a short hash identifying the effective RBAC
// permissions for a user. Users with the same set of RoleBindings and
// ClusterRoleBindings produce the same identity, enabling shared L1 cache
// entries across users with identical permissions.
//
// The identity is the first 12 hex chars of SHA-256(sorted binding names).
// Returns "" if the informers have not synced yet.
func (rw *RBACWatcher) ComputeBindingIdentity(username string, groups []string) string {
	if !rw.synced {
		return ""
	}

	subjects := make(map[string]bool, 1+len(groups))
	subjects["User:"+username] = true
	for _, g := range groups {
		subjects["Group:"+g] = true
	}

	var bindingNames []string

	// ClusterRoleBindings (cluster-scoped).
	if crbs, err := rw.crbLister.List(labels.Everything()); err == nil {
		for _, crb := range crbs {
			if matchesSubjects(crb.Subjects, subjects) {
				bindingNames = append(bindingNames, "crb:"+crb.Name)
			}
		}
	}

	// RoleBindings (namespace-scoped).
	if rbs, err := rw.rbLister.List(labels.Everything()); err == nil {
		for _, rb := range rbs {
			if matchesSubjects(rb.Subjects, subjects) {
				bindingNames = append(bindingNames, "rb:"+rb.Namespace+"/"+rb.Name)
			}
		}
	}

	if len(bindingNames) == 0 {
		return "no-bindings"
	}

	sort.Strings(bindingNames)
	h := sha256.New()
	for _, name := range bindingNames {
		h.Write([]byte(name))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// matchesSubjects returns true if any subject in the binding matches the
// user's identity (username or group membership).
func matchesSubjects(bindingSubjects []rbacv1.Subject, userSubjects map[string]bool) bool {
	for _, s := range bindingSubjects {
		switch s.Kind {
		case rbacv1.UserKind:
			if userSubjects["User:"+s.Name] {
				return true
			}
		case rbacv1.GroupKind:
			if userSubjects["Group:"+s.Name] {
				return true
			}
		}
	}
	return false
}

func extractSubjects(obj any) ([]rbacv1.Subject, bool) {
	switch b := obj.(type) {
	case *rbacv1.RoleBinding:
		return b.Subjects, true
	case *rbacv1.ClusterRoleBinding:
		return b.Subjects, true
	case k8scache.DeletedFinalStateUnknown:
		return extractSubjects(b.Obj)
	}
	return nil, false
}

// EvaluateRBAC performs in-memory RBAC rule evaluation using the informer
// cache, avoiding any K8s API call. It follows the K8s RBAC authorizer
// logic: ClusterRoleBindings are checked first, then namespace-scoped
// RoleBindings. Returns true on the first matching allow rule (short-circuit).
//
// Falls back to false (deny) if informers have not synced yet. This is safe
// because the caller keeps the SSAR path as a fallback when EvaluateRBAC
// is not available (RBACWatcher not in context).
func (rw *RBACWatcher) EvaluateRBAC(username string, groups []string, verb string, gr schema.GroupResource, namespace string) bool {
	if !rw.synced {
		return false
	}

	// Q-OOM-FIX cache short-circuit. Key shape:
	//   "username|verb|group|resource|namespace"
	//
	// Groups are intentionally NOT part of the key. The K8s authorizer's
	// allow decision is the union of all matching subjects (User + Group),
	// so a per-(user,verb,gr,ns) decision changes only when a binding
	// affecting this user changes — and our two invalidation hooks
	// (invalidate / purgeUserCacheData) cover exactly that. Including
	// groups would explode key cardinality (cyberjoker JWT + cert paths
	// pass disjoint group sets for the same user) and hurt hit rate.
	cacheKey := evalCacheKey(username, verb, gr, namespace)
	if rw.evalCache != nil {
		if v, ok := rw.evalCache.Get(cacheKey); ok {
			return v
		}
	}

	allowed := rw.evaluateRBACUncached(username, groups, verb, gr, namespace)
	if rw.evalCache != nil {
		rw.evalCache.Add(cacheKey, allowed)
	}
	return allowed
}

// evalCacheKey derives the lookup key for evalCache. Kept package-private
// and inlined-friendly so unit tests can exercise the same key shape.
func evalCacheKey(username, verb string, gr schema.GroupResource, namespace string) string {
	return username + "|" + verb + "|" + gr.Group + "|" + gr.Resource + "|" + namespace
}

// evaluateRBACUncached runs the actual lister scan. Extracted so the hot
// path in EvaluateRBAC stays small (compiler can keep the cache lookup
// inline-able).
func (rw *RBACWatcher) evaluateRBACUncached(username string, groups []string, verb string, gr schema.GroupResource, namespace string) bool {
	// Build subject identity set for matching.
	subjects := make(map[string]bool, 1+len(groups))
	subjects["User:"+username] = true
	for _, g := range groups {
		subjects["Group:"+g] = true
	}

	// Phase 1: Check ClusterRoleBindings (K8s evaluates CRBs before RBs).
	if crbs, err := rw.crbLister.List(labels.Everything()); err == nil {
		for _, crb := range crbs {
			if !matchesSubjects(crb.Subjects, subjects) {
				continue
			}
			// Resolve the referenced ClusterRole.
			if crb.RoleRef.Kind != "ClusterRole" {
				continue
			}
			cr, err := rw.crLister.Get(crb.RoleRef.Name)
			if err != nil {
				continue
			}
			for _, rule := range cr.Rules {
				if ruleAllows(rule, verb, gr.Group, gr.Resource) {
					return true
				}
			}
		}
	}

	// Phase 2: Check RoleBindings in the target namespace (skip for cluster-wide).
	if namespace == "" {
		return false
	}
	if rbs, err := rw.rbLister.RoleBindings(namespace).List(labels.Everything()); err == nil {
		for _, rb := range rbs {
			if !matchesSubjects(rb.Subjects, subjects) {
				continue
			}
			switch rb.RoleRef.Kind {
			case "Role":
				role, err := rw.roleLister.Roles(namespace).Get(rb.RoleRef.Name)
				if err != nil {
					continue
				}
				for _, rule := range role.Rules {
					if ruleAllows(rule, verb, gr.Group, gr.Resource) {
						return true
					}
				}
			case "ClusterRole":
				cr, err := rw.crLister.Get(rb.RoleRef.Name)
				if err != nil {
					continue
				}
				for _, rule := range cr.Rules {
					if ruleAllows(rule, verb, gr.Group, gr.Resource) {
						return true
					}
				}
			}
		}
	}

	return false
}

// ruleAllows returns true if the policy rule grants the given verb on the
// given API group and resource. All three must match (conjunctive). The
// wildcard "*" matches any value.
func ruleAllows(rule rbacv1.PolicyRule, verb, group, resource string) bool {
	return containsOrStar(rule.Verbs, verb) &&
		containsOrStar(rule.APIGroups, group) &&
		containsOrStar(rule.Resources, resource)
}

// containsOrStar returns true if the list contains item or the wildcard "*".
func containsOrStar(list []string, item string) bool {
	for _, s := range list {
		if s == "*" || s == item {
			return true
		}
	}
	return false
}
