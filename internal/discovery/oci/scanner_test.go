// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// --- Helpers: RSA key generation --------------------------------------
//
// generateTestKey produces a 2048-bit RSA keypair and the PKCS#1 PEM
// encoding the signing helpers expect. Tests use it to exercise the
// real signing path without checking a real OCI private key into the
// repo.

func generateTestKey(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	derBytes := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: derBytes,
	})
	return pemBytes, key
}

func generateTestKeyPKCS8(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	derBytes, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: derBytes,
	})
	return pemBytes, key
}

// --- Signing key tests -----------------------------------------------

func TestSigningKey_ParsePrivateKey_HappyPath(t *testing.T) {
	pemBytes, want := generateTestKey(t)
	got, err := ParsePrivateKey(pemBytes)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want.N.BitLen(), got.N.BitLen())
}

func TestSigningKey_ParsePrivateKey_HappyPath_PKCS8(t *testing.T) {
	pemBytes, want := generateTestKeyPKCS8(t)
	got, err := ParsePrivateKey(pemBytes)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want.N.BitLen(), got.N.BitLen())
}

func TestSigningKey_ParsePrivateKey_MalformedPEM_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", []byte("")},
		{"not PEM", []byte("not a pem-encoded thing")},
		{"wrong block type", []byte(`-----BEGIN CERTIFICATE-----
MIIBHTCBxKADAgECAhEA1234567890abcdef==
-----END CERTIFICATE-----`)},
		{"corrupted PKCS1 body", []byte(`-----BEGIN RSA PRIVATE KEY-----
ZGVmaW5pdGVseS1ub3QtYS1rZXk=
-----END RSA PRIVATE KEY-----`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePrivateKey(tc.in)
			require.Error(t, err, "expected ParsePrivateKey to reject %q", tc.name)
		})
	}
}

func TestSigningKey_KeyIDFormat(t *testing.T) {
	sk := &SigningKey{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
	}
	assert.Equal(t, "ocid1.tenancy.oc1..aaa/ocid1.user.oc1..bbb/aa:bb:cc:dd", sk.KeyID())
}

func TestSigningKey_SignRequest_SetsAuthorizationHeader(t *testing.T) {
	_, key := generateTestKey(t)
	sk := &SigningKey{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		PrivateKey:  key,
	}
	req, err := http.NewRequest(http.MethodGet, "https://iaas.us-phoenix-1.oraclecloud.com/20160918/instances?compartmentId=ocid1.compartment.oc1..ccc", nil)
	require.NoError(t, err)

	require.NoError(t, sk.SignRequest(req))

	authz := req.Header.Get("Authorization")
	require.NotEmpty(t, authz)
	assert.True(t, strings.HasPrefix(authz, `Signature version="1"`), "got: %s", authz)
	assert.Contains(t, authz, `algorithm="rsa-sha256"`)
	assert.Contains(t, authz, `headers="(request-target) date host"`)
	assert.Contains(t, authz, sk.KeyID())
	assert.Contains(t, authz, "signature=")
	assert.NotEmpty(t, req.Header.Get("Date"))
	assert.NotEmpty(t, req.Header.Get("Host"))
}

func TestSigningKey_SignRequest_NilKey_Errors(t *testing.T) {
	sk := &SigningKey{}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	require.Error(t, sk.SignRequest(req))
}

// --- Mock OCI server -------------------------------------------------
//
// fakeOCI is an httptest-backed mock of the OCI Identity + Compute
// endpoints. The test server multiplexes /20160918/compartments and
// /20160918/instances via path-based dispatch; the scanner's
// ociEndpoint field points at the same base URL so identity and
// compute calls both land here (the real production identity and
// compute hosts differ, but the path scheme is identical).

type fakeOCI struct {
	mu sync.Mutex

	// Compartments returned by the identity list call.
	Compartments []ociCompartment

	// InstancesByCompartment maps compartmentId -> instances served
	// when /instances is called with that compartmentId. A missing
	// compartmentId returns an empty list (not a 404) so tests can
	// configure per-compartment failures via the Status fields.
	InstancesByCompartment map[string][]ociInstance

	// DBSystemsByCompartment maps compartmentId -> DB Systems served
	// when /dbSystems is called with that compartmentId. Same
	// missing-compartment-returns-empty convention as the instances
	// map (per-compartment failures configure via DBStatus / ForCompartment).
	DBSystemsByCompartment map[string][]dbSystem

	// AutonomousByCompartment maps compartmentId -> Autonomous
	// Databases served when /autonomousDatabases is called with that
	// compartmentId. Same convention.
	AutonomousByCompartment map[string][]autonomousDatabase

	// OKEByCompartment maps compartmentId -> OKE clusters served
	// when /clusters is called with that compartmentId. Same
	// missing-compartment-returns-empty convention as the
	// instances / dbSystems / autonomousDatabases maps
	// (per-compartment failures configure via OKEStatus / OKEForCompartment).
	OKEByCompartment map[string][]okeCluster

	// CompartmentsStatus, when non-zero, makes the next compartments
	// call return this status with a JSON error body.
	CompartmentsStatus    int
	CompartmentsErrorCode string

	// InstancesStatus, when non-zero, makes every instances call
	// return this status for the configured ForCompartment (or for
	// every compartment when ForCompartment is empty).
	InstancesStatus        int
	InstancesErrorCode     string
	InstancesForCompartment string
	InstancesRetryAfter    string

	// DBSystemsStatus / ErrorCode / ForCompartment / RetryAfter:
	// same shape as the instances knobs. Configures partial-failure
	// scenarios for the DB Systems walk.
	DBSystemsStatus         int
	DBSystemsErrorCode      string
	DBSystemsForCompartment string
	DBSystemsRetryAfter     string

	// AutonomousStatus / ErrorCode / ForCompartment / RetryAfter:
	// same shape for the Autonomous Database walk.
	AutonomousStatus         int
	AutonomousErrorCode      string
	AutonomousForCompartment string
	AutonomousRetryAfter     string

	// DBSystemsNetwork / AutonomousNetwork, when true, makes the
	// corresponding endpoint hijack the connection and return a
	// hard transport error to the client. Used to model a network
	// fault inside the per-surface walk without taking down the
	// other endpoints in the same test.
	DBSystemsNetwork  bool
	AutonomousNetwork bool

	// OKEStatus / ErrorCode / ForCompartment / RetryAfter: same
	// shape as the instances / DB knobs. Configures partial-
	// failure scenarios for the OKE clusters walk.
	OKEStatus         int
	OKEErrorCode      string
	OKEForCompartment string
	OKERetryAfter     string

	// OKENetwork, when true, makes the /clusters endpoint hijack
	// the connection and return a hard transport error to the
	// client. Used to model a network fault inside the OKE walk
	// without taking down the other endpoints in the same test.
	OKENetwork bool

	// AuthorizationGate, when true, makes the server return 401 on
	// any /instances call regardless of the InstancesStatus
	// configuration. Used to model "the OCI gateway rejected the
	// signature".
	AuthorizationGate bool

	// Call counters.
	CompartmentsCalls int
	InstancesCalls    int
	DBSystemsCalls    int
	AutonomousCalls   int
	OKECalls          int

	// Captured Authorization header from the most recent call.
	LastAuthz string
}

