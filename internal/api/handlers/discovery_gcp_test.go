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
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/services"
)

// --- test fixtures ------------------------------------------------------

// gcpTestKey32 is a deterministic 32-byte key used to construct
// credstore.NewKey for the GCP handler tests. Real deployments
// supply the key via SQUADRON_SECRETS_KEY; tests inject the fixture
// so each test starts from a known cipher posture.
var gcpTestKey32 = []byte("0123456789abcdef0123456789abcdef")

// validSAJSON returns a Service Account JSON payload pinned to the
// given project ID. Mirrors the GCP-issued shape closely enough for
// validateGCPServiceAccountJSON + extractGCPSAProjectID to read it.
func validSAJSON(projectID string) []byte {
	payload := map[string]any{
		"type":         "service_account",
		"project_id":   projectID,
		"private_key":  "-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----\n",
		"client_email": "squadron-discovery@" + projectID + ".iam.gserviceaccount.com",
	}
	out, _ := json.Marshal(payload)
	return out
}

// encodeSA base64-encodes the SA JSON in the wire shape the handler
// expects.
func encodeSA(saJSON []byte) string {
	return base64.StdEncoding.EncodeToString(saJSON)
}

// fakeScanner is the in-test scanner.Scanner implementation. Records
// the call (so tests can assert on inputs) and returns a pre-canned
// Result or error.
type fakeScanner struct {
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

func (f *fakeScanner) Provider() credstore.Provider {
	return credstore.Provider(gcpconnstore.ProviderGCP)
}
func (f *fakeScanner) Scan(_ context.Context, _ *credstore.CloudConnection, regions []string) (*scanner.Result, error) {
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
func (f *fakeScanner) Validate(_ context.Context, _ *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	return &scanner.ValidationResult{AssumeRoleOK: true}, nil
}

// ScanEventSources satisfies EventSourceDiscoveryScanner so the GCP
// scan handler's tier-gated event-source dispatch (v0.89.195) can fold
// the returned snapshots into the response.
func (f *fakeScanner) ScanEventSources(_ context.Context, _ scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	return f.eventSources, nil
}

// fakeGCPScannerFactory satisfies GCPScannerFactory by returning a
// pre-seeded fakeScanner. Records the unsealed SA bytes the handler
// passed so tests can assert the unseal happened end-to-end without
// poking at credstore internals.
type fakeGCPScannerFactory struct {
	scanner   *fakeScanner
	buildErr  error
	gotSA     []byte
	gotProj   string
	buildCall int
}

func (f *fakeGCPScannerFactory) Build(conn *gcpconnstore.GCPConnection, saJSON []byte) (scanner.Scanner, error) {
	f.buildCall++
	f.gotSA = append([]byte{}, saJSON...)
	if conn != nil {
		f.gotProj = conn.ProjectID
	}
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return f.scanner, nil
}

// newGCPTestHandlers builds DiscoveryGCPHandlers wired with the
// in-memory store + a fresh credstore.Key + the supplied audit and
// scanner factory. logger is a no-op so test output stays clean.
func newGCPTestHandlers(t *testing.T, audit services.AuditService, factory GCPScannerFactory) (*DiscoveryGCPHandlers, gcpconnstore.Store, *credstore.Key) {
	t.Helper()
	store := gcpconnstore.NewMemoryStore()
	key, err := credstore.NewKey(gcpTestKey32)
	if err != nil {
		t.Fatalf("credstore.NewKey: %v", err)
	}
	h := NewDiscoveryGCPHandlers(store, zap.NewNop()).
		WithGCPCredstoreKey(key)
	if audit != nil {
		h.WithGCPAuditService(audit)
	}
	if factory != nil {
		h.WithGCPScannerFactory(factory)
	}
	return h, store, key
}

// newGCPRouter wires every GCP route the handler exposes so the
// HTTP-layer integration is exercised end-to-end.
func newGCPRouter(h *DiscoveryGCPHandlers) *gin.Engine {
	r := gin.New()
	r.POST("/api/v1/discovery/gcp/connections", h.HandleCreateGCPConnection)
	r.GET("/api/v1/discovery/gcp/connections", h.HandleListGCPConnections)
	r.GET("/api/v1/discovery/gcp/connections/:id", h.HandleGetGCPConnection)
	r.PATCH("/api/v1/discovery/gcp/connections/:id", h.HandleUpdateGCPConnection)
	r.DELETE("/api/v1/discovery/gcp/connections/:id", h.HandleDeleteGCPConnection)
	r.POST("/api/v1/discovery/gcp/connections/:id/validate", h.HandleValidateGCPConnection)
	r.POST("/api/v1/discovery/gcp/connections/:id/scan", h.HandleScanGCPConnection)
	r.POST("/api/v1/discovery/gcp/connections/:id/recommendations", h.HandleRecommendationsForGCPScan)
	return r
}

// gcpDoRequest is the shared HTTP harness.
func gcpDoRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
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

// seedGCPConnection inserts a GCPConnection directly via the store
// (bypassing the create handler) so tests of read-side endpoints can
// start from a known row without re-asserting the create path.
func seedGCPConnection(t *testing.T, store gcpconnstore.Store, key *credstore.Key, displayName, projectID, region string) *gcpconnstore.GCPConnection {
	t.Helper()
	sealed, err := credstore.SealGCPServiceAccount(key, validSAJSON(projectID))
	if err != nil {
		t.Fatalf("SealGCPServiceAccount: %v", err)
	}
	conn := &gcpconnstore.GCPConnection{
		DisplayName:                      displayName,
		ProjectID:                        projectID,
		Region:                           region,
		SealedSA:                         sealed,
		LearnFromAcceptedRecommendations: true,
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	return conn
}

// --- Create -------------------------------------------------------------

func TestCreateGCPConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, _ := newGCPTestHandlers(t, audit, nil)
	r := newGCPRouter(h)

	body := `{"display_name":"Prod GCP","project_id":"sandbox-12345","sealed_sa":"` + encodeSA(validSAJSON("sandbox-12345")) + `","region":"us-central1"}`
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections", body)
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
	if row.DisplayName != "Prod GCP" {
		t.Errorf("row.DisplayName = %q", row.DisplayName)
	}
	if row.ProjectID != "sandbox-12345" {
		t.Errorf("row.ProjectID = %q", row.ProjectID)
	}
	if row.Region != "us-central1" {
		t.Errorf("row.Region = %q", row.Region)
	}
	if len(row.SealedSA) == 0 {
		t.Errorf("row.SealedSA is empty — seal did not run")
	}

	// Response body MUST NOT carry the sealed_sa bytes.
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "sealed_sa") {
		t.Errorf("response leaked sealed_sa key: %s", bodyStr)
	}
	if strings.Contains(bodyStr, base64.StdEncoding.EncodeToString(row.SealedSA)) {
		t.Errorf("response leaked sealed_sa value: %s", bodyStr)
	}

	// One audit entry on the right topic, no SA bytes in the payload.
	if got := len(audit.entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventDiscoveryGCPConnectionCreated {
		t.Errorf("audit EventType = %q, want %q", e.EventType, services.AuditEventDiscoveryGCPConnectionCreated)
	}
	payloadJSON, _ := json.Marshal(e.Payload)
	if strings.Contains(string(payloadJSON), "sealed_sa") {
		t.Errorf("sealed_sa key leaked into audit payload: %s", payloadJSON)
	}
	if strings.Contains(string(payloadJSON), "BEGIN PRIVATE KEY") {
		t.Fatalf("SA private-key bytes leaked into audit payload: %s", payloadJSON)
	}
}

func TestCreateGCPConnection_KeylessAuthorizedUser_Accepted(t *testing.T) {
	// Keyless auth (v0.89.223): Google disables Service Account key
	// creation by default (constraints/iam.disableServiceAccountKey
	// Creation), so the connector must accept non-service_account
	// credential JSON. authorized_user is the shape `gcloud auth
	// application-default login` produces — no downloadable key.
	audit := &discoveryRecordingAudit{}
	h, store, _ := newGCPTestHandlers(t, audit, nil)
	r := newGCPRouter(h)

	adc := `{"type":"authorized_user","client_id":"x.apps.googleusercontent.com","client_secret":"s","refresh_token":"1//rt"}`
	body := `{"display_name":"Keyless GCP","project_id":"sandbox-12345","sealed_sa":"` + encodeSA([]byte(adc)) + `","region":"us-central1"}`
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("keyless authorized_user credential rejected: status=%d body=%s", w.Code, w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 1 || len(conns[0].SealedSA) == 0 {
		t.Fatalf("keyless credential not persisted/sealed: %+v", conns)
	}
}

func TestCreateGCPConnection_UnsupportedCredentialType_Returns400(t *testing.T) {
	// A credential type the GCP loader can't use (e.g. a bare OAuth
	// token doc) is still rejected with an actionable message.
	h, store, _ := newGCPTestHandlers(t, nil, nil)
	r := newGCPRouter(h)
	bad := `{"type":"magic_beans"}`
	body := `{"display_name":"Bad","project_id":"sandbox-12345","sealed_sa":"` + encodeSA([]byte(bad)) + `","region":"us-central1"}`
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "does not support") {
		t.Errorf("expected unsupported-type message, got: %s", w.Body.String())
	}
	_ = store
}

func TestCreateGCPConnection_MissingFields_Returns400(t *testing.T) {
	h, store, _ := newGCPTestHandlers(t, nil, nil)
	r := newGCPRouter(h)

	// Missing project_id.
	body := `{"display_name":"Prod","sealed_sa":"` + encodeSA(validSAJSON("sandbox-12345")) + `","region":"us-central1"}`
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "project ID is required") {
		t.Errorf("missing-project-id message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on missing field, got %d rows", len(conns))
	}
}

func TestCreateGCPConnection_InvalidProjectID_Returns400(t *testing.T) {
	h, store, _ := newGCPTestHandlers(t, nil, nil)
	r := newGCPRouter(h)

	// UPPERCASE is invalid — GCP requires lower-case ASCII.
	body := `{"display_name":"Prod","project_id":"UPPERCASE","sealed_sa":"` + encodeSA(validSAJSON("uppercase")) + `","region":"us-central1"}`
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "does not match the required format") {
		t.Errorf("invalid-project-id message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on invalid project_id, got %d rows", len(conns))
	}
}

