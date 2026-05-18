// phase1_roots.go — 0.30.107: the navigation-root SOURCE for Phase 1.
//
// THE CONTRACT (feedback_no_special_cases.md — hard requirement):
//
//   Phase 1 must NOT hardcode that navigation starts from `navmenus` /
//   `routesloaders`. Those resource names are themselves a no-special-
//   cases violation. The navigation entry points are READ from the
//   frontend ConfigMap — that ConfigMap IS the navigation contract.
//
//   The frontend ConfigMap `krateo-system/frontend-config-vars` carries a
//   single key `config.json` (a JSON string). Two fields name the two
//   `/call` entry points the frontend itself dispatches on login:
//
//     .api.INIT          — e.g. /call?resource=navmenus&apiVersion=...
//                          &name=sidebar-nav-menu&namespace=krateo-system
//     .api.ROUTES_LOADER  — e.g. /call?resource=routesloaders&apiVersion=...
//                          &name=routes-loader&namespace=krateo-system
//
//   Phase 1 reads that ConfigMap, parses `config.json`, extracts INIT and
//   ROUTES_LOADER, decodes each `/call?...` URL into an ObjectReference
//   (resource + apiVersion + name + namespace), and resolves those EXACT
//   widget CRs as the navigation roots. If the frontend changes its INIT
//   widget, Phase 1 follows automatically — zero Go change.
//
//   The strings `navmenus` and `routesloaders` therefore appear NOWHERE
//   as Go literals driving root selection. They arrive at runtime from
//   parsing the ConfigMap's `/call` URLs.
//
//   Even the POINTER to that ConfigMap is config, not hardcode: the
//   ConfigMap name is the FRONTEND_CONFIG_CONFIGMAP env var (chart value,
//   default `frontend-config-vars`) and its namespace is the existing
//   AUTHN_NAMESPACE (the krateo-system control-plane namespace snowplow
//   already runs in / authenticates against).
//
// FALLBACK: if the ConfigMap cannot be read or parsed, Phase 1 logs a
// warning and returns no roots — the walk then warms only the meta-query
// seeds + CRD-watch, and lazy register-on-navigation still covers every
// GVR on the first real request. A missing ConfigMap is degraded, not
// fatal — exactly the posture the rest of Phase 1 takes for partial
// failures.

package dispatchers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/krateoplatformops/plumbing/env"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sdynamic "k8s.io/client-go/dynamic"
)

// frontendConfigConfigMapEnv names the env var (chart value) that points
// at the frontend ConfigMap. Default `frontend-config-vars`. Per the
// no-special-cases contract: even the ConfigMap NAME is config, never a
// Go constant — an operator who renames the ConfigMap re-points Phase 1
// without a code change.
const frontendConfigConfigMapEnv = "FRONTEND_CONFIG_CONFIGMAP"

// frontendConfigConfigMapDefault is the default ConfigMap name when
// FRONTEND_CONFIG_CONFIGMAP is unset. It is a DEFAULT, not a hardcoded
// policy — the env var overrides it.
const frontendConfigConfigMapDefault = "frontend-config-vars"

// frontendConfigDataKey is the single key inside the ConfigMap whose
// value is the `config.json` JSON string.
const frontendConfigDataKey = "config.json"

// configMapGVR is the GVR for core/v1 ConfigMap — the meta object Phase 1
// reads the navigation contract from. This is NOT a business GVR and NOT
// a per-resource carve-out: it is the bare type of the navigation-config
// document, exactly as customResourceDefinitionGVR is the bare type the
// CRD-watch reads. The BUSINESS GVRs (navmenus, routesloaders, …) are
// still never named in Go — they come out of config.json at runtime.
var configMapGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "configmaps",
}

// frontendConfig is the subset of the frontend `config.json` document
// Phase 1 needs: the two `/call` navigation entry-point URLs.
type frontendConfig struct {
	API struct {
		Init         string `json:"INIT"`
		RoutesLoader string `json:"ROUTES_LOADER"`
	} `json:"api"`
}

// frontendConfigConfigMapName returns the configured frontend ConfigMap
// name (FRONTEND_CONFIG_CONFIGMAP, default frontendConfigConfigMapDefault).
// An UNSET or EMPTY env var falls back to the default — an empty chart
// value is treated as "use the default", not "no ConfigMap".
func frontendConfigConfigMapName() string {
	if v := strings.TrimSpace(env.String(frontendConfigConfigMapEnv, "")); v != "" {
		return v
	}
	return frontendConfigConfigMapDefault
}