func newFakeOCI() *fakeOCI {
	return &fakeOCI{
		InstancesByCompartment:  map[string][]ociInstance{},
		DBSystemsByCompartment:  map[string][]dbSystem{},
		AutonomousByCompartment: map[string][]autonomousDatabase{},
		OKEByCompartment:        map[string][]okeCluster{},
	}
}

func (f *fakeOCI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.LastAuthz = r.Header.Get("Authorization")

		switch {
		case strings.HasSuffix(r.URL.Path, "/compartments"):
			f.CompartmentsCalls++
			if f.CompartmentsStatus != 0 {
				code := f.CompartmentsErrorCode
				if code == "" {
					code = "InternalServerError"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.CompartmentsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: code, Message: "mock error"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.Compartments)
			return

		case strings.HasSuffix(r.URL.Path, "/instances"):
			f.InstancesCalls++
			compartmentID := r.URL.Query().Get("compartmentId")

			if f.AuthorizationGate {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "NotAuthenticated", Message: "signature rejected"})
				return
			}

			if f.InstancesRetryAfter != "" {
				w.Header().Set("Retry-After", f.InstancesRetryAfter)
			}

			if f.InstancesStatus != 0 && (f.InstancesForCompartment == "" || f.InstancesForCompartment == compartmentID) {
				code := f.InstancesErrorCode
				if code == "" {
					code = "InternalServerError"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.InstancesStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: code, Message: "mock error"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			instances := f.InstancesByCompartment[compartmentID]
			if instances == nil {
				instances = []ociInstance{}
			}
			_ = json.NewEncoder(w).Encode(instances)
			return

		case strings.HasSuffix(r.URL.Path, "/dbSystems"):
			f.DBSystemsCalls++
			compartmentID := r.URL.Query().Get("compartmentId")

			if f.DBSystemsNetwork {
				// Hijack the connection and close it without
				// writing a response so the client sees an io.EOF
				// / "EOF" transport error rather than a 4xx body.
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
						return
					}
				}
				// Fallback if Hijack is unavailable for the
				// underlying handler — write a 500 so the test
				// still sees a non-2xx and the request fails.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if f.DBSystemsRetryAfter != "" {
				w.Header().Set("Retry-After", f.DBSystemsRetryAfter)
			}

			if f.DBSystemsStatus != 0 && (f.DBSystemsForCompartment == "" || f.DBSystemsForCompartment == compartmentID) {
				code := f.DBSystemsErrorCode
				if code == "" {
					code = "InternalServerError"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.DBSystemsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: code, Message: "mock error"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			systems := f.DBSystemsByCompartment[compartmentID]
			if systems == nil {
				systems = []dbSystem{}
			}
			_ = json.NewEncoder(w).Encode(systems)
			return

		case strings.HasSuffix(r.URL.Path, "/autonomousDatabases"):
			f.AutonomousCalls++
			compartmentID := r.URL.Query().Get("compartmentId")

			if f.AutonomousNetwork {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
						return
					}
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if f.AutonomousRetryAfter != "" {
				w.Header().Set("Retry-After", f.AutonomousRetryAfter)
			}

			if f.AutonomousStatus != 0 && (f.AutonomousForCompartment == "" || f.AutonomousForCompartment == compartmentID) {
				code := f.AutonomousErrorCode
				if code == "" {
					code = "InternalServerError"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.AutonomousStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: code, Message: "mock error"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			adbs := f.AutonomousByCompartment[compartmentID]
			if adbs == nil {
				adbs = []autonomousDatabase{}
			}
			_ = json.NewEncoder(w).Encode(adbs)
			return

		case strings.HasSuffix(r.URL.Path, "/clusters"):
			f.OKECalls++
			compartmentID := r.URL.Query().Get("compartmentId")

			if f.OKENetwork {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
						return
					}
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if f.OKERetryAfter != "" {
				w.Header().Set("Retry-After", f.OKERetryAfter)
			}

			if f.OKEStatus != 0 && (f.OKEForCompartment == "" || f.OKEForCompartment == compartmentID) {
				code := f.OKEErrorCode
				if code == "" {
					code = "InternalServerError"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.OKEStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: code, Message: "mock error"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			clusters := f.OKEByCompartment[compartmentID]
			if clusters == nil {
				clusters = []okeCluster{}
			}
			_ = json.NewEncoder(w).Encode(clusters)
			return
		}

		// Unmatched path — surface as 404 with diagnostic body so
		// test failures are obvious rather than the scanner silently
		// consuming an empty body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ociErrorBody{
			Code:    "NotFound",
			Message: fmt.Sprintf("unhandled mock path: %s", r.URL.Path),
		})
	})
}

// newScannerWithFake wires a Scanner against the supplied fake's
// httptest server. The test takes ownership of cleanup via t.Cleanup.
func newScannerWithFake(t *testing.T, fake *fakeOCI, region string) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	pemBytes, _ := generateTestKey(t)
	r := region
	if r == "" {
		r = "us-phoenix-1"
	}
	return &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		PrivateKey:  pemBytes,
		Region:      r,
		httpClient:  srv.Client(),
		ociEndpoint: srv.URL,
	}
}

// --- Helpers: instance + compartment construction --------------------

func makeCompartment(id, name string) ociCompartment {
	return ociCompartment{
		ID:             id,
		Name:           name,
		LifecycleState: "ACTIVE",
	}
}

func makeInstance(name, shape, region string, freeform map[string]string, defined map[string]map[string]interface{}) ociInstance {
	return ociInstance{
		ID:                 "ocid1.instance.oc1..." + name,
		DisplayName:        name,
		Shape:              shape,
		Region:             region,
		AvailabilityDomain: "AD-1",
		LifecycleState:     "RUNNING",
		FreeformTags:       freeform,
		DefinedTags:        defined,
	}
}

