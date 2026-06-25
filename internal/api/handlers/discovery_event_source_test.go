// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// TestParseTiersOrDefault_AcceptsEventSource — event-source-tier
// slice 1 chunk 1 regression: "event_source" passes the validator and
// lands on the normalized list.
func TestParseTiersOrDefault_AcceptsEventSource(t *testing.T) {
	got := parseTiersOrDefault([]string{"event_source"})
	if len(got) != 1 || got[0] != TierEventSource {
		t.Fatalf("parseTiersOrDefault([event_source]) = %v, want [event_source]", got)
	}
}

// TestScanHandler_AcceptsEventSourceTier_ReturnsSnapshots — the tier
// parser normalizes "event_source" the same way as any other tier and
// rejects nothing. The case-insensitive trim variant also lands.
func TestScanHandler_AcceptsEventSourceTier_ReturnsSnapshots(t *testing.T) {
	for _, in := range []string{"event_source", "EVENT_SOURCE", "  event_source  "} {
		got := parseTiersOrDefault([]string{in})
		if len(got) != 1 || got[0] != TierEventSource {
			t.Errorf("parseTiersOrDefault(%q) = %v, want [event_source]", in, got)
		}
	}
}

// TestScanHandler_DefaultTierListIncludesEventSource — DefaultScanTiers
// contains TierEventSource so the implicit empty-request scan covers
// the event source surface alongside the other tiers.
func TestScanHandler_DefaultTierListIncludesEventSource(t *testing.T) {
	found := false
	for _, tier := range DefaultScanTiers {
		if tier == TierEventSource {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DefaultScanTiers missing TierEventSource: %v", DefaultScanTiers)
	}
}

// TestMarshalScanResult_IncludesEventSources — a Result with
// EventSources entries surfaces them on the wire as eventSourceRow
// values with the universal columns + per-surface Detail bag. Mirrors
// the orchestration variant.
func TestMarshalScanResult_IncludesEventSources(t *testing.T) {
	seen := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	r := &scanner.Result{
		ScanID:    "scan-1",
		Provider:  credstore.ProviderAWS,
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		EventSources: []scanner.EventSourceInstanceSnapshot{
			{
				Provider:     "aws",
				Surface:      "eventbridge",
				AccountID:    "123456789012",
				Region:       "us-east-1",
				ResourceName: "default",
				ResourceARN:  "arn:aws:events:us-east-1:123456789012:event-bus/default",
				SourceType:   "bus",
				HasTraceAxis: true,
				HasLogAxis:   true,
				LastSeenAt:   &seen,
				Detail:       map[string]any{"rule_count": 3},
			},
		},
		InstrumentedCount: 1,
	}

	out := marshalScanResult(r)
	if len(out.EventSources) != 1 {
		t.Fatalf("EventSources on wire = %d, want 1", len(out.EventSources))
	}
	row := out.EventSources[0]
	if row.Provider != "aws" || row.Surface != "eventbridge" {
		t.Errorf("provider/surface = %q/%q, want aws/eventbridge", row.Provider, row.Surface)
	}
	if row.SourceType != "bus" {
		t.Errorf("source_type lost on wire: got %q", row.SourceType)
	}
	if !row.HasTraceAxis || !row.HasLogAxis {
		t.Errorf("axes lost on wire: trace=%v log=%v", row.HasTraceAxis, row.HasLogAxis)
	}
	if row.LastSeenAt == nil || !row.LastSeenAt.Equal(seen) {
		t.Errorf("LastSeenAt lost on wire: got %v", row.LastSeenAt)
	}
	if got, _ := row.Detail["rule_count"].(int); got != 3 {
		t.Errorf("Detail[rule_count] lost on wire: got %v", row.Detail["rule_count"])
	}
}

// TestInventoryHandler_ResponseIncludesEventSourcesField — the JSON
// response carries the "event_sources" key alongside the other
// per-tier wire fields. Empty EventSources surfaces as "[]" rather
// than "null", matching the non-null posture on every other category
// array.
func TestInventoryHandler_ResponseIncludesEventSourcesField(t *testing.T) {
	r := &scanner.Result{
		ScanID:    "scan-1",
		Provider:  credstore.ProviderAWS,
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
	}
	out := marshalScanResult(r)
	if out.EventSources == nil {
		t.Errorf("EventSources = nil, want []eventSourceRow{}")
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := probe["event_sources"]; !ok {
		t.Errorf("response missing 'event_sources' field. keys=%v", keysOf(probe))
	}

	// The JSON field must contain "[]" (or equivalent) not "null" —
	// the per-cloud Inventory tab's empty-state branch is a single
	// `.length === 0` check.
	if !containsServerlessTokens(string(raw), `"event_sources":[]`) {
		t.Errorf("event_sources field not non-null array. raw=%s", string(raw))
	}
}

// stubEventSourceDiscoveryScanner is a minimal stub satisfying the
// optional EventSourceDiscoveryScanner interface so the dispatcher
// type-assertion path is exercised at compile time.
type stubEventSourceDiscoveryScanner struct{}

func (stubEventSourceDiscoveryScanner) Scan(_ context.Context, _ *credstore.CloudConnection, _ []string) (*scanner.Result, error) {
	return &scanner.Result{}, nil
}

func (stubEventSourceDiscoveryScanner) ScanEventSources(_ context.Context, _ scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	return []scanner.EventSourceInstanceSnapshot{{
		Provider:     "aws",
		Surface:      "eventbridge",
		ResourceName: "default",
		ResourceARN:  "arn:aws:events:us-east-1:123456789012:event-bus/default",
	}}, nil
}

// TestEventSourceDiscoveryScanner_InterfaceAssertable — defense-in-depth:
// the stub above satisfies BOTH DiscoveryScanner and
// EventSourceDiscoveryScanner at compile time, mirroring the orchestration
// arc's type-assertion path in runAWSScan.
func TestEventSourceDiscoveryScanner_InterfaceAssertable(t *testing.T) {
	var s DiscoveryScanner = stubEventSourceDiscoveryScanner{}
	es, ok := s.(EventSourceDiscoveryScanner)
	if !ok {
		t.Fatalf("stub does not satisfy EventSourceDiscoveryScanner")
	}
	out, err := es.ScanEventSources(context.Background(), scanner.ScanScope{})
	if err != nil {
		t.Fatalf("ScanEventSources: %v", err)
	}
	if len(out) != 1 || out[0].Surface != "eventbridge" {
		t.Errorf("unexpected snapshot list: %+v", out)
	}
}
