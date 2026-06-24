// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Event source tier slice 6 chunk 1 (v0.89.147, #787 Stream 185).
// The tests in this file pin the Event Grid scanner's per-topic
// detection logic (HasLogAxis via diagnostic settings, HasTraceAxis
// via inputSchema = CloudEventSchemaV1_0) per docs/proposals/
// event-source-tier-slice6.md §3 + §11 acceptance tests 1-9 + the
// two-way dispatcher's partial-scan posture (tests 10-13).
//
// The Event Grid mock routes three endpoints: the OAuth token
// endpoint, Microsoft.EventGrid/topics list (subscription scope),
// and the per-topic microsoft.insights/diagnosticSettings GET.
// Parallel to fakeAzureServiceBus from slice 1 chunk 3
// (v0.89.101) — the diagnostic-settings mock reuses the shared
// armServiceBusDiagnosticSettingsResponse types per the slice 6
// chunk 1 design doc §5 note on shared shape.

type fakeAzureEventGrid struct {
	mu sync.Mutex

	Topics              []armEventGridTopic
	TopicsPages         []armEventGridTopicListResponse
	DiagSettingsByTopic map[string]armServiceBusDiagnosticSettingsResponse

	// Failure-injection knobs.
	TopicsListStatus      int
	DiagSettingsStatus    int
	DiagSettings404ByName map[string]bool

	// Call counters for assertions.
	TokenCalls        int
	TopicsListCalls   int
	DiagSettingsCalls int
	LastBearer        string
}

func newFakeAzureEventGrid() *fakeAzureEventGrid {
	return &fakeAzureEventGrid{
		DiagSettingsByTopic:   map[string]armServiceBusDiagnosticSettingsResponse{},
		DiagSettings404ByName: map[string]bool{},
	}
}

func (f *fakeAzureEventGrid) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth2/v2.0/token"):
			f.TokenCalls++
			writeJSON(w, armTokenResponse{
				AccessToken: "fake-bearer-token",
				TokenType:   "Bearer",
				ExpiresIn:   3600,
			})

		case strings.HasSuffix(path, "/providers/Microsoft.EventGrid/topics"):
			f.TopicsListCalls++
			f.LastBearer = r.Header.Get("Authorization")
			if f.TopicsListStatus != 0 {
				writeStatus(w, f.TopicsListStatus, "topics list failure")
				return
			}
			if len(f.TopicsPages) > 0 {
				idx := f.TopicsListCalls - 1
				if idx >= len(f.TopicsPages) {
					idx = len(f.TopicsPages) - 1
				}
				writeJSON(w, f.TopicsPages[idx])
				return
			}
			writeJSON(w, armEventGridTopicListResponse{Value: f.Topics})

		case strings.Contains(path, "/providers/Microsoft.EventGrid/topics/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			f.DiagSettingsCalls++
			topicName := extractTopicNameFromDiagPath(path)
			if f.DiagSettings404ByName[topicName] {
				writeStatus(w, http.StatusNotFound, "no diagnostic settings")
				return
			}
			if f.DiagSettingsStatus != 0 {
				writeStatus(w, f.DiagSettingsStatus, "diag failure")
				return
			}
			settings, ok := f.DiagSettingsByTopic[topicName]
			if !ok {
				writeStatus(w, http.StatusNotFound, "no diagnostic settings")
				return
			}
			writeJSON(w, settings)

		default:
			writeStatus(w, http.StatusNotFound, fmt.Sprintf("unhandled mock path: %s", path))
		}
	})
}

// extractTopicNameFromDiagPath pulls the {topic} segment out of the
// per-topic diagnostic-settings URL path:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.EventGrid/topics/<topic>/providers/microsoft.insights/diagnosticSettings
func extractTopicNameFromDiagPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "topics" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func newEventGridScannerWithFake(t *testing.T, fake *fakeAzureEventGrid) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: testSubID,
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}
}

func newPaginatedEventGridScanner(t *testing.T, fake *fakeAzureEventGrid) (*Scanner, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: testSubID,
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}, srv
}

// --- Helpers --------------------------------------------------------

func makeEventGridTopic(name, location, inputSchema string) armEventGridTopic {
	return armEventGridTopic{
		ID: fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/rg-%s/providers/Microsoft.EventGrid/topics/%s",
			testSubID, name, name,
		),
		Name:     name,
		Location: location,
		Type:     "Microsoft.EventGrid/topics",
		Properties: armEventGridTopicProperties{
			InputSchema:       inputSchema,
			ProvisioningState: "Succeeded",
		},
	}
}