// --- Scan tests -------------------------------------------------------

func TestScan_ReturnsInstancesWithComputeInstanceSnapshotShape(t *testing.T) {
	fake := newFakeOCI()
	fake.Compartments = []ociCompartment{
		makeCompartment("ocid1.compartment.oc1..teamA", "team-a"),
		makeCompartment("ocid1.compartment.oc1..teamB", "team-b"),
	}
	// Root compartment (tenancy) gets one instance; team-a gets one;
	// team-b gets one. Three total.
	fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
		makeInstance("web-1", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"env": "prod"}, nil),
	}
	fake.InstancesByCompartment["ocid1.compartment.oc1..teamA"] = []ociInstance{
		makeInstance("web-2", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"otel-collector": "v1"}, nil),
	}
	fake.InstancesByCompartment["ocid1.compartment.oc1..teamB"] = []ociInstance{
		makeInstance("worker-1", "VM.Standard.A1.Flex", "us-phoenix-1", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 3, "expected 3 instances across tenancy + 2 child compartments")
	assert.Equal(t, credstore.ProviderOCI, res.Provider)
	assert.Equal(t, "ocid1.tenancy.oc1..aaa", res.AccountID)
	assert.False(t, res.Partial)
	assert.Empty(t, res.PartialReason)
	assert.Empty(t, res.FailedServices)
	assert.NotEmpty(t, res.ScanID)
	assert.False(t, res.ScanStartedAt.IsZero())
	assert.False(t, res.ScanCompletedAt.IsZero())
	assert.Equal(t, []string{"us-phoenix-1"}, res.Regions)

	// Confirm 3 instances calls (root + 2 children) + 1 compartments call.
	assert.Equal(t, 1, fake.CompartmentsCalls)
	assert.Equal(t, 3, fake.InstancesCalls)

	// Confirm every call carried a Signature Authorization header.
	assert.True(t, strings.HasPrefix(fake.LastAuthz, `Signature version="1"`))

	byID := map[string]int{}
	for i, c := range res.Compute {
		byID[c.ResourceID] = i
	}
	require.Contains(t, byID, "web-1")
	require.Contains(t, byID, "web-2")
	require.Contains(t, byID, "worker-1")

	web1 := res.Compute[byID["web-1"]]
	assert.Equal(t, "VM.Standard.E4.Flex", web1.InstanceType)
	assert.Equal(t, "us-phoenix-1", web1.Region)
	assert.Equal(t, "unknown", web1.OSFamily, "slice 1 leaves OSFamily=unknown per design doc §14")
	assert.Equal(t, map[string]string{"env": "prod"}, web1.Tags)
	assert.False(t, web1.HasOTel)

	web2 := res.Compute[byID["web-2"]]
	assert.True(t, web2.HasOTel)

	worker := res.Compute[byID["worker-1"]]
	assert.Nil(t, worker.Tags)
	assert.False(t, worker.HasOTel)
}

func TestScan_HasOTelTrueForFreeformOtelTag(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
	}{
		{"lowercase otel prefix", map[string]string{"otel": "v1"}},
		{"otel-collector compound", map[string]string{"otel-collector": "v1", "env": "prod"}},
		{"OTEL uppercase prefix", map[string]string{"OTEL_AGENT": "v1"}},
		{"mixed-case", map[string]string{"Otel": "v1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeOCI()
			fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
				makeInstance("inst", "VM.Standard.E4.Flex", "us-phoenix-1", tc.tags, nil),
			}
			s := newScannerWithFake(t, fake, "us-phoenix-1")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Compute, 1)
			assert.True(t, res.Compute[0].HasOTel, "expected HasOTel=true for freeform tags %v", tc.tags)
			assert.Equal(t, 1, res.InstrumentedCount)
			assert.Equal(t, 0, res.UninstrumentedCount)
		})
	}
}

func TestScan_HasOTelTrueForDefinedOtelTag(t *testing.T) {
	cases := []struct {
		name string
		defined map[string]map[string]interface{}
	}{
		{
			"otel under squadron namespace",
			map[string]map[string]interface{}{
				"Squadron": {"otel": "enabled"},
			},
		},
		{
			"OTEL_AGENT under operations namespace",
			map[string]map[string]interface{}{
				"Operations": {"OTEL_AGENT": "v1.2"},
			},
		},
		{
			"otel-collector under custom namespace",
			map[string]map[string]interface{}{
				"Custom": {"otel-collector": "running", "team": "platform"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeOCI()
			fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
				makeInstance("inst", "VM.Standard.E4.Flex", "us-phoenix-1", nil, tc.defined),
			}
			s := newScannerWithFake(t, fake, "us-phoenix-1")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Compute, 1)
			assert.True(t, res.Compute[0].HasOTel, "expected HasOTel=true for defined tags %v", tc.defined)
			assert.Equal(t, 1, res.InstrumentedCount)
		})
	}
}

func TestScan_HasOTelFalseForNoOtelTag(t *testing.T) {
	cases := []struct {
		name     string
		freeform map[string]string
		defined  map[string]map[string]interface{}
	}{
		{"no tags", nil, nil},
		{"empty tags", map[string]string{}, nil},
		{"non-otel freeform", map[string]string{"env": "prod", "team": "platform"}, nil},
		{
			"non-otel defined",
			nil,
			map[string]map[string]interface{}{
				"Custom": {"telemetry": "on", "monitoring": "yes"},
			},
		},
		{"close-but-not-prefix freeform", map[string]string{"telemetry": "on"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeOCI()
			fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
				makeInstance("inst", "VM.Standard.E4.Flex", "us-phoenix-1", tc.freeform, tc.defined),
			}
			s := newScannerWithFake(t, fake, "us-phoenix-1")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Compute, 1)
			assert.False(t, res.Compute[0].HasOTel, "expected HasOTel=false for tags %+v", tc)
			assert.Equal(t, 0, res.InstrumentedCount)
			assert.Equal(t, 1, res.UninstrumentedCount)
		})
	}
}

func TestScan_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	// compartments call succeeds with one child
	fake.Compartments = []ociCompartment{makeCompartment("ocid1.compartment.oc1..teamA", "team-a")}
	// instances call returns 403 — partial failure surfaced.
	fake.InstancesStatus = http.StatusForbidden
	fake.InstancesErrorCode = "NotAuthorizedOrNotFound"

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "permission denied at instance list is a partial-failure surface")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.FailedServices, ServiceIDCompute)
	assert.Empty(t, res.Compute)
}

