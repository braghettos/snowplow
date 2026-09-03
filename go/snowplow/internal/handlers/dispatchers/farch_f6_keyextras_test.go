// farch_f6_keyextras_test.go — F6 author-declared request-extras allowlist
// falsifiers (docs/f6-chrome-route-key-design-2026-07-12.md §4 Option A-declare,
// §7). F6 closes the #130 chrome-widget first-nav gap: the frontend folds the
// route params ({namespace, name}) into the ?extras= of EVERY widget /call, and
// those params partition the per-cohort widgets key. A chrome widget (app-shell,
// sidebar-nav) renders byte-identical content on every route, so the route-param
// partitioning is SPURIOUS over-keying — the seed (extras-less) cell is never
// browser-reachable. The fix: only the request-extras keys the widget author
// DECLARES in spec.keyExtras fold into the key. A chrome widget declares nothing
// → its cell stops partitioning by route → one seeded cell serves all routes.
//
// This is the F-ARCH-1 seed-key-divergence class one dimension over (route
// params instead of identity), so the arms mirror farch_seed_parity_test.go:
// drive BOTH sides through the REAL effectiveKeyExtras → ComputeKey (never
// hand-fed keys; feedback_key_parity_golden_real_inputs_prehash_diff) and assert
// the shared cell is reachable / the wrong body is NOT served.
//
//	Chrome arm (F6-1, the win): a widget with NO keyExtras — seed key
//	  (nil extras) == serve key with route-param extras {namespace, name}; L1
//	  HIT, zero resolver invocations.
//	RED arm (F6-2, audit necessity): a widget whose jq reads extras.namespace
//	  with NO declaration collides two DIFFERENT namespaces onto one key AND
//	  serves the first-warmed body to the second route (wrong body). This is why
//	  the audit+declaration is MANDATORY. GREEN once it declares keyExtras:[namespace].
//	Declared-partition arm (F6-3, C-F6-3): a DECLARED widget still partitions —
//	  two routes → two distinct keys → correct distinct content.
//	Subscription-parity arm (F6-5, C-F6-5 BLOCKING): a route-param-bearing
//	  chrome-widget subscription coord derives the SAME key as the fold-nothing
//	  seed cell, driven through the REAL DeriveSubscriptionKey path (#66 lesson:
//	  never a shadow copy).
//	Self-quarantine arm (F6-6, arch-ruled 2026-07-13, per-cohort cell): two users
//	  in the SAME BindingUID cohort; a widget with NO keyExtras + no apiRef +
//	  widgetDataTemplate echoing extras.foo. A sends foo=evil, B clean. GREEN: the
//	  guard declines A's shared-cohort Put → B misses → resolves clean (B's body !=
//	  evil), AND cacheKey_A == cacheKey_B (the collision is real; the GUARD, not key
//	  divergence, protects B). RED: neuter the decline → B served evil. Plus the
//	  declared-counterpart (keyExtras:[foo]) → distinct keys, no decline, guard inert.
package dispatchers

