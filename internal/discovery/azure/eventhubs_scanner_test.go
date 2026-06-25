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

// Event source tier slice 8 chunk 1 (v0.89.153, #795 Stream 192)
// acceptance tests per docs/proposals/event-source-tier-slice8.md §11.
//
// Two flights:
//   - Single-surface tests against a fake routing only the Event Hubs
//     endpoints (namespaces list, per-namespace diagnostic settings,
//     per-namespace hubs list for Capture detection).
//   - Three-way dispatcher tests against a fake routing ALL THREE
//     Azure event source surfaces (Service Bus + Event Grid +
//     Event Hubs).

// --- Single-surface fake -------------------------------------------

type fakeAzureEventHubs struct {
	mu sync.Mutex

	Namespaces           []armEventHubsNamespace
	DiagByNS             map[string]armServiceBusDiagnosticSettingsResponse
	Diag404ByNS          map[string]bool
	HubsByNS             map[string][]armEventHubsHub
	Hubs404ByNS          map[string]bool
	NamespacesListStatus int
}

func newFakeAzureEventHubs() *fakeAzureEventHubs {
	return &fakeAzureEventHubs{
		DiagByNS:    map[string]armServiceBusDiagnosticSettingsResponse{},
		Diag404ByNS: map[string]bool{},
		HubsByNS:    map[string][]armEventHubsHub{},
		Hubs404ByNS: map[string]bool{},
	}
}

func (f *fakeAzureEventHubs) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth2/v2.0/token"):
			writeJSON(w, armTokenResponse{
				AccessToken: "fake-bearer-token",
				TokenType:   "Bearer",
				ExpiresIn:   3600,
			})

		case strings.HasSuffix(path, "/providers/Microsoft.EventHub/namespaces"):
			if f.NamespacesListStatus != 0 {
				writeStatus(w, f.NamespacesListStatus, "namespaces list failure")
				return
			}
			writeJSON(w, armEventHubsNamespaceListResponse{Value: f.Namespaces})

		case strings.Contains(path, "/providers/Microsoft.EventHub/namespaces/") &&
			strings.HasSuffix(path, "/eventhubs"):
			nsName := extractNamespaceFromTwoSurfacePath(path, "namespaces")
			if f.Hubs404ByNS[nsName] {
				writeStatus(w, http.StatusNotFound, "no hubs")
				return
			}
			writeJSON(w, armEventHubsHubListResponse{Value: f.HubsByNS[nsName]})

		case strings.Contains(path, "/providers/Microsoft.EventHub/namespaces/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			nsName := extractNamespaceFromTwoSurfacePath(path, "namespaces")
			if f.Diag404ByNS[nsName] {
				writeStatus(w, http.StatusNotFound, "no diag settings")
				return
			}
			settings, ok := f.DiagByNS[nsName]
			if !ok {
				writeStatus(w, http.StatusNotFound, "no diag settings")
				return
			}
			writeJSON(w, settings)

		default:
			writeStatus(w, http.StatusNotFound, fmt.Sprintf("unhandled mock path: %s", path))
		}
	})
}

