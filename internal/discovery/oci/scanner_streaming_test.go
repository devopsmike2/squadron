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

// --- Helpers: OCI Streaming + Logging httptest mock ------------------
//
// fakeOCIStreaming is a slim httptest-backed mock targeted at the
// chunk 4 OCI Streaming scanner tests. It mirrors the existing
// fakeOCIFunctions scaffolding (see scanner_functions_test.go) but
// only routes the endpoints chunk 4 needs — /compartments + /streams
// + /logs — so the chunk 4 tests don't depend on the compute /
// database / OKE / functions fields the older fakes expose. Keeping
// it separate also keeps the chunk 1 compute / chunk 2 database /
// OKE / chunk 4 functions tests in their respective files untouched
// per the chunk 4 brief's "DO NOT modify existing OCI scanner tests"
// constraint.

type fakeOCIStreaming struct {
	mu sync.Mutex

	// Compartments returned by the identity list call. Empty means
	// the call returns an empty array (the scanner falls back to
	// scanning only the tenancy root).
	Compartments []ociCompartment

	// StreamsByCompartment maps compartmentId -> Streams served when
	// /streams is called with that compartmentId. Missing
	// compartmentId returns an empty list (not a 404). When
	// StreamsByPage is non-empty for a compartment, the mock prefers
	// the pagination shape.
	StreamsByCompartment map[string][]ociStream

	// LogsByStream maps stream.ID -> Logs returned when /logs is
	// called with searchTerm=<stream.id>. The mock side-effects the
	// search semantics: requests with searchTerm matching a key in
	// LogsByStream return that key's logs; other searchTerms return
	// an empty list (not a 404).
	//
	// Each entry's Configuration.Source.Resource MUST be set to the
	// stream's OCID for the detection rule to fire — same as
	// production OCI Logging responses.
	LogsByStream map[string][]ociLogResource

	// PaginationCompartment, when set, opts the named compartment
	// into a multi-page /streams response. The mock then returns
	// StreamsByPage[""] on the first call, StreamsByPage["next"] on
	// the page=next call, etc. The opc-next-page header carries the
	// token for the subsequent page; the final page emits no header.
	PaginationCompartment string

	// StreamsByPage holds the per-page streams list for the
	// PaginationCompartment opt-in flow. The key is the page token
	// (empty string for the first page); the value is the page's
	// streams. NextPageByPage maps each page token to the next-page
	// token to set on the response header.
	StreamsByPage        map[string][]ociStream
	NextPageByPageStream map[string]string

	// Call counters.
	CompartmentsCalls int
	StreamsCalls      int
	LogsCalls         int

	// Per-endpoint failure switches. Status==0 means "no failure
	// configured"; any non-zero status returns that code with a JSON
	// error body.
	StreamsStatus      int
	LogsStatus         int
	CompartmentsStatus int

	// LogsFailureForStream lets a specific stream OCID trigger a
	// per-stream Logging call failure while other streams' calls
	// succeed. Mirrors production where Logging may be rate-limited
	// on a single stream while the rest of the compartment walks
	// cleanly. Empty -> no per-stream failure injection.
	LogsFailureForStream string
}

func newFakeOCIStreaming() *fakeOCIStreaming {
	return &fakeOCIStreaming{
		StreamsByCompartment: map[string][]ociStream{},
		LogsByStream:         map[string][]ociLogResource{},
		StreamsByPage:        map[string][]ociStream{},
		NextPageByPageStream: map[string]string{},
	}
}

