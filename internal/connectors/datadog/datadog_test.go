// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

const (
	testAPIKey = "test-api-key-SECRET"
	testAppKey = "test-app-key-SECRET"
)

// creds returns a valid paired-key credential set.
func creds() connectors.ConnectorCredentials {
	return connectors.ConnectorCredentials{
		Headers: map[string]string{
			HeaderAPIKey: testAPIKey,
			HeaderAppKey: testAppKey,
		},
	}
}

// newClientForServer builds a Client pointed at a test server via the
// Endpoint override.
func newClientForServer(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(connectors.Config{
		ID:       "dd-1",
		Type:     TypeName,
		Endpoint: url,
	}, creds())
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	return c
}

func TestNew_MissingCredentials(t *testing.T) {
	cases := []struct {
		name  string
		creds connectors.ConnectorCredentials
	}{
		{"no keys at all", connectors.ConnectorCredentials{}},
		{"only api key", connectors.ConnectorCredentials{Headers: map[string]string{HeaderAPIKey: testAPIKey}}},
		{"only app key", connectors.ConnectorCredentials{Headers: map[string]string{HeaderAppKey: testAppKey}}},
		{"blank keys", connectors.ConnectorCredentials{Headers: map[string]string{HeaderAPIKey: "  ", HeaderAppKey: "  "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(connectors.Config{Type: TypeName}, tc.creds)
			if !errors.Is(err, ErrMissingCredentials) {
				t.Fatalf("expected ErrMissingCredentials, got %v", err)
			}
		})
	}
}

func TestNew_TokenFallbackForAPIKey(t *testing.T) {
	// A store that only carries a bearer Token (+ app key header) should
	// still construct: Token is accepted as the API key.
	c, err := New(connectors.Config{Type: TypeName}, connectors.ConnectorCredentials{
		Token:   testAPIKey,
		Headers: map[string]string{HeaderAppKey: testAppKey},
	})
	if err != nil {
		t.Fatalf("New with token fallback: unexpected error: %v", err)
	}
	if c.apiKey != testAPIKey || c.appKey != testAppKey {
		t.Fatalf("keys not resolved from token fallback: api=%q app=%q", c.apiKey, c.appKey)
	}
}

func TestResolveBaseURL_SiteConfigurable(t *testing.T) {
	cases := []struct {
		name    string
		cfg     connectors.Config
		want    string
		wantErr bool
	}{
		{"default site", connectors.Config{}, "https://api.datadoghq.com", false},
		{"eu site", connectors.Config{Settings: map[string]string{SettingSite: "datadoghq.eu"}}, "https://api.datadoghq.eu", false},
		{"us3 site", connectors.Config{Settings: map[string]string{SettingSite: "us3.datadoghq.com"}}, "https://api.us3.datadoghq.com", false},
		{"us5 site", connectors.Config{Settings: map[string]string{SettingSite: "us5.datadoghq.com"}}, "https://api.us5.datadoghq.com", false},
		{"ap1 site", connectors.Config{Settings: map[string]string{SettingSite: "ap1.datadoghq.com"}}, "https://api.ap1.datadoghq.com", false},
		{"gov site", connectors.Config{Settings: map[string]string{SettingSite: "ddog-gov.com"}}, "https://api.ddog-gov.com", false},
		{"site with api. prefix normalized", connectors.Config{Settings: map[string]string{SettingSite: "api.datadoghq.eu"}}, "https://api.datadoghq.eu", false},
		{"site with scheme normalized", connectors.Config{Settings: map[string]string{SettingSite: "https://datadoghq.eu"}}, "https://api.datadoghq.eu", false},
		{"explicit endpoint override wins", connectors.Config{Endpoint: "https://dd-proxy.internal/dd"}, "https://dd-proxy.internal/dd", false},
		{"endpoint trailing slash trimmed", connectors.Config{Endpoint: "https://dd-proxy.internal/"}, "https://dd-proxy.internal", false},
		{"bad endpoint", connectors.Config{Endpoint: "not-a-url"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBaseURL(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got base %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("base URL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListHosts_AuthHeadersAndSiteURL(t *testing.T) {
	var gotAPIKey, gotAppKey, gotPath, gotStart, gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get(HeaderAPIKey)
		gotAppKey = r.Header.Get(HeaderAppKey)
		gotPath = r.URL.Path
		gotStart = r.URL.Query().Get("start")
		gotCount = r.URL.Query().Get("count")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rawHostsResponse{
			HostList: []rawHost{{
				Name:             "web-01",
				HostName:         "web-01.internal",
				Aliases:          []string{"web-01", "i-0abc123"},
				Up:               true,
				IsMuted:          true,
				LastReportedTime: 1700000000,
				Sources:          []string{"agent", "aws"},
				Apps:             []string{"nginx", "system"},
				TagsBySource:     map[string][]string{"Datadog": {"env:prod"}, "aws": {"region:us-east-1", "env:prod"}},
				AWSName:          "aws-web-01",
			}},
			TotalReturned: 1,
			TotalMatching: 1,
		})
	}))
	defer srv.Close()

	c := newClientForServer(t, srv.URL)
	hosts, err := c.ListHosts(context.Background())
	if err != nil {
		t.Fatalf("ListHosts: unexpected error: %v", err)
	}

	if gotAPIKey != testAPIKey {
		t.Errorf("DD-API-KEY header = %q, want %q", gotAPIKey, testAPIKey)
	}
	if gotAppKey != testAppKey {
		t.Errorf("DD-APPLICATION-KEY header = %q, want %q", gotAppKey, testAppKey)
	}
	if gotPath != "/api/v1/hosts" {
		t.Errorf("path = %q, want /api/v1/hosts", gotPath)
	}
	if gotStart != "0" {
		t.Errorf("start = %q, want 0", gotStart)
	}
	if gotCount != strconv.Itoa(hostsPageSize) {
		t.Errorf("count = %q, want %d", gotCount, hostsPageSize)
	}

	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1", len(hosts))
	}
	h := hosts[0]
	if h.Name != "web-01" {
		t.Errorf("Name = %q, want web-01", h.Name)
	}
	if !h.Up || !h.Muted {
		t.Errorf("Up=%v Muted=%v, want both true", h.Up, h.Muted)
	}
	if !h.LastReportedAt.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("LastReportedAt = %v, want unix 1700000000", h.LastReportedAt)
	}
	// Aliases exclude the canonical name and are deduped+sorted.
	wantAliases := []string{"aws-web-01", "i-0abc123", "web-01.internal"}
	if strings.Join(h.Aliases, ",") != strings.Join(wantAliases, ",") {
		t.Errorf("Aliases = %v, want %v", h.Aliases, wantAliases)
	}
	// Tags flattened across sources, deduped, sorted.
	wantTags := []string{"env:prod", "region:us-east-1"}
	if strings.Join(h.Tags, ",") != strings.Join(wantTags, ",") {
		t.Errorf("Tags = %v, want %v", h.Tags, wantTags)
	}
}

