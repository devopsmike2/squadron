// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package loki is the first real telemetry query-connector (ADR 0034
// slice 2): a FEDERATED (live-query) connector for Grafana Loki.
//
// It implements the slice-1 connectors.Connector interface. Query
// translates a connectors.NormalizedQuery into LogQL and executes it
// against Loki's query API, then normalizes the JSON response back into a
// connectors.NormalizedResult:
//
//   - a logs query becomes a LogQL stream pipeline and returns `streams`
//     which normalize to connectors.LogRow values;
//   - a metric query (Signal metrics, or any pushed-down Aggregation)
//     becomes a LogQL metric query (rate/count_over_time wrapped in an
//     optional vector aggregation) and returns `matrix` (stepped) or
//     `vector` (instant) which normalize to connectors.MetricSeries;
//   - a single-element `vector` normalizes to a connectors.ScalarResult
//     so the SLO / burn-rate decision-context path (a later slice) can
//     read a Loki-derived scalar without a model change.
//
// Federated mode only. Scheduled ingest (ModeIngest) is ADR 0034 slice 4
// and is deliberately not implemented here.
//
// The connector is OSS-inert until wired: registering it is opt-in via
// Register(reg). No production code calls Register, so — like the rest of
// the S1 framework — it cannot be selected in a running deployment yet;
// it is instantiable by type name for callers that choose to wire it and
// exercised end-to-end by this package's tests.
package loki

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// TypeName is the registry key for the Loki connector. It matches
// connectors.Config.Type and connectors.Descriptor.Type.
const TypeName = "loki"

// Settings keys interpreted by New from connectors.Config.Settings. All
// are non-secret (secrets live in connectors.ConnectorCredentials): the
// org id is a routing header, and the auth mode / timeout are knobs.
const (
	// SettingOrgID sets the X-Scope-OrgID multi-tenant header value. When
	// empty, no tenant header is sent (single-tenant Loki).
	SettingOrgID = "org_id"

	// SettingAuthMode selects how credentials authenticate: "basic",
	// "bearer", or "none". When empty, the mode is inferred from the
	// resolved credentials (a Token implies bearer, a Username implies
	// basic, neither implies none).
	SettingAuthMode = "auth_mode"

	// SettingTimeoutSeconds overrides the per-request HTTP timeout. When
	// empty or unparsable, defaultTimeout is used.
	SettingTimeoutSeconds = "timeout_seconds"
)

// Auth modes for SettingAuthMode.
const (
	AuthNone   = "none"
	AuthBasic  = "basic"
	AuthBearer = "bearer"
)

// defaultTimeout is the per-request HTTP timeout when SettingTimeoutSeconds
// is unset.
const defaultTimeout = 30 * time.Second

// defaultMetricRange is the LogQL range-vector window used for metric
// queries when the query carries neither a Step nor a usable TimeRange
// duration to derive one from.
const defaultMetricRange = 5 * time.Minute

// lokiConnector is a configured, concurrency-safe federated Loki
// connector. A single instance serves many queries.
type lokiConnector struct {
	cfg     connectors.Config
	creds   connectors.ConnectorCredentials
	baseURL string
	orgID   string
	auth    string
	client  *http.Client
}

// compile-time assertion that the connector satisfies the interface.
var _ connectors.Connector = (*lokiConnector)(nil)

// New is the connectors.Factory for the Loki connector. It builds a
// connector from a stored Config and the resolved (already-decrypted)
// credentials. It does not perform I/O — use HealthCheck to validate the
// endpoint and credentials.
func New(cfg connectors.Config, creds connectors.ConnectorCredentials) (connectors.Connector, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("loki: endpoint is required")
	}
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("loki: endpoint %q is not a valid absolute URL", cfg.Endpoint)
	}

	timeout := defaultTimeout
	if v := cfg.Settings[SettingTimeoutSeconds]; v != "" {
		if secs, perr := strconv.Atoi(v); perr == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	auth := cfg.Settings[SettingAuthMode]
	if auth == "" {
		// Infer from the resolved credentials.
		switch {
		case creds.Token != "":
			auth = AuthBearer
		case creds.Username != "":
			auth = AuthBasic
		default:
			auth = AuthNone
		}
	}

	return &lokiConnector{
		cfg:     cfg,
		creds:   creds,
		baseURL: strings.TrimRight(cfg.Endpoint, "/"),
		orgID:   cfg.Settings[SettingOrgID],
		auth:    auth,
		// Nil Transport => http.DefaultTransport, which honors HTTPS_PROXY
		// / HTTP_PROXY / NO_PROXY via ProxyFromEnvironment. The context on
		// each request carries cancellation and the caller's deadline; the
		// client Timeout is a backstop.
		client: &http.Client{Timeout: timeout},
	}, nil
}

