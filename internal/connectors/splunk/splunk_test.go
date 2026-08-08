// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package splunk

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

// ---- SPL translation (pure, no I/O) ----------------------------------

func TestBuildSPL(t *testing.T) {
	now := time.Now().UTC()
	tr := connectors.TimeRange{Start: now.Add(-time.Hour), End: now}
	trStep := connectors.TimeRange{Start: now.Add(-time.Hour), End: now, Step: time.Minute}

	tests := []struct {
		name  string
		q     connectors.NormalizedQuery
		index string
		want  string
	}{
		{
			name: "logs single equality matcher",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				TimeRange: tr,
				Matchers:  []connectors.LabelMatcher{{Label: "service", Op: connectors.MatchEqual, Value: "checkout"}},
			},
			want: `search service="checkout"`,
		},
		{
			name: "logs all four matcher operators map to SPL (regex become | regex)",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				Selector:  "error",
				TimeRange: tr,
				Matchers: []connectors.LabelMatcher{
					{Label: "service", Op: connectors.MatchEqual, Value: "checkout"},
					{Label: "env", Op: connectors.MatchNotEqual, Value: "prod"},
					{Label: "path", Op: connectors.MatchRegex, Value: "/api.*"},
					{Label: "region", Op: connectors.MatchNotRegex, Value: "eu.*"},
				},
			},
			index: "main",
			want:  `search index="main" error service="checkout" env!="prod" | regex path="/api.*" | regex region!="eu.*"`,
		},
		{
			name: "logs no terms defaults to wildcard",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				TimeRange: tr,
			},
			want: `search *`,
		},
		{
			name: "raw SPL escape hatch passes through",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				TimeRange: tr,
				Raw:       `search index=main sourcetype=access_combined | stats count by status`,
			},
			want: `search index=main sourcetype=access_combined | stats count by status`,
		},
		{
			name: "raw SPL without leading command gets search prefix",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalLogs,
				TimeRange: tr,
				Raw:       `sourcetype=access_combined status=500`,
			},
			want: `search sourcetype=access_combined status=500`,
		},
		{
			name: "metrics count defaults to stats count",
			q: connectors.NormalizedQuery{
				Signal:    connectors.SignalMetrics,
				TimeRange: tr,
				Matchers:  []connectors.LabelMatcher{{Label: "sourcetype", Op: connectors.MatchEqual, Value: "access"}},
			},
			want: `search sourcetype="access" | stats count`,
		},
		{
			name: "metrics avg over selector field with group-by",
			q: connectors.NormalizedQuery{
				Signal:      connectors.SignalMetrics,
				Selector:    "latency",
				TimeRange:   tr,
				Aggregation: connectors.AggAvg,
				GroupBy:     []string{"service", "region"},
				Matchers:    []connectors.LabelMatcher{{Label: "index", Op: connectors.MatchEqual, Value: "app"}},
			},
			want: `search index="app" | stats avg(latency) by service, region`,
		},
		{
			name: "stepped metric query becomes timechart",
			q: connectors.NormalizedQuery{
				Signal:      connectors.SignalMetrics,
				Selector:    "bytes",
				TimeRange:   trStep,
				Aggregation: connectors.AggSum,
				GroupBy:     []string{"host"},
			},
			want: `search * | timechart span=60s sum(bytes) by host`,
		},
		{
			name: "rate aggregation maps to per_second",
			q: connectors.NormalizedQuery{
				Signal:      connectors.SignalMetrics,
				Selector:    "count",
				TimeRange:   tr,
				Aggregation: connectors.AggRate,
			},
			want: `search * | stats per_second(count)`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildSPL(tc.q, tc.index)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildSPL_Errors(t *testing.T) {
	now := time.Now().UTC()
	tr := connectors.TimeRange{Start: now.Add(-time.Hour), End: now}

	t.Run("avg without a metric field is an error", func(t *testing.T) {
		_, err := buildSPL(connectors.NormalizedQuery{
			Signal:      connectors.SignalMetrics,
			TimeRange:   tr,
			Aggregation: connectors.AggAvg,
		}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires a metric field")
	})
}

// ---- End-to-end via httptest -----------------------------------------

// capturedRequest records what the connector sent so tests can assert on the
// path, form parameters, and headers.
type capturedRequest struct {
	path       string
	method     string
	search     string
	execMode   string
	outputMode string
	earliest   string
	latest     string
	count      string
	authz      string
	extra      string
	hadBasic   bool
	user       string
	pass       string
}

// newSearchServer starts an httptest server that serves respBody for the
// search endpoint and records the request into cap.
func newSearchServer(t *testing.T, respBody string, cap *capturedRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.method = r.Method
		_ = r.ParseForm()
		cap.search = r.FormValue("search")
		cap.execMode = r.FormValue("exec_mode")
		cap.outputMode = r.FormValue("output_mode")
		cap.earliest = r.FormValue("earliest_time")
		cap.latest = r.FormValue("latest_time")
		cap.count = r.FormValue("count")
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

func TestQuery_EventsToLogRows(t *testing.T) {
	body := `{"preview":false,"messages":[],"results":[
		{"_time":"2023-11-14T22:13:20.000+00:00","_raw":"boom happened","host":"h1","source":"/var/log/app"},
		{"_time":"2023-11-14T22:13:21.000+00:00","_raw":"still broken","host":"h1","source":"/var/log/app"}
	]}`
	var cap capturedRequest
	srv := newSearchServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL, Settings: map[string]string{SettingIndex: "main"}}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:    connectors.SignalLogs,
		Selector:  "boom",
		TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Matchers:  []connectors.LabelMatcher{{Label: "sourcetype", Op: connectors.MatchEqual, Value: "app"}},
		Limit:     2,
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	// Oneshot POST shape.
	assert.Equal(t, http.MethodPost, cap.method)
	assert.Equal(t, "/services/search/jobs", cap.path)
	assert.Equal(t, "oneshot", cap.execMode)
	assert.Equal(t, "json", cap.outputMode)
	assert.Equal(t, `search index="main" boom sourcetype="app"`, cap.search)
	assert.Equal(t, "2", cap.count)
	assert.NotEmpty(t, cap.earliest)
	assert.NotEmpty(t, cap.latest)

	assert.Equal(t, connectors.ResultLogs, res.Kind)
	require.Len(t, res.Logs, 2)
	assert.Equal(t, "boom happened", res.Logs[0].Line)
	assert.Equal(t, "h1", res.Logs[0].Labels["host"])
	assert.Equal(t, "/var/log/app", res.Logs[0].Labels["source"])
	// _raw and _time are not leaked into labels.
	_, hasRaw := res.Logs[0].Labels["_raw"]
	assert.False(t, hasRaw)
	assert.Equal(t, 2023, res.Logs[0].Timestamp.Year())
	// Row count equals Limit => truncation warning.
	assert.Contains(t, res.Warnings, "results truncated at limit")
}

func TestQuery_StatsSingleValueToScalar(t *testing.T) {
	body := `{"preview":false,"messages":[],"results":[{"count":"42"}]}`
	var cap capturedRequest
	srv := newSearchServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Aggregation: connectors.AggCount,
		Matchers:    []connectors.LabelMatcher{{Label: "sourcetype", Op: connectors.MatchEqual, Value: "access"}},
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	assert.Equal(t, `search sourcetype="access" | stats count`, cap.search)
	assert.Equal(t, connectors.ResultScalar, res.Kind)
	require.NotNil(t, res.Scalar)
	assert.Equal(t, 42.0, res.Scalar.Value)
	assert.Equal(t, connectors.ScalarGeneric, res.Scalar.Kind)
	assert.Equal(t, now.Unix(), res.Scalar.EvaluatedAt.Unix())
}

func TestQuery_StatsGroupByToSeries(t *testing.T) {
	body := `{"preview":false,"messages":[],"results":[
		{"service":"a","avg(latency)":"1.5"},
		{"service":"b","avg(latency)":"2.5"}
	]}`
	var cap capturedRequest
	srv := newSearchServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		Selector:    "latency",
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Aggregation: connectors.AggAvg,
		GroupBy:     []string{"service"},
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	assert.Equal(t, `search * | stats avg(latency) by service`, cap.search)
	assert.Equal(t, connectors.ResultMetrics, res.Kind)
	require.Len(t, res.Series, 2)
	// Series carry the group-by label and the metric column name.
	assert.Equal(t, "a", res.Series[0].Labels["service"])
	assert.Equal(t, "avg(latency)", res.Series[0].Labels["metric"])
	require.Len(t, res.Series[0].Samples, 1)
	assert.Equal(t, 1.5, res.Series[0].Samples[0].Value)
	assert.Equal(t, 2.5, res.Series[1].Samples[0].Value)
}

func TestQuery_TimechartToSeries(t *testing.T) {
	body := `{"preview":false,"messages":[],"results":[
		{"_time":"2023-11-14T22:13:20.000+00:00","count":"5"},
		{"_time":"2023-11-14T22:14:20.000+00:00","count":"7"}
	]}`
	var cap capturedRequest
	srv := newSearchServer(t, body, &cap)

	conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
	now := time.Now().UTC()
	res, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:      connectors.SignalMetrics,
		TimeRange:   connectors.TimeRange{Start: now.Add(-time.Hour), End: now, Step: time.Minute},
		Aggregation: connectors.AggCount,
	})
	require.NoError(t, err)
	require.NoError(t, res.Validate())

	assert.Equal(t, `search * | timechart span=60s count`, cap.search)
	assert.Equal(t, connectors.ResultMetrics, res.Kind)
	require.Len(t, res.Series, 1)
	assert.Equal(t, "count", res.Series[0].Labels["metric"])
	require.Len(t, res.Series[0].Samples, 2)
	assert.Equal(t, 5.0, res.Series[0].Samples[0].Value)
	assert.Equal(t, 7.0, res.Series[0].Samples[1].Value)
	assert.Equal(t, 2023, res.Series[0].Samples[0].Timestamp.Year())
}

