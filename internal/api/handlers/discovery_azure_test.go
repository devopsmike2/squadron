// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/services"
)

// --- test fixtures ------------------------------------------------------

// azureTestKey32 is a deterministic 32-byte key used to construct
// credstore.NewKey for the Azure handler tests. Real deployments
// supply the key via SQUADRON_SECRETS_KEY; tests inject the fixture
// so each test starts from a known cipher posture.
var azureTestKey32 = []byte("0123456789abcdef0123456789abcdef")

// Canonical UUIDs reused across the Azure handler tests. Picking
// fixed values rather than generating per-test makes the test
// surface deterministic and keeps test failure diffs readable.
const (
	azureTestTenantID       = "11111111-2222-3333-4444-555555555555"
	azureTestSubscriptionID = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	azureTestClientID       = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	azureTestClientSecret   = "Az8Q~fakeNotARealSecretButShapedLike1Z2~zz.~"
)

// encodeAzureSecret base64-encodes the SP client_secret in the wire
// shape the handler expects.
func encodeAzureSecret(secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(secret))
}

// fakeAzureScanner is the in-test scanner.Scanner implementation
// used by the Azure handler tests. Records the call (so tests can
// assert on inputs) and returns a pre-canned Result or error.
//
// A separate type from the GCP fakeScanner so the two surfaces stay
// independent — tests of the Azure handler shouldn't accidentally
// reach for GCP fixtures and vice versa.
type fakeAzureScanner struct {
	mu sync.Mutex
	// scanCalls counts Scan invocations across tests that need to
	// distinguish "scanner ran" from "scanner short-circuited".
	scanCalls int
	// gotRegions captures the regions slice from the most recent
	// Scan call.
	gotRegions []string
	// result is returned on Scan() unless err is set.
	result *scanner.Result
	err    error
	// eventSources is returned by ScanEventSources (event-source tier).
	eventSources []scanner.EventSourceInstanceSnapshot
}

