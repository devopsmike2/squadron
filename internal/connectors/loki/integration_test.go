// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package loki

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// testConnector builds a Loki connector against a real Loki from
// TEST_LOKI_URL (a `services: loki` container in CI). It skips when the
// env var is unset so local `go test ./...` stays green without a Loki —
// mirroring the ADR 0033 Postgres integration test's TEST_POSTGRES_DSN
// pattern.
func testConnector(t *testing.T) connectors.Connector {
	t.Helper()
	base := os.Getenv("TEST_LOKI_URL")
	if base == "" {
		t.Skip("TEST_LOKI_URL not set; skipping Loki integration test")
	}
	conn, err := New(connectors.Config{
		ID:       "loki-it",
		Type:     TypeName,
		Endpoint: base,
	}, connectors.ConnectorCredentials{})
	if err != nil {
		t.Fatalf("new connector: %v", err)
	}
	waitReady(t, conn)
	return conn
}

// waitReady polls the connector's HealthCheck until Loki reports ready or the
// deadline elapses. Loki 2.9.8's /ready endpoint returns 503 for a short
// window after container start (the ingester has to join the ring and clear
// the default min-ready-duration) — so a single one-shot check races the
// `services: loki` container's startup in CI and flakes with a 503. This is
// the runtime gate the workflow comment refers to: block on readiness here
// rather than assuming the backend is up the instant the job starts.
func waitReady(t *testing.T, conn connectors.Connector) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := conn.HealthCheck(ctx)
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Loki did not become ready within 90s: %v", lastErr)
}

func TestIntegration_HealthCheck(t *testing.T) {
	conn := testConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := conn.HealthCheck(ctx); err != nil {
		t.Fatalf("health check against real Loki failed: %v", err)
	}
}

func TestIntegration_LogQuery(t *testing.T) {
	conn := testConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now().UTC()
	// A broad regex selector that matches any stream by job label; an empty
	// result set is a valid response (a fresh Loki has no logs). We assert
	// the query round-trips and normalizes without error.
	res, err := conn.Query(ctx, connectors.NormalizedQuery{
		Signal:    connectors.SignalLogs,
		TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Matchers:  []connectors.LabelMatcher{{Label: "job", Op: connectors.MatchRegex, Value: ".+"}},
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("log query against real Loki failed: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result failed validation: %v", err)
	}
	if res.Kind != connectors.ResultLogs {
		t.Fatalf("want logs result, got %q", res.Kind)
	}
}

func TestIntegration_MetricQuery(t *testing.T) {
	conn := testConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now().UTC()
	res, err := conn.Query(ctx, connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now, Step: time.Minute},
		Aggregation: connectors.AggRate,
		Matchers:    []connectors.LabelMatcher{{Label: "job", Op: connectors.MatchRegex, Value: ".+"}},
	})
	if err != nil {
		t.Fatalf("metric query against real Loki failed: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result failed validation: %v", err)
	}
	if res.Kind != connectors.ResultMetrics {
		t.Fatalf("want metrics result, got %q", res.Kind)
	}
}
