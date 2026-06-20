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