func (f *fakeAzureScanner) Provider() credstore.Provider {
	return credstore.Provider(azureconnstore.ProviderAzure)
}
func (f *fakeAzureScanner) Scan(_ context.Context, _ *credstore.CloudConnection, regions []string) (*scanner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanCalls++
	if regions != nil {
		f.gotRegions = append([]string{}, regions...)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}
func (f *fakeAzureScanner) Validate(_ context.Context, _ *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	return &scanner.ValidationResult{AssumeRoleOK: true}, nil
}

// ScanEventSources satisfies EventSourceDiscoveryScanner so the Azure
// scan handler's tier-gated event-source dispatch (v0.89.195) folds the
// returned snapshots into the response.
func (f *fakeAzureScanner) ScanEventSources(_ context.Context, _ scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	return f.eventSources, nil
}

// fakeAzureScannerFactory satisfies AzureScannerFactory by returning
// a pre-seeded fakeAzureScanner. Records the unsealed client_secret
// bytes the handler passed so tests can assert the unseal happened
// end-to-end without poking at credstore internals.
type fakeAzureScannerFactory struct {
	scanner         *fakeAzureScanner
	buildErr        error
	gotClientSecret []byte
	gotSubscription string
	gotTenant       string
	gotClient       string
	buildCall       int
}

func (f *fakeAzureScannerFactory) Build(conn azureconnstore.AzureConnection, clientSecret []byte) (scanner.Scanner, error) {
	f.buildCall++
	f.gotClientSecret = append([]byte{}, clientSecret...)
	f.gotSubscription = conn.SubscriptionID
	f.gotTenant = conn.TenantID
	f.gotClient = conn.ClientID
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return f.scanner, nil
}

// newAzureTestHandlers builds DiscoveryAzureHandlers wired with the
// in-memory store + a fresh credstore.Key + the supplied audit and
// scanner factory. logger is a no-op so test output stays clean.
func newAzureTestHandlers(t *testing.T, audit services.AuditService, factory AzureScannerFactory) (*DiscoveryAzureHandlers, azureconnstore.Store, *credstore.Key) {
	t.Helper()
	store := azureconnstore.NewMemoryStore()
	key, err := credstore.NewKey(azureTestKey32)
	if err != nil {
		t.Fatalf("credstore.NewKey: %v", err)
	}
	h := NewDiscoveryAzureHandlers(store, zap.NewNop()).
		WithAzureCredstoreKey(key)
	if audit != nil {
		h.WithAzureAuditService(audit)
	}
	if factory != nil {
		h.WithAzureScannerFactory(factory)
	}
	return h, store, key
}

// newAzureRouter wires every Azure route the handler exposes so the
// HTTP-layer integration is exercised end-to-end.
func newAzureRouter(h *DiscoveryAzureHandlers) *gin.Engine {
	r := gin.New()
	r.POST("/api/v1/discovery/azure/connections", h.HandleCreateAzureConnection)
	r.GET("/api/v1/discovery/azure/connections", h.HandleListAzureConnections)
	r.GET("/api/v1/discovery/azure/connections/:id", h.HandleGetAzureConnection)
	r.PATCH("/api/v1/discovery/azure/connections/:id", h.HandleUpdateAzureConnection)
	r.DELETE("/api/v1/discovery/azure/connections/:id", h.HandleDeleteAzureConnection)
	r.POST("/api/v1/discovery/azure/connections/:id/validate", h.HandleValidateAzureConnection)
	r.POST("/api/v1/discovery/azure/connections/:id/scan", h.HandleScanAzureConnection)
	r.POST("/api/v1/discovery/azure/connections/:id/recommendations", h.HandleRecommendationsForAzureScan)
	return r
}

// azureDoRequest is the shared HTTP harness.
func azureDoRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var buf *bytes.Buffer
	if body == "" {
		buf = bytes.NewBuffer(nil)
	} else {
		buf = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Async recommendations (v0.89.210): a successful /recommendations
	// kick-off returns 202 + a job_id; await the job via the shared store
	// and synthesize the response the synchronous handler used to return,
	// so existing assertions hold. Non-202 responses pass through.
	if w.Code != http.StatusAccepted {
		return w
	}
	var acc recommendationJobAcceptedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil || acc.JobID == "" {
		return w
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, ok := defaultRecommendationJobStore.Get(acc.JobID)
		if ok && (job.Status == RecJobSucceeded || job.Status == RecJobFailed) {
			synth := httptest.NewRecorder()
			synth.Header().Set("Content-Type", "application/json")
			if job.Status == RecJobSucceeded {
				synth.Code = http.StatusOK
				synth.Body.Write(job.ResultJSON)
			} else {
				synth.Code = job.HTTPStatus
				eb, _ := json.Marshal(gin.H{"error": job.Err})
				synth.Body.Write(eb)
			}
			return synth
		}
		if time.Now().After(deadline) {
			return w
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// seedAzureConnection inserts an AzureConnection directly via the
// store (bypassing the create handler) so tests of read-side
// endpoints can start from a known row without re-asserting the
// create path.
func seedAzureConnection(t *testing.T, store azureconnstore.Store, key *credstore.Key, displayName, tenantID, subscriptionID, clientID, location string) *azureconnstore.AzureConnection {
	t.Helper()
	sealed, err := credstore.SealAzureClientSecret(key, []byte(azureTestClientSecret))
	if err != nil {
		t.Fatalf("SealAzureClientSecret: %v", err)
	}
	conn := &azureconnstore.AzureConnection{
		DisplayName:                      displayName,
		TenantID:                         tenantID,
		SubscriptionID:                   subscriptionID,
		ClientID:                         clientID,
		SealedSecret:                     sealed,
		Location:                         location,
		LearnFromAcceptedRecommendations: true,
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	return conn
}

// --- Create -------------------------------------------------------------

func TestCreateAzureConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, _ := newAzureTestHandlers(t, audit, nil)
	r := newAzureRouter(h)

	body := `{"display_name":"Prod Azure","tenant_id":"` + azureTestTenantID +
		`","subscription_id":"` + azureTestSubscriptionID +
		`","client_id":"` + azureTestClientID +
		`","sealed_secret":"` + encodeAzureSecret(azureTestClientSecret) +
		`","location":"eastus"}`
	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	// One row persisted, with the right shape.
	conns, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("store rows = %d, want 1", len(conns))
	}
	row := conns[0]
	if row.DisplayName != "Prod Azure" {
		t.Errorf("row.DisplayName = %q", row.DisplayName)
	}
	if row.TenantID != azureTestTenantID {
		t.Errorf("row.TenantID = %q", row.TenantID)
	}
	if row.SubscriptionID != azureTestSubscriptionID {
		t.Errorf("row.SubscriptionID = %q", row.SubscriptionID)
	}
	if row.ClientID != azureTestClientID {
		t.Errorf("row.ClientID = %q", row.ClientID)
	}
	if row.Location != "eastus" {
		t.Errorf("row.Location = %q", row.Location)
	}
	if len(row.SealedSecret) == 0 {
		t.Errorf("row.SealedSecret is empty — seal did not run")
	}

	// Response body MUST NOT carry the sealed_secret bytes.
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "sealed_secret") {
		t.Errorf("response leaked sealed_secret key: %s", bodyStr)
	}
	if strings.Contains(bodyStr, base64.StdEncoding.EncodeToString(row.SealedSecret)) {
		t.Errorf("response leaked sealed_secret value: %s", bodyStr)
	}
	if strings.Contains(bodyStr, azureTestClientSecret) {
		t.Errorf("response leaked plaintext client_secret: %s", bodyStr)
	}

	// One audit entry on the right topic, no secret bytes in the
	// payload.
	if got := len(audit.entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventDiscoveryAzureConnectionCreated {
		t.Errorf("audit EventType = %q, want %q", e.EventType, services.AuditEventDiscoveryAzureConnectionCreated)
	}
	payloadJSON, _ := json.Marshal(e.Payload)
	if strings.Contains(string(payloadJSON), "sealed_secret") {
		t.Errorf("sealed_secret key leaked into audit payload: %s", payloadJSON)
	}
	if strings.Contains(string(payloadJSON), azureTestClientSecret) {
		t.Fatalf("client_secret plaintext leaked into audit payload: %s", payloadJSON)
	}
}

func TestCreateAzureConnection_MissingFields_Returns400(t *testing.T) {
	h, store, _ := newAzureTestHandlers(t, nil, nil)
	r := newAzureRouter(h)

	// Missing subscription_id.
	body := `{"display_name":"Prod","tenant_id":"` + azureTestTenantID +
		`","client_id":"` + azureTestClientID +
		`","sealed_secret":"` + encodeAzureSecret(azureTestClientSecret) +
		`","location":"eastus"}`
	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "subscription ID is required") {
		t.Errorf("missing-subscription-id message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on missing field, got %d rows", len(conns))
	}
}

func TestCreateAzureConnection_InvalidTenantID_Returns400(t *testing.T) {
	h, store, _ := newAzureTestHandlers(t, nil, nil)
	r := newAzureRouter(h)

	// Non-UUID tenant_id is rejected at the handler.
	body := `{"display_name":"Prod","tenant_id":"not-a-uuid","subscription_id":"` + azureTestSubscriptionID +
		`","client_id":"` + azureTestClientID +
		`","sealed_secret":"` + encodeAzureSecret(azureTestClientSecret) +
		`","location":"eastus"}`
	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidTenantID") {
		t.Errorf("invalid-tenant-id message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on invalid tenant_id, got %d rows", len(conns))
	}
}

func TestCreateAzureConnection_InvalidSubscriptionID_Returns400(t *testing.T) {
	h, store, _ := newAzureTestHandlers(t, nil, nil)
	r := newAzureRouter(h)

	// Non-UUID subscription_id is rejected at the handler.
	body := `{"display_name":"Prod","tenant_id":"` + azureTestTenantID +
		`","subscription_id":"definitely-not-a-uuid","client_id":"` + azureTestClientID +
		`","sealed_secret":"` + encodeAzureSecret(azureTestClientSecret) +
		`","location":"eastus"}`
	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidSubscriptionID") {
		t.Errorf("invalid-subscription-id message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on invalid subscription_id, got %d rows", len(conns))
	}
}

