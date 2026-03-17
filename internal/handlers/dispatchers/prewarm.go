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
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	preWarmConcurrency      = 10
	preWarmTimeout          = 30 * time.Second
	widgetGroup             = "widgets.templates.krateo.io"
	clientConfigSecretSuffix = "-clientconfig"
)

// ---------------------------------------------------------------------------
// Request-time child pre-warming
// ---------------------------------------------------------------------------

// preWarmChildWidgets resolves child widget references discovered during parent
// widget resolution and stores them in L1 cache. This eliminates the cold-start
// fan-out where the frontend issues N individual requests (e.g. 42 Routes after
// a RoutesLoader) that would each be an L1 miss.
func preWarmChildWidgets(parentCtx context.Context, c *cache.RedisCache, resolved *unstructured.Unstructured, authnNS string) {
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
		resolveL1RefsForUser(ctx, user, ep, accessToken, c, refs, authnNS)
		slog.Default().Info("L1 pre-warm completed",
			slog.String("user", user.Username),
			slog.Int("candidates", len(refs)),
		)
	}()
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

	log.Info("L1 warmup: starting",
		slog.Int("users", len(users)),
		slog.Int("widgetGVRs", len(widgetGVRs)),
		slog.Int("restActions", len(restActions)),
	)

	var totalWarmed int64
	for _, u := range users {
		token := mintJWT(u.userInfo, signKey)
		warmed := warmL1ForUser(ctx, c, u.userInfo, u.endpoint, token, widgetGVRs, authnNS)
		totalWarmed += warmed

		if len(restActions) > 0 {
			raWarmed := warmL1RestActionsForUser(ctx, c, u.userInfo, u.endpoint, token, restActions, authnNS)
			totalWarmed += raWarmed
		}
	}

	log.Info("L1 warmup: completed",
		slog.Int("users", len(users)),
		slog.Int64("totalWarmed", totalWarmed),
	)
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

	gvrKeys, err := c.SMembers(ctx, cache.WatchedGVRsKey)
	if err != nil || len(gvrKeys) == 0 {
		log.Info("RBAC warmup: no watched GVRs yet")
		return
	}

	var namespaces []string
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	nsListKey := cache.ListKey(nsGVR, "")
	var nsList unstructured.UnstructuredList
	if hit, gerr := c.Get(ctx, nsListKey, &nsList); hit && gerr == nil {
		for _, ns := range nsList.Items {
			namespaces = append(namespaces, ns.GetName())
		}
	}
	if len(namespaces) == 0 {
		log.Info("RBAC warmup: no namespaces in L3 cache")
		return
	}

	var total int64
	for _, u := range users {
		token := mintJWT(u.userInfo, signKey)
		rctx := xcontext.BuildContext(ctx,
			xcontext.WithUserConfig(u.endpoint),
			xcontext.WithUserInfo(u.userInfo),
			xcontext.WithAccessToken(token),
		)
		rctx = cache.WithCache(rctx, c)

		for _, gvrKeyStr := range gvrKeys {
			gvr := cache.ParseGVRKey(gvrKeyStr)
			if gvr.Resource == "" {
				continue
			}
			gr := gvr.GroupResource()
			for _, ns := range namespaces {
				rbac.UserCan(rctx, rbac.UserCanOptions{
					Verb: "list", GroupResource: gr, Namespace: ns,
				})
				total++
			}
		}
	}
	log.Info("RBAC warmup: completed",
		slog.Int("users", len(users)),
		slog.Int("gvrs", len(gvrKeys)),
		slog.Int("namespaces", len(namespaces)),
		slog.Int64("checks", total))
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

