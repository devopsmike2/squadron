// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package splunk

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// Running this integration test against a real Splunk:
//
// The splunk/splunk container is heavy (large image, ~1-2 min startup) and
// needs a license acceptance and an admin password, so — unlike the light
// Loki container — it is deliberately NOT wired as an always-on `services:`
// container in pr-tests.yml (that would bloat and flake CI). CI coverage for
// this connector is the offline httptest unit tests; this test is env-gated
// and skips by default.
//
// To run it locally against a throwaway Splunk:
//
//	docker run -d --name splunk -p 8089:8089 \
//	  -e SPLUNK_START_ARGS=--accept-license \
//	  -e SPLUNK_PASSWORD=Changed123! \
//	  splunk/splunk:latest
//
//	TEST_SPLUNK_URL=https://localhost:8089 \
//	TEST_SPLUNK_USERNAME=admin \
//	TEST_SPLUNK_PASSWORD=Changed123! \
//	TEST_SPLUNK_INSECURE=true \
//	  go test ./internal/connectors/splunk/ -run Integration -v
//
// Alternatively set TEST_SPLUNK_TOKEN for an auth token instead of basic auth.

// testConnector builds a Splunk connector from the TEST_SPLUNK_* env. It
// skips when TEST_SPLUNK_URL is unset so local `go test ./...` stays green
// without a Splunk — mirroring the Loki integration test's TEST_LOKI_URL
// pattern.
func testConnector(t *testing.T) connectors.Connector {
	t.Helper()
	base := os.Getenv("TEST_SPLUNK_URL")
	if base == "" {
		t.Skip("TEST_SPLUNK_URL not set; skipping Splunk integration test")
	}

	settings := map[string]string{}
	if os.Getenv("TEST_SPLUNK_INSECURE") == "true" {
		settings[SettingInsecureSkipVerify] = "true"
	}

	var creds connectors.ConnectorCredentials
	switch {
	case os.Getenv("TEST_SPLUNK_TOKEN") != "":
		creds.Token = os.Getenv("TEST_SPLUNK_TOKEN")
		settings[SettingAuthMode] = AuthBearer
	case os.Getenv("TEST_SPLUNK_USERNAME") != "":
		creds.Username = os.Getenv("TEST_SPLUNK_USERNAME")
		creds.Password = os.Getenv("TEST_SPLUNK_PASSWORD")
		settings[SettingAuthMode] = AuthBasic
	default:
		t.Skip("TEST_SPLUNK_URL set but no TEST_SPLUNK_TOKEN or TEST_SPLUNK_USERNAME; skipping")
	}

	conn, err := New(connectors.Config{
		ID:       "splunk-it",
		Type:     TypeName,
		Endpoint: base,
		Settings: settings,
	}, creds)
	if err != nil {
		t.Fatalf("new connector: %v", err)
	}
	waitReady(t, conn)
	return conn
}

// waitReady polls the connector's HealthCheck until Splunk answers or the
// deadline elapses. A freshly started splunkd takes a while to bind its
// management port and finish first-run setup, so a single one-shot check
// races container startup and flakes — block on readiness here rather than
// assuming the backend is up the instant the test starts (the readiness-poll
// gotcha the Loki connector's integration test also observes).
func waitReady(t *testing.T, conn connectors.Connector) {
	t.Helper()
	deadline := time.Now().Add(180 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := conn.HealthCheck(ctx)
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("Splunk did not become ready within 180s: %v", lastErr)
}

func TestIntegration_HealthCheck(t *testing.T) {
	conn := testConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := conn.HealthCheck(ctx); err != nil {
		t.Fatalf("health check against real Splunk failed: %v", err)
	}
}

func TestIntegration_LogQuery(t *testing.T) {
	conn := testConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	now := time.Now().UTC()
	// Splunk's own _internal index always has events, so this exercises the
	// full event->LogRow path against a real backend. An empty result set is
	// still a valid response; we assert the query round-trips and normalizes.
	res, err := conn.Query(ctx, connectors.NormalizedQuery{
		Signal:    connectors.SignalLogs,
		TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Matchers:  []connectors.LabelMatcher{{Label: "index", Op: connectors.MatchEqual, Value: "_internal"}},
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("log query against real Splunk failed: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result failed validation: %v", err)
	}
	if res.Kind != connectors.ResultLogs {
		t.Fatalf("want logs result, got %q", res.Kind)
	}
}

func TestIntegration_ScalarStat(t *testing.T) {
	conn := testConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	now := time.Now().UTC()
	// A `stats count` with no group-by over _internal returns a single scalar
	// row, exercising the scalar (SLO / burn-rate decision-context) path.
	res, err := conn.Query(ctx, connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Aggregation: connectors.AggCount,
		Matchers:    []connectors.LabelMatcher{{Label: "index", Op: connectors.MatchEqual, Value: "_internal"}},
	})
	if err != nil {
		t.Fatalf("scalar stat query against real Splunk failed: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result failed validation: %v", err)
	}
	if res.Kind != connectors.ResultScalar {
		t.Fatalf("want scalar result, got %q", res.Kind)
	}
}
