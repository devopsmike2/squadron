// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package splunk is a FEDERATED (live-query) telemetry query-connector for
// Splunk (ADR 0034 slice 3), mirroring the slice-2 Loki connector's shape.
//
// It implements the slice-1 connectors.Connector interface. Query
// translates a connectors.NormalizedQuery into SPL and runs a synchronous
// oneshot search against Splunk's REST search API
// (POST /services/search/jobs with exec_mode=oneshot, output_mode=json),
// then normalizes the JSON `results` back into a connectors.NormalizedResult:
//
//   - a logs query becomes an SPL `search` and returns raw events which
//     normalize to connectors.LogRow values (_raw -> Line, _time ->
//     Timestamp, remaining fields -> Labels);
//   - a metric query (Signal metrics, or any pushed-down Aggregation)
//     becomes an SPL `stats`/`timechart` and returns rows which normalize to
//     connectors.MetricSeries;
//   - a single scalar stat (one row, one numeric column, no group-by)
//     normalizes to a connectors.ScalarResult so the SLO / burn-rate
//     decision-context path (a later slice) can read a Splunk-derived scalar
//     without a model change.
//
// Federated mode only. Scheduled ingest (ModeIngest) is ADR 0034 slice 4 and
// is deliberately not implemented here.
//
// The connector is OSS-inert until wired: registering it is opt-in via
// Register(reg). No production code calls Register, so — like the rest of the
// S1 framework — it cannot be selected in a running deployment yet; it is
// instantiable by type name for callers that choose to wire it and exercised
// end-to-end by this package's tests.
package splunk

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// TypeName is the registry key for the Splunk connector. It matches
// connectors.Config.Type and connectors.Descriptor.Type.
const TypeName = "splunk"

// Settings keys interpreted by New from connectors.Config.Settings. All are
// non-secret (secrets live in connectors.ConnectorCredentials).
const (
	// SettingIndex sets a default Splunk index to constrain searches to
	// (rendered as `index=<value>` in the SPL). Empty means "no index
	// constraint beyond the query itself".
	SettingIndex = "index"

	// SettingApp scopes REST calls to a Splunk app namespace via the
	// /servicesNS/-/<app>/... path. Empty uses the default /services/...
	// namespace.
	SettingApp = "app"

	// SettingAuthMode selects how credentials authenticate: "bearer"
	// (Authorization: Bearer <token>), "session" (Authorization: Splunk
	// <key>), "basic", or "none". When empty, the mode is inferred from the
	// resolved credentials (a Token implies bearer, a Username implies
	// basic, neither implies none).
	SettingAuthMode = "auth_mode"

	// SettingTimeoutSeconds overrides the per-request HTTP timeout. When
	// empty or unparsable, defaultTimeout is used.
	SettingTimeoutSeconds = "timeout_seconds"

	// SettingInsecureSkipVerify, when "true", disables TLS certificate
	// verification. Splunk's management port (:8089) commonly presents a
	// self-signed certificate; this toggle lets an operator opt into
	// talking to it. It defaults to false and weakens the connection's
	// trust posture — set it only for a deployment whose cert cannot be
	// verified by the system trust store.
	SettingInsecureSkipVerify = "insecure_skip_verify"
)

// Auth modes for SettingAuthMode.
const (
	AuthNone    = "none"
	AuthBasic   = "basic"
	AuthBearer  = "bearer"
	AuthSession = "session"
)

// defaultTimeout is the per-request HTTP timeout when SettingTimeoutSeconds
// is unset. Splunk oneshot searches run synchronously, so allow a generous
// window.
const defaultTimeout = 60 * time.Second

// splunkConnector is a configured, concurrency-safe federated Splunk
// connector. A single instance serves many queries.
type splunkConnector struct {
	cfg      connectors.Config
	creds    connectors.ConnectorCredentials
	baseURL  string
	pathBase string // "/services" or "/servicesNS/-/<app>"
	index    string
	auth     string
	client   *http.Client
}