// --- List ---------------------------------------------------------------

func TestListAzureConnections_StripsSealedSecret(t *testing.T) {
	h, store, key := newAzureTestHandlers(t, nil, nil)
	r := newAzureRouter(h)

	a := seedAzureConnection(t, store, key, "Alpha", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")
	b := seedAzureConnection(t, store, key, "Beta",
		"00000000-1111-2222-3333-444444444444",
		"55555555-6666-7777-8888-999999999999",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"westus")

	w := azureDoRequest(r, http.MethodGet, "/api/v1/discovery/azure/connections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "sealed_secret") {
		t.Errorf("list response leaked sealed_secret key: %s", body)
	}
	if strings.Contains(body, azureTestClientSecret) {
		t.Errorf("list response leaked plaintext client_secret: %s", body)
	}
	// Both display names visible.
	if !strings.Contains(body, "Alpha") || !strings.Contains(body, "Beta") {
		t.Errorf("list response missing one of the connections: %s", body)
	}
	// IDs round-tripped.
	if !strings.Contains(body, a.ID) || !strings.Contains(body, b.ID) {
		t.Errorf("list response missing one of the IDs: %s", body)
	}
}

// --- Update -------------------------------------------------------------

func TestUpdateAzureConnection_PreservesUntouchedFields(t *testing.T) {
	h, store, key := newAzureTestHandlers(t, nil, nil)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Original", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")
	originalSecret := append([]byte{}, conn.SealedSecret...)

	// Only change display_name; tenant/subscription/client/secret
	// must all stay put.
	patch := `{"display_name":"Renamed"}`
	w := azureDoRequest(r, http.MethodPatch, "/api/v1/discovery/azure/connections/"+conn.ID, patch)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	after, err := store.Get(context.Background(), conn.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if after.DisplayName != "Renamed" {
		t.Errorf("after.DisplayName = %q, want Renamed", after.DisplayName)
	}
	if after.TenantID != azureTestTenantID {
		t.Errorf("tenant_id mutated: %q", after.TenantID)
	}
	if after.SubscriptionID != azureTestSubscriptionID {
		t.Errorf("subscription_id mutated: %q", after.SubscriptionID)
	}
	if after.ClientID != azureTestClientID {
		t.Errorf("client_id mutated: %q", after.ClientID)
	}
	if after.Location != "eastus" {
		t.Errorf("location mutated: %q", after.Location)
	}
	if !bytes.Equal(after.SealedSecret, originalSecret) {
		t.Errorf("SealedSecret mutated; PATCH should never touch sealed bytes")
	}
}

// --- Delete -------------------------------------------------------------

func TestDeleteAzureConnection_RemovesAndAudits(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, key := newAzureTestHandlers(t, audit, nil)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodDelete, "/api/v1/discovery/azure/connections/"+conn.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	if _, err := store.Get(context.Background(), conn.ID); !errors.Is(err, azureconnstore.ErrConnectionNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrConnectionNotFound", err)
	}

	if got := len(audit.entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	if audit.entries[0].EventType != services.AuditEventDiscoveryAzureConnectionDeleted {
		t.Errorf("audit EventType = %q, want %q",
			audit.entries[0].EventType, services.AuditEventDiscoveryAzureConnectionDeleted)
	}
	payload := audit.entries[0].Payload
	if payload["subscription_id"] != azureTestSubscriptionID {
		t.Errorf("audit payload subscription_id = %v, want %s", payload["subscription_id"], azureTestSubscriptionID)
	}
	if payload["tenant_id"] != azureTestTenantID {
		t.Errorf("audit payload tenant_id = %v, want %s", payload["tenant_id"], azureTestTenantID)
	}
}

// --- Validate -----------------------------------------------------------

func TestValidateAzureConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeAzureScanner{
		result: &scanner.Result{
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "vm-1"},
				{ResourceID: "vm-2"},
				{ResourceID: "vm-3"},
			},
		},
	}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, audit, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp azureValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if !resp.OK {
		t.Errorf("ok = false, want true; resp=%+v", resp)
	}
	if resp.InstanceCount != 3 {
		t.Errorf("instance_count = %d, want 3", resp.InstanceCount)
	}
	if factory.buildCall != 1 {
		t.Errorf("factory.Build calls = %d, want 1", factory.buildCall)
	}
	if string(factory.gotClientSecret) != azureTestClientSecret {
		t.Errorf("factory did not receive unsealed client_secret: got %q", string(factory.gotClientSecret))
	}
	if factory.gotSubscription != azureTestSubscriptionID {
		t.Errorf("factory got subscription = %q, want %q", factory.gotSubscription, azureTestSubscriptionID)
	}
	// Per runbook: validate produces no audit signal.
	if got := len(audit.entries); got != 0 {
		t.Errorf("validate should not emit audit events, got %d", got)
	}
}

