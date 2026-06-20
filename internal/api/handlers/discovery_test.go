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

	"github.com/devopsmike2/squadron/internal/ai"
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

// TestHandleAWSValidate_NoCredentialsReturnsHumanizedError covers the
// v0.85.0 SEV2 regression: when Squadron itself has no AWS credentials
// configured (no env vars, no shared config file, not on EC2), the
// validator surfaces a NoCredentials humanized error through
// ValidationResult.AssumeRoleErr — the handler must return 200 with
// the humanized payload (the wizard's "what just happened" panel
// renders it verbatim) and the response must arrive in well under the
// handler's 60s safety budget. Pre-fix the call hung for 30+ seconds.
func TestHandleAWSValidate_NoCredentialsReturnsHumanizedError(t *testing.T) {
	mv := &mockValidator{
		result: &scanner.ValidationResult{
			AssumeRoleOK: false,
			AssumeRoleErr: &scanner.HumanizedError{
				Code:          "no_credentials",
				Message:       "Squadron has no AWS credentials configured. Set AWS_REGION + AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY in Squadron's environment, or run Squadron on an EC2/ECS/EKS instance with an IAM role attached.",
				SuggestedStep: "role-arn",
				DocLink:       "https://docs.aws.amazon.com/sdkref/latest/guide/standardized-credentials.html",
			},
		},
	}
	h := newTestHandlers(t, mv)
	body := `{"role_arn":"arn:aws:iam::123456789012:role/SquadronDiscovery","external_id":"xid","regions":["us-east-1"],"account_id":"123456789012"}`

	start := time.Now()
	w := doRequest(h, body)
	elapsed := time.Since(start)

	// The handler returns 200 even on AssumeRole failure — the
	// failure is in the typed body, not the HTTP status. The wizard
	// reads `assume_role_ok=false` and `assume_role_err` from the
	// payload.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Hard upper bound: comfortably below the 60s safety budget AND
	// below the pre-fix 30s hang. A mock validator returns
	// instantly, so anything beyond a couple seconds indicates the
	// handler grew a synchronous block.
	if elapsed > 6*time.Second {
		t.Fatalf("validate handler took %v with a mock validator; pre-fix bug returning?", elapsed)
	}
	if !mv.called {
		t.Fatalf("validator was not called")
	}

	var resp struct {
		AssumeRoleOK  bool `json:"assume_role_ok"`
		AssumeRoleErr *struct {
			Code          string `json:"code"`
			Message       string `json:"message"`
			SuggestedStep string `json:"suggested_step"`
			DocLink       string `json:"doc_link"`
		} `json:"assume_role_err"`
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body did not decode: %v body=%s", err, w.Body.String())
	}
	if resp.AssumeRoleOK {
		t.Errorf("assume_role_ok = true, want false on no-credentials path")
	}
	if resp.AssumeRoleErr == nil {
		t.Fatalf("assume_role_err missing from response; body=%s", w.Body.String())
	}
	if resp.AssumeRoleErr.Code != "no_credentials" {
		t.Errorf("AssumeRoleErr.Code = %q, want %q", resp.AssumeRoleErr.Code, "no_credentials")
	}
	if !strings.Contains(resp.AssumeRoleErr.Message, "AWS_ACCESS_KEY_ID") {
		t.Errorf("AssumeRoleErr.Message should name the env vars: %q", resp.AssumeRoleErr.Message)
	}
	if !strings.Contains(resp.AssumeRoleErr.Message, "EC2/ECS/EKS") {
		t.Errorf("AssumeRoleErr.Message should mention the EC2/ECS/EKS instance-role alternative: %q", resp.AssumeRoleErr.Message)
	}
	if resp.AssumeRoleErr.SuggestedStep != "role-arn" {
		t.Errorf("SuggestedStep = %q, want role-arn", resp.AssumeRoleErr.SuggestedStep)
	}
	// The top-level errors[] convenience field must also carry the
	// humanized error so the UI's flat-list rendering picks it up
	// without re-walking the typed struct.
	if len(resp.Errors) == 0 {
		t.Errorf("top-level errors[] should include the assume-role failure; body=%s", w.Body.String())
	} else if resp.Errors[0].Code != "no_credentials" {
		t.Errorf("errors[0].Code = %q, want no_credentials", resp.Errors[0].Code)
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

func TestHandleAWSListConnections_SurfacesConnectionID(t *testing.T) {
	// Regression guard for #581 (v0.87.2): the list endpoint omitted
	// connection_id even though the save response and the scan endpoint
	// both use it as the canonical handle. The UI had to infer
	// connection_id from account_id, which works today because the
	// substrate has no separate UUID — but the wire shape was
	// asymmetric and a future substrate change would silently break
	// scan URLs.
	//
	// This test pins:
	//   1. connection_id is present on every row,
	//   2. its value equals account_id (today's substrate invariant),
	//   3. the redaction posture still holds — no role_arn, no
	//      external_id, no credentials material in the response.
	now := time.Now().UTC()
	rows := []*credstore.CloudConnection{
		{
			AccountID:        "111111111111",
			Provider:         credstore.ProviderAWS,
			ConnectionType:   credstore.ConnectionAPIDiscovered,
			DisplayName:      "Prod AWS",
			Regions:          []string{"us-east-1"},
			Credentials:      []byte("ciphertext-one"),
			CredentialsNonce: []byte("nonce-one"),
			CreatedAt:        now,
		},
		{
			AccountID:        "222222222222",
			Provider:         credstore.ProviderAWS,
			ConnectionType:   credstore.ConnectionAPIDiscovered,
			DisplayName:      "Staging AWS",
			Regions:          []string{"us-west-2"},
			Credentials:      []byte("ciphertext-two"),
			CredentialsNonce: []byte("nonce-two"),
			CreatedAt:        now,
		},
	}
	store := &listSpyStore{listResult: rows}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	w := doListRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Connections []struct {
			ConnectionID string `json:"connection_id"`
			AccountID    string `json:"account_id"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if got := len(resp.Connections); got != 2 {
		t.Fatalf("connections length = %d, want 2; body=%s", got, w.Body.String())
	}

	// (1) connection_id non-empty on every row, (2) equal to account_id.
	for i, row := range resp.Connections {
		if row.ConnectionID == "" {
			t.Errorf("row[%d].connection_id is empty; the list endpoint must surface it so the UI can build /connections/:id/scan URLs", i)
		}
		if row.ConnectionID != row.AccountID {
			t.Errorf("row[%d].connection_id = %q, want %q (account_id); substrate has no separate UUID today",
				i, row.ConnectionID, row.AccountID)
		}
	}

	// (3) Redaction must still hold. Adding connection_id does not
	// loosen the posture; the list response still must NOT contain any
	// credential material. A future change that widens the row with a
	// sensitive field (role_arn, external_id, credentials_v2, ...) will
	// fail here.
	body := w.Body.String()
	for _, forbidden := range []string{
		"role_arn",
		"external_id",
		"credentials",
		"credentials_ciphertext",
		"credentials_nonce",
		"ciphertext-one",
		"ciphertext-two",
		"nonce-one",
		"nonce-two",
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
	// doc's audit-invariants section names. Slice 2 added
	// database_count; slice 3a (v0.88.0) added object_store_count +
	// load_balancer_count. All count fields are mandatory and
	// always present on the wire — they do NOT drop out via
	// omitempty even when the inventory is empty.
	payloadJSON, _ := json.Marshal(completed.Payload)
	for _, want := range []string{
		"account_id",
		"scan_id",
		"compute_count",
		"function_count",
		"database_count",
		"object_store_count",
		"load_balancer_count",
		"instrumented_count",
		"uninstrumented_count",
		"partial",
	} {
		if !strings.Contains(string(payloadJSON), want) {
			t.Errorf("scan_completed payload missing %q: %s", want, payloadJSON)
		}
	}
}

// TestHandleAWSRunScan_AuditEmitsObjectStoreAndLoadBalancerCounts
// pins the slice 3a (v0.88.0) audit-shape extension — the
// scan_completed event payload now carries object_store_count and
// load_balancer_count as MANDATORY fields. They always emit (even
// when the corresponding inventory is empty) so an operator
// skimming the audit log sees the slice 3a categories' coverage in
// the same place as compute/function/database counts.
//
// Regression discipline: if a future refactor moves these into the
// conditional-insert path (partial_reason / failed_services
// style), the audit log loses operator-visible counts for empty
// inventories.
func TestHandleAWSRunScan_AuditEmitsObjectStoreAndLoadBalancerCounts(t *testing.T) {
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

	// One S3 bucket + two ALBs in the inventory so the counts are
	// distinguishable from zero. The instrumentation flags don't
	// matter for this test — the assertion is on the count keys,
	// not on coverage tallies.
	scanResult := &scanner.Result{
		ScanID:          "test-scan-s3-alb-counts",
		ScanStartedAt:   now,
		ScanCompletedAt: now.Add(2 * time.Second),
		Provider:        credstore.ProviderAWS,
		AccountID:       "123456789012",
		Regions:         []string{"us-east-1"},
		ObjectStores: []scanner.ObjectStoreSnapshot{
			{ResourceID: "prod-logs", Region: "us-east-1", ServerAccessLoggingEnabled: true},
		},
		LoadBalancers: []scanner.LoadBalancerSnapshot{
			{ResourceID: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/a/x", Name: "a", Type: "application", Scheme: "internet-facing", AccessLogsEnabled: true, AccessLogsS3Bucket: "prod-logs", Region: "us-east-1"},
			{ResourceID: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/b/y", Name: "b", Type: "application", Scheme: "internal", AccessLogsEnabled: false, Region: "us-east-1"},
		},
		InstrumentedCount:   2,
		UninstrumentedCount: 1,
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

	if got := len(audit.entries); got != 2 {
		t.Fatalf("audit entries = %d, want 2", got)
	}
	completed := audit.entries[1]
	if completed.EventType != "discovery.aws.scan_completed" {
		t.Fatalf("entry[1].event_type = %q", completed.EventType)
	}

	// Both new count keys are present and carry the expected
	// values. payload is map[string]any; the values are typed int.
	gotObj, ok := completed.Payload["object_store_count"]
	if !ok {
		t.Fatalf("scan_completed payload missing object_store_count key: %+v", completed.Payload)
	}
	if got, _ := gotObj.(int); got != 1 {
		t.Errorf("object_store_count = %v, want 1", gotObj)
	}
	gotLB, ok := completed.Payload["load_balancer_count"]
	if !ok {
		t.Fatalf("scan_completed payload missing load_balancer_count key: %+v", completed.Payload)
	}
	if got, _ := gotLB.(int); got != 2 {
		t.Errorf("load_balancer_count = %v, want 2", gotLB)
	}
}

// TestHandleAWSRunScan_AuditEmitsPartialReasonAndFailedServices_S3
// is the slice 3a (v0.88.0) S3 failure parallel to the v0.87.3 RDS
// audit emission test. When the s3 walk fails, FailedServices
// carries ["s3"] and PartialReason carries the formatted
// explanation — same shape audit consumers pattern-match against.
func TestHandleAWSRunScan_AuditEmitsPartialReasonAndFailedServices_S3(t *testing.T) {
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
		ScanID:              "test-scan-partial-s3",
		ScanStartedAt:       now,
		ScanCompletedAt:     now.Add(2 * time.Second),
		Provider:            credstore.ProviderAWS,
		AccountID:           "123456789012",
		Regions:             []string{"us-east-1"},
		InstrumentedCount:   0,
		UninstrumentedCount: 0,
		Partial:             true,
		PartialReason:       "s3 scan failed in us-east-1: AccessDenied",
		FailedServices:      []string{"s3"},
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

	completed := audit.entries[1]
	gotReason, ok := completed.Payload["partial_reason"]
	if !ok {
		t.Fatalf("scan_completed payload missing partial_reason key: %+v", completed.Payload)
	}
	if got, want := gotReason, "s3 scan failed in us-east-1: AccessDenied"; got != want {
		t.Errorf("partial_reason = %v, want %q", got, want)
	}
	gotServices, ok := completed.Payload["failed_services"]
	if !ok {
		t.Fatalf("scan_completed payload missing failed_services key: %+v", completed.Payload)
	}
	gotSlice, ok := gotServices.([]string)
	if !ok {
		t.Fatalf("failed_services = %T (%v), want []string", gotServices, gotServices)
	}
	if len(gotSlice) != 1 || gotSlice[0] != "s3" {
		t.Errorf("failed_services = %v, want [\"s3\"]", gotSlice)
	}
}

// TestHandleAWSRunScan_AuditEmitsPartialReasonAndFailedServices_ALB
// is the slice 3a (v0.88.0) ALB failure parallel. Same shape as
// the s3 + rds parallels above.
func TestHandleAWSRunScan_AuditEmitsPartialReasonAndFailedServices_ALB(t *testing.T) {
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
		ScanID:              "test-scan-partial-alb",
		ScanStartedAt:       now,
		ScanCompletedAt:     now.Add(2 * time.Second),
		Provider:            credstore.ProviderAWS,
		AccountID:           "123456789012",
		Regions:             []string{"us-east-1"},
		InstrumentedCount:   0,
		UninstrumentedCount: 0,
		Partial:             true,
		PartialReason:       "alb scan failed in us-east-1: AccessDenied",
		FailedServices:      []string{"alb"},
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

	completed := audit.entries[1]
	gotServices, ok := completed.Payload["failed_services"]
	if !ok {
		t.Fatalf("scan_completed payload missing failed_services key: %+v", completed.Payload)
	}
	gotSlice, ok := gotServices.([]string)
	if !ok {
		t.Fatalf("failed_services = %T (%v), want []string", gotServices, gotServices)
	}
	if len(gotSlice) != 1 || gotSlice[0] != "alb" {
		t.Errorf("failed_services = %v, want [\"alb\"]", gotSlice)
	}
}

// TestHandleAWSRunScan_AuditEmitsPartialReasonAndFailedServices pins the
// v0.87.3 audit-shape widening — the scan_completed event payload MUST
// surface BOTH the human-readable partial_reason and the structured
// failed_services list when the scanner returns Partial=true. The bug
// surfaced by Track 3 prerequisite verification (task #584) was that
// audit consumers (SIEM forwarders, Timeline UI, squadronctl, the
// proposer's future scan-history learning loop) could see partial:true
// but had no way to identify which service caused the partial scan.
//
// Regression discipline: if this test fails because the handler
// regressed to omitting either field, the audit log loses operator-
// visible failure attribution — a v0.87.3-shape contract violation.
func TestHandleAWSRunScan_AuditEmitsPartialReasonAndFailedServices(t *testing.T) {
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

	// Live reproducer shape: rds:DescribeDBInstances revoked from the
	// SquadronDiscoveryReadOnly inline policy. Scan returns
	// Partial=true with the rds walk's failure as PartialReason and
	// "rds" as the only FailedServices entry. Mirrors the
	// scanner.go emission site at the rds branch of the per-region
	// loop.
	scanResult := &scanner.Result{
		ScanID:              "test-scan-partial-rds",
		ScanStartedAt:       now,
		ScanCompletedAt:     now.Add(2 * time.Second),
		Provider:            credstore.ProviderAWS,
		AccountID:           "123456789012",
		Regions:             []string{"us-east-1"},
		InstrumentedCount:   0,
		UninstrumentedCount: 0,
		Partial:             true,
		PartialReason:       "rds scan failed in us-east-1: AccessDenied",
		FailedServices:      []string{"rds"},
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

	if got := len(audit.entries); got != 2 {
		t.Fatalf("audit entries = %d, want 2", got)
	}
	completed := audit.entries[1]
	if completed.EventType != "discovery.aws.scan_completed" {
		t.Fatalf("entry[1].event_type = %q", completed.EventType)
	}

	// Pin both fields. partial_reason carries the operator-visible
	// string; failed_services carries the structured list audit
	// consumers pattern-match against.
	gotReason, ok := completed.Payload["partial_reason"]
	if !ok {
		t.Fatalf("scan_completed payload missing partial_reason key: %+v", completed.Payload)
	}
	if got, want := gotReason, "rds scan failed in us-east-1: AccessDenied"; got != want {
		t.Errorf("partial_reason = %v, want %q", got, want)
	}

	gotServices, ok := completed.Payload["failed_services"]
	if !ok {
		t.Fatalf("scan_completed payload missing failed_services key: %+v", completed.Payload)
	}
	gotSlice, ok := gotServices.([]string)
	if !ok {
		t.Fatalf("failed_services = %T (%v), want []string", gotServices, gotServices)
	}
	if len(gotSlice) != 1 || gotSlice[0] != "rds" {
		t.Errorf("failed_services = %v, want [\"rds\"]", gotSlice)
	}

	// partial itself stays true, mirroring the existing invariant.
	if got, _ := completed.Payload["partial"].(bool); !got {
		t.Errorf("partial = %v, want true", completed.Payload["partial"])
	}
}

// TestHandleAWSRunScan_AuditOmitsPartialFieldsOnHappyPath pins the
// v0.87.4 omitempty parity — the scan_completed payload is a
// map[string]any which does NOT honor JSON-tag omitempty (only struct
// fields do). The handler conditionally inserts partial_reason and
// failed_services so happy-path events emit only the mandatory fields
// and don't ship `partial_reason: ""` + `failed_services: null` line
// noise on every successful scan. Mirrors the HTTP response's
// omitempty behavior. Regression guard: if a future refactor goes
// back to an unconditional map literal, audit consumers regain the
// asymmetric shape between happy + failure scans.
func TestHandleAWSRunScan_AuditOmitsPartialFieldsOnHappyPath(t *testing.T) {
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

	// Happy-path scan: empty inventory, no partial flag, empty
	// PartialReason, nil FailedServices. The handler must NOT insert
	// the empty/nil values into the audit payload — they should be
	// absent entirely.
	scanResult := &scanner.Result{
		ScanID:              "test-scan-happy",
		ScanStartedAt:       now,
		ScanCompletedAt:     now.Add(1 * time.Second),
		Provider:            credstore.ProviderAWS,
		AccountID:           "123456789012",
		Regions:             []string{"us-east-1"},
		InstrumentedCount:   0,
		UninstrumentedCount: 0,
		Partial:             false,
		// PartialReason and FailedServices left as zero values
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

	if got := len(audit.entries); got != 2 {
		t.Fatalf("audit entries = %d, want 2", got)
	}
	completed := audit.entries[1]
	if completed.EventType != "discovery.aws.scan_completed" {
		t.Fatalf("entry[1].event_type = %q", completed.EventType)
	}

	// Both optional fields MUST be absent from the payload — not
	// present-with-empty-value.
	if _, ok := completed.Payload["partial_reason"]; ok {
		t.Errorf("partial_reason key present on happy path (value=%v); should be absent", completed.Payload["partial_reason"])
	}
	if _, ok := completed.Payload["failed_services"]; ok {
		t.Errorf("failed_services key present on happy path (value=%v); should be absent", completed.Payload["failed_services"])
	}

	// partial itself stays false, mirroring the existing invariant.
	if got, _ := completed.Payload["partial"].(bool); got {
		t.Errorf("partial = %v, want false", completed.Payload["partial"])
	}
}

// --- HandleAWSGenerateRecommendations tests (Stream 2F) --------------

// mockAIProposer records the context it was handed and returns a
// pre-canned ProposalResult. Lets the recommendations handler tests
// exercise the convert/validate/walk path without touching the
// Anthropic SDK.
type mockAIProposer struct {
	called bool
	gotCtx *ai.DiscoveryScanContext
	result *ai.ProposalResult
	err    error
}

func (m *mockAIProposer) ProposeFromDiscoveryScan(_ context.Context, in *ai.DiscoveryScanContext) (*ai.ProposalResult, error) {
	m.called = true
	m.gotCtx = in
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

// sampleRecsScanResultBody returns a JSON body the recommendations
// handler will accept — a minimal scan_result with the same account_id
// the test uses on the path.
func sampleRecsScanResultBody(accountID string) string {
	body := map[string]any{
		"scan_result": map[string]any{
			"scan_id":              "scan-test-uuid",
			"scan_started_at":      time.Now().UTC().Format(time.RFC3339),
			"scan_completed_at":    time.Now().UTC().Format(time.RFC3339),
			"account_id":           accountID,
			"provider":             "aws",
			"regions":              []string{"us-east-1"},
			"compute":              []map[string]any{{"resource_id": "i-aaa", "instance_type": "t3.micro", "tags": map[string]string{}, "has_otel": false, "os_family": "linux", "region": "us-east-1"}},
			"functions":            []map[string]any{{"resource_id": "arn:aws:lambda:us-east-1:123:function:hello", "name": "hello", "runtime": "python3.11", "has_otel_layer": false, "region": "us-east-1"}},
			"instrumented_count":   0,
			"uninstrumented_count": 2,
			"partial":              false,
		},
	}
	buf, _ := json.Marshal(body)
	return string(buf)
}

func doRecsRequest(h *DiscoveryHandlers, accountID, body string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/api/v1/discovery/aws/connections/:id/recommendations", h.HandleAWSGenerateRecommendations)
	url := "/api/v1/discovery/aws/connections/" + accountID + "/recommendations"
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

// newRecsHandlers wires the recommendations handler with a stored
// connection (so the credstore lookup hits), an AI proposer stub, and
// an audit recorder. Tests adjust the proposer's pre-canned result
// per-scenario.
func newRecsHandlers(t *testing.T, conn *credstore.CloudConnection, mp *mockAIProposer, audit services.AuditService) *DiscoveryHandlers {
	t.Helper()
	store := &spyStore{getResult: conn}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	h.WithAIProposer(mp)
	if audit != nil {
		h.WithAuditService(audit)
	}
	return h
}

func TestHandleAWSGenerateRecommendations_BadRequest(t *testing.T) {
	mp := &mockAIProposer{}
	conn := &credstore.CloudConnection{AccountID: "123456789012", Provider: credstore.ProviderAWS}
	h := newRecsHandlers(t, conn, mp, nil)
	w := doRecsRequest(h, "123456789012", `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if mp.called {
		t.Errorf("proposer should not be called on malformed body")
	}
}

func TestHandleAWSGenerateRecommendations_AccountMismatch(t *testing.T) {
	mp := &mockAIProposer{}
	conn := &credstore.CloudConnection{AccountID: "123456789012", Provider: credstore.ProviderAWS}
	h := newRecsHandlers(t, conn, mp, nil)
	// URL :id = 123456789012; scan_result.account_id = 999... — mismatch.
	body := sampleRecsScanResultBody("999999999999")
	w := doRecsRequest(h, "123456789012", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AccountIDMismatch") && !strings.Contains(w.Body.String(), "does not match") {
		t.Errorf("response should explain the mismatch: %s", w.Body.String())
	}
	if mp.called {
		t.Errorf("proposer should not be called when account_id mismatches")
	}
}

func TestHandleAWSGenerateRecommendations_ConnectionNotFound(t *testing.T) {
	mp := &mockAIProposer{}
	// spyStore.getResult is nil by default → "no row matches".
	store := &spyStore{}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	h.WithAIProposer(mp)
	w := doRecsRequest(h, "999999999999", sampleRecsScanResultBody("999999999999"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if mp.called {
		t.Errorf("proposer should not be called for an unknown connection")
	}
}

func TestHandleAWSGenerateRecommendations_Declined(t *testing.T) {
	mp := &mockAIProposer{
		result: &ai.ProposalResult{
			Declined: true,
			Reason:   "Every scanned resource already has OTel coverage.",
			Kind:     ai.ProposalKindPlan,
		},
	}
	conn := &credstore.CloudConnection{AccountID: "123456789012", Provider: credstore.ProviderAWS}
	audit := &discoveryRecordingAudit{}
	h := newRecsHandlers(t, conn, mp, audit)
	w := doRecsRequest(h, "123456789012", sampleRecsScanResultBody("123456789012"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Declined        bool   `json:"declined"`
		Reason          string `json:"reason"`
		Recommendations []any  `json:"recommendations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if !resp.Declined {
		t.Errorf("response.declined should be true")
	}
	if !strings.Contains(resp.Reason, "already has OTel coverage") {
		t.Errorf("reason not surfaced: %q", resp.Reason)
	}
	if len(resp.Recommendations) != 0 {
		t.Errorf("recommendations should be empty on declined; got %d", len(resp.Recommendations))
	}

	// No recommendations_generated audit event when nothing was
	// generated. The proposer call WAS recorded by the mock (called
	// flag), but no audit row should fire from the handler.
	for _, e := range audit.entries {
		if e.EventType == "discovery.aws.recommendations_generated" {
			t.Errorf("recommendations_generated event should not fire on declined; got %+v", e)
		}
	}
}

func TestHandleAWSGenerateRecommendations_HappyPath(t *testing.T) {
	// Two-step plan with real-looking Terraform per step. The
	// audit-payload-leak assertion below checks the snippet text does
	// NOT appear in the marshaled audit payload — the most important
	// invariant of this endpoint.
	const tfStep0 = `resource "aws_lambda_function" "hello" {
  function_name = "hello"
  layers = ["arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-python-amd64-ver-1-21-0:1"]
}
`
	const tfStep1 = `resource "aws_ssm_association" "adot_install" {
  name = "AWS-RunShellScript"
}
`
	mp := &mockAIProposer{
		result: &ai.ProposalResult{
			Declined:  false,
			Kind:      ai.ProposalKindPlan,
			Reasoning: "Two Lambdas plus one EC2 instance lack OTel. Stage Lambda first, then EC2.",
			Plan: ai.PlanCandidate{
				Steps: []ai.PlanStepCandidate{
					{
						Name:                "AI plan step 0: instrument 2 Lambda functions with OpenTelemetry layer",
						GroupID:             "123456789012",
						InlineConfigSnippet: tfStep0,
						RequireApproval:     true,
						Stages:              []ai.RolloutStageCandidate{{Mode: "percent", Percentage: 100, DwellSeconds: 0}},
						AbortCriteria:       ai.AbortCriteriaCandidate{MaxDriftedAgents: 5, MaxErrorLogsPerMinute: 50, MinDwellSecondsBeforeAbort: 120},
					},
					{
						Name:                "AI plan step 1: instrument 1 EC2 instance with ADOT collector",
						GroupID:             "123456789012",
						InlineConfigSnippet: tfStep1,
						Stages:              []ai.RolloutStageCandidate{{Mode: "percent", Percentage: 100, DwellSeconds: 0}},
						AbortCriteria:       ai.AbortCriteriaCandidate{MaxDriftedAgents: 5, MaxErrorLogsPerMinute: 50, MinDwellSecondsBeforeAbort: 120},
					},
				},
			},
			Evidence:  []ai.EvidenceRefCandidate{{Kind: "audit_event", ID: "scan-test-uuid"}},
			Model:     "claude-sonnet-4-6",
			TokensIn:  123,
			TokensOut: 456,
		},
	}
	conn := &credstore.CloudConnection{AccountID: "123456789012", Provider: credstore.ProviderAWS}
	audit := &discoveryRecordingAudit{}
	h := newRecsHandlers(t, conn, mp, audit)
	w := doRecsRequest(h, "123456789012", sampleRecsScanResultBody("123456789012"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Proposer was called with the converted context.
	if !mp.called {
		t.Fatalf("proposer was not called")
	}
	if mp.gotCtx == nil || mp.gotCtx.AccountID != "123456789012" {
		t.Errorf("proposer received ctx = %+v", mp.gotCtx)
	}
	if mp.gotCtx.ScanID != "scan-test-uuid" {
		t.Errorf("proposer received scan_id = %q", mp.gotCtx.ScanID)
	}
	if len(mp.gotCtx.Functions) != 1 || mp.gotCtx.Functions[0].Runtime != "python3.11" {
		t.Errorf("functions did not round-trip into the AI context: %+v", mp.gotCtx.Functions)
	}
	if len(mp.gotCtx.ComputeInstances) != 1 || mp.gotCtx.ComputeInstances[0].ResourceID != "i-aaa" {
		t.Errorf("compute did not round-trip: %+v", mp.gotCtx.ComputeInstances)
	}

	// Response shape: 2 recommendations, each with the right Source +
	// IaC + Action fields.
	var resp struct {
		Declined        bool   `json:"declined"`
		Reasoning       string `json:"reasoning"`
		Recommendations []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Source struct {
				Kind  string `json:"kind"`
				RefID string `json:"ref_id"`
			} `json:"source"`
			Action struct {
				Kind    string          `json:"kind"`
				Payload json.RawMessage `json:"payload"`
			} `json:"action"`
			IaC struct {
				Format string `json:"format"`
				Source string `json:"source"`
			} `json:"iac"`
		} `json:"recommendations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.Declined {
		t.Errorf("declined should be false on happy path")
	}
	if !strings.Contains(resp.Reasoning, "Two Lambdas") {
		t.Errorf("reasoning not surfaced: %q", resp.Reasoning)
	}
	if got := len(resp.Recommendations); got != 2 {
		t.Fatalf("recommendations length = %d, want 2", got)
	}
	r0 := resp.Recommendations[0]
	if r0.Source.Kind != "discovery_scan" {
		t.Errorf("rec[0].source.kind = %q, want discovery_scan", r0.Source.Kind)
	}
	if r0.Source.RefID != "scan-test-uuid" {
		t.Errorf("rec[0].source.ref_id = %q", r0.Source.RefID)
	}
	if r0.IaC.Format != "terraform" {
		t.Errorf("rec[0].iac.format = %q, want terraform", r0.IaC.Format)
	}
	if !strings.Contains(r0.IaC.Source, "aws_lambda_function") {
		t.Errorf("rec[0].iac.source missing Lambda Terraform: %q", r0.IaC.Source)
	}
	if r0.Action.Kind != "plan" {
		t.Errorf("rec[0].action.kind = %q, want plan", r0.Action.Kind)
	}
	if !strings.Contains(string(r0.Action.Payload), "aws_lambda_function") {
		t.Errorf("rec[0].action.payload should include the step JSON: %s", r0.Action.Payload)
	}

	r1 := resp.Recommendations[1]
	if !strings.Contains(r1.IaC.Source, "aws_ssm_association") {
		t.Errorf("rec[1].iac.source missing SSM Terraform: %q", r1.IaC.Source)
	}

	// Audit event fires with the right shape — AND the Terraform
	// content is NOT in the payload. This is the load-bearing
	// invariant of this endpoint: the audit log shouldn't grow with
	// snippet size, AND auditors should not have to scrub
	// customer-cloud Terraform out of compliance exports.
	var generated *services.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].EventType == "discovery.aws.recommendations_generated" {
			generated = &audit.entries[i]
			break
		}
	}
	if generated == nil {
		t.Fatalf("recommendations_generated audit event did not fire; entries = %+v", audit.entries)
	}
	if generated.TargetID != "123456789012" {
		t.Errorf("audit TargetID = %q", generated.TargetID)
	}
	if generated.TargetType != credstore.TargetTypeCloudConnection {
		t.Errorf("audit TargetType = %q", generated.TargetType)
	}
	payloadJSON, _ := json.Marshal(generated.Payload)
	for _, want := range []string{"account_id", "scan_id", "step_count", "tokens_in", "tokens_out"} {
		if !strings.Contains(string(payloadJSON), want) {
			t.Errorf("audit payload missing %q: %s", want, payloadJSON)
		}
	}
	// THE LOAD-BEARING ASSERTION: no Terraform snippet content in the
	// audit payload. A regression that started serializing the step
	// into the payload (e.g. via map[string]any{"plan": result.Plan})
	// would leak the entire Terraform body into every audit row.
	for _, forbidden := range []string{
		"aws_lambda_function",
		"aws_ssm_association",
		"aws-otel-python",
	} {
		if strings.Contains(string(payloadJSON), forbidden) {
			t.Fatalf("Terraform content leaked into audit payload (%q): %s", forbidden, payloadJSON)
		}
	}
}

func TestHandleAWSGenerateRecommendations_AINotWired(t *testing.T) {
	// Direct-struct construction (no WithAIProposer call). The
	// handler should 503 — the trampoline 503s too on the production
	// path, but this guards the struct-literal route.
	store := &spyStore{getResult: &credstore.CloudConnection{AccountID: "123456789012", Provider: credstore.ProviderAWS}}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	w := doRecsRequest(h, "123456789012", sampleRecsScanResultBody("123456789012"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AI assist is not configured") {
		t.Errorf("response should name the AI-not-configured cause: %s", w.Body.String())
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
