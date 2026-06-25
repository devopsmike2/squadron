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
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// inventory_cold_start_test.go — Cold-start latency analysis slice 1
// chunk 3 (v0.89.115, #753 Stream 151). Pins the
// AnnotateServerlessWithColdStart pass + the new awsServerlessRow
// fields end-to-end through the AWS scan handler. Covers §11
// acceptance test 12 (inventory endpoint includes cold_start_p95_ms
// field on Lambda rows).

// stubColdStartStore is a deterministic in-memory
// ColdStartObservationReader for the chunk-3 annotation tests. Pairs
// the (resource_arn, window_hours) key against the canned
// ColdStartObservationRow returned by LatestColdStartObservation.
type stubColdStartStore struct {
	mu    sync.Mutex
	rows  map[string]sqlite.ColdStartObservationRow
	err   error
	calls int
}

func (s *stubColdStartStore) key(arn string, hours int) string {
	return arn + "|" + coldStartItoa(hours)
}

func (s *stubColdStartStore) LatestColdStartObservation(
	_ context.Context,
	resourceARN string,
	windowHours int,
) (sqlite.ColdStartObservationRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return sqlite.ColdStartObservationRow{}, false, s.err
	}
	row, ok := s.rows[s.key(resourceARN, windowHours)]
	return row, ok, nil
}

func coldStartItoa(i int) string {
	return decimalString(int64(i))
}

func decimalString(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// fixedColdStartConstants pins the four substrate values the chunk-3
// annotation reads. Production threads
// NewStaticColdStartDetectionConstants from internal/discovery/aws;
// the test pins them directly.
type fixedColdStartConstants struct{}

func (fixedColdStartConstants) CurrentWindowHours() int  { return 24 }
func (fixedColdStartConstants) BaselineWindowHours() int { return 168 }
func (fixedColdStartConstants) RatioThreshold() float64  { return 1.5 }
func (fixedColdStartConstants) FloorMs() float64         { return 500.0 }

const awsAccountColdStart = "123456789012"

// awsScanResultForColdStart returns a result with one Lambda row so
// the annotation pass has something to walk.
func awsScanResultForColdStart() *scanner.Result {
	return &scanner.Result{
		ScanID:    "scan-cold-start",
		Provider:  credstore.ProviderAWS,
		AccountID: awsAccountColdStart,
		Regions:   []string{"us-east-1"},
		Serverless: []scanner.ServerlessInstanceSnapshot{
			{
				Provider:      "aws",
				Surface:       "lambda",
				AccountID:     awsAccountColdStart,
				Region:        "us-east-1",
				ResourceName:  "order-processor",
				ResourceARN:   "arn:aws:lambda:us-east-1:123456789012:function:order-processor",
				Runtime:       "python3.11",
				HasTraceAxis:  true,
				HasOTelDistro: false,
			},
		},
	}
}

func newAWSHandlerForColdStart(
	t *testing.T,
	result *scanner.Result,
	store ColdStartObservationReader,
	thresholds ColdStartAnnotationThresholds,
) *DiscoveryHandlers {
	t.Helper()
	conn := &credstore.CloudConnection{
		AccountID:      awsAccountColdStart,
		Provider:       credstore.ProviderAWS,
		ConnectionType: credstore.ConnectionAPIDiscovered,
		Regions:        []string{"us-east-1"},
		Credentials:    []byte("ciphertext"),
	}
	spy := &spyStore{getResult: conn}
	ms := &mockScanner{result: result}
	h := NewDiscoveryHandlers(spy, zap.NewNop())
	h.WithAWSScannerFactory(func(_ *credstore.CloudConnection) (DiscoveryScanner, error) {
		return ms, nil
	})
	if store != nil {
		h.WithColdStartObservationStore(store, thresholds)
	}
	return h
}

// awsColdStartRow is a minimal projection of the JSON wire shape used
// to assert per-row cold_start_p95_ms + cold_start_exceeds_threshold.
type awsColdStartRow struct {
	Serverless []struct {
		Surface                   string   `json:"surface"`
		ResourceARN               string   `json:"resource_arn"`
		ColdStartP95Ms            *float64 `json:"cold_start_p95_ms"`
		ColdStartExceedsThreshold *bool    `json:"cold_start_exceeds_threshold"`
	} `json:"serverless"`
}

// TestInventoryHandler_LambdaRow_IncludesColdStartFields — §11
// acceptance test 12. The AWS scan response's serverless[] rows
// include the new cold_start_p95_ms + cold_start_exceeds_threshold
// fields, sourced from the per-Lambda cold_start_observation lookup.
func TestInventoryHandler_LambdaRow_IncludesColdStartFields(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:order-processor"
	store := &stubColdStartStore{rows: map[string]sqlite.ColdStartObservationRow{
		(&stubColdStartStore{}).key(arn, 24): {
			ResourceARN: arn,
			WindowHours: 24,
			P95Ms:       4230,
			SampleCount: 142,
			ObservedAt:  time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC),
		},
		(&stubColdStartStore{}).key(arn, 168): {
			ResourceARN: arn,
			WindowHours: 168,
			P95Ms:       2820,
			SampleCount: 1086,
			ObservedAt:  time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC),
		},
	}}
	h := newAWSHandlerForColdStart(t, awsScanResultForColdStart(), store, fixedColdStartConstants{})

	w := doScanRequest(h, awsAccountColdStart, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp awsColdStartRow
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Serverless) != 1 {
		t.Fatalf("serverless rows = %d, want 1", len(resp.Serverless))
	}
	row := resp.Serverless[0]
	if row.ColdStartP95Ms == nil {
		t.Fatalf("cold_start_p95_ms = nil, want populated")
	}
	if *row.ColdStartP95Ms != 4230 {
		t.Errorf("cold_start_p95_ms = %v, want 4230", *row.ColdStartP95Ms)
	}
	if row.ColdStartExceedsThreshold == nil {
		t.Fatalf("cold_start_exceeds_threshold = nil, want populated")
	}
	// 4230 / 2820 = 1.5; >= threshold AND >= floor 500 → true.
	if !*row.ColdStartExceedsThreshold {
		t.Errorf("cold_start_exceeds_threshold = false, want true (4230ms / 2820ms = 1.5x, >= 500ms floor)")
	}
}

