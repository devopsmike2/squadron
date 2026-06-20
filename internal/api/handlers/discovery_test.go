// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/services"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// mockValidator records the connection it was handed and returns a
// pre-canned ValidationResult. Lets the handler tests verify that the
// request body was correctly transformed into a CloudConnection
// without ever touching the AWS SDK.
type mockValidator struct {
	called   bool
	gotCreds credstore.AWSCredentials
	gotConn  *credstore.CloudConnection
	result   *scanner.ValidationResult
	err      error
}

func (m *mockValidator) Validate(_ context.Context, conn *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	m.called = true
	m.gotConn = conn
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &scanner.ValidationResult{AssumeRoleOK: true}, nil
}

// newTestHandlers builds DiscoveryHandlers wired against the supplied
// mock validator. The credstore is nil (the validate endpoint should
// never need it); the logger is a no-op.
func newTestHandlers(_ *testing.T, mv *mockValidator) *DiscoveryHandlers {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	h.WithAWSValidatorFactory(func(creds credstore.AWSCredentials, _ string) DiscoveryValidator {
		mv.gotCreds = creds
		return mv
	})
	return h
}

// doRequest is the shared POST harness for these tests. Returns the
// recorder so each test inspects status + body.
func doRequest(h *DiscoveryHandlers, body string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/api/v1/discovery/aws/validate", h.HandleAWSValidate)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/aws/validate", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleAWSValidate_BadRequest(t *testing.T) {
	h := newTestHandlers(t, &mockValidator{})
	// Completely malformed JSON.
	w := doRequest(h, `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "could not be parsed") {
		t.Errorf("body should explain the parse failure: %s", w.Body.String())
	}
}

func TestHandleAWSValidate_MissingRoleARN(t *testing.T) {
	mv := &mockValidator{}
	h := newTestHandlers(t, mv)
	body := `{"external_id":"abc","regions":["us-east-1"]}`
	w := doRequest(h, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if mv.called {
		t.Fatalf("validator should not have been called when role_arn is missing")
	}
	if !strings.Contains(w.Body.String(), "Role ARN is required") {
		t.Errorf("missing-role-ARN message not surfaced: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"suggested_step":"role-arn"`) && !strings.Contains(w.Body.String(), `"SuggestedStep":"role-arn"`) {
		// HumanizedError has no json tags, so fields land
		// capitalised. The wizard reads both cases; the test
		// accepts either to avoid being a JSON-shape test.
		t.Errorf("suggested_step pointer not surfaced: %s", w.Body.String())
	}
}

func TestHandleAWSValidate_MissingExternalID(t *testing.T) {
	mv := &mockValidator{}
	h := newTestHandlers(t, mv)
	body := `{"role_arn":"arn:aws:iam::123:role/x","regions":["us-east-1"]}`
	w := doRequest(h, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "External ID is required") {
		t.Errorf("missing-external-id message not surfaced: %s", w.Body.String())
	}
}

func TestHandleAWSValidate_ScannerCalled(t *testing.T) {
	mv := &mockValidator{
		result: &scanner.ValidationResult{
			AssumeRoleOK: true,
			Preflight: []scanner.PreflightCheck{
				{Service: "ec2", OK: true, SampleCount: 3},
				{Service: "lambda", OK: true, SampleCount: 1},
			},
		},
	}
	h := newTestHandlers(t, mv)
	body := `{"role_arn":"arn:aws:iam::123456789012:role/SquadronDiscovery","external_id":"xid","regions":["us-east-1"],"account_id":"123456789012"}`
	w := doRequest(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !mv.called {
		t.Fatalf("validator was not called")
	}
	if mv.gotCreds.RoleARN != "arn:aws:iam::123456789012:role/SquadronDiscovery" {
		t.Errorf("RoleARN not forwarded: %q", mv.gotCreds.RoleARN)
	}
	if mv.gotCreds.ExternalID != "xid" {
		t.Errorf("ExternalID not forwarded: %q", mv.gotCreds.ExternalID)
	}
	if mv.gotConn == nil {
		t.Fatalf("transient connection not forwarded")
	}
	if mv.gotConn.Provider != credstore.ProviderAWS {
		t.Errorf("Provider on conn = %q, want %q", mv.gotConn.Provider, credstore.ProviderAWS)
	}
	if mv.gotConn.AccountID != "123456789012" {
		t.Errorf("AccountID on conn = %q", mv.gotConn.AccountID)
	}
	if len(mv.gotConn.Regions) != 1 || mv.gotConn.Regions[0] != "us-east-1" {
		t.Errorf("Regions on conn = %+v, want [us-east-1]", mv.gotConn.Regions)
	}
	// Response shape sanity — assume_role_ok must round-trip and
	// the preflight rows must be present.
	var resp struct {
		AssumeRoleOK bool `json:"assume_role_ok"`
		Preflight    []struct {
			Service     string `json:"service"`
			OK          bool   `json:"ok"`
			SampleCount int    `json:"sample_count"`
		} `json:"preflight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body did not decode: %v body=%s", err, w.Body.String())
	}
	if !resp.AssumeRoleOK {
		t.Errorf("assume_role_ok = false, want true")
	}
	if len(resp.Preflight) != 2 {
		t.Errorf("preflight rows = %d, want 2", len(resp.Preflight))
	}
}

// --- HandleAWSSaveConnection tests ----------------------------------

// spyStore records the connection it was asked to persist. The Save
// handler tests assert on the captured row to verify the right fields
// (account_id, provider, regions, credentials ciphertext) reach
// StoreConnection — without needing a real SQLite-backed substrate.
type spyStore struct {
	mu        sync.Mutex
	stored    []credstore.CloudConnection
	storeErr  error
	getResult *credstore.CloudConnection
	getErr    error
}

func (s *spyStore) StoreConnection(_ context.Context, conn credstore.CloudConnection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.storeErr != nil {
		return s.storeErr
	}
	s.stored = append(s.stored, conn)
	return nil
}

func (s *spyStore) GetConnection(_ context.Context, _ string) (*credstore.CloudConnection, error) {
	return s.getResult, s.getErr
}

func (s *spyStore) ListConnections(_ context.Context, _ credstore.ListFilter) ([]*credstore.CloudConnection, error) {
	return nil, nil
}

func (s *spyStore) DeleteConnection(_ context.Context, _ string) error { return nil }
func (s *spyStore) Close() error                                       { return nil }

// discoveryRecordingAudit captures every audit entry. Used to verify the Save
// handler emits discovery.aws.connection_created on the happy path
// and that the ExternalId never appears in the payload.
type discoveryRecordingAudit struct {
	mu      sync.Mutex
	entries []services.AuditEntry
}

func (r *discoveryRecordingAudit) Record(_ context.Context, e services.AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *discoveryRecordingAudit) List(_ context.Context, _ services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return nil, nil
}

func (r *discoveryRecordingAudit) Get(_ context.Context, _ string) (*services.AuditEvent, error) {
	return nil, nil
}

func (r *discoveryRecordingAudit) SetExplanation(_ context.Context, _, _, _ string, _ time.Time) error {
	return nil
}

// passthroughMarshaller is the test-side AWSCredMarshaller — it
// JSON-encodes the creds as the "ciphertext" and returns a fixed
// "nonce" so the test can assert against the bytes that reached the
// store without actually invoking the AEAD.
func passthroughMarshaller(creds credstore.AWSCredentials) ([]byte, []byte, error) {
	plain, err := json.Marshal(creds)
	if err != nil {
		return nil, nil, err
	}
	return plain, []byte("test-nonce"), nil
}

// newSaveHandlers builds DiscoveryHandlers wired with the supplied
// store + audit + validator + marshaller. All four are required for
// the happy-path Save test; failure-path tests can pass nils for the
// pieces they're not exercising.
func newSaveHandlers(t *testing.T, store credstore.Store, mv *mockValidator, audit services.AuditService) *DiscoveryHandlers {
	t.Helper()
	h := NewDiscoveryHandlers(store, zap.NewNop())
	h.WithAWSValidatorFactory(func(creds credstore.AWSCredentials, _ string) DiscoveryValidator {
		mv.gotCreds = creds
		return mv
	})
	h.WithCredMarshaller(passthroughMarshaller)
	if audit != nil {
		h.WithAuditService(audit)
	}
	return h
}

func doSaveRequest(h *DiscoveryHandlers, body string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/api/v1/discovery/aws/connections", h.HandleAWSSaveConnection)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/aws/connections", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleAWSSaveConnection_BadRequest(t *testing.T) {
	store := &spyStore{}
	mv := &mockValidator{}
	h := newSaveHandlers(t, store, mv, nil)
	w := doSaveRequest(h, `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "could not be parsed") {
		t.Errorf("parse-failure message not surfaced: %s", w.Body.String())
	}
	if len(store.stored) != 0 {
		t.Errorf("store should be empty on bad request, got %d rows", len(store.stored))
	}
	if mv.called {
		t.Errorf("validator should not have been called on a malformed body")
	}
}

func TestHandleAWSSaveConnection_MissingFields(t *testing.T) {
	store := &spyStore{}
	mv := &mockValidator{}
	h := newSaveHandlers(t, store, mv, nil)

	// Missing role_arn — every other field present.
	body := `{"account_id":"123456789012","external_id":"xid","display_name":"Prod","regions":["us-east-1"]}`
	w := doSaveRequest(h, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Role ARN is required") {
		t.Errorf("missing-role-arn message not surfaced: %s", w.Body.String())
	}
	if mv.called {
		t.Errorf("validator should not have been called when role_arn is missing")
	}
	if len(store.stored) != 0 {
		t.Errorf("store should be empty on missing field, got %d rows", len(store.stored))
	}
}

func TestHandleAWSSaveConnection_ValidationFails(t *testing.T) {
	store := &spyStore{}
	// Validator returns AssumeRoleErr — the design contract says the
	// scanner returns a typed result rather than a Go error for
	// operator-recoverable failures.
	mv := &mockValidator{
		result: &scanner.ValidationResult{
			AssumeRoleOK: false,
			AssumeRoleErr: &scanner.HumanizedError{
				Code:          "AccessDenied",
				Message:       "trust policy does not authorize Squadron",
				SuggestedStep: "trust-policy",
			},
		},
	}
	audit := &discoveryRecordingAudit{}
	h := newSaveHandlers(t, store, mv, audit)
	body := `{"account_id":"123456789012","role_arn":"arn:aws:iam::123456789012:role/SquadronDiscovery","external_id":"xid","display_name":"Prod","regions":["us-east-1"]}`
	w := doSaveRequest(h, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !mv.called {
		t.Errorf("validator should have been called before persistence")
	}
	if len(store.stored) != 0 {
		t.Errorf("store should be empty when validation fails, got %d rows", len(store.stored))
	}
	if len(audit.entries) != 0 {
		t.Errorf("no audit event should fire when validation fails, got %d", len(audit.entries))
	}
	if !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("AccessDenied code not surfaced: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "trust-policy") {
		t.Errorf("suggested_step pointer not surfaced: %s", w.Body.String())
	}
}

func TestHandleAWSSaveConnection_HappyPath(t *testing.T) {
	store := &spyStore{}
	mv := &mockValidator{
		result: &scanner.ValidationResult{
			AssumeRoleOK: true,
			Preflight: []scanner.PreflightCheck{
				{Service: "ec2", OK: true, SampleCount: 3},
				{Service: "lambda", OK: true, SampleCount: 1},
			},
		},
	}
	audit := &discoveryRecordingAudit{}
	h := newSaveHandlers(t, store, mv, audit)
	body := `{"account_id":"123456789012","role_arn":"arn:aws:iam::123456789012:role/SquadronDiscovery","external_id":"super-secret-xid","display_name":"Prod AWS account","regions":["us-east-1"]}`
	w := doSaveRequest(h, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	// One row persisted, with the right shape.
	if got := len(store.stored); got != 1 {
		t.Fatalf("store rows = %d, want 1", got)
	}
	row := store.stored[0]
	if row.AccountID != "123456789012" {
		t.Errorf("row.AccountID = %q", row.AccountID)
	}
	if row.Provider != credstore.ProviderAWS {
		t.Errorf("row.Provider = %q", row.Provider)
	}
	if row.ConnectionType != credstore.ConnectionAPIDiscovered {
		t.Errorf("row.ConnectionType = %q", row.ConnectionType)
	}
	if row.DisplayName != "Prod AWS account" {
		t.Errorf("row.DisplayName = %q", row.DisplayName)
	}
	if len(row.Regions) != 1 || row.Regions[0] != "us-east-1" {
		t.Errorf("row.Regions = %+v", row.Regions)
	}
	if len(row.Credentials) == 0 {
		t.Errorf("row.Credentials is empty — the marshaller did not run")
	}
	if string(row.CredentialsNonce) != "test-nonce" {
		t.Errorf("row.CredentialsNonce = %q, want test-nonce", string(row.CredentialsNonce))
	}

	// One audit entry, no ExternalId in the payload.
	if got := len(audit.entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	e := audit.entries[0]
	if e.EventType != "discovery.aws.connection_created" {
		t.Errorf("audit EventType = %q", e.EventType)
	}
	if e.TargetID != "123456789012" {
		t.Errorf("audit TargetID = %q", e.TargetID)
	}
	if e.TargetType != credstore.TargetTypeCloudConnection {
		t.Errorf("audit TargetType = %q, want %q", e.TargetType, credstore.TargetTypeCloudConnection)
	}
	// The single most load-bearing assertion: no ExternalId leak.
	payloadJSON, _ := json.Marshal(e.Payload)
	if strings.Contains(string(payloadJSON), "super-secret-xid") {
		t.Fatalf("ExternalId leaked into audit payload: %s", payloadJSON)
	}
	if strings.Contains(string(payloadJSON), "external_id") {
		t.Fatalf("external_id key present in audit payload: %s", payloadJSON)
	}
	// account_id, role_arn, regions, display_name MUST be present.
	for _, want := range []string{"account_id", "role_arn", "regions", "display_name"} {
		if !strings.Contains(string(payloadJSON), want) {
			t.Errorf("audit payload missing %q: %s", want, payloadJSON)
		}
	}

	// Response shape: {connection_id, status:"connected"}.
	var resp struct {
		ConnectionID string `json:"connection_id"`
		Status       string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response decode: %v body=%s", err, w.Body.String())
	}
	if resp.ConnectionID != "123456789012" {
		t.Errorf("connection_id = %q", resp.ConnectionID)
	}
	if resp.Status != "connected" {
		t.Errorf("status = %q", resp.Status)
	}
}

// --- HandleAWSListConnections tests ---------------------------------

// listSpyStore extends spyStore with a configurable ListConnections
// response. The Save tests use spyStore.ListConnections to return nil,
// nil; the list tests need to inject a stored set of rows and verify
// the response shape.
type listSpyStore struct {
	spyStore
	listResult []*credstore.CloudConnection
	listErr    error
	listFilter credstore.ListFilter
}

func (s *listSpyStore) ListConnections(_ context.Context, f credstore.ListFilter) ([]*credstore.CloudConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listFilter = f
	return s.listResult, s.listErr
}

func doListRequest(h *DiscoveryHandlers) *httptest.ResponseRecorder {
	r := gin.New()
	r.GET("/api/v1/discovery/aws/connections", h.HandleAWSListConnections)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery/aws/connections", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleAWSListConnections_Empty(t *testing.T) {
	store := &listSpyStore{listResult: nil}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	w := doListRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The filter must scope to AWS — slice 1's list endpoint is the
	// AWS-only view, even though the substrate stores any provider.
	if store.listFilter.Provider != credstore.ProviderAWS {
		t.Errorf("ListConnections filter.Provider = %q, want %q",
			store.listFilter.Provider, credstore.ProviderAWS)
	}
	// Empty array, NOT null. The UI's empty-state branch keys off
	// .length === 0 — a literal null would break it.
	var resp struct {
		Connections []json.RawMessage `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.Connections == nil {
		t.Fatalf("connections is null; want empty array")
	}
	if len(resp.Connections) != 0 {
		t.Errorf("connections length = %d, want 0", len(resp.Connections))
	}
	// The literal bytes must contain "connections":[] (not :null).
	if !strings.Contains(w.Body.String(), `"connections":[]`) {
		t.Errorf("response should contain 'connections':[]; got %s", w.Body.String())
	}
}

func TestHandleAWSListConnections_Populated(t *testing.T) {
	now := time.Now().UTC()
	rows := []*credstore.CloudConnection{
		{
			AccountID:        "123456789012",
			Provider:         credstore.ProviderAWS,
			ConnectionType:   credstore.ConnectionAPIDiscovered,
			DisplayName:      "Prod AWS",
			Regions:          []string{"us-east-1"},
			Credentials:      []byte("super-secret-ciphertext"),
			CredentialsNonce: []byte("secret-nonce"),
			CreatedAt:        now,
		},
		{
			AccountID:        "987654321098",
			Provider:         credstore.ProviderAWS,
			ConnectionType:   credstore.ConnectionAPIDiscovered,
			DisplayName:      "Staging AWS",
			Regions:          []string{"us-west-2", "eu-west-1"},
			Credentials:      []byte("another-secret-ciphertext"),
			CredentialsNonce: []byte("another-nonce"),
			CreatedAt:        now,
		},
	}
	store := &listSpyStore{listResult: rows}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	w := doListRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Display fields must round-trip.
	var resp struct {
		Connections []struct {
			AccountID   string   `json:"account_id"`
			DisplayName string   `json:"display_name"`
			Regions     []string `json:"regions"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if got := len(resp.Connections); got != 2 {
		t.Fatalf("connections length = %d, want 2; body=%s", got, w.Body.String())
	}
	if resp.Connections[0].AccountID != "123456789012" {
		t.Errorf("row[0].account_id = %q", resp.Connections[0].AccountID)
	}
	if resp.Connections[0].DisplayName != "Prod AWS" {
		t.Errorf("row[0].display_name = %q", resp.Connections[0].DisplayName)
	}
	if resp.Connections[1].AccountID != "987654321098" {
		t.Errorf("row[1].account_id = %q", resp.Connections[1].AccountID)
	}

	// THE LOAD-BEARING ASSERTION: no role-ARN-shaped, no
	// external-id-shaped, no credentials-shaped fields in the response.
	// Operators see "this account is connected"; they cannot read back
	// credential material. Grep the literal response bytes — a future
	// addition that names a new field "credentials_v2" or similar will
	// still be caught.
	body := w.Body.String()
	for _, forbidden := range []string{
		"role_arn",
		"external_id",
		"credentials",
		"credentials_ciphertext",
		"credentials_nonce",
		"super-secret-ciphertext",
		"another-secret-ciphertext",
		"secret-nonce",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("response contains forbidden token %q: %s", forbidden, body)
		}
	}
}

// --- HandleAWSRunScan tests -----------------------------------------

// mockScanner records the args it was handed and returns the
// pre-canned Result / err. Lets the run-scan handler tests exercise
// the audit invariants and response shape without ever calling AWS.
type mockScanner struct {
	called    bool
	gotConn   *credstore.CloudConnection
	gotRegs   []string
	result    *scanner.Result
	scanErr   error
	buildErr  error
	buildArgs *credstore.CloudConnection
}

func (m *mockScanner) Scan(_ context.Context, conn *credstore.CloudConnection, regions []string) (*scanner.Result, error) {
	m.called = true
	m.gotConn = conn
	m.gotRegs = regions
	if m.scanErr != nil {
		return nil, m.scanErr
	}
	return m.result, nil
}

func doScanRequest(h *DiscoveryHandlers, accountID, body string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/api/v1/discovery/aws/connections/:id/scan", h.HandleAWSRunScan)
	url := "/api/v1/discovery/aws/connections/" + accountID + "/scan"
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, url, nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleAWSRunScan_NotFound(t *testing.T) {
	// spyStore.GetConnection returns (nil, nil) by default — the
	// "no row matches" contract from credstore.Store.
	store := &spyStore{}
	ms := &mockScanner{}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	h.WithAWSScannerFactory(func(_ *credstore.CloudConnection) (DiscoveryScanner, error) {
		return ms, nil
	})
	audit := &discoveryRecordingAudit{}
	h.WithAuditService(audit)

	w := doScanRequest(h, "999999999999", `{"regions":["us-east-1"]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if ms.called {
		t.Errorf("scanner should not have been called for an unknown connection")
	}
	// scan_started fires on intent — the operator's request reached the
	// handler. But scan_completed must NOT fire when the lookup failed.
	// Mirrors the design doc's "scan_started without scan_completed
	// implies failure" invariant. Slice 1's NotFound path is a 404 with
	// neither event — the missing connection is not an operator intent
	// to scan an existent account.
	if len(audit.entries) != 0 {
		t.Errorf("expected zero audit entries for a not-found lookup; got %d", len(audit.entries))
	}
}

func TestHandleAWSRunScan_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	conn := &credstore.CloudConnection{
		AccountID:      "123456789012",
		Provider:       credstore.ProviderAWS,
		ConnectionType: credstore.ConnectionAPIDiscovered,
		DisplayName:    "Prod AWS",
		Regions:        []string{"us-east-1"},
		Credentials:    []byte("ciphertext"),
		CreatedAt:      now,
	}
	store := &spyStore{getResult: conn}

	scanResult := &scanner.Result{
		ScanID:          "test-scan-uuid",
		ScanStartedAt:   now,
		ScanCompletedAt: now.Add(2 * time.Second),
		Provider:        credstore.ProviderAWS,
		AccountID:       "123456789012",
		Regions:         []string{"us-east-1"},
		Compute: []scanner.ComputeInstanceSnapshot{
			{
				ResourceID:   "i-aaa",
				InstanceType: "t3.micro",
				Tags:         map[string]string{"Name": "web-1"},
				HasOTel:      true,
				OSFamily:     "linux",
				Region:       "us-east-1",
			},
			{
				ResourceID:   "i-bbb",
				InstanceType: "m5.large",
				Tags:         map[string]string{"Name": "db-1"},
				HasOTel:      false,
				OSFamily:     "linux",
				Region:       "us-east-1",
			},
		},
		Functions: []scanner.FunctionRuntimeSnapshot{
			{
				ResourceID:   "arn:aws:lambda:us-east-1:123:function:hello",
				Name:         "hello",
				Runtime:      "python3.11",
				HasOTelLayer: false,
				Region:       "us-east-1",
			},
		},
		InstrumentedCount:   1,
		UninstrumentedCount: 2,
	}
	ms := &mockScanner{result: scanResult}
	audit := &discoveryRecordingAudit{}

	h := NewDiscoveryHandlers(store, zap.NewNop())
	h.WithAWSScannerFactory(func(c *credstore.CloudConnection) (DiscoveryScanner, error) {
		ms.buildArgs = c
		return ms, nil
	})
	h.WithAuditService(audit)

	w := doScanRequest(h, "123456789012", `{"regions":["us-east-1"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Scanner ran with the right args.
	if !ms.called {
		t.Fatalf("scanner was not called on the happy path")
	}
	if ms.gotConn == nil || ms.gotConn.AccountID != "123456789012" {
		t.Errorf("scanner received conn = %+v", ms.gotConn)
	}
	if len(ms.gotRegs) != 1 || ms.gotRegs[0] != "us-east-1" {
		t.Errorf("scanner received regions = %+v", ms.gotRegs)
	}

	// Response shape carries the snake_case fields and the per-row
	// data the Inventory tab will render.
	var resp struct {
		ScanID    string `json:"scan_id"`
		AccountID string `json:"account_id"`
		Compute   []struct {
			ResourceID   string `json:"resource_id"`
			InstanceType string `json:"instance_type"`
			HasOTel      bool   `json:"has_otel"`
		} `json:"compute"`
		Functions []struct {
			Name         string `json:"name"`
			Runtime      string `json:"runtime"`
			HasOTelLayer bool   `json:"has_otel_layer"`
		} `json:"functions"`
		InstrumentedCount   int  `json:"instrumented_count"`
		UninstrumentedCount int  `json:"uninstrumented_count"`
		Partial             bool `json:"partial"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.ScanID != "test-scan-uuid" {
		t.Errorf("scan_id = %q", resp.ScanID)
	}
	if resp.AccountID != "123456789012" {
		t.Errorf("account_id = %q", resp.AccountID)
	}
	if len(resp.Compute) != 2 {
		t.Errorf("compute rows = %d, want 2", len(resp.Compute))
	} else {
		if resp.Compute[0].ResourceID != "i-aaa" {
			t.Errorf("compute[0].resource_id = %q", resp.Compute[0].ResourceID)
		}
		if !resp.Compute[0].HasOTel {
			t.Errorf("compute[0].has_otel should be true")
		}
	}
	if len(resp.Functions) != 1 {
		t.Errorf("function rows = %d, want 1", len(resp.Functions))
	} else if resp.Functions[0].Name != "hello" {
		t.Errorf("functions[0].name = %q", resp.Functions[0].Name)
	}
	if resp.InstrumentedCount != 1 {
		t.Errorf("instrumented_count = %d, want 1", resp.InstrumentedCount)
	}
	if resp.UninstrumentedCount != 2 {
		t.Errorf("uninstrumented_count = %d, want 2", resp.UninstrumentedCount)
	}

	// Both audit events fired, in order, with the right payloads.
	if got := len(audit.entries); got != 2 {
		t.Fatalf("audit entries = %d, want 2", got)
	}
	started := audit.entries[0]
	completed := audit.entries[1]
	if started.EventType != "discovery.aws.scan_started" {
		t.Errorf("entry[0].event_type = %q", started.EventType)
	}
	if started.TargetID != "123456789012" {
		t.Errorf("entry[0].target_id = %q", started.TargetID)
	}
	if completed.EventType != "discovery.aws.scan_completed" {
		t.Errorf("entry[1].event_type = %q", completed.EventType)
	}
	if completed.TargetID != "123456789012" {
		t.Errorf("entry[1].target_id = %q", completed.TargetID)
	}
	// scan_completed payload carries the slice-1 counts the design
	// doc's audit-invariants section names.
	payloadJSON, _ := json.Marshal(completed.Payload)
	for _, want := range []string{
		"account_id",
		"scan_id",
		"compute_count",
		"function_count",
		"instrumented_count",
		"uninstrumented_count",
		"partial",
	} {
		if !strings.Contains(string(payloadJSON), want) {
			t.Errorf("scan_completed payload missing %q: %s", want, payloadJSON)
		}
	}
}

func TestHandleAWSValidate_ZeroPreflightOnAssumeFailure(t *testing.T) {
	// When assume-role fails, the handler should still 200 — the
	// validation result's typed AssumeRoleErr is the wizard's signal
	// to highlight the failing step, not an HTTP error code.
	mv := &mockValidator{
		result: &scanner.ValidationResult{
			AssumeRoleOK: false,
			AssumeRoleErr: &scanner.HumanizedError{
				Code:          "AccessDenied",
				Message:       "trust policy",
				SuggestedStep: "trust-policy",
			},
		},
	}
	h := newTestHandlers(t, mv)
	body := `{"role_arn":"arn:aws:iam::1:role/x","external_id":"y","regions":["us-east-1"]}`
	w := doRequest(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on assume-role failure; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("assume-role err not surfaced: %s", w.Body.String())
	}
}