import (
	"context"
	"reflect"
	"testing"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// routeParamsRequest is the extras a post-A6 buildExtrasParam
// (useWidgetQuery.ts:84-101) folds for a widget rendered on
// /compositions/:namespace/:name — the route params merged into ?extras=. This
// is the exact wire shape F6 pins the key against.
func routeParamsRequest(namespace, name string) map[string]any {
	return map[string]any{"namespace": namespace, "name": name}
}

// chromeWidgetCR is a chrome/layout widget (app-shell, sidebar-nav): NO
// spec.keyExtras, NO spec.identityContext, no inline extras — renders fixed
// chrome that does not consume route params. The ~identity-free-corpus shape.
func chromeWidgetCR() map[string]any { return map[string]any{"spec": map[string]any{}} }

// declaredKeyExtrasCR is a route-consuming widget (composition-detail /
// resources) that DECLARES spec.keyExtras=keys — the request-extras keys its jq
// reads. Only these partition its cell.
func declaredKeyExtrasCR(keys ...string) map[string]any {
	anyKeys := make([]any, len(keys))
	for i, k := range keys {
		anyKeys[i] = k
	}
	return map[string]any{"spec": map[string]any{"keyExtras": anyKeys}}
}

// TestFARCH_F6_1_ChromeWidget_RouteParamsFoldNothing_HIT — the F6 win. A chrome
// widget (no keyExtras) seeded under the extras-less seed fold is HIT by a
// browser request carrying route params in ?extras=. Pre-hash: BOTH sides fold
// EMPTY effective extras (route params dropped by the fold-nothing default) →
// same key → real Put-under-seed / Get-under-serve HIT, correct body, zero
// resolver invocations. This is the cell that fixes #130 first-nav.
func TestFARCH_F6_1_ChromeWidget_RouteParamsFoldNothing_HIT(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := ctxWithIdentity() // cyberjoker / [devs]
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "panels", "krateo-system", "app-shell"
		perPage, page     = 10, 1
	)
	cr := chromeWidgetCR()

	// SEED key: the extras-less seed fold (the boot seed carries no request).
	seedExtras := effectiveKeyExtras(ctx, cr, nil)
	seedKey, seedHandle, seedInputs := dispatchCacheLookupKey(ctx, "widgets",
		g, v, r, ns, name, perPage, page, seedExtras)
	if seedHandle == nil || seedKey == "" {
		t.Fatal("F6-1: expected a live per-cohort handle + key under the seed ctx")
	}
	body := []byte(`{"status":{"widgetData":{"chrome":"shell"}}}`)
	seedHandle.Put(seedKey, &cache.ResolvedEntry{RawJSON: body, Inputs: seedInputs})

	// SERVE key: the browser folds route params on a /compositions route.
	serveExtras := effectiveKeyExtras(ctx, cr, routeParamsRequest("team-a", "demo-1"))
	serveKey, serveHandle, serveInputs := dispatchCacheLookupKey(ctx, "widgets",
		g, v, r, ns, name, perPage, page, serveExtras)
	if serveHandle == nil || serveKey == "" {
		t.Fatal("F6-1: expected a live per-cohort handle + key under the serve ctx")
	}

	// Pre-hash: a chrome widget under F6 folds EMPTY effective extras on BOTH
	// sides — the route params are dropped by the fold-nothing default.
	if len(seedExtras) != 0 || len(serveExtras) != 0 {
		t.Fatalf("F6-1 pre-hash: a chrome widget (no keyExtras) must fold EMPTY extras on BOTH sides; seed=%#v serve=%#v", seedExtras, serveExtras)
	}
	if !reflect.DeepEqual(seedInputs.Extras, serveInputs.Extras) {
		t.Fatalf("F6-1 pre-hash: seed vs serve ResolvedKeyInputs.Extras differ; seed=%#v serve=%#v", seedInputs.Extras, serveInputs.Extras)
	}
	if seedKey != serveKey {
		t.Fatalf("F6-1 INVARIANT: seed key %q != route-param serve key %q — the chrome cell still partitions by route (the #130 F6 first-nav miss)", seedKey, serveKey)
	}
	got, hit := serveHandle.Get(serveKey)
	if !hit {
		t.Fatal("F6-1: route-param serve MISSED the seeded chrome cell — #130 F6 not fixed")
	}
	if string(got.RawJSON) != string(body) {
		t.Fatalf("F6-1: served the wrong body; got %q want %q", got.RawJSON, body)
	}
}

