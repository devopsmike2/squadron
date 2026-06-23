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

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/services"
)

// --- test fixtures ------------------------------------------------------

// ociTestKey32 is a deterministic 32-byte key used to construct
// credstore.NewKey for the OCI handler tests. Real deployments
// supply the key via SQUADRON_SECRETS_KEY; tests inject the fixture
// so each test starts from a known cipher posture.
var ociTestKey32 = []byte("0123456789abcdef0123456789abcdef")

// Canonical OCID + fingerprint + region values reused across the
// OCI handler tests. Picking fixed values rather than generating
// per-test makes the test surface deterministic and keeps test
// failure diffs readable.
const (
	ociTestTenancyOCID = "ocid1.tenancy.oc1..aaaaaaaa00000000000000000000000000000000000000000000000000"
	ociTestUserOCID    = "ocid1.user.oc1..bbbbbbbb00000000000000000000000000000000000000000000000000"
	ociTestFingerprint = "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
	ociTestRegion      = "us-phoenix-1"
	// ociTestPrivateKey stands in for a PEM-encoded RSA private key
	// — the handler tests don't actually parse it; they only check
	// that the unsealed bytes round-trip through credstore.
	ociTestPrivateKey = "-----BEGIN PRIVATE KEY-----\nFAKE_NOT_A_REAL_KEY_JUST_FIXTURE_BYTES\n-----END PRIVATE KEY-----\n"
)

// encodeOCIPrivateKey base64-encodes the RSA private key in the
// wire shape the handler expects.
func encodeOCIPrivateKey(key string) string {
	return base64.StdEncoding.EncodeToString([]byte(key))
}

// fakeOCIScanner is the in-test scanner.Scanner implementation used
// by the OCI handler tests. Records the call (so tests can assert
// on inputs) and returns a pre-canned Result or error.
//
// A separate type from the GCP / Azure fakeScanners so the surfaces
// stay independent — tests of the OCI handler shouldn't accidentally
// reach for cross-provider fixtures and vice versa.
type fakeOCIScanner struct {
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
}

func (f *fakeOCIScanner) Provider() credstore.Provider {
	return credstore.Provider(ociconnstore.ProviderOCI)
}
func (f *fakeOCIScanner) Scan(_ context.Context, _ *credstore.CloudConnection, regions []string) (*scanner.Result, error) {
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
func (f *fakeOCIScanner) Validate(_ context.Context, _ *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	return &scanner.ValidationResult{AssumeRoleOK: true}, nil
}

// fakeOCIScannerFactory satisfies OCIScannerFactory by returning a
// pre-seeded fakeOCIScanner. Records the unsealed private key bytes
// the handler passed so tests can assert the unseal happened
// end-to-end without poking at credstore internals.
type fakeOCIScannerFactory struct {
	scanner       *fakeOCIScanner
	buildErr      error
	gotPrivateKey []byte
	gotTenancy    string
	gotUser       string
	gotFingerprt  string
	gotRegion     string
	buildCall     int
}

func (f *fakeOCIScannerFactory) Build(conn ociconnstore.OCIConnection, privateKey []byte) (scanner.Scanner, error) {
	f.buildCall++
	f.gotPrivateKey = append([]byte{}, privateKey...)
	f.gotTenancy = conn.TenancyOCID
	f.gotUser = conn.UserOCID
	f.gotFingerprt = conn.Fingerprint
	f.gotRegion = conn.Region
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return f.scanner, nil
}

// newOCITestHandlers builds DiscoveryOCIHandlers wired with the
// in-memory store + a fresh credstore.Key + the supplied audit and
// scanner factory. logger is a no-op so test output stays clean.
func newOCITestHandlers(t *testing.T, audit services.AuditService, factory OCIScannerFactory) (*DiscoveryOCIHandlers, ociconnstore.Store, *credstore.Key) {
	t.Helper()
	store := ociconnstore.NewMemoryStore()
	key, err := credstore.NewKey(ociTestKey32)
	if err != nil {
		t.Fatalf("credstore.NewKey: %v", err)
	}
	h := NewDiscoveryOCIHandlers(store, zap.NewNop()).
		WithOCICredstoreKey(key)
	if audit != nil {
		h.WithOCIAuditService(audit)
	}
	if factory != nil {
		h.WithOCIScannerFactory(factory)
	}
	return h, store, key
}

// newOCIRouter wires every OCI route the handler exposes so the
// HTTP-layer integration is exercised end-to-end.
func newOCIRouter(h *DiscoveryOCIHandlers) *gin.Engine {
	r := gin.New()
	r.POST("/api/v1/discovery/oci/connections", h.HandleCreateOCIConnection)
	r.GET("/api/v1/discovery/oci/connections", h.HandleListOCIConnections)
	r.GET("/api/v1/discovery/oci/connections/:id", h.HandleGetOCIConnection)
	r.PATCH("/api/v1/discovery/oci/connections/:id", h.HandleUpdateOCIConnection)
	r.DELETE("/api/v1/discovery/oci/connections/:id", h.HandleDeleteOCIConnection)
	r.POST("/api/v1/discovery/oci/connections/:id/validate", h.HandleValidateOCIConnection)
	r.POST("/api/v1/discovery/oci/connections/:id/scan", h.HandleScanOCIConnection)
	r.POST("/api/v1/discovery/oci/connections/:id/recommendations", h.HandleRecommendationsForOCIScan)
	return r
}

// ociDoRequest is the shared HTTP harness.
func ociDoRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
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
	return w
}

