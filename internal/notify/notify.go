// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package notify — vendor-aware webhook dispatcher.
//
// Squadron has three places that fire outbound notifications today:
//
//   - silent-agent alerts        (v0.33)
//   - deploy completion webhook  (v0.35)
//   - cost-spike webhook         (v0.29)
//
// Before v0.43 each of those just POSTed a Squadron-specific JSON
// body to whatever URL the operator pasted into squadron.yaml.
// That works for a homemade receiver, but it's painful when the
// destination is Slack (which wants Block Kit), Teams (which wants
// AdaptiveCards), PagerDuty (which has a strict Events API v2),
// or Opsgenie (which has its own schema).
//
// This package centralizes the translation. The caller builds one
// canonical Event, passes a Destination (URL + Type), and Dispatch
// reshapes the body for the target vendor before POSTing.
//
// New vendors slot in by adding a Format method to a new type and
// extending the switch in Dispatcher.Dispatch. The Event struct is
// vendor-agnostic — if you find yourself adding fields just to make
// one vendor render, push the vendor-specific shape into the
// formatter instead.
//
// Added in v0.43.0 (ChatOps).

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DestinationType picks the vendor formatter. Empty / "generic"
// preserves the legacy plain-JSON shape so existing receivers don't
// break on upgrade.
type DestinationType string

const (
	TypeGeneric   DestinationType = "generic"
	TypeSlack     DestinationType = "slack"
	TypeTeams     DestinationType = "teams"
	TypePagerDuty DestinationType = "pagerduty"
	TypeOpsgenie  DestinationType = "opsgenie"
	// v0.62 — Discord incoming webhook. Same destination URL shape as
	// Slack/Teams; the formatter emits a single embed per event with
	// severity-tinted color, fields, and a deep link.
	TypeDiscord DestinationType = "discord"
)

// Severity drives color/urgency in vendor formats. PagerDuty uses
// "critical", "warning", "info"; Slack uses block colors; Teams
// uses themeColor. Squadron normalizes to these three buckets so
// adding a vendor doesn't require a Squadron-side schema change.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Event is the vendor-agnostic notification payload. Every field is
// optional EXCEPT Title; the formatters degrade gracefully.
type Event struct {
	// Title is the one-line summary — always rendered prominently.
	Title string `json:"title"`
	// Summary is a 1-3 sentence body shown beneath the title.
	Summary string `json:"summary,omitempty"`
	// Severity drives color and (for PagerDuty/Opsgenie) urgency.
	Severity Severity `json:"severity,omitempty"`
	// Kind classifies the event so receivers can route. Examples:
	// "silent_agent.firing", "silent_agent.resolved",
	// "deploy.completed", "cost_spike.opened".
	Kind string `json:"kind,omitempty"`
	// DedupKey is the stable identifier for an ongoing incident.
	// PagerDuty uses it to deduplicate triggers; Opsgenie does the
	// same with the "alias" field. For silent-agent alerts, this is
	// usually the agent ID; for cost spikes, the spike row ID.
	DedupKey string `json:"dedup_key,omitempty"`
	// Action is what should happen on the receiver side. Maps onto
	// PagerDuty's event_action and Opsgenie's POST vs /close path.
	// Valid values: "trigger" (open/refresh), "resolve" (close).
	Action string `json:"action,omitempty"`
	// Link is a deep-link back into Squadron — e.g. /agents/<id>,
	// /deploy, /timeline. Each vendor renders it as a button or
	// link depending on its idiom.
	Link string `json:"link,omitempty"`
	// Fields are key/value extras shown as a labeled list in
	// Slack/Teams and as custom_details in PagerDuty/Opsgenie.
	// Pairs are rendered in insertion order; use this to surface
	// "host: foo", "last_seen: 3m ago" type metadata.
	Fields []Field `json:"fields,omitempty"`
	// At is when the event occurred. Falls back to time.Now() when
	// zero so existing call sites don't have to plumb a clock.
	At time.Time `json:"at,omitempty"`
}

// Field is one key/value row in the event details.
type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Destination is one notification target — URL plus vendor type.
// Extra is a kv blob for vendor-specific knobs (PagerDuty needs a
// routing key, Opsgenie needs an API key) so the Destination
// struct stays small.
type Destination struct {
	URL   string
	Type  DestinationType
	Extra map[string]string
}

// Dispatcher routes an Event to a Destination through the right
// formatter. One Dispatcher serves the whole process — there's no
// per-destination state.
type Dispatcher struct {
	HTTP *http.Client
}

// NewDispatcher builds a Dispatcher with a 10s HTTP timeout. Pass
// nil to NewDispatcherWith to take the default; otherwise injection
// for tests.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Dispatch formats the event for the destination's vendor and POSTs.
// Empty URL is a no-op (used by tests / disabled config) — returns
// nil so callers don't have to guard.
func (d *Dispatcher) Dispatch(ctx context.Context, dest Destination, ev Event) error {
	if dest.URL == "" {
		return nil
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.Severity == "" {
		ev.Severity = SeverityInfo
	}

	body, contentType, targetURL, err := d.formatFor(dest, ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	// PagerDuty and Opsgenie need their API tokens as a header.
	for k, v := range vendorHeaders(dest, ev) {
		req.Header.Set(k, v)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("notify %s %d: %s",
			dest.Type, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// formatFor picks the vendor formatter and returns (body, content-type, url).
// PagerDuty and Opsgenie have a fixed API URL — the URL on the
// Destination is ignored for those, but Extra["routing_key"] /
// Extra["api_key"] are required.
func (d *Dispatcher) formatFor(dest Destination, ev Event) ([]byte, string, string, error) {
	switch dest.Type {
	case TypeSlack:
		b, err := formatSlack(ev)
		return b, "application/json", dest.URL, err
	case TypeTeams:
		b, err := formatTeams(ev)
		return b, "application/json", dest.URL, err
	case TypeDiscord:
		b, err := formatDiscord(ev)
		return b, "application/json", dest.URL, err
	case TypePagerDuty:
		b, err := formatPagerDuty(dest, ev)
		// PagerDuty Events API v2 endpoint. The Destination URL is
		// ignored in favor of the canonical endpoint; the operator
		// supplies the routing key via Extra.
		return b, "application/json", "https://events.pagerduty.com/v2/enqueue", err
	case TypeOpsgenie:
		b, err := formatOpsgenie(ev)
		// Opsgenie supports US + EU regions; default US. Region picked
		// from Extra["region"] when set.
		region := dest.Extra["region"]
		base := "https://api.opsgenie.com"
		if strings.EqualFold(region, "eu") {
			base = "https://api.eu.opsgenie.com"
		}
		// "resolve" maps onto a different path (alias-targeted close).
		if ev.Action == "resolve" && ev.DedupKey != "" {
			return b, "application/json",
				base + "/v2/alerts/" + ev.DedupKey + "/close?identifierType=alias", err
		}
		return b, "application/json", base + "/v2/alerts", err
	case TypeGeneric, "":
		b, err := json.Marshal(ev)
		return b, "application/json", dest.URL, err
	default:
		return nil, "", "", fmt.Errorf("unknown destination type %q", dest.Type)
	}
}

// vendorHeaders returns the per-vendor auth header set. Returns nil
// for vendors that don't need anything beyond Content-Type.
func vendorHeaders(dest Destination, _ Event) map[string]string {
	switch dest.Type {
	case TypeOpsgenie:
		// Opsgenie's REST API uses "GenieKey" auth.
		if k := dest.Extra["api_key"]; k != "" {
			return map[string]string{"Authorization": "GenieKey " + k}
		}
	}
	return nil
}
