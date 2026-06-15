// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package billing — connectors that fetch ACTUAL ingest stats from
// telemetry destinations and surface them alongside Squadron's
// estimates.
//
// The story we tell on the Savings page today is "we estimate you're
// shipping ~X GB/month, priced at $Y/GB, so ~$Z/month." That's based
// on the OTLP volume Squadron itself sees on the receiver. But the
// real invoice number depends on what the destination ACTUALLY
// ingests after its own dedup, indexing rules, license throttling,
// and so on. The gap between "estimated" and "actual" is one of the
// most common questions from finance.
//
// This package wraps each destination's "tell me my ingest" API into
// a normalized SnapshotProvider. The Savings page calls each
// configured provider once a day, caches the result, and renders an
// "Actual vs estimated" comparison tile.
//
// v0.42.0 ships the Splunk connector. New Relic, Honeycomb, and
// Datadog billing API connectors slot in here later — same shape.
//
// Added in v0.42.0 (connectors part 2).

package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Snapshot is the normalized view each provider returns. Bytes is
// the ACTUAL ingested bytes over the snapshot window (typically
// last 24h or last 30d).
type Snapshot struct {
	// Provider is the connector's vendor name — "splunk", "datadog",
	// etc. Used by the UI to pick the icon.
	Provider string `json:"provider"`
	// Window is the period the snapshot covers as a human-friendly
	// string ("24h", "30d", etc.).
	Window string `json:"window"`
	// Bytes is total ingested bytes in the window, per the
	// destination's billing report.
	Bytes int64 `json:"bytes"`
	// USD is the destination's own price quote if its API surfaces
	// it; otherwise zero (the UI falls back to Squadron's pricing
	// rules to compute a dollar number from Bytes).
	USD float64 `json:"usd,omitempty"`
	// At is the moment Squadron fetched the snapshot. Used to drive
	// freshness affordances ("collected 4h ago").
	At time.Time `json:"at"`
	// SourceURL is a deep link back to the destination's billing
	// dashboard so the operator can drill in for the underlying
	// numbers.
	SourceURL string `json:"source_url,omitempty"`
}

// SnapshotProvider is the interface every billing connector
// implements. Snapshot returns the most recent billing window
// available. Cancellation comes via ctx — destinations sometimes
// have multi-minute query times when billing reports are warming up.
type SnapshotProvider interface {
	Snapshot(ctx context.Context) (*Snapshot, error)
	// Name is the connector's vendor identifier. Lowercase, no
	// spaces. Used as the storage / cache key.
	Name() string
}

// ----------------------------------------------------------------
// Splunk connector
// ----------------------------------------------------------------

// SplunkConfig is the per-deployment settings. SearchHead is the
// host of the Splunk search head ("https://splunk.your-co.com").
// LicenseUsage points at a saved search the deployment runs against
// the index=_internal sourcetype=splunkd "INFO License usage" log
// channel — Splunk's standard daily license usage report.
//
// The token must have access to the dispatched search and to the
// /services/search/jobs/export endpoint. For SC's typical wiring,
// that's a service account with capability "search" on the _internal
// index.
type SplunkConfig struct {
	SearchHead string // e.g. "https://splunk.your-co.com:8089"
	Token      string // Splunk auth token (Bearer)
	// Window in days the connector should report on. 30 is the
	// natural billing cycle; 1 lets the connector be polled for
	// faster feedback. Default 30 when zero.
	WindowDays int
	// InsecureSkipVerify lets the connector talk to a Splunk
	// instance with a self-signed cert. Set ONLY if the operator
	// explicitly opts in via squadron.yaml; we don't surface this
	// in the UI to avoid accidentally weakening the trust posture.
	InsecureSkipVerify bool
	// HTTP client override for tests; nil → 30s default.
	HTTP *http.Client
}

// SplunkSnapshotProvider implements SnapshotProvider against a
// Splunk Enterprise / Cloud deployment.
type SplunkSnapshotProvider struct {
	cfg  SplunkConfig
	http *http.Client
}

