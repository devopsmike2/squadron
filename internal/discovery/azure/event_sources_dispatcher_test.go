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

// --- Slice 6 two-way dispatcher tests --------------------------------
//
// ScanEventSources fans out across Service Bus + Event Grid with a
// two-way partial-scan posture. Tests 10 / 11 / 12 / 13 of the
// slice 6 design doc pin the contract: both surfaces dispatched
// independently; either failing does NOT block the other; only when
// BOTH fail does the dispatcher return a non-nil error wrapping
// every per-surface cause.
//
// The slice 6 chunk 1 dispatcher mock routes FIVE endpoints: the
// OAuth token endpoint (single token shared across surfaces),
// Microsoft.ServiceBus/namespaces list, per-namespace diagnostic
// settings + authorization rules, Microsoft.EventGrid/topics list,
// and per-topic diagnostic settings. The shared mock unifies the
// two slice-1 / slice-6 surface mocks so the dispatcher tests
// exercise the real cross-surface fan-out.

type fakeAzureTwoSurface struct {
	mu sync.Mutex

	// Service Bus state.
	Namespaces           []armServiceBusNamespace
	SBDiagByNS           map[string]armServiceBusDiagnosticSettingsResponse
	SBDiag404ByNS        map[string]bool
	SBAuthRules404ByNS   map[string]bool
	NamespacesListStatus int

	// Event Grid state.
	Topics           []armEventGridTopic
	EGDiagByTopic    map[string]armServiceBusDiagnosticSettingsResponse
	EGDiag404ByTopic map[string]bool
	TopicsListStatus int
}

func newFakeAzureTwoSurface() *fakeAzureTwoSurface {
	return &fakeAzureTwoSurface{
		SBDiagByNS:         map[string]armServiceBusDiagnosticSettingsResponse{},
		SBDiag404ByNS:      map[string]bool{},
		SBAuthRules404ByNS: map[string]bool{},
		EGDiagByTopic:      map[string]armServiceBusDiagnosticSettingsResponse{},
		EGDiag404ByTopic:   map[string]bool{},
	}
}