func TestCreateGCPConnection_SAJSONClientEmailMismatch_Returns400(t *testing.T) {
	h, store, _ := newGCPTestHandlers(t, nil, nil)
	r := newGCPRouter(h)

	bad := map[string]any{
		"type":         "service_account",
		"project_id":   "sandbox-12345",
		"client_email": "not-a-real-sa@example.com",
	}
	badJSON, _ := json.Marshal(bad)
	body := `{"display_name":"Prod","project_id":"sandbox-12345","sealed_sa":"` + encodeSA(badJSON) + `","region":"us-central1"}`
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "iam.gserviceaccount.com") {
		t.Errorf("client_email mismatch message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on SA mismatch, got %d rows", len(conns))
	}
}

// --- List ---------------------------------------------------------------

func TestListGCPConnections_StripsSealedSA(t *testing.T) {
	h, store, key := newGCPTestHandlers(t, nil, nil)
	r := newGCPRouter(h)

	a := seedGCPConnection(t, store, key, "Alpha", "alpha-12345", "us-central1")
	b := seedGCPConnection(t, store, key, "Beta", "beta-12345", "us-east1")

	w := gcpDoRequest(r, http.MethodGet, "/api/v1/discovery/gcp/connections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "sealed_sa") {
		t.Errorf("list response leaked sealed_sa key: %s", body)
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

func TestUpdateGCPConnection_PreservesUntouchedFields(t *testing.T) {
	h, store, key := newGCPTestHandlers(t, nil, nil)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Original", "sandbox-12345", "us-central1")
	originalSA := append([]byte{}, conn.SealedSA...)

	// Only change display_name.
	patch := `{"display_name":"Renamed"}`
	w := gcpDoRequest(r, http.MethodPatch, "/api/v1/discovery/gcp/connections/"+conn.ID, patch)
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
	if after.ProjectID != "sandbox-12345" {
		t.Errorf("project_id mutated: %q", after.ProjectID)
	}
	if after.Region != "us-central1" {
		t.Errorf("region mutated: %q", after.Region)
	}
	if !bytes.Equal(after.SealedSA, originalSA) {
		t.Errorf("SealedSA mutated; PATCH should never touch sealed bytes")
	}
}

// --- Delete -------------------------------------------------------------

func TestDeleteGCPConnection_RemovesAndAudits(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, key := newGCPTestHandlers(t, audit, nil)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodDelete, "/api/v1/discovery/gcp/connections/"+conn.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	if _, err := store.Get(context.Background(), conn.ID); !errors.Is(err, gcpconnstore.ErrConnectionNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrConnectionNotFound", err)
	}

	if got := len(audit.entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	if audit.entries[0].EventType != services.AuditEventDiscoveryGCPConnectionDeleted {
		t.Errorf("audit EventType = %q, want %q",
			audit.entries[0].EventType, services.AuditEventDiscoveryGCPConnectionDeleted)
	}
	payload := audit.entries[0].Payload
	if payload["project_id"] != "sandbox-12345" {
		t.Errorf("audit payload project_id = %v, want sandbox-12345", payload["project_id"])
	}
}

// --- Validate -----------------------------------------------------------

func TestValidateGCPConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeScanner{
		result: &scanner.Result{
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "i-1"},
				{ResourceID: "i-2"},
				{ResourceID: "i-3"},
			},
		},
	}
	factory := &fakeGCPScannerFactory{scanner: fs}
	h, store, key := newGCPTestHandlers(t, audit, factory)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp gcpValidateResponse
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
	if !strings.Contains(string(factory.gotSA), "service_account") {
		t.Errorf("factory did not receive unsealed SA JSON: %q", string(factory.gotSA))
	}
	// Per runbook: validate produces no audit signal.
	if got := len(audit.entries); got != 0 {
		t.Errorf("validate should not emit audit events, got %d", got)
	}
}