func TestListHosts_Pagination(t *testing.T) {
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		starts = append(starts, start)
		w.Header().Set("Content-Type", "application/json")

		var page rawHostsResponse
		page.TotalMatching = hostsPageSize + 3
		switch start {
		case "0":
			// A full page => triggers a second request.
			for i := 0; i < hostsPageSize; i++ {
				page.HostList = append(page.HostList, rawHost{Name: fmt.Sprintf("host-%04d", i)})
			}
		case strconv.Itoa(hostsPageSize):
			// A partial page => stop.
			for i := 0; i < 3; i++ {
				page.HostList = append(page.HostList, rawHost{Name: fmt.Sprintf("tail-%d", i)})
			}
		default:
			t.Errorf("unexpected start offset %q", start)
		}
		page.TotalReturned = len(page.HostList)
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	c := newClientForServer(t, srv.URL)
	hosts, err := c.ListHosts(context.Background())
	if err != nil {
		t.Fatalf("ListHosts: unexpected error: %v", err)
	}

	if len(hosts) != hostsPageSize+3 {
		t.Fatalf("got %d hosts, want %d", len(hosts), hostsPageSize+3)
	}
	if len(starts) != 2 || starts[0] != "0" || starts[1] != strconv.Itoa(hostsPageSize) {
		t.Fatalf("pagination offsets = %v, want [0 %d]", starts, hostsPageSize)
	}
}

func TestListHosts_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// Datadog can echo the offending key in the body; make sure our
		// error does not propagate it.
		_, _ = w.Write([]byte(`{"errors":["Forbidden: key ` + testAPIKey + ` is invalid"]}`))
	}))
	defer srv.Close()

	c := newClientForServer(t, srv.URL)
	_, err := c.ListHosts(context.Background())
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got %v", err)
	}
	assertNoSecretLeak(t, err)
}

func TestHealthCheck(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("count"); got != "1" {
				t.Errorf("health check count = %q, want 1", got)
			}
			_ = json.NewEncoder(w).Encode(rawHostsResponse{TotalMatching: 42})
		}))
		defer srv.Close()
		if err := newClientForServer(t, srv.URL).HealthCheck(context.Background()); err != nil {
			t.Fatalf("HealthCheck: unexpected error: %v", err)
		}
	})

	t.Run("auth failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		err := newClientForServer(t, srv.URL).HealthCheck(context.Background())
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("expected ErrAuthFailed, got %v", err)
		}
	})
}