func TestScan_TenancyNotFound_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.CompartmentsStatus = http.StatusNotFound
	fake.CompartmentsErrorCode = "NotAuthorizedOrNotFound"

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	// Root failure is a hard error.
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "tenancy not found")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "tenancy not found")
	assert.Contains(t, res.FailedServices, ServiceIDCompute)
	assert.Empty(t, res.Compute)
}

func TestScan_CredentialsInvalid_TokenSigningFails(t *testing.T) {
	fake := newFakeOCI()
	// Root call succeeds — populate one compartment so we proceed
	// to the instance walk. Then gate every instance call with 401.
	fake.Compartments = []ociCompartment{makeCompartment("ocid1.compartment.oc1..teamA", "team-a")}
	fake.AuthorizationGate = true

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "401 at instance list is partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "credentials invalid")
	assert.Contains(t, res.FailedServices, ServiceIDCompute)
}

func TestScan_RateLimit_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.Compartments = []ociCompartment{makeCompartment("ocid1.compartment.oc1..teamA", "team-a")}
	fake.InstancesStatus = http.StatusTooManyRequests
	fake.InstancesErrorCode = "TooManyRequests"

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "partial failures return nil error")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDCompute)
}

func TestScan_NetworkError_RecordsPartialFailure(t *testing.T) {
	// Token endpoint doesn't apply for OCI (no OAuth flow), but the
	// compartment list call goes to a dead address — the substrate
	// surfaces this as a hard error since the root call failed.
	// To exercise the per-compartment network-error partial path,
	// we run a working compartments endpoint with a fake that
	// dispatches to a closed listener for instances. The simplest
	// shape is to point the entire scanner at a dead address — that
	// yields a "tenancy_not_found"-shaped fail at root but with the
	// network-error tail.

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	pemBytes, _ := generateTestKey(t)
	s := &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		PrivateKey:  pemBytes,
		Region:      "us-phoenix-1",
		httpClient:  &http.Client{Timeout: 2 * time.Second},
		ociEndpoint: "http://" + addr,
	}
	res, err := s.Scan(context.Background())
	// Network error at root is a hard error.
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "network")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "network")
	assert.Contains(t, res.FailedServices, ServiceIDCompute)
}

func TestScan_PrivateKeyInvalid_HardError(t *testing.T) {
	s := &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		PrivateKey:  []byte("not a real PEM"),
		Region:      "us-phoenix-1",
	}
	res, err := s.Scan(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signing failed")
	// Bracketed timestamps are still set on hard failure.
	assert.NotEmpty(t, res.ScanID)
	assert.False(t, res.ScanStartedAt.IsZero())
	assert.False(t, res.ScanCompletedAt.IsZero())
	assert.Empty(t, res.Compute)
}

func TestScan_RequiresAllRequiredFields(t *testing.T) {
	pemBytes, _ := generateTestKey(t)
	cases := []struct {
		name string
		s    *Scanner
		want string
	}{
		{
			"missing TenancyOCID",
			&Scanner{UserOCID: "u", Fingerprint: "f", PrivateKey: pemBytes, Region: "r"},
			"TenancyOCID",
		},
		{
			"missing UserOCID",
			&Scanner{TenancyOCID: "t", Fingerprint: "f", PrivateKey: pemBytes, Region: "r"},
			"UserOCID",
		},
		{
			"missing Fingerprint",
			&Scanner{TenancyOCID: "t", UserOCID: "u", PrivateKey: pemBytes, Region: "r"},
			"Fingerprint",
		},
		{
			"missing PrivateKey",
			&Scanner{TenancyOCID: "t", UserOCID: "u", Fingerprint: "f", Region: "r"},
			"PrivateKey",
		},
		{
			"missing Region",
			&Scanner{TenancyOCID: "t", UserOCID: "u", Fingerprint: "f", PrivateKey: pemBytes},
			"Region",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.s.Scan(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestScan_InstrumentedCountMatchesHasOTelTrue(t *testing.T) {
	fake := newFakeOCI()
	fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
		makeInstance("a", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"otel": "v1"}, nil),
		makeInstance("b", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"otel-collector": "v1"}, nil),
		makeInstance("c", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"env": "prod"}, nil),
		makeInstance("d", "VM.Standard.E4.Flex", "us-phoenix-1", nil, nil),
		makeInstance("e", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"team": "data"}, nil),
	}
	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Compute, 5)
	assert.Equal(t, 2, res.InstrumentedCount)
	assert.Equal(t, 3, res.UninstrumentedCount)
}

func TestScan_Provider_ReturnsOCI(t *testing.T) {
	s := &Scanner{}
	assert.Equal(t, credstore.ProviderOCI, s.Provider())
}

// --- Helper-function direct tests ------------------------------------

func TestHasOTelTag_DirectCases(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"nil map", nil, false},
		{"empty map", map[string]string{}, false},
		{"single otel key", map[string]string{"otel": "v"}, true},
		{"otel prefix mixed case", map[string]string{"OtelCollector": "v"}, true},
		{"otel-suffixed key matches prefix", map[string]string{"OTEL_AGENT_VERSION": "1"}, true},
		{"telemetry is not otel", map[string]string{"telemetry": "on"}, false},
		{"otel buried mid-string does not match", map[string]string{"env-otel-prod": "v"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasOTelTag(tc.tags))
		})
	}
}

func TestFlattenTags_NamespaceDroppedAndFreeformWins(t *testing.T) {
	got := flattenTags(
		map[string]string{"env": "prod", "team": "platform"},
		map[string]map[string]interface{}{
			"Operations": {"otel": "enabled"},
			"Custom":     {"env": "should-be-overridden", "owner": "alice"},
		},
	)
	assert.Equal(t, "prod", got["env"], "freeform tag wins over defined tag on key collision")
	assert.Equal(t, "platform", got["team"])
	assert.Equal(t, "enabled", got["otel"])
	assert.Equal(t, "alice", got["owner"])
}

func TestFlattenTags_NilForEmpty(t *testing.T) {
	assert.Nil(t, flattenTags(nil, nil))
	assert.Nil(t, flattenTags(map[string]string{}, map[string]map[string]interface{}{}))
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncate(long, 200)
	assert.Equal(t, 203, len(got))
	assert.True(t, strings.HasSuffix(got, "..."))
	assert.Equal(t, "short", truncate("short", 200))
}