func TestValidateGCPConnection_ProjectMismatch(t *testing.T) {
	fs := &fakeScanner{}
	factory := &fakeGCPScannerFactory{scanner: fs}
	h, store, key := newGCPTestHandlers(t, nil, factory)
	r := newGCPRouter(h)

	// Seed a connection with the row's project_id = "real-12345" but
	// the SA JSON's project_id field = "other-12345".
	sealed, err := credstore.SealGCPServiceAccount(key, validSAJSON("other-12345"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	conn := &gcpconnstore.GCPConnection{
		DisplayName: "Prod",
		ProjectID:   "real-12345",
		Region:      "us-central1",
		SealedSA:    sealed,
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp gcpValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.OK {
		t.Errorf("ok = true, want false on project mismatch")
	}
	if resp.ErrorKind != "project_mismatch" {
		t.Errorf("error_kind = %q, want project_mismatch", resp.ErrorKind)
	}
	// The scanner must NOT have been called when the cross-check failed.
	if factory.buildCall != 0 {
		t.Errorf("factory.Build calls = %d, want 0 on project mismatch", factory.buildCall)
	}
}

func TestValidateGCPConnection_PermissionDenied(t *testing.T) {
	fs := &fakeScanner{
		err: errors.New("googleapi: Error 403: Permission denied on compute.instances.list"),
	}
	factory := &fakeGCPScannerFactory{scanner: fs}
	h, store, key := newGCPTestHandlers(t, nil, factory)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp gcpValidateResponse
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

// --- Scan ---------------------------------------------------------------

func TestScanGCPConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeScanner{
		result: &scanner.Result{
			ScanID: "scan-abc",
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "i-1", HasOTel: true},
				{ResourceID: "i-2", HasOTel: true},
				{ResourceID: "i-3", HasOTel: true},
				{ResourceID: "i-4", HasOTel: false},
				{ResourceID: "i-5", HasOTel: false},
			},
			InstrumentedCount:   3,
			UninstrumentedCount: 2,
		},
	}
	factory := &fakeGCPScannerFactory{scanner: fs}
	h, store, key := newGCPTestHandlers(t, audit, factory)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp gcpScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.ScanID != "scan-abc" {
		t.Errorf("scan_id = %q, want scan-abc", resp.ScanID)
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
	if audit.entries[0].EventType != services.AuditEventDiscoveryGCPScanStarted {
		t.Errorf("first audit = %q, want scan_started", audit.entries[0].EventType)
	}
	completed := audit.entries[len(audit.entries)-1]
	if completed.EventType != services.AuditEventDiscoveryGCPScanCompleted {
		t.Errorf("last audit = %q, want scan_completed", completed.EventType)
	}
	// Verify the payload carries every field the design doc + brief
	// requires.
	for _, k := range []string{"connection_id", "project_id", "region", "scan_id", "instance_count", "instrumented_count", "uninstrumented_count", "partial"} {
		if _, ok := completed.Payload[k]; !ok {
			t.Errorf("scan_completed payload missing %q: %+v", k, completed.Payload)
		}
	}
	if completed.Payload["instrumented_count"].(int) != 3 {
		t.Errorf("payload.instrumented_count = %v, want 3", completed.Payload["instrumented_count"])
	}
}

func TestScanGCPConnection_PartialFailure_AuditPayloadCarriesPartialReason(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeScanner{
		result: &scanner.Result{
			ScanID: "scan-partial",
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "i-1", HasOTel: false},
			},
			InstrumentedCount:   0,
			UninstrumentedCount: 1,
			Partial:             true,
			PartialReason:       "rate limit on compute.instances.list",
			FailedServices:      []string{"gce"},
		},
	}
	factory := &fakeGCPScannerFactory{scanner: fs}
	h, store, key := newGCPTestHandlers(t, audit, factory)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	completed := audit.entries[len(audit.entries)-1]
	if completed.EventType != services.AuditEventDiscoveryGCPScanCompleted {
		t.Fatalf("last audit = %q, want scan_completed", completed.EventType)
	}
	if completed.Payload["partial"] != true {
		t.Errorf("payload.partial = %v, want true", completed.Payload["partial"])
	}
	if completed.Payload["partial_reason"] != "rate limit on compute.instances.list" {
		t.Errorf("payload.partial_reason = %v", completed.Payload["partial_reason"])
	}
	fs2, ok := completed.Payload["failed_services"].([]string)
	if !ok || len(fs2) != 1 || fs2[0] != "gce" {
		t.Errorf("payload.failed_services = %v, want [gce]", completed.Payload["failed_services"])
	}
}

