// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package loki

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- LogQL translation (pure, no I/O) --------------------------------

func TestBuildLogQL(t *testing.T) {
	now := time.Now().UTC()
	tr := connectors.TimeRange{Start: now.Add(-time.Hour), End: now}
	trStep := connectors.TimeRange{Start: now.Add(-time.Hour), End: now, Step: time.Minute}

	tests := []struct {
		name string
		q    connectors.NormalizedQuery
		want string
	}{
		{
			name: "logs stream selector from a single matcher",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				TimeRange: tr,
				Matchers:  []connectors.LabelMatcher{{Label: "service", Op: connectors.MatchEqual, Value: "checkout"}},
			},
			want: `{service="checkout"}`,
		},
		{
			name: "logs all four matcher operators map to LogQL",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				TimeRange: tr,
				Matchers: []connectors.LabelMatcher{
					{Label: "service", Op: connectors.MatchEqual, Value: "checkout"},
					{Label: "env", Op: connectors.MatchNotEqual, Value: "prod"},
					{Label: "path", Op: connectors.MatchRegex, Value: "/api.*"},
					{Label: "region", Op: connectors.MatchNotRegex, Value: "eu.*"},
				},
			},
			want: `{service="checkout", env!="prod", path=~"/api.*", region!~"eu.*"}`,
		},
		{
			name: "logs Selector becomes a line filter",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				Selector:  "error",
				TimeRange: tr,
				Matchers:  []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
			},
			want: `{app="api"} |= "error"`,
		},
		{
			name: "logs raw {..} Selector passes through when no matchers",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				Selector:  `{app="api", env="prod"}`,
				TimeRange: tr,
			},
			want: `{app="api", env="prod"}`,
		},
		{
			name: "metrics signal defaults to count_over_time",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalMetrics,
				TimeRange: trStep,
				Matchers:  []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
			},
			want: `count_over_time({app="api"} [60s])`,
		},
		{
			name: "rate aggregation on logs",
			q: connectors.NormalizedQuery{
				Signal:      connectors.SignalLogs,
				TimeRange:   trStep,
				Aggregation: connectors.AggRate,
				Matchers:    []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
			},
			want: `rate({app="api"} [60s])`,
		},
		{
			name: "rate with group-by wraps in sum by",
			q: connectors.NormalizedQuery{
				Signal:      connectors.SignalMetrics,
				TimeRange:   trStep,
				Aggregation: connectors.AggRate,
				GroupBy:     []string{"service"},
				Matchers:    []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
			},
			want: `sum by (service) (rate({app="api"} [60s]))`,
		},
		{
			name: "sum aggregation wraps count_over_time with by-clause",
			q: connectors.NormalizedQuery{
				Signal:      connectors.SignalMetrics,
				TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now, Step: 5 * time.Minute},
				Aggregation: connectors.AggSum,
				GroupBy:     []string{"service", "region"},
				Matchers:    []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
			},
			want: `sum by (service, region)(count_over_time({app="api"} [300s]))`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLogQL(tc.q)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildLogQL_Errors(t *testing.T) {
	now := time.Now().UTC()
	tr := connectors.TimeRange{Start: now.Add(-time.Hour), End: now}

	t.Run("no matchers and no selector is an error", func(t *testing.T) {
		_, err := buildLogQL(connectors.NormalizedQuery{Signal: connectors.SignalLogs, TimeRange: tr})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stream selector is required")
	})

	t.Run("raw selector conflicting with matchers is an error", func(t *testing.T) {
		_, err := buildLogQL(connectors.NormalizedQuery{
			Signal:    connectors.SignalLogs,
			Selector:  `{a="b"}`,
			TimeRange: tr,
			Matchers:  []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
		})
		require.Error(t, err)
	})
}

// ---- End-to-end via httptest -----------------------------------------

// capturedRequest records what the connector sent so tests can assert on
// the URL, query params, and headers.
type capturedRequest struct {
	path     string
	query    string
	orgID    string
	authz    string
	extra    string
	hadBasic bool
	user     string
	pass     string
}

