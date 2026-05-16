// internal_dispatch.go — 0.30.104 Phase-1 TLS-CA fix.
//
// THE BUG (0.30.103 flag-ON re-bench, reproduced on every boot):
//
//   Phase 1's SA-credentialed startup walk resolves the `routesloaders`
//   navigation root under the snowplow service-account identity. The
//   walk's FIRST step after resolving the root is an inner api[] stage
//   that LISTs `/api/v1/namespaces` — the resolver dispatches it through
//   plumbing's httpcall.Do, which builds the HTTP client from the
//   plumbing Endpoint shape via HTTPClientForEndpoint -> tlsConfigFor.
//
//   plumbing's tlsConfigFor (http/request/transport.go) installs a custom
//   CA pool into RootCAs ONLY inside the `HasCertAuth()` branch. The
//   snowplow SA endpoint is TOKEN-auth (bearer JWT, no client cert), so
//   `HasCertAuth()` is false and tlsConfigFor returns at the
//   `!ep.HasCertAuth()` early-exit — the SA endpoint's
//   CertificateAuthorityData (the raw-PEM cluster CA) is NEVER applied.
//   The client then verifies the apiserver cert against the system root
//   store, which does not contain the cluster's self-signed CA:
//
//     ERROR api call response failure name=namespaces
//       path=/api/v1/namespaces
//       error="tls: failed to verify certificate: x509: certificate
//              signed by unknown authority"
//
//   The namespace LIST fails -> the walk never discovers the composition
//   GVR -> Phase 1 registers only the 8 infrastructure informers, not the
//   composition informer -> Phase 1 pre-warms nothing at scale.
//
//   plumbing is upstream (project_no_upstream_authority.md) — it cannot
//   be patched. plumbing's tlsConfigFor structurally has only two TLS
//   outcomes: Insecure (skip-verify) or HasCertAuth (build CA pool +
//   client cert). A token-auth endpoint carrying a custom CA cannot be
//   served by it at all.
//
// THE FIX (snowplow-side, behavior-neutral):
//
//   Phase 1 already attaches its SA *rest.Config — the value
//   rest.InClusterConfig() returns, which carries the cluster CA and the
//   SA bearer token with the correct in-cluster TLS semantics — to the
//   context via cache.WithInternalRESTConfig (used since 0.30.103 by the
//   objects.Get / resourcesrefs.Resolve fetch sites). 0.30.104 extends
//   that same context-carried *rest.Config to the api-stage K8s GET/LIST
//   dispatch: dispatchViaInternalRESTConfig is a sibling of
//   dispatchViaInformer wired into resolve.go's inner-call worker BEFORE
//   httpcall.Do. When the context carries an internal *rest.Config and
//   the call is a GET against an apiserver-shaped path, the dispatch goes
//   through a client-go dynamic client built from that *rest.Config —
//   client-go's transport installs the CA correctly. Every other call
//   shape (no internal config, non-GET verb, external/subresource path)
//   falls through to the unchanged httpcall.Do path.
//
//   BEHAVIOR-NEUTRAL: ordinary per-user requests NEVER set
//   cache.WithInternalRESTConfig, so dispatchViaInternalRESTConfig
//   immediately returns served=false for them and the path is byte-
//   identical to pre-0.30.104. The mechanism is keyed only on context
//   state — uniform across GVRs, no per-resource carve-out
//   (feedback_no_special_cases.md).
//
// CRITICAL — this path is validated ON-CLUSTER. Two prior Phase-1-SA
// fixes (0.30.102 base64, 0.30.103) passed unit tests and failed on the
// real cluster; a unit test cannot exercise the real cluster CA or a real
// apiserver TLS handshake. The falsifier in internal_dispatch_tls_test.go
// is necessary but not sufficient.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	xcontext "github.com/krateoplatformops/plumbing/context"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// internalClientCache memoises the client-go dynamic client built from a
// given internal *rest.Config. Phase 1's walk fans out many inner api[]
// calls all carrying the SAME SA *rest.Config pointer; rebuilding the
// dynamic client per call would re-create the TLS transport each time.
// Keyed on the *rest.Config pointer identity — Phase 1 builds exactly one
// SA config and attaches it verbatim, so the pointer is a stable cache
// key for the process lifetime.
//
// The dynamic client is used directly with the parsed GVR — NO RESTMapper
// / discovery round-trip. The resolver's inner-call path has already
// produced a fully-qualified apiserver REST path; ParseAPIServerPathToDep
// extracts the exact (group, version, resource), so k8sdynamic's
// Resource(gvr) is sufficient and avoids the apiserver discovery I/O that
// a typed/mapped client would incur.
//
// Concurrency: resolve.go's inner-call worker is a bounded errgroup, so
// dispatchViaInternalRESTConfig is called from parallel goroutines. The
// map is guarded by internalClientMu. The cached dynamic.Interface itself
// is safe for concurrent use.
var (
	internalClientMu    sync.Mutex
	internalClientCache = map[*rest.Config]k8sdynamic.Interface{}
)

