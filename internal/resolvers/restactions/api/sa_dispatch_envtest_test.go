//go:build envtest
// +build envtest

// Q-RBAC-DECOUPLE C(d) v6 — Path B byte-equivalence envtest
// (audit 2026-05-04).
//
// This test asserts that the v6 Path B SA dispatch (client-go dynamic
// client → json.Marshal) produces JSON bytes that decode to the SAME
// data structure as v5 Path A's httpcall.Do dispatch against the same
// real K8s apiserver. The contract is "wire-shape compatible": the JQ
// filters in the 6 portal-cache YAMLs MUST keep working unchanged
// because they parse `{apiVersion, kind, items: [...]}` shape — and
// both dispatch paths produce bytes with that shape.
//
// Why this is the load-bearing v6 regression test:
// the v5 D1 fix (host-pin to kubernetes.default.svc) silently degraded
// to 200-with-`[]` via D3a graceful-degradation when TLS handshake
// failed — operators saw "no missing namespaces", but the apiserver
// was actually never queried. v6 closes that hole structurally; this
// test catches any future drift between Path A and Path B (e.g. a
// future kube-client release changes UnstructuredList wire shape, or
// a snowplow change to ParseK8sAPIPath GVR mapping).
//
// Build/run:
//   export KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir ~/.envtest -p path)
//   go test -tags envtest -timeout 5m -run TestSADispatch ./internal/resolvers/restactions/api/

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	plumbingendpoints "github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	httpcall "github.com/krateoplatformops/snowplow/internal/httpcall"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TestSADispatch_ByteEquivalence_PathA_vs_PathB asserts:
//
//   1. Both Path A (httpcall.Do via SnowplowEndpointFromConfig) and
//      Path B (dispatchSAViaClientGo via dynamic.NewClient) succeed
//      against the same envtest apiserver.
//   2. The decoded data structures are equivalent: same `kind`, same
//      `apiVersion`, same `items[].metadata.name` set.
//
// The byte-level comparison uses a normalized form (json.Unmarshal →
// reflect.DeepEqual) because:
//   - apiserver wire output uses `kind: NamespaceList` `apiVersion: v1`;
//     client-go's `*unstructured.UnstructuredList` is marshaled the same
//     way (UnstructuredList.MarshalJSON delegates to the underlying
//     map[string]any).
//   - field ordering between encoders may differ; what matters for the
//     downstream JQ filters is the parsed-tree equality.
//
// The test deliberately picks a list verb (Namespaces) and a get verb
// (a specific Namespace) so both code paths in dispatchSAViaClientGo
// are exercised.
//
// Path A here goes through plumbing's `HasCertAuth() == true` branch
// because envtest's rest.Config carries a client cert+key. So Path A
// in this test is a positive baseline (working), NOT a reproduction of
// the v5 token-auth bug. The TLS-handshake regression catcher lives in
// internal/httpcall/do_tls_test.go which uses a token-auth+CA endpoint
// (the production-pod shape).
func TestSADispatch_ByteEquivalence_PathA_vs_PathB(t *testing.T) {
	// Test mode flag is required so SnowplowEndpointFromConfig copies
	// rc.Host (the envtest URL with random port) instead of pinning to
	// https://kubernetes.default.svc — the latter wouldn't resolve
	// outside an in-cluster pod.
	env.SetTestMode(true)
	t.Cleanup(func() { env.SetTestMode(false) })

	cfg, teardown := startEnvtest(t)
	defer teardown()

	// Seed: 5 namespaces. We don't need the full RBAC scenario for byte
	// equivalence — both Path A and Path B run as the envtest's
	// default admin-equivalent user, so RBAC denials don't enter the
	// equation. RBAC behaviour is tested in the existing user-access
	// filter envtest; this test is about wire-shape parity.
	cli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig: %v", err)
	}
	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelSeed()
	wantNS := []string{"v6-test-a", "v6-test-b", "v6-test-c"}
	for _, ns := range wantNS {
		_, cerr := cli.CoreV1().Namespaces().Create(seedCtx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}, metav1.CreateOptions{})
		if cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			t.Fatalf("create namespace %q: %v", ns, cerr)
		}
	}

	dispatchCtx, cancelDispatch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelDispatch()

	// ── Path A: httpcall.Do via manually-built endpoint ─────────────
	// envtest's rest.Config carries client cert+key+CA but no bearer
	// token, so SnowplowEndpointFromConfig (which requires a token)
	// would error. Build the endpoint directly from the rest.Config
	// fields to drive the same plumbing TLS code path that production
	// would use, with whatever auth shape envtest provides. This still
	// validates the wire-shape parity assertion: Path A returns
	// apiserver bytes, Path B returns dynamic-client bytes, both
	// decode to equivalent trees.
	// Plumbing's tlsConfigFor expects base64-encoded PEM (a layer of
	// base64 ON TOP of the PEM that K8s Secrets carry). Match that
	// encoding so the existing plumbing code path is exercised
	// faithfully — same as production reads from a clientconfig
	// Secret. The CertData/KeyData/CAData fields on rest.Config are
	// already raw PEM, so we b64-encode them once here.
	ep := &plumbingendpoints.Endpoint{
		ServerURL:                cfg.Host,
		CertificateAuthorityData: base64.StdEncoding.EncodeToString(cfg.TLSClientConfig.CAData),
		ClientCertificateData:    base64.StdEncoding.EncodeToString(cfg.TLSClientConfig.CertData),
		ClientKeyData:            base64.StdEncoding.EncodeToString(cfg.TLSClientConfig.KeyData),
		Insecure:                 cfg.TLSClientConfig.Insecure,
	}

	pathAList, errA := dispatchPathA(dispatchCtx, ep, "/api/v1/namespaces")
	if errA != nil {
		t.Fatalf("Path A list dispatch failed: %v", errA)
	}

	// ── Path B: dynamic.NewClient + dispatchSAViaClientGo ─────────────
	dynCli, derr := dynamic.NewClient(cfg)
	if derr != nil {
		t.Fatalf("dynamic.NewClient: %v", derr)
	}
	pathBList := dispatchSAViaClientGo(dispatchCtx, dynCli, "/api/v1/namespaces")
	if pathBList.err != nil {
		t.Fatalf("Path B dispatch failed: %v", pathBList.err)
	}

	// ── Byte-tree equivalence ────────────────────────────────────────
	if drift := compareJSONTrees(pathAList, pathBList.raw); drift != "" {
		t.Fatalf("Path A vs Path B drift on /api/v1/namespaces (BUG):\n%s", drift)
	}
	t.Logf("byte-tree equivalence OK on /api/v1/namespaces (Path A %d bytes, Path B %d bytes)",
		len(pathAList), len(pathBList.raw))

	// Sanity: assert both paths returned the seeded namespaces.
	pathANames := extractItemNames(t, pathAList)
	pathBNames := extractItemNames(t, pathBList.raw)
	if !containsAll(pathANames, wantNS) {
		t.Fatalf("Path A list missing seeded namespaces: got %v want subset %v", pathANames, wantNS)
	}
	if !containsAll(pathBNames, wantNS) {
		t.Fatalf("Path B list missing seeded namespaces: got %v want subset %v", pathBNames, wantNS)
	}

	// ── Single-item GET (the .Get() branch of dispatchSAViaClientGo) ─
	const probeNS = "v6-test-a"
	pathAGet, errAG := dispatchPathA(dispatchCtx, ep, "/api/v1/namespaces/"+probeNS)
	if errAG != nil {
		t.Fatalf("Path A GET dispatch failed: %v", errAG)
	}
	pathBGet := dispatchSAViaClientGo(dispatchCtx, dynCli, "/api/v1/namespaces/"+probeNS)
	if pathBGet.err != nil {
		t.Fatalf("Path B GET dispatch failed: %v", pathBGet.err)
	}
	if drift := compareJSONTrees(pathAGet, pathBGet.raw); drift != "" {
		t.Fatalf("Path A vs Path B drift on /api/v1/namespaces/%s (BUG):\n%s",
			probeNS, drift)
	}
	t.Logf("byte-tree equivalence OK on /api/v1/namespaces/%s (Path A %d bytes, Path B %d bytes)",
		probeNS, len(pathAGet), len(pathBGet.raw))
}