// compile-time assertion that the connector satisfies the interface.
var _ connectors.Connector = (*splunkConnector)(nil)

// New is the connectors.Factory for the Splunk connector. It builds a
// connector from a stored Config and the resolved (already-decrypted)
// credentials. It does not perform I/O — use HealthCheck to validate the
// endpoint and credentials.
func New(cfg connectors.Config, creds connectors.ConnectorCredentials) (connectors.Connector, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("splunk: endpoint is required")
	}
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("splunk: endpoint %q is not a valid absolute URL", cfg.Endpoint)
	}

	timeout := defaultTimeout
	if v := cfg.Settings[SettingTimeoutSeconds]; v != "" {
		if secs, perr := strconv.Atoi(v); perr == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	auth := cfg.Settings[SettingAuthMode]
	if auth == "" {
		// Infer from the resolved credentials. A Token defaults to bearer;
		// an operator selects "session" explicitly when the token is a
		// Splunk session key rather than an auth token.
		switch {
		case creds.Token != "":
			auth = AuthBearer
		case creds.Username != "":
			auth = AuthBasic
		default:
			auth = AuthNone
		}
	}

	pathBase := "/services"
	if app := strings.TrimSpace(cfg.Settings[SettingApp]); app != "" {
		pathBase = "/servicesNS/-/" + url.PathEscape(app)
	}

	insecure := cfg.Settings[SettingInsecureSkipVerify] == "true"

	return &splunkConnector{
		cfg:      cfg,
		creds:    creds,
		baseURL:  strings.TrimRight(cfg.Endpoint, "/"),
		pathBase: pathBase,
		index:    strings.TrimSpace(cfg.Settings[SettingIndex]),
		auth:     auth,
		client:   newHTTPClient(timeout, insecure),
	}, nil
}

