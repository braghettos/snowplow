package dispatchers

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/l1cache"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// prewarmDynClient is a SA-backed dynamic client with its rate limiter
// disabled (QPS = -1). Used for CR fallback reads on L2 miss during
// prewarm so the 20K+ reads per user do not exhaust the default
// 5 QPS / 10 burst budget of the per-user rest.Config.
//
// Safe to use for *reading* CRs: authorisation/RBAC is applied by the
// widget and restaction resolvers later, which run under each user's
// context. The CR content itself is not user-sensitive; it is cluster
// configuration already visible to the snowplow service account.
//
// Initialised once by WarmL1ForAllUsers at startup. Request-time callers
// (e.g. preWarmChildWidgets via the widgets HTTP dispatcher) read it
// without synchronisation — the read happens only after prewarm kickoff,
// which already wrote this var, and preWarmComplete gates most callers.
var prewarmDynClient k8sdynamic.Interface

func newUnthrottledDynClient(rc *rest.Config) (k8sdynamic.Interface, error) {
	cfg := rest.CopyConfig(rc)
	cfg.QPS = -1
	cfg.Burst = 0
	return k8sdynamic.NewForConfig(cfg)
}

// prewarmFetchCR returns the unstructured.Unstructured for (gvr, ns, name).
// Fast path: Redis L2 cache (populated by warmer.warmGVR at startup).
// Slow path: a single GET via the shared unthrottled dynamic client, with
// the result written back to L2 so subsequent prewarm passes skip the GET.
//
// Returns (nil, false) if neither L2 nor the k8s GET yields a CR (eg the
// resource does not exist, RBAC denies, or the fallback client was nil).
func prewarmFetchCR(ctx context.Context, c *cache.RedisCache, dynClient k8sdynamic.Interface, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, bool) {
	if c != nil {
		var cached unstructured.Unstructured
		l2Key := cache.GetKey(gvr, ns, name)
		if hit, cerr := c.Get(ctx, l2Key, &cached); hit && cerr == nil {
			return &cached, true
		}
	}
	if dynClient == nil {
		return nil, false
	}
	uns, err := dynClient.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, false
	}
	cache.StripAnnotationsFromUnstructured(uns)
	if c != nil {
		_ = c.SetForGVR(ctx, gvr, cache.GetKey(gvr, ns, name), uns)
	}
	return uns, true
}

const (
	preWarmTimeout          = 30 * time.Second
	preWarmMaxDepth         = 5
	widgetGroup             = "widgets.templates.krateo.io"
	clientConfigSecretSuffix = "-clientconfig"

	// prewarmPerPage is the page size used during prewarm widget resolution.
	// Matches the frontend default so prewarm resolves only the first visible
	// page of paginated widgets (e.g., the compositions DataGrid shows 5 panels
	// initially). Without this bound, the prewarm resolves ALL 50K compositions
	// and recurses into every panel's children.
	prewarmPerPage = 5
	prewarmPage    = 1
)

// ---------------------------------------------------------------------------
// Pre-warm lifecycle control
// ---------------------------------------------------------------------------

// preWarmComplete is set to true after the initial L1 warmup finishes.
// Once set, preWarmChildWidgets becomes a no-op — runtime L1 updates
// are handled exclusively by the event-driven dirty+ticker system.
var preWarmComplete atomic.Bool

// IsPreWarmComplete returns true if the initial L1 warmup has finished.
// Used by the readiness probe to gate traffic until cache is warm.
func IsPreWarmComplete() bool {
	return preWarmComplete.Load()
}

// ---------------------------------------------------------------------------
// Request-time child pre-warming
// ---------------------------------------------------------------------------

