package dispatchers

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/l1cache"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
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
	preWarmConcurrency      = 10
	preWarmTimeout          = 30 * time.Second
	preWarmMaxDepth         = 5
	widgetGroup             = "widgets.templates.krateo.io"
	clientConfigSecretSuffix = "-clientconfig"
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

	refs := extractChildWidgetRefs(parentCtx, c, items, user.Username)
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
		nextRefs = append(nextRefs, extractChildWidgetRefs(ctx, c, childItems, user.Username)...)
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


// rbacPreWarmConcurrency limits the number of concurrent SelfSubjectAccessReview
// API calls during per-user RBAC pre-warming to avoid overwhelming the API server.
const rbacPreWarmConcurrency = 20

// PreWarmRBACForUser pre-populates the RBAC cache for a single user across all
// watched GVR x namespace x verb combinations. Called by UserSecretWatcher when
// a -clientconfig secret is created or updated.
//
// Uses bounded concurrency (rbacPreWarmConcurrency) for the SelfSubjectAccessReview
// calls. The function blocks until all checks complete or the context is cancelled.
func PreWarmRBACForUser(ctx context.Context, c *cache.RedisCache, rc *rest.Config, authnNS, signKey, username string) {
	log := slog.Default()
	if authnNS == "" || username == "" {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Load the user's endpoint from their -clientconfig secret.
	ep, err := endpoints.FromSecret(ctx, rc, username+clientConfigSecretSuffix, authnNS)
	if err != nil {
		log.Warn("RBAC pre-warm: failed to load user endpoint",
			slog.String("username", username), slog.Any("err", err))
		return
	}
	groups := extractGroupsFromClientCert(ep.ClientCertificateData)
	user := jwtutil.UserInfo{Username: username, Groups: groups}
	token := mintJWT(user, signKey)

	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(token),
	)
	rctx = cache.WithCache(rctx, c)

	// Get all watched GVRs.
	gvrKeys, err := c.SMembers(ctx, cache.WatchedGVRsKey)
	if err != nil || len(gvrKeys) == 0 {
		log.Debug("RBAC pre-warm: no watched GVRs", slog.String("username", username))
		return
	}

	// Get all namespaces from L3 cache.
	namespaces := getNamespacesFromL3(ctx, c)
	if len(namespaces) == 0 {
		log.Debug("RBAC pre-warm: no namespaces in L3", slog.String("username", username))
		return
	}

	verbs := []string{"list", "get", "create", "update", "delete", "patch"}

	// Build the work items.
	type rbacCheck struct {
		verb string
		gr   schema.GroupResource
		ns   string
	}
	var checks []rbacCheck
	for _, gvrKeyStr := range gvrKeys {
		gvr := cache.ParseGVRKey(gvrKeyStr)
		if gvr.Resource == "" {
			continue
		}
		gr := gvr.GroupResource()
		for _, ns := range namespaces {
			for _, verb := range verbs {
				// Skip if already cached.
				if allowed, cached := c.IsRBACAllowed(ctx, username, verb, gr, ns); cached {
					_ = allowed // already in cache, skip
					continue
				}
				checks = append(checks, rbacCheck{verb: verb, gr: gr, ns: ns})
			}
		}
	}

	if len(checks) == 0 {
		log.Debug("RBAC pre-warm: all checks already cached",
			slog.String("username", username))
		return
	}

	log.Info("RBAC pre-warm: starting",
		slog.String("username", username),
		slog.Int("checks", len(checks)),
		slog.Int("gvrs", len(gvrKeys)),
		slog.Int("namespaces", len(namespaces)),
	)

	// Execute checks with bounded concurrency.
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, rbacPreWarmConcurrency)
	)
	for _, check := range checks {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ch rbacCheck) {
			defer wg.Done()
			defer func() { <-sem }()
			rbac.UserCan(rctx, rbac.UserCanOptions{
				Verb:          ch.verb,
				GroupResource: ch.gr,
				Namespace:     ch.ns,
			})
		}(check)
	}
	wg.Wait()

	log.Info("RBAC pre-warm: completed",
		slog.String("username", username),
		slog.Int("checks", len(checks)),
	)
}

// MakeRBACPreWarmer returns a UserReadyFunc suitable for use with
// UserSecretWatcher.SetOnUserReady. It captures the dependencies needed
// to perform per-user RBAC pre-warming.
func MakeRBACPreWarmer(c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) cache.UserReadyFunc {
	return func(ctx context.Context, username string) {
		PreWarmRBACForUser(ctx, c, rc, authnNS, signKey, username)
	}
}