func TestQuery_AuthInjection(t *testing.T) {
	body := `{"preview":false,"messages":[],"results":[]}`

	t.Run("bearer token", func(t *testing.T) {
		var cap capturedRequest
		srv := newSearchServer(t, body, &cap)
		conn := mustConnector(t,
			connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL},
			connectors.ConnectorCredentials{Token: "s3cr3t", Headers: map[string]string{"X-Extra": "xv"}},
		)
		runOneLogQuery(t, conn)
		assert.Equal(t, "Bearer s3cr3t", cap.authz)
		assert.Equal(t, "xv", cap.extra)
	})

	t.Run("session key via auth_mode", func(t *testing.T) {
		var cap capturedRequest
		srv := newSearchServer(t, body, &cap)
		conn := mustConnector(t,
			connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL, Settings: map[string]string{SettingAuthMode: AuthSession}},
			connectors.ConnectorCredentials{Token: "sessionkey"},
		)
		runOneLogQuery(t, conn)
		assert.Equal(t, "Splunk sessionkey", cap.authz)
	})

	t.Run("basic auth", func(t *testing.T) {
		var cap capturedRequest
		srv := newSearchServer(t, body, &cap)
		conn := mustConnector(t,
			connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL},
			connectors.ConnectorCredentials{Username: "admin", Password: "changeme"},
		)
		runOneLogQuery(t, conn)
		assert.True(t, cap.hadBasic)
		assert.Equal(t, "admin", cap.user)
		assert.Equal(t, "changeme", cap.pass)
	})
}

