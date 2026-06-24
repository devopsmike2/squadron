// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// stubQualityIndex satisfies QualitySnapshotIndex from a fixed slice
// of snapshots. The per-key lookup is a map; SnapshotAll preserves
// insertion order so tests can assert on bucketing math without
// chasing iteration nondeterminism.
type stubQualityIndex struct {
	snaps []traceindex.QualityCountersSnapshot
	byKey map[string]traceindex.QualityCountersSnapshot
}

func newStubQualityIndex(snaps ...traceindex.QualityCountersSnapshot) *stubQualityIndex {
	idx := &stubQualityIndex{
		snaps: snaps,
		byKey: make(map[string]traceindex.QualityCountersSnapshot, len(snaps)),
	}
	for _, s := range snaps {
		idx.byKey[s.Key] = s
	}
	return idx
}

func (s *stubQualityIndex) SnapshotAll() []traceindex.QualityCountersSnapshot {
	return s.snaps
}

func (s *stubQualityIndex) SnapshotKey(key string) (traceindex.QualityCountersSnapshot, bool) {
	v, ok := s.byKey[key]
	return v, ok
}

// stubKeyProjector resolves (provider, kind, id) -> key from a fixed
// map. The handler treats a missing entry the same as a missing
// inventory row — 404.
type stubKeyProjector struct {
	keys map[string]string
}

func (p *stubKeyProjector) ProjectKey(_ context.Context, provider, kind, id string) (string, bool) {
	key, ok := p.keys[provider+"/"+kind+"/"+id]
	return key, ok
}

// recordingAuditService captures every Record call. Used to assert
// the cache-miss-only emission semantics.
type recordingAuditService struct {
	mu       sync.Mutex
	recorded []services.AuditEntry
}

func (r *recordingAuditService) Record(_ context.Context, e services.AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, e)
	return nil
}
func (r *recordingAuditService) List(context.Context, services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return nil, nil
}
func (r *recordingAuditService) Get(context.Context, string) (*services.AuditEvent, error) {
	return nil, nil
}
func (r *recordingAuditService) SetExplanation(context.Context, string, string, string, time.Time) error {
	return nil
}

func (r *recordingAuditService) calls() []services.AuditEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]services.AuditEntry, len(r.recorded))
	copy(out, r.recorded)
	return out
}

// newSpanQualityRouter wires the handlers under a fresh gin router so
// the tests assert on the route registration shape too. ttl + clock
// are overridable so the cache-refresh test can drive deterministic
// expiration.
func newSpanQualityRouter(t *testing.T, idx QualitySnapshotIndex, proj ResourceKeyProjector, audit services.AuditService, ttl time.Duration, clock func() time.Time) (*gin.Engine, *DiscoverySpanQualityHandlers) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewDiscoverySpanQualityHandlers(idx, proj, audit, ttl, clock, nil)
	r := gin.New()
	r.GET("/api/v1/discovery/span_quality", h.HandleSpanQuality)
	r.GET("/api/v1/discovery/:provider/inventory/:kind/:id/span_quality", h.HandleResourceSpanQuality)
	return r, h
}

// TestSpanQuality_NoObservations_ReturnsZeroes — cold-start posture
// from §6.1: zero snapshots → 200 with every provider key populated
// as zero-counts, Totals all zero.
func TestSpanQuality_NoObservations_ReturnsZeroes(t *testing.T) {
	idx := newStubQualityIndex()
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got SpanQualityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		v, ok := got.Providers[p]
		if !ok {
			t.Errorf("provider %q missing from response", p)
			continue
		}
		if v.ResourceCount != 0 || v.ResourcesWithIssues != 0 || v.OrphanPct != 0 {
			t.Errorf("provider %q expected zero-counts, got %+v", p, v)
		}
	}
	if got.Totals.ResourceCount != 0 {
		t.Errorf("Totals.ResourceCount = %d, want 0", got.Totals.ResourceCount)
	}
}

