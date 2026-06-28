// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Event source tier slice 9 chunk 1 (v0.89.156, #798 Stream 195)
// acceptance tests per docs/proposals/event-source-tier-slice9.md §11.
//
// Two flights:
//   - Single-surface tests against a fake routing /compartments +
//     /queues + /logs.
//   - Three-way dispatcher tests against a fake routing /streams +
//     /topics + /queues + /logs + /compartments — exercises the
//     combinatorial partial-scan posture across all three OCI event
//     source surfaces.

// --- Single-surface fake -------------------------------------------

type fakeOCIQueues struct {
	mu sync.Mutex

	Compartments []ociCompartment

	QueuesByCompartment map[string][]ociQueue
	LogsByQueue         map[string][]ociLogResource

	QueuesStatus       int
	LogsStatus         int
	CompartmentsStatus int
}

func newFakeOCIQueues() *fakeOCIQueues {
	return &fakeOCIQueues{
		QueuesByCompartment: map[string][]ociQueue{},
		LogsByQueue:         map[string][]ociLogResource{},
	}
}

func (f *fakeOCIQueues) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/compartments"):
			if f.CompartmentsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.CompartmentsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			comps := f.Compartments
			if comps == nil {
				comps = []ociCompartment{}
			}
			_ = json.NewEncoder(w).Encode(comps)
			return

		case strings.HasSuffix(r.URL.Path, "/queues"):
			if f.QueuesStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.QueuesStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			compartmentID := r.URL.Query().Get("compartmentId")
			queues := f.QueuesByCompartment[compartmentID]
			if queues == nil {
				queues = []ociQueue{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(queues)
			return

		case strings.HasSuffix(r.URL.Path, "/logGroups"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fakeLogGroupList())
			return

		case strings.HasSuffix(r.URL.Path, "/logs"):
			if f.LogsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.LogsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(flattenFakeLogs(f.LogsByQueue))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "NotFound", Message: "unhandled mock path: " + r.URL.Path})
	})
}

func newQueuesScannerWithFake(t *testing.T, fake *fakeOCIQueues, region string) *Scanner {
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
		Fingerprint: "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99",
		PrivateKey:  pemBytes,
		Region:      r,
		ociEndpoint: srv.URL,
		httpClient:  srv.Client(),
	}
}

func makeOCIQueue(name string) ociQueue {
	return ociQueue{
		ID:                  "ocid1.queue.oc1..." + name,
		DisplayName:         name,
		CompartmentID:       "ocid1.tenancy.oc1..aaa",
		LifecycleState:      "ACTIVE",
		VisibilityInSeconds: 30,
		RetentionInSeconds:  86400,
	}
}

// --- Test §11.1: ScanQueues returns queues --------------------------

func TestQueuesScanner_ListReturnsQueues(t *testing.T) {
	fake := newFakeOCIQueues()
	fake.QueuesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociQueue{
		makeOCIQueue("alpha"),
		makeOCIQueue("bravo"),
	}

	s := newQueuesScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanQueues(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 2)

	assert.Equal(t, "oci", snaps[0].Provider)
	assert.Equal(t, "queues", snaps[0].Surface)
	assert.Equal(t, "queue", snaps[0].SourceType)
	assert.Equal(t, "alpha", snaps[0].ResourceName)
	assert.Equal(t, "ocid1.queue.oc1..."+"alpha", snaps[0].ResourceARN)
}

// --- Test §11.2: Queue with Logging configured → HasLogAxis = true --