// TestInventoryHandler_LambdaRow_ColdStartFieldsNilWhenNoObservation —
// when no cold_start_observation row exists for the Lambda, both
// fields stay nil and omitempty drops them from the JSON shape. UI
// renders "—".
func TestInventoryHandler_LambdaRow_ColdStartFieldsNilWhenNoObservation(t *testing.T) {
	store := &stubColdStartStore{rows: map[string]sqlite.ColdStartObservationRow{}}
	h := newAWSHandlerForColdStart(t, awsScanResultForColdStart(), store, fixedColdStartConstants{})

	w := doScanRequest(h, awsAccountColdStart, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp awsColdStartRow
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Serverless) != 1 {
		t.Fatalf("serverless rows = %d, want 1", len(resp.Serverless))
	}
	row := resp.Serverless[0]
	if row.ColdStartP95Ms != nil {
		t.Errorf("cold_start_p95_ms = %v, want nil for no observation", *row.ColdStartP95Ms)
	}
	if row.ColdStartExceedsThreshold != nil {
		t.Errorf("cold_start_exceeds_threshold = %v, want nil for no observation", *row.ColdStartExceedsThreshold)
	}
}

// TestInventoryHandler_LambdaRow_ColdStartBelowThreshold_AmberFalse —
// a Lambda with a fresh observation that doesn't cross the 1.5x ratio
// AND 500ms floor predicates surfaces exceeds_threshold=false so the
// UI renders the cell at the default color.
func TestInventoryHandler_LambdaRow_ColdStartBelowThreshold_AmberFalse(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:order-processor"
	store := &stubColdStartStore{rows: map[string]sqlite.ColdStartObservationRow{
		(&stubColdStartStore{}).key(arn, 24): {
			ResourceARN: arn,
			WindowHours: 24,
			P95Ms:       320, // current
			SampleCount: 142,
		},
		(&stubColdStartStore{}).key(arn, 168): {
			ResourceARN: arn,
			WindowHours: 168,
			P95Ms:       200, // baseline
			SampleCount: 1086,
		},
	}}
	h := newAWSHandlerForColdStart(t, awsScanResultForColdStart(), store, fixedColdStartConstants{})

	w := doScanRequest(h, awsAccountColdStart, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp awsColdStartRow
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	row := resp.Serverless[0]
	if row.ColdStartP95Ms == nil || *row.ColdStartP95Ms != 320 {
		t.Errorf("cold_start_p95_ms = %v, want 320", row.ColdStartP95Ms)
	}
	// 320/200 = 1.6x ratio crosses, but 320ms < 500ms floor → false.
	if row.ColdStartExceedsThreshold == nil || *row.ColdStartExceedsThreshold {
		t.Errorf("cold_start_exceeds_threshold = %v, want false (320ms < 500ms floor despite 1.6x ratio)",
			row.ColdStartExceedsThreshold)
	}
}

// TestInventoryHandler_NilStore_NoColdStartFields — a deployment with
// no cold-start store wired leaves the per-row fields nil; the wire
// shape omits them entirely. Pins the safe disabled-mode posture.
func TestInventoryHandler_NilStore_NoColdStartFields(t *testing.T) {
	h := newAWSHandlerForColdStart(t, awsScanResultForColdStart(), nil, nil)
	w := doScanRequest(h, awsAccountColdStart, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if containsToken(body, "cold_start_p95_ms") {
		t.Errorf("expected no cold_start_p95_ms in body when store is nil; got: %s", body)
	}
}

// TestInventoryHandler_FlakyStore_DoesNotBreakResponse — a store
// returning an error per call must not sink the scan endpoint. Mirrors
// the trace-emission flaky-index posture.
func TestInventoryHandler_FlakyStore_DoesNotBreakResponse(t *testing.T) {
	store := &stubColdStartStore{err: errors.New("boom: cold_start_observation table unreachable")}
	h := newAWSHandlerForColdStart(t, awsScanResultForColdStart(), store, fixedColdStartConstants{})
	w := doScanRequest(h, awsAccountColdStart, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

func containsToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); i++ {
		if s[i:i+len(tok)] == tok {
			return true
		}
	}
	return false
}

// Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream
// 157) — per-provider AnnotateServerlessWithColdStart tests siblings
// to the slice 1 AWS handler tests above. The annotator now handles
// all 4 serverless surfaces; these tests call it directly (since the
// per-cloud scan handlers don't yet expose the Serverless wire
// surface in production paths) with a slice carrying GCP / Azure /
// OCI rows.

// TestAnnotateGCPServerless_PopulatesColdStartFields — slice 2 §11
// acceptance test 12 extension. A serverless slice with a Cloud Run
// row whose ARN matches a cold_start_observation entry gets the new
// fields populated, and the threshold predicate is amber when the
// 1.5x ratio + 500ms floor both hold.
func TestAnnotateGCPServerless_PopulatesColdStartFields(t *testing.T) {
	arn := "projects/my-proj/locations/us-central1/services/checkout-svc"
	store := &stubColdStartStore{rows: map[string]sqlite.ColdStartObservationRow{
		(&stubColdStartStore{}).key(arn, 24): {
			ResourceARN: arn, WindowHours: 24, P95Ms: 1800, SampleCount: 120,
			ObservedAt: time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC),
		},
		(&stubColdStartStore{}).key(arn, 168): {
			ResourceARN: arn, WindowHours: 168, P95Ms: 1000, SampleCount: 840,
			ObservedAt: time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC),
		},
	}}
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{
			Provider: "gcp", Surface: "cloudrun",
			AccountID: "my-proj", Region: "us-central1",
			ResourceName: "checkout-svc", ResourceARN: arn,
		},
		{
			Provider: "gcp", Surface: "cloudfunc",
			AccountID: "my-proj", Region: "us-central1",
			ResourceName: "no-obs", ResourceARN: "projects/my-proj/locations/us-central1/functions/no-obs",
		},
	}
	AnnotateServerlessWithColdStart(context.Background(), store, fixedColdStartConstants{}, snapshots, zap.NewNop())
	if snapshots[0].ColdStartP95Ms == nil {
		t.Fatalf("cloudrun row 0: ColdStartP95Ms = nil, want populated")
	}
	if *snapshots[0].ColdStartP95Ms != 1800 {
		t.Errorf("cloudrun row 0: ColdStartP95Ms = %v, want 1800", *snapshots[0].ColdStartP95Ms)
	}
	if snapshots[0].ColdStartExceedsThreshold == nil {
		t.Fatalf("cloudrun row 0: ColdStartExceedsThreshold = nil, want populated")
	}
	if !*snapshots[0].ColdStartExceedsThreshold {
		t.Errorf("cloudrun row 0: ColdStartExceedsThreshold = false, want true (1800/1000 = 1.8, >= 500ms floor)")
	}
	// Row 1 has no observation — fields stay nil.
	if snapshots[1].ColdStartP95Ms != nil {
		t.Errorf("cloudfunc row 1: ColdStartP95Ms = %v, want nil (no observation)", *snapshots[1].ColdStartP95Ms)
	}
	if snapshots[1].ColdStartExceedsThreshold != nil {
		t.Errorf("cloudfunc row 1: ColdStartExceedsThreshold = %v, want nil (no observation)", *snapshots[1].ColdStartExceedsThreshold)
	}
}