// TestSpanQuality_AllProvidersAggregateCorrectly — §6.1 happy path.
// Four providers, one snapshot each, varying percentages. Per-provider
// aggregates equal the per-snapshot values; totals re-mean across all
// four.
func TestSpanQuality_AllProvidersAggregateCorrectly(t *testing.T) {
	idx := newStubQualityIndex(
		traceindex.QualityCountersSnapshot{Key: "aws:123:i-1", Provider: "aws", TotalSpans: 500, OrphanPct: 4.0, MissingAttrPct: 8.0, AttrMismatchPct: 2.0},
		traceindex.QualityCountersSnapshot{Key: "gcp:proj-x:host-1", Provider: "gcp", TotalSpans: 600, OrphanPct: 6.0, MissingAttrPct: 4.0, AttrMismatchPct: 0.0},
		traceindex.QualityCountersSnapshot{Key: "azure:sub-y:host-2", Provider: "azure", TotalSpans: 300, OrphanPct: 0.0, MissingAttrPct: 12.0, AttrMismatchPct: 3.0},
		traceindex.QualityCountersSnapshot{Key: "oci:ten-z:host-3", Provider: "oci", TotalSpans: 700, OrphanPct: 2.0, MissingAttrPct: 4.0, AttrMismatchPct: 7.0},
	)
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got SpanQualityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Providers["aws"].OrphanPct != 4.0 {
		t.Errorf("aws.OrphanPct = %v, want 4.0", got.Providers["aws"].OrphanPct)
	}
	if got.Providers["gcp"].OrphanPct != 6.0 {
		t.Errorf("gcp.OrphanPct = %v, want 6.0", got.Providers["gcp"].OrphanPct)
	}
	if got.Totals.ResourceCount != 4 {
		t.Errorf("Totals.ResourceCount = %d, want 4", got.Totals.ResourceCount)
	}
	// 4+6+0+2 = 12 / 4 = 3.0 — orphan mean across four resources.
	if got.Totals.OrphanPct != 3.0 {
		t.Errorf("Totals.OrphanPct = %v, want 3.0", got.Totals.OrphanPct)
	}
}

// TestSpanQuality_ResourcesWithIssues_CountsKeysWithAnyPathology —
// §6.1 contract: a key is "with issues" when AT LEAST ONE of orphan /
// missing / mismatch is non-zero, not when all three are.
func TestSpanQuality_ResourcesWithIssues_CountsKeysWithAnyPathology(t *testing.T) {
	idx := newStubQualityIndex(
		// Clean — no issues.
		traceindex.QualityCountersSnapshot{Key: "aws:1:a", Provider: "aws", TotalSpans: 100},
		// Orphan only.
		traceindex.QualityCountersSnapshot{Key: "aws:1:b", Provider: "aws", TotalSpans: 100, OrphanPct: 5.0},
		// Missing only.
		traceindex.QualityCountersSnapshot{Key: "aws:1:c", Provider: "aws", TotalSpans: 100, MissingAttrPct: 30.0},
		// All three.
		traceindex.QualityCountersSnapshot{Key: "aws:1:d", Provider: "aws", TotalSpans: 100, OrphanPct: 12.0, MissingAttrPct: 26.0, AttrMismatchPct: 6.0},
	)
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	var got SpanQualityResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Providers["aws"].ResourceCount != 4 {
		t.Errorf("aws.ResourceCount = %d, want 4", got.Providers["aws"].ResourceCount)
	}
	if got.Providers["aws"].ResourcesWithIssues != 3 {
		t.Errorf("aws.ResourcesWithIssues = %d, want 3 (only the clean one is excluded)", got.Providers["aws"].ResourcesWithIssues)
	}
}