// --- Helpers: DB System + Autonomous Database construction ----------

func makeDBSystem(name, shape, version, mgmtStatus string, freeform map[string]string, defined map[string]map[string]interface{}) dbSystem {
	return dbSystem{
		ID:             "ocid1.dbsystem.oc1..." + name,
		DisplayName:    name,
		Shape:          shape,
		Version:        version,
		LifecycleState: "AVAILABLE",
		FreeformTags:   freeform,
		DefinedTags:    defined,
		DatabaseManagementConfig: dbSystemManagementConfig{
			DatabaseManagementStatus: mgmtStatus,
		},
	}
}

func makeAutonomousDB(name, workload string, cpuCount int, mgmtStatus string, freeform map[string]string, defined map[string]map[string]interface{}) autonomousDatabase {
	return autonomousDatabase{
		ID:                       "ocid1.autonomousdatabase.oc1..." + name,
		DisplayName:              name,
		DbName:                   name,
		DbWorkload:               workload,
		CpuCoreCount:             cpuCount,
		LifecycleState:           "AVAILABLE",
		FreeformTags:             freeform,
		DefinedTags:              defined,
		DatabaseManagementStatus: mgmtStatus,
	}
}

func findDB(t *testing.T, dbs []scanner.DatabaseInstanceSnapshot, resourceID string) scanner.DatabaseInstanceSnapshot {
	t.Helper()
	for _, d := range dbs {
		if d.ResourceID == resourceID {
			return d
		}
	}
	t.Fatalf("expected snapshot with ResourceID=%q in %v", resourceID, dbs)
	return scanner.DatabaseInstanceSnapshot{}
}

// --- DB Systems Scan tests ------------------------------------------

func TestScan_DBSystem_ReturnsDatabaseInstanceSnapshot(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{
		makeDBSystem("db-prod", "VM.Standard2.4", "19.0.0.0", "ENABLED", map[string]string{"env": "prod"}, nil),
		makeDBSystem("db-stage", "BM.DenseIO2.52", "21.0.0.0", "ENABLED", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 2, "expected 2 DB System snapshots")
	for _, d := range res.Databases {
		assert.True(t, d.DatabaseManagementEnabled, "expected DatabaseManagementEnabled=true for %s", d.ResourceID)
		assert.Equal(t, "oracle", d.Engine)
		assert.Equal(t, "oci", d.Provider)
		assert.Equal(t, "us-phoenix-1", d.Region)
	}

	prod := findDB(t, res.Databases, "db-prod")
	assert.Equal(t, "VM.Standard2.4", prod.InstanceClass)
	assert.Equal(t, "19.0.0.0", prod.EngineVersion)
	assert.Equal(t, map[string]string{"env": "prod"}, prod.Tags)
}

func TestScan_DBSystem_ManagementNotEnabled_DetectsAsFalse(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{
		makeDBSystem("db-off", "VM.Standard2.4", "19.0.0.0", "NOT_ENABLED", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.False(t, res.Databases[0].DatabaseManagementEnabled, "NOT_ENABLED status should flip the boolean false")
}

// --- Autonomous Database Scan tests ---------------------------------

func TestScan_AutonomousDatabase_ReturnsDatabaseInstanceSnapshot(t *testing.T) {
	fake := newFakeOCI()
	fake.AutonomousByCompartment["ocid1.tenancy.oc1..aaa"] = []autonomousDatabase{
		makeAutonomousDB("adb-oltp", "OLTP", 2, "NOT_ENABLED", nil, nil),
		makeAutonomousDB("adb-dw", "DW", 4, "NOT_ENABLED", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 2)
	oltp := findDB(t, res.Databases, "adb-oltp")
	dw := findDB(t, res.Databases, "adb-dw")
	assert.Equal(t, "autonomous-oltp", oltp.EngineVersion)
	assert.Equal(t, "autonomous-dw", dw.EngineVersion)
	assert.Equal(t, "ocpu-2", oltp.InstanceClass)
	assert.Equal(t, "ocpu-4", dw.InstanceClass)
	assert.Equal(t, "oracle", oltp.Engine)
	assert.Equal(t, "oci", oltp.Provider)
}

func TestScan_AutonomousDatabase_ManagementEnabled_DetectsTrue(t *testing.T) {
	fake := newFakeOCI()
	fake.AutonomousByCompartment["ocid1.tenancy.oc1..aaa"] = []autonomousDatabase{
		makeAutonomousDB("adb-on", "OLTP", 2, "ENABLED", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 1)
	assert.True(t, res.Databases[0].DatabaseManagementEnabled, "top-level ENABLED status should flip the boolean true")
}

// --- Cross-family + lifecycle tests ---------------------------------

func TestScan_OCI_DB_BothFamilies_BothWalked(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{
		makeDBSystem("db-prod", "VM.Standard2.4", "19.0.0.0", "ENABLED", nil, nil),
	}
	fake.AutonomousByCompartment["ocid1.tenancy.oc1..aaa"] = []autonomousDatabase{
		makeAutonomousDB("adb-oltp", "OLTP", 2, "ENABLED", nil, nil),
		makeAutonomousDB("adb-dw", "DW", 4, "NOT_ENABLED", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 3, "expected DB System + 2 Autonomous DBs all walked in the same scan")
	dbProd := findDB(t, res.Databases, "db-prod")
	adbOLTP := findDB(t, res.Databases, "adb-oltp")
	adbDW := findDB(t, res.Databases, "adb-dw")
	assert.Equal(t, "19.0.0.0", dbProd.EngineVersion)
	assert.Equal(t, "autonomous-oltp", adbOLTP.EngineVersion)
	assert.Equal(t, "autonomous-dw", adbDW.EngineVersion)
	assert.True(t, dbProd.DatabaseManagementEnabled)
	assert.True(t, adbOLTP.DatabaseManagementEnabled)
	assert.False(t, adbDW.DatabaseManagementEnabled)
}

func TestScan_OCI_DB_SkipsNonAvailableLifecycle(t *testing.T) {
	fake := newFakeOCI()
	terminating := makeDBSystem("db-gone", "VM.Standard2.4", "19.0.0.0", "ENABLED", nil, nil)
	terminating.LifecycleState = "TERMINATING"
	provisioning := makeAutonomousDB("adb-pending", "OLTP", 2, "ENABLED", nil, nil)
	provisioning.LifecycleState = "PROVISIONING"
	available := makeDBSystem("db-live", "VM.Standard2.4", "19.0.0.0", "ENABLED", nil, nil)

	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{terminating, available}
	fake.AutonomousByCompartment["ocid1.tenancy.oc1..aaa"] = []autonomousDatabase{provisioning}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Databases, 1, "TERMINATING / PROVISIONING rows should be skipped")
	assert.Equal(t, "db-live", res.Databases[0].ResourceID)
}

// --- Partial-failure tests ------------------------------------------

func TestScan_OCI_DB_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsStatus = http.StatusForbidden
	fake.DBSystemsErrorCode = "NotAuthorizedOrNotFound"

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "permission denied on the DB Systems walk is a partial-failure surface")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, res.FailedServices, ServiceIDDatabase)
}

func TestScan_OCI_DB_RateLimited_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsStatus = http.StatusTooManyRequests
	fake.DBSystemsErrorCode = "TooManyRequests"
	fake.DBSystemsRetryAfter = "10"

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "rate limit on the DB Systems walk is partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDDatabase)
}

func TestScan_OCI_DB_NetworkError_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsNetwork = true

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "network error on the DB Systems walk is partial — autonomous walk + compute succeeded")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "network")
	assert.Contains(t, res.FailedServices, ServiceIDDatabase)
}

