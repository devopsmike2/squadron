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

// Slice 2 chunk 3 of the Event source tier arc (v0.89.106, #743 Stream
// 141). The tests in this file pin the per-namespace propagation
// detection logic — inspectAuthorizationRules — plus the
// scanner-level integration: the authorizationRules sub-resource walk
// is folded into ScanServiceBus and the per-namespace
// EventSourceInstanceSnapshot now carries HasPropagationConfig +
// PropagationNotes. The acceptance tests 11-12 from
// docs/proposals/event-source-tier-slice2.md §11 land here.
//
// Why this file is separate from servicebus_scanner_test.go: the
// slice-1 tests pinned the diagnostic-settings axis; the slice-2
// tests pin the authorizationRules surface. Keeping them in two
// files makes the pre-slice-2 / post-slice-2 diff obvious to
// reviewers and lets the chunk-3 commit land without rewriting the
// slice-1 fixture helpers.

// --- Fixture builders -----------------------------------------------

// makeAuthRule returns a ServiceBusAuthorizationRule with the supplied
// name and rights. The Properties.Rights slice is taken verbatim —
// callers supply [Listen, Send], [Listen], [Send], [Manage], etc.
func makeAuthRule(name string, rights ...string) ServiceBusAuthorizationRule {
	return ServiceBusAuthorizationRule{
		Name: name,
		Properties: ServiceBusAuthorizationRuleProperties{
			Rights: rights,
		},
	}
}

// --- inspectAuthorizationRules table tests --------------------------

// TestInspectAuthorizationRules_ListenSendRule_Preserved — acceptance
// test 11. A namespace with a single rule carrying both Listen and
// Send rights preserves propagation (publishers can attach
// ApplicationProperties including traceparent).
func TestInspectAuthorizationRules_ListenSendRule_Preserved(t *testing.T) {
	rules := []ServiceBusAuthorizationRule{
		makeAuthRule("RootManageSharedAccessKey", ServiceBusRightListen, ServiceBusRightSend),
	}
	preserved, note := inspectAuthorizationRules(rules)
	assert.True(t, preserved, "Listen+Send rule must preserve propagation")
	assert.Empty(t, note, "preserved namespaces carry no per-issue note")
}

// TestInspectAuthorizationRules_SendOnlyRule_Preserved — Send alone
// is sufficient. The chunk-3 detection rule keys off Send specifically
// (the right required for publishers to attach properties) — the
// rule does not need Listen to satisfy the propagation axis.
func TestInspectAuthorizationRules_SendOnlyRule_Preserved(t *testing.T) {
	rules := []ServiceBusAuthorizationRule{
		makeAuthRule("publisher-only", ServiceBusRightSend),
	}
	preserved, note := inspectAuthorizationRules(rules)
	assert.True(t, preserved, "Send-only rule must preserve propagation (Send is the load-bearing right)")
	assert.Empty(t, note)
}

// TestInspectAuthorizationRules_ListenOnlyRules_BrokenNote —
// namespaces with only Listen-capable rules cannot have any publisher
// attach the traceparent ApplicationProperty. PROPAGATION BROKEN with
// the informational note that names the missing right.
func TestInspectAuthorizationRules_ListenOnlyRules_BrokenNote(t *testing.T) {
	rules := []ServiceBusAuthorizationRule{
		makeAuthRule("consumer-1", ServiceBusRightListen),
		makeAuthRule("consumer-2", ServiceBusRightListen),
	}
	preserved, note := inspectAuthorizationRules(rules)
	assert.False(t, preserved, "Listen-only rules cannot host a Send-capable publisher")
	require.NotEmpty(t, note)
	assert.Contains(t, strings.ToLower(note), "send")
	assert.Contains(t, strings.ToLower(note), "traceparent")
}

// TestInspectAuthorizationRules_MultipleRules_AnySendSatisfies — a
// namespace with a mix of Listen-only and Listen+Send rules satisfies
// the propagation axis because at least one rule carries Send. The
// "any Send-capable rule" predicate matches the design doc §3.3
// simplification (the heuristic is an OR over the rules list).
func TestInspectAuthorizationRules_MultipleRules_AnySendSatisfies(t *testing.T) {
	rules := []ServiceBusAuthorizationRule{
		makeAuthRule("consumer-only", ServiceBusRightListen),
		makeAuthRule("consumer-only-2", ServiceBusRightListen),
		makeAuthRule("publisher", ServiceBusRightListen, ServiceBusRightSend),
	}
	preserved, note := inspectAuthorizationRules(rules)
	assert.True(t, preserved, "at least one Send-capable rule satisfies the namespace axis")
	assert.Empty(t, note)
}

