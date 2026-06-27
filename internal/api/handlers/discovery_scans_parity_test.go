// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// assertScanHistoryEndpoints exercises the shared list/get behavior against a
// router that already has a cloud's scan-history routes registered. provider is
// the stored ScanRecord.Provider; connID is the route :id (the scope key).
func assertScanHistoryEndpoints(t *testing.T, r *gin.Engine, base, provider, connID string) {
	t.Helper()
	do := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	// List returns the seeded scan, result_json omitted.
	w := do(base + "/" + connID + "/scans")
	if w.Code != http.StatusOK {
		t.Fatalf("[%s] list want 200, got %d: %s", provider, w.Code, w.Body.String())
	}
	var list struct {
		Scans []types.ScanRecord `json:"scans"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("[%s] list unmarshal: %v", provider, err)
	}
	if len(list.Scans) != 1 || list.Scans[0].ScanID != "s1" {
		t.Fatalf("[%s] unexpected list: %+v", provider, list.Scans)
	}
	if list.Scans[0].ResultJSON != "" {
		t.Errorf("[%s] list leaked result_json", provider)
	}

	// Get returns the full inventory under result.
	w = do(base + "/" + connID + "/scans/s1")
	if w.Code != http.StatusOK {
		t.Fatalf("[%s] get want 200, got %d: %s", provider, w.Code, w.Body.String())
	}
	var detail map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &detail)
	if _, ok := detail["result"]; !ok {
		t.Errorf("[%s] get missing result blob", provider)
	}

	// Unknown id 404s.
	if w = do(base + "/" + connID + "/scans/nope"); w.Code != http.StatusNotFound {
		t.Errorf("[%s] unknown want 404, got %d", provider, w.Code)
	}

	// Cross-scope (different :id than the stored scope) 404s.
	if w = do(base + "/other-conn/scans/s1"); w.Code != http.StatusNotFound {
		t.Errorf("[%s] cross-scope want 404, got %d", provider, w.Code)
	}
}

func seededFake(provider, connID string) *fakeScanStore {
	f := newFakeScanStore()
	_ = f.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID: "s1", Provider: provider, ScopeID: connID,
		Summary: map[string]int{"compute": 1}, ResultJSON: `{"scan_id":"s1"}`,
	})
	return f
}

func TestGCPScanHistoryEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewDiscoveryGCPHandlers(nil, zap.NewNop()).WithGCPScanStore(seededFake("gcp", "conn-g"))
	r := gin.New()
	r.GET("/discovery/gcp/connections/:id/scans", h.HandleGCPListScans)
	r.GET("/discovery/gcp/connections/:id/scans/:scanID", h.HandleGCPGetScan)
	assertScanHistoryEndpoints(t, r, "/discovery/gcp/connections", "gcp", "conn-g")

	// Unwired → 503.
	h2 := NewDiscoveryGCPHandlers(nil, zap.NewNop())
	r2 := gin.New()
	r2.GET("/discovery/gcp/connections/:id/scans", h2.HandleGCPListScans)
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discovery/gcp/connections/conn-g/scans", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("gcp unwired want 503, got %d", w.Code)
	}
}

func TestAzureScanHistoryEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewDiscoveryAzureHandlers(nil, zap.NewNop()).WithAzureScanStore(seededFake("azure", "conn-a"))
	r := gin.New()
	r.GET("/discovery/azure/connections/:id/scans", h.HandleAzureListScans)
	r.GET("/discovery/azure/connections/:id/scans/:scanID", h.HandleAzureGetScan)
	assertScanHistoryEndpoints(t, r, "/discovery/azure/connections", "azure", "conn-a")
}

func TestOCIScanHistoryEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewDiscoveryOCIHandlers(nil, zap.NewNop()).WithOCIScanStore(seededFake("oci", "conn-o"))
	r := gin.New()
	r.GET("/discovery/oci/connections/:id/scans", h.HandleOCIListScans)
	r.GET("/discovery/oci/connections/:id/scans/:scanID", h.HandleOCIGetScan)
	assertScanHistoryEndpoints(t, r, "/discovery/oci/connections", "oci", "conn-o")
}