// preWarmChildWidgets recursively resolves child widget references discovered
// during parent widget resolution and stores them in L1 cache. It walks the
// entire widget tree (up to preWarmMaxDepth levels) so that all descendants
// are cached before the frontend requests them.
func preWarmChildWidgets(parentCtx context.Context, c *cache.RedisCache, resolved *unstructured.Unstructured, authnNS string) {
	// After initial warmup completes, pre-warming is disabled.
	// Runtime L1 updates are handled by the event-driven dirty+ticker system.
	if preWarmComplete.Load() {
		return
	}

	items, found, err := unstructured.NestedSlice(resolved.Object, "status", "resourcesRefs", "items")
	if err != nil || !found || len(items) == 0 {
		return
	}

	user, uerr := xcontext.UserInfo(parentCtx)
	if uerr != nil {
		return
	}
	ep, eerr := xcontext.UserConfig(parentCtx)
	if eerr != nil {
		return
	}
	accessToken, _ := xcontext.AccessToken(parentCtx)

	identity := cache.CacheIdentity(parentCtx, user.Username)
	actionRefIDs := extractActionRefIDs(resolved.Object)
	refs := extractChildWidgetRefs(parentCtx, c, items, identity, actionRefIDs)
	if len(refs) == 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), preWarmTimeout)
		defer cancel()
		visited := make(map[string]bool)
		recursivePreWarm(ctx, user, ep, accessToken, c, prewarmDynClient, refs, authnNS, visited, 1)
		slog.Default().Info("L1 recursive pre-warm completed",
			slog.String("user", user.Username),
			slog.Int("total_warmed", len(visited)),
		)
	}()
}

// recursivePreWarm resolves refs into L1 cache, then extracts their children
// and recurses until maxDepth or no more children are found.
func recursivePreWarm(ctx context.Context, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, c *cache.RedisCache, dynClient k8sdynamic.Interface, refs []l1Ref, authnNS string, visited map[string]bool, depth int) {
	if depth > preWarmMaxDepth || len(refs) == 0 {
		return
	}
	if ctx.Err() != nil {
		return
	}

	var unvisited []l1Ref
	for _, r := range refs {
		key := r.gvr.String() + ":" + r.ns + ":" + r.name
		if visited[key] {
			continue
		}
		visited[key] = true
		unvisited = append(unvisited, r)
	}
	if len(unvisited) == 0 {
		return
	}

	resolved := resolveL1RefsCollect(ctx, user, ep, accessToken, c, dynClient, unvisited, authnNS)

	var nextRefs []l1Ref
	for _, res := range resolved {
		childItems, found, err := unstructured.NestedSlice(res.Object, "status", "resourcesRefs", "items")
		if err != nil || !found || len(childItems) == 0 {
			continue
		}
		// Collect ref IDs that are action-linked (navigate, openDrawer,
		// openModal, etc.). These are deferred — the frontend only
		// resolves them on user interaction — so the prewarm must skip
		// them to avoid recursing into 50K+ composition children.
		actionRefIDs := extractActionRefIDs(res.Object)
		nextRefs = append(nextRefs, extractChildWidgetRefs(ctx, c, childItems, cache.CacheIdentity(ctx, user.Username), actionRefIDs)...)
	}

	recursivePreWarm(ctx, user, ep, accessToken, c, dynClient, nextRefs, authnNS, visited, depth+1)
}

// ---------------------------------------------------------------------------
// Startup L1 warmup for all users
// ---------------------------------------------------------------------------

