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

// Event source tier slice 7 chunk 1 — OCI Notification Service scanner
// acceptance tests.
//
// Test coverage per docs/proposals/event-source-tier-slice7.md §11.
// The two-way dispatcher tests reuse the fakeOCIDualEventSources mock
// which routes /compartments + /streams + /topics + /logs on the same
// httptest server (matching production where every OCI service shares
// the per-region signing flow but uses distinct per-service hostnames
// — the test mock collapses the hostnames into a single httptest port
// per the existing fakeOCIStreaming convention).
//
// The chunk 4 (slice 1) scanner_streaming_test.go fake is left
// untouched per the slice 7 design doc's cold-start parity test
// (§11 test 13): the existing single-surface tests must keep passing
// byte-identically against the same fixtures.

// --- Helpers: OCI ONS httptest mock ---------------------------------

// fakeOCIONS is a slim httptest-backed mock targeted at slice 7
// chunk 1 single-surface tests. It mirrors fakeOCIStreaming but
// routes /topics instead of /streams (and a parallel /logs lookup
// by topic OCID rather than stream OCID — the Logging response shape
// is the per-OCID-resource match used by both surfaces).
type fakeOCIONS struct {
	mu sync.Mutex

	Compartments []ociCompartment

	// TopicsByCompartment maps compartmentId -> Topics served when
	// /topics is called with that compartmentId. Missing
	// compartmentId returns an empty list (not a 404).
	TopicsByCompartment map[string][]ociNotificationTopic

	// LogsByTopic maps topic.TopicID -> Logs returned when /logs is
	// called with searchTerm=<topic.TopicID>. Mirrors
	// fakeOCIStreaming.LogsByStream — same defensive
	// Configuration.Source.Resource side-check semantics.
	LogsByTopic map[string][]ociLogResource

	// Call counters.
	CompartmentsCalls int
	TopicsCalls       int
	LogsCalls         int

	// Per-endpoint failure switches.
	TopicsStatus       int
	LogsStatus         int
	CompartmentsStatus int
}

func newFakeOCIONS() *fakeOCIONS {
	return &fakeOCIONS{
		TopicsByCompartment: map[string][]ociNotificationTopic{},
		LogsByTopic:         map[string][]ociLogResource{},
	}
}

func (f *fakeOCIONS) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/compartments"):
			f.CompartmentsCalls++
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

		case strings.HasSuffix(r.URL.Path, "/topics"):
			f.TopicsCalls++
			if f.TopicsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.TopicsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			compartmentID := r.URL.Query().Get("compartmentId")
			topics := f.TopicsByCompartment[compartmentID]
			if topics == nil {
				topics = []ociNotificationTopic{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(topics)
			return

		case strings.HasSuffix(r.URL.Path, "/logs"):
			f.LogsCalls++
			if f.LogsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.LogsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			searchTerm := r.URL.Query().Get("searchTerm")
			logs := f.LogsByTopic[searchTerm]
			if logs == nil {
				logs = []ociLogResource{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(logs)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ociErrorBody{
			Code:    "NotFound",
			Message: "unhandled mock path: " + r.URL.Path,
		})
	})
}

