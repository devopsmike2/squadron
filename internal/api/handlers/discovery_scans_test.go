// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeScanStore is the test-side DiscoveryScanStore.
type fakeScanStore struct {
	mu      sync.Mutex
	saved   []*types.ScanRecord
	byID    map[string]*types.ScanRecord
	listErr error
	getErr  error
}

func newFakeScanStore() *fakeScanStore {
	return &fakeScanStore{byID: map[string]*types.ScanRecord{}}
}

func (f *fakeScanStore) SaveDiscoveryScan(_ context.Context, rec *types.ScanRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *rec
	f.saved = append(f.saved, &cp)
	f.byID[rec.ScanID] = &cp
	return nil
}

func (f *fakeScanStore) ListDiscoveryScans(_ context.Context, provider, scopeID string, _ int) ([]*types.ScanRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*types.ScanRecord
	for _, r := range f.saved {
		if r.Provider == provider && (scopeID == "" || r.ScopeID == scopeID) {
			cp := *r
			cp.ResultJSON = ""
			out = append(out, &cp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

func (f *fakeScanStore) GetDiscoveryScan(_ context.Context, scanID string) (*types.ScanRecord, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.byID[scanID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func scanRouter(h *DiscoveryHandlers) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/discovery/aws/connections/:id/scans", h.HandleAWSListScans)
	r.GET("/discovery/aws/connections/:id/scans/:scanID", h.HandleAWSGetScan)
	return r
}

func TestRecordScan_ProjectsResultFields(t *testing.T) {
	store := newFakeScanStore()
	r := &scanner.Result{
		ScanID:          "scan-1",
		AccountID:       "123456789012",
		Regions:         []string{"us-east-1"},
		ScanStartedAt:   time.Now().Add(-time.Minute),
		ScanCompletedAt: time.Now(),
		Partial:         true,
		PartialReason:   "throttled",
	}
	recordScan(context.Background(), store, zap.NewNop(), "aws", "123456789012", r, []byte(`{"scan_id":"scan-1"}`))
	if len(store.saved) != 1 {
		t.Fatalf("expected 1 saved record, got %d", len(store.saved))
	}
	got := store.saved[0]
	if got.ScanID != "scan-1" || got.Provider != "aws" || got.ScopeID != "123456789012" {
		t.Errorf("bad projection: %+v", got)
	}
	if !got.Partial || got.PartialReason != "throttled" {
		t.Errorf("partial fields not projected: %+v", got)
	}
	if got.ResultJSON != `{"scan_id":"scan-1"}` {
		t.Errorf("result_json not stored: %q", got.ResultJSON)
	}
	if got.Summary == nil {
		t.Errorf("summary not populated")
	}
}

func TestRecordScan_NilStoreNoPanic(t *testing.T) {
	recordScan(context.Background(), nil, zap.NewNop(), "aws", "x", &scanner.Result{ScanID: "x"}, nil)
}

func TestHandleAWSListScans_ReturnsHistory(t *testing.T) {
	store := newFakeScanStore()
	_ = store.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID: "s1", Provider: "aws", ScopeID: "111", Summary: map[string]int{"compute": 2},
		ResultJSON: `{"big":"blob"}`,
	})
	h := NewDiscoveryHandlers(nil, zap.NewNop()).WithScanStore(store)
	w := httptest.NewRecorder()
	scanRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/scans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Scans []types.ScanRecord `json:"scans"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Scans) != 1 || resp.Scans[0].ScanID != "s1" {
		t.Fatalf("unexpected scans: %+v", resp.Scans)
	}
	if resp.Scans[0].ResultJSON != "" {
		t.Errorf("list leaked result_json")
	}
}

func TestHandleAWSListScans_StoreUnwired503(t *testing.T) {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	w := httptest.NewRecorder()
	scanRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/scans", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
}

func TestHandleAWSGetScan_FullInventory(t *testing.T) {
	store := newFakeScanStore()
	_ = store.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID: "s1", Provider: "aws", ScopeID: "111",
		Summary: map[string]int{"compute": 1}, ResultJSON: `{"scan_id":"s1","compute":[{"id":"i-1"}]}`,
	})
	h := NewDiscoveryHandlers(nil, zap.NewNop()).WithScanStore(store)
	w := httptest.NewRecorder()
	scanRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/scans/s1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["result"]; !ok {
		t.Errorf("get did not embed the inventory under result: %s", w.Body.String())
	}
}

func TestHandleAWSGetScan_UnknownID404(t *testing.T) {
	h := NewDiscoveryHandlers(nil, zap.NewNop()).WithScanStore(newFakeScanStore())
	w := httptest.NewRecorder()
	scanRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/scans/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

// A scan whose stored scope differs from the path account must 404 — guards
// against cross-scope ID guessing.
func TestHandleAWSGetScan_CrossScope404(t *testing.T) {
	store := newFakeScanStore()
	_ = store.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID: "s1", Provider: "aws", ScopeID: "999", ResultJSON: `{}`,
	})
	h := NewDiscoveryHandlers(nil, zap.NewNop()).WithScanStore(store)
	w := httptest.NewRecorder()
	scanRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/scans/s1", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for cross-scope, got %d", w.Code)
	}
}

// TestRunScanForAccount_DemoHappyPath: the demo account short-circuits inside
// runAWSScan (no credstore/scanner needed) so the exported scheduler entry
// returns nil.
func TestRunScanForAccount_DemoHappyPath(t *testing.T) {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	if err := h.RunScanForAccount(context.Background(), demo.SentinelAccountID); err != nil {
		t.Fatalf("demo scan should succeed, got %v", err)
	}
}

// TestRunScanForAccount_UnwiredCredstoreErrors: a real account with no credstore
// wired surfaces an error the scheduler will log + count.
func TestRunScanForAccount_UnwiredCredstoreErrors(t *testing.T) {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	if err := h.RunScanForAccount(context.Background(), "123456789012"); err == nil {
		t.Fatal("expected an error when credstore is not wired")
	}
}

func TestHandleAWSScanDrift_LatestTwo(t *testing.T) {
	store := newFakeScanStore()
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	_ = store.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID: "old", Provider: "aws", ScopeID: "111", StartedAt: older,
		ResultJSON: `{"compute":[{"resource_id":"i-1","has_otel":false}]}`,
	})
	_ = store.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID: "new", Provider: "aws", ScopeID: "111", StartedAt: newer,
		ResultJSON: `{"compute":[{"resource_id":"i-1","has_otel":true},{"resource_id":"i-2","has_otel":false}]}`,
	})
	h := NewDiscoveryHandlers(nil, zap.NewNop()).WithScanStore(store)
	r := gin.New()
	r.GET("/discovery/aws/connections/:id/drift", h.HandleAWSScanDrift)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/drift", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		FromScanID string `json:"from_scan_id"`
		ToScanID   string `json:"to_scan_id"`
		Drift      struct {
			TotalAdded                  int `json:"total_added"`
			TotalInstrumentationChanged int `json:"total_instrumentation_changed"`
		} `json:"drift"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.FromScanID != "old" || resp.ToScanID != "new" {
		t.Errorf("from/to = %s/%s, want old/new", resp.FromScanID, resp.ToScanID)
	}
	if resp.Drift.TotalAdded != 1 || resp.Drift.TotalInstrumentationChanged != 1 {
		t.Errorf("drift totals added=%d flips=%d, want 1/1", resp.Drift.TotalAdded, resp.Drift.TotalInstrumentationChanged)
	}
}

func TestHandleAWSScanDrift_InsufficientHistory(t *testing.T) {
	store := newFakeScanStore()
	_ = store.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID: "only", Provider: "aws", ScopeID: "111", StartedAt: time.Now(), ResultJSON: `{}`,
	})
	h := NewDiscoveryHandlers(nil, zap.NewNop()).WithScanStore(store)
	r := gin.New()
	r.GET("/discovery/aws/connections/:id/drift", h.HandleAWSScanDrift)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/drift", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["insufficient_history"] != true {
		t.Errorf("expected insufficient_history=true, got %v", resp)
	}
}

func TestHandleAWSScanDrift_UnwiredStore503(t *testing.T) {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	r := gin.New()
	r.GET("/discovery/aws/connections/:id/drift", h.HandleAWSScanDrift)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/aws/connections/111/drift", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", w.Code)
	}
}