// Register registers the Loki connector's Factory under TypeName in reg.
// It is the opt-in wiring point: nothing in production code calls it, so
// the connector stays OSS-inert until a caller chooses to register it.
func Register(reg *connectors.Registry) error {
	return reg.Register(TypeName, New)
}

// Describe returns the connector's static identity.
func (c *lokiConnector) Describe() connectors.Descriptor {
	return connectors.Descriptor{
		Type:        TypeName,
		DisplayName: "Grafana Loki",
		Description: "Federated live-query connector for Grafana Loki (LogQL over the query API).",
	}
}

// Capabilities reports what this connector can serve: logs natively, and
// metrics via LogQL metric queries; federated mode only; aggregation
// push-down and a scalar (SLO / burn-rate) result are both supported.
func (c *lokiConnector) Capabilities() connectors.Capabilities {
	return connectors.Capabilities{
		Signals:             []connectors.Signal{connectors.SignalLogs, connectors.SignalMetrics},
		Modes:               []connectors.Mode{connectors.ModeFederated},
		SupportsAggregation: true,
		SupportsScalar:      true,
	}
}

// setHeaders attaches the tenant and auth headers to every request. It
// never logs or returns secret material.
func (c *lokiConnector) setHeaders(req *http.Request) {
	if c.orgID != "" {
		req.Header.Set("X-Scope-OrgID", c.orgID)
	}
	switch c.auth {
	case AuthBearer:
		if c.creds.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.creds.Token)
		}
	case AuthBasic:
		if c.creds.Username != "" || c.creds.Password != "" {
			req.SetBasicAuth(c.creds.Username, c.creds.Password)
		}
	}
	// Extra secret headers some deployments require (sealed in creds).
	for k, v := range c.creds.Headers {
		req.Header.Set(k, v)
	}
}

// HealthCheck validates the endpoint and credentials without running a
// data query by hitting Loki's /ready endpoint. It returns nil on a 200,
// or a descriptive (secret-free) error otherwise.
func (c *lokiConnector) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ready", nil)
	if err != nil {
		return fmt.Errorf("loki: build health request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("loki: health request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a little of the body so the connection can be reused; ignore
	// content (may be "ready\n" or a not-ready reason).
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<10)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("loki: health check to %s/ready returned status %d", c.baseURL, resp.StatusCode)
	}
	return nil
}

// Query translates q into LogQL, executes it against Loki, and normalizes
// the response. Log queries and stepped metric queries use
// /loki/api/v1/query_range; instant metric queries (Step == 0) use
// /loki/api/v1/query so a single-value result comes back as a `vector`
// that normalizes to a ScalarResult. Query never mutates the backend.
func (c *lokiConnector) Query(ctx context.Context, q connectors.NormalizedQuery) (connectors.NormalizedResult, error) {
	if err := q.Validate(); err != nil {
		return connectors.NormalizedResult{}, err
	}
	if q.Signal == connectors.SignalTraces {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: signal %q is not supported", q.Signal)
	}

	logql, err := buildLogQL(q)
	if err != nil {
		return connectors.NormalizedResult{}, err
	}

	isMetric := q.Signal == connectors.SignalMetrics || q.Aggregation != connectors.AggNone
	instant := isMetric && q.TimeRange.Step == 0

	endpoint := "/loki/api/v1/query_range"
	if instant {
		endpoint = "/loki/api/v1/query"
	}

	params := url.Values{}
	params.Set("query", logql)
	if instant {
		params.Set("time", strconv.FormatInt(q.TimeRange.End.UnixNano(), 10))
	} else {
		params.Set("start", strconv.FormatInt(q.TimeRange.Start.UnixNano(), 10))
		params.Set("end", strconv.FormatInt(q.TimeRange.End.UnixNano(), 10))
		if isMetric && q.TimeRange.Step > 0 {
			params.Set("step", formatLokiRange(q.TimeRange.Step))
		}
		if !isMetric {
			// Log query: newest-first, honor the row cap.
			params.Set("direction", "backward")
			if q.Limit > 0 {
				params.Set("limit", strconv.Itoa(q.Limit))
			}
		}
	}

	reqURL := c.baseURL + endpoint + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: build query request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: query request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: query returned status %d: %s", resp.StatusCode, snippet(body))
	}

	return parseResponse(body, q)
}