func egDiagWithAppInsights(connStr string) armServiceBusDiagnosticSettingsResponse {
	return armServiceBusDiagnosticSettingsResponse{
		Value: []armServiceBusDiagnosticSetting{{
			Name: "ds-ai",
			Properties: armServiceBusDiagnosticSettingProperties{
				ApplicationInsights: armServiceBusDiagnosticAppInsightsDestination{
					ConnectionString: connStr,
				},
			},
		}},
	}
}

func egDiagWithWorkspace(workspaceID string) armServiceBusDiagnosticSettingsResponse {
	return armServiceBusDiagnosticSettingsResponse{
		Value: []armServiceBusDiagnosticSetting{{
			Name: "ds-la",
			Properties: armServiceBusDiagnosticSettingProperties{
				WorkspaceID: workspaceID,
			},
		}},
	}
}

// --- Tests ----------------------------------------------------------

// Acceptance test 1 — paginated list response is walked.
func TestScanEventGridTopics_ListReturnsTopics_Paginated(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.TopicsPages = []armEventGridTopicListResponse{
		{Value: []armEventGridTopic{makeEventGridTopic("eg-page1", "eastus", EventGridCloudEventSchemaV1)}},
		{Value: []armEventGridTopic{makeEventGridTopic("eg-page2", "westus", "EventGridSchema")}},
	}
	// 404 on both topics' diag settings — pagination test does not
	// care about the per-topic axes; both rows surface regardless.
	fake.DiagSettings404ByName["eg-page1"] = true
	fake.DiagSettings404ByName["eg-page2"] = true

	s, srv := newPaginatedEventGridScanner(t, fake)
	fake.TopicsPages[0].NextLink = fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.EventGrid/topics?api-version=%s&page=2",
		srv.URL, testSubID, EventGridTopicsAPIVersion,
	)

	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 2, "both pages must be walked")
	assert.Equal(t, 2, fake.TopicsListCalls, "expected two list calls (one per page)")
}

// Acceptance test 2 — topic with diagnostic settings to App Insights
// → HasLogAxis = true.
func TestScanEventGridTopics_TopicWithDiagnosticSettingsToAppInsights_HasLogAxis(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-ai", "eastus", "EventGridSchema"),
	}
	fake.DiagSettingsByTopic["eg-ai"] = egDiagWithAppInsights(
		"InstrumentationKey=abc;IngestionEndpoint=https://x")

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasLogAxis, "App Insights diagnostic setting must flip HasLogAxis")
	assert.False(t, snap.HasTraceAxis, "EventGridSchema input does NOT satisfy the trace axis")
}

// Acceptance test 3 — topic with diagnostic settings to Log Analytics
// workspace → HasLogAxis = true.
func TestScanEventGridTopics_TopicWithDiagnosticSettingsToLogAnalyticsWorkspace_HasLogAxis(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-la", "westus", "EventGridSchema"),
	}
	fake.DiagSettingsByTopic["eg-la"] = egDiagWithWorkspace(
		"/subscriptions/x/y/Microsoft.OperationalInsights/workspaces/w1")

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasLogAxis, "Log Analytics workspace destination must flip HasLogAxis")
	assert.Equal(t, "westus", snap.Region)
}

// Acceptance test 4 — topic without diagnostic settings → HasLogAxis = false.
func TestScanEventGridTopics_TopicWithoutDiagnosticSettings_NoLogAxis(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-bare", "eastus", "EventGridSchema"),
	}
	// Intentionally not seeding DiagSettingsByTopic — fake serves
	// 404, which the scanner treats as "no diagnostic settings".

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasLogAxis)
	assert.False(t, snaps[0].HasTraceAxis)
}

// Acceptance test 5 — topic with inputSchema = "CloudEventSchemaV1_0"
// → HasTraceAxis = true (CloudEvents 1.0 carries traceparent via the
// distributed-tracing extension).
func TestScanEventGridTopics_TopicWithCloudEventSchemaV1_HasTraceAxis(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-cev1", "eastus", EventGridCloudEventSchemaV1),
	}
	fake.DiagSettings404ByName["eg-cev1"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasTraceAxis,
		"inputSchema == CloudEventSchemaV1_0 must flip HasTraceAxis")
	require.NotNil(t, snap.Detail)
	assert.Equal(t, EventGridCloudEventSchemaV1, snap.Detail["input_schema"])
}

// Acceptance test 6 — topic with inputSchema = "EventGridSchema" →
// HasTraceAxis = false (Azure proprietary schema lacks the CloudEvents
// 1.0 distributed-tracing extension).
func TestScanEventGridTopics_TopicWithEventGridSchema_NoTraceAxis(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-egschema", "eastus", "EventGridSchema"),
	}
	fake.DiagSettings404ByName["eg-egschema"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasTraceAxis,
		"EventGridSchema is the Azure proprietary format; must NOT flip HasTraceAxis")
}