// WarmL1ForAllUsers discovers every user from their -clientconfig Secrets, then
// resolves every instance of every widget GVR for each user, populating the
// per-user L1 cache. It also warms explicitly-configured RESTActions so that
// navigation and dashboard requests have zero cold-start latency.
func WarmL1ForAllUsers(ctx context.Context, c *cache.RedisCache, rc *rest.Config, authnNS, signKey string, widgetGVRs []schema.GroupVersionResource, restActions []cache.WarmupRestAction) {
	log := slog.Default()
	if len(widgetGVRs) == 0 && len(restActions) == 0 || authnNS == "" {
		log.Info("L1 warmup: skipped (no GVRs/restactions or authn namespace)")
		return
	}

	users, err := discoverUsers(ctx, rc, authnNS)
	if err != nil {
		log.Warn("L1 warmup: failed to discover users", slog.Any("err", err))
		return
	}
	if len(users) == 0 {
		log.Info("L1 warmup: no users found")
		return
	}

	// Build a SA-backed unthrottled dynamic client for CR fallback reads
	// on L2 miss. See prewarmDynClient for rationale.
	dynClient, dcErr := newUnthrottledDynClient(rc)
	if dcErr != nil {
		log.Warn("L1 warmup: unable to build unthrottled dyn client; L2 miss will skip fallback",
			slog.Any("err", dcErr))
		// dynClient stays nil; prewarmFetchCR will treat L2 miss as skip.
	}
	prewarmDynClient = dynClient

	log.Info("L1 warmup: starting",
		slog.Int("users", len(users)),
		slog.Int("widgetGVRs", len(widgetGVRs)),
		slog.Int("restActions", len(restActions)),
	)

	// RESTActions BEFORE widgets: widget warmup issues 20K+ CR fetches
	// (panels, buttons, etc). Running RAs first guarantees the small set
	// of critical RESTAction L1 entries (compositions-list etc.) is warm
	// before we start the large widget walk.
	var totalWarmed int64
	for _, u := range users {
		token := mintJWT(u.userInfo, signKey)

		if len(restActions) > 0 {
			raWarmed := warmL1RestActionsForUser(ctx, c, dynClient, u.userInfo, u.endpoint, token, restActions, authnNS)
			totalWarmed += raWarmed
		}

		warmed := warmL1ForUser(ctx, c, dynClient, u.userInfo, u.endpoint, token, widgetGVRs, authnNS)
		totalWarmed += warmed
	}

	log.Info("L1 warmup: completed",
		slog.Int("users", len(users)),
		slog.Int64("totalWarmed", totalWarmed),
	)
	cache.MarkL1Ready(ctx, c)

	// Mark pre-warm phase as complete. From this point, preWarmChildWidgets
	// becomes a no-op and all L1 updates are event-driven.
	preWarmComplete.Store(true)
	log.Info("L1 warmup: pre-warm disabled, switching to event-driven updates")
}

// FilterWidgetGVRs returns only the GVRs from the warmup config whose group
// is widgets.templates.krateo.io (i.e., widget resources that have L1 output).
func FilterWidgetGVRs(cfg *cache.WarmupConfig) []schema.GroupVersionResource {
	if cfg == nil {
		return nil
	}
	var out []schema.GroupVersionResource
	for _, entry := range cfg.Warmup.GVRs {
		if entry.Group == widgetGroup {
			out = append(out, schema.GroupVersionResource{
				Group:    entry.Group,
				Version:  entry.Version,
				Resource: entry.Resource,
			})
		}
	}
	return out
}