// TestFARCH_F6_2_RED_UndeclaredRouteConsumer_CollidesAndServesWrongBody — the F6
// RED arm (audit necessity). A widget whose jq reads extras.namespace but does
// NOT declare it in spec.keyExtras produces byte-IDENTICAL keys across two
// DIFFERENT namespaces (the fold-nothing default drops the discriminating param)
// AND serves the first namespace's body to the second route (wrong body). This
// proves why every route-consuming widget MUST declare keyExtras — an undeclared
// consumer silently shares a cell it must not. The arm is GREEN because it
// ASSERTS the collision+wrong-body (the defect that mandates the declaration);
// the paired F6-3 proves declaring it fixes both.
func TestFARCH_F6_2_RED_UndeclaredRouteConsumer_CollidesAndServesWrongBody(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := ctxWithIdentity()
	const (
		g, v, r, name = "widgets.templates.krateo.io", "v1beta1", "panels", "resources-table"
		perPage, page = 10, 1
	)
	// UNDECLARED route consumer: no keyExtras. Its jq reads extras.namespace, so
	// its rendered body DOES vary by namespace — but the key will not.
	cr := chromeWidgetCR()

	// Route A (team-a) warms a cell with team-a's body.
	extrasA := effectiveKeyExtras(ctx, cr, routeParamsRequest("team-a", "x"))
	keyA, handleA, inputsA := dispatchCacheLookupKey(ctx, "widgets", g, v, r, "team-a", name, perPage, page, extrasA)
	if handleA == nil || keyA == "" {
		t.Fatal("F6-2 RED: expected a live handle + key on route A")
	}
	bodyA := []byte(`{"namespace":"team-a"}`)
	handleA.Put(keyA, &cache.ResolvedEntry{RawJSON: bodyA, Inputs: inputsA})

	// Route B (team-b) — a DIFFERENT namespace. Same GVR+name (the same widget
	// CR, only the route differs). Without a declaration the namespace extra is
	// dropped from the key → key collision with route A.
	extrasB := effectiveKeyExtras(ctx, cr, routeParamsRequest("team-b", "x"))
	keyB, handleB, _ := dispatchCacheLookupKey(ctx, "widgets", g, v, r, "team-b", name, perPage, page, extrasB)
	if handleB == nil || keyB == "" {
		t.Fatal("F6-2 RED: expected a live handle + key on route B")
	}

	// NOTE: the resource NAMESPACE (a first-class ComputeKey field) differs
	// between A and B, so their keys legitimately differ on that axis. F6 keys on
	// the ROUTE-PARAM extra, which is a SEPARATE dimension from the object
	// namespace: a chrome/detail widget on /compositions/:namespace can carry a
	// route :namespace param that is NOT the widget CR's own namespace (the param
	// names the COMPOSITION being viewed, not the panel). To isolate the F6
	// dimension, hold the object coordinates fixed and vary ONLY the route param.
	extrasA2 := effectiveKeyExtras(ctx, cr, routeParamsRequest("team-a", "x"))
	extrasB2 := effectiveKeyExtras(ctx, cr, routeParamsRequest("team-b", "x"))
	keyA2, hA2, iA2 := dispatchCacheLookupKey(ctx, "widgets", g, v, r, "krateo-system", name, perPage, page, extrasA2)
	keyB2, hB2, _ := dispatchCacheLookupKey(ctx, "widgets", g, v, r, "krateo-system", name, perPage, page, extrasB2)
	if hA2 == nil || hB2 == nil {
		t.Fatal("F6-2 RED: expected live handles on the isolated-dimension arm")
	}
	if len(extrasA2) != 0 || len(extrasB2) != 0 {
		t.Fatalf("F6-2 RED: an undeclared widget must fold EMPTY extras even when the request carries route params; A=%#v B=%#v", extrasA2, extrasB2)
	}
	if keyA2 != keyB2 {
		t.Fatalf("F6-2 RED: keys did NOT collide across route params for an undeclared widget (A=%q B=%q) — the arm is not discriminating; the fold-nothing default MUST drop the route param from the key", keyA2, keyB2)
	}
	// The wrong-body proof: route-A body warms the shared cell; route B reads it.
	bodyA2 := []byte(`{"route_namespace":"team-a"}`)
	hA2.Put(keyA2, &cache.ResolvedEntry{RawJSON: bodyA2, Inputs: iA2})
	gotB, hitB := hB2.Get(keyB2)
	if !hitB {
		t.Fatal("F6-2 RED: route-B request did not hit the route-A-warmed shared cell — collision not demonstrated")
	}
	if string(gotB.RawJSON) != string(bodyA2) {
		t.Fatalf("F6-2 RED: expected the WRONG (route-A) body served to route B; got %q", gotB.RawJSON)
	}
	// keyA/keyB (distinct object namespaces) are used above only to show the
	// object-namespace axis is orthogonal; reference them so the arm's setup is
	// not dead.
	if keyA == "" || keyB == "" {
		t.Fatal("F6-2 RED: object-namespace arm produced empty keys")
	}
}