// seedOCIConnection inserts an OCIConnection directly via the store
// (bypassing the create handler) so tests of read-side endpoints
// can start from a known row without re-asserting the create path.
func seedOCIConnection(t *testing.T, store ociconnstore.Store, key *credstore.Key, displayName, tenancyOCID, userOCID, fingerprint, region string) *ociconnstore.OCIConnection {
	t.Helper()
	sealed, err := credstore.SealOCIPrivateKey(key, []byte(ociTestPrivateKey))
	if err != nil {
		t.Fatalf("SealOCIPrivateKey: %v", err)
	}
	conn := &ociconnstore.OCIConnection{
		DisplayName:                      displayName,
		TenancyOCID:                      tenancyOCID,
		UserOCID:                         userOCID,
		Fingerprint:                      fingerprint,
		SealedPrivateKey:                 sealed,
		Region:                           region,
		LearnFromAcceptedRecommendations: true,
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	return conn
}

// --- Create -------------------------------------------------------------

func TestCreateOCIConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, _ := newOCITestHandlers(t, audit, nil)
	r := newOCIRouter(h)

	body := `{"display_name":"Prod OCI","tenancy_ocid":"` + ociTestTenancyOCID +
		`","user_ocid":"` + ociTestUserOCID +
		`","fingerprint":"` + ociTestFingerprint +
		`","sealed_private_key":"` + encodeOCIPrivateKey(ociTestPrivateKey) +
		`","region":"` + ociTestRegion + `"}`
	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections", body)
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
	if row.DisplayName != "Prod OCI" {
		t.Errorf("row.DisplayName = %q", row.DisplayName)
	}
	if row.TenancyOCID != ociTestTenancyOCID {
		t.Errorf("row.TenancyOCID = %q", row.TenancyOCID)
	}
	if row.UserOCID != ociTestUserOCID {
		t.Errorf("row.UserOCID = %q", row.UserOCID)
	}
	if row.Fingerprint != ociTestFingerprint {
		t.Errorf("row.Fingerprint = %q", row.Fingerprint)
	}
	if row.Region != ociTestRegion {
		t.Errorf("row.Region = %q", row.Region)
	}
	if len(row.SealedPrivateKey) == 0 {
		t.Errorf("row.SealedPrivateKey is empty — seal did not run")
	}

	// Response body MUST NOT carry the sealed_private_key bytes.
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "sealed_private_key") {
		t.Errorf("response leaked sealed_private_key key: %s", bodyStr)
	}
	if strings.Contains(bodyStr, base64.StdEncoding.EncodeToString(row.SealedPrivateKey)) {
		t.Errorf("response leaked sealed_private_key value: %s", bodyStr)
	}
	if strings.Contains(bodyStr, ociTestPrivateKey) {
		t.Errorf("response leaked plaintext private key: %s", bodyStr)
	}

	// One audit entry on the right topic, no secret bytes in the
	// payload.
	if got := len(audit.entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventDiscoveryOCIConnectionCreated {
		t.Errorf("audit EventType = %q, want %q", e.EventType, services.AuditEventDiscoveryOCIConnectionCreated)
	}
	payloadJSON, _ := json.Marshal(e.Payload)
	if strings.Contains(string(payloadJSON), "sealed_private_key") {
		t.Errorf("sealed_private_key key leaked into audit payload: %s", payloadJSON)
	}
	if strings.Contains(string(payloadJSON), ociTestPrivateKey) {
		t.Fatalf("private key plaintext leaked into audit payload: %s", payloadJSON)
	}
}