func newEventHubsScanner(t *testing.T, fake *fakeAzureEventHubs) *Scanner {
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

func makeEventHubsNamespace(name, location string) armEventHubsNamespace {
	return armEventHubsNamespace{
		ID:       fmt.Sprintf("/subscriptions/%s/resourceGroups/rg-test/providers/Microsoft.EventHub/namespaces/%s", testSubID, name),
		Name:     name,
		Location: location,
		Properties: armEventHubsNamespaceProperties{
			Status: "Active",
		},
	}
}

func makeEventHubsHubWithCapture(name string, captureEnabled bool) armEventHubsHub {
	return armEventHubsHub{
		ID:   fmt.Sprintf("/subscriptions/%s/resourceGroups/rg-test/providers/Microsoft.EventHub/namespaces/ns/eventhubs/%s", testSubID, name),
		Name: name,
		Properties: armEventHubsHubProperties{
			CaptureDescription: armEventHubsCaptureDescription{Enabled: captureEnabled},
		},
	}
}

func makeDiagSettingsAppInsights() armServiceBusDiagnosticSettingsResponse {
	return armServiceBusDiagnosticSettingsResponse{
		Value: []armServiceBusDiagnosticSetting{
			{
				Properties: armServiceBusDiagnosticSettingProperties{
					ApplicationInsights: armServiceBusDiagnosticAppInsightsDestination{ConnectionString: "InstrumentationKey=fake"},
				},
			},
		},
	}
}

// --- Test §11.1: list returns namespaces ----------------------------

func TestEventHubsScanner_ListReturnsNamespaces(t *testing.T) {
	fake := newFakeAzureEventHubs()
	fake.Namespaces = []armEventHubsNamespace{
		makeEventHubsNamespace("alpha", "eastus"),
		makeEventHubsNamespace("bravo", "westus"),
	}
	fake.Diag404ByNS["alpha"] = true
	fake.Diag404ByNS["bravo"] = true
	fake.Hubs404ByNS["alpha"] = true
	fake.Hubs404ByNS["bravo"] = true

	s := newEventHubsScanner(t, fake)
	out, err := s.ScanEventHubsNamespaces(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 2)

	assert.Equal(t, "azure", out[0].Provider)
	assert.Equal(t, EventHubsSurface, out[0].Surface)
	assert.Equal(t, eventHubsSourceTypeNamespace, out[0].SourceType)
	assert.Equal(t, "alpha", out[0].ResourceName)
}

// --- Test §11.2-3: diagnostic settings axis -------------------------

func TestEventHubsScanner_DiagnosticSettings_HasLogAxis(t *testing.T) {
	fake := newFakeAzureEventHubs()
	fake.Namespaces = []armEventHubsNamespace{makeEventHubsNamespace("with-diag", "eastus")}
	fake.DiagByNS["with-diag"] = makeDiagSettingsAppInsights()
	fake.Hubs404ByNS["with-diag"] = true

	s := newEventHubsScanner(t, fake)
	out, err := s.ScanEventHubsNamespaces(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasLogAxis,
		"diagnostic settings with App Insights destination must flip HasLogAxis true")
	assert.Equal(t, true, out[0].Detail["has_log"])
}

// --- Test §11.4: no diagnostic settings -----------------------------

func TestEventHubsScanner_NoDiagnosticSettings_HasLogAxisFalse(t *testing.T) {
	fake := newFakeAzureEventHubs()
	fake.Namespaces = []armEventHubsNamespace{makeEventHubsNamespace("no-diag", "eastus")}
	fake.Diag404ByNS["no-diag"] = true
	fake.Hubs404ByNS["no-diag"] = true

	s := newEventHubsScanner(t, fake)
	out, err := s.ScanEventHubsNamespaces(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasLogAxis)
	assert.Equal(t, false, out[0].Detail["has_log"])
}

// --- Test §11.5: at-least-one hub with Capture → has_capture = true -

func TestEventHubsScanner_AtLeastOneHubCapture_HasCaptureTrue(t *testing.T) {
	fake := newFakeAzureEventHubs()
	fake.Namespaces = []armEventHubsNamespace{makeEventHubsNamespace("with-capture", "eastus")}
	fake.Diag404ByNS["with-capture"] = true
	fake.HubsByNS["with-capture"] = []armEventHubsHub{
		makeEventHubsHubWithCapture("hub-no-cap", false),
		makeEventHubsHubWithCapture("hub-cap", true),
	}

	s := newEventHubsScanner(t, fake)
	out, err := s.ScanEventHubsNamespaces(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["has_capture"],
		"at least one hub with captureDescription.enabled=true must flip Detail[has_capture] true")
}

// --- Test §11.6: zero hubs with Capture → has_capture = false -------

func TestEventHubsScanner_ZeroHubsWithCapture_HasCaptureFalse(t *testing.T) {
	fake := newFakeAzureEventHubs()
	fake.Namespaces = []armEventHubsNamespace{makeEventHubsNamespace("no-cap-anywhere", "eastus")}
	fake.Diag404ByNS["no-cap-anywhere"] = true
	fake.HubsByNS["no-cap-anywhere"] = []armEventHubsHub{
		makeEventHubsHubWithCapture("h1", false),
		makeEventHubsHubWithCapture("h2", false),
	}

	s := newEventHubsScanner(t, fake)
	out, err := s.ScanEventHubsNamespaces(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, false, out[0].Detail["has_capture"])
}

// --- Test §11.7: empty hubs list → has_capture = false --------------

func TestEventHubsScanner_EmptyHubsList_HasCaptureFalse(t *testing.T) {
	fake := newFakeAzureEventHubs()
	fake.Namespaces = []armEventHubsNamespace{makeEventHubsNamespace("empty-ns", "eastus")}
	fake.Diag404ByNS["empty-ns"] = true
	fake.Hubs404ByNS["empty-ns"] = true // canonical "namespace has no hubs"

	s := newEventHubsScanner(t, fake)
	out, err := s.ScanEventHubsNamespaces(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 1, "empty namespace still surfaces — operator sees inventory")
	assert.Equal(t, false, out[0].Detail["has_capture"],
		"empty namespace → no hubs to audit → has_capture must be false")
}

// --- Three-way dispatcher fake -------------------------------------

type fakeAzureThreeSurface struct {
	mu sync.Mutex

	Namespaces           []armServiceBusNamespace
	SBDiag404ByNS        map[string]bool
	SBAuthRules404ByNS   map[string]bool
	NamespacesListStatus int

	Topics            []armEventGridTopic
	EGDiag404ByTopic  map[string]bool
	TopicsListStatus  int

	EHNamespaces            []armEventHubsNamespace
	EHDiag404ByNS           map[string]bool
	EHHubs404ByNS           map[string]bool
	EHNamespacesListStatus  int
}

func newFakeAzureThreeSurface() *fakeAzureThreeSurface {
	return &fakeAzureThreeSurface{
		SBDiag404ByNS:      map[string]bool{},
		SBAuthRules404ByNS: map[string]bool{},
		EGDiag404ByTopic:   map[string]bool{},
		EHDiag404ByNS:      map[string]bool{},
		EHHubs404ByNS:      map[string]bool{},
	}
}

func (f *fakeAzureThreeSurface) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/oauth2/v2.0/token"):
			writeJSON(w, armTokenResponse{AccessToken: "fake-bearer-token", TokenType: "Bearer", ExpiresIn: 3600})

		case strings.HasSuffix(path, "/providers/Microsoft.ServiceBus/namespaces"):
			if f.NamespacesListStatus != 0 {
				writeStatus(w, f.NamespacesListStatus, "sb namespaces list failure")
				return
			}
			writeJSON(w, armServiceBusNamespaceListResponse{Value: f.Namespaces})

		case strings.Contains(path, "/providers/Microsoft.ServiceBus/namespaces/") &&
			strings.HasSuffix(path, "/authorizationRules"):
			writeJSON(w, ServiceBusAuthorizationRulesResponse{})

		case strings.Contains(path, "/providers/Microsoft.ServiceBus/namespaces/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			writeStatus(w, http.StatusNotFound, "no diag")

		case strings.HasSuffix(path, "/providers/Microsoft.EventGrid/topics"):
			if f.TopicsListStatus != 0 {
				writeStatus(w, f.TopicsListStatus, "eg topics list failure")
				return
			}
			writeJSON(w, armEventGridTopicListResponse{Value: f.Topics})

		case strings.Contains(path, "/providers/Microsoft.EventGrid/topics/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			writeStatus(w, http.StatusNotFound, "no diag")

		case strings.HasSuffix(path, "/providers/Microsoft.EventHub/namespaces"):
			if f.EHNamespacesListStatus != 0 {
				writeStatus(w, f.EHNamespacesListStatus, "eh namespaces list failure")
				return
			}
			writeJSON(w, armEventHubsNamespaceListResponse{Value: f.EHNamespaces})

		case strings.Contains(path, "/providers/Microsoft.EventHub/namespaces/") &&
			strings.HasSuffix(path, "/eventhubs"):
			writeJSON(w, armEventHubsHubListResponse{})

		case strings.Contains(path, "/providers/Microsoft.EventHub/namespaces/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			writeStatus(w, http.StatusNotFound, "no diag")

		default:
			writeStatus(w, http.StatusNotFound, fmt.Sprintf("unhandled mock path: %s", path))
		}
	})
}

