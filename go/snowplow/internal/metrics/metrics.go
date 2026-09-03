// Package metrics mirrors snowplow's existing expvar counters/gauges onto
// an OpenTelemetry OTLP/HTTP MeterProvider so they can be scraped by the
// OTel collector alongside traces.
//
// ADDITIVE + GATED (load-bearing): the expvar surface at /debug/vars is
// UNTOUCHED — these OTLP instruments are a parallel export of the SAME
// underlying counters, read through the same exported accessors the expvar
// closures use. The whole pipeline is gated by OTEL_METRICS_ENABLED, which
// DEFAULTS to the value of the OTEL_ENABLED master switch when unset (so
// OTEL_ENABLED=true turns metrics on; a per-signal OTEL_METRICS_ENABLED
// still overrides). With both unset it is false. When the gate is off,
// Setup registers NOTHING (no MeterProvider, no exporter, no instruments)
// and returns a no-op shutdown, so the off-path is byte-identical to the
// pre-OTel binary.
//
// This matches the canonical platform OTel enablement contract (the
// chart-inspector internal/telemetry ConfigFromEnv and the runtimes):
//
//	OTEL_ENABLED            master switch                (default false)
//	OTEL_TRACING_ENABLED    gate tracing  (default: value of OTEL_ENABLED)
//	OTEL_METRICS_ENABLED    gate metrics  (default: value of OTEL_ENABLED)
//	OTEL_EXPORTER_OTLP_ENDPOINT   collector OTLP/HTTP endpoint
//
// Every instrument DECLARED HERE is OBSERVABLE (async): a single registered
// callback reads the live counter snapshots at collection time. This matches
// the expvar.Func "computed-on-read" semantics exactly and adds zero cost to
// the hot path — the callback only runs when the collector reads, and only
// when metrics are enabled.
//
// # THE PROCESS IS NOT FREE, EVEN THOUGH THESE INSTRUMENTS ARE (1.12.4)
//
// The sentence above is about snowplow's OWN instruments and is true of
// them. It is NOT true of the process, and the difference is what the
// serving path experiences. Enabling metrics costs three SYNCHRONOUS,
// UNSAMPLED histogram Records on EVERY non-filtered request:
//
//   - otelhttp captures the GLOBAL MeterProvider at handler-construction
//     time (its newConfig defaults MeterProvider to otel.GetMeterProvider()).
//   - Setup below calls otel.SetMeterProvider BEFORE main constructs the
//     otelhttp handler, so the REAL provider is what gets captured — not
//     the no-op.
//   - Each request then performs http.server.request.body.size,
//     .response.body.size and .request.duration Records, each with a
//     freshly built attribute set.
//
// Metrics do NOT honour the trace sampler, so every request pays this even
// at 5% trace sampling. OTEL_METRICS_ENABLED="false" is the rollback and
// needs no binary change. F11 pins the contract; the real acceptance is the
// Phase-6 bench at SCALE=50000.
//
// The compensation is worth claiming: those three histograms include
// http.server.request.duration, a per-route server-side latency
// distribution for /call — unsampled, complete, and the FIRST server-side
// latency distribution snowplow has ever exported.
//
// Not a 1.12.4 delta: otelhttp already built its metric attribute set per
// request in 1.12.3 under the no-op meter. That cost is already shipped.
package metrics