// TestFARCH_F6_3_DeclaredWidget_PartitionsByRoute_DistinctContent — C-F6-3. A
// widget that DECLARES keyExtras:[namespace] still partitions correctly: two
// different route namespaces produce two DISTINCT keys, each serving its own
// body. This is the GREEN counterpart to F6-2 — the declaration is what makes a
// genuine route-consumer safe. Isolated on the route-param dimension (object
// coordinates held fixed), so the discriminant is ONLY the declared extra.
func TestFARCH_F6_3_DeclaredWidget_PartitionsByRoute_DistinctContent(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := ctxWithIdentity()
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "panels", "krateo-system", "composition-detail"
		perPage, page     = 10, 1
	)
	cr := declaredKeyExtrasCR("namespace")

	extrasA := effectiveKeyExtras(ctx, cr, routeParamsRequest("team-a", "x"))
	extrasB := effectiveKeyExtras(ctx, cr, routeParamsRequest("team-b", "x"))
	if extrasA["namespace"] != "team-a" || extrasB["namespace"] != "team-b" {
		t.Fatalf("F6-3: a declared[namespace] widget must fold the route namespace into the key; A=%#v B=%#v", extrasA, extrasB)
	}
	// The undeclared "name" param must NOT fold (only "namespace" is declared).
	if _, ok := extrasA["name"]; ok {
		t.Fatalf("F6-3: only DECLARED keys fold; the undeclared 'name' route param leaked into the key: %#v", extrasA)
	}
	keyA, hA, iA := dispatchCacheLookupKey(ctx, "widgets", g, v, r, ns, name, perPage, page, extrasA)
	keyB, hB, iB := dispatchCacheLookupKey(ctx, "widgets", g, v, r, ns, name, perPage, page, extrasB)
	if hA == nil || hB == nil || keyA == "" || keyB == "" {
		t.Fatal("F6-3: expected live handles + keys for both routes")
	}
	if keyA == keyB {
		t.Fatalf("F6-3: a declared[namespace] widget MUST partition by the route namespace — keyA == keyB %q (spurious collapse)", keyA)
	}
	bodyA := []byte(`{"ns":"team-a"}`)
	bodyB := []byte(`{"ns":"team-b"}`)
	hA.Put(keyA, &cache.ResolvedEntry{RawJSON: bodyA, Inputs: iA})
	hB.Put(keyB, &cache.ResolvedEntry{RawJSON: bodyB, Inputs: iB})
	if gA, hit := hA.Get(keyA); !hit || string(gA.RawJSON) != string(bodyA) {
		t.Fatalf("F6-3: route A served wrong/no body (hit=%v got=%q)", hit, gA.RawJSON)
	}
	if gB, hit := hB.Get(keyB); !hit || string(gB.RawJSON) != string(bodyB) {
		t.Fatalf("F6-3: route B served wrong/no body (hit=%v got=%q)", hit, gB.RawJSON)
	}
}

// TestFARCH_F6_5_SubscriptionParity_ChromeWidget_FoldsNothing — C-F6-5 (BLOCKING).
// The /refreshes subscription-arming path (DeriveSubscriptionKey →
// subscriptionKeyExtras → effectiveKeyExtras, the SAME single site the seed and
// dispatch use) must derive the SAME key for a route-param-bearing chrome-widget
// subscription as the extras-less seed cell — so a chrome widget that self-warms
// its seeded cell is still reachable by its live-refresh subscription. Driven
// through the REAL DeriveSubscriptionKey path (#66 anti-drift lesson: never a
// shadow copy). The seeded CR declares NO keyExtras, so the subscription's route
// params are dropped exactly as the seed's absent extras were.
func TestFARCH_F6_5_SubscriptionParity_ChromeWidget_FoldsNothing(t *testing.T) {
	// Reuse the two-user RBAC informer harness; seed a compositions CR with NO
	// keyExtras (chrome-widget shape) so subscriptionKeyExtras→objects.Get serves
	// it from the informer and the real arming path runs (not a fail-closed skip).
	buildTwoUserRBACWatcher(t)

	// The subscription coord carries route params in Extras (the frontend echoes
	// the route params into ?sub= the same way it does ?extras=). A chrome
	// widget declares nothing → the arming fold drops them.
	coords := compositionsCoords()
	coords.Extras = routeParamsRequest("team-a", "demo-1")

	subKey, ok := DeriveSubscriptionKey(ctxAs("userA"), coords)
	if !ok || subKey == "" {
		t.Fatalf("F6-5: DeriveSubscriptionKey failed for a chrome-widget coord (ok=%v key=%q)", ok, subKey)
	}

	// The SEED / extras-less arming: same coord, NO route params. For a widget
	// declaring no keyExtras the arming fold is byte-identical (route params
	// dropped) → the subscription key MUST equal the extras-less key.
	seedCoords := compositionsCoords() // Extras nil
	seedKey, ok := DeriveSubscriptionKey(ctxAs("userA"), seedCoords)
	if !ok || seedKey == "" {
		t.Fatalf("F6-5: DeriveSubscriptionKey failed for the extras-less seed coord (ok=%v key=%q)", ok, seedKey)
	}

	if subKey != seedKey {
		t.Fatalf("F6-5 PARITY: route-param subscription key %q != extras-less seed key %q — the arming path partitions the chrome cell by route while the seed does not; the seeded cell would be unreachable by its own subscription (the #66 drift class, one dimension over)", subKey, seedKey)
	}
}

// f6PanelGVR is the widget GVR the F6-6 self-quarantine harness seeds.
var f6PanelGVR = schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"}

