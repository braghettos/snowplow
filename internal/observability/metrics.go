package observability

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// OTel metric instruments. Nil until InitMetrics is called successfully.
var (
	CacheHits    otelmetric.Int64Counter
	CacheMisses  otelmetric.Int64Counter
	CacheLatency otelmetric.Float64Histogram
	RBACHits     otelmetric.Int64Counter
	RBACMisses   otelmetric.Int64Counter

	// L1 refresh observability
	L1RefreshDuration otelmetric.Float64Histogram
	L1RefreshUsers    otelmetric.Int64Counter

	// initialized is set to 1 after InitMetrics completes. Checked by
	// IncrementCacheMetric to avoid nil-pointer dereferences.
	initialized atomic.Int32
)

// latencyBuckets for cache.lookup.duration histogram (milliseconds).
var latencyBuckets = []float64{0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500}

// InitMetrics creates OTel metric instruments from the given Meter.
// Must be called once after the MeterProvider is configured.
func InitMetrics(meter otelmetric.Meter) error {
	var err error

	CacheHits, err = meter.Int64Counter("cache.hits",
		otelmetric.WithDescription("Number of cache hits"),
		otelmetric.WithUnit("{hit}"),
	)
	if err != nil {
		return err
	}

	CacheMisses, err = meter.Int64Counter("cache.misses",
		otelmetric.WithDescription("Number of cache misses"),
		otelmetric.WithUnit("{miss}"),
	)
	if err != nil {
		return err
	}

	CacheLatency, err = meter.Float64Histogram("cache.lookup.duration",
		otelmetric.WithDescription("Cache lookup duration in milliseconds"),
		otelmetric.WithUnit("ms"),
		otelmetric.WithExplicitBucketBoundaries(latencyBuckets...),
	)
	if err != nil {
		return err
	}

	RBACHits, err = meter.Int64Counter("rbac.hits",
		otelmetric.WithDescription("Number of RBAC cache hits"),
		otelmetric.WithUnit("{hit}"),
	)
	if err != nil {
		return err
	}

	RBACMisses, err = meter.Int64Counter("rbac.misses",
		otelmetric.WithDescription("Number of RBAC cache misses"),
		otelmetric.WithUnit("{miss}"),
	)
	if err != nil {
		return err
	}

	// L1 refresh duration histogram (seconds)
	l1Buckets := []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120}
	L1RefreshDuration, err = meter.Float64Histogram("l1.refresh.duration",
		otelmetric.WithDescription("L1 refresh duration in seconds"),
		otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(l1Buckets...),
	)
	if err != nil {
		return err
	}

	L1RefreshUsers, err = meter.Int64Counter("l1.refresh.users",
		otelmetric.WithDescription("Number of users refreshed in L1 refresh cycles"),
		otelmetric.WithUnit("{user}"),
	)
	if err != nil {
		return err
	}

	initialized.Store(1)
	return nil
}

// fieldToLayer maps the cache metric field name (used in Metrics.Inc) to an
// OTel attribute layer value and whether it is a hit or miss.
var fieldToLayer = map[string]struct {
	layer string
	hit   bool
}{
	"get_hits":         {layer: "l3", hit: true},
	"get_misses":       {layer: "l3", hit: false},
	"list_hits":        {layer: "l3", hit: true},
	"list_misses":      {layer: "l3", hit: false},
	"raw_hits":         {layer: "l3", hit: true},
	"raw_misses":       {layer: "l3", hit: false},
	"l1_hits":          {layer: "l1", hit: true},
	"l1_misses":        {layer: "l1", hit: false},
	"call_hits":        {layer: "call", hit: true},
	"call_misses":      {layer: "call", hit: false},
	"negative_hits":    {layer: "negative", hit: true},
	"l3_promotions":    {layer: "l3", hit: true},
	"expiry_refreshes": {layer: "expiry", hit: true},
	"rbac_hits":        {layer: "rbac", hit: true},
	"rbac_misses":      {layer: "rbac", hit: false},
}

// IncrementCacheMetric maps a cache metric field name to the corresponding
// OTel counter and increments it. No-op if OTel is not initialized.
func IncrementCacheMetric(field string) {
	if initialized.Load() == 0 {
		return
	}

	info, ok := fieldToLayer[field]
	if !ok {
		return
	}

	layerAttr := attribute.String("layer", info.layer)

	ctx := context.Background()

	// RBAC uses dedicated counters.
	if info.layer == "rbac" {
		if info.hit {
			RBACHits.Add(ctx, 1, otelmetric.WithAttributes(layerAttr))
		} else {
			RBACMisses.Add(ctx, 1, otelmetric.WithAttributes(layerAttr))
		}
		return
	}

	if info.hit {
		CacheHits.Add(ctx, 1, otelmetric.WithAttributes(layerAttr))
	} else {
		CacheMisses.Add(ctx, 1, otelmetric.WithAttributes(layerAttr))
	}
}