import (
	"context"

	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/kubeutil"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/dynamic"
	"github.com/krateo-platformops/snowplow/internal/handlers/dispatchers"
	"github.com/krateo-platformops/snowplow/internal/otelresource"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	"github.com/krateo-platformops/snowplow/internal/resolvers/crds/schema"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

const (
	// EnvEnabled is the master OTel switch. Default false. It supplies the
	// default for EnvMetricsEnabled so a single flag turns the whole pipeline
	// on.
	EnvEnabled = "OTEL_ENABLED"

	// EnvMetricsEnabled gates the entire OTLP metrics pipeline. It DEFAULTS to
	// the value of EnvEnabled (OTEL_ENABLED) when unset; a per-signal value
	// overrides the master. Default (with the master unset) is false.
	EnvMetricsEnabled = "OTEL_METRICS_ENABLED"

	// EnvOTLPEndpoint is the OTLP/HTTP collector endpoint, shared with the
	// trace pipeline via the standard OTEL_EXPORTER_OTLP_ENDPOINT contract.
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

	meterName = "github.com/krateo-platformops/snowplow"
)

// ShutdownFunc flushes and stops the metrics pipeline. Always non-nil; a
// no-op when metrics were not enabled.
type ShutdownFunc func(context.Context) error

// metricsEnabled resolves the metrics gate from the canonical contract:
// OTEL_METRICS_ENABLED, defaulting to the value of the OTEL_ENABLED master
// when the per-signal var is unset. With both unset it is false, preserving
// the default-off byte-identical guarantee.
func metricsEnabled() bool {
	return env.Bool(EnvMetricsEnabled, env.Bool(EnvEnabled, false))
}

// Setup wires the OTLP/HTTP metric exporter + a periodic-reader
// MeterProvider when metrics resolve to enabled (OTEL_METRICS_ENABLED,
// defaulting to OTEL_ENABLED), registers it as the global MeterProvider,
// and registers the observable instruments that mirror snowplow's expvar
// counters.
//
// No-op (registers nothing) when the gate is off.
func Setup(ctx context.Context, build string) (ShutdownFunc, error) {
	noop := func(context.Context) error { return nil }

	if !metricsEnabled() {
		return noop, nil
	}

	opts := []otlpmetrichttp.Option{}
	if ep := env.String(EnvOTLPEndpoint, ""); ep != "" {
		opts = append(opts, otlpmetrichttp.WithEndpointURL(ep))
	}

	exp, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return noop, err
	}

	// 1.12.4 — the shared resource (internal/otelresource). Identical by
	// construction to the trace and log pipelines' resource, including
	// the downward-API pod identity that keeps the collector's
	// k8sattributes association deterministic.
	res, err := otelresource.Build(ctx, build, kubeutil.ServiceAccountNamespace)
	if err != nil {
		return noop, err
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	if err := registerInstruments(mp.Meter(meterName), build); err != nil {
		// Best-effort: shut the provider down so we don't leak the reader
		// goroutine, then surface the error.
		_ = mp.Shutdown(ctx)
		return noop, err
	}

	return mp.Shutdown, nil
}

// registerInstruments declares every observable instrument and a single
// callback that reads the live counter snapshots. Instrument names mirror
// the expvar keys (with the conventional `_total` suffix on monotonic
// counters) so dashboards can correlate the two surfaces.
func registerInstruments(m metric.Meter, build string) error {
	// --- cache: apiserver fallthrough + assertion violations ---
	fallthroughTotal, err := m.Int64ObservableCounter(
		"snowplow_apiserver_fallthrough_total",
		metric.WithDescription("Cumulative apiserver fall-throughs (cache miss path)."),
	)
	if err != nil {
		return err
	}
	assertionViolations, err := m.Int64ObservableCounter(
		"snowplow_assertion_violations_total",
		metric.WithDescription("Cumulative read-path-scoped assertion violations."),
	)
	if err != nil {
		return err
	}

	// --- cache: RBAC snapshot publish sequence ---
	rbacPublishSeq, err := m.Int64ObservableCounter(
		"snowplow_rbac_publish_seq",
		metric.WithDescription("RBAC snapshot publish sequence number."),
	)
	if err != nil {
		return err
	}

	// --- cache: CRD discovery bridge counters (one gauge, keyed by stat) ---
	crdDiscovery, err := m.Int64ObservableCounter(
		"snowplow_crd_discovery",
		metric.WithDescription("CRD-discovery bridge counters, labelled by stat."),
	)
	if err != nil {
		return err
	}

	// --- cache: registered GVR count ---
	registeredGVRs, err := m.Int64ObservableGauge(
		"snowplow_plurals_registered_gvrs",
		metric.WithDescription("Number of GVRs with a registered informer."),
	)
	if err != nil {
		return err
	}

	// --- cache: prewarm completion boundary ---
	prewarmDone, err := m.Int64ObservableGauge(
		"snowplow_prewarm_complete",
		metric.WithDescription("1 once Phase1Done has flipped, else 0."),
	)
	if err != nil {
		return err
	}
	prewarmElapsed, err := m.Int64ObservableGauge(
		"snowplow_prewarm_complete_elapsed_ms",
		metric.WithDescription("Process-start to Phase1Done wall-clock (ms); -1 until flip."),
	)
	if err != nil {
		return err
	}

	// --- cache: live-refresh broadcaster ---
	refreshPublished, err := m.Int64ObservableCounter("snowplow_refresh_broadcaster_published_total",
		metric.WithDescription("Live-refresh signals published."))
	if err != nil {
		return err
	}
	refreshDelivered, err := m.Int64ObservableCounter("snowplow_refresh_broadcaster_delivered_total",
		metric.WithDescription("Live-refresh signals delivered to subscribers."))
	if err != nil {
		return err
	}
	refreshDropped, err := m.Int64ObservableCounter("snowplow_refresh_broadcaster_dropped_total",
		metric.WithDescription("Live-refresh signals dropped (slow consumer)."))
	if err != nil {
		return err
	}
	refreshCoalesced, err := m.Int64ObservableCounter("snowplow_refresh_broadcaster_coalesced_total",
		metric.WithDescription("Live-refresh signals coalesced."))
	if err != nil {
		return err
	}
	refreshSubscribers, err := m.Int64ObservableGauge("snowplow_refresh_broadcaster_subscribers",
		metric.WithDescription("Current live-refresh subscriber count."))
	if err != nil {
		return err
	}

	// --- rbac: snapshot authz memo ---
	memoHits, err := m.Int64ObservableCounter("snowplow_authz_memo_hits",
		metric.WithDescription("Snapshot authz memo hits."))
	if err != nil {
		return err
	}
	memoMisses, err := m.Int64ObservableCounter("snowplow_authz_memo_misses",
		metric.WithDescription("Snapshot authz memo misses."))
	if err != nil {
		return err
	}
	memoSwaps, err := m.Int64ObservableCounter("snowplow_authz_memo_swaps",
		metric.WithDescription("Snapshot authz memo generation swaps."))
	if err != nil {
		return err
	}
	memoRefused, err := m.Int64ObservableCounter("snowplow_authz_memo_refused",
		metric.WithDescription("Snapshot authz memo cap-breach refused inserts."))
	if err != nil {
		return err
	}
	memoDenyUncached, err := m.Int64ObservableCounter("snowplow_authz_memo_deny_uncached_total",
		metric.WithDescription("Deny verdicts not cached by the memo."))
	if err != nil {
		return err
	}
	memoEntries, err := m.Int64ObservableGauge("snowplow_authz_memo_entries",
		metric.WithDescription("Live entry count of the current memo shard."))
	if err != nil {
		return err
	}

	// --- prewarm engine: the unified walk/seed worker (production path) ---
	prewarmEngEnqueued, err := m.Int64ObservableCounter("snowplow_prewarm_engine_enqueued_total",
		metric.WithDescription("Cumulative enqueueScope calls (every enqueue, even dedup-coalesced)."))
	if err != nil {
		return err
	}
	prewarmEngProcessed, err := m.Int64ObservableCounter("snowplow_prewarm_engine_processed_total",
		metric.WithDescription("Prewarm-engine scopes fully processed by the worker."))
	if err != nil {
		return err
	}
	prewarmEngYield, err := m.Int64ObservableCounter("snowplow_prewarm_engine_yield_total",
		metric.WithDescription("Prewarm-engine worker parked for a customer /call (customer-priority yield)."))
	if err != nil {
		return err
	}
	prewarmEngPending, err := m.Int64ObservableGauge("snowplow_prewarm_engine_pending_depth",
		metric.WithDescription("Live prewarm-engine pending-scope depth; 0 once the worker drains."))
	if err != nil {
		return err
	}

	// --- prewarm phase-1: apiRef pagination coverage ---
	phase1UnitsPlanned, err := m.Int64ObservableGauge("snowplow_phase1_units_planned",
		metric.WithDescription("widgetContent cells the apiRef pagination walk planned to seed."))
	if err != nil {
		return err
	}
	phase1UnitsSeeded, err := m.Int64ObservableGauge("snowplow_phase1_units_seeded",
		metric.WithDescription("Page cells handed to populateWidgetContentL1 with a non-nil envelope."))
	if err != nil {
		return err
	}
	phase1ApiRefPages, err := m.Int64ObservableCounter("snowplow_phase1_apiref_pages_total",
		metric.WithDescription("Extra apiRef pages (page 2..N) resolved across all paginated widgets."))
	if err != nil {
		return err
	}
	phase1EligibleNoContinue, err := m.Int64ObservableCounter("snowplow_phase1_eligible_no_continue_total",
		metric.WithDescription("Distinct eligible widgets whose page-1 resolve produced no continuation."))
	if err != nil {
		return err
	}

	// --- prewarm phase-1: boot-walk fan-out ---
	phase1WalkZeroChildren, err := m.Int64ObservableCounter("snowplow_phase1_walk_zero_children_total",
		metric.WithDescription("Boot-walk observations that found zero children."))
	if err != nil {
		return err
	}
	phase1WalkObservations, err := m.Int64ObservableCounter("snowplow_phase1_walk_observations_total",
		metric.WithDescription("Total boot-walk children-count observations (zero-children denominator)."))
	if err != nil {
		return err
	}

	// --- prewarm phase-1: per-target seed outcomes ---
	phase1SeedResolves, err := m.Int64ObservableCounter("snowplow_phase1_bindingset_seed_resolves_total",
		metric.WithDescription("Per-binding-target phase-1 seed resolves."))
	if err != nil {
		return err
	}
	phase1SeedFailures, err := m.Int64ObservableCounter("snowplow_phase1_bindingset_seed_failures_total",
		metric.WithDescription("Grand-total phase-1 seed failures (= rbac_deny + operational)."))
	if err != nil {
		return err
	}
	phase1SeedRBACDeny, err := m.Int64ObservableCounter("snowplow_phase1_seed_rbac_deny_total",
		metric.WithDescription("EXPECTED narrow-RBAC seed denies (403/401); cohort genuinely can't read the target."))
	if err != nil {
		return err
	}
	phase1SeedOpFail, err := m.Int64ObservableCounter("snowplow_phase1_seed_operational_fail_total",
		metric.WithDescription("UNEXPECTED seed failures (ctx timeout/cancel, 5xx, transport, panic); should be 0."))
	if err != nil {
		return err
	}

	// --- refresher: background re-resolve worker pool (one counter keyed by stat) ---
	refresher, err := m.Int64ObservableCounter("snowplow_refresher",
		metric.WithDescription("Refresher worker-pool counters, labelled by stat."))
	if err != nil {
		return err
	}
	refresherQueueDepth, err := m.Int64ObservableGauge("snowplow_refresher_queue_depth",
		metric.WithDescription("Live refresher workqueue depth; climbing with stagnant completed = workers stuck."))
	if err != nil {
		return err
	}

	// --- discovery: SA-discovery client (one counter keyed by stat) ---
	saDiscovery, err := m.Int64ObservableCounter("snowplow_sa_discovery",
		metric.WithDescription("SA-discovery client counters, labelled by stat."))
	if err != nil {
		return err
	}

	// --- discovery: compiled-CRD-schema memo (one counter keyed by stat) ---
	crdSchemaMemo, err := m.Int64ObservableCounter("snowplow_crd_schema_memo",
		metric.WithDescription("Compiled-CRD-schema memo counters, labelled by stat."))
	if err != nil {
		return err
	}

	// --- upstream health: aggregate controller + webhook gauges ---
	upstreamControllers, err := m.Int64ObservableGauge("snowplow_upstream_controllers",
		metric.WithDescription("Auto-discovered upstream controllers, labelled by health (healthy/unhealthy)."))
	if err != nil {
		return err
	}
	upstreamWebhooks, err := m.Int64ObservableGauge("snowplow_upstream_webhooks",
		metric.WithDescription("Discovered admission webhooks, labelled by policy (total/fail)."))
	if err != nil {
		return err
	}

	// --- dispatch: L1 resolved-output lookups.
	//
	// 1.12.4 WIDENED IN PLACE. This instrument kept its name but gained
	// the `class` and `gvr` attributes and the `seed_hit` outcome; the
	// aggregate-only declaration it replaces observed a single
	// process-wide hit/miss pair. Declared below with the other 1.12.4
	// instruments as dispatchL1Cells.

	// --- RAFullList: cheap Go-slice serve outcomes + index drift canary ---
	raFullListServe, err := m.Int64ObservableCounter("snowplow_ra_full_list_serve",
		metric.WithDescription("RAFullList serve-outcome counters, labelled by outcome."))
	if err != nil {
		return err
	}
	bindingsDeltaSkipped, err := m.Int64ObservableCounter("snowplow_bindings_by_gvr_delta_skipped_non_typed",
		metric.WithDescription("Delta-event objects neither typed nor convertible, DROPPED (index drift canary); should be 0."))
	if err != nil {
		return err
	}

	// ===== 1.12.4 (design §3.3) — the instrument gap =====
	//
	// Everything below already existed as a live counter inside the
	// process, reachable only through expvar at the JWT-gated
	// /debug/vars, or (worse) only through an INFO log line the chart's
	// LOG_LEVEL=warn suppresses. None of it is a new measurement; all of
	// it is an existing measurement finally leaving the pod.

	// --- A-1 UAF Put declines (1.12.3). Non-zero is CORRECT in 1.12.x:
	// the A-1 cross-tenant mitigation is active and declining the Puts.
	// They return to 0 in 1.13.0 when the v7 key re-enables them. The
	// dashboard panel MUST carry that annotation or the series reads as
	// an error.
	uafRestactionsDeclined, err := m.Int64ObservableCounter(
		"snowplow_restactions_uaf_put_declined_total",
		metric.WithDescription("restactions-class L1 Puts declined because the RESTAction declares a userAccessFilter (A-1 mitigation). Non-zero is CORRECT in 1.12.x."))
	if err != nil {
		return err
	}
	uafWidgetsDeclined, err := m.Int64ObservableCounter(
		"snowplow_widgets_uaf_put_declined_total",
		metric.WithDescription("widgets-class L1 Puts declined under the A-1 UAF mitigation. Non-zero is CORRECT in 1.12.x."))
	if err != nil {
		return err
	}
	uafRAFullListBypass, err := m.Int64ObservableCounter(
		"snowplow_ra_full_list_uaf_bypass_total",
		metric.WithDescription("raFullList serves that bypassed the UAF-narrowed cache path (A-1 mitigation)."))
	if err != nil {
		return err
	}

	// --- dispatch L1, WIDENED. The aggregate above collapses every
	// class and GVR into one hit/miss pair, in which a widgets hit-rate
	// of 99% and a widgetContent hit-rate of 0% average to a
	// healthy-looking number while the portal is broken.
	dispatchL1Cells, err := m.Int64ObservableCounter(
		"snowplow_dispatch_l1_lookups_total",
		metric.WithDescription("Dispatch-L1 resolved-output lookups by class, gvr and outcome (hit/miss/seed_hit)."))
	if err != nil {
		return err
	}
	seedAttributableHits, err := m.Int64ObservableCounter(
		"snowplow_resolved_cache_hits_seed_attributable_total",
		metric.WithDescription("Resolved-cache hits served from a boot-seeded cell — did the boot seed warm anything a browser actually hit."))
	if err != nil {
		return err
	}

	// --- the fall-through family, per cell. The grand total was already
	// instrument #1; what was missing is the path|gvr|reason breakdown,
	// which is the half that carries the diagnostic value. Capped on the
	// observation path (see cache.FallthroughCellsSnapshot).
	fallthroughCells, err := m.Int64ObservableCounter(
		"snowplow_apiserver_fallthrough_cells_total",
		metric.WithDescription("GENUINE apiserver fall-throughs by path, gvr and reason."))
	if err != nil {
		return err
	}
	diagnosticTotal, err := m.Int64ObservableCounter(
		"snowplow_cache_diagnostic_total",
		metric.WithDescription("Cache-diagnostic observations that reach NO apiserver (1.12.4 reclassification). Expect this to dwarf the fall-through total ~8:1."))
	if err != nil {
		return err
	}
	diagnosticCells, err := m.Int64ObservableCounter(
		"snowplow_cache_diagnostic_cells_total",
		metric.WithDescription("Cache-diagnostic observations by path, gvr and reason."))
	if err != nil {
		return err
	}
	seriesTruncated, err := m.Int64ObservableCounter(
		"snowplow_metrics_series_truncated_total",
		metric.WithDescription("Cell series folded into gvr=__other__ by the OTLP cardinality cap, by family. Non-zero means a cardinality regression — alert."))
	if err != nil {
		return err
	}

	// --- boot SLI. Log-only before 1.12.4: an unexported expvar.Int
	// with no accessor, so nothing could alert on it. A healthy boot
	// leaves it 0.
	readyzBackstop, err := m.Int64ObservableCounter(
		"snowplow_readyz_backstop_fired_total",
		metric.WithDescription("Boots whose /readyz flipped Ready via the C2 backstop instead of firstNav-complete — a FAILED-but-serving boot. Alert on > 0."))
	if err != nil {
		return err
	}

	// --- L1 store occupancy + lifetime, keyed by stat. Computed by
	// Stats() every N seconds since forever and emitted only into an
	// INFO line the production LOG_LEVEL=warn discards.
	resolvedCache, err := m.Int64ObservableGauge(
		"snowplow_resolved_cache",
		metric.WithDescription("Resolved-output L1 store occupancy, ceilings, hit/miss/store and evict counters, labelled by stat."))
	if err != nil {
		return err
	}

	// --- informer servability, the leading indicator for the
	// informer-fallthrough-not-synced cell.
	informerServable, err := m.Int64ObservableGauge(
		"snowplow_informer_servable",
		metric.WithDescription("Informer counts by state (registered/synced/servable/watch_broken/confirmed). watch_broken > 0 is the stale-delete latch."))
	if err != nil {
		return err
	}

	// --- build identity, so every other panel can be pinned to a commit.
	buildInfo, err := m.Int64ObservableGauge(
		"snowplow_build_info",
		metric.WithDescription("Constant 1, labelled with the snowplow build (git short commit)."))
	if err != nil {
		return err
	}

	// Single callback reading every snapshot at collection time.
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(fallthroughTotal, int64(cache.FallthroughTotal()))
		o.ObserveInt64(assertionViolations, int64(cache.AssertionViolationsTotal()),
			metric.WithAttributes(attribute.String("check", "read_paths_scoped")))
		o.ObserveInt64(rbacPublishSeq, int64(cache.RBACGen()))

		s := cache.CRDDiscoveryStatsSnapshot()
		for stat, v := range map[string]uint64{
			"events_enqueued":      s.EventsEnqueued,
			"events_dropped":       s.EventsDropped,
			"events_processed":     s.EventsProcessed,
			"discovery_invoked":    s.DiscoveryInvoked,
			"discovery_skipped_ng": s.DiscoverySkippedNG,
			"deletes_processed":    s.DeletesProcessed,
			"delete_skipped_ng":    s.DeleteSkippedNG,
			"panics_recovered":     s.PanicsRecovered,
			"schema_relists_fired": s.SchemaRelistsFired,
			"schema_unchanged":     s.SchemaUnchanged,
		} {
			o.ObserveInt64(crdDiscovery, int64(v),
				metric.WithAttributes(attribute.String("stat", stat)))
		}

		o.ObserveInt64(registeredGVRs, registeredGVRCount())

		done, elapsed := cache.PrewarmCompleteSnapshot()
		o.ObserveInt64(prewarmDone, done)
		o.ObserveInt64(prewarmElapsed, elapsed)

		published, delivered, dropped, coalesced := cache.RefreshBroadcasterCounters()
		o.ObserveInt64(refreshPublished, int64(published))
		o.ObserveInt64(refreshDelivered, int64(delivered))
		o.ObserveInt64(refreshDropped, int64(dropped))
		o.ObserveInt64(refreshCoalesced, int64(coalesced))
		o.ObserveInt64(refreshSubscribers, int64(cache.RefreshSubscriberCount()))

		hits, misses, swaps, refused, denyUncached, entries := rbac.AuthzMemoSnapshot()
		o.ObserveInt64(memoHits, int64(hits))
		o.ObserveInt64(memoMisses, int64(misses))
		o.ObserveInt64(memoSwaps, int64(swaps))
		o.ObserveInt64(memoRefused, int64(refused))
		o.ObserveInt64(memoDenyUncached, int64(denyUncached))
		o.ObserveInt64(memoEntries, int64(entries))

		// --- prewarm engine ---
		engEnq, engProc, engYield, engPending := dispatchers.PrewarmEngineSnapshot()
		o.ObserveInt64(prewarmEngEnqueued, int64(engEnq))
		o.ObserveInt64(prewarmEngProcessed, int64(engProc))
		o.ObserveInt64(prewarmEngYield, int64(engYield))
		o.ObserveInt64(prewarmEngPending, engPending)

		// --- phase-1 pagination ---
		planned, seeded, apiRefPages, eligibleNoCont := dispatchers.Phase1PaginationSnapshot()
		o.ObserveInt64(phase1UnitsPlanned, int64(planned))
		o.ObserveInt64(phase1UnitsSeeded, int64(seeded))
		o.ObserveInt64(phase1ApiRefPages, int64(apiRefPages))
		o.ObserveInt64(phase1EligibleNoContinue, int64(eligibleNoCont))

		// --- phase-1 boot walk ---
		zeroChildren, walkObservations := dispatchers.Phase1WalkSnapshot()
		o.ObserveInt64(phase1WalkZeroChildren, int64(zeroChildren))
		o.ObserveInt64(phase1WalkObservations, int64(walkObservations))

		// --- phase-1 seed outcomes ---
		seedResolves, seedFailures, seedRBACDeny, seedOpFail := dispatchers.Phase1SeedSnapshot()
		o.ObserveInt64(phase1SeedResolves, int64(seedResolves))
		o.ObserveInt64(phase1SeedFailures, int64(seedFailures))
		o.ObserveInt64(phase1SeedRBACDeny, int64(seedRBACDeny))
		o.ObserveInt64(phase1SeedOpFail, int64(seedOpFail))

		// --- refresher pool ---
		rEnq, rComp, rFail, rRetried, rDropped,
			rSkipNoEntry, rSkipNoHandler, rSkipStageErr,
			rYielded, rCapped, rFloored, rQueueDepth := cache.RefresherSnapshot()
		for stat, v := range map[string]uint64{
			"enqueue":             rEnq,
			"completed":           rComp,
			"failed":              rFail,
			"retried":             rRetried,
			"dropped":             rDropped,
			"skipped_no_entry":    rSkipNoEntry,
			"skipped_no_handler":  rSkipNoHandler,
			"skipped_stage_error": rSkipStageErr,
			"yielded":             rYielded,
			"capped":              rCapped,
			"floored":             rFloored,
		} {
			o.ObserveInt64(refresher, int64(v),
				metric.WithAttributes(attribute.String("stat", stat)))
		}
		o.ObserveInt64(refresherQueueDepth, rQueueDepth)

		// --- SA-discovery ---
		sa := dynamic.SADiscoveryStatsSnapshot()
		for stat, v := range map[string]uint64{
			"builds":        sa.Builds,
			"invalidations": sa.Invalidations,
			"fallbacks":     sa.Fallbacks,
		} {
			o.ObserveInt64(saDiscovery, int64(v),
				metric.WithAttributes(attribute.String("stat", stat)))
		}

		// --- CRD-schema memo ---
		csHits, csMisses, csStale, csInval := schema.CRDSchemaMemoSnapshot()
		for stat, v := range map[string]uint64{
			"hits":          csHits,
			"misses":        csMisses,
			"stale_dropped": csStale,
			"invalidations": csInval,
		} {
			o.ObserveInt64(crdSchemaMemo, int64(v),
				metric.WithAttributes(attribute.String("stat", stat)))
		}

		// --- upstream controller / webhook health ---
		ctrlHealthy, ctrlUnhealthy, whTotal, whFail := cache.UpstreamHealthSnapshot()
		o.ObserveInt64(upstreamControllers, ctrlHealthy,
			metric.WithAttributes(attribute.String("health", "healthy")))
		o.ObserveInt64(upstreamControllers, ctrlUnhealthy,
			metric.WithAttributes(attribute.String("health", "unhealthy")))
		o.ObserveInt64(upstreamWebhooks, whTotal,
			metric.WithAttributes(attribute.String("policy", "total")))
		o.ObserveInt64(upstreamWebhooks, whFail,
			metric.WithAttributes(attribute.String("policy", "fail")))

		// --- dispatch L1, per (class, gvr, outcome) ---
		//
		// 1.12.4: emit one point per cell instead of one process-wide
		// hit/miss pair. seed_hit is a SUBSET of hit, not a third
		// disjoint outcome — a ratio panel must divide seed_hit by hit,
		// and hit by (hit+miss). Cardinality is 3 classes x registered
		// GVRs; the GVR set does not grow with composition count.
		for _, c := range dispatchers.DispatchL1LookupCells() {
			base := []attribute.KeyValue{
				attribute.String("class", c.Class),
				attribute.String("gvr", c.GVR),
			}
			for outcome, v := range map[string]uint64{
				"hit":      c.Hit,
				"miss":     c.Miss,
				"seed_hit": c.SeedHit,
			} {
				o.ObserveInt64(dispatchL1Cells, int64(v),
					metric.WithAttributes(append(append([]attribute.KeyValue{}, base...),
						attribute.String("outcome", outcome))...))
			}
		}
		o.ObserveInt64(seedAttributableHits, int64(dispatchers.HitsSeedAttributable()))

		// --- 1.12.4: A-1 UAF Put declines ---
		o.ObserveInt64(uafRestactionsDeclined, int64(cache.RestactionsUAFPutDeclined()))
		o.ObserveInt64(uafWidgetsDeclined, int64(cache.WidgetsUAFPutDeclined()))
		o.ObserveInt64(uafRAFullListBypass, int64(cache.RAFullListUAFBypass()))

		// --- 1.12.4: the two cell families, capped ---
		//
		// The cap is applied inside the snapshot accessors, on THIS path
		// only: /debug/vars keeps the full uncapped maps so the bench
		// harness is unaffected. Overflow lands on gvr="__other__" and
		// is counted by snowplow_metrics_series_truncated_total below,
		// so a cardinality regression is visible rather than silent.
		ftCells, _ := cache.FallthroughCellsSnapshot()
		for _, c := range ftCells {
			o.ObserveInt64(fallthroughCells, int64(c.Count),
				metric.WithAttributes(
					attribute.String("path", c.Path),
					attribute.String("gvr", c.GVR),
					attribute.String("reason", c.Reason)))
		}
		o.ObserveInt64(diagnosticTotal, int64(cache.DiagnosticTotal()))
		diagCells, _ := cache.CacheDiagnosticCellsSnapshot()
		for _, c := range diagCells {
			o.ObserveInt64(diagnosticCells, int64(c.Count),
				metric.WithAttributes(
					attribute.String("path", c.Path),
					attribute.String("gvr", c.GVR),
					attribute.String("reason", c.Reason)))
		}
		for family, n := range cache.SeriesTruncatedSnapshot() {
			o.ObserveInt64(seriesTruncated, int64(n),
				metric.WithAttributes(attribute.String("family", family)))
		}

		// --- 1.12.4: boot SLI ---
		o.ObserveInt64(readyzBackstop, dispatchers.ReadinessBackstopFired())

		// --- 1.12.4: L1 store occupancy + lifetime ---
		for stat, v := range cache.ResolvedCacheStatsByStat() {
			o.ObserveInt64(resolvedCache, v,
				metric.WithAttributes(attribute.String("stat", stat)))
		}

		// --- 1.12.4: informer servability ---
		reg, syncedN, servableN, brokenN, confirmedN := cache.ServableCountsSnapshot()
		for state, v := range map[string]int{
			"registered":   reg,
			"synced":       syncedN,
			"servable":     servableN,
			"watch_broken": brokenN,
			"confirmed":    confirmedN,
		} {
			o.ObserveInt64(informerServable, int64(v),
				metric.WithAttributes(attribute.String("state", state)))
		}

		// --- 1.12.4: build identity, constant 1 ---
		o.ObserveInt64(buildInfo, 1,
			metric.WithAttributes(attribute.String("version", buildLabel(build))))

		// --- RAFullList serve outcomes + index drift canary ---
		ra := cache.RAFullListServeSnapshot()
		for outcome, v := range map[string]uint64{
			"hit":            ra.Hit,
			"repopulate":     ra.Repopulate,
			"verified_slice": ra.VerifiedSlice,
			"fallback":       ra.Fallback,
		} {
			o.ObserveInt64(raFullListServe, int64(v),
				metric.WithAttributes(attribute.String("outcome", outcome)))
		}
		o.ObserveInt64(bindingsDeltaSkipped, int64(cache.BindingsIndexDeltaSkippedNonTyped()))
		return nil
	},
		fallthroughTotal, assertionViolations, rbacPublishSeq, crdDiscovery,
		registeredGVRs, prewarmDone, prewarmElapsed,
		refreshPublished, refreshDelivered, refreshDropped, refreshCoalesced, refreshSubscribers,
		memoHits, memoMisses, memoSwaps, memoRefused, memoDenyUncached, memoEntries,
		prewarmEngEnqueued, prewarmEngProcessed, prewarmEngYield, prewarmEngPending,
		phase1UnitsPlanned, phase1UnitsSeeded, phase1ApiRefPages, phase1EligibleNoContinue,
		phase1WalkZeroChildren, phase1WalkObservations,
		phase1SeedResolves, phase1SeedFailures, phase1SeedRBACDeny, phase1SeedOpFail,
		refresher, refresherQueueDepth,
		saDiscovery, crdSchemaMemo,
		upstreamControllers, upstreamWebhooks,
		raFullListServe, bindingsDeltaSkipped,
		// --- 1.12.4 ---
		uafRestactionsDeclined, uafWidgetsDeclined, uafRAFullListBypass,
		dispatchL1Cells, seedAttributableHits,
		fallthroughCells, diagnosticTotal, diagnosticCells, seriesTruncated,
		readyzBackstop, resolvedCache, informerServable, buildInfo,
	)
	return err
}

// buildLabel normalises the build string for the snowplow_build_info
// version attribute. An empty build — a local `go build` without the
// linker flag — becomes "unknown" rather than "", so a dashboard panel
// never renders a blank label that reads as a scrape failure. Matches
// the expvar route's normalisation in build_info_expvar.go, so the two
// surfaces report the same thing for the same binary.
func buildLabel(build string) string {
	if build == "" {
		return "unknown"
	}
	return build
}

// registeredGVRCount returns the number of GVRs with a registered informer,
// mirroring the `count` field of snowplow_plurals_registered_gvrs. Returns
// 0 when the global watcher is nil (cache off / not wired).
func registeredGVRCount() int64 {
	rw := cache.Global()
	if rw == nil {
		return 0
	}
	return int64(len(rw.RegisteredGVRs()))
}