func (f *fakeOCIStreaming) handler() http.Handler {
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
			compartmentID := r.URL.Query().Get("compartmentId")
			pageToken := r.URL.Query().Get("page")

			if f.StreamsStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.StreamsStatus)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}

			// Pagination opt-in path.
			if compartmentID == f.PaginationCompartment && f.PaginationCompartment != "" {
				streams := f.StreamsByPage[pageToken]
				if streams == nil {
					streams = []ociStream{}
				}
				if next := f.NextPageByPageStream[pageToken]; next != "" {
					w.Header().Set("opc-next-page", next)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(streams)
				return
			}

			streams := f.StreamsByCompartment[compartmentID]
			if streams == nil {
				streams = []ociStream{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(streams)
			return

		case strings.HasSuffix(r.URL.Path, "/logGroups"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fakeLogGroupList())
			return

		case strings.HasSuffix(r.URL.Path, "/logs"):
			f.LogsCalls++
			// The per-group /logs call carries no resource id, so a
			// Logging failure is now per-compartment (not per-stream):
			// either LogsStatus or LogsFailureForStream being set fails
			// the whole logs lookup.
			if f.LogsStatus != 0 || f.LogsFailureForStream != "" {
				status := f.LogsStatus
				if status == 0 {
					status = http.StatusServiceUnavailable
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(ociErrorBody{Code: "MockError", Message: "mock"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(flattenFakeLogs(f.LogsByStream))
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

// newStreamingScannerWithFake wires a Scanner against the supplied
// fake's httptest server. Test takes ownership of cleanup via
// t.Cleanup. Mirrors newFunctionsScannerWithFake (scanner_functions_test.go)
// but targets the chunk 4 streaming mock specifically.
func newStreamingScannerWithFake(t *testing.T, fake *fakeOCIStreaming, region string) *Scanner {
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

// --- Fixture builders ------------------------------------------------

func makeOCIStream(name string) ociStream {
	return ociStream{
		ID:             "ocid1.stream.oc1..." + name,
		Name:           name,
		CompartmentID:  "ocid1.tenancy.oc1..aaa",
		LifecycleState: "ACTIVE",
	}
}

func makeOCILogResource(streamID string) ociLogResource {
	return ociLogResource{
		ID:          "ocid1.log.oc1..." + streamID,
		DisplayName: "log-for-" + streamID,
		Configuration: ociLogConfiguration{
			Source: ociLogSource{
				Resource: streamID,
				Category: OCIStreamingLogCategoryAllEvents,
			},
		},
	}
}

// --- Test 10 (slice 1 §11): stream with Logging configured flips
// HasLogAxis. --------------------------------------------------------

func TestStreamingScanner_StreamWithLoggingConfigured_HasLogAxis(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("orders-stream"),
	}
	fake.LogsByStream["ocid1.stream.oc1...orders-stream"] = []ociLogResource{
		makeOCILogResource("ocid1.stream.oc1...orders-stream"),
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasLogAxis, "matching Logging log resource should flip HasLogAxis")
	assert.Equal(t, "oci", snap.Provider)
	assert.Equal(t, "streaming", snap.Surface)
	assert.Equal(t, "stream", snap.SourceType)
	assert.Equal(t, "orders-stream", snap.ResourceName)
	assert.Equal(t, "ocid1.stream.oc1...orders-stream", snap.ResourceARN)
	assert.Equal(t, "us-phoenix-1", snap.Region)
	assert.Equal(t, "ocid1.tenancy.oc1..aaa", snap.AccountID)
}

// --- Test 11 (slice 1 §11): stream without Logging leaves
// HasLogAxis false. --------------------------------------------------

func TestStreamingScanner_StreamWithoutLogging_NoLogAxis(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("untracked-stream"),
	}
	// LogsByStream intentionally empty -> the searchTerm-scoped
	// /logs call returns an empty list.

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.False(t, snap.HasLogAxis, "no Logging entry should leave HasLogAxis false")
	assert.False(t, snap.HasTraceAxis,
		"no Logging entry should leave HasTraceAxis false (chunk 4 Logging proxy)")
	assert.False(t, snap.IsInstrumented(),
		"a stream with neither axis is uninstrumented")
}

// --- Test: HasLogAxis acts as HasTraceAxis proxy (chunk 4 §3.4) -----

func TestStreamingScanner_LogAxisActsAsTraceAxisProxy(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("payments-stream"),
	}
	fake.LogsByStream["ocid1.stream.oc1...payments-stream"] = []ociLogResource{
		makeOCILogResource("ocid1.stream.oc1...payments-stream"),
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasLogAxis,
		"matching Logging log resource should flip HasLogAxis")
	assert.True(t, snap.HasTraceAxis,
		"slice 1 chunk 4 proxy: HasLogAxis=true forces HasTraceAxis=true")
	assert.True(t, snap.IsInstrumented(),
		"either-axis OR rule: a stream with HasLogAxis counts as instrumented")
}

// --- Test: pagination on /streams via opc-next-page ------------------

func TestStreamingScanner_PaginationFollowsOpcNextPage(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.PaginationCompartment = "ocid1.tenancy.oc1..aaa"

	// Three pages of streams: empty-token -> page1 then "p2",
	// "p2" -> page2 then "p3", "p3" -> page3 with no next token.
	s1 := ociStream{ID: "ocid1.stream.oc1...s1", Name: "s1", LifecycleState: "ACTIVE"}
	s2 := ociStream{ID: "ocid1.stream.oc1...s2", Name: "s2", LifecycleState: "ACTIVE"}
	s3 := ociStream{ID: "ocid1.stream.oc1...s3", Name: "s3", LifecycleState: "ACTIVE"}

	fake.StreamsByPage[""] = []ociStream{s1}
	fake.StreamsByPage["p2"] = []ociStream{s2}
	fake.StreamsByPage["p3"] = []ociStream{s3}
	fake.NextPageByPageStream[""] = "p2"
	fake.NextPageByPageStream["p2"] = "p3"
	// No NextPageByPageStream["p3"] entry -> final page emits no
	// next token -> loop terminates.

	sc := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := sc.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 3, "three pages of one stream each -> three snapshots")

	// Three /streams calls (one per page).
	assert.Equal(t, 3, fake.StreamsCalls,
		"opc-next-page pagination should drive three /streams calls")
	// Three /logs calls (one per stream).
	assert.Equal(t, 3, fake.LogsCalls,
		"one /logs detection call per stream regardless of pagination")

	got := map[string]bool{}
	for _, snap := range snaps {
		got[snap.ResourceName] = true
	}
	assert.True(t, got["s1"])
	assert.True(t, got["s2"])
	assert.True(t, got["s3"])
}

// --- Test: empty compartment surface returns empty slice -------------

func TestStreamingScanner_EmptyCompartmentReturnsEmptySlice(t *testing.T) {
	fake := newFakeOCIStreaming()
	// No streams for any compartment.

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	assert.Empty(t, snaps, "compartments with no streams return no snapshots")

	// 1 compartments call + 1 streams call against the tenancy root.
	assert.Equal(t, 1, fake.CompartmentsCalls)
	assert.Equal(t, 1, fake.StreamsCalls)
	// Zero /logs calls — there were no streams to detect against.
	assert.Equal(t, 0, fake.LogsCalls)
}

// --- Test: multi-compartment walk visits every compartment ----------

func TestStreamingScanner_MultiCompartmentWalksAll(t *testing.T) {
	fake := newFakeOCIStreaming()
	// Two child compartments + tenancy root.
	fake.Compartments = []ociCompartment{
		{ID: "ocid1.compartment.oc1..teamA", Name: "team-a", LifecycleState: "ACTIVE"},
		{ID: "ocid1.compartment.oc1..teamB", Name: "team-b", LifecycleState: "ACTIVE"},
	}
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("root-stream"),
	}
	fake.StreamsByCompartment["ocid1.compartment.oc1..teamA"] = []ociStream{
		makeOCIStream("team-a-stream"),
	}
	fake.StreamsByCompartment["ocid1.compartment.oc1..teamB"] = []ociStream{
		makeOCIStream("team-b-stream-1"),
		makeOCIStream("team-b-stream-2"),
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 4, "1 root + 1 team-a + 2 team-b = 4 streams")

	// 1 compartments call (Identity list) + 3 streams calls (root +
	// team-a + team-b).
	assert.Equal(t, 1, fake.CompartmentsCalls)
	assert.Equal(t, 3, fake.StreamsCalls)
	assert.Equal(t, 4, fake.LogsCalls, "one /logs call per stream")

	names := map[string]bool{}
	for _, snap := range snaps {
		names[snap.ResourceName] = true
	}
	assert.True(t, names["root-stream"])
	assert.True(t, names["team-a-stream"])
	assert.True(t, names["team-b-stream-1"])
	assert.True(t, names["team-b-stream-2"])
}

// --- Test: ResourceName + ResourceARN populated correctly -----------

func TestStreamingScanner_ResourceNameAndARNPopulated(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		{
			ID:             "ocid1.stream.oc1.iad.aaaaaaaa1234567890abcdef",
			Name:           "production-events",
			CompartmentID:  "ocid1.tenancy.oc1..aaa",
			LifecycleState: "ACTIVE",
		},
	}

	s := newStreamingScannerWithFake(t, fake, "us-ashburn-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.Equal(t, "production-events", snap.ResourceName,
		"ResourceName should carry the stream's Name")
	assert.Equal(t, "ocid1.stream.oc1.iad.aaaaaaaa1234567890abcdef", snap.ResourceARN,
		"ResourceARN should carry the stream's full OCID")
	assert.Equal(t, "us-ashburn-1", snap.Region,
		"Region should carry the scanner's configured Region")
}

// --- Test: per-stream Logging call failure does not abort the walk --

func TestStreamingScanner_LoggingAPICallFailureNonFatal(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("stream-a"),
		makeOCIStream("stream-b"),
		makeOCIStream("stream-c"),
	}
	// The OCI Logging list calls (logGroups + logs-per-group) carry no
	// resource id, so a Logging failure is per-compartment, not
	// per-stream: when the logs lookup fails, every stream in the
	// compartment dims to axis=false, and the failure must remain
	// non-fatal (every stream still surfaces).
	fake.LogsFailureForStream = "trigger" // any non-empty value fails the logs call
	fake.LogsByStream["ocid1.stream.oc1...stream-c"] = []ociLogResource{
		makeOCILogResource("ocid1.stream.oc1...stream-c"),
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err, "Logging failure must not abort the walk")
	require.Len(t, snaps, 3, "every stream still surfaces even when Logging fails")

	for _, snap := range snaps {
		assert.False(t, snap.HasLogAxis,
			"Logging call failed -> axis defaults to false for all streams (non-fatal)")
	}
}

// --- Test: SourceType is "stream" -----------------------------------

func TestStreamingScanner_SourceTypeIsStream(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("any-stream"),
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, "stream", snaps[0].SourceType,
		"OCI Streaming snapshots must carry SourceType=stream per the per-cloud convention")
	assert.Equal(t, "streaming", snaps[0].Surface,
		"OCI Streaming snapshots must carry Surface=streaming")
	assert.Equal(t, "oci", snaps[0].Provider)
}