// internalClientFor returns a memoised client-go dynamic client for rc,
// building one on first use. rc carries the cluster CA + SA bearer token
// verbatim (it is the rest.InClusterConfig() value) so client-go's
// NewForConfig produces a transport that trusts the cluster CA. This is
// the load-bearing difference from plumbing's httpcall.Do path, whose
// tlsConfigFor drops the CA for token-auth endpoints.
func internalClientFor(rc *rest.Config) (k8sdynamic.Interface, error) {
	internalClientMu.Lock()
	defer internalClientMu.Unlock()
	if cli, ok := internalClientCache[rc]; ok {
		return cli, nil
	}
	cli, err := k8sdynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	internalClientCache[rc] = cli
	return cli, nil
}

// resetInternalClientCacheForTest clears the memoised client map.
// TEST-ONLY — the production cache is set-once-per-config and never
// cleared. Exported within-package for the falsifier test.
func resetInternalClientCacheForTest() {
	internalClientMu.Lock()
	defer internalClientMu.Unlock()
	internalClientCache = map[*rest.Config]k8sdynamic.Interface{}
}

// dispatchViaInternalRESTConfig attempts to serve `call` through a
// client-go dynamic client built from the context-carried internal
// *rest.Config (cache.WithInternalRESTConfig). It is the api-stage
// sibling of dispatchViaInformer and is wired into resolve.go's inner-
// call worker BEFORE httpcall.Do.
//
// Returns (rawBytes, true, nil) when the call was served — the caller
// feeds rawBytes to call.ResponseHandler exactly as it does for the
// informer-pivot branch. Returns (nil, false, nil) for every gate that
// must take the unchanged httpcall.Do path:
//
//   - no internal *rest.Config on the context (every ordinary per-user
//     request — the behavior-neutral invariant);
//   - the context value is the wrong type / a nil pointer;
//   - non-GET verb (POST/PUT/PATCH/DELETE — client-go dynamic Get/List
//     here is read-only; writes are not a Phase-1 shape and stay on the
//     unchanged path);
//   - a non-apiserver path (external URL, JQ-leaked `${...}`, malformed
//     shape) — the internal dispatcher only owns apiserver GVR paths;
//   - a subresource path (.../status, .../scale, ...) — no dynamic-Get
//     shape, same gate as the informer pivot.
//
// Returns (nil, false, err) ONLY when the apiserver call itself errored
// after the dispatcher committed to serving (client build failed, or the
// Get/List returned a non-recoverable error). resolve.go treats a non-nil
// err here exactly as it treats an httpcall.Do StatusFailure — it does
// NOT silently fall through to httpcall.Do, because that would just
// re-hit the same broken plumbing TLS path. Surfacing the error keeps
// the failure diagnosable (it is the real apiserver error, e.g. a 403 or
// a genuine connectivity fault) rather than masking it behind a second
// x509 error.
//
// On the served path the LIST output is wrapped in the apiserver LIST
// envelope (apiVersion/kind/items) and a GET-by-name returns the bare
// object — byte-equivalent to what httpcall.Do would have delivered, so
// the downstream JQ pipeline is invariant (feedback_cache_must_not_constrain_jq.md).
func dispatchViaInternalRESTConfig(ctx context.Context, call httpcall.RequestOptions) ([]byte, bool, error) {
	// Gate 1: internal *rest.Config present? Absent => ordinary per-user
	// request => behavior-neutral fall-through to httpcall.Do.
	v, ok := cache.InternalRESTConfigFromContext(ctx)
	if !ok {
		return nil, false, nil
	}
	rc, rcOK := v.(*rest.Config)
	if !rcOK || rc == nil {
		// An internal driver attached the wrong shape (not a *rest.Config,
		// or a nil pointer). Fall through to httpcall.Do — but WARN: a
		// mis-wired internal driver's apiserver dispatches will silently
		// take the plumbing path, which drops the SA CA for a token-auth
		// endpoint and fails with the x509 error. Loud so a future caller
		// passing the wrong shape is diagnosable (mirrors resolveOne's
		// WithInternalEndpoint wrong-type warning).
		xcontext.Logger(ctx).Warn("dispatchViaInternalRESTConfig: internal REST config present but not a usable *rest.Config; falling through to httpcall.Do",
			slog.String("subsystem", "cache"),
			slog.String("got_type", fmt.Sprintf("%T", v)),
			slog.String("hint", "an internal driver (e.g. Phase 1) must pass *rest.Config to cache.WithInternalRESTConfig"),
		)
		return nil, false, nil
	}

	// Gate 2: verb. client-go dynamic Get/List is read-only.
	if verb := ptr.Deref(call.Verb, http.MethodGet); verb != http.MethodGet {
		return nil, false, nil
	}

	// Gate 3: subresource path. Same exclusion as the informer pivot —
	// status/scale/log/exec/binding/proxy have no dynamic-Get shape.
	if hasSubresourceSuffix(call.Path) {
		return nil, false, nil
	}

	// Gate 4: parse path -> GVR + namespace + name. Non-apiserver paths
	// (external URLs, unresolved `${...}`) return ok=false; the internal
	// dispatcher only owns apiserver GVR paths.
	gvr, namespace, name, parseOK := cache.ParseAPIServerPathToDep(call.Path)
	if !parseOK {
		return nil, false, nil
	}

	cli, err := internalClientFor(rc)
	if err != nil {
		return nil, false, err
	}

	// Select the resource interface. namespace=="" => cluster-scoped
	// (the /api/v1/namespaces LIST itself, and any cluster-scoped GVR);
	// a non-empty namespace => the namespaced resource interface.
	ri := cli.Resource(gvr)
	var nri k8sdynamic.ResourceInterface = ri
	if namespace != "" {
		nri = ri.Namespace(namespace)
	}

	// Served path. Two shapes — GET-by-name and LIST (name=="").
	if name != "" {
		obj, getErr := nri.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return nil, false, getErr
		}
		raw, mErr := json.Marshal(obj.Object)
		if mErr != nil {
			return nil, false, mErr
		}
		return raw, true, nil
	}

	list, listErr := nri.List(ctx, metav1.ListOptions{})
	if listErr != nil {
		return nil, false, listErr
	}
	// CRITICAL — marshal list.UnstructuredContent(), NOT list.Object.
	// client-go's *unstructured.UnstructuredList keeps the item objects
	// in the separate typed `Items []Unstructured` field; `list.Object`
	// carries ONLY the envelope scalars (apiVersion / kind / metadata)
	// and does NOT contain an `items` key. Marshalling list.Object
	// therefore yields a LIST envelope with NO items — the resolver's
	// iterator filter `[.<id>.items[] | ...]` then evaluates against a
	// null `items` and the walk discovers nothing (0.30.104 first
	// on-cluster smoke check: the namespace LIST succeeded but
	// `.namespaces.items` was null, so Phase 1 registered only the 8
	// infra informers — no composition informer).
	//
	// UnstructuredContent() shallow-copies the envelope scalars AND
	// folds the typed Items slice back into an `items` array — the exact
	// apiserver LIST shape (apiVersion / kind / metadata / items) the JQ
	// pipeline expects, byte-equivalent to the httpcall.Do path
	// (feedback_cache_must_not_constrain_jq.md).
	raw, mErr := json.Marshal(list.UnstructuredContent())
	if mErr != nil {
		return nil, false, mErr
	}
	return raw, true, nil
}
