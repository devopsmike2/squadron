// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package siem

import "time"

// DestinationType is the wire protocol for a SIEM destination.
// Adding new ones (syslog, kafka, etc.) is a code change so the
// dispatcher knows how to build the request.
type DestinationType string

const (
	// SplunkHEC posts to Splunk's HTTP Event Collector. Token in
	// Secret, endpoint typically https://splunk.example.com:8088/
	// services/collector/event. Splunk's docs and conventions are
	// well-trodden at utilities.
	SplunkHEC DestinationType = "splunk_hec"
	// GenericWebhook posts signed JSON to an arbitrary HTTPS endpoint.
	// Secret is the HMAC-SHA256 signing key. Receivers verify the
	// X-Squadron-Signature header to confirm provenance.
	GenericWebhook DestinationType = "webhook"
)

// Destination is one configured SIEM endpoint.
//
// Secret is the encrypted-at-rest form (nonce || ciphertext) and is
// never returned in API responses. The UI sees HasSecret instead.
//
// EventTypePrefix is an optional allowlist filter — only events whose
// EventType starts with one of these prefixes are forwarded to this
// destination. Empty means forward all. Useful when one SIEM gets the
// full audit stream and another only gets, say, rollout.* events.
type Destination struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Type            DestinationType `json:"type"`
	URL             string          `json:"url"`
	Secret          []byte          `json:"-"` // ciphertext; never serialize
	HasSecret       bool            `json:"has_secret"`
	Enabled         bool            `json:"enabled"`
	EventTypePrefix []string        `json:"event_type_prefix,omitempty"`

	// Operational visibility — set by the dispatcher when it
	// drains the queue. Helps the operator see at a glance
	// whether the pipe is flowing.
	LastEventSentAt *time.Time `json:"last_event_sent_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastErrorAt     *time.Time `json:"last_error_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Event is the wire shape forwarded to exporters. Mirror of
// services.AuditEvent with field names that read well in a SIEM
// search bar.
type Event struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	Actor      string         `json:"actor"`
	EventType  string         `json:"event_type"`
	TargetType string         `json:"target_type,omitempty"`
	TargetID   string         `json:"target_id,omitempty"`
	Action     string         `json:"action,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Source     string         `json:"source"` // always "squadron"
}

// MatchesFilter reports whether an event should be forwarded to a
// destination given its EventTypePrefix allowlist. Empty allowlist =
// forward everything.
func (d *Destination) MatchesFilter(eventType string) bool {
	if len(d.EventTypePrefix) == 0 {
		return true
	}
	for _, p := range d.EventTypePrefix {
		if p == "" || hasPrefix(eventType, p) {
			return true
		}
	}
	return false
}

func hasPrefix(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	return s[:len(p)] == p
}