func newThreeSurfaceScanner(t *testing.T, fake *fakeAzureThreeSurface) *Scanner {
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

// --- Test §11.10: three-way dispatcher surfaces all three -----------

func TestScanEventSources_ThreeWay_DispatchesAllSurfaces(t *testing.T) {
	fake := newFakeAzureThreeSurface()
	fake.Namespaces = []armServiceBusNamespace{makeServiceBusNamespace("sb-ns", "eastus", "Standard")}
	fake.Topics = []armEventGridTopic{makeEventGridTopic("eg-topic", "westus", EventGridCloudEventSchemaV1)}
	fake.EHNamespaces = []armEventHubsNamespace{makeEventHubsNamespace("eh-ns", "centralus")}

	s := newThreeSurfaceScanner(t, fake)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 3, "three-way dispatcher must surface BOTH Service Bus AND Event Grid AND Event Hubs")

	var surfaces []string
	for _, snap := range out {
		surfaces = append(surfaces, snap.Surface)
	}
	assert.Contains(t, surfaces, serviceBusEventSourceSurface)
	assert.Contains(t, surfaces, EventGridSurface)
	assert.Contains(t, surfaces, EventHubsSurface)
}

// --- Test §11.13: Event Hubs fails, SB + EG still surface -----------

func TestScanEventSources_ThreeWay_EventHubsFails_OthersStillSurface(t *testing.T) {
	fake := newFakeAzureThreeSurface()
	fake.Namespaces = []armServiceBusNamespace{makeServiceBusNamespace("sb-ns", "eastus", "Standard")}
	fake.Topics = []armEventGridTopic{makeEventGridTopic("eg-topic", "westus", EventGridCloudEventSchemaV1)}
	fake.EHNamespacesListStatus = http.StatusForbidden

	s := newThreeSurfaceScanner(t, fake)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err, "three-way partial-scan: Event Hubs alone failed, SB + EG must still surface")
	require.Len(t, out, 2)
}

