// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// inventory_lastseen_test.go — trace integration slice 1 chunk 4
// (v0.89.77, #708 Stream 106). Tests the per-provider scan response
// annotation pipeline end-to-end:
//
//   1. AWS / GCP / Azure / OCI scan handlers each get the same
//      three-tier coverage trio:
//        - LastSeenAt_PopulatedFromTraceIndex (happy path)
//        - LastSeenAt_NullWhenNoTraces (cold-start)
//        - LastSeenAt_NilTraceIndex_StillWorks (disabled mode)
//   2. Per-provider Database + Cluster snapshots each get one
//      happy-path test exercising the matching projection helper.
//
// stubLookup is a self-contained TraceIndexLookup implementation —
// no dependency on the chunk-1 traceindex.Index machinery.

// --- stub --------------------------------------------------------------

type stubLookup struct {
	mu     sync.Mutex
	values map[string]time.Time
	err    error
	calls  int
}

func (s *stubLookup) LastSeenAt(_ context.Context, key string) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return time.Time{}, false, s.err
	}
	v, ok := s.values[key]
	return v, ok, nil
}

// --- projection helper unit tests --------------------------------------

// (Projection helpers live in the traceindex package; testing them
// through the annotation pipeline rather than the package boundary
// keeps the surface readable. Per-key tests live in
// traceindex/inventory_keying_test.go below.)

// --- AWS --------------------------------------------------------------

const awsAccount = "123456789012"

func awsScanResultForLastSeen() *scanner.Result {
	return &scanner.Result{
		ScanID:    "scan-uuid",
		Provider:  credstore.ProviderAWS,
		AccountID: awsAccount,
		Regions:   []string{"us-east-1"},
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "i-aaa", InstanceType: "t3.micro", HasOTel: true, Region: "us-east-1"},
			{ResourceID: "i-bbb", InstanceType: "m5.large", HasOTel: false, Region: "us-east-1"},
		},
		Databases: []scanner.DatabaseInstanceSnapshot{
			{ResourceID: "db-prod", Engine: "postgres", Region: "us-east-1"},
		},
		Clusters: []scanner.ClusterSnapshot{
			{ResourceID: "arn:aws:eks:us-east-1:123:cluster/prod", Name: "prod-cluster", Region: "us-east-1"},
		},
		InstrumentedCount:   1,
		UninstrumentedCount: 1,
	}
}

func newAWSHandlerForLastSeen(t *testing.T, result *scanner.Result, lookup TraceIndexLookup) *DiscoveryHandlers {
	t.Helper()
	conn := &credstore.CloudConnection{
		AccountID:      awsAccount,
		Provider:       credstore.ProviderAWS,
		ConnectionType: credstore.ConnectionAPIDiscovered,
		Regions:        []string{"us-east-1"},
		Credentials:    []byte("ciphertext"),
	}
	store := &spyStore{getResult: conn}
	ms := &mockScanner{result: result}
	h := NewDiscoveryHandlers(store, zap.NewNop())
	h.WithAWSScannerFactory(func(_ *credstore.CloudConnection) (DiscoveryScanner, error) {
		return ms, nil
	})
	if lookup != nil {
		h.WithTraceIndex(lookup)
	}
	return h
}

// awsLastSeenRow is a minimal projection of the JSON wire shape used
// to assert per-row last_seen_at without depending on every field.
type awsLastSeenRow struct {
	Compute []struct {
		ResourceID string  `json:"resource_id"`
		LastSeenAt *string `json:"last_seen_at"`
	} `json:"compute"`
	Databases []struct {
		ResourceID string  `json:"resource_id"`
		LastSeenAt *string `json:"last_seen_at"`
	} `json:"databases"`
	Clusters []struct {
		ResourceID string  `json:"resource_id"`
		Name       string  `json:"name"`
		LastSeenAt *string `json:"last_seen_at"`
	} `json:"clusters"`
}