func TestValidateAzureConnection_PermissionDenied(t *testing.T) {
	fs := &fakeAzureScanner{
		err: errors.New("RESPONSE 403: AuthorizationFailed: client does not have permission to perform action 'Microsoft.Compute/virtualMachines/read' over scope"),
	}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, nil, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp azureValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.OK {
		t.Errorf("ok = true, want false on permission denied")
	}
	if resp.ErrorKind != "permission_denied" {
		t.Errorf("error_kind = %q, want permission_denied", resp.ErrorKind)
	}
}

func TestValidateAzureConnection_CredentialsInvalid(t *testing.T) {
	fs := &fakeAzureScanner{
		// AADSTS7000215 is Azure AD's "invalid client_secret" code.
		err: errors.New("RESPONSE 401: AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value."),
	}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, nil, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp azureValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.OK {
		t.Errorf("ok = true, want false on invalid credentials")
	}
	if resp.ErrorKind != "credentials_invalid" {
		t.Errorf("error_kind = %q, want credentials_invalid", resp.ErrorKind)
	}
}

func TestValidateAzureConnection_SubscriptionNotFound(t *testing.T) {
	fs := &fakeAzureScanner{
		err: errors.New("RESPONSE 404: SubscriptionNotFound: The subscription could not be found"),
	}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, nil, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp azureValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.OK {
		t.Errorf("ok = true, want false on subscription_not_found")
	}
	if resp.ErrorKind != "subscription_not_found" {
		t.Errorf("error_kind = %q, want subscription_not_found", resp.ErrorKind)
	}
}