func runOneLogQuery(t *testing.T, conn connectors.Connector) {
	t.Helper()
	now := time.Now().UTC()
	_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
		Signal:    connectors.SignalLogs,
		TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		Matchers:  []connectors.LabelMatcher{{Label: "sourcetype", Op: connectors.MatchEqual, Value: "app"}},
	})
	require.NoError(t, err)
}

func TestHealthCheck(t *testing.T) {
	t.Run("200 is healthy and hits server/info", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/services/server/info", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"generator":{}}`))
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
		require.NoError(t, conn.HealthCheck(context.Background()))
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
		err := conn.HealthCheck(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
	})

	t.Run("app namespace scopes the path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/servicesNS/-/search/server/info", r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL, Settings: map[string]string{SettingApp: "search"}}, connectors.ConnectorCredentials{})
		require.NoError(t, conn.HealthCheck(context.Background()))
	})
}

func TestQuery_ErrorHandling(t *testing.T) {
	t.Run("non-200 query response is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"messages":[{"type":"FATAL","text":"invalid search"}]}`))
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
		now := time.Now().UTC()
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
			Signal:    connectors.SignalLogs,
			TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
			Matchers:  []connectors.LabelMatcher{{Label: "sourcetype", Op: connectors.MatchEqual, Value: "app"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "400")
	})

	t.Run("200 with FATAL message is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"messages":[{"type":"FATAL","text":"Error in 'search' command"}],"results":[]}`))
		}))
		defer srv.Close()
		conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: srv.URL}, connectors.ConnectorCredentials{})
		now := time.Now().UTC()
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
			Signal:    connectors.SignalLogs,
			TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
			Matchers:  []connectors.LabelMatcher{{Label: "sourcetype", Op: connectors.MatchEqual, Value: "app"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "search error")
	})

	t.Run("malformed query is rejected before any request", func(t *testing.T) {
		conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: "http://127.0.0.1:0"}, connectors.ConnectorCredentials{})
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{})
		require.Error(t, err)
		assert.ErrorIs(t, err, connectors.ErrEmptySignal)
	})

	t.Run("traces signal is unsupported", func(t *testing.T) {
		conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: "http://127.0.0.1:0"}, connectors.ConnectorCredentials{})
		now := time.Now().UTC()
		_, err := conn.Query(context.Background(), connectors.NormalizedQuery{
			Signal:    connectors.SignalTraces,
			TimeRange: connectors.TimeRange{Start: now.Add(-time.Hour), End: now},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported")
	})
}

