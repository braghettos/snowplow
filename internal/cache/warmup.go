package cache

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

const warmupConcurrency = 4

type WarmupGVR struct {
	Group    string `yaml:"group"`
	Version  string `yaml:"version"`
	Resource string `yaml:"resource"`
	TTL      string `yaml:"ttl,omitempty"`
}

type WarmupRestAction struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type WarmupConfig struct {
	Warmup struct {
		GVRs               []WarmupGVR        `yaml:"gvrs"`
		L1RestActions      []WarmupRestAction  `yaml:"l1RestActions,omitempty"`
		AutoDiscoverGroups []string            `yaml:"autoDiscoverGroups,omitempty"`
		Categories         []string            `yaml:"categories,omitempty"`
	} `yaml:"warmup"`
}

func LoadWarmupConfig(path string) (*WarmupConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading warmup config %s: %w", path, err)
	}
	var cfg WarmupConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing warmup config: %w", err)
	}
	return &cfg, nil
}

type Warmer struct {
	cache              *RedisCache
	rc                 *rest.Config
	gvrs               []schema.GroupVersionResource
	gvrTTLs            map[schema.GroupVersionResource]time.Duration
	categories         []string
	autoDiscoverGroups []string
}

// matchesGroupPatterns checks if a group matches any pattern. Supports
// exact match ("composition.krateo.io") and suffix match ("*.krateo.io").
func matchesGroupPatterns(group string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasPrefix(p, "*.") {
			if strings.HasSuffix(group, p[1:]) {
				return true
			}
		} else if group == p {
			return true
		}
	}
	return false
}

func NewWarmer(c *RedisCache, rc *rest.Config) *Warmer {
	return &Warmer{cache: c, rc: rc, gvrTTLs: make(map[schema.GroupVersionResource]time.Duration)}
}

// SetWarmupConfig stores GVRs, per-GVR TTL overrides, and discovery categories.
func (w *Warmer) SetWarmupConfig(cfg *WarmupConfig) {
	if cfg == nil {
		return
	}
	w.categories = cfg.Warmup.Categories
	w.autoDiscoverGroups = cfg.Warmup.AutoDiscoverGroups
	for _, entry := range cfg.Warmup.GVRs {
		gvr := schema.GroupVersionResource{Group: entry.Group, Version: entry.Version, Resource: entry.Resource}
		w.gvrs = append(w.gvrs, gvr)
		if entry.TTL != "" {
			if ttl, err := time.ParseDuration(entry.TTL); err == nil {
				w.gvrTTLs[gvr] = ttl
				w.cache.RegisterGVRTTL(gvr, ttl)
			} else {
				slog.Warn("warmup: invalid TTL for GVR; using default",
					slog.String("gvr", gvr.String()), slog.String("ttl", entry.TTL))
			}
		}
	}
}

func (w *Warmer) SetGVRs(gvrs []schema.GroupVersionResource) { w.gvrs = gvrs }

// DiscoverCompositionGVRs finds CRDs matching the autoDiscoverGroups patterns
// and adds them to the warmup set. These are dynamic CRDs created by
// CompositionDefinitions — they can't be hardcoded in the warmup config
// because they vary per cluster.
func (w *Warmer) DiscoverCompositionGVRs(ctx context.Context) {
	if len(w.autoDiscoverGroups) == 0 {
		return
	}
	dc, err := discovery.NewDiscoveryClientForConfig(w.rc)
	if err != nil {
		slog.Warn("warmup: failed to create discovery client for composition GVRs",
			slog.Any("err", err))
		return
	}
	lists, err := dc.ServerPreferredResources()
	if err != nil && lists == nil {
		slog.Warn("warmup: failed to fetch server resources for composition GVRs",
			slog.Any("err", err))
		return
	}

	var added int
	for _, list := range lists {
		gv, perr := schema.ParseGroupVersion(list.GroupVersion)
		if perr != nil {
			continue
		}
		g := gv.Group
		if !matchesGroupPatterns(g, w.autoDiscoverGroups) {
			continue
		}
		for _, res := range list.APIResources {
			if strings.Contains(res.Name, "/") {
				continue // skip subresources
			}
			v := res.Version
			if v == "" {
				v = gv.Version
			}
			rg := res.Group
			if rg == "" {
				rg = g
			}
			gvr := schema.GroupVersionResource{Group: rg, Version: v, Resource: res.Name}
			w.gvrs = append(w.gvrs, gvr)
			added++
			slog.Info("warmup: auto-discovered composition GVR",
				slog.String("gvr", gvr.String()))
		}
	}
	if added > 0 {
		slog.Info("warmup: discovered composition GVRs", slog.Int("count", added))
	}
}

// PreRegisterGVRs calls SAddGVR for every configured GVR to trigger informers
// before WaitForSync is called.
func (w *Warmer) PreRegisterGVRs(ctx context.Context) {
	for _, gvr := range w.gvrs {
		if err := w.cache.SAddGVR(ctx, gvr); err != nil {
			slog.Warn("warmup: failed to pre-register GVR",
				slog.String("gvr", gvr.String()), slog.Any("err", err))
		}
	}
}

