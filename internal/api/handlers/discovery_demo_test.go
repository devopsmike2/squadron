package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// TestHandleAWSRunScan_DemoServesSampleInventory verifies the demo short-
// circuit: a scan against the reserved demo connection returns the canned
// sample inventory with NO credstore or scanner factory wired (the demo check
// runs before either is touched).
func TestHandleAWSRunScan_DemoServesSampleInventory(t *testing.T) {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	w := doScanRequest(h, demo.SentinelAccountID, "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp awsScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.AccountID != demo.SentinelAccountID {
		t.Errorf("account_id = %q, want %q", resp.AccountID, demo.SentinelAccountID)
	}
	if len(resp.Compute) != 5 {
		t.Errorf("compute rows = %d, want 5", len(resp.Compute))
	}
	if len(resp.Functions) != 3 {
		t.Errorf("function rows = %d, want 3", len(resp.Functions))
	}
	if len(resp.Databases) != 2 {
		t.Errorf("database rows = %d, want 2", len(resp.Databases))
	}
}

func doDemoRequest(h *DiscoveryHandlers, method, path string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	r := gin.New()
	r.Handle(method, path, handler)
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestHandleDemoEnable_PersistsConnection verifies enable stores the reserved
// demo connection with the store-required non-empty credential bytes.
func TestHandleDemoEnable_PersistsConnection(t *testing.T) {
	store := &spyStore{}
	h := NewDiscoveryHandlers(store, zap.NewNop())

	w := doDemoRequest(h, http.MethodPost, "/api/v1/discovery/demo/enable", h.HandleDemoEnable)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(store.stored) != 1 {
		t.Fatalf("stored connections = %d, want 1", len(store.stored))
	}
	got := store.stored[0]
	if got.AccountID != demo.SentinelAccountID {
		t.Errorf("stored AccountID = %q, want %q", got.AccountID, demo.SentinelAccountID)
	}
	if len(got.Credentials) == 0 || len(got.CredentialsNonce) == 0 {
		t.Error("stored demo connection has empty credential bytes; store would reject it")
	}
}

// TestHandleDemoDisable_OK verifies disable returns 200 (DeleteConnection is
// idempotent in the spy).
func TestHandleDemoDisable_OK(t *testing.T) {
	store := &spyStore{}
	h := NewDiscoveryHandlers(store, zap.NewNop())

	w := doDemoRequest(h, http.MethodDelete, "/api/v1/discovery/demo", h.HandleDemoDisable)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleDemoEnable_NoStore503s verifies the belt-and-braces nil-store guard.
func TestHandleDemoEnable_NoStore(t *testing.T) {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	w := doDemoRequest(h, http.MethodPost, "/api/v1/discovery/demo/enable", h.HandleDemoEnable)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when store unwired", w.Code)
	}
}
