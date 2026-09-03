// Package otelresource builds the ONE OpenTelemetry Resource that
// snowplow's three signal pipelines — traces, metrics and logs — all
// stamp onto every record they export.
//
// WHY A SHARED HELPER. Before 1.12.4 each of internal/tracing,
// internal/metrics and internal/logging constructed its own resource
// with an identical two-attribute literal. They agreed by coincidence,
// and logging.go's own doc-comment relies on that agreement ("identical
// to the TracerProvider's resource so logs and spans agree"). A single
// constructor makes the agreement structural: a future attribute cannot
// be added to spans and forgotten on metrics.
//
// # What the collector does with this, and why each attribute is here
//
// The node-local daemonset agent runs a `k8sattributes` processor whose
// `extract.metadata` list contains `service.name`, `service.version`,
// `service.namespace` and `service.instance.id` verbatim, plus every pod
// label via `key_regex: (.*)`. Two consequences drive this file.
//
// (1) DIVERGENCE. The chart labels every pod
// `app.kubernetes.io/version: {{ .Chart.AppVersion }}`, so the processor
// would derive service.version = the CHART version (1.12.4) while the
// SDK sets it to the GIT SHORT COMMIT. Two values, one key. The
// processor's documented contract is not to overwrite an attribute
// already on the resource, so the SDK should win — but that is inferred,
// not traced, and F7(a) settles it after deploy. Setting all four
// explicitly is correct EITHER WAY: there is nothing left to derive.
// The chart appVersion is not lost — it stays on the
// app.kubernetes.io/version label, which the processor copies in under
// its own raw key regardless.
//
// (2) ASSOCIATION [C5], the more dangerous one. The processor's
// `pod_association` is, in order, `k8s.pod.ip` -> `k8s.pod.uid` ->
// `connection`. Before 1.12.4 snowplow's resource carried NEITHER an IP
// nor a UID, so association fell through to `connection` — the source
// address of the OTLP connection. Snowplow exports to
// `$(HOST_IP):4318`, a hostPort on its OWN node, and pod->node-hostPort
// traffic on GKE is commonly SNAT'd to the node address. Association
// would then resolve to the NODE, not the snowplow pod, and NO `k8s.*`
// attribute and NO pod label would attach to any snowplow row. That
// silently breaks the "chart appVersion is not lost" claim above and
// every per-pod / per-replica filter on the dashboard. Setting
// `k8s.pod.uid` from the downward API removes the dependency entirely:
// it is the documented deterministic association source.
//
// # Env contract
//
// The three pod-identity values come from downward-API `env:` entries
// the chart's Deployment adds (POD_UID / POD_NAME / POD_NAMESPACE,
// alongside HOST_IP for the endpoint). They are read here rather than
// threaded through three Setup signatures so all three pipelines
// observe the same values with no call-site skew.
//
// EVERY ATTRIBUTE IS OMITTED WHEN ITS SOURCE IS EMPTY. An attribute set
// to "" is worse than an absent one: it is a resource key the collector
// sees as present-but-blank, which suppresses the processor's own
// enrichment for that key and produces a permanently empty column. So a
// binary running outside Kubernetes (a unit test, a local run) emits
// just service.name and service.version, exactly as before 1.12.4.
package otelresource

import (
	"context"

	"github.com/krateo-platformops/plumbing/env"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	// ServiceName is the resource service.name reported on every span,
	// metric and log record. It is the otel_logs.ServiceName /
	// otel_traces.ServiceName primary key every dashboard filters on, so
	// it must be identical across the three pipelines — which is now
	// structural rather than a convention.
	ServiceName = "snowplow"

	// EnvPodUID / EnvPodName / EnvPodNamespace are the downward-API
	// entries the chart's Deployment sets. Unset outside Kubernetes.
	EnvPodUID       = "POD_UID"
	EnvPodName      = "POD_NAME"
	EnvPodNamespace = "POD_NAMESPACE"
)

// Build returns the resource for all three pipelines.
//
// build is the snowplow build string (main.build — the git short
// commit), recorded as service.version. It is the artifact identity: it
// answers "which commit produced this row", which the chart appVersion
// cannot.
//
// nsFallback supplies service.namespace when POD_NAMESPACE is unset —
// callers pass kubeutil.ServiceAccountNamespace, which reads the
// projected service-account token's namespace file. It is a func rather
// than a string so the file read only happens when the env var is
// actually missing, and its error is DISCARDED on purpose: outside a
// pod there is no namespace file, which is not a reason to fail a
// telemetry pipeline. The attribute is simply omitted.
//
// PARTIAL-RESOURCE HANDLING. resource.New can return a NON-NIL resource
// together with a non-fatal merge error (schema-URL skew being the
// common case). The pre-1.12.4 pipelines each handled this by keeping
// whatever came back, and that behaviour is preserved exactly: on error
// with a non-nil resource we return the partial resource and NO error,
// so a schema skew degrades the attribute set rather than disabling the
// signal. Only a nil resource falls back to resource.Default().
func Build(ctx context.Context, build string, nsFallback func() (string, error)) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(build),
	}

	ns := env.String(EnvPodNamespace, "")
	if ns == "" && nsFallback != nil {
		if v, err := nsFallback(); err == nil {
			ns = v
		}
	}
	if ns != "" {
		// service.namespace groups snowplow with the other Krateo
		// components in the same install; k8s.namespace.name is what the
		// k8sattributes processor would otherwise have to derive.
		attrs = append(attrs,
			semconv.ServiceNamespace(ns),
			semconv.K8SNamespaceName(ns),
		)
	}

	if podName := env.String(EnvPodName, ""); podName != "" {
		// service.instance.id distinguishes replicas: without it, two
		// pods' metrics are indistinguishable series and a per-replica
		// breakdown is impossible.
		attrs = append(attrs,
			semconv.ServiceInstanceID(podName),
			semconv.K8SPodName(podName),
		)
	}

	if podUID := env.String(EnvPodUID, ""); podUID != "" {
		// [C5] The deterministic pod_association key. Without it the
		// processor falls through to source-IP matching, which the
		// hostPort hop can defeat.
		attrs = append(attrs, semconv.K8SPodUID(podUID))
	}

	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		if res == nil {
			return resource.Default(), nil
		}
		// Partial resource + non-fatal merge error: use what came back,
		// exactly as the three pipelines did before this helper existed.
		return res, nil
	}
	return res, nil
}