// TestAnnotateAzureServerless_PopulatesColdStartFields — Azure
// Functions surface. The annotator joins on resource_arn (the ARM
// resource id) and stamps the same fields as AWS / GCP.
func TestAnnotateAzureServerless_PopulatesColdStartFields(t *testing.T) {
	arn := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Web/sites/payments-fn"
	store := &stubColdStartStore{rows: map[string]sqlite.ColdStartObservationRow{
		(&stubColdStartStore{}).key(arn, 24): {
			ResourceARN: arn, WindowHours: 24, P95Ms: 3200, SampleCount: 110,
		},
		(&stubColdStartStore{}).key(arn, 168): {
			ResourceARN: arn, WindowHours: 168, P95Ms: 2000, SampleCount: 770,
		},
	}}
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{
			Provider: "azure", Surface: "azfunc",
			AccountID: "sub1", Region: "eastus",
			ResourceName: "payments-fn", ResourceARN: arn,
		},
	}
	AnnotateServerlessWithColdStart(context.Background(), store, fixedColdStartConstants{}, snapshots, zap.NewNop())
	if snapshots[0].ColdStartP95Ms == nil {
		t.Fatalf("azfunc row: ColdStartP95Ms = nil, want populated")
	}
	if *snapshots[0].ColdStartP95Ms != 3200 {
		t.Errorf("azfunc row: ColdStartP95Ms = %v, want 3200", *snapshots[0].ColdStartP95Ms)
	}
	if snapshots[0].ColdStartExceedsThreshold == nil || !*snapshots[0].ColdStartExceedsThreshold {
		t.Errorf("azfunc row: ColdStartExceedsThreshold not true (3200/2000 = 1.6, >= 500ms)")
	}
}