func TestCreateOCIConnection_MissingFields_Returns400(t *testing.T) {
	h, store, _ := newOCITestHandlers(t, nil, nil)
	r := newOCIRouter(h)

	// Missing user_ocid.
	body := `{"display_name":"Prod","tenancy_ocid":"` + ociTestTenancyOCID +
		`","fingerprint":"` + ociTestFingerprint +
		`","sealed_private_key":"` + encodeOCIPrivateKey(ociTestPrivateKey) +
		`","region":"` + ociTestRegion + `"}`
	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "user OCID is required") {
		t.Errorf("missing-user-ocid message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on missing field, got %d rows", len(conns))
	}
}

func TestCreateOCIConnection_InvalidTenancyOCID_Returns400(t *testing.T) {
	h, store, _ := newOCITestHandlers(t, nil, nil)
	r := newOCIRouter(h)

	// Non-OCID tenancy_ocid is rejected at the handler.
	body := `{"display_name":"Prod","tenancy_ocid":"not-an-ocid","user_ocid":"` + ociTestUserOCID +
		`","fingerprint":"` + ociTestFingerprint +
		`","sealed_private_key":"` + encodeOCIPrivateKey(ociTestPrivateKey) +
		`","region":"` + ociTestRegion + `"}`
	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidTenancyOCID") {
		t.Errorf("invalid-tenancy-ocid message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on invalid tenancy_ocid, got %d rows", len(conns))
	}
}

func TestCreateOCIConnection_InvalidUserOCID_Returns400(t *testing.T) {
	h, store, _ := newOCITestHandlers(t, nil, nil)
	r := newOCIRouter(h)

	// Non-OCID user_ocid is rejected at the handler.
	body := `{"display_name":"Prod","tenancy_ocid":"` + ociTestTenancyOCID +
		`","user_ocid":"definitely-not-an-ocid","fingerprint":"` + ociTestFingerprint +
		`","sealed_private_key":"` + encodeOCIPrivateKey(ociTestPrivateKey) +
		`","region":"` + ociTestRegion + `"}`
	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InvalidUserOCID") {
		t.Errorf("invalid-user-ocid message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on invalid user_ocid, got %d rows", len(conns))
	}
}

func TestCreateOCIConnection_MissingRegion_Returns400(t *testing.T) {
	h, store, _ := newOCITestHandlers(t, nil, nil)
	r := newOCIRouter(h)

	// OCI requires Region (unlike AWS/GCP/Azure where empty = scan
	// all). Empty region is rejected at the handler.
	body := `{"display_name":"Prod","tenancy_ocid":"` + ociTestTenancyOCID +
		`","user_ocid":"` + ociTestUserOCID +
		`","fingerprint":"` + ociTestFingerprint +
		`","sealed_private_key":"` + encodeOCIPrivateKey(ociTestPrivateKey) +
		`","region":""}`
	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MissingRegion") {
		t.Errorf("missing-region message not surfaced: %s", w.Body.String())
	}
	conns, _ := store.List(context.Background())
	if len(conns) != 0 {
		t.Errorf("store should be empty on missing region, got %d rows", len(conns))
	}
}

// --- List ---------------------------------------------------------------