func TestScanGCPConnection_HardError_EmitsScanFailedAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeScanner{err: errors.New("googleapi: 500 internal error from compute backend")}
	factory := &fakeGCPScannerFactory{scanner: fs}
	h, store, key := newGCPTestHandlers(t, audit, factory)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	// Expect started + failed.
	if got := len(audit.entries); got < 2 {
		t.Fatalf("audit entries = %d, want >= 2", got)
	}
	last := audit.entries[len(audit.entries)-1]
	if last.EventType != services.AuditEventDiscoveryGCPScanFailed {
		t.Errorf("last audit = %q, want scan_failed", last.EventType)
	}
	if last.Payload["error_kind"] == nil {
		t.Errorf("scan_failed payload missing error_kind: %+v", last.Payload)
	}
	if last.Payload["humanized_message"] == nil {
		t.Errorf("scan_failed payload missing humanized_message: %+v", last.Payload)
	}
}

// --- Trampoline unwired path -------------------------------------------

func TestStoreNotWired_Returns500(t *testing.T) {
	// Construct a bare handler with nil store to exercise the
	// belt-and-braces 500 path. The trampoline's 503 path is
	// exercised by the server-level tests; the handler-level 500 is
	// the struct-literal-construction defense the brief asks for.
	h := NewDiscoveryGCPHandlers(nil, zap.NewNop())
	r := newGCPRouter(h)
	w := gcpDoRequest(r, http.MethodGet, "/api/v1/discovery/gcp/connections", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GCPStoreNotWired") {
		t.Errorf("expected GCPStoreNotWired code: %s", w.Body.String())
	}
}