// WarmL1FromEntryPoints discovers the widget tree starting from the frontend
// entry points (INIT, ROUTES_LOADER) and resolves them per unique binding
// identity group. Users with identical RBAC bindings share L1 entries, so
// only one representative user per group is resolved. RBAC decisions are
// populated organically during resolution.
func WarmL1FromEntryPoints(ctx context.Context, c *cache.RedisCache, rc *rest.Config,
	authnNS, signKey string, entryPoints []cache.EntryPoint, rbacWatcher *cache.RBACWatcher) {
	log := slog.Default()
	if len(entryPoints) == 0 || authnNS == "" {
		log.Info("L1 entry-point warmup: skipped (no entry points or authn namespace)")
		return
	}

	users, err := discoverUsers(ctx, rc, authnNS)
	if err != nil {
		log.Warn("L1 entry-point warmup: failed to discover users", slog.Any("err", err))
		return
	}
	if len(users) == 0 {
		log.Info("L1 entry-point warmup: no users found")
		return
	}

	dynClient, dcErr := newUnthrottledDynClient(rc)
	if dcErr != nil {
		log.Warn("L1 entry-point warmup: unable to build unthrottled dyn client",
			slog.Any("err", dcErr))
	}
	prewarmDynClient = dynClient

	// Group users by binding identity. Only one representative user per
	// group needs to be resolved — the L1 keys use the binding identity,
	// so the resolved output is shared across all users in the group.
	type bindingGroup struct {
		identity string
		user     discoveredUser
		members  int
	}
	groups := make(map[string]*bindingGroup)
	for _, u := range users {
		bid := ""
		if rbacWatcher != nil {
			bid = rbacWatcher.CachedBindingIdentity(u.userInfo.Username, u.userInfo.Groups)
		}
		if bid == "" {
			bid = u.userInfo.Username // fallback: treat as unique
		}
		// Register the mapping for L1 refresh credential lookup.
		cache.RegisterBindingUser(bid, u.userInfo.Username)

		if g, ok := groups[bid]; ok {
			g.members++
		} else {
			groups[bid] = &bindingGroup{identity: bid, user: u, members: 1}
		}
	}

	log.Info("L1 entry-point warmup: starting",
		slog.Int("users", len(users)),
		slog.Int("bindingGroups", len(groups)),
		slog.Int("entryPoints", len(entryPoints)),
	)

	// Convert entry points to l1Refs.
	var epRefs []l1Ref
	for _, ep := range entryPoints {
		epRefs = append(epRefs, l1Ref{gvr: ep.GVR, ns: ep.Namespace, name: ep.Name})
	}

	// Warm one representative user per binding group.
	var totalWarmed int64
	for _, g := range groups {
		u := g.user
		token := mintJWT(u.userInfo, signKey)

		// Inject binding identity into context so all downstream key
		// building uses the shared identity, not the username.
		warmCtx := cache.WithBindingIdentity(ctx, g.identity)
		// Inject RBACWatcher for local RBAC evaluation during prewarm.
		if rbacWatcher != nil {
			warmCtx = cache.WithRBACWatcher(warmCtx, rbacWatcher)
		}

		visited := make(map[string]bool)
		recursivePreWarm(warmCtx, u.userInfo, u.endpoint, token, c, dynClient, epRefs, authnNS, visited, 1)
		warmed := int64(len(visited))
		totalWarmed += warmed
		log.Info("L1 entry-point warmup: group done",
			slog.String("identity", g.identity),
			slog.String("representative", u.userInfo.Username),
			slog.Int("members", g.members),
			slog.Int64("warmed", warmed),
		)
	}

	log.Info("L1 entry-point warmup: completed",
		slog.Int("users", len(users)),
		slog.Int("bindingGroups", len(groups)),
		slog.Int64("totalWarmed", totalWarmed),
	)

	preWarmComplete.Store(true)
	cache.MarkL1Ready(ctx, c)
	log.Info("L1 entry-point warmup: pre-warm disabled, switching to event-driven updates")
}