// Acceptance test 7 — topic with inputSchema = "CustomEventSchema" →
// HasTraceAxis = false (operator-defined schema lacks the CloudEvents
// 1.0 distributed-tracing extension).
func TestScanEventGridTopics_TopicWithCustomEventSchema_NoTraceAxis(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-custom", "eastus", "CustomEventSchema"),
	}
	fake.DiagSettings404ByName["eg-custom"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.False(t, snaps[0].HasTraceAxis,
		"CustomEventSchema is the operator-defined format; must NOT flip HasTraceAxis")
}

// Acceptance test 8 — topic with publicNetworkAccess = "Enabled" →
// snapshot Detail records the flag.
func TestScanEventGridTopics_TopicWithPublicNetworkAccess_DetailRecordsFlag(t *testing.T) {
	fake := newFakeAzureEventGrid()
	topic := makeEventGridTopic("eg-pna", "eastus", "EventGridSchema")
	topic.Properties.PublicNetworkAccess = "Enabled"
	fake.Topics = []armEventGridTopic{topic}
	fake.DiagSettings404ByName["eg-pna"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	require.NotNil(t, snaps[0].Detail)
	assert.Equal(t, "Enabled", snaps[0].Detail["public_network_access"],
		"publicNetworkAccess must surface in Detail bag for operator review")
}

// Acceptance test 9 — topic with disableLocalAuth = true → snapshot
// Detail records the AAD-only flag.
func TestScanEventGridTopics_TopicWithDisableLocalAuth_DetailRecordsAADOnlyFlag(t *testing.T) {
	fake := newFakeAzureEventGrid()
	topic := makeEventGridTopic("eg-aad", "eastus", "EventGridSchema")
	topic.Properties.DisableLocalAuth = true
	fake.Topics = []armEventGridTopic{topic}
	fake.DiagSettings404ByName["eg-aad"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	require.NotNil(t, snaps[0].Detail)
	assert.Equal(t, true, snaps[0].Detail["disable_local_auth"],
		"disableLocalAuth=true must surface in Detail bag (AAD-only auth)")
}

// --- Surgical coverage tests ----------------------------------------

func TestScanEventGridTopics_DiagnosticSettings404TreatedAsNoSettings(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-404", "eastus", "EventGridSchema"),
	}
	fake.DiagSettings404ByName["eg-404"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err, "404 on diag settings must NOT bubble up as an error")
	require.Len(t, snaps, 1)
	assert.False(t, snaps[0].HasLogAxis)
}

func TestScanEventGridTopics_AccessTokenEmpty_GracefullyReturnsEmpty(t *testing.T) {
	// A scanner with no SubscriptionID configured returns nil, nil
	// without making any network calls — matches the design doc §5
	// graceful posture.
	s := &Scanner{}
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	assert.Empty(t, snaps,
		"unconfigured scanner must return empty rows without erroring")
}

func TestScanEventGridTopics_ResourceNamePopulated(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-meta", "eastus", "EventGridSchema"),
	}
	fake.DiagSettings404ByName["eg-meta"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.Equal(t, "eg-meta", snap.ResourceName, "ResourceName must be the topic name")
	assert.Contains(t, snap.ResourceARN, "Microsoft.EventGrid/topics/eg-meta",
		"ResourceARN must carry the full ARM id")
	assert.Equal(t, testSubID, snap.AccountID, "AccountID falls back to the subscription id")
}

func TestScanEventGridTopics_SurfaceIsEventgrid(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-surface", "eastus", "EventGridSchema"),
	}
	fake.DiagSettings404ByName["eg-surface"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.Equal(t, EventGridSurface, snaps[0].Surface,
		"slice 6 chunk 1 surfaces Event Grid Custom Topics — Surface must be \"eventgrid\"")
	assert.Equal(t, azureProviderID, snaps[0].Provider, "Provider must be \"azure\"")
	assert.Equal(t, eventGridSourceTypeTopic, snaps[0].SourceType,
		"slice 6 chunk 1 surfaces Custom Topics — SourceType must be \"topic\"")
}

func TestScanEventGridTopics_AccountIDOverride(t *testing.T) {
	fake := newFakeAzureEventGrid()
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-acct", "eastus", "EventGridSchema"),
	}
	fake.DiagSettings404ByName["eg-acct"] = true

	s := newEventGridScannerWithFake(t, fake)
	snaps, err := s.ScanEventGridTopics(context.Background(), scanner.ScanScope{AccountID: "override-acct"})
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, "override-acct", snaps[0].AccountID)
}