// newServer starts an httptest server whose handler serves respBody with
// status 200 for the given endpoint and records the request into cap.
func newServer(t *testing.T, respBody string, cap *capturedRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.query = r.URL.Query().Get("query")
		cap.orgID = r.Header.Get("X-Scope-OrgID")
		cap.authz = r.Header.Get("Authorization")
		cap.extra = r.Header.Get("X-Extra")
		if u, p, ok := r.BasicAuth(); ok {
			cap.hadBasic = true
			cap.user, cap.pass = u, p
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustConnector(t *testing.T, cfg connectors.Config, creds connectors.ConnectorCredentials) connectors.Connector {
	t.Helper()
	c, err := New(cfg, creds)
	require.NoError(t, err)
	return c
}

func TestQuery_LogsStreamsToLogRows(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"streams","result":[
		{"stream":{"app":"api","level":"error"},"values":[
			["1700000000000000000","boom happened"],
			["1700000001000000000","still broken"]
		]}
	]}}`
	var cap capturedRequest
	srv := newServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:    connectors.SignalLogs,
		Selector:  "boom",
		TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Matchers:  []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
		Limit:     2,
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	assert.Equal(t, "/loki/api/v1/query_range", cap.path)
	assert.Equal(t, `{app="api"} |= "boom"`, cap.query)

	assert.Equal(t, connectors.ResultLogs, res.Kind)
	require.Len(t, res.Logs, 2)
	assert.Equal(t, "boom happened", res.Logs[0].Line)
	assert.Equal(t, "api", res.Logs[0].Labels["app"])
	assert.Equal(t, int64(1700000000000000000), res.Logs[0].Timestamp.UnixNano())
	// Row count equals Limit => truncation warning.
	assert.Contains(t, res.Warnings, "results truncated at limit")
}

func TestQuery_MatrixToSeries(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"service":"checkout"},"values":[[1700000000,"1.5"],[1700000060,"2.5"]]}
	]}}`
	var cap capturedRequest
	srv := newServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now, Step: time.Minute},
		Aggregation: connectors.AggRate,
		Matchers:    []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	// Stepped metric query uses query_range.
	assert.Equal(t, "/loki/api/v1/query_range", cap.path)
	assert.Equal(t, `rate({app="api"} [60s])`, cap.query)

	assert.Equal(t, connectors.ResultMetrics, res.Kind)
	require.Len(t, res.Series, 1)
	assert.Equal(t, "checkout", res.Series[0].Labels["service"])
	require.Len(t, res.Series[0].Samples, 2)
	assert.Equal(t, 1.5, res.Series[0].Samples[0].Value)
	assert.Equal(t, int64(1700000000), res.Series[0].Samples[0].Timestamp.Unix())
}

func TestQuery_SingleValueVectorToScalar(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{},"value":[1700000000,"14.4"]}
	]}}`
	var cap capturedRequest
	srv := newServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	// Instant metric query (Step == 0) => /query, which returns a vector.
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Aggregation: connectors.AggRate,
		Matchers:    []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	assert.Equal(t, "/loki/api/v1/query", cap.path)
	assert.Equal(t, connectors.ResultScalar, res.Kind)
	require.NotNil(t, res.Scalar)
	assert.Equal(t, 14.4, res.Scalar.Value)
	assert.Equal(t, connectors.ScalarGeneric, res.Scalar.Kind)
	assert.Equal(t, int64(1700000000), res.Scalar.EvaluatedAt.Unix())
}

func TestQuery_MultiValueVectorToSeries(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"service":"a"},"value":[1700000000,"1"]},
		{"metric":{"service":"b"},"value":[1700000000,"2"]}
	]}}`
	var cap capturedRequest
	srv := newServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Aggregation: connectors.AggSum,
		GroupBy:     []string{"service"},
		Matchers:    []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	assert.Equal(t, connectors.ResultMetrics, res.Kind)
	require.Len(t, res.Series, 2)
	assert.Equal(t, "a", res.Series[0].Labels["service"])
	require.Len(t, res.Series[0].Samples, 1)
}