// --- Scan ---------------------------------------------------------------

func TestScanAzureConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeAzureScanner{
		result: &scanner.Result{
			ScanID: "scan-abc",
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "vm-1", HasOTel: true},
				{ResourceID: "vm-2", HasOTel: true},
				{ResourceID: "vm-3", HasOTel: true},
				{ResourceID: "vm-4", HasOTel: false},
				{ResourceID: "vm-5", HasOTel: false},
			},
			InstrumentedCount:   3,
			UninstrumentedCount: 2,
		},
	}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, audit, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp azureScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.ScanID != "scan-abc" {
		t.Errorf("scan_id = %q, want scan-abc", resp.ScanID)
	}
	if resp.SubscriptionID != azureTestSubscriptionID {
		t.Errorf("subscription_id = %q", resp.SubscriptionID)
	}
	if resp.InstrumentedCount != 3 {
		t.Errorf("instrumented_count = %d, want 3", resp.InstrumentedCount)
	}
	if len(resp.Compute) != 5 {
		t.Errorf("compute rows = %d, want 5", len(resp.Compute))
	}

	// Audit: started + completed, in that order.
	if got := len(audit.entries); got < 2 {
		t.Fatalf("audit entries = %d, want at least 2 (started + completed)", got)
	}
	if audit.entries[0].EventType != services.AuditEventDiscoveryAzureScanStarted {
		t.Errorf("first audit = %q, want scan_started", audit.entries[0].EventType)
	}
	completed := audit.entries[len(audit.entries)-1]
	if completed.EventType != services.AuditEventDiscoveryAzureScanCompleted {
		t.Errorf("last audit = %q, want scan_completed", completed.EventType)
	}
	// Verify the payload carries every field the design doc + brief
	// requires.
	for _, k := range []string{"connection_id", "tenant_id", "subscription_id", "location", "scan_id", "instance_count", "instrumented_count", "uninstrumented_count", "partial"} {
		if _, ok := completed.Payload[k]; !ok {
			t.Errorf("scan_completed payload missing %q: %+v", k, completed.Payload)
		}
	}
	if completed.Payload["instrumented_count"].(int) != 3 {
		t.Errorf("payload.instrumented_count = %v, want 3", completed.Payload["instrumented_count"])
	}
}