// TestInspectAuthorizationRules_EmptyRulesList_BrokenNote — a
// namespace with zero authorizationRules is the canonical RBAC-only
// namespace shape. The chunk-3 heuristic cannot prove or disprove
// propagation in this case (RBAC role property restrictions are out
// of scope per §3.3), so propagation DEFAULTS TO PRESERVED with an
// informational note rather than emitting a false-positive broken
// recommendation. The "BrokenNote" name predates the no-false-
// positives stance; the test name reads as "the empty-list branch
// emits a note" — the note is informational, the axis is preserved.
func TestInspectAuthorizationRules_EmptyRulesList_BrokenNote(t *testing.T) {
	preserved, note := inspectAuthorizationRules(nil)
	assert.True(t, preserved,
		"empty rules list defaults to preserved (no false positives on RBAC-only namespaces)")
	require.NotEmpty(t, note)
	assert.Contains(t, strings.ToLower(note), "rbac")
}

// TestInspectAuthorizationRules_ManageRuleAlone_NotPreserved — the
// chunk-3 detection rule reads Send literally; a Manage-only rule
// (which would imply Send + Listen in practice) does NOT satisfy the
// chunk-3 predicate. The chunk-3 simplification trades a
// missed-detection for code simplicity; slice 3 may broaden if
// operator feedback warrants. This test pins the documented
// chunk-3 behavior so a future broadening is an explicit edit, not
// an accidental regression.
func TestInspectAuthorizationRules_ManageRuleAlone_NotPreserved(t *testing.T) {
	rules := []ServiceBusAuthorizationRule{
		makeAuthRule("admin", ServiceBusRightManage),
	}
	preserved, note := inspectAuthorizationRules(rules)
	assert.False(t, preserved,
		"chunk-3 rule reads Send literally — Manage alone does not satisfy")
	require.NotEmpty(t, note)
}

// --- Fake handler for the authorizationRules sub-resource -----------

// fakeAzureServiceBusWithAuth extends the slice-1 fake with
// authorizationRules routing. Mirrors the slice-1 fakeAzureServiceBus
// layout — same handler dispatch shape, same failure-injection knobs,
// plus per-namespace auth-rules fixtures and a pagination knob.
type fakeAzureServiceBusWithAuth struct {
	mu sync.Mutex

	Namespaces         []armServiceBusNamespace
	DiagSettingsByNS   map[string]armServiceBusDiagnosticSettingsResponse
	AuthRulesByNS      map[string]ServiceBusAuthorizationRulesResponse
	AuthRulesPagesByNS map[string][]ServiceBusAuthorizationRulesResponse

	// Failure-injection knobs.
	DiagSettings404ForNS map[string]bool
	AuthRules404ForNS    map[string]bool
	AuthRulesStatus      int

	// Call counters.
	TokenCalls         int
	NamespacesCalls    int
	DiagSettingsCalls  int
	AuthRulesCalls     int
	AuthRulesCallsByNS map[string]int
}

func newFakeAzureServiceBusWithAuth() *fakeAzureServiceBusWithAuth {
	return &fakeAzureServiceBusWithAuth{
		DiagSettingsByNS:     map[string]armServiceBusDiagnosticSettingsResponse{},
		AuthRulesByNS:        map[string]ServiceBusAuthorizationRulesResponse{},
		AuthRulesPagesByNS:   map[string][]ServiceBusAuthorizationRulesResponse{},
		DiagSettings404ForNS: map[string]bool{},
		AuthRules404ForNS:    map[string]bool{},
		AuthRulesCallsByNS:   map[string]int{},
	}
}

func (f *fakeAzureServiceBusWithAuth) handler() http.Handler {
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

		case strings.HasSuffix(path, "/providers/Microsoft.ServiceBus/namespaces"):
			f.NamespacesCalls++
			writeJSON(w, armServiceBusNamespaceListResponse{Value: f.Namespaces})

		case strings.Contains(path, "/providers/Microsoft.ServiceBus/namespaces/") &&
			strings.Contains(path, "/providers/microsoft.insights/diagnosticSettings"):
			f.DiagSettingsCalls++
			nsName := extractNamespaceFromDiagPath(path)
			if f.DiagSettings404ForNS[nsName] {
				writeStatus(w, http.StatusNotFound, "no diagnostic settings")
				return
			}
			settings, ok := f.DiagSettingsByNS[nsName]
			if !ok {
				writeStatus(w, http.StatusNotFound, "no diagnostic settings")
				return
			}
			writeJSON(w, settings)

		case strings.Contains(path, "/providers/Microsoft.ServiceBus/namespaces/") &&
			strings.HasSuffix(path, "/authorizationRules"):
			nsName := extractNamespaceFromAuthRulesPath(path)
			f.AuthRulesCalls++
			f.AuthRulesCallsByNS[nsName]++
			if f.AuthRules404ForNS[nsName] {
				writeStatus(w, http.StatusNotFound, "no authorization rules")
				return
			}
			if f.AuthRulesStatus != 0 {
				writeStatus(w, f.AuthRulesStatus, "auth rules failure")
				return
			}
			if pages, ok := f.AuthRulesPagesByNS[nsName]; ok && len(pages) > 0 {
				idx := f.AuthRulesCallsByNS[nsName] - 1
				if idx >= len(pages) {
					idx = len(pages) - 1
				}
				writeJSON(w, pages[idx])
				return
			}
			if rules, ok := f.AuthRulesByNS[nsName]; ok {
				writeJSON(w, rules)
				return
			}
			// Default: empty list (RBAC-only namespace shape).
			writeJSON(w, ServiceBusAuthorizationRulesResponse{})

		default:
			writeStatus(w, http.StatusNotFound, fmt.Sprintf("unhandled mock path: %s", path))
		}
	})
}