func TestListHosts_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":["boom"]}`))
	}))
	defer srv.Close()

	_, err := newClientForServer(t, srv.URL).ListHosts(context.Background())
	if err == nil {
		t.Fatal("expected an error on 500")
	}
	if errors.Is(err, ErrAuthFailed) {
		t.Fatal("500 must not be classified as auth failure")
	}
}

func TestListHosts_Unreachable(t *testing.T) {
	// Point at a server that is immediately closed => connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := newClientForServer(t, url)
	_, err := c.ListHosts(context.Background())
	if err == nil {
		t.Fatal("expected an error against an unreachable backend")
	}
	assertNoSecretLeak(t, err)
}

func TestListHosts_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	_, err := newClientForServer(t, srv.URL).ListHosts(context.Background())
	if err == nil {
		t.Fatal("expected a decode error on malformed JSON")
	}
}

func TestListHosts_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rawHostsResponse{})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := newClientForServer(t, srv.URL).ListHosts(ctx)
	if err == nil {
		t.Fatal("expected an error with a cancelled context")
	}
}

func TestNormalizeHost_NameFallback(t *testing.T) {
	// No canonical name => fall back to host_name.
	h := normalizeHost(rawHost{HostName: "fallback-host", Aliases: []string{"fallback-host", "other"}})
	if h.Name != "fallback-host" {
		t.Fatalf("Name = %q, want fallback-host", h.Name)
	}
	// The alias equal to the resolved name is excluded.
	for _, a := range h.Aliases {
		if strings.EqualFold(a, "fallback-host") {
			t.Fatalf("alias list must not include the canonical name: %v", h.Aliases)
		}
	}
	if len(h.Aliases) != 1 || h.Aliases[0] != "other" {
		t.Fatalf("Aliases = %v, want [other]", h.Aliases)
	}
}

func TestCorrelateFleet(t *testing.T) {
	hosts := []Host{
		{Name: "web-01", Aliases: []string{"web-01.internal"}},
		{Name: "DB-02"},     // matches fleet case-insensitively
		{Name: "orphan-03"}, // gap: not in fleet
		{Name: "cache-04", Aliases: []string{"c4"}}, // matched via alias
	}
	fleet := map[string]string{
		"web-01": "agent-web",   // matches canonical
		"db-02":  "agent-db",    // matches DB-02 lowercased
		"c4":     "agent-cache", // matches cache-04 by alias
	}

	rep := CorrelateFleet(hosts, fleet)

	if rep.TotalDatadogHosts != 4 {
		t.Errorf("TotalDatadogHosts = %d, want 4", rep.TotalDatadogHosts)
	}
	if rep.CoveredCount != 3 || rep.GapCount != 1 {
		t.Fatalf("covered=%d gap=%d, want covered=3 gap=1", rep.CoveredCount, rep.GapCount)
	}
	if len(rep.Gaps) != 1 || rep.Gaps[0].Host.Name != "orphan-03" {
		t.Fatalf("gaps = %+v, want single orphan-03", rep.Gaps)
	}
	// Check the alias match recorded the matched identity + fleet id.
	var cache Coverage
	for _, c := range rep.Covered {
		if c.Host.Name == "cache-04" {
			cache = c
		}
	}
	if !cache.Covered || cache.MatchedFleetID != "agent-cache" || cache.MatchedOn != "c4" {
		t.Fatalf("cache-04 coverage = %+v, want matched via alias c4 -> agent-cache", cache)
	}
}

func TestCorrelateFleet_NilFleetAllGaps(t *testing.T) {
	hosts := []Host{{Name: "a"}, {Name: "b"}}
	rep := CorrelateFleet(hosts, nil)
	if rep.GapCount != 2 || rep.CoveredCount != 0 {
		t.Fatalf("nil fleet: covered=%d gap=%d, want 0/2", rep.CoveredCount, rep.GapCount)
	}
	// Slices are non-nil (JSON serializes as [] not null) — a repeatedly
	// hit class of blank-page bug in this project.
	if rep.Covered == nil || rep.Gaps == nil {
		t.Fatal("Covered/Gaps must be non-nil slices")
	}
}

func TestCorrelateFleet_EmptyHosts(t *testing.T) {
	rep := CorrelateFleet(nil, map[string]string{"x": "y"})
	if rep.TotalDatadogHosts != 0 || rep.CoveredCount != 0 || rep.GapCount != 0 {
		t.Fatalf("empty hosts: %+v", rep)
	}
	if rep.Covered == nil || rep.Gaps == nil {
		t.Fatal("Covered/Gaps must be non-nil slices even when empty")
	}
}

// assertNoSecretLeak fails if either configured key appears in the error
// text — the no-secret-in-errors invariant.
func assertNoSecretLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, testAPIKey) || strings.Contains(msg, testAppKey) {
		t.Fatalf("error leaked a secret: %q", msg)
	}
}