func TestAWSScan_LastSeenAt_PopulatedFromTraceIndex(t *testing.T) {
	now := time.Date(2026, 6, 23, 14, 32, 0, 0, time.UTC)
	lookup := &stubLookup{values: map[string]time.Time{
		"aws:" + awsAccount + ":i-aaa":                 now,
		"aws:" + awsAccount + ":db:postgresql:db-prod": now.Add(-5 * time.Minute),
		"aws:" + awsAccount + ":k8s:prod-cluster":      now.Add(-1 * time.Hour),
	}}
	h := newAWSHandlerForLastSeen(t, awsScanResultForLastSeen(), lookup)

	w := doScanRequest(h, awsAccount, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp awsLastSeenRow
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Compute: i-aaa populated, i-bbb stays nil (no row in stub).
	if len(resp.Compute) != 2 {
		t.Fatalf("compute rows = %d, want 2", len(resp.Compute))
	}
	if resp.Compute[0].ResourceID != "i-aaa" || resp.Compute[0].LastSeenAt == nil {
		t.Errorf("compute[0]=%+v want i-aaa with last_seen_at set", resp.Compute[0])
	}
	if resp.Compute[1].LastSeenAt != nil {
		t.Errorf("compute[1].last_seen_at = %v, want nil for missing row", *resp.Compute[1].LastSeenAt)
	}
	if len(resp.Databases) != 1 || resp.Databases[0].LastSeenAt == nil {
		t.Errorf("databases[0]=%+v want last_seen_at set", resp.Databases)
	}
	if len(resp.Clusters) != 1 || resp.Clusters[0].LastSeenAt == nil {
		t.Errorf("clusters[0]=%+v want last_seen_at set", resp.Clusters)
	}
	// Three lookup calls — one per snapshot row.
	if lookup.calls != 4 {
		// 2 compute + 1 db + 1 cluster = 4
		t.Errorf("lookup.calls = %d, want 4", lookup.calls)
	}
}

func TestAWSScan_LastSeenAt_NullWhenNoTraces(t *testing.T) {
	lookup := &stubLookup{values: map[string]time.Time{}}
	h := newAWSHandlerForLastSeen(t, awsScanResultForLastSeen(), lookup)
	w := doScanRequest(h, awsAccount, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp awsLastSeenRow
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, row := range resp.Compute {
		if row.LastSeenAt != nil {
			t.Errorf("compute[%d].last_seen_at = %v, want nil", i, *row.LastSeenAt)
		}
	}
	for i, row := range resp.Databases {
		if row.LastSeenAt != nil {
			t.Errorf("databases[%d].last_seen_at = %v, want nil", i, *row.LastSeenAt)
		}
	}
	for i, row := range resp.Clusters {
		if row.LastSeenAt != nil {
			t.Errorf("clusters[%d].last_seen_at = %v, want nil", i, *row.LastSeenAt)
		}
	}
}

func TestAWSScan_LastSeenAt_NilTraceIndex_StillWorks(t *testing.T) {
	h := newAWSHandlerForLastSeen(t, awsScanResultForLastSeen(), nil)
	w := doScanRequest(h, awsAccount, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// last_seen_at must not appear in the wire body at all when no
	// annotation ran — omitempty drops the field.
	if containsLastSeen(body) {
		t.Errorf("expected no last_seen_at in body; got: %s", body)
	}
}

// TestAWSScan_LastSeenAt_FlakyIndex_DoesNotBreakResponse pins the
// constraint that a lookup error never sinks the scan endpoint.
func TestAWSScan_LastSeenAt_FlakyIndex_DoesNotBreakResponse(t *testing.T) {
	lookup := &stubLookup{err: errors.New("boom: store unreachable")}
	h := newAWSHandlerForLastSeen(t, awsScanResultForLastSeen(), lookup)
	w := doScanRequest(h, awsAccount, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

func containsLastSeen(s string) bool {
	for i := 0; i+len("last_seen_at") <= len(s); i++ {
		if s[i:i+len("last_seen_at")] == "last_seen_at" {
			return true
		}
	}
	return false
}

// --- GCP --------------------------------------------------------------

func TestGCPScan_LastSeenAt_PopulatedFromTraceIndex(t *testing.T) {
	now := time.Date(2026, 6, 23, 14, 32, 0, 0, time.UTC)
	lookup := &stubLookup{values: map[string]time.Time{
		"gcp:sandbox-12345:i-gce-1":              now.Add(-2 * time.Minute),
		"gcp:sandbox-12345:db:postgresql:gcp-db": now.Add(-7 * time.Minute),
		"gcp:sandbox-12345:k8s:gke-prod":         now.Add(-3 * time.Hour),
	}}
	fs := &fakeScanner{result: &scanner.Result{
		ScanID: "scan-gcp",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "i-gce-1", HasOTel: true},
			{ResourceID: "i-gce-2", HasOTel: false},
		},
		Databases: []scanner.DatabaseInstanceSnapshot{
			{ResourceID: "gcp-db", Engine: "postgres"},
		},
		Clusters: []scanner.ClusterSnapshot{
			{ResourceID: "projects/sandbox-12345/locations/us-central1/clusters/gke-prod", Name: "gke-prod"},
		},
	}}
	factory := &fakeGCPScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newGCPTestHandlers(t, audit, factory)
	h.WithGCPTraceIndex(lookup)
	r := newGCPRouter(h)
	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp gcpScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Compute) != 2 {
		t.Fatalf("compute rows = %d", len(resp.Compute))
	}
	if resp.Compute[0].LastSeenAt == nil {
		t.Errorf("compute[0].last_seen_at = nil, want populated")
	}
	if resp.Compute[1].LastSeenAt != nil {
		t.Errorf("compute[1].last_seen_at = %v, want nil", *resp.Compute[1].LastSeenAt)
	}
	if len(resp.Databases) != 1 || resp.Databases[0].LastSeenAt == nil {
		t.Errorf("databases[0].last_seen_at expected populated")
	}
	if len(resp.Clusters) != 1 || resp.Clusters[0].LastSeenAt == nil {
		t.Errorf("clusters[0].last_seen_at expected populated")
	}
}

func TestGCPScan_LastSeenAt_NullWhenNoTraces(t *testing.T) {
	fs := &fakeScanner{result: &scanner.Result{
		ScanID: "scan-gcp",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "i-gce-1", HasOTel: true},
		},
	}}
	factory := &fakeGCPScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newGCPTestHandlers(t, audit, factory)
	h.WithGCPTraceIndex(&stubLookup{values: map[string]time.Time{}})
	r := newGCPRouter(h)
	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp gcpScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Compute[0].LastSeenAt != nil {
		t.Errorf("compute[0].last_seen_at = %v, want nil", *resp.Compute[0].LastSeenAt)
	}
}