// extractNamespaceFromAuthRulesPath pulls the {namespace} segment out
// of the per-namespace authorizationRules URL path:
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.ServiceBus/namespaces/<namespace>/authorizationRules
func extractNamespaceFromAuthRulesPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "namespaces" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func newServiceBusScannerWithAuthFake(t *testing.T, fake *fakeAzureServiceBusWithAuth) (*Scanner, *httptest.Server) {
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

// --- Scanner-level integration tests --------------------------------

// TestServiceBusScanner_NamespaceWithListenSendRule_HasPropagationConfig
// — end-to-end: a namespace whose authorizationRules list returns a
// rule with Listen+Send rights surfaces a snapshot with
// HasPropagationConfig=true and zero PropagationNotes. Acceptance
// test 11.
func TestServiceBusScanner_NamespaceWithListenSendRule_HasPropagationConfig(t *testing.T) {
	fake := newFakeAzureServiceBusWithAuth()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-pub-sub", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-pub-sub"] = true
	fake.AuthRulesByNS["sb-pub-sub"] = ServiceBusAuthorizationRulesResponse{
		Value: []ServiceBusAuthorizationRule{
			makeAuthRule("RootManageSharedAccessKey",
				ServiceBusRightListen, ServiceBusRightSend),
		},
	}

	s, _ := newServiceBusScannerWithAuthFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasPropagationConfig,
		"Listen+Send rule must flip HasPropagationConfig true")
	assert.Empty(t, snap.PropagationNotes,
		"preserved namespaces carry no PropagationNotes")
}

// TestServiceBusScanner_NamespaceWithListenOnly_NoPropagationConfig
// — acceptance test 12 variant. A namespace with only Listen-capable
// rules cannot host a Send-capable publisher; HasPropagationConfig is
// false and PropagationNotes records the informational explanation.
func TestServiceBusScanner_NamespaceWithListenOnly_NoPropagationConfig(t *testing.T) {
	fake := newFakeAzureServiceBusWithAuth()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-consume-only", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-consume-only"] = true
	fake.AuthRulesByNS["sb-consume-only"] = ServiceBusAuthorizationRulesResponse{
		Value: []ServiceBusAuthorizationRule{
			makeAuthRule("consumer-a", ServiceBusRightListen),
			makeAuthRule("consumer-b", ServiceBusRightListen),
		},
	}

	s, _ := newServiceBusScannerWithAuthFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.False(t, snap.HasPropagationConfig,
		"Listen-only rules must flip HasPropagationConfig false")
	require.Len(t, snap.PropagationNotes, 1)
	assert.Contains(t, strings.ToLower(snap.PropagationNotes[0]), "send")
}

// TestServiceBusScanner_AuthorizationRulesAPIError_NonFatal — a 5xx
// (or other non-404 failure) on the per-namespace authorizationRules
// call MUST NOT drop the namespace row. The snapshot still surfaces
// with universal columns; HasPropagationConfig defaults to true
// (no-false-positives stance) and PropagationNotes carries the
// list-error informational note. The per-namespace failure also
// records a partial under "servicebus" so the operator-visible
// PartialReason carries the call-failure context.
func TestServiceBusScanner_AuthorizationRulesAPIError_NonFatal(t *testing.T) {
	fake := newFakeAzureServiceBusWithAuth()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-degraded", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-degraded"] = true
	fake.AuthRulesStatus = http.StatusInternalServerError

	s, _ := newServiceBusScannerWithAuthFake(t, fake)
	result := &scanner.Result{AccountID: testSubID}
	tok, err := s.acquireAccessToken(context.Background())
	require.NoError(t, err)
	s.ScanServiceBus(context.Background(), tok, result)

	require.Len(t, result.EventSources, 1,
		"namespace row must still surface when the auth rules call fails")
	snap := result.EventSources[0]
	assert.True(t, snap.HasPropagationConfig,
		"auth rules API error must default propagation to preserved (no false positives)")
	require.Len(t, snap.PropagationNotes, 1)
	assert.Contains(t, strings.ToLower(snap.PropagationNotes[0]), "failed")
	assert.True(t, result.Partial,
		"per-namespace auth rules failure must mark the result partial")
	assert.Contains(t, result.FailedServices, ServiceIDServiceBus)
}