// MakeRBACPreWarmer returns a no-op UserReadyFunc. RBAC decisions are now
// populated organically during widget/RESTAction resolution rather than
// pre-warmed via the GVR x NS x verb cartesian product.
func MakeRBACPreWarmer(c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) cache.UserReadyFunc {
	return func(ctx context.Context, username string) {
		// RBAC warms organically during resolution -- no explicit pre-warm.
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type discoveredUser struct {
	userInfo jwtutil.UserInfo
	endpoint endpoints.Endpoint
}

func discoverUsers(ctx context.Context, rc *rest.Config, authnNS string) ([]discoveredUser, error) {
	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	secrets, err := clientset.CoreV1().Secrets(authnNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var users []discoveredUser
	for _, sec := range secrets.Items {
		name := sec.Name
		if !strings.HasSuffix(name, clientConfigSecretSuffix) {
			continue
		}
		username := strings.TrimSuffix(name, clientConfigSecretSuffix)
		ep, err := endpoints.FromSecret(ctx, rc, name, authnNS)
		if err != nil {
			slog.Warn("L1 warmup: failed to load endpoint for user",
				slog.String("user", username), slog.Any("err", err))
			continue
		}
		groups := extractGroupsFromClientCert(ep.ClientCertificateData)
		slog.Debug("L1 warmup: discovered user",
			slog.String("user", username),
			slog.Any("groups", groups))
		users = append(users, discoveredUser{
			userInfo: jwtutil.UserInfo{Username: username, Groups: groups},
			endpoint: ep,
		})
	}
	return users, nil
}


// extractGroupsFromClientCert parses the PEM-encoded client certificate and
// returns the Subject.Organization values, which Kubernetes uses as groups.
func extractGroupsFromClientCert(certPEM string) []string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert.Subject.Organization
}

func warmL1ForUser(ctx context.Context, c *cache.RedisCache, dynClient k8sdynamic.Interface, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, gvrs []schema.GroupVersionResource, authnNS string) int64 {
	log := slog.Default()

	// Use binding identity for cache keys during prewarm. The prewarm context
	// does not go through HTTP middleware, so binding identity is not in ctx.
	// Use username as the identity (prewarm runs before binding identity is
	// available from the RBAC watcher's initial sync).
	identity := cache.CacheIdentity(ctx, user.Username)

	var allRefs []l1Ref
	for _, gvr := range gvrs {
		var list unstructured.UnstructuredList
		raw, idxHit, _ := c.AssembleListFromIndex(ctx, gvr, "")
		if !idxHit {
			continue
		}
		if jerr := json.Unmarshal(raw, &list); jerr != nil {
			continue
		}
		for _, obj := range list.Items {
			rKey := cache.ResolvedKey(identity, gvr, obj.GetNamespace(), obj.GetName(), -1, -1)
			if c.Exists(ctx, rKey) {
				continue
			}
			allRefs = append(allRefs, l1Ref{
				gvr:  gvr,
				ns:   obj.GetNamespace(),
				name: obj.GetName(),
			})
		}
	}

	if len(allRefs) == 0 {
		return 0
	}

	visited := make(map[string]bool)
	recursivePreWarm(ctx, user, ep, accessToken, c, dynClient, allRefs, authnNS, visited, 1)

	warmed := int64(len(visited))
	log.Info("L1 warmup: user done (recursive)",
		slog.String("user", user.Username),
		slog.Int("candidates", len(allRefs)),
		slog.Int64("warmed", warmed),
	)
	return warmed
}

type l1Ref struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string
}

// warmL1RestActionsForUser resolves the explicitly-configured RESTActions for
// a single user and stores them in L1 cache. Returns the number warmed.
func warmL1RestActionsForUser(ctx context.Context, c *cache.RedisCache, dynClient k8sdynamic.Interface, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, ras []cache.WarmupRestAction, authnNS string) int64 {
	log := slog.Default()
	raGVR := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}

	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(accessToken),
		xcontext.WithLogger(slog.Default()),
	)
	rctx = cache.WithCache(rctx, c)

	// Use binding identity for cache keys during prewarm.
	identity := cache.CacheIdentity(rctx, user.Username)

	var warmed int64
	for _, ra := range ras {
		rKey := cache.ResolvedKey(identity, raGVR, ra.Namespace, ra.Name, -1, -1)
		if c.Exists(rctx, rKey) {
			continue
		}

		// Fetch the RESTAction CR via L2-first with unthrottled k8s
		// fallback. See prewarmFetchCR for rationale — avoids the
		// default 5 QPS / 10 burst rate limiter on the per-user rest
		// config that would otherwise fail every fallback with
		// "client rate limiter Wait returned an error".
		cached, ok := prewarmFetchCR(rctx, c, dynClient, raGVR, ra.Namespace, ra.Name)
		if !ok {
			log.Warn("L1 warmup: failed to fetch RESTAction",
				slog.String("name", ra.Name),
				slog.String("namespace", ra.Namespace))
			continue
		}

		// Delegate convert → resolve → marshal → strip → L1 write to the
		// shared l1cache helper. Keeps prewarm, HTTP dispatcher, and
		// widget apiref paths on one code path — no drift risk on key
		// schema, dependency registration, or marshal step.
		if _, err := l1cache.ResolveAndCache(rctx, l1cache.Input{
			Cache:       c,
			Obj:         cached.Object,
			ResolvedKey: rKey,
			AuthnNS:     authnNS,
			PerPage:     -1,
			Page:        -1,
		}); err != nil {
			log.Warn("L1 warmup: failed to resolve RESTAction",
				slog.String("name", ra.Name), slog.Any("err", err))
			continue
		}

		// Touch the key so prewarm-resolved RESTActions start HOT.
		if rki, ok := cache.ParseResolvedKey(rKey); ok {
			cache.TouchKey(cache.ResolvedKeyBase(rki.Username, rki.GVR, rki.NS, rki.Name))
		}

		warmed++
		log.Info("L1 warmup: warmed RESTAction",
			slog.String("user", user.Username),
			slog.String("name", ra.Name))
	}
	return warmed
}