// --- Compute + Database integration tests ---------------------------

func TestScan_OCI_ComputeAndDB_BothWalked(t *testing.T) {
	fake := newFakeOCI()
	fake.Compartments = []ociCompartment{makeCompartment("ocid1.compartment.oc1..teamA", "team-a")}

	fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
		makeInstance("web-1", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"otel": "v1"}, nil),
	}
	fake.InstancesByCompartment["ocid1.compartment.oc1..teamA"] = []ociInstance{
		makeInstance("web-2", "VM.Standard.E4.Flex", "us-phoenix-1", nil, nil),
	}
	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{
		makeDBSystem("db-prod", "VM.Standard2.4", "19.0.0.0", "ENABLED", nil, nil),
	}
	fake.AutonomousByCompartment["ocid1.compartment.oc1..teamA"] = []autonomousDatabase{
		makeAutonomousDB("adb-oltp", "OLTP", 2, "NOT_ENABLED", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 2, "compute walk should still see 2 instances")
	require.Len(t, res.Databases, 2, "database walk should see 1 DB System + 1 Autonomous DB")
	assert.False(t, res.Partial)

	// Both DB Systems AND Autonomous Databases endpoints were
	// called once per compartment (2 compartments = 2 calls each).
	assert.Equal(t, 2, fake.DBSystemsCalls)
	assert.Equal(t, 2, fake.AutonomousCalls)
}

func TestScan_OCI_DB_TagsFlattening(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{
		makeDBSystem(
			"db-tagged",
			"VM.Standard2.4",
			"19.0.0.0",
			"ENABLED",
			map[string]string{"env": "prod", "team": "platform"},
			map[string]map[string]interface{}{
				"Operations": {"owner": "alice", "team": "should-be-overridden"},
			},
		),
	}
	fake.AutonomousByCompartment["ocid1.tenancy.oc1..aaa"] = []autonomousDatabase{
		makeAutonomousDB(
			"adb-tagged",
			"OLTP",
			2,
			"ENABLED",
			map[string]string{"env": "stage"},
			map[string]map[string]interface{}{
				"Custom": {"owner": "bob"},
			},
		),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 2)

	dbs := findDB(t, res.Databases, "db-tagged")
	assert.Equal(t, "prod", dbs.Tags["env"], "DB System tag flattening should keep freeform")
	assert.Equal(t, "platform", dbs.Tags["team"], "freeform should win over defined on key collision")
	assert.Equal(t, "alice", dbs.Tags["owner"], "defined-tag namespace stripped, inner key surfaced")

	adb := findDB(t, res.Databases, "adb-tagged")
	assert.Equal(t, "stage", adb.Tags["env"])
	assert.Equal(t, "bob", adb.Tags["owner"])
}

