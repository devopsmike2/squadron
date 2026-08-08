// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validQuery returns a well-formed metrics query for the given signal.
func validQuery(signal Signal) NormalizedQuery {
	now := time.Now().UTC()
	return NormalizedQuery{
		Signal:    signal,
		Selector:  "http_requests_total",
		TimeRange: TimeRange{Start: now.Add(-time.Hour), End: now, Step: time.Minute},
		Matchers: []LabelMatcher{
			{Label: "service", Op: MatchEqual, Value: "checkout"},
		},
	}
}

// TestConnectorContract exercises the Connector interface surface
// through the mock: Describe, Capabilities, Query (per signal), and
// HealthCheck (both healthy and failing).
func TestConnectorContract(t *testing.T) {
	factory := newMockFactory(nil)
	conn, err := factory(Config{ID: "c1", Type: mockType, Endpoint: "http://mock"}, ConnectorCredentials{})
	require.NoError(t, err)

	t.Run("describe is stable identity", func(t *testing.T) {
		d := conn.Describe()
		assert.Equal(t, mockType, d.Type)
		assert.NotEmpty(t, d.DisplayName)
	})

	t.Run("capabilities advertise signals and mode", func(t *testing.T) {
		caps := conn.Capabilities()
		assert.True(t, caps.Supports(SignalMetrics))
		assert.True(t, caps.Supports(SignalLogs))
		assert.False(t, caps.Supports(SignalTraces))
		assert.True(t, caps.SupportsMode(ModeFederated))
		assert.False(t, caps.SupportsMode(ModeIngest))
	})

	t.Run("query metrics returns a valid metrics result", func(t *testing.T) {
		res, err := conn.Query(context.Background(), validQuery(SignalMetrics))
		require.NoError(t, err)
		require.NoError(t, res.Validate())
		assert.Equal(t, ResultMetrics, res.Kind)
		require.Len(t, res.Series, 1)
		assert.Equal(t, "http_requests_total", res.Series[0].Labels["selector"])
	})

	t.Run("query logs returns a valid logs result", func(t *testing.T) {
		res, err := conn.Query(context.Background(), validQuery(SignalLogs))
		require.NoError(t, err)
		require.NoError(t, res.Validate())
		assert.Equal(t, ResultLogs, res.Kind)
		require.Len(t, res.Logs, 1)
	})

	t.Run("query rejects a malformed query", func(t *testing.T) {
		_, err := conn.Query(context.Background(), NormalizedQuery{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEmptySignal)
	})

	t.Run("healthcheck passes when backend is healthy", func(t *testing.T) {
		require.NoError(t, conn.HealthCheck(context.Background()))
	})

	t.Run("healthcheck surfaces backend failure", func(t *testing.T) {
		wantErr := errors.New("endpoint unreachable")
		failing, err := newMockFactory(wantErr)(Config{ID: "c2", Type: mockType, Endpoint: "http://mock"}, ConnectorCredentials{})
		require.NoError(t, err)
		assert.ErrorIs(t, failing.HealthCheck(context.Background()), wantErr)
	})
}

// TestNormalizedQueryValidate covers the query-validation contract.
func TestNormalizedQueryValidate(t *testing.T) {
	now := time.Now().UTC()
	base := func() NormalizedQuery {
		return NormalizedQuery{
			Signal:    SignalMetrics,
			Selector:  "m",
			TimeRange: TimeRange{Start: now.Add(-time.Hour), End: now},
		}
	}

	t.Run("valid", func(t *testing.T) {
		require.NoError(t, base().Validate())
	})

	t.Run("empty signal", func(t *testing.T) {
		q := base()
		q.Signal = ""
		assert.ErrorIs(t, q.Validate(), ErrEmptySignal)
	})

	t.Run("inverted time range", func(t *testing.T) {
		q := base()
		q.TimeRange = TimeRange{Start: now, End: now.Add(-time.Hour)}
		assert.ErrorIs(t, q.Validate(), ErrInvalidTimeRange)
	})

	t.Run("zero time range", func(t *testing.T) {
		q := base()
		q.TimeRange = TimeRange{}
		assert.ErrorIs(t, q.Validate(), ErrInvalidTimeRange)
	})

	t.Run("bad matcher operator", func(t *testing.T) {
		q := base()
		q.Matchers = []LabelMatcher{{Label: "svc", Op: MatchOp("~~"), Value: "x"}}
		assert.ErrorIs(t, q.Validate(), ErrInvalidMatcher)
	})

	t.Run("empty matcher label", func(t *testing.T) {
		q := base()
		q.Matchers = []LabelMatcher{{Label: "", Op: MatchEqual, Value: "x"}}
		assert.ErrorIs(t, q.Validate(), ErrInvalidMatcher)
	})

	t.Run("unknown aggregation", func(t *testing.T) {
		q := base()
		q.Aggregation = Aggregation("median")
		assert.ErrorIs(t, q.Validate(), ErrInvalidAggregation)
	})
}