// Run fetches and caches all configured GVRs concurrently, then primes discovery.
func (w *Warmer) Run(ctx context.Context) {
	if w.cache == nil || (len(w.gvrs) == 0 && len(w.categories) == 0) {
		return
	}
	log := slog.Default()

	dynClient, err := k8sdynamic.NewForConfig(w.rc)
	if err != nil {
		log.Error("warmup: failed to create dynamic client", slog.Any("err", err))
		return
	}

	sem := make(chan struct{}, warmupConcurrency)
	var wg sync.WaitGroup
	for _, gvr := range w.gvrs {
		wg.Add(1)
		sem <- struct{}{}
		go func(gvr schema.GroupVersionResource) {
			defer wg.Done()
			defer func() { <-sem }()
			w.warmGVR(ctx, dynClient, gvr)
		}(gvr)
	}
	wg.Wait()

	if len(w.categories) > 0 {
		w.warmDiscovery(ctx, log)
	}

	log.Info("warmup: completed", slog.Int("gvrs", len(w.gvrs)), slog.Int("categories", len(w.categories)))
}

func (w *Warmer) warmGVR(ctx context.Context, dynClient k8sdynamic.Interface, gvr schema.GroupVersionResource) {
	log := slog.Default()
	list, err := dynClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Warn("warmup: failed to list GVR", slog.String("gvr", gvr.String()), slog.Any("err", err))
		return
	}

	byNamespace := make(map[string][]int)
	for i := range list.Items {
		obj := &list.Items[i]
		StripAnnotationsFromUnstructured(obj)
		getKey := GetKey(gvr, obj.GetNamespace(), obj.GetName())
		if serr := w.cache.SetForGVR(ctx, gvr, getKey, obj); serr != nil {
			log.Warn("warmup: failed to cache object", slog.String("key", getKey), slog.Any("err", serr))
		}
		byNamespace[obj.GetNamespace()] = append(byNamespace[obj.GetNamespace()], i)
	}

	ttl := w.cache.TTLForGVR(gvr)

	// ── Per-item index SETs ──────────────────────────────────────────────────
	// Build cluster-wide index members.
	var clusterMembers []string
	for i := range list.Items {
		obj := &list.Items[i]
		ns, name := obj.GetNamespace(), obj.GetName()
		if ns != "" {
			clusterMembers = append(clusterMembers, ns+"/"+name)
		} else {
			clusterMembers = append(clusterMembers, name)
		}
	}
	if len(clusterMembers) > 0 {
		_ = w.cache.ReplaceSetWithTTL(ctx, ListIndexKey(gvr, ""), clusterMembers, ttl)
	}

	// Build per-namespace index members.
	nsMembers := make(map[string][]string)
	for ns, indices := range byNamespace {
		if ns == "" {
			continue
		}
		for _, idx := range indices {
			nsMembers[ns] = append(nsMembers[ns], list.Items[idx].GetName())
		}
	}
	for ns, members := range nsMembers {
		_ = w.cache.ReplaceSetWithTTL(ctx, ListIndexKey(gvr, ns), members, ttl)
	}

	log.Info("warmup: cached GVR", slog.String("gvr", gvr.String()),
		slog.Int("count", len(list.Items)), slog.Int("namespaces", len(byNamespace)))
}

func (w *Warmer) warmDiscovery(ctx context.Context, log *slog.Logger) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(w.rc)
	if err != nil {
		log.Warn("warmup: failed to create discovery client", slog.Any("err", err))
		return
	}
	lists, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		log.Warn("warmup: failed to fetch server resources", slog.Any("err", err))
		return
	}

	byCategory := make(map[string][]schema.GroupVersionResource)
	for _, list := range lists {
		if len(list.APIResources) == 0 {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, res := range list.APIResources {
			g := res.Group
			if g == "" {
				g = gv.Group
			}
			v := res.Version
			if v == "" {
				v = gv.Version
			}
			gvr := schema.GroupVersionResource{Group: g, Version: v, Resource: res.Name}
			for _, cat := range w.categories {
				if discoveryResourceMatchesCategory(res, cat) {
					byCategory[cat] = append(byCategory[cat], gvr)
				}
			}
		}
	}

	for cat, gvrs := range byCategory {
		if serr := w.cache.Set(ctx, DiscoveryKey(cat), gvrs); serr != nil {
			log.Warn("warmup: failed to cache discovery", slog.String("category", cat), slog.Any("err", serr))
		} else {
			log.Info("warmup: cached discovery", slog.String("category", cat), slog.Int("gvrs", len(gvrs)))
		}
	}
}

func discoveryResourceMatchesCategory(res metav1.APIResource, category string) bool {
	if res.Name == category || res.SingularName == category {
		return true
	}
	for _, sn := range res.ShortNames {
		if sn == category {
			return true
		}
	}
	for _, cat := range res.Categories {
		if cat == category {
			return true
		}
	}
	return false
}
