// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package datadog is an OSS opt-in Datadog connector (first slice).
//
// Datadog is an OBSERVABILITY BACKEND, not a cloud provider, so it does
// not map onto Squadron's cloud-discovery connectors (AWS/GCP/Azure/OCI).
// It also does not map onto the telemetry query-connector interface
// (internal/connectors.Connector), whose surface is metrics/logs/traces
// query + normalize (NormalizedQuery -> SPL/LogQL/DQL). A Datadog HOST
// INVENTORY is neither a metric series nor a log stream — it is a fleet
// roster — so this slice deliberately does NOT implement that interface.
//
// What it DOES reuse from the connectors framework is the seam that
// actually fits: the stored, non-secret connectors.Config (endpoint +
// type-specific Settings), the sealed-at-rest ConnectorCredentials
// shape (the paired API + application keys the credentials.go comment
// already anticipates for Datadog), and the same OSS-inert, opt-in
// posture as the Loki/Splunk connectors — nothing in production code
// instantiates this, so it cannot be selected in a running deployment
// until a caller chooses to wire it.
//
// First-slice capability: a read-only discovery call (GET /api/v1/hosts)
// that lists the hosts Datadog is tracking, normalizes them into a
// vendor-neutral Host inventory model, and correlates that roster against
// Squadron's OTel fleet to surface the OBSERVABILITY GAP — hosts Datadog
// sees that are NOT reporting OTel telemetry to Squadron. That gap is the
// demo-valuable angle. Cost/usage analytics and a Datadog query connector
// (the SLO/burn-rate decision-context path) are explicit follow-ups, not
// this slice.
//
// The Datadog API is read-only here. Auth is the paired API key
// (DD-API-KEY) + application key (DD-APPLICATION-KEY); the site is
// region-specific and CONFIGURABLE (datadoghq.com / datadoghq.eu /
// us3 / us5 / ap1 / ddog-gov, ...) — never hardcoded beyond a default.
package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// TypeName is the connector's stable type key. It matches
// connectors.Config.Type and connectors.Descriptor.Type, mirroring the
// loki/splunk convention even though this connector does not register in
// the query Registry (its surface is host inventory, not query).
const TypeName = "datadog"

// Settings keys interpreted from connectors.Config.Settings. All are
// non-secret (the API/application keys live in ConnectorCredentials):
// the site is region routing and the timeout is a knob.
const (
	// SettingSite selects the region-specific Datadog site the base URL
	// is derived from, e.g. "datadoghq.com", "datadoghq.eu",
	// "us3.datadoghq.com", "us5.datadoghq.com", "ap1.datadoghq.com",
	// "ddog-gov.com". When empty, DefaultSite is used. Never hardcode a
	// single site into a call — a customer on the EU or a Gov site would
	// silently hit the wrong region.
	SettingSite = "site"

	// SettingTimeoutSeconds overrides the per-request HTTP timeout. When
	// empty or unparsable, defaultTimeout is used.
	SettingTimeoutSeconds = "timeout_seconds"
)

// Credential header names. Datadog authenticates every request with a
// paired API key and application key. They are read from
// ConnectorCredentials.Headers under these keys (the shape the
// credentials.go comment anticipates), with the primary bearer Token
// accepted as a fallback for the API key so a single-token store still
// works.
const (
	// HeaderAPIKey is the Datadog API key header.
	HeaderAPIKey = "DD-API-KEY"

	// HeaderAppKey is the Datadog application key header.
	HeaderAppKey = "DD-APPLICATION-KEY"
)

// DefaultSite is the site used when SettingSite is unset. It is only a
// default, never a hardcode in the request path — SettingSite always
// wins.
const DefaultSite = "datadoghq.com"

// defaultTimeout is the per-request HTTP timeout when SettingTimeoutSeconds
// is unset.
const defaultTimeout = 30 * time.Second

// hostsPageSize is the page size for the paginated /api/v1/hosts scan.
// 1000 is the endpoint's documented maximum count.
const hostsPageSize = 1000

// maxHostPages bounds the pagination loop so a runaway or hostile
// total_matching cannot spin forever. 1000 pages * 1000 hosts = a 1M-host
// ceiling, far past any real fleet; hitting it degrades to a truncation
// warning, never an infinite loop.
const maxHostPages = 1000

// Sentinel errors. Callers errors.Is against these to distinguish a
// configuration/credential problem from a backend failure. None of these
// (nor any error this package returns) ever carries secret material.
var (
	// ErrMissingCredentials is returned by New when the API key or the
	// application key is absent. The Datadog hosts API requires both, so
	// this is caught at construction rather than surfacing as an opaque
	// 403 at call time.
	ErrMissingCredentials = errors.New("datadog: API key and application key are both required")

	// ErrAuthFailed wraps a 401/403 from Datadog: the keys or the site are
	// wrong. The wrapped error never quotes the keys.
	ErrAuthFailed = errors.New("datadog: authentication failed")
)

// Client is a configured, concurrency-safe Datadog connector. A single
// instance serves many read calls.
type Client struct {
	cfg     connectors.Config
	baseURL string
	apiKey  string
	appKey  string
	client  *http.Client
}