func TestQueuesScanner_HasLogAxisTrue_WhenLoggingMatches(t *testing.T) {
	fake := newFakeOCIQueues()
	queue := makeOCIQueue("with-logs")
	fake.QueuesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociQueue{queue}
	fake.LogsByQueue[queue.ID] = []ociLogResource{
		{
			ID:          "ocid1.log.oc1..z",
			DisplayName: "queue-delivery-log",
			Configuration: ociLogConfiguration{
				Source: ociLogSource{
					Resource: queue.ID,
					Category: "all",
				},
			},
		},
	}

	s := newQueuesScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanQueues(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.True(t, snaps[0].HasLogAxis,
		"OCI Logging /logs response carrying Source.Resource == queue.OCID must flip HasLogAxis true")
	assert.True(t, snaps[0].HasTraceAxis,
		"slice 9 chunk 1: Logging is the closest observability proxy; HasTraceAxis follows HasLogAxis")
	assert.Equal(t, true, snaps[0].Detail["has_log_group"])
}

// --- Test §11.3: Queue without Logging → HasLogAxis = false ---------

func TestQueuesScanner_HasLogAxisFalse_WhenLoggingMisses(t *testing.T) {
	fake := newFakeOCIQueues()
	queue := makeOCIQueue("no-logs")
	fake.QueuesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociQueue{queue}

	s := newQueuesScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanQueues(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.False(t, snaps[0].HasLogAxis)
	assert.False(t, snaps[0].HasTraceAxis)
}

// --- Test §11.5+6: Detail records informational signals -------------

func TestQueuesScanner_DetailRecordsInformationalSignals(t *testing.T) {
	fake := newFakeOCIQueues()
	queue := makeOCIQueue("rich-detail")
	queue.LifecycleState = "ACTIVE"
	queue.VisibilityInSeconds = 60
	queue.RetentionInSeconds = 604800
	queue.DeadLetterQueueDeliveryCount = 5
	queue.CustomEncryptionKeyID = "ocid1.key.oc1..xyz"
	fake.QueuesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociQueue{queue}

	s := newQueuesScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanQueues(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, "ACTIVE", snaps[0].Detail["lifecycle_state"])
	assert.Equal(t, 60, snaps[0].Detail["visibility_in_seconds"])
	assert.Equal(t, 604800, snaps[0].Detail["retention_in_seconds"])
	assert.Equal(t, 5, snaps[0].Detail["dead_letter_queue_delivery_count"])
	assert.Equal(t, true, snaps[0].Detail["kms_key_id_set"],
		"slice 9 records KMS key as boolean only — the raw OCID does NOT surface")
}

// --- Three-way dispatcher fake -------------------------------------

type fakeOCITriEventSources struct {
	mu sync.Mutex

	Compartments []ociCompartment

	StreamsByCompartment map[string][]ociStream
	TopicsByCompartment  map[string][]ociNotificationTopic
	QueuesByCompartment  map[string][]ociQueue
	LogsByResource       map[string][]ociLogResource

	StreamsStatus      int
	TopicsStatus       int
	QueuesStatus       int
	LogsStatus         int
	CompartmentsStatus int
}

func newFakeOCITriEventSources() *fakeOCITriEventSources {
	return &fakeOCITriEventSources{
		StreamsByCompartment: map[string][]ociStream{},
		TopicsByCompartment:  map[string][]ociNotificationTopic{},
		QueuesByCompartment:  map[string][]ociQueue{},
		LogsByResource:       map[string][]ociLogResource{},
	}
}

func (f *fakeOCITriEventSources) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/compartments"):
			if f.CompartmentsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.CompartmentsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.Compartments)
			return

		case strings.HasSuffix(r.URL.Path, "/streams"):
			if f.StreamsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.StreamsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			compartmentID := r.URL.Query().Get("compartmentId")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.StreamsByCompartment[compartmentID])
			return

		case strings.HasSuffix(r.URL.Path, "/topics"):
			if f.TopicsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.TopicsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			compartmentID := r.URL.Query().Get("compartmentId")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.TopicsByCompartment[compartmentID])
			return

		case strings.HasSuffix(r.URL.Path, "/queues"):
			if f.QueuesStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.QueuesStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			compartmentID := r.URL.Query().Get("compartmentId")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.QueuesByCompartment[compartmentID])
			return

		case strings.HasSuffix(r.URL.Path, "/logGroups"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fakeLogGroupList())
			return

		case strings.HasSuffix(r.URL.Path, "/logs"):
			if f.LogsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.LogsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(flattenFakeLogs(f.LogsByResource))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "NotFound", Message: "unhandled mock path: " + r.URL.Path})
	})
}

func newTriScannerWithFake(t *testing.T, fake *fakeOCITriEventSources, region string) *Scanner {
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
		Fingerprint: "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99",
		PrivateKey:  pemBytes,
		Region:      r,
		ociEndpoint: srv.URL,
		httpClient:  srv.Client(),
	}
}

// --- Test §11.7: Three-way dispatcher combines all three surfaces ---

func TestScanEventSources_ThreeWay_OCI_CombinesAllSurfaces(t *testing.T) {
	fake := newFakeOCITriEventSources()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{makeOCIStream("intake")}
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{makeOCITopic("alarm-fanout")}
	fake.QueuesByCompartment["ocid1.tenancy.oc1..aaa"] = []ociQueue{makeOCIQueue("task-queue")}

	s := newTriScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanEventSources(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 3,
		"three-way dispatcher must surface ALL THREE OCI event source surfaces from a single ScanEventSources call")

	var surfaces []string
	for _, snap := range snaps {
		surfaces = append(surfaces, snap.Surface)
	}
	assert.Contains(t, surfaces, "streaming")
	assert.Contains(t, surfaces, "notifications")
	assert.Contains(t, surfaces, "queues")
}

// --- Test §11.10: Queues fails → Streaming + ONS still surface ------

func TestScanEventSources_ThreeWay_OCI_QueuesFails_OthersStillSurface(t *testing.T) {
	fake := newFakeOCITriEventSources()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{makeOCIStream("intake")}
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{makeOCITopic("alarm-fanout")}
	fake.QueuesStatus = http.StatusForbidden

	s := newTriScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanEventSources(context.Background(), scopeRoot())
	require.NoError(t, err, "three-way partial-scan: Queues alone failed, Streaming + ONS must still surface")
	// Queues call failure is per-compartment-walk swallowed by
	// ScanQueues (returns nil err with empty snapshots). The other
	// surfaces succeed; dispatcher returns combined union with no
	// error. This is the honest contract — single-surface failures
	// at the per-list level do NOT trip the dispatcher's
	// three-way-fail branch.
	assert.GreaterOrEqual(t, len(snaps), 2)
}

// --- Test §11.12: All three fail → error mentions all three ---------

func TestScanEventSources_ThreeWay_OCI_AllFail_ErrorMentionsAllSurfaces(t *testing.T) {
	fake := newFakeOCITriEventSources()
	fake.CompartmentsStatus = http.StatusInternalServerError

	s := newTriScannerWithFake(t, fake, "us-phoenix-1")
	_, err := s.ScanEventSources(context.Background(), scopeRoot())
	require.Error(t, err)
	errStr := err.Error()
	assert.Contains(t, errStr, "streaming")
	assert.Contains(t, errStr, "notifications")
	assert.Contains(t, errStr, "queues")
}

// --- Test §11.15: Cold-start parity — no queues, no extra Logging ---

func TestScanEventSources_ThreeWay_OCI_ColdStartParity_NoQueues(t *testing.T) {
	fake := newFakeOCITriEventSources()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{makeOCIStream("intake")}
	// TopicsByCompartment + QueuesByCompartment intentionally empty.

	s := newTriScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanEventSources(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1,
		"empty /topics + /queues responses produce zero ONS + Queue rows; dispatcher output equals streaming-only output")
	assert.Equal(t, "streaming", snaps[0].Surface)
}
