package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/http/response"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func fetchObject(req *http.Request) (got objects.Result) {
	log := xcontext.Logger(req.Context())

	gvr, err := util.ParseGVR(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("GVR from request query parameters", slog.Any("gvr", gvr))

	nsn, err := util.ParseNamespacedName(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("Name and Namespace from request query parameters", slog.Any("nsn", nsn))

	return objects.Get(req.Context(), templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name: nsn.Name, Namespace: nsn.Namespace,
		},
		APIVersion: gvr.GroupVersion().String(),
		Resource:   gvr.Resource,
	})
}

func paginationInfo(log *slog.Logger, req *http.Request) (perPage, page int) {
	perPage, page = -1, -1

	if val := req.URL.Query().Get("perPage"); val != "" {
		var err error
		perPage, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert perPage parameter to int",
				slog.Any("err", err))
		}
	}

	if val := req.URL.Query().Get("page"); val != "" {
		var err error
		page, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert page parameter to int",
				slog.Any("err", err))
		}
	}

	if perPage > 0 && page <= 0 {
		page = 1
	}

	return
}

// checkDispatchRBAC is the cache=on permission gate (Revision 2
// binding, Tag 0.30.4). Returns true iff the user identified by ctx is
// permitted to GET the dispatched CR in namespace.
//
// The check runs against the *dispatch target* (RestAction or Widget
// CR) — the same object the cache=off fetchObject branch hits the
// apiserver for. In cache=on mode fetchObject does not enforce RBAC
// for that GET, so the gate must run explicitly here.
//
// Callers MUST only invoke this in cache=on mode (`!cache.Disabled()`).
// In cache=off mode the gate is implicit in fetchObject's per-user
// apiserver call.
func checkDispatchRBAC(ctx context.Context, gvr schema.GroupVersionResource, namespace string) bool {
	log := xcontext.Logger(ctx)

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("checkDispatchRBAC: unable to extract UserInfo",
			slog.Any("err", err),
		)
		return false
	}

	allowed, evalErr := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  ui.Username,
		Groups:    ui.Groups,
		Verb:      "get",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: namespace,
	})
	if evalErr != nil {
		log.Error("checkDispatchRBAC: EvaluateRBAC error",
			slog.String("user", ui.Username),
			slog.String("gvr", gvr.String()),
			slog.String("namespace", namespace),
			slog.Any("err", evalErr),
		)
		return false
	}
	return allowed
}

// dispatchCacheLookupKey builds the L1 resolved-output cache key and
// returns the live cache handle, if the L1 layer is enabled. Returns
// (key, nil) when L1 is disabled — callers MUST treat handle==nil as
// "skip cache lookup, take the 0.30.6 path".
//
// User identity is read from the request context; on error (missing
// or unparseable UserInfo) we treat the request as uncacheable —
// returning a nil handle — so the request still resolves correctly but
// never reads or writes the L1 cache. A keyless request would risk
// cross-user leaks, which is unacceptable.
func dispatchCacheLookupKey(ctx context.Context, handlerKind, group, version, resource, namespace, name string, perPage, page int, extras map[string]any) (string, cacheHandle) {
	c := cache.ResolvedCache()
	if c == nil {
		return "", nil
	}
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		// Defence in depth — without an identity we cannot key
		// safely. Skip the cache for this request.
		return "", nil
	}
	key := cache.ComputeKey(cache.ResolvedKeyInputs{
		HandlerKind: handlerKind,
		Group:       group,
		Version:     version,
		Resource:    resource,
		Namespace:   namespace,
		Name:        name,
		Username:    ui.Username,
		Groups:      ui.Groups,
		PerPage:     perPage,
		Page:        page,
		Extras:      extras,
	})
	return key, c
}

// cacheHandle is the narrow interface the dispatchers depend on. The
// real implementation is *cache.resolvedCache; tests can substitute
// stubs without dragging in the whole singleton. Kept package-private
// so it never leaks beyond dispatchers.
type cacheHandle interface {
	Get(key string) (*cache.ResolvedEntry, bool)
	Put(key string, entry *cache.ResolvedEntry)
}

// emitResolvedCacheLookup writes the per-request falsifier line per
// plan §"Code-path falsifier":
//
//	resolved_cache.lookup hit=true|false key_hash=... resident_bytes=N
//
// We log at INFO so a casual grep on production logs proves whether
// L1 is firing.
func emitResolvedCacheLookup(log *slog.Logger, handlerKind, key string, hit bool, residentBytes int) {
	if log == nil {
		return
	}
	log.Info("resolved_cache.lookup",
		slog.String("subsystem", "cache"),
		slog.String("handler", handlerKind),
		slog.String("key_hash", key),
		slog.Bool("hit", hit),
		slog.Int("resident_bytes", residentBytes),
	)
}

// encodeResolvedJSON marshals res with the same json.Encoder settings
// the dispatchers used before 0.30.7 (SetIndent("", "  ")). Centralising
// the encode here ensures the cache-hit path returns byte-identical
// output to the cache-miss path; any divergence would break the
// "cache=on warm response equals cache=off response" contract.
func encodeResolvedJSON(res any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeResolvedJSON writes the canonical Content-Type + 200 + payload.
// We deliberately do NOT log here on errors writing to the wire — a
// client disconnect mid-write is normal and not actionable.
func writeResolvedJSON(wri http.ResponseWriter, payload []byte) {
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, _ = wri.Write(payload)
}