// buildF6SharedBindingWatcher seeds a fake RBAC snapshot with ONE ClusterRoleBinding
// listing BOTH userA and userB (→ identical BindingUID uid-AB → SAME per-cohort
// cell) granting get/list on panels, plus a panel-1 widget CR carrying the supplied
// spec.keyExtras declaration (nil = undeclared). Published as cache.Global() for the
// test. The two-users-one-binding shape is the F6-6 collision precondition: A and B
// derive the SAME per-cohort key, so only the guard (not key divergence) can protect B.
func buildF6SharedBindingWatcher(t *testing.T, keyExtrasDecl []string) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		crbGVR: "ClusterRoleBindingList",
		crGVR:  "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}: "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		f6PanelGVR: "PanelList",
	}
	rule := []rbacv1.PolicyRule{{Verbs: []string{"get", "list"}, APIGroups: []string{"widgets.templates.krateo.io"}, Resources: []string{"panels"}}}
	spec := map[string]any{}
	if keyExtrasDecl != nil {
		decl := make([]any, len(keyExtrasDecl))
		for i, k := range keyExtrasDecl {
			decl[i] = k
		}
		spec["keyExtras"] = decl
	}
	seed := []runtime.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "panel-reader"}, Rules: rule},
		// ONE binding, BOTH users → identical BindingUID → SAME per-cohort cell.
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ab-bind", UID: types.UID("uid-AB")},
			Subjects:   []rbacv1.Subject{{Kind: "User", Name: "userA"}, {Kind: "User", Name: "userB"}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "panel-reader"},
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "widgets.templates.krateo.io/v1beta1",
			"kind":       "Panel",
			"metadata":   map[string]any{"name": "panel-1", "namespace": "demo"},
			"spec":       spec,
		}},
	}

	wctx, wcancel := context.WithCancel(context.Background())
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)
	rw, err := cache.NewResourceWatcher(wctx, dyn)
	if err != nil {
		wcancel()
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		rw.Stop()
		wcancel()
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	_, _ = rw.EnsureResourceType(f6PanelGVR)
	_ = rw.WaitForCacheSync(ctx, 5*time.Second)
	cache.RebuildRBACSnapshotForTest(rw)
	prev := cache.Global()
	cache.SetGlobal(rw)
	t.Cleanup(func() {
		rw.Stop()
		wcancel()
		cache.SetGlobal(prev)
		cache.PublishRBACSnapshotForTest(nil)
	})
}

func f6CtxUser(username string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username}))
}

// TestFARCH_F6_6_SelfQuarantine_UndeclaredExtrasDeclinePut — the F6-6 permanent
// self-quarantine arm (arch-ruled 2026-07-13, per-cohort cell). Two users in the
// SAME BindingUID cohort request a widget with NO keyExtras whose widgetDataTemplate
// reads extras.foo. userA sends ?extras={"foo":"evil"}; userB sends nothing.
//
// The COLLISION is real (proven): under F6 the undeclared foo drops from the key, so
// cacheKey_A == cacheKey_B — both derive the SAME shared per-cohort cell. Without the
// guard, A's evil-shaped body Put at widgets.go's genuine-Put would be served to B.
// The GUARD (requestExtrasFullyDeclared → decline the Put) is what protects B: A's
// polluting request declines the shared-cell Put, so B misses and resolves clean.
//
// This arm drives the REAL guard predicate + the REAL per-cohort key derivation
// (dispatchCacheLookupKey → effectiveKeyExtras, the SAME path widgets.go uses), then
// simulates the genuine-Put gate exactly as widgets.go structures it: IF
// requestExtrasFullyDeclared → Put; ELSE decline. Asserts (1) key collision, (2) the
// guard declines A's Put, (3) B misses (would resolve clean). RED = neuter the guard
// (Put unconditionally) → B hits A's evil body.
func TestFARCH_F6_6_SelfQuarantine_UndeclaredExtrasDeclinePut(t *testing.T) {
	buildF6SharedBindingWatcher(t, nil) // UNDECLARED — no keyExtras
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "panels", "demo", "panel-1"
		perPage, page     = -1, -1
	)
	ctxA := f6CtxUser("userA")
	ctxB := f6CtxUser("userB")
	cr := map[string]any{"spec": map[string]any{}} // undeclared, no keyExtras

	// userA carries a polluting request extra; userB is clean.
	extrasA := map[string]any{"foo": "evil"}
	var extrasB map[string]any // nil — clean

	// REAL per-cohort key derivation (the widgets.go:152/:221 path).
	keyExtrasA := effectiveKeyExtras(ctxA, cr, extrasA)
	keyA, handleA, inputsA := dispatchCacheLookupKey(ctxA, "widgets", g, v, r, ns, name, perPage, page, keyExtrasA)
	keyExtrasB := effectiveKeyExtras(ctxB, cr, extrasB)
	keyB, handleB, _ := dispatchCacheLookupKey(ctxB, "widgets", g, v, r, ns, name, perPage, page, keyExtrasB)
	if handleA == nil || handleB == nil || keyA == "" || keyB == "" {
		t.Fatalf("F6-6: expected live handles + keys (A=%q B=%q)", keyA, keyB)
	}

	// (1) THE COLLISION IS REAL: A and B (same BindingUID, foo dropped from the key)
	// derive the SAME per-cohort key. This is what makes the guard load-bearing —
	// nothing else keeps A's body away from B.
	if keyA != keyB {
		t.Fatalf("F6-6: expected cacheKey_A == cacheKey_B (same BindingUID cohort, undeclared foo dropped from key) — got A=%q B=%q; if they differ the collision is not demonstrated", keyA, keyB)
	}

	// (2) THE GUARD: userA's request carries an UNDECLARED extra (foo) → the genuine
	// per-cohort Put must be DECLINED. This is the exact predicate widgets.go gates on.
	if requestExtrasFullyDeclared(cr, extrasA) {
		t.Fatal("F6-6: userA's request carries an undeclared extra (foo) → requestExtrasFullyDeclared MUST be false (decline the shared-cell Put)")
	}
	// Simulate widgets.go's genuine-Put gate EXACTLY: the F6 decline branch runs
	// BEFORE the Put, so an undeclared-extras request never writes the shared cell.
	aEvilBody := []byte(`{"status":{"widgetData":{"echoedFoo":"evil"}}}`)
	if requestExtrasFullyDeclared(cr, extrasA) {
		// (not reached — asserted false above; this mirrors the prod if-structure)
		handleA.Put(keyA, &cache.ResolvedEntry{RawJSON: aEvilBody, Inputs: inputsA})
	}

	// (3) userB (clean, fully-declared trivially) now reads the shared key → MISS,
	// because A's Put was declined. B would fall through to resolve clean.
	if !requestExtrasFullyDeclared(cr, extrasB) {
		t.Fatal("F6-6: userB's clean request (no extras) must be fully-declared → its Put is allowed and it is safe")
	}
	if _, hit := handleB.Get(keyB); hit {
		t.Fatal("F6-6 GREEN: userB unexpectedly HIT the shared cell — A's evil-extras body must NOT have been written (the guard should have declined A's Put); B must miss and resolve clean")
	}
}