// newHTTPClient builds the connector's HTTP client. When insecure is false it
// leaves Transport nil so http.DefaultTransport is used, which honors
// HTTPS_PROXY / HTTP_PROXY / NO_PROXY via ProxyFromEnvironment. When insecure
// is true it clones the default transport (preserving proxy support) and only
// flips InsecureSkipVerify, so nothing else about the default transport
// changes. The context on each request carries cancellation and the caller's
// deadline; the client Timeout is a backstop.
func newHTTPClient(timeout time.Duration, insecure bool) *http.Client {
	if !insecure {
		return &http.Client{Timeout: timeout}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.InsecureSkipVerify = true
	return &http.Client{Timeout: timeout, Transport: tr}
}

// Register registers the Splunk connector's Factory under TypeName in reg.
// It is the opt-in wiring point: nothing in production code calls it, so the
// connector stays OSS-inert until a caller chooses to register it.
func Register(reg *connectors.Registry) error {
	return reg.Register(TypeName, New)
}

// Describe returns the connector's static identity.
func (c *splunkConnector) Describe() connectors.Descriptor {
	return connectors.Descriptor{
		Type:        TypeName,
		DisplayName: "Splunk",
		Description: "Federated live-query connector for Splunk (SPL over the REST search API).",
	}
}

// Capabilities reports what this connector can serve: logs natively, and
// metrics via SPL stats/timechart; federated mode only; aggregation push-down
// and a scalar (SLO / burn-rate) result are both supported.
func (c *splunkConnector) Capabilities() connectors.Capabilities {
	return connectors.Capabilities{
		Signals:             []connectors.Signal{connectors.SignalLogs, connectors.SignalMetrics},
		Modes:               []connectors.Mode{connectors.ModeFederated},
		SupportsAggregation: true,
		SupportsScalar:      true,
	}
}

// setHeaders attaches the auth header to every request. It never logs or
// returns secret material.
func (c *splunkConnector) setHeaders(req *http.Request) {
	switch c.auth {
	case AuthBearer:
		if c.creds.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.creds.Token)
		}
	case AuthSession:
		if c.creds.Token != "" {
			req.Header.Set("Authorization", "Splunk "+c.creds.Token)
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

// HealthCheck validates the endpoint and credentials without running a data
// query by hitting Splunk's server-info endpoint. It returns nil on a 200, or
// a descriptive (secret-free) error otherwise.
func (c *splunkConnector) HealthCheck(ctx context.Context) error {
	reqURL := c.baseURL + c.pathBase + "/server/info?output_mode=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("splunk: build health request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("splunk: health request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a little of the body so the connection can be reused.
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<10)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("splunk: health check to %s%s/server/info returned status %d", c.baseURL, c.pathBase, resp.StatusCode)
	}
	return nil
}

// Query translates q into SPL, runs a synchronous oneshot search against
// Splunk, and normalizes the response. Query never mutates the backend.
func (c *splunkConnector) Query(ctx context.Context, q connectors.NormalizedQuery) (connectors.NormalizedResult, error) {
	if err := q.Validate(); err != nil {
		return connectors.NormalizedResult{}, err
	}
	if q.Signal == connectors.SignalTraces {
		return connectors.NormalizedResult{}, fmt.Errorf("splunk: signal %q is not supported", q.Signal)
	}

	spl, err := buildSPL(q, c.index)
	if err != nil {
		return connectors.NormalizedResult{}, err
	}

	form := url.Values{}
	form.Set("search", spl)
	form.Set("exec_mode", "oneshot")
	form.Set("output_mode", "json")
	// Splunk accepts epoch seconds for the search time bounds; they are
	// unambiguous and always parseable.
	form.Set("earliest_time", strconv.FormatInt(q.TimeRange.Start.Unix(), 10))
	form.Set("latest_time", strconv.FormatInt(q.TimeRange.End.Unix(), 10))
	if q.Limit > 0 {
		// count caps the number of rows the oneshot search returns.
		form.Set("count", strconv.Itoa(q.Limit))
	}

	reqURL := c.baseURL + c.pathBase + "/search/jobs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("splunk: build query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("splunk: query request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("splunk: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return connectors.NormalizedResult{}, fmt.Errorf("splunk: query returned status %d: %s", resp.StatusCode, snippet(body))
	}

	return parseResponse(body, q)
}

// isMetricQuery reports whether q should be translated to an SPL
// stats/timechart (a metric query) rather than a raw event search.
func isMetricQuery(q connectors.NormalizedQuery) bool {
	return q.Signal == connectors.SignalMetrics || q.Aggregation != connectors.AggNone
}

// buildSPL translates a validated NormalizedQuery into an SPL search string.
//
// When Raw is set it is the escape hatch: the caller's full SPL is used
// verbatim (with a leading `search`/`|` command ensured). Otherwise the base
// `search` is assembled from the optional index, the Selector, and the
// equality/inequality Matchers; regex matchers (=~ / !~) become `| regex`
// pipeline commands; and a metric query appends a `stats` (instant) or
// `timechart` (stepped) aggregation.
func buildSPL(q connectors.NormalizedQuery, index string) (string, error) {
	if raw := strings.TrimSpace(q.Raw); raw != "" {
		return ensureLeadingCommand(raw), nil
	}

	var terms []string
	if index != "" {
		terms = append(terms, "index="+quoteSPL(index))
	}
	// For a log query the Selector is a search term; for a metric query it
	// names the metric field consumed by the stats/timechart aggregation, so
	// it must not also be added to the base search.
	if sel := strings.TrimSpace(q.Selector); sel != "" && !isMetricQuery(q) {
		terms = append(terms, sel)
	}

	var regexCmds []string
	for _, m := range q.Matchers {
		switch m.Op {
		case connectors.MatchEqual:
			terms = append(terms, fmt.Sprintf("%s=%s", m.Label, quoteSPL(m.Value)))
		case connectors.MatchNotEqual:
			terms = append(terms, fmt.Sprintf("%s!=%s", m.Label, quoteSPL(m.Value)))
		case connectors.MatchRegex:
			regexCmds = append(regexCmds, fmt.Sprintf("| regex %s=%s", m.Label, quoteSPL(m.Value)))
		case connectors.MatchNotRegex:
			regexCmds = append(regexCmds, fmt.Sprintf("| regex %s!=%s", m.Label, quoteSPL(m.Value)))
		default:
			return "", fmt.Errorf("splunk: unsupported matcher operator %q", m.Op)
		}
	}

	if len(terms) == 0 {
		// Match everything within the time window rather than emitting a bare
		// `search` (which Splunk rejects) — for both raw event and metric
		// queries, a metric query still needs a base search to pipe into
		// stats/timechart.
		terms = append(terms, "*")
	}

	spl := "search " + strings.Join(terms, " ")
	for _, rc := range regexCmds {
		spl += " " + rc
	}

	if isMetricQuery(q) {
		agg, err := buildAgg(q)
		if err != nil {
			return "", err
		}
		spl += " " + agg
	}

	return strings.TrimSpace(spl), nil
}

// buildAgg builds the `stats`/`timechart` command for a metric query. The
// metric field (the value to aggregate) is taken from the Selector; count
// needs no field. A non-zero Step produces a `timechart span=<step>` so the
// result carries a timeseries; otherwise `stats` produces an instant value
// (a single scalar when there is no group-by).
func buildAgg(q connectors.NormalizedQuery) (string, error) {
	fn, needsField := aggFunc(q.Aggregation, q.Selector)

	var expr string
	if !needsField {
		expr = fn
	} else {
		field := strings.TrimSpace(q.Selector)
		if field == "" {
			return "", fmt.Errorf("splunk: aggregation %q requires a metric field in Selector", q.Aggregation)
		}
		expr = fmt.Sprintf("%s(%s)", fn, field)
	}

	by := ""
	if len(q.GroupBy) > 0 {
		by = " by " + strings.Join(q.GroupBy, ", ")
	}

	if q.TimeRange.Step > 0 {
		return fmt.Sprintf("| timechart span=%s %s%s", formatSpan(q.TimeRange.Step), expr, by), nil
	}
	return fmt.Sprintf("| stats %s%s", expr, by), nil
}

// aggFunc maps a NormalizedQuery aggregation to an SPL stats function and
// reports whether that function needs a field argument. AggCount maps to bare
// `count` (no field). AggNone on a metric query defaults to avg of the metric
// field, or count when no field is given.
func aggFunc(a connectors.Aggregation, selector string) (fn string, needsField bool) {
	switch a {
	case connectors.AggSum:
		return "sum", true
	case connectors.AggAvg:
		return "avg", true
	case connectors.AggMin:
		return "min", true
	case connectors.AggMax:
		return "max", true
	case connectors.AggRate:
		return "per_second", true
	case connectors.AggCount:
		return "count", false
	default: // AggNone
		if strings.TrimSpace(selector) != "" {
			return "avg", true
		}
		return "count", false
	}
}

// ensureLeadingCommand makes a raw SPL fragment a valid search by prefixing
// `search ` when it does not already start with a generating command
// (`search`, `|`, or `|`-prefixed search).
func ensureLeadingCommand(spl string) string {
	trimmed := strings.TrimSpace(spl)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "search ") || strings.HasPrefix(trimmed, "|") || lower == "search" {
		return trimmed
	}
	return "search " + trimmed
}

// quoteSPL wraps a value in double quotes and escapes embedded quotes and
// backslashes so a matcher value with spaces or quotes is a single SPL token.
func quoteSPL(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// formatSpan renders a duration as a whole-second SPL span string (e.g.
// "60s"). Sub-second windows are rounded up to 1s.
func formatSpan(d time.Duration) string {
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