// ---- Identity, capabilities, registry --------------------------------

func TestDescribeAndCapabilities(t *testing.T) {
	conn := mustConnector(t, connectors.Config{ID: "s1", Type: TypeName, Endpoint: "https://splunk:8089"}, connectors.ConnectorCredentials{})
	d := conn.Describe()
	assert.Equal(t, TypeName, d.Type)
	assert.Equal(t, "Splunk", d.DisplayName)

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
	_, err := New(connectors.Config{ID: "s1", Type: TypeName, Endpoint: ""}, connectors.ConnectorCredentials{})
	require.Error(t, err)

	_, err = New(connectors.Config{ID: "s1", Type: TypeName, Endpoint: "not-a-url"}, connectors.ConnectorCredentials{})
	require.Error(t, err)
}

func TestNew_InsecureSkipVerifyBuildsCustomTransport(t *testing.T) {
	c, err := New(connectors.Config{
		ID:       "s1",
		Type:     TypeName,
		Endpoint: "https://splunk:8089",
		Settings: map[string]string{SettingInsecureSkipVerify: "true"},
	}, connectors.ConnectorCredentials{})
	require.NoError(t, err)
	sc, ok := c.(*splunkConnector)
	require.True(t, ok)
	tr, ok := sc.client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.TLSClientConfig)
	assert.True(t, tr.TLSClientConfig.InsecureSkipVerify)
}

func TestRegisterAndInstantiate(t *testing.T) {
	reg := connectors.NewRegistry()
	require.NoError(t, Register(reg))
	assert.True(t, reg.Has(TypeName))

	conn, err := reg.New(connectors.Config{ID: "s1", Type: TypeName, Endpoint: "https://splunk:8089"}, connectors.ConnectorCredentials{})
	require.NoError(t, err)
	assert.Equal(t, TypeName, conn.Describe().Type)

	// Fail-loud on duplicate registration.
	require.Error(t, Register(reg))
}