// --- Test: ScanEventSources delegates to ScanStreams ----------------

func TestStreamingScanner_ScanEventSourcesDelegatesToScanStreams(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("delegated-stream"),
	}
	fake.LogsByStream["ocid1.stream.oc1...delegated-stream"] = []ociLogResource{
		makeOCILogResource("ocid1.stream.oc1...delegated-stream"),
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")

	// Direct call to ScanStreams for parity comparison.
	direct, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)

	// Fresh fake / scanner for ScanEventSources call so the call
	// counts compare cleanly. Re-seed the same fixture.
	fake2 := newFakeOCIStreaming()
	fake2.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		makeOCIStream("delegated-stream"),
	}
	fake2.LogsByStream["ocid1.stream.oc1...delegated-stream"] = []ociLogResource{
		makeOCILogResource("ocid1.stream.oc1...delegated-stream"),
	}
	s2 := newStreamingScannerWithFake(t, fake2, "us-phoenix-1")

	delegated, err := s2.ScanEventSources(context.Background(), scopeRoot())
	require.NoError(t, err)

	require.Len(t, delegated, len(direct),
		"ScanEventSources should produce the same number of snapshots as ScanStreams")
	require.Len(t, delegated, 1)
	assert.Equal(t, direct[0].ResourceARN, delegated[0].ResourceARN,
		"the two entry points should produce structurally identical snapshots")
	assert.Equal(t, direct[0].HasLogAxis, delegated[0].HasLogAxis)
	assert.Equal(t, direct[0].HasTraceAxis, delegated[0].HasTraceAxis)
	assert.Equal(t, direct[0].Surface, delegated[0].Surface)
}

