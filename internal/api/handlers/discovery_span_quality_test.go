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