// getNamespacesFromL3 retrieves the list of namespace names from L3 cache.
// Tries per-item index first, falls back to monolithic blob.
func getNamespacesFromL3(ctx context.Context, c *cache.RedisCache) []string {
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	var nsList unstructured.UnstructuredList

	if raw, idxHit, _ := c.AssembleListFromIndex(ctx, nsGVR, ""); idxHit {
		if jerr := json.Unmarshal(raw, &nsList); jerr == nil {
			return extractNamespaceNames(nsList)
		}
	}
	return nil
}

func extractNamespaceNames(nsList unstructured.UnstructuredList) []string {
	names := make([]string, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		names = append(names, ns.GetName())
	}
	return names
}

// WarmRBACForAllUsers pre-populates the RBAC cache for all discovered users
// so that the watcher's cache-only RBAC checks and L1 warmup's L3→L2
// promotions have permission data available from the start.
func WarmRBACForAllUsers(ctx context.Context, c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) {
	log := slog.Default()
	if authnNS == "" {
		return
	}

	users, err := discoverUsers(ctx, rc, authnNS)
	if err != nil || len(users) == 0 {
		log.Warn("RBAC warmup: no users discovered", slog.Any("err", err))
		return
	}

	log.Info("RBAC warmup: starting", slog.Int("users", len(users)))

	// Delegate to PreWarmRBACForUser per user — it already has bounded
	// concurrency (sem of 20), skips cached results, and handles context.
	var wg sync.WaitGroup
	for _, u := range users {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(username string) {
			defer wg.Done()
			PreWarmRBACForUser(ctx, c, rc, authnNS, signKey, username)
		}(u.userInfo.Username)
	}
	wg.Wait()

	log.Info("RBAC warmup: completed", slog.Int("users", len(users)))
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
			rKey := cache.ResolvedKey(user.Username, gvr, obj.GetNamespace(), obj.GetName(), -1, -1)
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
	)
	rctx = cache.WithCache(rctx, c)

	var warmed int64
	for _, ra := range ras {
		rKey := cache.ResolvedKey(user.Username, raGVR, ra.Namespace, ra.Name, -1, -1)
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

		warmed++
		log.Info("L1 warmup: warmed RESTAction",
			slog.String("user", user.Username),
			slog.String("name", ra.Name))
	}
	return warmed
}

func extractChildWidgetRefs(ctx context.Context, c *cache.RedisCache, items []interface{}, username string) []l1Ref {
	var refs []l1Ref
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
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
		key := cache.ResolvedKey(username, gvr, ns, name, -1, -1)
		if c.Exists(ctx, key) {
			continue
		}
		refs = append(refs, l1Ref{gvr: gvr, ns: ns, name: name})
	}
	return refs
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
	)
	ctx = cache.WithCache(ctx, c)

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, preWarmConcurrency)
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
			rKey := cache.ResolvedKey(user.Username, r.gvr, r.ns, r.name, -1, -1)
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

			res, resolveErr := widgets.Resolve(tctx, widgets.ResolveOptions{
				In:      cached,
				AuthnNS: authnNS,
				PerPage: -1,
				Page:    -1,
			})
			if resolveErr != nil {
				return
			}
			raw, err := json.Marshal(res)
			if err != nil {
				return
			}

			_ = c.SetResolvedRaw(rctx, rKey, raw)
			cache.RegisterL1Dependencies(rctx, c, tracker, rKey)

			if newDeps := registerApiRefGVRDeps(rctx, c, cached, rKey, tracker); newDeps > 0 {
				_ = c.Delete(rctx, rKey)
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(ref)
	}

	wg.Wait()
	return results
}