func (f *fakeAzureTwoSurface) handler() http.Handler {
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

		case strings.HasSuffix(path, "/providers/Microsoft.ServiceBus/namespaces"):
			if f.NamespacesListStatus != 0 {
				writeStatus(w, f.NamespacesListStatus, "namespaces list failure")
				return
			}
			writeJSON(w, armServiceBusNamespaceListResponse{Value: f.Namespaces})

		case strings.Contains(path, "/providers/Microsoft.ServiceBus/namespaces/") &&
			strings.HasSuffix(path, "/authorizationRules"):
			// Per-namespace auth rules: tests default to empty list
			// (RBAC-only namespace; propagation = preserved).
			nsName := extractNamespaceFromTwoSurfacePath(path, "namespaces")
			if f.SBAuthRules404ByNS[nsName] {
				writeStatus(w, http.StatusNotFound, "no rules")
				return
			}
			writeJSON(w, ServiceBusAuthorizationRulesResponse{})

		case strings.Contains(path, "/providers/Microsoft.ServiceBus/namespaces/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			nsName := extractNamespaceFromTwoSurfacePath(path, "namespaces")
			if f.SBDiag404ByNS[nsName] {
				writeStatus(w, http.StatusNotFound, "no diag settings")
				return
			}
			settings, ok := f.SBDiagByNS[nsName]
			if !ok {
				writeStatus(w, http.StatusNotFound, "no diag settings")
				return
			}
			writeJSON(w, settings)

		case strings.HasSuffix(path, "/providers/Microsoft.EventGrid/topics"):
			if f.TopicsListStatus != 0 {
				writeStatus(w, f.TopicsListStatus, "topics list failure")
				return
			}
			writeJSON(w, armEventGridTopicListResponse{Value: f.Topics})

		case strings.Contains(path, "/providers/Microsoft.EventGrid/topics/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			topicName := extractNamespaceFromTwoSurfacePath(path, "topics")
			if f.EGDiag404ByTopic[topicName] {
				writeStatus(w, http.StatusNotFound, "no diag settings")
				return
			}
			settings, ok := f.EGDiagByTopic[topicName]
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

// extractNamespaceFromTwoSurfacePath pulls the resource-name segment
// out of an ARM child path, given the parent-collection name. Works
// for both "namespaces" (Service Bus) and "topics" (Event Grid).
func extractNamespaceFromTwoSurfacePath(path, collection string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == collection && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func newTwoSurfaceScanner(t *testing.T, fake *fakeAzureTwoSurface) *Scanner {
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

// TestScanEventSources_DispatchesToBothServiceBusAndEventGrid — slice
// 6 acceptance test 10: the dispatcher returns BOTH Service Bus
// namespaces AND Event Grid topics when both surfaces produce data.
func TestScanEventSources_DispatchesToBothServiceBusAndEventGrid(t *testing.T) {
	fake := newFakeAzureTwoSurface()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-namespace", "eastus", "Standard"),
	}
	fake.SBDiag404ByNS["sb-namespace"] = true
	fake.SBAuthRules404ByNS["sb-namespace"] = true
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-topic", "westus", EventGridCloudEventSchemaV1),
	}
	fake.EGDiag404ByTopic["eg-topic"] = true

	s := newTwoSurfaceScanner(t, fake)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, out, 2, "dispatcher must return both the Service Bus namespace AND the Event Grid topic")

	// Surface order: Service Bus first (slice 1 chunk 3 surface),
	// then Event Grid (slice 6 chunk 1 surface). The dispatcher's
	// ordering pins this for the per-cloud Inventory tab.
	assert.Equal(t, serviceBusEventSourceSurface, out[0].Surface)
	assert.Equal(t, "sb-namespace", out[0].ResourceName)
	assert.Equal(t, EventGridSurface, out[1].Surface)
	assert.Equal(t, "eg-topic", out[1].ResourceName)
	assert.True(t, out[1].HasTraceAxis,
		"Event Grid topic with CloudEventSchemaV1_0 input schema must carry HasTraceAxis through the dispatcher")
}

// TestScanEventSources_ServiceBusFails_EventGridStillSurfaces — slice
// 6 acceptance test 11: the partial-scan posture in the Service Bus-
// fails direction. The Event Grid topic row still surfaces; the
// dispatcher returns no error because at least one surface succeeded.
func TestScanEventSources_ServiceBusFails_EventGridStillSurfaces(t *testing.T) {
	fake := newFakeAzureTwoSurface()
	// Service Bus list returns 403 — the surface is "offline" from
	// the dispatcher's perspective.
	fake.NamespacesListStatus = http.StatusForbidden
	fake.Topics = []armEventGridTopic{
		makeEventGridTopic("eg-still-here", "eastus", EventGridCloudEventSchemaV1),
	}
	fake.EGDiag404ByTopic["eg-still-here"] = true

	s := newTwoSurfaceScanner(t, fake)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err, "two-way partial-scan posture: only Service Bus failed")
	require.Len(t, out, 1, "Event Grid topic still surfaces when Service Bus list fails")
	assert.Equal(t, EventGridSurface, out[0].Surface)
	assert.Equal(t, "eg-still-here", out[0].ResourceName)
}

// TestScanEventSources_EventGridFails_ServiceBusStillSurfaces — slice
// 6 acceptance test 12: the partial-scan posture in the Event Grid-
// fails direction. The Service Bus namespace row still surfaces; the
// dispatcher returns no error because at least one surface succeeded.
func TestScanEventSources_EventGridFails_ServiceBusStillSurfaces(t *testing.T) {
	fake := newFakeAzureTwoSurface()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-still-here", "eastus", "Standard"),
	}
	fake.SBDiag404ByNS["sb-still-here"] = true
	fake.SBAuthRules404ByNS["sb-still-here"] = true
	// Event Grid list returns 403 — the surface is "offline" from
	// the dispatcher's perspective.
	fake.TopicsListStatus = http.StatusForbidden

	s := newTwoSurfaceScanner(t, fake)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err, "two-way partial-scan posture: only Event Grid failed")
	require.Len(t, out, 1, "Service Bus namespace still surfaces when Event Grid list fails")
	assert.Equal(t, serviceBusEventSourceSurface, out[0].Surface)
	assert.Equal(t, "sb-still-here", out[0].ResourceName)
}

// TestScanEventSources_BothFailReturnsErrorMentioningBothSurfaces —
// slice 6 acceptance test 13: the dispatcher's only error-returning
// path on the slice 6 chunk 1 surface (the token-failure path is the
// slice 1 chunk 3 error). BOTH surfaces fail. The returned error
// must mention both servicebus AND eventgrid so the operator-facing
// error message captures the full failure envelope.
func TestScanEventSources_BothFailReturnsErrorMentioningBothSurfaces(t *testing.T) {
	fake := newFakeAzureTwoSurface()
	fake.NamespacesListStatus = http.StatusForbidden
	fake.TopicsListStatus = http.StatusForbidden

	s := newTwoSurfaceScanner(t, fake)
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), ServiceIDServiceBus,
		"both-fail error must name the Service Bus surface")
	assert.Contains(t, err.Error(), ServiceIDEventGrid,
		"both-fail error must name the Event Grid surface")
	assert.Empty(t, out, "both surfaces failed; no rows surface")
}