// TestSADispatch_PathB_NotFound_Mapping verifies the error-shape adapter
// in clientGoErrorToStatus produces a *response.Status whose Code field
// matches what httpcall.Do would emit on the same condition (apiserver
// 404).
func TestSADispatch_PathB_NotFound_Mapping(t *testing.T) {
	env.SetTestMode(true)
	t.Cleanup(func() { env.SetTestMode(false) })

	cfg, teardown := startEnvtest(t)
	defer teardown()

	dynCli, derr := dynamic.NewClient(cfg)
	if derr != nil {
		t.Fatalf("dynamic.NewClient: %v", derr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := dispatchSAViaClientGo(ctx, dynCli, "/api/v1/namespaces/does-not-exist-v6")
	if res.err == nil {
		t.Fatalf("expected NotFound error from Path B dispatch")
	}
	if res.status == nil {
		t.Fatalf("clientGoErrorToStatus returned nil status; expected populated *response.Status")
	}
	if res.status.Code != http.StatusNotFound {
		t.Fatalf("Path B NotFound mapping: status.Code=%d want %d (StatusNotFound)",
			res.status.Code, http.StatusNotFound)
	}
	if res.status.Status != response.StatusFailure {
		t.Fatalf("Path B NotFound mapping: status.Status=%q want %q",
			res.status.Status, response.StatusFailure)
	}
	t.Logf("NotFound error mapped to apiserver-style status: code=%d reason=%q message=%q",
		res.status.Code, res.status.Reason, res.status.Message)
}

// dispatchPathA drives a GET via httpcall.Do (the v5 production code
// path) against the envtest apiserver. Returns the raw response body
// bytes — the same bytes that the v5 production resolver feeds to
// jsonHandlerCompute.
func dispatchPathA(ctx context.Context, ep *plumbingendpoints.Endpoint, path string) ([]byte, error) {
	if ep == nil {
		return nil, fmt.Errorf("nil endpoint")
	}
	verb := http.MethodGet
	var raw []byte
	res := httpcall.Do(ctx, httpcall.RequestOptions{
		Endpoint: ep,
		RequestInfo: httpcall.RequestInfo{
			Path: path,
			Verb: ptr.To(verb),
		},
		ResponseHandler: func(r io.ReadCloser) error {
			b, rerr := io.ReadAll(r)
			if rerr != nil {
				return rerr
			}
			raw = b
			return nil
		},
	})
	if res.Status == response.StatusFailure {
		return nil, fmt.Errorf("Path A httpcall.Do failed: code=%d message=%q", res.Code, res.Message)
	}
	return raw, nil
}

// compareJSONTrees decodes both blobs to map[string]any and compares
// using reflect.DeepEqual. Returns "" if equivalent, otherwise a short
// diagnostic string. Pure-tree comparison ignores field ordering and
// whitespace — what matters for downstream JQ filters is parsed-tree
// equality, not textual byte equality.
func compareJSONTrees(a, b []byte) string {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return "Path A bytes do not decode as JSON: " + err.Error()
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return "Path B bytes do not decode as JSON: " + err.Error()
	}
	// Strip volatile fields that may differ across calls but don't
	// affect downstream JQ filter behaviour: resourceVersion,
	// managedFields, creationTimestamp, uid. Apiserver may bump these
	// between Path A and Path B requests because they're separate
	// calls; what we care about is kind/items/metadata.name shape.
	stripVolatile(av)
	stripVolatile(bv)
	if reflect.DeepEqual(av, bv) {
		return ""
	}
	return formatDriftSummary(av, bv)
}

// stripVolatile recursively removes fields whose values may differ
// across calls. Walks both maps and slices.
//
// We strip ONLY at non-top levels so the load-bearing kind/apiVersion
// at the root of the response (consumed by JQ filters in the 6
// portal-cache YAMLs) are still asserted. Inside items, kind/apiVersion
// may or may not be present depending on the apiserver build —
// stripping makes the comparison robust to that detail.
func stripVolatile(v any) {
	stripVolatileWalk(v, true /* topLevel */)
}

func stripVolatileWalk(v any, topLevel bool) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "resourceVersion")
		delete(t, "managedFields")
		delete(t, "creationTimestamp")
		delete(t, "uid")
		delete(t, "selfLink")
		delete(t, "generation")
		if !topLevel {
			delete(t, "kind")
			delete(t, "apiVersion")
		}
		for _, sv := range t {
			stripVolatileWalk(sv, false)
		}
	case []any:
		for _, item := range t {
			stripVolatileWalk(item, false)
		}
	}
}

func formatDriftSummary(a, b any) string {
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	akeys := mapKeys(am)
	bkeys := mapKeys(bm)
	sort.Strings(akeys)
	sort.Strings(bkeys)
	out := ""
	out += "  pathA top-level keys: " + strings.Join(akeys, ",") + "\n"
	out += "  pathB top-level keys: " + strings.Join(bkeys, ",") + "\n"
	if ai, ok := am["items"].([]any); ok {
		out += "  pathA items count: " + strconv.Itoa(len(ai)) + "\n"
	}
	if bi, ok := bm["items"].([]any); ok {
		out += "  pathB items count: " + strconv.Itoa(len(bi)) + "\n"
	}
	if ak, ok := am["kind"].(string); ok {
		out += "  pathA kind: " + ak + "\n"
	}
	if bk, ok := bm["kind"].(string); ok {
		out += "  pathB kind: " + bk + "\n"
	}
	return out
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func extractItemNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("extractItemNames: %v", err)
	}
	items, ok := v["items"].([]any)
	if !ok {
		// single-item shape (Get) — no "items" array.
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		md, _ := m["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func containsAll(haystack, needles []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}