func TestScanAzureConnection_PartialFailure_AuditPayloadCarriesPartialReason(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeAzureScanner{
		result: &scanner.Result{
			ScanID: "scan-partial",
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "vm-1", HasOTel: false},
			},
			InstrumentedCount:   0,
			UninstrumentedCount: 1,
			Partial:             true,
			PartialReason:       "rate limit on VirtualMachines.ListAll",
			FailedServices:      []string{"azurevm"},
		},
	}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, audit, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	completed := audit.entries[len(audit.entries)-1]
	if completed.EventType != services.AuditEventDiscoveryAzureScanCompleted {
		t.Fatalf("last audit = %q, want scan_completed", completed.EventType)
	}
	if completed.Payload["partial"] != true {
		t.Errorf("payload.partial = %v, want true", completed.Payload["partial"])
	}
	if completed.Payload["partial_reason"] != "rate limit on VirtualMachines.ListAll" {
		t.Errorf("payload.partial_reason = %v", completed.Payload["partial_reason"])
	}
	fs2, ok := completed.Payload["failed_services"].([]string)
	if !ok || len(fs2) != 1 || fs2[0] != "azurevm" {
		t.Errorf("payload.failed_services = %v, want [azurevm]", completed.Payload["failed_services"])
	}
}

func TestScanAzureConnection_HardError_EmitsScanFailedAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeAzureScanner{err: errors.New("RESPONSE 500: InternalServerError from Azure Resource Manager")}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, audit, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	// Expect started + failed.
	if got := len(audit.entries); got < 2 {
		t.Fatalf("audit entries = %d, want >= 2", got)
	}
	last := audit.entries[len(audit.entries)-1]
	if last.EventType != services.AuditEventDiscoveryAzureScanFailed {
		t.Errorf("last audit = %q, want scan_failed", last.EventType)
	}
	if last.Payload["error_kind"] == nil {
		t.Errorf("scan_failed payload missing error_kind: %+v", last.Payload)
	}
	if last.Payload["humanized_message"] == nil {
		t.Errorf("scan_failed payload missing humanized_message: %+v", last.Payload)
	}
}

// --- Recommendations (chunk 5, v0.89.198) ------------------------------