// NewSplunkSnapshotProvider builds a provider. Returns nil and an
// error if SearchHead or Token is unset; the caller treats this as
// "Splunk billing is not configured" and silently disables the UI
// tile.
func NewSplunkSnapshotProvider(cfg SplunkConfig) (*SplunkSnapshotProvider, error) {
	if strings.TrimSpace(cfg.SearchHead) == "" {
		return nil, fmt.Errorf("splunk billing: SearchHead required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("splunk billing: Token required")
	}
	if cfg.WindowDays <= 0 {
		cfg.WindowDays = 30
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &SplunkSnapshotProvider{cfg: cfg, http: httpClient}, nil
}

func (p *SplunkSnapshotProvider) Name() string { return "splunk" }

// Snapshot runs the daily-license-usage search and returns the sum
// of `b` (bytes) over the configured window. The SPL is intentionally
// boring — Splunk-savvy folks will recognize it as the canonical
// license meter report.
//
// SPL:
//
//	search index=_internal source=*license_usage.log* type=Usage
//	  | bucket _time span=1d
//	  | stats sum(b) as bytes
//
// We request CSV (`output_mode=csv`) because the export endpoint's
// JSON output streams chunked, which makes the parse fiddly with
// the cancellation contract. CSV is one row per event with the
// final sum as the last row — easy to scan.
func (p *SplunkSnapshotProvider) Snapshot(ctx context.Context) (*Snapshot, error) {
	since := fmt.Sprintf("-%dd@d", p.cfg.WindowDays)
	spl := `search index=_internal source=*license_usage.log* type=Usage ` +
		`| stats sum(b) as bytes`

	form := url.Values{}
	form.Set("search", spl)
	form.Set("earliest_time", since)
	form.Set("latest_time", "now")
	form.Set("output_mode", "csv")
	form.Set("exec_mode", "oneshot")

	u := strings.TrimRight(p.cfg.SearchHead, "/") + "/services/search/jobs/export"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u,
		bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("splunk %d: %s", resp.StatusCode, string(raw))
	}

	bytesVal, err := parseBytesFromCSV(resp.Body)
	if err != nil {
		return nil, err
	}

	return &Snapshot{
		Provider:  "splunk",
		Window:    fmt.Sprintf("%dd", p.cfg.WindowDays),
		Bytes:     bytesVal,
		At:        time.Now().UTC(),
		SourceURL: strings.TrimRight(p.cfg.SearchHead, "/"),
	}, nil
}

// parseBytesFromCSV walks the CSV output looking for a column named
// "bytes" and returns the value from the first data row. This is the
// shape `| stats sum(b) as bytes` produces.
//
// Pulled into its own helper so the test suite can exercise the
// parser independently of the HTTP plumbing.
func parseBytesFromCSV(r io.Reader) (int64, error) {
	// Splunk's export CSV starts with a header row, then one data
	// row per result. For our single-stat search, header is
	// `bytes` and the data row is the integer.
	rawAll, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(rawAll)), "\n")
	if len(lines) < 2 {
		// Empty result set — Splunk returns nothing if the search
		// matched zero events. Report zero bytes rather than error;
		// the UI's "no data yet" rendering handles this gracefully.
		return 0, nil
	}
	header := strings.Split(lines[0], ",")
	col := -1
	for i, h := range header {
		if strings.TrimSpace(strings.Trim(h, "\"")) == "bytes" {
			col = i
			break
		}
	}
	if col < 0 {
		return 0, fmt.Errorf("splunk csv missing 'bytes' column: %q", lines[0])
	}
	fields := strings.Split(lines[1], ",")
	if col >= len(fields) {
		return 0, fmt.Errorf("splunk csv row missing column %d", col)
	}
	raw := strings.TrimSpace(strings.Trim(fields[col], "\""))
	if raw == "" {
		return 0, nil
	}
	// Parse as int. Splunk's `sum(b)` always returns an integer
	// because b is logged as an int; if we ever see a decimal, the
	// truncation is fine for billing-scale numbers.
	var n int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// jsonOK is a small helper for tests / debug that round-trips a
// snapshot through JSON encoding. Kept exported in case the v0.43
// CLI surfaces a `squadronctl billing snapshot` subcommand.
func (s *Snapshot) JSON() ([]byte, error) {
	return json.Marshal(s)
}