func TestScan_OCI_DB_ProviderFieldSet(t *testing.T) {
	fake := newFakeOCI()
	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{
		makeDBSystem("db-prod", "VM.Standard2.4", "19.0.0.0", "ENABLED", nil, nil),
	}
	fake.AutonomousByCompartment["ocid1.tenancy.oc1..aaa"] = []autonomousDatabase{
		makeAutonomousDB("adb-oltp", "OLTP", 2, "NOT_ENABLED", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Databases, 2)
	for _, d := range res.Databases {
		assert.Equal(t, "oci", d.Provider, "every database snapshot should carry Provider=oci")
	}
}

// --- Helpers: OKE cluster construction ------------------------------

func makeOKECluster(name, version, lifecycle string, freeform map[string]string, defined map[string]map[string]interface{}) okeCluster {
	return okeCluster{
		ID:                "ocid1.cluster.oc1.iad.aaaaaaaa" + name,
		Name:              name,
		CompartmentID:     "ocid1.tenancy.oc1..aaa",
		KubernetesVersion: version,
		LifecycleState:    lifecycle,
		FreeformTags:      freeform,
		DefinedTags:       defined,
	}
}

func findCluster(t *testing.T, clusters []scanner.ClusterSnapshot, name string) scanner.ClusterSnapshot {
	t.Helper()
	for _, c := range clusters {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("expected cluster with Name=%q in %v", name, clusters)
	return scanner.ClusterSnapshot{}
}

// --- OKE Scan tests --------------------------------------------------

// TestScan_OKE_ReturnsClusterSnapshot covers design doc §11 test 6:
// OKE clusters with operations-insights-enabled=true freeform tag
// surface as ClusterSnapshot rows with OperationsInsightsEnabled=true.
func TestScan_OKE_ReturnsClusterSnapshot(t *testing.T) {
	fake := newFakeOCI()
	fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
		makeOKECluster("production-oke", "v1.29.1", "ACTIVE",
			map[string]string{"env": "prod", "operations-insights-enabled": "true"}, nil),
		makeOKECluster("staging-oke", "v1.30.0", "ACTIVE",
			map[string]string{"operations-insights-enabled": "true"}, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 2, "expected 2 OKE cluster snapshots")
	for _, c := range res.Clusters {
		assert.True(t, c.OperationsInsightsEnabled, "expected OperationsInsightsEnabled=true for %s", c.Name)
		assert.Equal(t, "oci", c.Provider)
		assert.Equal(t, "us-phoenix-1", c.Region)
		assert.Equal(t, "ACTIVE", c.Status)
	}

	prod := findCluster(t, res.Clusters, "production-oke")
	assert.Equal(t, "1.29", prod.KubernetesVersion, "extractMajorMinor normalizes v1.29.1 to 1.29")
	assert.Equal(t, map[string]string{"env": "prod", "operations-insights-enabled": "true"}, prod.Tags)
	assert.Contains(t, prod.ResourceID, "ocid1.cluster.oc1.iad.")
}

// TestScan_OKE_NoOpsInsightsTag_DetectsAsFalse covers design doc §11 test 8:
// OKE cluster without the tag → OperationsInsightsEnabled=false (the
// proposer will emit oke-ops-insights-enable in chunk 5).
func TestScan_OKE_NoOpsInsightsTag_DetectsAsFalse(t *testing.T) {
	fake := newFakeOCI()
	fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
		makeOKECluster("untagged-oke", "v1.29.1", "ACTIVE",
			map[string]string{"env": "prod", "team": "platform"}, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 1)
	assert.False(t, res.Clusters[0].OperationsInsightsEnabled,
		"absence of operations-insights-enabled tag should flip the boolean false")
}

// TestScan_OKE_OpsInsightsTagFalseValue_DetectsAsFalse — tag key
// present but value "false" → OperationsInsightsEnabled=false. The
// rule fires only when both key matches AND value (trimmed,
// case-insensitive) equals "true".
func TestScan_OKE_OpsInsightsTagFalseValue_DetectsAsFalse(t *testing.T) {
	fake := newFakeOCI()
	fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
		makeOKECluster("explicitly-off-oke", "v1.29.1", "ACTIVE",
			map[string]string{"operations-insights-enabled": "false"}, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 1)
	assert.False(t, res.Clusters[0].OperationsInsightsEnabled,
		"tag value 'false' should not flip the boolean true")
}

// TestScan_OKE_CaseInsensitiveKey covers design doc §11 test 7 (key
// half): operators may capitalize the tag key
// ("Operations-Insights-Enabled") and the rule still fires.
func TestScan_OKE_CaseInsensitiveKey(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
	}{
		{"Title-Case key", map[string]string{"Operations-Insights-Enabled": "true"}},
		{"upper-case key", map[string]string{"OPERATIONS-INSIGHTS-ENABLED": "true"}},
		{"Mixed-Case key", map[string]string{"OpeRations-INsights-EnAbled": "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeOCI()
			fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
				makeOKECluster("case-key-oke", "v1.29.1", "ACTIVE", tc.tags, nil),
			}
			s := newScannerWithFake(t, fake, "us-phoenix-1")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Clusters, 1)
			assert.True(t, res.Clusters[0].OperationsInsightsEnabled,
				"case-insensitive key match should fire for tags %v", tc.tags)
		})
	}
}

// TestScan_OKE_CaseInsensitiveValue covers design doc §11 test 7
// (value half): operators may use any casing on the tag value
// ("TRUE", "True") and the rule still fires.
func TestScan_OKE_CaseInsensitiveValue(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
	}{
		{"upper-case value", map[string]string{"operations-insights-enabled": "TRUE"}},
		{"Title-case value", map[string]string{"operations-insights-enabled": "True"}},
		{"mixed-case value", map[string]string{"operations-insights-enabled": "TrUe"}},
		{"value with whitespace", map[string]string{"operations-insights-enabled": "  true  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeOCI()
			fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
				makeOKECluster("case-value-oke", "v1.29.1", "ACTIVE", tc.tags, nil),
			}
			s := newScannerWithFake(t, fake, "us-phoenix-1")
			res, err := s.Scan(context.Background())
			require.NoError(t, err)
			require.Len(t, res.Clusters, 1)
			assert.True(t, res.Clusters[0].OperationsInsightsEnabled,
				"case-insensitive value match should fire for tags %v", tc.tags)
		})
	}
}

// TestScan_OKE_VersionExtraction covers extractMajorMinor's
// leading-v normalization: "v1.29.1" → "1.29"; "1.30.0" → "1.30";
// "v1.28" → "1.28".
func TestScan_OKE_VersionExtraction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"v1.29.1", "1.29"},
		{"1.30.0", "1.30"},
		{"v1.28", "1.28"},
		{"v1.27.5-rc.1", "1.27"},
		{"v2.0.0", "2.0"},
		{"v1", "1"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := extractMajorMinor(tc.in)
			assert.Equal(t, tc.want, got, "extractMajorMinor(%q)", tc.in)
		})
	}
}

// TestScan_OKE_SkipsNonActiveLifecycle — clusters mid-create /
// mid-delete / mid-update have no observability surface; the
// scanner skips them so the Inventory tab + proposer don't recommend
// against a transient state.
func TestScan_OKE_SkipsNonActiveLifecycle(t *testing.T) {
	fake := newFakeOCI()
	fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
		makeOKECluster("creating-oke", "v1.29.1", "CREATING",
			map[string]string{"operations-insights-enabled": "true"}, nil),
		makeOKECluster("deleting-oke", "v1.29.1", "DELETING",
			map[string]string{"operations-insights-enabled": "true"}, nil),
		makeOKECluster("updating-oke", "v1.29.1", "UPDATING",
			map[string]string{"operations-insights-enabled": "true"}, nil),
		makeOKECluster("failed-oke", "v1.29.1", "FAILED",
			map[string]string{"operations-insights-enabled": "true"}, nil),
		makeOKECluster("live-oke", "v1.29.1", "ACTIVE",
			map[string]string{"operations-insights-enabled": "true"}, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Clusters, 1,
		"CREATING / DELETING / UPDATING / FAILED clusters should be skipped")
	assert.Equal(t, "live-oke", res.Clusters[0].Name)
}

// --- OKE partial-failure tests --------------------------------------

func TestScan_OKE_PermissionDenied_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.OKEStatus = http.StatusForbidden
	fake.OKEErrorCode = "NotAuthorizedOrNotFound"

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "permission denied on the OKE walk is a partial-failure surface")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "permission denied")
	assert.Contains(t, strings.ToLower(res.PartialReason), "cluster-family",
		"403 hint should name the cluster-family policy statement")
	assert.Contains(t, res.FailedServices, ServiceIDKubernetes)
	assert.Empty(t, res.Clusters)
}