// --- Test: explicit CompartmentIDs scope overrides default walk -----

func TestStreamingScanner_ScopeCompartmentIDsOverridesDefault(t *testing.T) {
	fake := newFakeOCIStreaming()
	// If the scanner walked the default tenancy-root + children, it
	// would hit /compartments. The scope override bypasses that and
	// walks exactly the supplied compartment.
	fake.StreamsByCompartment["ocid1.compartment.oc1..explicit"] = []ociStream{
		makeOCIStream("explicit-stream"),
	}

	scope := scopeRoot()
	scope.CompartmentIDs = []string{"ocid1.compartment.oc1..explicit"}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scope)
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, 0, fake.CompartmentsCalls,
		"explicit CompartmentIDs scope must bypass the /compartments call")
	assert.Equal(t, 1, fake.StreamsCalls,
		"explicit CompartmentIDs scope walks exactly the supplied compartment list")
	assert.Equal(t, "explicit-stream", snaps[0].ResourceName)
}

// --- Test: classifyOCIStreamingError surface --------------------------

func TestStreamingScanner_ClassifyError_Surface(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		atRoot     bool
		wantPrefix string
		wantEmpty  bool
	}{
		{
			name:       "401_credentials_invalid",
			err:        &ociCallError{StatusCode: http.StatusUnauthorized},
			wantPrefix: ServiceIDEventSource + ": credentials invalid",
		},
		{
			name:       "403_permission_denied",
			err:        &ociCallError{StatusCode: http.StatusForbidden, Message: "policy"},
			wantPrefix: ServiceIDEventSource + ": permission denied",
		},
		{
			name:      "404_midwalk_silent",
			err:       &ociCallError{StatusCode: http.StatusNotFound},
			atRoot:    false,
			wantEmpty: true,
		},
		{
			name:       "404_at_root",
			err:        &ociCallError{StatusCode: http.StatusNotFound},
			atRoot:     true,
			wantPrefix: ServiceIDEventSource + ": Streaming surface not found",
		},
		{
			name:       "429_rate_limit",
			err:        &ociCallError{StatusCode: http.StatusTooManyRequests},
			wantPrefix: ServiceIDEventSource + ": rate limit exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyOCIStreamingError(tc.err, tc.atRoot)
			if tc.wantEmpty {
				assert.Equal(t, "", got)
				return
			}
			assert.True(t, strings.HasPrefix(got, tc.wantPrefix),
				"got %q want prefix %q", got, tc.wantPrefix)
		})
	}
}