// --- Test §11.14: two surfaces fail, third still surfaces -----------

func TestScanEventSources_ThreeWay_TwoFail_ThirdStillSurfaces(t *testing.T) {
	fake := newFakeAzureThreeSurface()
	fake.NamespacesListStatus = http.StatusForbidden
	fake.TopicsListStatus = http.StatusForbidden
	fake.EHNamespaces = []armEventHubsNamespace{makeEventHubsNamespace("eh-only", "eastus")}

	s := newThreeSurfaceScanner(t, fake)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err, "three-way partial-scan: two surfaces failed, Event Hubs alone surfaces")
	require.Len(t, out, 1)
	assert.Equal(t, EventHubsSurface, out[0].Surface)
}

// --- Test §11.15: all three fail → error mentions all three ---------

func TestScanEventSources_ThreeWay_AllFail_ErrorMentionsAllThreeSurfaces(t *testing.T) {
	fake := newFakeAzureThreeSurface()
	fake.NamespacesListStatus = http.StatusForbidden
	fake.TopicsListStatus = http.StatusForbidden
	fake.EHNamespacesListStatus = http.StatusForbidden

	s := newThreeSurfaceScanner(t, fake)
	_, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.Error(t, err)
	errStr := err.Error()
	assert.Contains(t, errStr, ServiceIDServiceBus)
	assert.Contains(t, errStr, ServiceIDEventGrid)
	assert.Contains(t, errStr, ServiceIDEventHubs)
}