func TestScan_OKE_RateLimited_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.OKEStatus = http.StatusTooManyRequests
	fake.OKEErrorCode = "TooManyRequests"
	fake.OKERetryAfter = "10"

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "rate limit on the OKE walk is partial, not hard")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "rate limit")
	assert.Contains(t, res.FailedServices, ServiceIDKubernetes)
}

func TestScan_OKE_NetworkError_RecordsPartialFailure(t *testing.T) {
	fake := newFakeOCI()
	fake.OKENetwork = true

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err, "network error on the OKE walk is partial — compute + databases succeeded")

	assert.True(t, res.Partial)
	assert.Contains(t, strings.ToLower(res.PartialReason), "network")
	assert.Contains(t, res.FailedServices, ServiceIDKubernetes)
}

func TestScan_OKE_ProviderFieldSet(t *testing.T) {
	fake := newFakeOCI()
	fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
		makeOKECluster("oke-1", "v1.29.1", "ACTIVE",
			map[string]string{"operations-insights-enabled": "true"}, nil),
		makeOKECluster("oke-2", "v1.30.0", "ACTIVE", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Clusters, 2)
	for _, c := range res.Clusters {
		assert.Equal(t, "oci", c.Provider, "every cluster snapshot should carry Provider=oci")
	}
}

// TestScan_OCI_ComputeDBAndOKE_AllThreeWalked verifies the three
// per-surface walks (compute + database + kubernetes) coexist in a
// single Scan invocation, share the same signing path, and
// produce one Result with all three categories populated.
func TestScan_OCI_ComputeDBAndOKE_AllThreeWalked(t *testing.T) {
	fake := newFakeOCI()
	fake.Compartments = []ociCompartment{makeCompartment("ocid1.compartment.oc1..teamA", "team-a")}

	fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
		makeInstance("web-1", "VM.Standard.E4.Flex", "us-phoenix-1", map[string]string{"otel": "v1"}, nil),
	}
	fake.InstancesByCompartment["ocid1.compartment.oc1..teamA"] = []ociInstance{
		makeInstance("web-2", "VM.Standard.E4.Flex", "us-phoenix-1", nil, nil),
	}
	fake.DBSystemsByCompartment["ocid1.tenancy.oc1..aaa"] = []dbSystem{
		makeDBSystem("db-prod", "VM.Standard2.4", "19.0.0.0", "ENABLED", nil, nil),
	}
	fake.AutonomousByCompartment["ocid1.compartment.oc1..teamA"] = []autonomousDatabase{
		makeAutonomousDB("adb-oltp", "OLTP", 2, "NOT_ENABLED", nil, nil),
	}
	fake.OKEByCompartment["ocid1.tenancy.oc1..aaa"] = []okeCluster{
		makeOKECluster("oke-platform", "v1.29.1", "ACTIVE",
			map[string]string{"operations-insights-enabled": "true"}, nil),
	}
	fake.OKEByCompartment["ocid1.compartment.oc1..teamA"] = []okeCluster{
		makeOKECluster("oke-team-a", "v1.30.0", "ACTIVE", nil, nil),
	}

	s := newScannerWithFake(t, fake, "us-phoenix-1")
	res, err := s.Scan(context.Background())
	require.NoError(t, err)

	require.Len(t, res.Compute, 2, "compute walk should still see 2 instances")
	require.Len(t, res.Databases, 2, "database walk should see 1 DB System + 1 Autonomous DB")
	require.Len(t, res.Clusters, 2, "OKE walk should see 2 clusters across both compartments")
	assert.False(t, res.Partial)

	// Each surface called once per compartment (2 compartments).
	assert.Equal(t, 2, fake.DBSystemsCalls)
	assert.Equal(t, 2, fake.AutonomousCalls)
	assert.Equal(t, 2, fake.OKECalls)

	platform := findCluster(t, res.Clusters, "oke-platform")
	teamA := findCluster(t, res.Clusters, "oke-team-a")
	assert.True(t, platform.OperationsInsightsEnabled, "tagged cluster should be detected as instrumented")
	assert.False(t, teamA.OperationsInsightsEnabled, "untagged cluster should be detected as not instrumented")
}

// --- Direct helper tests --------------------------------------------

func TestClusterHasOperationsInsights_DirectCases(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"nil map", nil, false},
		{"empty map", map[string]string{}, false},
		{"canonical key + value", map[string]string{"operations-insights-enabled": "true"}, true},
		{"upper-case key", map[string]string{"OPERATIONS-INSIGHTS-ENABLED": "true"}, true},
		{"mixed-case key", map[string]string{"Operations-Insights-Enabled": "true"}, true},
		{"value upper-case", map[string]string{"operations-insights-enabled": "TRUE"}, true},
		{"value Title-case", map[string]string{"operations-insights-enabled": "True"}, true},
		{"value with whitespace", map[string]string{"operations-insights-enabled": "  true  "}, true},
		{"value false", map[string]string{"operations-insights-enabled": "false"}, false},
		{"value empty", map[string]string{"operations-insights-enabled": ""}, false},
		{"non-matching key", map[string]string{"observability-enabled": "true"}, false},
		{"key partial match", map[string]string{"operations-insights": "true"}, false},
		{"multiple tags one matches", map[string]string{"env": "prod", "Operations-Insights-Enabled": "TRUE"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, clusterHasOperationsInsights(tc.tags))
		})
	}
}

func TestIsClusterActive_DirectCases(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"ACTIVE", true},
		{"active", true},
		{"Active", true},
		{"CREATING", false},
		{"DELETING", false},
		{"UPDATING", false},
		{"FAILED", false},
		{"DELETED", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			assert.Equal(t, tc.want, isClusterActive(tc.state))
		})
	}
}

// TestScan_PrivateKeyMemoized_OnlyParsedOnce verifies the
// per-scan single-parse contract: calling Scan twice on the same
// Scanner instance parses the PEM bytes once.
func TestScan_PrivateKeyMemoized_OnlyParsedOnce(t *testing.T) {
	fake := newFakeOCI()
	fake.InstancesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociInstance{
		makeInstance("a", "VM.Standard.E4.Flex", "us-phoenix-1", nil, nil),
	}
	s := newScannerWithFake(t, fake, "us-phoenix-1")

	// Run two scans; the second should reuse parsedKey.
	_, err := s.Scan(context.Background())
	require.NoError(t, err)
	first := s.parsedKey
	require.NotNil(t, first)

	_, err = s.Scan(context.Background())
	require.NoError(t, err)
	assert.Same(t, first, s.parsedKey, "parsed RSA key should be memoized across Scan calls")
}