func TestListOCIConnections_StripsSealedPrivateKey(t *testing.T) {
	h, store, key := newOCITestHandlers(t, nil, nil)
	r := newOCIRouter(h)

	a := seedOCIConnection(t, store, key, "Alpha", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)
	b := seedOCIConnection(t, store, key, "Beta",
		"ocid1.tenancy.oc1..zzzzzzzz00000000000000000000000000000000000000000000000000",
		"ocid1.user.oc1..yyyyyyyy00000000000000000000000000000000000000000000000000",
		"11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00",
		"eu-frankfurt-1")

	w := ociDoRequest(r, http.MethodGet, "/api/v1/discovery/oci/connections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "sealed_private_key") {
		t.Errorf("list response leaked sealed_private_key key: %s", body)
	}
	if strings.Contains(body, ociTestPrivateKey) {
		t.Errorf("list response leaked plaintext private key: %s", body)
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

func TestUpdateOCIConnection_PreservesUntouchedFields(t *testing.T) {
	h, store, key := newOCITestHandlers(t, nil, nil)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Original", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)
	originalKey := append([]byte{}, conn.SealedPrivateKey...)

	// Only change display_name; tenancy/user/fingerprint/region/key
	// must all stay put.
	patch := `{"display_name":"Renamed"}`
	w := ociDoRequest(r, http.MethodPatch, "/api/v1/discovery/oci/connections/"+conn.ID, patch)
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
	if after.TenancyOCID != ociTestTenancyOCID {
		t.Errorf("tenancy_ocid mutated: %q", after.TenancyOCID)
	}
	if after.UserOCID != ociTestUserOCID {
		t.Errorf("user_ocid mutated: %q", after.UserOCID)
	}
	if after.Fingerprint != ociTestFingerprint {
		t.Errorf("fingerprint mutated: %q", after.Fingerprint)
	}
	if after.Region != ociTestRegion {
		t.Errorf("region mutated: %q", after.Region)
	}
	if !bytes.Equal(after.SealedPrivateKey, originalKey) {
		t.Errorf("SealedPrivateKey mutated; PATCH should never touch sealed bytes")
	}
}

// --- Delete -------------------------------------------------------------

func TestDeleteOCIConnection_RemovesAndAudits(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, key := newOCITestHandlers(t, audit, nil)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodDelete, "/api/v1/discovery/oci/connections/"+conn.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	if _, err := store.Get(context.Background(), conn.ID); !errors.Is(err, ociconnstore.ErrConnectionNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrConnectionNotFound", err)
	}

	if got := len(audit.entries); got != 1 {
		t.Fatalf("audit entries = %d, want 1", got)
	}
	if audit.entries[0].EventType != services.AuditEventDiscoveryOCIConnectionDeleted {
		t.Errorf("audit EventType = %q, want %q",
			audit.entries[0].EventType, services.AuditEventDiscoveryOCIConnectionDeleted)
	}
	payload := audit.entries[0].Payload
	if payload["tenancy_ocid"] != ociTestTenancyOCID {
		t.Errorf("audit payload tenancy_ocid = %v, want %s", payload["tenancy_ocid"], ociTestTenancyOCID)
	}
	if payload["region"] != ociTestRegion {
		t.Errorf("audit payload region = %v, want %s", payload["region"], ociTestRegion)
	}
}

// --- Validate -----------------------------------------------------------

func TestValidateOCIConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeOCIScanner{
		result: &scanner.Result{
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "inst-1"},
				{ResourceID: "inst-2"},
				{ResourceID: "inst-3"},
			},
		},
	}
	factory := &fakeOCIScannerFactory{scanner: fs}
	h, store, key := newOCITestHandlers(t, audit, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ociValidateResponse
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
	if string(factory.gotPrivateKey) != ociTestPrivateKey {
		t.Errorf("factory did not receive unsealed private key: got %q", string(factory.gotPrivateKey))
	}
	if factory.gotTenancy != ociTestTenancyOCID {
		t.Errorf("factory got tenancy = %q, want %q", factory.gotTenancy, ociTestTenancyOCID)
	}
	if factory.gotRegion != ociTestRegion {
		t.Errorf("factory got region = %q, want %q", factory.gotRegion, ociTestRegion)
	}
	// Per runbook: validate produces no audit signal.
	if got := len(audit.entries); got != 0 {
		t.Errorf("validate should not emit audit events, got %d", got)
	}
}