// newONSScannerWithFake wires a Scanner against the supplied fake's
// httptest server. Mirrors newStreamingScannerWithFake.
func newONSScannerWithFake(t *testing.T, fake *fakeOCIONS, region string) *Scanner {
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

// makeOCITopic builds a minimal ociNotificationTopic with a
// deterministic OCID derived from the supplied name. Used to keep
// test fixtures readable while still exercising the production
// per-OCID Logging detection rule.
func makeOCITopic(name string) ociNotificationTopic {
	return ociNotificationTopic{
		TopicID:        "ocid1.onstopic.oc1..." + name,
		Name:           name,
		CompartmentID:  "ocid1.tenancy.oc1..aaa",
		LifecycleState: "ACTIVE",
	}
}

// --- Test §11.1: ScanNotificationTopics returns topics -------------

func TestONSScanner_ListReturnsTopics(t *testing.T) {
	fake := newFakeOCIONS()
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{
		makeOCITopic("alpha"),
		makeOCITopic("bravo"),
	}

	s := newONSScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanNotificationTopics(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 2)

	assert.Equal(t, "oci", snaps[0].Provider)
	assert.Equal(t, "notifications", snaps[0].Surface,
		"ONS snapshots must carry Surface=notifications per the per-cloud convention")
	assert.Equal(t, "ons_topic", snaps[0].SourceType)
	assert.Equal(t, "alpha", snaps[0].ResourceName)
	assert.Equal(t, "ocid1.onstopic.oc1..."+"alpha", snaps[0].ResourceARN)
}

// --- Test §11.2: Topic with Logging configured → HasLogAxis = true --

func TestONSScanner_HasLogAxisTrue_WhenLoggingMatches(t *testing.T) {
	fake := newFakeOCIONS()
	topic := makeOCITopic("with-logs")
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{topic}
	fake.LogsByTopic[topic.TopicID] = []ociLogResource{
		{
			ID:          "ocid1.log.oc1..z",
			DisplayName: "ons-delivery-log",
			Configuration: ociLogConfiguration{
				Source: ociLogSource{
					Resource: topic.TopicID,
					Category: "all",
				},
			},
		},
	}

	s := newONSScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanNotificationTopics(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.True(t, snaps[0].HasLogAxis,
		"OCI Logging /logs response carrying Source.Resource == topic.OCID must flip HasLogAxis true")
	assert.True(t, snaps[0].HasTraceAxis,
		"slice 7 chunk 1 design: Logging is the closest observability proxy; HasTraceAxis follows HasLogAxis until slice 8+ separates them")
	assert.Equal(t, true, snaps[0].Detail["has_log_group"])
}

// --- Test §11.3: Topic without Logging → HasLogAxis = false ---------

func TestONSScanner_HasLogAxisFalse_WhenLoggingMisses(t *testing.T) {
	fake := newFakeOCIONS()
	topic := makeOCITopic("no-logs")
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{topic}
	// LogsByTopic intentionally empty for topic.TopicID — the /logs
	// response carries an empty array.

	s := newONSScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanNotificationTopics(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasLogAxis,
		"no Logging entry for the topic OCID must leave HasLogAxis false")
	assert.False(t, snaps[0].HasTraceAxis)
	assert.Equal(t, false, snaps[0].Detail["has_log_group"])
}

// --- Test §11.4+5: Detail records informational axes ---------------

func TestONSScanner_DetailRecordsInformationalAxes(t *testing.T) {
	fake := newFakeOCIONS()
	topic := makeOCITopic("with-kms")
	topic.LifecycleState = "CREATING"
	topic.ShortTopicID = "short-handle-1"
	topic.KmsKeyID = "ocid1.key.oc1..xyz"
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{topic}

	s := newONSScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanNotificationTopics(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	// Lifecycle state is recorded informationally; CREATING does NOT
	// suppress the snapshot per slice 7 design doc §3 ("Topic
	// lifecycle state ... informational only").
	assert.Equal(t, "CREATING", snaps[0].Detail["lifecycle_state"])

	// short_topic_id and kms_key_id are recorded as booleans without
	// leaking the raw OCID values — per slice 7 design doc §3
	// (informational only; the scanner records that they exist).
	assert.Equal(t, true, snaps[0].Detail["short_topic_id_set"])
	assert.Equal(t, true, snaps[0].Detail["kms_key_id_set"])
}

// --- Test §11.6 (variant): subscription count is NOT recorded -------
//
// Slice 7 chunk 1 intentionally does NOT record subscription count on
// the topic snapshot (design doc §3 + per-topic snapshot godoc on
// ociNotificationTopic). The per-topic /subscriptions?topicId= walk
// is deferred to slice 8+. This test pins that constraint so a future
// refactor that accidentally adds the field surfaces a deliberate
// design conversation rather than a silent behavior change.

func TestONSScanner_SubscriptionCountNotRecordedOnSnapshot(t *testing.T) {
	fake := newFakeOCIONS()
	topic := makeOCITopic("counts-not-tracked")
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{topic}

	s := newONSScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanNotificationTopics(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	_, ok := snaps[0].Detail["subscription_count"]
	assert.False(t, ok,
		"slice 7 chunk 1 must not record subscription_count — slice 8+ candidate per design doc §13")
}

// --- Helpers: combined Streaming + ONS mock -------------------------
//
// fakeOCIDualEventSources routes /compartments + /streams + /topics +
// /logs on a single httptest server. Used by the two-way dispatcher
// tests so the slice 7 ScanEventSources fan-out can be exercised
// against both surfaces without two separate httptest ports.

type fakeOCIDualEventSources struct {
	mu sync.Mutex

	Compartments []ociCompartment

	StreamsByCompartment map[string][]ociStream
	TopicsByCompartment  map[string][]ociNotificationTopic

	// LogsByResource maps an OCID (stream or topic) -> Logs returned
	// when /logs is called with searchTerm=<ocid>. Shared map keeps
	// the per-surface Logging detection setup uniform across the
	// dispatcher tests.
	LogsByResource map[string][]ociLogResource

	// Per-endpoint failure switches.
	StreamsStatus      int
	TopicsStatus       int
	LogsStatus         int
	CompartmentsStatus int

	// Call counters.
	CompartmentsCalls int
	StreamsCalls      int
	TopicsCalls       int
	LogsCalls         int
}

func newFakeOCIDualEventSources() *fakeOCIDualEventSources {
	return &fakeOCIDualEventSources{
		StreamsByCompartment: map[string][]ociStream{},
		TopicsByCompartment:  map[string][]ociNotificationTopic{},
		LogsByResource:       map[string][]ociLogResource{},
	}
}

func (f *fakeOCIDualEventSources) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/compartments"):
			f.CompartmentsCalls++
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

		case strings.HasSuffix(r.URL.Path, "/streams"):
			f.StreamsCalls++
			if f.StreamsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.StreamsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			compartmentID := r.URL.Query().Get("compartmentId")
			streams := f.StreamsByCompartment[compartmentID]
			if streams == nil {
				streams = []ociStream{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(streams)
			return

		case strings.HasSuffix(r.URL.Path, "/topics"):
			f.TopicsCalls++
			if f.TopicsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.TopicsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			compartmentID := r.URL.Query().Get("compartmentId")
			topics := f.TopicsByCompartment[compartmentID]
			if topics == nil {
				topics = []ociNotificationTopic{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(topics)
			return

		case strings.HasSuffix(r.URL.Path, "/logs"):
			f.LogsCalls++
			if f.LogsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.LogsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			searchTerm := r.URL.Query().Get("searchTerm")
			logs := f.LogsByResource[searchTerm]
			if logs == nil {
				logs = []ociLogResource{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(logs)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ociErrorBody{
			Code:    "NotFound",
			Message: "unhandled mock path: " + r.URL.Path,
		})
	})
}

func newDualScannerWithFake(t *testing.T, fake *fakeOCIDualEventSources, region string) *Scanner {
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

// --- Test §11.7: Two-way dispatcher combines streams + topics -------

func TestScanEventSources_CombinesStreamsAndTopics(t *testing.T) {
	fake := newFakeOCIDualEventSources()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("kafka-intake"),
	}
	fake.TopicsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociNotificationTopic{
		makeOCITopic("alarm-fanout"),
	}

	s := newDualScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanEventSources(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 2,
		"two-way dispatcher must surface BOTH streaming streams AND ONS topics from a single ScanEventSources call")

	var surfaces []string
	for _, snap := range snaps {
		surfaces = append(surfaces, snap.Surface)
	}
	assert.Contains(t, surfaces, "streaming",
		"streaming surface must appear in the dispatcher output")
	assert.Contains(t, surfaces, "notifications",
		"notifications surface must appear in the dispatcher output")
}

// --- Test §11.10: Both surfaces fail → dispatcher returns error -----
//
// Both ScanStreams and ScanNotificationTopics share substrate
// validation via compartmentsForEventSource. When the /compartments
// endpoint returns 500, BOTH surface walks fail with the same root
// cause. The slice 7 dispatcher's partial-scan posture surfaces the
// terminal error mentioning both surfaces — pinned here so a future
// dispatcher refactor that silently swallows one side's error surfaces
// the regression.

func TestScanEventSources_BothFail_MentionsBothSurfaces(t *testing.T) {
	fake := newFakeOCIDualEventSources()
	fake.CompartmentsStatus = http.StatusInternalServerError

	s := newDualScannerWithFake(t, fake, "us-phoenix-1")
	_, err := s.ScanEventSources(context.Background(), scopeRoot())
	require.Error(t, err)
	errStr := err.Error()
	assert.Contains(t, errStr, "streaming",
		"dispatcher's both-failed error message must mention the streaming surface")
	assert.Contains(t, errStr, "notifications",
		"dispatcher's both-failed error message must mention the notifications surface")
}

// --- Test §11.13: Cold-start parity — Streaming-only fixtures still
// produce identical snapshots through the two-way dispatcher
//
// The slice 7 design doc's cold-start parity test (§11 test 13) pins
// that an environment with no ONS topics produces a snapshot stream
// byte-identical to what the slice 1 chunk 4 ScanEventSources entry
// point produced. With the new two-way dispatcher, this means: when
// the /topics endpoint returns empty for every compartment, the
// dispatcher's output must be exactly ScanStreams's output (no extra
// per-topic Logging calls leaking into the cold path — only what the
// empty /topics responses themselves cost).

func TestScanEventSources_ColdStartParity_NoTopics(t *testing.T) {
	fake := newFakeOCIDualEventSources()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("intake"),
	}
	// TopicsByCompartment intentionally empty — every /topics call
	// returns []. The cold path: zero ONS rows, no extra LogsByTopic
	// lookups.

	s := newDualScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanEventSources(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1,
		"empty /topics responses must produce zero ONS rows; dispatcher output must equal streaming-only output")
	assert.Equal(t, "streaming", snaps[0].Surface,
		"the single snapshot must be the streaming row — ONS must not contribute when /topics is empty")

	// Verify the Logging call count stayed at exactly 1 (the per-
	// stream Logging detection from ScanStreams), confirming no
	// extra per-topic Logging calls fired against empty topic lists.
	assert.Equal(t, 1, fake.LogsCalls,
		"per-topic Logging detection must NOT fire when there are no topics — keeps the cold path identical to slice 1 cost")
}