// buildLogQL translates a validated NormalizedQuery into a LogQL query
// string. The stream selector {..} is built from Matchers; a non-empty
// Selector is treated as either a raw {..} stream selector (when it starts
// with '{') or a line filter otherwise. Metric queries wrap the stream
// pipeline in a range-vector function and an optional vector aggregation.
func buildLogQL(q connectors.NormalizedQuery) (string, error) {
	pipeline, err := buildPipeline(q)
	if err != nil {
		return "", err
	}

	isMetric := q.Signal == connectors.SignalMetrics || q.Aggregation != connectors.AggNone
	if !isMetric {
		return pipeline, nil
	}

	rng := metricRange(q.TimeRange)
	var inner string
	if q.Aggregation == connectors.AggRate {
		inner = fmt.Sprintf("rate(%s [%s])", pipeline, rng)
	} else {
		inner = fmt.Sprintf("count_over_time(%s [%s])", pipeline, rng)
	}

	if vf, ok := vectorAggFunc(q.Aggregation); ok {
		by := ""
		if len(q.GroupBy) > 0 {
			by = fmt.Sprintf(" by (%s)", strings.Join(q.GroupBy, ", "))
		}
		return fmt.Sprintf("%s%s(%s)", vf, by, inner), nil
	}
	// AggNone or AggRate: sum a rate across a group when requested,
	// otherwise return the bare range-vector metric.
	if q.Aggregation == connectors.AggRate && len(q.GroupBy) > 0 {
		return fmt.Sprintf("sum by (%s) (%s)", strings.Join(q.GroupBy, ", "), inner), nil
	}
	return inner, nil
}

// buildPipeline builds the "{selector} |= filter" log stream pipeline from
// the query's Matchers and Selector.
func buildPipeline(q connectors.NormalizedQuery) (string, error) {
	sel := strings.TrimSpace(q.Selector)

	var streamSelector string
	var lineFilter string

	switch {
	case len(q.Matchers) > 0:
		matchers := make([]string, 0, len(q.Matchers))
		for _, m := range q.Matchers {
			matchers = append(matchers, fmt.Sprintf("%s%s%q", m.Label, string(m.Op), m.Value))
		}
		streamSelector = "{" + strings.Join(matchers, ", ") + "}"
		if sel != "" {
			if strings.HasPrefix(sel, "{") {
				return "", fmt.Errorf("loki: Selector is a raw {..} stream selector but Matchers are also set; provide one or the other")
			}
			lineFilter = fmt.Sprintf(" |= %q", sel)
		}
	case sel != "" && strings.HasPrefix(sel, "{"):
		// Advanced passthrough: Selector already is a stream selector.
		streamSelector = sel
	default:
		return "", fmt.Errorf("loki: a stream selector is required (provide Matchers or a {..} Selector)")
	}

	return streamSelector + lineFilter, nil
}

// vectorAggFunc maps a NormalizedQuery aggregation to a LogQL vector
// aggregation operator. AggNone and AggRate return ok=false (they are
// handled by buildLogQL directly).
func vectorAggFunc(a connectors.Aggregation) (string, bool) {
	switch a {
	case connectors.AggSum:
		return "sum", true
	case connectors.AggAvg:
		return "avg", true
	case connectors.AggMin:
		return "min", true
	case connectors.AggMax:
		return "max", true
	case connectors.AggCount:
		return "count", true
	default:
		return "", false
	}
}

// metricRange picks the LogQL range-vector window for a metric query:
// the Step when set, else the whole window duration, else the default.
func metricRange(tr connectors.TimeRange) string {
	switch {
	case tr.Step > 0:
		return formatLokiRange(tr.Step)
	case tr.Duration() > 0:
		return formatLokiRange(tr.Duration())
	default:
		return formatLokiRange(defaultMetricRange)
	}
}

// formatLokiRange renders a duration as a whole-second LogQL range string
// (e.g. "300s"). Seconds are unambiguous and always parseable by LogQL;
// sub-second windows are rounded up to 1s.
func formatLokiRange(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10) + "s"
}

// snippet returns a short, safe prefix of an error body for diagnostics.
func snippet(b []byte) string {
	const max = 256
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