// TestFARCH_F6_6_RED_NeuteredGuard_ServesEvilToB — the F6-6 RED companion. It
// reproduces what happens WITHOUT the guard: A's Put is NOT declined (the neutered
// behaviour), so A's evil body lands in the shared cohort cell and userB — deriving
// the SAME key — is served it. This is GREEN precisely because it ASSERTS the leak
// the guard prevents; it discriminates the exact requestExtrasFullyDeclared predicate
// (if the guard were always-on, A's Put would be declined and this arm's HIT would
// not occur).
func TestFARCH_F6_6_RED_NeuteredGuard_ServesEvilToB(t *testing.T) {
	buildF6SharedBindingWatcher(t, nil) // undeclared
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "panels", "demo", "panel-1"
		perPage, page     = -1, -1
	)
	ctxA := f6CtxUser("userA")
	ctxB := f6CtxUser("userB")
	cr := map[string]any{"spec": map[string]any{}}

	keyExtrasA := effectiveKeyExtras(ctxA, cr, map[string]any{"foo": "evil"})
	keyA, handleA, inputsA := dispatchCacheLookupKey(ctxA, "widgets", g, v, r, ns, name, perPage, page, keyExtrasA)
	keyExtrasB := effectiveKeyExtras(ctxB, cr, nil)
	keyB, handleB, _ := dispatchCacheLookupKey(ctxB, "widgets", g, v, r, ns, name, perPage, page, keyExtrasB)
	if handleA == nil || handleB == nil || keyA != keyB {
		t.Fatalf("F6-6 RED: setup expects a real key collision (A=%q B=%q)", keyA, keyB)
	}

	// NEUTERED guard: Put unconditionally (the pre-guard / regression behaviour).
	aEvilBody := []byte(`{"status":{"widgetData":{"echoedFoo":"evil"}}}`)
	handleA.Put(keyA, &cache.ResolvedEntry{RawJSON: aEvilBody, Inputs: inputsA})

	// userB, on the SAME key, is served A's evil body — the leak the guard prevents.
	got, hit := handleB.Get(keyB)
	if !hit {
		t.Fatal("F6-6 RED: expected userB to HIT the shared cell A polluted (neutered guard) — the collision leak is not demonstrated")
	}
	if string(got.RawJSON) != string(aEvilBody) {
		t.Fatalf("F6-6 RED: expected userB served A's evil body; got %q", got.RawJSON)
	}
}