func warmL1ForUser(ctx context.Context, c *cache.RedisCache, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, gvrs []schema.GroupVersionResource, authnNS string) int64 {
	log := slog.Default()

	var allRefs []l1Ref
	for _, gvr := range gvrs {
		listKey := cache.ListKey(gvr, "")
		var list unstructured.UnstructuredList
		if hit, err := c.Get(ctx, listKey, &list); !hit || err != nil {
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

	warmed := resolveL1RefsForUser(ctx, user, ep, accessToken, c, allRefs, authnNS)

	log.Info("L1 warmup: user done",
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
func warmL1RestActionsForUser(ctx context.Context, c *cache.RedisCache, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, ras []cache.WarmupRestAction, authnNS string) int64 {
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

		got := objects.Get(rctx, templatesv1.ObjectReference{
			Reference:  templatesv1.Reference{Name: ra.Name, Namespace: ra.Namespace},
			APIVersion: raGVR.GroupVersion().String(),
			Resource:   raGVR.Resource,
		})
		if got.Err != nil {
			log.Warn("L1 warmup: failed to fetch RESTAction",
				slog.String("name", ra.Name), slog.Any("err", got.Err))
			continue
		}

		var cr templatesv1.RESTAction
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); err != nil {
			log.Warn("L1 warmup: failed to convert RESTAction",
				slog.String("name", ra.Name), slog.Any("err", err))
			continue
		}

		tracker := cache.NewDependencyTracker()
		tctx := cache.WithDependencyTracker(rctx, tracker)

		res, err := restactions.Resolve(tctx, restactions.ResolveOptions{
			In:      &cr,
			AuthnNS: authnNS,
			PerPage: -1,
			Page:    -1,
		})
		if err != nil {
			log.Warn("L1 warmup: failed to resolve RESTAction",
				slog.String("name", ra.Name), slog.Any("err", err))
			continue
		}

		raw, merr := json.Marshal(res)
		if merr != nil {
			continue
		}

		_ = c.SetResolvedRaw(tctx, rKey, cache.StripBulkyAnnotations(raw))
		for _, gvrKey := range tracker.GVRKeys() {
			_ = c.SAddWithTTL(tctx, cache.L1GVRKey(gvrKey), rKey, cache.ReverseIndexTTL)
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
func resolveL1RefsForUser(ctx context.Context, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, c *cache.RedisCache, refs []l1Ref, authnNS string) int64 {
	ctx = xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(accessToken),
	)
	ctx = cache.WithCache(ctx, c)

	var (
		wg     sync.WaitGroup
		sem    = make(chan struct{}, preWarmConcurrency)
		warmed int64
	)

	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(r l1Ref) {
			defer wg.Done()
			defer func() { <-sem }()

			rctx := xcontext.BuildContext(ctx,
				xcontext.WithUserConfig(ep),
				xcontext.WithUserInfo(user),
				xcontext.WithAccessToken(accessToken),
			)
			rctx = cache.WithCache(rctx, c)

			got := objects.Get(rctx, templatesv1.ObjectReference{
				Reference: templatesv1.Reference{
					Name: r.name, Namespace: r.ns,
				},
				APIVersion: r.gvr.GroupVersion().String(),
				Resource:   r.gvr.Resource,
			})
			if got.Err != nil {
				return
			}

			tracker := cache.NewDependencyTracker()
			tctx := cache.WithDependencyTracker(rctx, tracker)

			if r.gvr.Group != widgetGroup {
				return
			}
			res, resolveErr := widgets.Resolve(tctx, widgets.ResolveOptions{
				In:      got.Unstructured,
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

			rKey := cache.ResolvedKey(user.Username, r.gvr, r.ns, r.name, -1, -1)
			_ = c.SetResolvedRaw(rctx, rKey, raw)
			for _, gvrKey := range tracker.GVRKeys() {
				_ = c.SAddWithTTL(rctx, cache.L1GVRKey(gvrKey), rKey, cache.ReverseIndexTTL)
			}

			if newDeps := registerApiRefGVRDeps(rctx, c, got.Unstructured, rKey, tracker); newDeps > 0 {
				_ = c.Delete(rctx, rKey)
			}

			atomic.AddInt64(&warmed, 1)
		}(ref)
	}

	wg.Wait()
	return warmed
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
	for _, k := range tracker.GVRKeys() {
		alreadyTracked[k] = true
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
		_ = c.SAddWithTTL(ctx, cache.L1GVRKey(gvrKey), l1Key, cache.ReverseIndexTTL)
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