// TestSpanQuality_CacheRefreshes_AfterTTL — §6.1 cache contract: a
// second request inside the TTL returns the same body without
// re-walking SnapshotAll; once TTL elapses, the body refreshes.
func TestSpanQuality_CacheRefreshes_AfterTTL(t *testing.T) {
	clockTime := time.Now()
	clock := func() time.Time { return clockTime }
	idx := newStubQualityIndex(
		traceindex.QualityCountersSnapshot{Key: "aws:1:a", Provider: "aws", TotalSpans: 100, OrphanPct: 2.0},
	)
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 100*time.Millisecond, clock)

	doRequest := func() SpanQualityResponse {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
		var got SpanQualityResponse
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return got
	}

	first := doRequest()
	if first.Providers["aws"].OrphanPct != 2.0 {
		t.Fatalf("first orphan = %v, want 2.0", first.Providers["aws"].OrphanPct)
	}

	// Mutate the underlying snapshot. The cache is still warm — the
	// response should still reflect the FIRST snapshot.
	idx.snaps[0].OrphanPct = 9.9
	idx.byKey[idx.snaps[0].Key] = idx.snaps[0]
	cached := doRequest()
	if cached.Providers["aws"].OrphanPct != 2.0 {
		t.Errorf("inside TTL: orphan = %v, want cached 2.0", cached.Providers["aws"].OrphanPct)
	}

	// Advance past TTL. Next request must refresh.
	clockTime = clockTime.Add(200 * time.Millisecond)
	refreshed := doRequest()
	if refreshed.Providers["aws"].OrphanPct != 9.9 {
		t.Errorf("after TTL: orphan = %v, want refreshed 9.9", refreshed.Providers["aws"].OrphanPct)
	}
}

// TestSpanQuality_CacheMissEmitsAudit — cache MISS emits exactly one
// AuditEventSpanQualityRequested row; subsequent cache HITs emit
// none.
func TestSpanQuality_CacheMissEmitsAudit(t *testing.T) {
	idx := newStubQualityIndex()
	audit := &recordingAuditService{}
	r, _ := newSpanQualityRouter(t, idx, nil, audit, 5*time.Second, nil)

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	}
	calls := audit.calls()
	if len(calls) != 1 {
		t.Fatalf("audit calls = %d, want exactly 1 (cache-miss-only)", len(calls))
	}
	if calls[0].EventType != services.AuditEventSpanQualityRequested {
		t.Errorf("event type = %q, want %q", calls[0].EventType, services.AuditEventSpanQualityRequested)
	}
	if calls[0].Payload["cache_status"] != "miss" {
		t.Errorf("payload.cache_status = %v, want miss", calls[0].Payload["cache_status"])
	}
}