// extractChildWidgetRefs filters resourcesRefs items to only those that should
// be pre-warmed. Items whose ID appears in skipIDs (action-linked refs like
// navigate/openDrawer/openModal) are excluded — they are deferred and only
// resolved on user interaction.
func extractChildWidgetRefs(ctx context.Context, c *cache.RedisCache, items []interface{}, identity string, skipIDs map[string]bool) []l1Ref {
	var refs []l1Ref
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		// Skip action-linked refs: their ID matches a resourceRefId in the
		// widget's actions map. The frontend resolves these lazily on click.
		if id, _ := m["id"].(string); id != "" && skipIDs[id] {
			continue
		}
		path, _ := m["path"].(string)
		if path == "" || !strings.Contains(path, "/call?") {
			continue
		}
		gvr, ns, name := cache.ParseCallPath(path)
		if gvr.Resource == "" || name == "" {
			continue
		}
		if gvr.Group != widgetGroup {
			continue
		}
		key := cache.ResolvedKey(identity, gvr, ns, name, -1, -1)
		if c.Exists(ctx, key) {
			continue
		}
		refs = append(refs, l1Ref{gvr: gvr, ns: ns, name: name})
	}
	return refs
}

// extractActionRefIDs collects all resourceRefId values from the resolved
// widget's status.widgetData.actions map. Action types include navigate,
// openDrawer, openModal, and rest — each is an array of items with a
// resourceRefId field. These refs are deferred (resolved only on user
// interaction) and must be excluded from prewarm recursion.
func extractActionRefIDs(obj map[string]interface{}) map[string]bool {
	ids := make(map[string]bool)
	// The resolved widget stores widgetData under status.widgetData.
	widgetData, found, err := unstructured.NestedMap(obj, "status", "widgetData")
	if err != nil || !found {
		return ids
	}
	actions, found, err := unstructured.NestedMap(widgetData, "actions")
	if err != nil || !found {
		return ids
	}
	// Walk every action type (navigate, openDrawer, openModal, rest, etc.).
	// Each is an array of objects with a resourceRefId field.
	for _, actionList := range actions {
		items, ok := actionList.([]interface{})
		if !ok {
			continue
		}
		for _, item := range items {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if refID, ok := m["resourceRefId"].(string); ok && refID != "" {
				ids[refID] = true
			}
		}
	}
	return ids
}