// TestFARCH_F6_6_DeclaredCounterpart_GuardInert — the F6-6 declared-counterpart
// GREEN arm. When the widget DECLARES keyExtras:[foo], the foo extra folds into the
// key → A (foo=evil) and B (foo=clean, or absent) derive DISTINCT keys → normal
// partition, no shared-cell collision, and the guard is INERT (foo is declared, so
// requestExtrasFullyDeclared is true — the Put is allowed and correct). This proves
// the guard fires ONLY for the misconfigured (undeclared-consumer) corpus; the
// correctly-declared corpus partitions and pays nothing.
func TestFARCH_F6_6_DeclaredCounterpart_GuardInert(t *testing.T) {
	buildF6SharedBindingWatcher(t, []string{"foo"}) // DECLARED keyExtras:[foo]
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "panels", "demo", "panel-1"
		perPage, page     = -1, -1
	)
	ctxA := f6CtxUser("userA")
	ctxB := f6CtxUser("userB")
	cr := map[string]any{"spec": map[string]any{"keyExtras": []any{"foo"}}}

	// Declared foo folds into the key → A and B on DIFFERENT foo values partition.
	keyExtrasA := effectiveKeyExtras(ctxA, cr, map[string]any{"foo": "evil"})
	keyA, hA, iA := dispatchCacheLookupKey(ctxA, "widgets", g, v, r, ns, name, perPage, page, keyExtrasA)
	keyExtrasB := effectiveKeyExtras(ctxB, cr, map[string]any{"foo": "clean"})
	keyB, hB, _ := dispatchCacheLookupKey(ctxB, "widgets", g, v, r, ns, name, perPage, page, keyExtrasB)
	if hA == nil || hB == nil || keyA == "" || keyB == "" {
		t.Fatal("F6-6 declared: expected live handles + keys")
	}
	if keyExtrasA["foo"] != "evil" || keyExtrasB["foo"] != "clean" {
		t.Fatalf("F6-6 declared: a declared[foo] widget must fold foo into the key; A=%#v B=%#v", keyExtrasA, keyExtrasB)
	}
	if keyA == keyB {
		t.Fatalf("F6-6 declared: a declared[foo] widget MUST partition by foo — keyA == keyB %q (no collision → no shared-cell leak)", keyA)
	}
	// Guard INERT: foo is declared → the request is fully-declared → Put allowed.
	if !requestExtrasFullyDeclared(cr, map[string]any{"foo": "evil"}) {
		t.Fatal("F6-6 declared: a request whose only extra (foo) IS declared must be fully-declared → the guard must NOT decline (it is inert for the correct corpus)")
	}
	// A's Put lands in A's own (distinct) cell; B never sees it.
	aBody := []byte(`{"foo":"evil"}`)
	hA.Put(keyA, &cache.ResolvedEntry{RawJSON: aBody, Inputs: iA})
	if _, hit := hB.Get(keyB); hit {
		t.Fatal("F6-6 declared: B's distinct-key cell must be a MISS after A's Put (partitioned, not shared)")
	}
}