// New is the connector factory. It mirrors connectors.Factory's shape
// (cfg + resolved credentials in, provider out) so the credential/config
// seam matches the query connectors, even though the returned type is a
// host-inventory Client rather than a query connectors.Connector.
//
// New performs no I/O. It validates that both keys are present (returning
// ErrMissingCredentials otherwise) and resolves the region-specific base
// URL from the configured site. Use HealthCheck to validate reachability
// and that the keys authenticate.
func New(cfg connectors.Config, creds connectors.ConnectorCredentials) (*Client, error) {
	apiKey := strings.TrimSpace(creds.Headers[HeaderAPIKey])
	if apiKey == "" {
		// Fall back to the primary bearer Token as the API key so a store
		// that only holds a single token still works.
		apiKey = strings.TrimSpace(creds.Token)
	}
	appKey := strings.TrimSpace(creds.Headers[HeaderAppKey])
	if apiKey == "" || appKey == "" {
		return nil, ErrMissingCredentials
	}

	baseURL, err := resolveBaseURL(cfg)
	if err != nil {
		return nil, err
	}

	timeout := defaultTimeout
	if v := cfg.Settings[SettingTimeoutSeconds]; v != "" {
		if secs, perr := strconv.Atoi(v); perr == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	return &Client{
		cfg:     cfg,
		baseURL: baseURL,
		apiKey:  apiKey,
		appKey:  appKey,
		// Nil Transport => http.DefaultTransport, which honors
		// HTTPS_PROXY / HTTP_PROXY / NO_PROXY via ProxyFromEnvironment.
		// The context on each request carries the caller's deadline; the
		// client Timeout is a backstop.
		client: &http.Client{Timeout: timeout},
	}, nil
}

// resolveBaseURL derives the API base URL. An explicit, absolute
// Config.Endpoint wins verbatim (a proxy, an air-gapped mirror, or a test
// server); otherwise the base is https://api.<site> from SettingSite,
// defaulting to DefaultSite. The returned URL has no trailing slash.
func resolveBaseURL(cfg connectors.Config) (string, error) {
	if ep := strings.TrimSpace(cfg.Endpoint); ep != "" {
		u, err := url.Parse(strings.TrimRight(ep, "/"))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", fmt.Errorf("datadog: endpoint %q is not a valid absolute URL", ep)
		}
		return strings.TrimRight(ep, "/"), nil
	}

	site := strings.TrimSpace(cfg.Settings[SettingSite])
	if site == "" {
		site = DefaultSite
	}
	// Guard against an operator pasting a full URL or an "api." prefix into
	// the site knob; normalize to a bare host and prepend the api subdomain.
	site = strings.TrimPrefix(site, "https://")
	site = strings.TrimPrefix(site, "http://")
	site = strings.TrimPrefix(site, "api.")
	site = strings.Trim(site, "/")
	if site == "" {
		return "", fmt.Errorf("datadog: %q is not a valid site", cfg.Settings[SettingSite])
	}
	return "https://api." + site, nil
}

// Describe returns the connector's static identity, reusing the framework
// Descriptor type so the connector reads consistently with the query
// connectors in any listing surface.
func (c *Client) Describe() connectors.Descriptor {
	return connectors.Descriptor{
		Type:        TypeName,
		DisplayName: "Datadog",
		Description: "Read-only Datadog host-inventory connector: lists Datadog-tracked hosts and correlates them against the OTel fleet to surface observability gaps.",
	}
}

// setHeaders attaches the paired auth headers to a request. It never logs
// or returns secret material.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set(HeaderAPIKey, c.apiKey)
	req.Header.Set(HeaderAppKey, c.appKey)
	req.Header.Set("Accept", "application/json")
}

// HealthCheck validates reachability and that the keys authenticate,
// without pulling the whole roster, by requesting a single host. It
// returns nil on success, ErrAuthFailed (errors.Is-comparable) on a
// 401/403, or a descriptive, secret-free error otherwise.
func (c *Client) HealthCheck(ctx context.Context) error {
	_, err := c.getHostsPage(ctx, 0, 1)
	return err
}

// ListHosts pages through GET /api/v1/hosts and returns the normalized
// host inventory. It is strictly read-only. On a 401/403 it returns
// ErrAuthFailed; on any other transport or decode failure it returns a
// descriptive, secret-free error. Pagination stops when a page returns
// fewer than the requested count, when the running total reaches
// total_matching, or at the maxHostPages safeguard (which stops cleanly
// and returns the partial roster rather than looping forever).
func (c *Client) ListHosts(ctx context.Context) ([]Host, error) {
	var hosts []Host
	seen := make(map[string]struct{})

	for page := 0; page < maxHostPages; page++ {
		start := page * hostsPageSize
		resp, err := c.getHostsPage(ctx, start, hostsPageSize)
		if err != nil {
			return nil, err
		}

		for i := range resp.HostList {
			h := normalizeHost(resp.HostList[i])
			// Defensive de-dup on canonical name: a host shifting between
			// pages during a scan could otherwise appear twice.
			key := NormalizeHostname(h.Name)
			if key != "" {
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
			}
			hosts = append(hosts, h)
		}

		// Stop when this page was not full (last page) or we have reached
		// the backend's reported match total.
		if len(resp.HostList) < hostsPageSize {
			break
		}
		if resp.TotalMatching > 0 && len(hosts) >= resp.TotalMatching {
			break
		}
	}

	return hosts, nil
}

// getHostsPage requests one page of the hosts endpoint and decodes it.
func (c *Client) getHostsPage(ctx context.Context, start, count int) (*rawHostsResponse, error) {
	params := url.Values{}
	params.Set("start", strconv.Itoa(start))
	params.Set("count", strconv.Itoa(count))

	reqURL := c.baseURL + "/api/v1/hosts?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("datadog: build hosts request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("datadog: hosts request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("datadog: read hosts response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Never quote the body here — a Datadog auth error can echo header
		// values. Return only the status.
		return nil, fmt.Errorf("%w (status %d): check the API key, application key, and the configured site", ErrAuthFailed, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("datadog: hosts request returned status %d: %s", resp.StatusCode, snippet(body))
	}

	var out rawHostsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("datadog: decode hosts response: %w", err)
	}
	return &out, nil
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