// TestAnnotateOCIServerless_PopulatesColdStartFields — OCI Functions
// surface. Same shape; verifies the annotator handles the
// "ocifunc" surface discriminator.
func TestAnnotateOCIServerless_PopulatesColdStartFields(t *testing.T) {
	arn := "ocid1.fnfunc.oc1.iad.aaaa"
	store := &stubColdStartStore{rows: map[string]sqlite.ColdStartObservationRow{
		(&stubColdStartStore{}).key(arn, 24): {
			ResourceARN: arn, WindowHours: 24, P95Ms: 2100, SampleCount: 80,
		},
		(&stubColdStartStore{}).key(arn, 168): {
			ResourceARN: arn, WindowHours: 168, P95Ms: 1300, SampleCount: 560,
		},
	}}
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{
			Provider: "oci", Surface: "ocifunc",
			AccountID: "ocid1.tenancy.oc1..aaaa", Region: "us-ashburn-1",
			ResourceName: "ingest-worker", ResourceARN: arn,
		},
	}
	AnnotateServerlessWithColdStart(context.Background(), store, fixedColdStartConstants{}, snapshots, zap.NewNop())
	if snapshots[0].ColdStartP95Ms == nil {
		t.Fatalf("ocifunc row: ColdStartP95Ms = nil, want populated")
	}
	if *snapshots[0].ColdStartP95Ms != 2100 {
		t.Errorf("ocifunc row: ColdStartP95Ms = %v, want 2100", *snapshots[0].ColdStartP95Ms)
	}
	if snapshots[0].ColdStartExceedsThreshold == nil || !*snapshots[0].ColdStartExceedsThreshold {
		t.Errorf("ocifunc row: ColdStartExceedsThreshold not true (2100/1300 ~= 1.62, >= 500ms)")
	}
}

// TestAnnotateServerless_UnknownSurface_LeavesNilFields — defensive
// gate: an unknown surface (future slice 3+ extension) leaves both
// fields nil. The annotator MUST NOT panic on the broader catalog.
func TestAnnotateServerless_UnknownSurface_LeavesNilFields(t *testing.T) {
	store := &stubColdStartStore{rows: map[string]sqlite.ColdStartObservationRow{}}
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{
			Provider: "aws", Surface: "future-surface",
			AccountID: "123", Region: "us-east-1",
			ResourceName: "x", ResourceARN: "future-arn",
		},
	}
	AnnotateServerlessWithColdStart(context.Background(), store, fixedColdStartConstants{}, snapshots, zap.NewNop())
	if snapshots[0].ColdStartP95Ms != nil || snapshots[0].ColdStartExceedsThreshold != nil {
		t.Errorf("unknown surface: fields should stay nil; got p95=%v, exceeds=%v",
			snapshots[0].ColdStartP95Ms, snapshots[0].ColdStartExceedsThreshold)
	}
}

// TestIsColdStartAnnotatableSurface_KnownSurfaces — pins the four
// per-cloud surface discriminators against the annotator's gate. A
// future slice-3+ surface addition without updating this constant
// would surface here.
func TestIsColdStartAnnotatableSurface_KnownSurfaces(t *testing.T) {
	for _, surface := range []string{"lambda", "cloudrun", "cloudfunc", "azfunc", "ocifunc"} {
		if !isColdStartAnnotatableSurface(surface) {
			t.Errorf("isColdStartAnnotatableSurface(%q) = false; want true", surface)
		}
	}
	for _, surface := range []string{"", "future", "ec2"} {
		if isColdStartAnnotatableSurface(surface) {
			t.Errorf("isColdStartAnnotatableSurface(%q) = true; want false", surface)
		}
	}
}