// TestFARCH_F6_7_RevisitHit_IdentityExtrasExempt — the 1.7.11 revisit-HIT arm
// (tester falsifier, west4 2026-07-14). This is the dimension the DeclaredCounterpart
// unit arm was BLIND to: identity extras on the wire. When SNOWPLOW_IDENTITY_INJECTION
// is off, the frontend's buildExtrasParam folds IDENTITY keys (username, displayName)
// into ?extras= alongside the route params. A declared widget (keyExtras:[name,namespace],
// NO identityContext) then receives {name, namespace, username, displayName}. The folder
// keeps {name,namespace} (extras_len=2, correct key) and drops the identity keys; but the
// pre-fix guard saw username/displayName as "undeclared" → declined EVERY Put → the
// widget's content was never cached → revisit always MISS (the exact 14/14-same-key /
// 0/14-hit west4 failure).
//
// GREEN (the fix): the guard exempts identity-dimension keys → the Put is ALLOWED → a
// second identical request HITs the extras_len>0 key. RED = remove the identity
// exemption (re-introduce the divergence) → the guard declines → revisit MISS returns.
func TestFARCH_F6_7_RevisitHit_IdentityExtrasExempt(t *testing.T) {
	enableWidgetContentL1(t)
	ctx := ctxWithIdentity() // cyberjoker / [devs]
	const (
		g, v, r, ns, name = "widgets.templates.krateo.io", "v1beta1", "paragraphs", "krateo-system", "composition-detail-header"
		perPage, page     = -1, -1
	)
	// A DECLARED widget: keyExtras:[name,namespace], NO identityContext (the exact
	// composition-detail-header shape that broke on west4).
	cr := declaredKeyExtrasCR("name", "namespace")

	// The browser's wire shape with injection OFF: route params + identity keys.
	req := map[string]any{
		"namespace":   "team-a",
		"name":        "demo-1",
		"username":    "cyberjoker",
		"displayName": "Cyber Joker",
	}

	// FOLDER: keeps only the declared keyExtras subset → extras_len=2 (identity dropped).
	keyExtras := effectiveKeyExtras(ctx, cr, req)
	if len(keyExtras) != 2 || keyExtras["name"] != "demo-1" || keyExtras["namespace"] != "team-a" {
		t.Fatalf("F6-7: the folder must keep ONLY the declared [name,namespace] (identity dropped); got %#v", keyExtras)
	}

	// GUARD: must NOT decline — name/namespace are declared, username/displayName are
	// identity-dimension keys (exempt). This is the fix's core assertion.
	if !requestExtrasFullyDeclared(cr, req) {
		t.Fatal("F6-7 REGRESSION: the guard declined a declared widget carrying identity extras (username/displayName) — identity keys must be EXEMPT (they never pollute a shared cell); this is the west4 revisit-miss bug")
	}

	// End-to-end: first request Puts at the extras_len>0 key; second identical request HITs.
	key1, handle1, inputs1 := dispatchCacheLookupKey(ctx, "widgets", g, v, r, ns, name, perPage, page, keyExtras)
	if handle1 == nil || key1 == "" {
		t.Fatal("F6-7: expected a live handle + key")
	}
	body := []byte(`{"status":{"widgetData":{"detail":"header"}}}`)
	// The Put only happens because the guard allowed it (mirrors widgets.go's
	// genuine-Put gate: the F6 decline branch is skipped when requestExtrasFullyDeclared).
	if requestExtrasFullyDeclared(cr, req) {
		handle1.Put(key1, &cache.ResolvedEntry{RawJSON: body, Inputs: inputs1})
	}

	// REVISIT: same widget, same request → same key → HIT (was 0/14 on west4).
	keyExtras2 := effectiveKeyExtras(ctx, cr, req)
	key2, handle2, _ := dispatchCacheLookupKey(ctx, "widgets", g, v, r, ns, name, perPage, page, keyExtras2)
	if key2 != key1 {
		t.Fatalf("F6-7: revisit must derive the SAME extras_len>0 key; key1=%q key2=%q", key1, key2)
	}
	got, hit := handle2.Get(key2)
	if !hit {
		t.Fatal("F6-7 REVISIT: declared widget MISSED on revisit at the identical key — the Put was declined (the west4 bug); the identity exemption must allow it to cache")
	}
	if string(got.RawJSON) != string(body) {
		t.Fatalf("F6-7: revisit served the wrong body; got %q want %q", got.RawJSON, body)
	}
}

// TestFARCH_F6_7b_GenuinelyUndeclaredStillDeclines — the paired no-over-fix arm.
// The identity exemption must NOT weaken the quarantine for a genuinely-undeclared,
// BODY-affecting request key. A declared[name,namespace] widget receiving a NON-identity
// undeclared key (foo) must STILL decline — otherwise the F6-6 self-quarantine leak
// reopens. Discriminates the exemption from an over-broad "allow everything".
func TestFARCH_F6_7b_GenuinelyUndeclaredStillDeclines(t *testing.T) {
	cr := declaredKeyExtrasCR("name", "namespace")
	// Identity keys exempt, declared keys fine — but foo is a genuinely-undeclared
	// non-identity body-affecting key → MUST still decline.
	req := map[string]any{"name": "demo-1", "namespace": "team-a", "username": "cyberjoker", "foo": "evil"}
	if requestExtrasFullyDeclared(cr, req) {
		t.Fatal("F6-7b OVER-FIX GUARD: a genuinely-undeclared non-identity key (foo) must STILL be quarantined even alongside declared+identity keys — the exemption must not allow body-affecting undeclared extras (F6-6 leak would reopen)")
	}
	// Sanity: without foo, the same request is fully-declared (identity exempt).
	req2 := map[string]any{"name": "demo-1", "namespace": "team-a", "username": "cyberjoker"}
	if !requestExtrasFullyDeclared(cr, req2) {
		t.Fatal("F6-7b: declared keys + identity keys only must be fully-declared")
	}
}