func TestGCPScan_LastSeenAt_NilTraceIndex_StillWorks(t *testing.T) {
	fs := &fakeScanner{result: &scanner.Result{
		ScanID: "scan-gcp",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "i-gce-1", HasOTel: true},
		},
	}}
	factory := &fakeGCPScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newGCPTestHandlers(t, audit, factory)
	// No WithGCPTraceIndex call.
	r := newGCPRouter(h)
	conn := seedGCPConnection(t, store, key, "Prod", "sandbox-12345", "us-central1")

	w := gcpDoRequest(r, http.MethodPost, "/api/v1/discovery/gcp/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if containsLastSeen(w.Body.String()) {
		t.Errorf("expected no last_seen_at in body with nil traceIndex")
	}
}

// --- Azure --------------------------------------------------------------

func TestAzureScan_LastSeenAt_PopulatedFromTraceIndex(t *testing.T) {
	now := time.Date(2026, 6, 23, 14, 32, 0, 0, time.UTC)
	lookup := &stubLookup{values: map[string]time.Time{
		"azure:" + azureTestSubscriptionID + ":vm-prod":            now,
		"azure:" + azureTestSubscriptionID + ":db:mssql:az-sql-db": now,
		"azure:" + azureTestSubscriptionID + ":k8s:aks-prod":       now,
	}}
	fs := &fakeAzureScanner{result: &scanner.Result{
		ScanID: "scan-az",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "vm-prod", HasOTel: true},
		},
		Databases: []scanner.DatabaseInstanceSnapshot{
			{ResourceID: "az-sql-db", Engine: "sqlserver"},
		},
		Clusters: []scanner.ClusterSnapshot{
			{ResourceID: "/subscriptions/.../aks-prod", Name: "aks-prod"},
		},
	}}
	factory := &fakeAzureScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newAzureTestHandlers(t, audit, factory)
	h.WithAzureTraceIndex(lookup)
	r := newAzureRouter(h)
	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp azureScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Compute) != 1 || resp.Compute[0].LastSeenAt == nil {
		t.Errorf("compute[0].last_seen_at expected populated; resp=%+v", resp.Compute)
	}
	if len(resp.Databases) != 1 || resp.Databases[0].LastSeenAt == nil {
		t.Errorf("databases[0].last_seen_at expected populated")
	}
	if len(resp.Clusters) != 1 || resp.Clusters[0].LastSeenAt == nil {
		t.Errorf("clusters[0].last_seen_at expected populated")
	}
}