// classifyGCPScanError-level coverage on the unhappy-path strings the
// validate / scan_failed audits depend on. Kept as a small unit test
// to anchor the handler-side error_kind mapping.
func TestClassifyGCPScanError(t *testing.T) {
	cases := []struct {
		in   error
		want string
	}{
		{errors.New("Error 403: forbidden"), "permission_denied"},
		{errors.New("project Not Found"), "project_not_found"},
		{errors.New("oauth signing failure"), "credentials_invalid"},
		{errors.New("dial tcp: connection refused"), "network"},
		{errors.New("something else entirely"), "unknown"},
		{nil, ""},
	}
	for i, tc := range cases {
		got := classifyGCPScanError(tc.in)
		if got != tc.want {
			t.Errorf("case %d (%v): got %q, want %q", i, tc.in, got, tc.want)
		}
	}
	// Avoid unused-import lint when time imports aren't otherwise hit.
	_ = time.Now
}

// TestScanGCPConnection_SurfacesEventSources pins the v0.89.195
// event-source-tier wiring: when the GCP scanner returns event-source
// snapshots, the scan response carries them (the GCP Inventory tab's
// Event-sources sub-tab already renders scan.event_sources), including
// the slice-2 propagation axis.
func TestScanGCPConnection_SurfacesEventSources(t *testing.T) {
	fs := &fakeScanner{
		result: &scanner.Result{
			ScanID:              "scan-es-gcp",
			InstrumentedCount:   1,
			UninstrumentedCount: 0,
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "i-1", HasOTel: true},
			},
		},
		eventSources: []scanner.EventSourceInstanceSnapshot{
			{
				Provider: "gcp", Surface: "pubsub", SourceType: "topic",
				ResourceName:         "orders-topic",
				ResourceARN:          "projects/sandbox-12345/topics/orders-topic",
				Region:               "us-central1",
				HasTraceAxis:         true,
				HasLogAxis:           true,
				HasPropagationConfig: false,
				PropagationNotes:     []string{"subscription 'orders-sub' attribute filter excludes traceparent"},
			},
		},
	}
	factory := &fakeGCPScannerFactory{scanner: fs}
	h, store, key := newGCPTestHandlers(t, nil, factory)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp gcpScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if len(resp.EventSources) != 1 {
		t.Fatalf("event_sources len = %d, want 1; body=%s", len(resp.EventSources), w.Body.String())
	}
	es := resp.EventSources[0]
	if es.ResourceName != "orders-topic" {
		t.Errorf("resource_name = %q, want orders-topic", es.ResourceName)
	}
	if es.HasPropagationConfig {
		t.Errorf("has_propagation_config = true, want false (propagation gap)")
	}
	if len(es.PropagationNotes) != 1 {
		t.Errorf("propagation_notes len = %d, want 1", len(es.PropagationNotes))
	}
}