// listNavigationRootsFromConfigMap is the 0.30.107 navigation-root
// SOURCE. It reads the frontend ConfigMap, parses config.json, decodes
// the INIT and ROUTES_LOADER `/call` URLs into ObjectReferences, fetches
// each named root widget CR via the SA dynamic client, and returns them
// for the recursive walker.
//
// Errors are NON-FATAL: a missing/unparseable ConfigMap or a missing
// root CR yields a warning + a (possibly empty) partial result — the
// walk degrades gracefully, lazy register-on-navigation still covers
// every GVR on first request. It returns an error only when NOTHING
// could be produced AND a hard read error occurred, so phase1WarmupWith
// can log a roots_list_failed.
func listNavigationRootsFromConfigMap(ctx context.Context, dynCli k8sdynamic.Interface, cfgNamespace string) ([]*unstructured.Unstructured, error) {
	log := slog.Default()

	cmName := frontendConfigConfigMapName()
	cfg, err := readFrontendConfig(ctx, dynCli, cfgNamespace, cmName)
	if err != nil {
		log.Warn("phase1.roots.configmap_read_failed",
			slog.String("subsystem", "cache"),
			slog.String("configmap", cfgNamespace+"/"+cmName),
			slog.Any("err", err),
			slog.String("effect", "no navigation roots; lazy register-on-navigation still covers GVRs on first request"),
		)
		return nil, err
	}

	// Parse the two entry-point /call URLs into ObjectReferences. Each is
	// the EXACT widget CR the frontend dispatches — by name+namespace.
	// parseCallPathToObjectRef is the same generic /call decoder the
	// recursive walk uses for child endpoints (phase1_walk.go); it is not
	// a hardcoded path special-case.
	rawRoots := []struct {
		field string
		url   string
	}{
		{"INIT", cfg.API.Init},
		{"ROUTES_LOADER", cfg.API.RoutesLoader},
	}

	var (
		refs    []templatesv1.ObjectReference
		seenRef = map[string]struct{}{}
	)
	for _, rr := range rawRoots {
		if rr.url == "" {
			log.Warn("phase1.roots.entry_point_empty",
				slog.String("subsystem", "cache"),
				slog.String("field", rr.field),
				slog.String("effect", "navigation entry point not declared in config.json — skipped"),
			)
			continue
		}
		ref, ok := util.ParseCallPathToObjectRef(rr.url)
		if !ok {
			log.Warn("phase1.roots.entry_point_unparseable",
				slog.String("subsystem", "cache"),
				slog.String("field", rr.field),
				slog.String("url", rr.url),
				slog.String("effect", "not a /call widget endpoint — skipped"),
			)
			continue
		}
		// Dedupe: if INIT and ROUTES_LOADER happen to name the same CR,
		// resolve it once.
		k := navWidgetEndpointKey(ref)
		if _, dup := seenRef[k]; dup {
			continue
		}
		seenRef[k] = struct{}{}
		refs = append(refs, ref)
		log.Info("phase1.roots.entry_point_resolved",
			slog.String("subsystem", "cache"),
			slog.String("field", rr.field),
			slog.String("resource", ref.Resource),
			slog.String("apiVersion", ref.APIVersion),
			slog.String("name", ref.Name),
			slog.String("namespace", ref.Namespace),
		)
	}

	if len(refs) == 0 {
		return nil, fmt.Errorf("frontend config %s/%s declared no usable navigation entry points", cfgNamespace, cmName)
	}

	// Fetch each named root widget CR. objects.Get honours the
	// internal-dispatch context (cache.WithInternalRESTConfig) the caller
	// installs, so the SA credentials are used. A missing root CR is
	// non-fatal — log + skip; the other root still warms its subtree.
	out := make([]*unstructured.Unstructured, 0, len(refs))
	for _, ref := range refs {
		got := objects.Get(ctx, ref)
		if got.Err != nil {
			log.Warn("phase1.roots.root_fetch_failed",
				slog.String("subsystem", "cache"),
				slog.String("root", ref.Resource+"/"+ref.Name),
				slog.String("namespace", ref.Namespace),
				slog.Any("err", got.Err),
				slog.String("effect", "this navigation root not walked; other roots + lazy register-on-navigation still cover GVRs"),
			)
			continue
		}
		if got.Unstructured == nil {
			continue
		}
		out = append(out, got.Unstructured)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no navigation root CR could be fetched from %s/%s entry points", cfgNamespace, cmName)
	}
	return out, nil
}

// readFrontendConfig fetches the frontend ConfigMap via the SA dynamic
// client and unmarshals its config.json into a frontendConfig.
func readFrontendConfig(ctx context.Context, dynCli k8sdynamic.Interface, namespace, name string) (frontendConfig, error) {
	var cfg frontendConfig

	u, err := dynCli.Resource(configMapGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cfg, err
	}

	var cm corev1.ConfigMap
	if err := runtimeFromUnstructured(u, &cm); err != nil {
		return cfg, fmt.Errorf("frontend configmap %s/%s: %w", namespace, name, err)
	}

	raw, ok := cm.Data[frontendConfigDataKey]
	if !ok || raw == "" {
		return cfg, fmt.Errorf("frontend configmap %s/%s has no %q key", namespace, name, frontendConfigDataKey)
	}

	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("frontend configmap %s/%s %q is not valid JSON: %w", namespace, name, frontendConfigDataKey, err)
	}
	return cfg, nil
}

// runtimeFromUnstructured converts an *unstructured.Unstructured into a
// typed object via its JSON round-trip. ConfigMap.Data is a plain
// map[string]string so a json round-trip is exact and cheap.
func runtimeFromUnstructured(u *unstructured.Unstructured, into any) error {
	if u == nil {
		return fmt.Errorf("nil unstructured object")
	}
	data, err := json.Marshal(u.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}