func TestRecommendationsForAzureScan_HappyPath(t *testing.T) {
	mock := &mockAIProposer{
		result: &ai.ProposalResult{
			Reasoning: "Azure instrumentation plan",
			Model:     "claude-test",
			Plan: ai.PlanCandidate{
				Steps: []ai.PlanStepCandidate{
					{Name: "Preserve traceparent on orders-ns", InlineConfigSnippet: "resource \"azurerm_servicebus_namespace_authorization_rule\" \"r\" {}"},
				},
			},
		},
	}
	audit := &discoveryRecordingAudit{}
	h, store, key := newAzureTestHandlers(t, audit, nil)
	h.WithAzureAIProposer(mock)
	r := newAzureRouter(h)
	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	body, err := json.Marshal(azureGenerateRecommendationsRequest{
		ScanResult: azureScanResponse{
			ScanID:         "scan-azure-recs",
			SubscriptionID: azureTestSubscriptionID,
			Location:       "eastus",
			EventSources: []eventSourceRow{
				{
					Provider: "azure", Surface: "servicebus", SourceType: "namespace",
					ResourceName: "orders-ns", Region: "eastus",
					HasTraceAxis: true, HasLogAxis: true,
					HasPropagationConfig: false,
					PropagationNotes:     []string{"authorization rule blocks traceparent"},
				},
			},
			InstrumentedCount:   0,
			UninstrumentedCount: 1,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/recommendations", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if mock.gotCtx.Provider != "azure" {
		t.Errorf("ctx.Provider = %q, want azure", mock.gotCtx.Provider)
	}
	if mock.gotCtx.SubscriptionID != azureTestSubscriptionID {
		t.Errorf("ctx.SubscriptionID = %q", mock.gotCtx.SubscriptionID)
	}
	if len(mock.gotCtx.EventSources) != 1 || mock.gotCtx.EventSources[0].HasPropagationConfig {
		t.Errorf("event source not threaded with propagation gap: %+v", mock.gotCtx.EventSources)
	}
	var resp awsGenerateRecommendationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(resp.Recommendations) != 1 {
		t.Fatalf("recommendations len = %d, want 1", len(resp.Recommendations))
	}

	var sawProposalCreated bool
	for _, e := range audit.entries {
		if e.EventType == services.AuditEventDiscoveryProposalCreated {
			sawProposalCreated = true
		}
	}
	if !sawProposalCreated {
		t.Error("discovery_proposal.created audit event was not emitted")
	}
}

func TestRecommendationsForAzureScan_ProposerNotWired(t *testing.T) {
	h, store, key := newAzureTestHandlers(t, nil, nil)
	r := newAzureRouter(h)
	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")
	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/recommendations", `{"scan_result":{"scan_id":"s1","subscription_id":"`+azureTestSubscriptionID+`"}}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// --- Trampoline unwired path -------------------------------------------

func TestAzureStoreNotWired_Returns500(t *testing.T) {
	// Construct a bare handler with nil store to exercise the
	// belt-and-braces 500 path. The trampoline's 503 path is
	// exercised by the server-level tests; the handler-level 500 is
	// the struct-literal-construction defense the brief asks for.
	h := NewDiscoveryAzureHandlers(nil, zap.NewNop())
	r := newAzureRouter(h)
	w := azureDoRequest(r, http.MethodGet, "/api/v1/discovery/azure/connections", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AzureStoreNotWired") {
		t.Errorf("expected AzureStoreNotWired code: %s", w.Body.String())
	}
}

// classifyAzureScanError-level coverage on the unhappy-path strings
// the validate / scan_failed audits depend on. Kept as a small unit
// test to anchor the handler-side error_kind mapping.
func TestClassifyAzureScanError(t *testing.T) {
	cases := []struct {
		in   error
		want string
	}{
		{errors.New("RESPONSE 403: AuthorizationFailed"), "permission_denied"},
		{errors.New("forbidden"), "permission_denied"},
		{errors.New("RESPONSE 404: SubscriptionNotFound"), "subscription_not_found"},
		{errors.New("AADSTS90002: Tenant not found"), "tenant_invalid"},
		{errors.New("AADSTS7000215: Invalid client secret"), "credentials_invalid"},
		{errors.New("RESPONSE 401: unauthorized"), "credentials_invalid"},
		{errors.New("dial tcp: connection refused"), "network"},
		{errors.New("something else entirely"), "unknown"},
		{nil, ""},
	}
	for i, tc := range cases {
		got := classifyAzureScanError(tc.in)
		if got != tc.want {
			t.Errorf("case %d (%v): got %q, want %q", i, tc.in, got, tc.want)
		}
	}
	// Avoid unused-import lint when time imports aren't otherwise hit.
	_ = time.Now
}

// TestScanAzureConnection_SurfacesEventSources pins the v0.89.195
// event-source-tier wiring for Azure Service Bus, including the
// slice-2 propagation axis.
func TestScanAzureConnection_SurfacesEventSources(t *testing.T) {
	fs := &fakeAzureScanner{
		result: &scanner.Result{
			ScanID:              "scan-es-azure",
			InstrumentedCount:   1,
			UninstrumentedCount: 0,
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "vm-1", HasOTel: true},
			},
		},
		eventSources: []scanner.EventSourceInstanceSnapshot{
			{
				Provider: "azure", Surface: "servicebus", SourceType: "namespace",
				ResourceName:         "orders-ns",
				ResourceARN:          "/subscriptions/" + azureTestSubscriptionID + "/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/orders-ns",
				Region:               "eastus",
				HasTraceAxis:         true,
				HasLogAxis:           true,
				HasPropagationConfig: false,
				PropagationNotes:     []string{"authorization rule restricts ApplicationProperties, blocking traceparent"},
			},
		},
	}
	factory := &fakeAzureScannerFactory{scanner: fs}
	h, store, key := newAzureTestHandlers(t, nil, factory)
	r := newAzureRouter(h)

	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")
	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp azureScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if len(resp.EventSources) != 1 {
		t.Fatalf("event_sources len = %d, want 1; body=%s", len(resp.EventSources), w.Body.String())
	}
	es := resp.EventSources[0]
	if es.ResourceName != "orders-ns" {
		t.Errorf("resource_name = %q, want orders-ns", es.ResourceName)
	}
	if es.HasPropagationConfig {
		t.Errorf("has_propagation_config = true, want false (propagation gap)")
	}
	if len(es.PropagationNotes) != 1 {
		t.Errorf("propagation_notes len = %d, want 1", len(es.PropagationNotes))
	}
}