// TestRecommendationsForGCPScan_HappyPath pins the chunk-5 GCP
// recommendations endpoint (v0.89.197): the posted scan result becomes
// a Provider="gcp" DiscoveryScanContext (event sources included) and the
// proposer's plan is walked into recommendation envelopes.
func TestRecommendationsForGCPScan_HappyPath(t *testing.T) {
	mock := &mockAIProposer{
		result: &ai.ProposalResult{
			Reasoning: "GCP instrumentation plan",
			Model:     "claude-test",
			TokensIn:  10,
			TokensOut: 20,
			Plan: ai.PlanCandidate{
				Steps: []ai.PlanStepCandidate{
					{
						Name:                "Preserve traceparent on orders-topic",
						InlineConfigSnippet: "resource \"google_pubsub_subscription\" \"orders\" {}",
						AffectedResources:   []string{"orders-topic"},
					},
				},
			},
		},
	}
	audit := &discoveryRecordingAudit{}
	h, store, key := newGCPTestHandlers(t, audit, &fakeGCPScannerFactory{scanner: &fakeScanner{}})
	h.WithGCPAIProposer(mock)
	r := newGCPRouter(h)

	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	body, err := json.Marshal(gcpGenerateRecommendationsRequest{
		ScanResult: gcpScanResponse{
			ScanID:    "scan-gcp-recs",
			ProjectID: "sandbox-12345",
			Region:    "us-central1",
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "i-1", InstanceType: "e2-medium", OSFamily: "linux", Region: "us-central1", HasOTel: false},
			},
			EventSources: []eventSourceRow{
				{
					Provider: "gcp", Surface: "pubsub", SourceType: "topic",
					ResourceName: "orders-topic", Region: "us-central1",
					HasTraceAxis: true, HasLogAxis: true,
					HasPropagationConfig: false,
					PropagationNotes:     []string{"subscription filter excludes traceparent"},
				},
			},
			InstrumentedCount:   0,
			UninstrumentedCount: 1,
		},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/recommendations", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !mock.called {
		t.Fatal("proposer was not called")
	}
	// The proposer must have received a GCP-shaped context with the
	// event source folded in.
	if mock.gotCtx.Provider != "gcp" {
		t.Errorf("ctx.Provider = %q, want gcp", mock.gotCtx.Provider)
	}
	if mock.gotCtx.ProjectID != "sandbox-12345" {
		t.Errorf("ctx.ProjectID = %q, want sandbox-12345", mock.gotCtx.ProjectID)
	}
	if len(mock.gotCtx.EventSources) != 1 || mock.gotCtx.EventSources[0].ResourceName != "orders-topic" {
		t.Errorf("ctx.EventSources = %+v, want 1 orders-topic", mock.gotCtx.EventSources)
	}
	if mock.gotCtx.EventSources[0].HasPropagationConfig {
		t.Errorf("event source propagation should be false (gap)")
	}

	var resp awsGenerateRecommendationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.Declined {
		t.Errorf("declined = true, want false")
	}
	if len(resp.Recommendations) != 1 {
		t.Fatalf("recommendations len = %d, want 1; body=%s", len(resp.Recommendations), w.Body.String())
	}
	if resp.Recommendations[0].Title != "Preserve traceparent on orders-topic" {
		t.Errorf("rec title = %q", resp.Recommendations[0].Title)
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

// TestRecommendationsForGCPScan_ProposerNotWired returns 503 when AI
// assist is off (no proposer wired).
func TestRecommendationsForGCPScan_ProposerNotWired(t *testing.T) {
	h, store, key := newGCPTestHandlers(t, nil, &fakeGCPScannerFactory{scanner: &fakeScanner{}})
	r := newGCPRouter(h)
	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/recommendations", `{"scan_result":{"scan_id":"s1","project_id":"sandbox-12345"}}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestRecommendationsForGCPScan_AppendsRegressionRecs drives the recs endpoint
// end-to-end (request → async job → response) and verifies the detection→
// proposal wiring shipped in v0.89.319: a Cloud Run row whose cold-start
// detector fired (annotation on the scan response) AND whose error-rate
// observations clear the gates yields BOTH deterministic regression recs
// alongside the LLM step. The error-rate path is store-gated, so a rec
// appearing proves WithGCPRegressionStores actually reaches the helper through
// the handler (the seam the unit tests don't cover).
func TestRecommendationsForGCPScan_AppendsRegressionRecs(t *testing.T) {
	mock := &mockAIProposer{
		result: &ai.ProposalResult{
			Reasoning: "GCP instrumentation plan",
			Model:     "claude-test",
			Plan: ai.PlanCandidate{
				Steps: []ai.PlanStepCandidate{
					{Name: "Install OTel on checkout", InlineConfigSnippet: "resource \"x\" \"y\" {}"},
				},
			},
		},
	}
	h, store, key := newGCPTestHandlers(t, &discoveryRecordingAudit{}, &fakeGCPScannerFactory{scanner: &fakeScanner{}})
	h.WithGCPAIProposer(mock)

	const crARN = "//run.googleapis.com/projects/sandbox-12345/services/checkout"
	now := time.Now().UTC()
	errStore := &stubErrorRateReader{}
	// current 2000/5000 = 0.40, baseline 100/10000 = 0.01 → ratio 40x, fires.
	errStore.set(crARN, regressionCurrentWindowHours, 2000, 5000, 0.40, now)
	errStore.set(crARN, regressionBaselineWindowHours, 100, 10000, 0.01, now)
	// coldStartStore nil (cold-start gates on the snapshot flag); exclusions nil.
	h.WithGCPRegressionStores(nil, errStore, nil)

	r := newGCPRouter(h)
	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	exceeds := true
	p95 := 720.0
	body, err := json.Marshal(gcpGenerateRecommendationsRequest{
		ScanResult: gcpScanResponse{
			ScanID:    "scan-gcp-regression",
			ProjectID: "sandbox-12345",
			Region:    "us-central1",
			Serverless: []scanner.ServerlessInstanceSnapshot{{
				Provider:                  "gcp",
				Surface:                   "cloudrun",
				ResourceName:              "checkout",
				ResourceARN:               crARN,
				Region:                    "us-central1",
				ColdStartP95Ms:            &p95,
				ColdStartExceedsThreshold: &exceeds,
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/recommendations", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp awsGenerateRecommendationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	kinds := map[string]bool{}
	for _, rec := range resp.Recommendations {
		kinds[rec.ResourceKind] = true
	}
	if !kinds["cloudrun-cold-start-baseline"] {
		t.Errorf("missing cold-start regression rec; kinds=%v body=%s", kinds, w.Body.String())
	}
	if !kinds["span-quality-error-rate-spike"] {
		t.Errorf("missing error-rate regression rec (store wiring); kinds=%v body=%s", kinds, w.Body.String())
	}
	// The LLM step must still be present (regression recs are additive).
	if len(resp.Recommendations) != 3 {
		t.Errorf("recommendations len = %d, want 3 (1 LLM + 2 regression); body=%s", len(resp.Recommendations), w.Body.String())
	}
}

// TestRecommendationsForGCPScan_DeclineStillAppendsRegression pins the
// decline-guard fix (#328 follow-up): when the LLM proposer DECLINES (empty
// plan), the deterministic detector-based regression recs must still fire.
// Before the fix, the handler returned on result.Declined BEFORE the
// appendRegressionRecs call, silently dropping a real cold-start/error-rate
// finding. Same fixture as AppendsRegressionRecs, but the proposer declines —
// the response must be declined:false with the 2 regression recs (no LLM step).
func TestRecommendationsForGCPScan_DeclineStillAppendsRegression(t *testing.T) {
	mock := &mockAIProposer{
		result: &ai.ProposalResult{
			Declined: true,
			Reason:   "No compute or functions to instrument in this project.",
			Model:    "claude-test",
		},
	}
	h, store, key := newGCPTestHandlers(t, &discoveryRecordingAudit{}, &fakeGCPScannerFactory{scanner: &fakeScanner{}})
	h.WithGCPAIProposer(mock)

	const crARN = "//run.googleapis.com/projects/sandbox-12345/services/checkout"
	now := time.Now().UTC()
	errStore := &stubErrorRateReader{}
	errStore.set(crARN, regressionCurrentWindowHours, 2000, 5000, 0.40, now)
	errStore.set(crARN, regressionBaselineWindowHours, 100, 10000, 0.01, now)
	h.WithGCPRegressionStores(nil, errStore, nil)

	r := newGCPRouter(h)
	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	exceeds := true
	p95 := 720.0
	body, err := json.Marshal(gcpGenerateRecommendationsRequest{
		ScanResult: gcpScanResponse{
			ScanID:    "scan-gcp-decline-regression",
			ProjectID: "sandbox-12345",
			Region:    "us-central1",
			Serverless: []scanner.ServerlessInstanceSnapshot{{
				Provider:                  "gcp",
				Surface:                   "cloudrun",
				ResourceName:              "checkout",
				ResourceARN:               crARN,
				Region:                    "us-central1",
				ColdStartP95Ms:            &p95,
				ColdStartExceedsThreshold: &exceeds,
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/recommendations", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp awsGenerateRecommendationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.Declined {
		t.Error("declined = true; want false — deterministic regression recs fired despite the LLM decline")
	}
	kinds := map[string]bool{}
	for _, rec := range resp.Recommendations {
		kinds[rec.ResourceKind] = true
	}
	if !kinds["cloudrun-cold-start-baseline"] || !kinds["span-quality-error-rate-spike"] {
		t.Errorf("missing regression rec(s) on decline path; kinds=%v body=%s", kinds, w.Body.String())
	}
	if len(resp.Recommendations) != 2 {
		t.Errorf("recommendations len = %d, want 2 (both regression, no LLM step); body=%s", len(resp.Recommendations), w.Body.String())
	}
}

// TestGCPDemo_EnableScanDisable exercises the credential-free GCP demo:
// enable provisions the demo project, scan short-circuits to the canned
// sample inventory (no SA decrypt, no scanner Build), enable is idempotent,
// and disable removes it.
func TestGCPDemo_EnableScanDisable(t *testing.T) {
	h, store, _ := newGCPTestHandlers(t, nil, &fakeGCPScannerFactory{})
	r := gin.New()
	r.POST("/api/v1/discovery/gcp/demo/enable", h.HandleGCPDemoEnable)
	r.DELETE("/api/v1/discovery/gcp/demo", h.HandleGCPDemoDisable)
	r.POST("/api/v1/discovery/gcp/connections/:id/scan", h.HandleScanGCPConnection)

	// Enable.
	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/demo/enable", "")
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var conn struct {
		ID        string `json:"id"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &conn); err != nil {
		t.Fatalf("enable unmarshal: %v", err)
	}
	if conn.ProjectID != demo.GCPProjectID {
		t.Errorf("project_id = %q, want %q", conn.ProjectID, demo.GCPProjectID)
	}

	// Scan the demo connection → canned inventory.
	w = gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("scan status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var scanResp gcpScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &scanResp); err != nil {
		t.Fatalf("scan unmarshal: %v", err)
	}
	if len(scanResp.Compute) != 3 {
		t.Errorf("compute rows = %d, want 3", len(scanResp.Compute))
	}
	if len(scanResp.Databases) != 2 {
		t.Errorf("database rows = %d, want 2", len(scanResp.Databases))
	}

	// Enable again — idempotent, no duplicate row.
	_ = gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/demo/enable", "")
	conns, _ := store.List(context.Background())
	demoCount := 0
	for _, c := range conns {
		if c != nil && demo.IsGCPDemoProject(c.ProjectID) {
			demoCount++
		}
	}
	if demoCount != 1 {
		t.Errorf("demo connections after double-enable = %d, want 1", demoCount)
	}

	// Disable.
	w = gcpDoRequest(r, http.MethodDelete, "/api/v1/discovery/gcp/demo", "")
	if w.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200", w.Code)
	}
	conns, _ = store.List(context.Background())
	for _, c := range conns {
		if c != nil && demo.IsGCPDemoProject(c.ProjectID) {
			t.Error("demo connection still present after disable")
		}
	}
}