func TestQuery_HeaderInjection(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"streams","result":[]}}`

	t.Run("org id and bearer token", func(t *testing.T) {
		var cap capturedRequest
		srv := newServer(t, body, &cap)
		conn := mustConnector(t,
			connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL, Settings: map[string]string{SettingOrgID: "tenant-7"}},
			connectors.ConnectorCredentials{Token: "s3cr3t", Headers: map[string]string{"X-Extra": "xv"}},
		)
		now := time.Now().UTC()
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
			Signal:    connectors.SignalLogs,
			TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
			Matchers:  []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
		})
		require.NoError(t, err)
		assert.Equal(t, "tenant-7", cap.orgID)
		assert.Equal(t, "Bearer s3cr3t", cap.authz)
		assert.Equal(t, "xv", cap.extra)
	})

	t.Run("basic auth", func(t *testing.T) {
		var cap capturedRequest
		srv := newServer(t, body, &cap)
		conn := mustConnector(t,
			connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL},
			connectors.ConnectorCredentials{Username: "u", Password: "p"},
		)
		now := time.Now().UTC()
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
			Signal:    connectors.SignalLogs,
			TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
			Matchers:  []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
		})
		require.NoError(t, err)
		assert.True(t, cap.hadBasic)
		assert.Equal(t, "u", cap.user)
		assert.Equal(t, "p", cap.pass)
	})
}

func TestHealthCheck(t *testing.T) {
	t.Run("200 is healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/ready", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
		require.NoError(t, conn.HealthCheck(context.Background()))
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
		err := conn.HealthCheck(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "503")
	})
}

func TestQuery_ErrorHandling(t *testing.T) {
	t.Run("non-200 query response is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`parse error at line 1`))
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
		now := time.Now().UTC()
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
			Signal:    connectors.SignalLogs,
			TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
			Matchers:  []connectors.LabelMatcher{{Label: "app", Op: connectors.MatchEqual, Value: "api"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "400")
	})

	t.Run("malformed query is rejected before any request", func(t *testing.T) {
		conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: "http://127.0.0.1:0"}, connectors.ConnectorCredentials{})
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{})
		require.Error(t, err)
		assert.ErrorIs(t, err, connectors.ErrEmptySignal)
	})

	t.Run("missing stream selector is an error", func(t *testing.T) {
		conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: "http://127.0.0.1:0"}, connectors.ConnectorCredentials{})
		now := time.Now().UTC()
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
			Signal:    connectors.SignalLogs,
			TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stream selector is required")
	})
}

// ---- Identity, capabilities, registry --------------------------------

func TestDescribeAndCapabilities(t *testing.T) {
	conn := mustConnector(t, connectors.Config{ID: "l1", Type: TypeName, Endpoint: "http://loki:3100"}, connectors.ConnectorCredentials{})
	d := conn.Describe()
	assert.Equal(t, TypeName, d.Type)
	assert.Equal(t, "Grafana Loki", d.DisplayName)

	caps := conn.Capabilities()
	assert.True(t, caps.Supports(connectors.SignalLogs))
	assert.True(t, caps.Supports(connectors.SignalMetrics))
	assert.False(t, caps.Supports(connectors.SignalTraces))
	assert.True(t, caps.SupportsMode(connectors.ModeFederated))
	assert.False(t, caps.SupportsMode(connectors.ModeIngest))
	assert.True(t, caps.SupportsAggregation)
	assert.True(t, caps.SupportsScalar)
}

func TestNew_RejectsBadEndpoint(t *testing.T) {
	_, err := New(connectors.Config{ID: "l1", Type: TypeName, Endpoint: ""}, connectors.ConnectorCredentials{})
	require.Error(t, err)

	_, err = New(connectors.Config{ID: "l1", Type: TypeName, Endpoint: "not-a-url"}, connectors.ConnectorCredentials{})
	require.Error(t, err)
}

func TestRegisterAndInstantiate(t *testing.T) {
	reg := connectors.NewRegistry()
	require.NoError(t, Register(reg))
	assert.True(t, reg.Has(TypeName))

	conn, err := reg.New(connectors.Config{ID: "l1", Type: TypeName, Endpoint: "http://loki:3100"}, connectors.ConnectorCredentials{})
	require.NoError(t, err)
	assert.Equal(t, TypeName, conn.Describe().Type)

	// Fail-loud on duplicate registration.
	require.Error(t, Register(reg))
}