// TestResourceSpanQuality_404OnMissingObservations — §6.2 cold-start
// posture: a resource with no Quality observations returns 404.
func TestResourceSpanQuality_404OnMissingObservations(t *testing.T) {
	idx := newStubQualityIndex()
	proj := &stubKeyProjector{keys: map[string]string{
		"aws/ec2/i-0abc": "aws:123:i-0abc",
	}}
	r, _ := newSpanQualityRouter(t, idx, proj, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/aws/inventory/ec2/i-0abc/span_quality", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

// TestResourceSpanQuality_200ReturnsPlaceholderList — §6.2 happy
// path: snapshot present, placeholder list preserved verbatim.
func TestResourceSpanQuality_200ReturnsPlaceholderList(t *testing.T) {
	idx := newStubQualityIndex(traceindex.QualityCountersSnapshot{
		Key:             "aws:123:i-0abc",
		Provider:        "aws",
		TotalSpans:      500,
		WindowStart:     time.Date(2026, 6, 23, 11, 0, 0, 0, time.UTC),
		OrphanPct:       3.2,
		MissingAttrPct:  8.1,
		AttrMismatchPct: 12.5,
		Placeholders: []traceindex.PlaceholderObservation{
			{Attribute: "host.name", Placeholder: "localhost", SeenAt: time.Date(2026, 6, 23, 11, 30, 0, 0, time.UTC)},
			{Attribute: "cloud.account.id", Placeholder: "000000000000", SeenAt: time.Date(2026, 6, 23, 11, 35, 0, 0, time.UTC)},
		},
	})
	proj := &stubKeyProjector{keys: map[string]string{
		"aws/ec2/i-0abc": "aws:123:i-0abc",
	}}
	r, _ := newSpanQualityRouter(t, idx, proj, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/aws/inventory/ec2/i-0abc/span_quality", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got ResourceSpanQuality
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ResourceID != "i-0abc" {
		t.Errorf("ResourceID = %q", got.ResourceID)
	}
	if got.TotalSpans != 500 {
		t.Errorf("TotalSpans = %d, want 500", got.TotalSpans)
	}
	if !got.HasIssues {
		t.Errorf("HasIssues = false; want true (orphan + missing + mismatch all non-zero)")
	}
	if len(got.Placeholders) != 2 {
		t.Fatalf("Placeholders len = %d, want 2", len(got.Placeholders))
	}
	if got.Placeholders[0].Attribute != "host.name" || got.Placeholders[0].Placeholder != "localhost" {
		t.Errorf("Placeholders[0] = %+v", got.Placeholders[0])
	}
}

// TestSpanQuality_ProviderInferredFromKey_WhenSnapshotProviderEmpty —
// the inferProviderFromKey fallback kicks in when the snapshot's
// Provider field is empty (a legacy chunk-1 observation made before
// chunk 2 wired provider through). The handler still buckets the
// snapshot correctly under "aws" by reading the key prefix.
func TestSpanQuality_ProviderInferredFromKey_WhenSnapshotProviderEmpty(t *testing.T) {
	idx := newStubQualityIndex(
		traceindex.QualityCountersSnapshot{Key: "aws:1:a", Provider: "", TotalSpans: 100, OrphanPct: 4.0},
		traceindex.QualityCountersSnapshot{Key: "arn:aws:ec2:us-east-1:1:instance/i-abc", Provider: "", TotalSpans: 100, OrphanPct: 6.0},
	)
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	var got SpanQualityResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Providers["aws"].ResourceCount != 2 {
		t.Errorf("aws.ResourceCount = %d, want 2 (key-prefix inference)", got.Providers["aws"].ResourceCount)
	}
}

// --- Slice 2 (v0.89.110) tests — W3C trace context fields ---------

// TestSpanQuality_IncludesNewPercentageFields — the cross-provider
// endpoint exposes the two new W3C percentages in both ProviderSpanQuality
// and SpanQualityTotals. Mirrors §11 acceptance tests 13/15: the API
// surface must reflect the per-resource counters.
func TestSpanQuality_IncludesNewPercentageFields(t *testing.T) {
	idx := newStubQualityIndex(
		traceindex.QualityCountersSnapshot{
			Key:                          "aws:1:a",
			Provider:                     "aws",
			TotalSpans:                   1000,
			SpansWithTraceparent:         200,
			ChildSpans:                   400,
			MalformedTraceparentPct:      4.0,
			MissingTraceparentOnChildPct: 8.0,
		},
	)
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got SpanQualityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Providers["aws"].MalformedTraceparentPct != 4.0 {
		t.Errorf("aws.MalformedTraceparentPct = %v, want 4.0", got.Providers["aws"].MalformedTraceparentPct)
	}
	if got.Providers["aws"].MissingTraceparentOnChildPct != 8.0 {
		t.Errorf("aws.MissingTraceparentOnChildPct = %v, want 8.0", got.Providers["aws"].MissingTraceparentOnChildPct)
	}
	if got.Totals.MalformedTraceparentPct != 4.0 {
		t.Errorf("totals.MalformedTraceparentPct = %v, want 4.0", got.Totals.MalformedTraceparentPct)
	}
	if got.Totals.MissingTraceparentOnChildPct != 8.0 {
		t.Errorf("totals.MissingTraceparentOnChildPct = %v, want 8.0", got.Totals.MissingTraceparentOnChildPct)
	}
}

// TestSpanQuality_AggregationUsesHonestDenominators — the per-provider
// mean for the two new percentages divides by the count of resources
// that ACTUALLY contributed to the rate (SpansWithTraceparent > 0 for
// malformed; ChildSpans > 0 for missing-on-child), not the total
// resource count. A fleet of mostly root spans shouldn't dilute the
// missing-on-child rate to zero. Pins §11 acceptance test 14's intent
// extended to the cross-provider aggregation.
func TestSpanQuality_AggregationUsesHonestDenominators(t *testing.T) {
	// Two resources contribute to malformed (10% and 6% — mean 8%).
	// One additional resource has 0 spans-with-traceparent, which
	// should NOT pull the malformed mean down to (10+6+0)/3 = 5.3%.
	// Two resources contribute to missing-on-child (12% and 6% —
	// mean 9%); a third resource has 0 child spans and stays out
	// of the missing-on-child denominator.
	idx := newStubQualityIndex(
		traceindex.QualityCountersSnapshot{
			Key: "aws:1:a", Provider: "aws", TotalSpans: 1000,
			SpansWithTraceparent: 200, ChildSpans: 400,
			MalformedTraceparentPct: 10.0, MissingTraceparentOnChildPct: 12.0,
		},
		traceindex.QualityCountersSnapshot{
			Key: "aws:1:b", Provider: "aws", TotalSpans: 1000,
			SpansWithTraceparent: 100, ChildSpans: 200,
			MalformedTraceparentPct: 6.0, MissingTraceparentOnChildPct: 6.0,
		},
		traceindex.QualityCountersSnapshot{
			// No traceparent / no child spans — must NOT count in
			// either denominator.
			Key: "aws:1:c", Provider: "aws", TotalSpans: 1000,
			SpansWithTraceparent: 0, ChildSpans: 0,
		},
	)
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	var got SpanQualityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 10 + 6 = 16 / 2 = 8.0, NOT (10+6+0)/3 = 5.3.
	if got.Providers["aws"].MalformedTraceparentPct != 8.0 {
		t.Errorf("aws.MalformedTraceparentPct = %v, want 8.0 (honest denominator)", got.Providers["aws"].MalformedTraceparentPct)
	}
	// 12 + 6 = 18 / 2 = 9.0.
	if got.Providers["aws"].MissingTraceparentOnChildPct != 9.0 {
		t.Errorf("aws.MissingTraceparentOnChildPct = %v, want 9.0 (honest denominator)", got.Providers["aws"].MissingTraceparentOnChildPct)
	}
	// Totals get the same honest-denominator treatment.
	if got.Totals.MalformedTraceparentPct != 8.0 {
		t.Errorf("totals.MalformedTraceparentPct = %v, want 8.0", got.Totals.MalformedTraceparentPct)
	}
	if got.Totals.MissingTraceparentOnChildPct != 9.0 {
		t.Errorf("totals.MissingTraceparentOnChildPct = %v, want 9.0", got.Totals.MissingTraceparentOnChildPct)
	}
}

// TestSpanQuality_PerResourceEndpoint_IncludesNewFields — the
// per-resource detail endpoint exposes the four new fields
// (MalformedTraceparentPct + MissingTraceparentOnChildPct percentages
// plus their raw denominators SpansWithTraceparent + ChildSpans so the
// drill-down can render honest "N of M" framing).
func TestSpanQuality_PerResourceEndpoint_IncludesNewFields(t *testing.T) {
	idx := newStubQualityIndex(traceindex.QualityCountersSnapshot{
		Key:                          "aws:123:i-0abc",
		Provider:                     "aws",
		TotalSpans:                   500,
		SpansWithTraceparent:         150,
		ChildSpans:                   200,
		MalformedTraceparentPct:      2.7,
		MissingTraceparentOnChildPct: 4.3,
	})
	proj := &stubKeyProjector{keys: map[string]string{
		"aws/ec2/i-0abc": "aws:123:i-0abc",
	}}
	r, _ := newSpanQualityRouter(t, idx, proj, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/aws/inventory/ec2/i-0abc/span_quality", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got ResourceSpanQuality
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MalformedTraceparentPct != 2.7 {
		t.Errorf("MalformedTraceparentPct = %v, want 2.7", got.MalformedTraceparentPct)
	}
	if got.MissingTraceparentOnChildPct != 4.3 {
		t.Errorf("MissingTraceparentOnChildPct = %v, want 4.3", got.MissingTraceparentOnChildPct)
	}
	if got.SpansWithTraceparent != 150 {
		t.Errorf("SpansWithTraceparent = %d, want 150", got.SpansWithTraceparent)
	}
	if got.ChildSpans != 200 {
		t.Errorf("ChildSpans = %d, want 200", got.ChildSpans)
	}
}

// TestSpanQuality_PerResourceEndpoint_HasIssuesExtendsToNewFields —
// when ONLY a traceparent percentage is non-zero (the three slice-1
// percentages are all zero), HasIssues still returns true so the
// drill-down panel surfaces. The previous chunk-1 behavior would have
// returned false here and hidden the traceparent pathology.
func TestSpanQuality_PerResourceEndpoint_HasIssuesExtendsToNewFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap traceindex.QualityCountersSnapshot
	}{
		{
			name: "malformed only",
			snap: traceindex.QualityCountersSnapshot{
				Key: "aws:123:i-x", Provider: "aws", TotalSpans: 500,
				SpansWithTraceparent: 200, MalformedTraceparentPct: 2.0,
			},
		},
		{
			name: "missing on child only",
			snap: traceindex.QualityCountersSnapshot{
				Key: "aws:123:i-x", Provider: "aws", TotalSpans: 500,
				ChildSpans: 200, MissingTraceparentOnChildPct: 7.0,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newStubQualityIndex(tc.snap)
			proj := &stubKeyProjector{keys: map[string]string{
				"aws/ec2/i-x": "aws:123:i-x",
			}}
			r, _ := newSpanQualityRouter(t, idx, proj, nil, 0, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/aws/inventory/ec2/i-x/span_quality", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var got ResourceSpanQuality
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.HasIssues {
				t.Errorf("HasIssues = false; want true (only traceparent percentage non-zero)")
			}
		})
	}
}

// --- Sampling rate slice 1 (v0.89.124, #764 Stream 162) ---------------

// stubSamplingInventoryReader returns a fixed per-provider count map.
// Used by the sampling-rate aggregation tests to drive deterministic
// SamplingTooAggressivePct math without standing up a real inventory.
type stubSamplingInventoryReader struct {
	counts map[string]SamplingProviderCount
}

func (s *stubSamplingInventoryReader) SamplingQualifyingCounts(_ context.Context) map[string]SamplingProviderCount {
	return s.counts
}

// TestSpanQuality_IncludesSamplingTooAggressivePct — the response
// shape carries SamplingTooAggressivePct on every per-provider row
// and on the totals row. Nil inventory reader leaves every value
// at 0 (cold-start posture); a wired reader populates the percentage
// from the per-provider qualifying / too-aggressive counts.
func TestSpanQuality_IncludesSamplingTooAggressivePct(t *testing.T) {
	idx := newStubQualityIndex()
	reader := &stubSamplingInventoryReader{counts: map[string]SamplingProviderCount{
		// AWS: 2 of 10 qualifying serverless resources fire the
		// sampling-too-aggressive recommendation → 20%.
		"aws": {QualifyingCount: 10, TooAggressiveCount: 2},
		// GCP: 1 of 5 → 20%.
		"gcp": {QualifyingCount: 5, TooAggressiveCount: 1},
		// Azure: no qualifying resources → 0% (denominator zero, not
		// included in totals).
		"azure": {QualifyingCount: 0, TooAggressiveCount: 0},
		// OCI: 0 of 3 fire → 0%.
		"oci": {QualifyingCount: 3, TooAggressiveCount: 0},
	}}
	r, h := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	h.SetSamplingInventoryReader(reader)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got SpanQualityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Per-provider percentages.
	if got.Providers["aws"].SamplingTooAggressivePct != 20.0 {
		t.Errorf("aws.SamplingTooAggressivePct = %v, want 20.0", got.Providers["aws"].SamplingTooAggressivePct)
	}
	if got.Providers["gcp"].SamplingTooAggressivePct != 20.0 {
		t.Errorf("gcp.SamplingTooAggressivePct = %v, want 20.0", got.Providers["gcp"].SamplingTooAggressivePct)
	}
	if got.Providers["azure"].SamplingTooAggressivePct != 0 {
		t.Errorf("azure.SamplingTooAggressivePct = %v, want 0 (no qualifying)", got.Providers["azure"].SamplingTooAggressivePct)
	}
	if got.Providers["oci"].SamplingTooAggressivePct != 0 {
		t.Errorf("oci.SamplingTooAggressivePct = %v, want 0 (none fire)", got.Providers["oci"].SamplingTooAggressivePct)
	}
	// Totals: (2 + 1 + 0 + 0) / (10 + 5 + 0 + 3) = 3 / 18 ≈ 16.7%.
	if got.Totals.SamplingTooAggressivePct != 16.7 {
		t.Errorf("Totals.SamplingTooAggressivePct = %v, want 16.7", got.Totals.SamplingTooAggressivePct)
	}
}

// TestSpanQuality_AggregationCountsOnlyResourcesAboveMinimumInvocations
// — the inventory reader's QualifyingCount is the §3 minimum
// invocation gate (>= 1000 invocations); resources below the
// minimum don't enter either side of the ratio. A deployment where
// 1 of 1 below-minimum resources fires the recommendation surfaces
// 0% (the recommendation is suppressed by the statistical noise
// floor anyway). This test pins that contract — the percentage
// math doesn't double-count noise.
func TestSpanQuality_AggregationCountsOnlyResourcesAboveMinimumInvocations(t *testing.T) {
	idx := newStubQualityIndex()
	reader := &stubSamplingInventoryReader{counts: map[string]SamplingProviderCount{
		// AWS: 5 qualifying resources, 1 fires → 20%.
		// The reader filters out below-minimum resources before
		// reporting; the handler trusts the reader's gate.
		"aws":   {QualifyingCount: 5, TooAggressiveCount: 1},
		"gcp":   {},
		"azure": {},
		"oci":   {},
	}}
	r, h := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	h.SetSamplingInventoryReader(reader)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	var got SpanQualityResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Providers["aws"].SamplingTooAggressivePct != 20.0 {
		t.Errorf("aws.SamplingTooAggressivePct = %v, want 20.0", got.Providers["aws"].SamplingTooAggressivePct)
	}
	// Totals reflect the same — only AWS contributes to the
	// denominator (the other three providers have zero qualifying).
	if got.Totals.SamplingTooAggressivePct != 20.0 {
		t.Errorf("Totals.SamplingTooAggressivePct = %v, want 20.0", got.Totals.SamplingTooAggressivePct)
	}
}

// TestSpanQuality_NoSamplingInventoryReader_SamplingPctZero — cold-
// start posture for deployments that haven't wired the per-cloud
// MetricQuerier substrate. Nil reader leaves every sampling
// percentage at 0; the existing quality-derived percentages still
// flow through unchanged.
func TestSpanQuality_NoSamplingInventoryReader_SamplingPctZero(t *testing.T) {
	idx := newStubQualityIndex(
		traceindex.QualityCountersSnapshot{Key: "aws:1:a", Provider: "aws", TotalSpans: 100, OrphanPct: 4.0},
	)
	r, _ := newSpanQualityRouter(t, idx, nil, nil, 0, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/discovery/span_quality", nil))
	var got SpanQualityResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Providers["aws"].SamplingTooAggressivePct != 0 {
		t.Errorf("aws.SamplingTooAggressivePct = %v, want 0 (no reader wired)", got.Providers["aws"].SamplingTooAggressivePct)
	}
	if got.Totals.SamplingTooAggressivePct != 0 {
		t.Errorf("Totals.SamplingTooAggressivePct = %v, want 0", got.Totals.SamplingTooAggressivePct)
	}
	// Existing quality-derived pct unchanged.
	if got.Providers["aws"].OrphanPct != 4.0 {
		t.Errorf("aws.OrphanPct = %v, want 4.0", got.Providers["aws"].OrphanPct)
	}
}