// registerApiRefGVRDeps ensures that widgets with an apiRef (which always
// points to a RESTAction) are registered in the L1 GVR reverse indexes for
// the RESTAction's transitive dependencies — even when those dependencies
// returned empty data during warmup.
//
// During startup warmup, dynamic CRD data may not yet be in L3, causing the
// RESTAction to resolve with empty results. The DependencyTracker only
// captures GVRs that were actually visited, so the dynamic CRD GVRs are
// missed. Without this function, L1 refresh would never fire for those
// missing dependencies because the widget's L1 key would not appear in
// their reverse indexes.
//
// The fix: fetch the RESTAction CR from L3, parse its API items' paths to
// extract K8s API groups (both static paths and JQ template strings), use
// the K8s discovery API to find all resources belonging to those groups,
// register informers (SAddGVR) and add the widget's L1 key to the GVR
// reverse indexes. This works even during early startup because CRDs are
// already registered in the cluster API server.
func registerApiRefGVRDeps(ctx context.Context, c *cache.RedisCache, widgetObj *unstructured.Unstructured, l1Key string, tracker *cache.DependencyTracker) int {
	if c == nil {
		return 0
	}

	raName, _, _ := unstructured.NestedString(widgetObj.Object, "spec", "apiRef", "name")
	raNS, _, _ := unstructured.NestedString(widgetObj.Object, "spec", "apiRef", "namespace")
	if raName == "" {
		return 0
	}

	raGVR := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}
	raKey := cache.GetKey(raGVR, raNS, raName)
	var raObj unstructured.Unstructured
	if hit, err := c.Get(ctx, raKey, &raObj); !hit || err != nil {
		return 0
	}

	apis, found, _ := unstructured.NestedSlice(raObj.Object, "spec", "api")
	if !found {
		return 0
	}

	alreadyTracked := make(map[string]bool)
	if tracker != nil {
		for _, k := range tracker.GVRKeys() {
			alreadyTracked[k] = true
		}
	}

	groups := extractAPIGroups(apis)
	if len(groups) == 0 {
		return 0
	}

	rc, err := rest.InClusterConfig()
	if err != nil {
		return 0
	}
	discoveredGVRs := discoverGVRsForGroups(rc, groups)
	if len(discoveredGVRs) == 0 {
		return 0
	}

	var newGVRs []schema.GroupVersionResource
	for _, gvr := range discoveredGVRs {
		gvrKey := cache.GVRToKey(gvr)
		if alreadyTracked[gvrKey] {
			continue
		}
		newGVRs = append(newGVRs, gvr)
	}

	for _, gvr := range newGVRs {
		_ = c.SAddGVR(ctx, gvr)
	}

	if len(newGVRs) > 0 {
		slog.Debug("L1 warmup: registered apiRef transitive GVR deps",
			slog.String("widget", l1Key),
			slog.String("restaction", raName),
			slog.Int("registered", len(newGVRs)))
	}
	return len(newGVRs)
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

// discoverGVRsForGroups queries the K8s discovery API and returns all GVRs
// whose group matches one of the requested groups. Only list-capable
// resources are returned (subresources and non-listable resources are
// excluded).
func discoverGVRsForGroups(rc *rest.Config, groups []string) []schema.GroupVersionResource {
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil
	}
	lists, err := dc.ServerPreferredResources()
	if err != nil && lists == nil {
		return nil
	}
	wantGroup := make(map[string]bool, len(groups))
	for _, g := range groups {
		wantGroup[g] = true
	}
	var out []schema.GroupVersionResource
	for _, list := range lists {
		gv, perr := schema.ParseGroupVersion(list.GroupVersion)
		if perr != nil {
			continue
		}
		for _, res := range list.APIResources {
			g := res.Group
			if g == "" {
				g = gv.Group
			}
			if !wantGroup[g] {
				continue
			}
			if strings.Contains(res.Name, "/") {
				continue
			}
			v := res.Version
			if v == "" {
				v = gv.Version
			}
			out = append(out, schema.GroupVersionResource{Group: g, Version: v, Resource: res.Name})
		}
	}
	return out
}

// apisGroupRe matches "/apis/{group}/" in both static paths and JQ template
// strings (where the path is embedded in a string literal).
var apisGroupRe = regexp.MustCompile(`/apis/([a-zA-Z0-9][a-zA-Z0-9._-]*\.[a-zA-Z]{2,})/`)

// extractAPIGroups scans RESTAction API items' paths and returns unique K8s
// API group names found in them. It handles both direct paths
// ("/apis/composition.krateo.io/v1/...") and JQ template strings that embed
// such paths ("${ \"/apis/composition.krateo.io/\" + ... }").
func extractAPIGroups(apis []interface{}) []string {
	seen := map[string]bool{}
	for _, item := range apis {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		path, _ := m["path"].(string)
		if path == "" {
			continue
		}
		for _, match := range apisGroupRe.FindAllStringSubmatch(path, -1) {
			if len(match) > 1 {
				seen[match[1]] = true
			}
		}
	}
	var out []string
	for g := range seen {
		out = append(out, g)
	}
	return out
}
