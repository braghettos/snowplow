// Q-RBAC-DECOUPLE C(d) v6 — Path B SA dispatch via client-go (audit 2026-05-04).
//
// This file replaces the SA-elevated dispatch branch's reliance on
// httpcall.Do (which goes through plumbing's tlsConfigFor and silently
// drops CertificateAuthorityData on token-auth endpoints — the v5 D1
// defect). Instead, we use the existing internal/dynamic.Client which is
// built on top of k8s.io/client-go's well-tested rest.TLSConfigFor — no
// HasCertAuth() early-return bug — so SA dispatches succeed structurally
// without any reflection-based plumbing patch.
//
// The SA branch only ever targets the in-cluster K8s apiserver (proven
// statically: CEL admission at apis/templates/v1/core.go binds UAF to the
// SA path AND forbids non-GET, and SnowplowEndpointFromConfig pins
// ServerURL to https://kubernetes.default.svc). So replacing the HTTP
// transport with a typed K8s client is a pure structural simplification:
// no functionality is lost, and the bug class around plumbing TLS handling
// for token-auth+CA endpoints is eliminated entirely on this path.
//
// The byte-equivalence test in sa_dispatch_test.go drives the same K8s
// resource through (a) httpcall.Do via SnowplowEndpointFromConfig and
// (b) this Path B helper, asserting json.Marshal of both is byte-identical
// modulo deterministic key ordering — guarantees the JQ filters in the 6
// portal-cache YAMLs keep behaving identically.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// saDispatchResult is the local shape used to bridge a client-go call
// back to the existing httpcall response-handler path. raw is the JSON
// bytes that would have been the apiserver's HTTP response body; err is
// non-nil iff the dispatch failed (in which case raw is nil and the
// caller emits the err_write audit). status is the *response.Status that
// callers need to feed response.AsMap on the failure path.
type saDispatchResult struct {
	raw    []byte
	err    error
	status *response.Status
}

// dispatchSAViaClientGo runs an SA-elevated dispatch through client-go's
// dynamic client. Returns the JSON bytes that match what the apiserver
// would have returned over HTTP (so the caller's existing
// jsonHandlerCompute / mergeIntoDict path applies unchanged).
//
// path MUST be a K8s API path of one of the shapes:
//   - /api/v1/<resource>                        (cluster-wide list)
//   - /api/v1/namespaces/<ns>/<resource>        (namespaced list)
//   - /api/v1/namespaces/<ns>/<resource>/<name> (namespaced get)
//   - /apis/<group>/<version>/<resource>
//   - /apis/<group>/<version>/namespaces/<ns>/<resource>
//   - /apis/<group>/<version>/namespaces/<ns>/<resource>/<name>
//
// Non-K8s paths (/call?..., external-endpoint URLs) are out-of-scope:
// the SA branch is statically guaranteed to target only K8s API paths
// (see file-header comment) and the caller short-circuits to httpcall.Do
// if SnowplowK8sClient is nil.
func dispatchSAViaClientGo(ctx context.Context, client dynamic.Client, path string) saDispatchResult {
	if client == nil {
		return saDispatchResult{
			err:    fmt.Errorf("nil snowplow K8s client"),
			status: response.New(http.StatusInternalServerError, fmt.Errorf("snowplow k8s client unavailable")),
		}
	}

	gvr, ns, name := cache.ParseK8sAPIPath(path)
	if gvr.Resource == "" {
		return saDispatchResult{
			err:    fmt.Errorf("unparseable K8s API path: %q", path),
			status: response.New(http.StatusBadRequest, fmt.Errorf("unparseable K8s API path: %q", path)),
		}
	}

	opts := dynamic.Options{GVR: gvr, Namespace: ns}

	if name == "" {
		list, err := client.List(ctx, opts)
		if err != nil {
			return saDispatchResult{
				err:    err,
				status: clientGoErrorToStatus(err),
			}
		}
		raw, merr := json.Marshal(list)
		if merr != nil {
			return saDispatchResult{
				err:    fmt.Errorf("marshal UnstructuredList: %w", merr),
				status: response.New(http.StatusInternalServerError, merr),
			}
		}
		return saDispatchResult{raw: raw}
	}

	obj, err := client.Get(ctx, name, opts)
	if err != nil {
		return saDispatchResult{
			err:    err,
			status: clientGoErrorToStatus(err),
		}
	}
	raw, merr := json.Marshal(obj)
	if merr != nil {
		return saDispatchResult{
			err:    fmt.Errorf("marshal Unstructured: %w", merr),
			status: response.New(http.StatusInternalServerError, merr),
		}
	}
	return saDispatchResult{raw: raw}
}

// clientGoErrorToStatus maps a client-go API error to the
// *response.Status shape expected by the err_write audit path.
//
// Mapping rules (by precedence):
//  1. apierrors.StatusError carries the canonical apiserver Status — pass
//     its embedded Code through. This handles all RBAC denials (403),
//     not-founds (404), conflicts (409), unauthorized (401), etc.
//  2. Network / context-cancellation / generic errors map to 500 with
//     the err.Error() text — same shape that plumbing's RetryClient.Do
//     would produce on a transport failure (see do.go:252).
//
// The audit downstream (auditUserAccessFilterSkipped + dict[call.ErrorKey])
// receives the same response.Status keys (Code/Message/Reason) regardless
// of which dispatch path produced the error, so log shape stability is
// preserved across the v5→v6 transition.
func clientGoErrorToStatus(err error) *response.Status {
	if err == nil {
		return response.New(http.StatusOK, nil)
	}
	// apierrors.StatusError exposes the apiserver-canonical reason/code.
	// Use its Code() helper which returns the int32 HTTP-equivalent code.
	if statusErr, ok := err.(*apierrors.StatusError); ok {
		code := int(statusErr.ErrStatus.Code)
		if code == 0 {
			code = http.StatusInternalServerError
		}
		return response.New(code, fmt.Errorf("%s", statusErr.ErrStatus.Message))
	}
	// Common typed predicates — same outcome but cleaner than chained
	// type-switches: client-go wraps several error categories in
	// non-StatusError types (e.g., timeout, network).
	switch {
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return response.New(http.StatusGatewayTimeout, err)
	case apierrors.IsUnauthorized(err):
		return response.New(http.StatusUnauthorized, err)
	case apierrors.IsForbidden(err):
		return response.New(http.StatusForbidden, err)
	case apierrors.IsNotFound(err):
		return response.New(http.StatusNotFound, err)
	}
	return response.New(http.StatusInternalServerError, err)
}