func TestAzureScan_LastSeenAt_NullWhenNoTraces(t *testing.T) {
	fs := &fakeAzureScanner{result: &scanner.Result{
		ScanID: "scan-az",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "vm-prod", HasOTel: true},
		},
	}}
	factory := &fakeAzureScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newAzureTestHandlers(t, audit, factory)
	h.WithAzureTraceIndex(&stubLookup{values: map[string]time.Time{}})
	r := newAzureRouter(h)
	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp azureScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Compute[0].LastSeenAt != nil {
		t.Errorf("compute[0].last_seen_at = %v, want nil", *resp.Compute[0].LastSeenAt)
	}
}

func TestAzureScan_LastSeenAt_NilTraceIndex_StillWorks(t *testing.T) {
	fs := &fakeAzureScanner{result: &scanner.Result{
		ScanID: "scan-az",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "vm-prod", HasOTel: true},
		},
	}}
	factory := &fakeAzureScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newAzureTestHandlers(t, audit, factory)
	r := newAzureRouter(h)
	conn := seedAzureConnection(t, store, key, "Prod", azureTestTenantID, azureTestSubscriptionID, azureTestClientID, "eastus")

	w := azureDoRequest(r, http.MethodPost, "/api/v1/discovery/azure/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if containsLastSeen(w.Body.String()) {
		t.Errorf("expected no last_seen_at in body with nil traceIndex")
	}
}

// --- OCI ----------------------------------------------------------------

func TestOCIScan_LastSeenAt_PopulatedFromTraceIndex(t *testing.T) {
	now := time.Date(2026, 6, 23, 14, 32, 0, 0, time.UTC)
	lookup := &stubLookup{values: map[string]time.Time{
		"oci:" + ociTestTenancyOCID + ":ocid1.instance.oc1..xxxx": now,
		"oci:" + ociTestTenancyOCID + ":db:oracle:oci-db":         now,
		"oci:" + ociTestTenancyOCID + ":k8s:oke-prod":             now,
	}}
	fs := &fakeOCIScanner{result: &scanner.Result{
		ScanID: "scan-oci",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "ocid1.instance.oc1..xxxx", HasOTel: true},
		},
		Databases: []scanner.DatabaseInstanceSnapshot{
			{ResourceID: "oci-db", Engine: "oracle"},
		},
		Clusters: []scanner.ClusterSnapshot{
			{ResourceID: "ocid1.cluster.oc1..yyyy", Name: "oke-prod"},
		},
	}}
	factory := &fakeOCIScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newOCITestHandlers(t, audit, factory)
	h.WithOCITraceIndex(lookup)
	r := newOCIRouter(h)
	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp ociScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Compute) != 1 || resp.Compute[0].LastSeenAt == nil {
		t.Errorf("compute[0].last_seen_at expected populated")
	}
	if len(resp.Databases) != 1 || resp.Databases[0].LastSeenAt == nil {
		t.Errorf("databases[0].last_seen_at expected populated")
	}
	if len(resp.Clusters) != 1 || resp.Clusters[0].LastSeenAt == nil {
		t.Errorf("clusters[0].last_seen_at expected populated")
	}
}

