// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package connectors is the telemetry query-connector framework (ADR
// 0034 slice 1). It defines the seam through which Squadron reasons over
// telemetry that already lives in an external backend (Splunk, Loki,
// Datadog, Dynatrace, or a data lake) instead of only the OTLP that
// flows into Squadron's own DuckDB store.
//
// This package is the FRAMEWORK ONLY. It ships:
//
//   - Connector: the provider interface every vendor implements.
//   - Registry: register connector types by name and instantiate one
//     from a stored Config.
//   - NormalizedQuery / NormalizedResult: the vendor-neutral query and
//     result model. The per-vendor translation (normalized -> SPL /
//     LogQL / DQL / SQL) and result normalization back is each
//     connector's job, landing in later slices.
//   - Credentials: a connector-scoped secret shape sealed at rest by
//     REUSING the discovery credstore's AES-256-GCM primitive
//     (internal/discovery/credstore) rather than inventing a second
//     sealing scheme.
//
// It is deliberately OSS-inert: no real backend connector is registered
// anywhere in production code, so the framework cannot be selected in a
// running deployment yet. The first federated vendor (Loki) lands in
// ADR 0034 slice 2; scheduled ingest and the auto-abort rollout hook are
// later slices. The result model already carries the scalar SLO /
// burn-rate shape (see ScalarResult) so those later consumers do not
// need a model change.
package connectors

import "context"

// Signal is the class of telemetry a query targets. A connector
// advertises the signals it can serve via Capabilities.
type Signal string

const (
	// SignalMetrics is numeric timeseries data (gauges, counters,
	// histograms). Normalized results carry MetricSeries.
	SignalMetrics Signal = "metrics"

	// SignalLogs is log lines. Normalized results carry LogRow values.
	SignalLogs Signal = "logs"

	// SignalTraces is distributed traces. Reserved for a later slice;
	// no S1 connector serves it, but the constant lives here so the
	// query/result model does not have to change when one does.
	SignalTraces Signal = "traces"
)

// Mode is an execution strategy a connector supports. ADR 0034 defines
// two: federated live query and scheduled ingest.
type Mode string

const (
	// ModeFederated translates a NormalizedQuery into the backend's own
	// query language, executes it against the source, and normalizes the
	// results back. Always current; no local storage. This is the S1/S2
	// path.
	ModeFederated Mode = "federated"

	// ModeIngest pulls a window of data from the source into DuckDB on
	// the existing scheduler, then serves it through the local query
	// path. Reserved for ADR 0034 slice 4; declared here so a connector
	// can advertise it without a Capabilities schema change.
	ModeIngest Mode = "ingest"
)

// Capabilities describes what a connector can do so callers can pick a
// connector for a query (and later gate UI) without instantiating one.
// It is intentionally small; fields are additive across slices.
type Capabilities struct {
	// Signals lists the telemetry classes the connector can serve.
	Signals []Signal

	// Modes lists the execution strategies the connector supports.
	// An S1 federated connector reports [ModeFederated].
	Modes []Mode

	// SupportsAggregation reports whether the backend can push down the
	// NormalizedQuery.Aggregation (sum/avg/rate/...). When false, the
	// caller must not send an aggregation the connector cannot honor.
	SupportsAggregation bool

	// SupportsScalar reports whether the connector can return a
	// ScalarResult (an SLO / burn-rate value). This is what a rollout
	// health check keys on when choosing a source for decision context.
	SupportsScalar bool
}

// Supports reports whether the capability set includes the given signal.
func (c Capabilities) Supports(s Signal) bool {
	for _, have := range c.Signals {
		if have == s {
			return true
		}
	}
	return false
}

// SupportsMode reports whether the capability set includes the given mode.
func (c Capabilities) SupportsMode(m Mode) bool {
	for _, have := range c.Modes {
		if have == m {
			return true
		}
	}
	return false
}

// Descriptor is the static identity of a connector type, returned by
// Describe. Unlike Capabilities (which can vary with configuration) a
// Descriptor is fixed for the type.
type Descriptor struct {
	// Type is the registry key, e.g. "loki" or "splunk". Matches
	// Config.Type.
	Type string

	// DisplayName is the operator-facing label, e.g. "Grafana Loki".
	DisplayName string

	// Description is a one-line summary of what the connector queries.
	Description string
}

// Connector is the provider interface every telemetry backend
// implements. It is deliberately small and stable: the hard,
// vendor-specific work (translating NormalizedQuery into SPL/LogQL/DQL/
// SQL and normalizing results back) lives inside each implementation,
// not in this surface.
//
// Implementations must be safe for concurrent use — a single connector
// instance serves many queries.
//
// The scheduled-ingest method (ADR 0034 slice 4) is intentionally NOT
// on this interface in slice 1. Adding it now would force every S1
// connector and the mock to carry a stub; it will be introduced as a
// separate optional interface (an IngestConnector the scheduler
// type-asserts for) when that slice lands, so the core read path stays
// minimal.
type Connector interface {
	// Describe returns the connector's static identity.
	Describe() Descriptor

	// Capabilities reports what this configured connector can serve.
	Capabilities() Capabilities

	// Query executes a federated query against the backend and returns
	// a normalized result. Implementations translate the NormalizedQuery
	// into the backend's own language, run it, and normalize the
	// response. The context carries cancellation and deadline; callers
	// set the per-query timeout. Query never mutates the backend
	// (ADR 0034 is read/query only).
	Query(ctx context.Context, q NormalizedQuery) (NormalizedResult, error)

	// HealthCheck validates the endpoint and credentials without running
	// a data query — the "Test connection" action. It returns nil when
	// the backend is reachable and the credentials authenticate, or a
	// descriptive error otherwise. Implementations must not leak secret
	// material into the returned error.
	HealthCheck(ctx context.Context) error
}