func TestValidateOCIConnection_PermissionDenied(t *testing.T) {
	fs := &fakeOCIScanner{
		err: errors.New("RESPONSE 403: NotAuthorizedOrNotFound: caller does not have permission to perform action ListInstances over the requested resource"),
	}
	factory := &fakeOCIScannerFactory{scanner: fs}
	h, store, key := newOCITestHandlers(t, nil, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ociValidateResponse
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

func TestValidateOCIConnection_FingerprintMismatch(t *testing.T) {
	fs := &fakeOCIScanner{
		// OCI's auth layer surfaces signature/fingerprint errors as
		// 401-shaped responses.
		err: errors.New("RESPONSE 401: NotAuthenticated: The required information to complete authentication was not provided or was incorrect. Fingerprint mismatch on key"),
	}
	factory := &fakeOCIScannerFactory{scanner: fs}
	h, store, key := newOCITestHandlers(t, nil, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ociValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.OK {
		t.Errorf("ok = true, want false on fingerprint mismatch")
	}
	if resp.ErrorKind != "fingerprint_mismatch" {
		t.Errorf("error_kind = %q, want fingerprint_mismatch", resp.ErrorKind)
	}
}

func TestValidateOCIConnection_TenancyNotFound(t *testing.T) {
	fs := &fakeOCIScanner{
		err: errors.New("RESPONSE 404: TenancyNotFound: The tenancy could not be found in this region"),
	}
	factory := &fakeOCIScannerFactory{scanner: fs}
	h, store, key := newOCITestHandlers(t, nil, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ociValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.OK {
		t.Errorf("ok = true, want false on tenancy_not_found")
	}
	if resp.ErrorKind != "tenancy_not_found" {
		t.Errorf("error_kind = %q, want tenancy_not_found", resp.ErrorKind)
	}
}

func TestValidateOCIConnection_PrivateKeyInvalid(t *testing.T) {
	// Scanner factory returns a parse error — chunk 2's manual
	// request-signer rejects malformed PEM before any HTTPS call,
	// surfacing the failure here. The handler should classify this
	// as private_key_invalid.
	factory := &fakeOCIScannerFactory{
		buildErr: errors.New("oci: failed to parse RSA private key from PEM block: x509: malformed private key"),
	}
	h, store, key := newOCITestHandlers(t, nil, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/validate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ociValidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.OK {
		t.Errorf("ok = true, want false on private_key_invalid")
	}
	if resp.ErrorKind != "private_key_invalid" {
		t.Errorf("error_kind = %q, want private_key_invalid", resp.ErrorKind)
	}
}

// --- Scan ---------------------------------------------------------------

func TestScanOCIConnection_HappyPath(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeOCIScanner{
		result: &scanner.Result{
			ScanID: "scan-abc",
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "inst-1", HasOTel: true},
				{ResourceID: "inst-2", HasOTel: true},
				{ResourceID: "inst-3", HasOTel: true},
				{ResourceID: "inst-4", HasOTel: false},
				{ResourceID: "inst-5", HasOTel: false},
			},
			InstrumentedCount:   3,
			UninstrumentedCount: 2,
		},
	}
	factory := &fakeOCIScannerFactory{scanner: fs}
	h, store, key := newOCITestHandlers(t, audit, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ociScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.ScanID != "scan-abc" {
		t.Errorf("scan_id = %q, want scan-abc", resp.ScanID)
	}
	if resp.TenancyOCID != ociTestTenancyOCID {
		t.Errorf("tenancy_ocid = %q", resp.TenancyOCID)
	}
	if resp.Region != ociTestRegion {
		t.Errorf("region = %q", resp.Region)
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
	if audit.entries[0].EventType != services.AuditEventDiscoveryOCIScanStarted {
		t.Errorf("first audit = %q, want scan_started", audit.entries[0].EventType)
	}
	completed := audit.entries[len(audit.entries)-1]
	if completed.EventType != services.AuditEventDiscoveryOCIScanCompleted {
		t.Errorf("last audit = %q, want scan_completed", completed.EventType)
	}
	// Verify the payload carries every field the design doc + brief
	// requires.
	for _, k := range []string{"connection_id", "tenancy_ocid", "region", "scan_id", "instance_count", "instrumented_count", "uninstrumented_count", "partial"} {
		if _, ok := completed.Payload[k]; !ok {
			t.Errorf("scan_completed payload missing %q: %+v", k, completed.Payload)
		}
	}
	if completed.Payload["instrumented_count"].(int) != 3 {
		t.Errorf("payload.instrumented_count = %v, want 3", completed.Payload["instrumented_count"])
	}
}

func TestScanOCIConnection_PartialFailure_AuditPayloadCarriesPartialReason(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeOCIScanner{
		result: &scanner.Result{
			ScanID: "scan-partial",
			Compute: []scanner.ComputeInstanceSnapshot{
				{ResourceID: "inst-1", HasOTel: false},
			},
			InstrumentedCount:   0,
			UninstrumentedCount: 1,
			Partial:             true,
			PartialReason:       "rate limit on Compute.ListInstances",
			FailedServices:      []string{"ocicompute"},
		},
	}
	factory := &fakeOCIScannerFactory{scanner: fs}
	h, store, key := newOCITestHandlers(t, audit, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	completed := audit.entries[len(audit.entries)-1]
	if completed.EventType != services.AuditEventDiscoveryOCIScanCompleted {
		t.Fatalf("last audit = %q, want scan_completed", completed.EventType)
	}
	if completed.Payload["partial"] != true {
		t.Errorf("payload.partial = %v, want true", completed.Payload["partial"])
	}
	if completed.Payload["partial_reason"] != "rate limit on Compute.ListInstances" {
		t.Errorf("payload.partial_reason = %v", completed.Payload["partial_reason"])
	}
	fs2, ok := completed.Payload["failed_services"].([]string)
	if !ok || len(fs2) != 1 || fs2[0] != "ocicompute" {
		t.Errorf("payload.failed_services = %v, want [ocicompute]", completed.Payload["failed_services"])
	}
}

func TestScanOCIConnection_HardError_EmitsScanFailedAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	fs := &fakeOCIScanner{err: errors.New("RESPONSE 500: InternalServerError from OCI Compute control plane")}
	factory := &fakeOCIScannerFactory{scanner: fs}
	h, store, key := newOCITestHandlers(t, audit, factory)
	r := newOCIRouter(h)

	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	// Expect started + failed.
	if got := len(audit.entries); got < 2 {
		t.Fatalf("audit entries = %d, want >= 2", got)
	}
	last := audit.entries[len(audit.entries)-1]
	if last.EventType != services.AuditEventDiscoveryOCIScanFailed {
		t.Errorf("last audit = %q, want scan_failed", last.EventType)
	}
	if last.Payload["error_kind"] == nil {
		t.Errorf("scan_failed payload missing error_kind: %+v", last.Payload)
	}
	if last.Payload["humanized_message"] == nil {
		t.Errorf("scan_failed payload missing humanized_message: %+v", last.Payload)
	}
}

// --- Recommendations stub ----------------------------------------------

func TestRecommendationsForOCIScan_NotImplementedStub(t *testing.T) {
	h, store, key := newOCITestHandlers(t, nil, nil)
	r := newOCIRouter(h)
	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)
	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/recommendations", "{}")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "chunk 5") {
		t.Errorf("body should explain chunk-5 deferral: %s", w.Body.String())
	}
}

// --- classifier coverage ----------------------------------------------

// TestClassifyOCIScanError anchors the handler-side error_kind
// mapping per design doc §7.1. The validate / scan_failed audit
// consumers pattern-match on these string values; a regression
// here ripples into the runbook / wizard remediation copy.
func TestClassifyOCIScanError(t *testing.T) {
	cases := []struct {
		in   error
		want string
	}{
		{errors.New("RESPONSE 403: NotAuthorizedOrNotFound"), "permission_denied"},
		{errors.New("forbidden"), "permission_denied"},
		{errors.New("RESPONSE 401: NotAuthenticated: Fingerprint mismatch"), "fingerprint_mismatch"},
		{errors.New("InvalidSignature: signature validation failed"), "fingerprint_mismatch"},
		{errors.New("RESPONSE 401: unauthorized"), "fingerprint_mismatch"},
		{errors.New("oci: failed to parse RSA private key from PEM block"), "private_key_invalid"},
		{errors.New("RESPONSE 404: TenancyNotFound: tenancy not found"), "tenancy_not_found"},
		{errors.New("dial tcp: connection refused"), "network"},
		{errors.New("something else entirely"), "unknown"},
		{nil, ""},
	}
	for i, tc := range cases {
		got := classifyOCIScanError(tc.in)
		if got != tc.want {
			t.Errorf("case %d (%v): got %q, want %q", i, tc.in, got, tc.want)
		}
	}
	// Avoid unused-import lint when time imports aren't otherwise hit.
	_ = time.Now
}