func TestOCIScan_LastSeenAt_NullWhenNoTraces(t *testing.T) {
	fs := &fakeOCIScanner{result: &scanner.Result{
		ScanID: "scan-oci",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "ocid1.instance.oc1..xxxx", HasOTel: true},
		},
	}}
	factory := &fakeOCIScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newOCITestHandlers(t, audit, factory)
	h.WithOCITraceIndex(&stubLookup{values: map[string]time.Time{}})
	r := newOCIRouter(h)
	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp ociScanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Compute[0].LastSeenAt != nil {
		t.Errorf("compute[0].last_seen_at = %v, want nil", *resp.Compute[0].LastSeenAt)
	}
}

func TestOCIScan_LastSeenAt_NilTraceIndex_StillWorks(t *testing.T) {
	fs := &fakeOCIScanner{result: &scanner.Result{
		ScanID: "scan-oci",
		Compute: []scanner.ComputeInstanceSnapshot{
			{ResourceID: "ocid1.instance.oc1..xxxx", HasOTel: true},
		},
	}}
	factory := &fakeOCIScannerFactory{scanner: fs}
	audit := &discoveryRecordingAudit{}
	h, store, key := newOCITestHandlers(t, audit, factory)
	r := newOCIRouter(h)
	conn := seedOCIConnection(t, store, key, "Prod", ociTestTenancyOCID, ociTestUserOCID, ociTestFingerprint, ociTestRegion)

	w := ociDoRequest(r, http.MethodPost, "/api/v1/discovery/oci/connections/"+conn.ID+"/scan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if containsLastSeen(w.Body.String()) {
		t.Errorf("expected no last_seen_at in body with nil traceIndex")
	}
}

// --- Annotate helpers — direct unit tests ----------------------------

// TestAnnotateCompute_NilLookup is a defense-in-depth no-op check: the
// scan-handler branch already guards on lookup==nil, but the helpers
// MUST also no-op when called with a nil lookup directly.
func TestAnnotateCompute_NilLookup(t *testing.T) {
	snaps := []scanner.ComputeInstanceSnapshot{{ResourceID: "i-aaa"}}
	AnnotateComputeWithLastSeen(context.Background(), nil, "aws", "123", snaps, zap.NewNop())
	if snaps[0].LastSeenAt != nil {
		t.Errorf("expected LastSeenAt to stay nil, got %v", snaps[0].LastSeenAt)
	}
}

// TestAnnotateCompute_EmptyResourceIDSkipped pins the projection
// guard: a row with no ResourceID can't produce a valid key, so the
// helper skips it without calling the lookup.
func TestAnnotateCompute_EmptyResourceIDSkipped(t *testing.T) {
	lookup := &stubLookup{values: map[string]time.Time{}}
	snaps := []scanner.ComputeInstanceSnapshot{{ResourceID: ""}, {ResourceID: "i-aaa"}}
	AnnotateComputeWithLastSeen(context.Background(), lookup, "aws", "123", snaps, zap.NewNop())
	if lookup.calls != 1 {
		t.Errorf("lookup.calls = %d, want 1 (empty resource skipped)", lookup.calls)
	}
}

// TestAnnotateCompute_FlakyLookup_NoAbort pins constraint 4: an
// error from the lookup must NOT abort the iteration — subsequent
// rows should still be attempted (they'll log + skip too, but the
// helper must not return early).
func TestAnnotateCompute_FlakyLookup_NoAbort(t *testing.T) {
	lookup := &stubLookup{err: errors.New("flaky")}
	snaps := []scanner.ComputeInstanceSnapshot{
		{ResourceID: "i-aaa"},
		{ResourceID: "i-bbb"},
	}
	AnnotateComputeWithLastSeen(context.Background(), lookup, "aws", "123", snaps, zap.NewNop())
	if lookup.calls != 2 {
		t.Errorf("lookup.calls = %d, want 2 (both rows attempted)", lookup.calls)
	}
	for i := range snaps {
		if snaps[i].LastSeenAt != nil {
			t.Errorf("snaps[%d].LastSeenAt = %v, want nil on error", i, snaps[i].LastSeenAt)
		}
	}
}