// TestServiceBusScanner_AuthorizationRulesPaginationFollowsNextLink —
// the per-namespace walker must follow nextLink across multiple
// pages. The fixture splits a single namespace's rules across two
// pages; the second page carries the load-bearing Send right. The
// detection must surface preserved=true because the walker iterated
// the second page.
func TestServiceBusScanner_AuthorizationRulesPaginationFollowsNextLink(t *testing.T) {
	fake := newFakeAzureServiceBusWithAuth()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-paged", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-paged"] = true

	s, srv := newServiceBusScannerWithAuthFake(t, fake)
	// Two pages: first carries Listen-only rules, second carries the
	// Listen+Send rule that satisfies the propagation axis. The
	// scanner must iterate both pages to discover the Send right.
	fake.AuthRulesPagesByNS["sb-paged"] = []ServiceBusAuthorizationRulesResponse{
		{
			Value: []ServiceBusAuthorizationRule{
				makeAuthRule("consumer-only", ServiceBusRightListen),
			},
			NextLink: fmt.Sprintf(
				"%s/subscriptions/%s/resourceGroups/rg-sb-paged/providers/Microsoft.ServiceBus/namespaces/sb-paged/authorizationRules?api-version=%s&page=2",
				srv.URL, testSubID, ServiceBusAuthorizationRulesAPIVersion,
			),
		},
		{
			Value: []ServiceBusAuthorizationRule{
				makeAuthRule("publisher", ServiceBusRightListen, ServiceBusRightSend),
			},
		},
	}

	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	assert.True(t, snaps[0].HasPropagationConfig,
		"walker must iterate the second page to discover the Send right")
	assert.Equal(t, 2, fake.AuthRulesCallsByNS["sb-paged"],
		"expected two auth-rules calls (one per page)")
}

// TestServiceBusScanner_AuthorizationRules404IsRBACOnlyNamespace — a
// namespace whose authorizationRules call returns 404 (or whose Value
// array is empty) maps to the canonical "RBAC-only namespace" shape
// per the design doc §3.3 simplification. HasPropagationConfig
// defaults to true with the informational "RBAC-only" note; no
// partial failure is recorded.
func TestServiceBusScanner_AuthorizationRules404IsRBACOnlyNamespace(t *testing.T) {
	fake := newFakeAzureServiceBusWithAuth()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-rbac", "eastus", "Premium"),
	}
	fake.DiagSettings404ForNS["sb-rbac"] = true
	fake.AuthRules404ForNS["sb-rbac"] = true

	s, _ := newServiceBusScannerWithAuthFake(t, fake)
	result := &scanner.Result{AccountID: testSubID}
	tok, err := s.acquireAccessToken(context.Background())
	require.NoError(t, err)
	s.ScanServiceBus(context.Background(), tok, result)

	require.Len(t, result.EventSources, 1)
	snap := result.EventSources[0]
	assert.True(t, snap.HasPropagationConfig,
		"404 on auth rules is the canonical RBAC-only namespace shape — preserved")
	require.Len(t, snap.PropagationNotes, 1,
		"the RBAC-only namespace carries one informational note")
	assert.Contains(t, strings.ToLower(snap.PropagationNotes[0]), "rbac")
	assert.False(t, result.Partial,
		"404 on auth rules is canonical, not a partial failure")
	assert.Empty(t, result.FailedServices)
}

// TestServiceBusScanner_AuthorizationRulesEmptyValueArray_RBACOnly — a
// namespace whose authorizationRules list returns an empty Value
// array (the more common ARM-API shape for RBAC-only namespaces in
// practice) is treated the same as a 404: preserved with the
// RBAC-only informational note, no partial failure.
func TestServiceBusScanner_AuthorizationRulesEmptyValueArray_RBACOnly(t *testing.T) {
	fake := newFakeAzureServiceBusWithAuth()
	fake.Namespaces = []armServiceBusNamespace{
		makeServiceBusNamespace("sb-empty-rules", "eastus", "Standard"),
	}
	fake.DiagSettings404ForNS["sb-empty-rules"] = true
	// AuthRulesByNS not seeded — handler default returns empty Value.

	s, _ := newServiceBusScannerWithAuthFake(t, fake)
	snaps, err := s.ScanEventSources(context.Background(), scanner.ScanScope{})
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasPropagationConfig,
		"empty Value array is the canonical RBAC-only namespace shape — preserved")
	require.Len(t, snap.PropagationNotes, 1)
	assert.Contains(t, strings.ToLower(snap.PropagationNotes[0]), "rbac")
}
