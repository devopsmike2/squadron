// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"context"
	"errors"
	"time"
)

// mockConnector is an in-memory Connector used ONLY in tests. It is the
// stand-in for a real backend so the interface contract, the registry,
// and the result model can be exercised without any external vendor.
// No production code registers it — the framework stays OSS-inert.
//
// It echoes back canned results per signal and records the last query it
// received so contract tests can assert the query reached it intact.
type mockConnector struct {
	cfg   Config
	creds ConnectorCredentials

	// healthErr, when non-nil, is what HealthCheck returns — lets tests
	// simulate an unreachable backend.
	healthErr error

	// lastQuery records the most recent query passed to Query.
	lastQuery NormalizedQuery
}

// mockType is the registry type name the mock registers under in tests.
const mockType = "mock"

// newMockFactory returns a Factory that builds a mockConnector. The
// optional healthErr is baked into every connector the factory produces.
func newMockFactory(healthErr error) Factory {
	return func(cfg Config, creds ConnectorCredentials) (Connector, error) {
		if cfg.Endpoint == "" {
			return nil, errors.New("mock: endpoint is required")
		}
		return &mockConnector{cfg: cfg, creds: creds, healthErr: healthErr}, nil
	}
}

func (m *mockConnector) Describe() Descriptor {
	return Descriptor{
		Type:        mockType,
		DisplayName: "Mock Connector (test only)",
		Description: "In-memory connector used by the connector-framework tests.",
	}
}

func (m *mockConnector) Capabilities() Capabilities {
	return Capabilities{
		Signals:             []Signal{SignalMetrics, SignalLogs},
		Modes:               []Mode{ModeFederated},
		SupportsAggregation: true,
		SupportsScalar:      true,
	}
}

func (m *mockConnector) Query(_ context.Context, q NormalizedQuery) (NormalizedResult, error) {
	if err := q.Validate(); err != nil {
		return NormalizedResult{}, err
	}
	m.lastQuery = q

	switch q.Signal {
	case SignalMetrics:
		return NewMetricsResult([]MetricSeries{{
			Labels:  map[string]string{"selector": q.Selector},
			Samples: []Sample{{Timestamp: q.TimeRange.End, Value: 42}},
		}}), nil
	case SignalLogs:
		return NewLogsResult([]LogRow{{
			Timestamp: q.TimeRange.End,
			Line:      "mock log line for " + q.Selector,
			Labels:    map[string]string{"selector": q.Selector},
		}}), nil
	default:
		return NormalizedResult{}, errors.New("mock: unsupported signal")
	}
}

func (m *mockConnector) HealthCheck(_ context.Context) error {
	return m.healthErr
}

// mockBurnRateResult is a helper the result-model tests use to build a
// representative SLO/burn-rate scalar, keeping that shape in one place.
func mockBurnRateResult() NormalizedResult {
	now := time.Now().UTC()
	return NewScalarResult(ScalarResult{
		Value:       14.4,
		Unit:        "multiple",
		Kind:        ScalarBurnRate,
		Objective:   "checkout-availability",
		SLOTarget:   0.999,
		Threshold:   14.4,
		Breached:    true,
		Window:      TimeRange{Start: now.Add(-time.Hour), End: now},
		EvaluatedAt: now,
	})
}

// compile-time assertion that the mock satisfies the interface.
var _ Connector = (*mockConnector)(nil)