// resolveL1RefsForUser resolves a batch of L1 refs (widgets or RESTActions) for
// a specific user and stores them in L1 cache. Returns the number successfully
// warmed. The caller provides the context (with its own deadline).
// resolveL1RefsCollect resolves refs into L1 and returns the resolved objects
// so callers can inspect children for recursive pre-warming.
func resolveL1RefsCollect(ctx context.Context, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, c *cache.RedisCache, dynClient k8sdynamic.Interface, refs []l1Ref, authnNS string) []*unstructured.Unstructured {
	ctx = xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(accessToken),
		xcontext.WithLogger(slog.Default()),
	)
	ctx = cache.WithCache(ctx, c)

	// Use binding identity for cache keys when available.
	identity := cache.CacheIdentity(ctx, user.Username)

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, runtime.GOMAXPROCS(0))
		mu      sync.Mutex
		results []*unstructured.Unstructured
	)

	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(r l1Ref) {
			defer wg.Done()
			defer func() { <-sem }()

			// Skip if L1 key already exists — pre-warmer only populates cold slots.
			// Event-driven dirty+ticker handles updates to existing keys.
			rKey := cache.ResolvedKey(identity, r.gvr, r.ns, r.name, -1, -1)
			if c != nil && c.Exists(ctx, rKey) {
				return
			}

			if r.gvr.Group != widgetGroup {
				return
			}

			rctx := xcontext.BuildContext(ctx,
				xcontext.WithUserConfig(ep),
				xcontext.WithUserInfo(user),
				xcontext.WithAccessToken(accessToken),
				xcontext.WithLogger(slog.Default()),
			)
			rctx = cache.WithCache(rctx, c)

			// Fetch the widget CR via L2-first with unthrottled k8s
			// fallback (see prewarmFetchCR). Avoids the 5 QPS / 10 burst
			// rate limiter on the per-user rest.Config which 20K+
			// sequential fetches would otherwise exhaust.
			cached, ok := prewarmFetchCR(rctx, c, dynClient, r.gvr, r.ns, r.name)
			if !ok {
				return
			}

			tracker := cache.NewDependencyTracker()
			tctx := cache.WithDependencyTracker(rctx, tracker)

			// Use bounded pagination during prewarm: resolve only page 1
			// with a small page size (matching the frontend default). This
			// prevents the prewarm from resolving ALL 50K compositions and
			// recursing into every panel's children. Widgets without
			// pagination are unaffected (PerPage/Page are ignored when the
			// widget has no apiRef or the apiRef has no .slice usage).
			res, resolveErr := widgets.Resolve(tctx, widgets.ResolveOptions{
				In:      cached,
				AuthnNS: authnNS,
				PerPage: prewarmPerPage,
				Page:    prewarmPage,
			})
			if resolveErr != nil {
				return
			}
			raw, err := json.Marshal(res)
			if err != nil {
				return
			}

			_ = c.SetResolvedRaw(rctx, rKey, raw)
			// Touch the key so prewarm-resolved keys start HOT.
			if rki, ok := cache.ParseResolvedKey(rKey); ok {
				cache.TouchKey(cache.ResolvedKeyBase(rki.Username, rki.GVR, rki.NS, rki.Name))
			}
			// Register cascade deps: widget → RESTAction (from tracker refs).
			// Same logic as widgets.go:244-255.
			if refs := tracker.ResourceRefs(); len(refs) > 0 {
				pipe := c.Pipeline(rctx)
				if pipe != nil {
					for _, ref := range refs {
						key := cache.L1ResourceDepKey(ref.GVRKey, ref.NS, ref.Name)
						pipe.SAdd(rctx, key, rKey)
						pipe.Expire(rctx, key, cache.ReverseIndexTTL)
					}
					_, _ = pipe.Exec(rctx)
				}
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(ref)
	}

	wg.Wait()
	return results
}

// mintJWT creates a short-lived JWT for internal use during L1 warmup and
// refresh. The token allows internal HTTP callbacks (e.g. RESTAction API items
// with exportJwt:true) to authenticate with snowplow.
func mintJWT(user jwtutil.UserInfo, signKey string) string {
	if signKey == "" {
		return ""
	}
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username:  user.Username,
		Groups:    user.Groups,
		Duration:  5 * time.Minute,
		SigningKey: signKey,
	})
	if err != nil {
		slog.Warn("L1: failed to mint JWT", slog.String("user", user.Username), slog.Any("err", err))
		return ""
	}
	return tok
}
