// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// discoveryBridgeFixture wires the DiscoveryBridge with a real memory
// store + a real in-memory iacconnstore. The acceptance tests seed
// pr_merged audit events on the store and a connection row on the
// iacconnstore, then call the bridge method and assert on the result.
type discoveryBridgeFixture struct {
	store        applicationstore.ApplicationStore
	conns        iacconnstore.Store
	connectionID string
	accountID    string
	region       string
	bridge       *DiscoveryBridge
}

func newDiscoveryBridgeFixture(t *testing.T, learnFlag bool) *discoveryBridgeFixture {
	t.Helper()
	store := memory.NewStore()
	conns := iacconnstore.NewMemoryStore()
	conn := &iacconnstore.IaCConnection{
		Provider:                         iacconnstore.ProviderGitHub,
		AuthKind:                         iacconnstore.AuthKindPAT,
		RepoFullName:                     "octo/widgets",
		DefaultBranch:                    "main",
		RepoLayout:                       iacconnstore.RepoLayoutMono,
		CredCiphertext:                   []byte("opaque"),
		LearnFromAcceptedRecommendations: learnFlag,
	}
	if err := conns.Create(context.Background(), conn); err != nil {
		t.Fatalf("seed iac connection: %v", err)
	}
	// memoryStore.Create auto-defaults the flag to true when callers
	// pass false; flip back if the test wants opt-out.
	if !learnFlag {
		if err := conns.UpdateLearnFromAcceptedRecommendations(context.Background(), conn.ConnectionID, false); err != nil {
			t.Fatalf("opt-out: %v", err)
		}
	}
	return &discoveryBridgeFixture{
		store:        store,
		conns:        conns,
		connectionID: conn.ConnectionID,
		accountID:    "123456789012",
		region:       "us-east-1",
		bridge:       NewDiscoveryBridge(store, conns),
	}
}

// seedPRMerged inserts a recommendation.pr_merged audit row with the
// supplied scope tuple and timestamp. Helper for the acceptance
// tests so the per-test setup stays focused on the scope assertion.
func (f *discoveryBridgeFixture) seedPRMerged(t *testing.T, connID, accountID, region, kind, prURL string, mergedAt time.Time) {
	t.Helper()
	ev := &types.AuditEvent{
		ID:         "audit-" + prURL,
		Timestamp:  mergedAt,
		Actor:      "github_webhook",
		EventType:  "recommendation.pr_merged",
		TargetType: "iac_recommendation",
		TargetID:   connID,
		Action:     "pr_merged",
		Payload: map[string]any{
			"repo_full_name":      "octo/widgets",
			"pr_number":           42,
			"pr_url":              prURL,
			"branch":              "squadron/rec/" + kind + "/" + accountID + "/" + region + "/abc1234-0",
			"merged_at":           mergedAt.UTC().Format(time.RFC3339),
			"merged_by":           "alice",
			"recommendation_kind": kind,
			"connection_id":       connID,
			"account_id":          accountID,
			"region":              region,
		},
	}
	if err := f.store.CreateAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("seed pr_merged: %v", err)
	}
}

// TestDiscoveryProposerLearning_ColdStartParity — §11.1. With zero
// pr_merged events on the connection, the bridge returns empty
// slices and the prompt is byte-for-byte identical to one built
// without the new field set.
func TestDiscoveryProposerLearning_ColdStartParity(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	exs, urls, err := f.bridge.AssembleAcceptedRecommendations(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(exs) != 0 || len(urls) != 0 {
		t.Fatalf("cold-start expected empty; got %d examples, %d urls", len(exs), len(urls))
	}
	// Byte-identity assertion: build the user message twice — once
	// with the AcceptedRecommendations field unset, once with it
	// explicitly set to nil. They must be identical.
	base := ai.DiscoveryScanContext{
		ScanID:    "scan-abc",
		AccountID: f.accountID,
		Regions:   []string{f.region},
	}
	withField := base
	withField.AcceptedRecommendations = nil
	if got, want := ai.BuildDiscoveryUserMessageForTest(withField), ai.BuildDiscoveryUserMessageForTest(base); got != want {
		t.Fatalf("cold-start prompt differs:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestDiscoveryProposerLearning_AcceptedExampleSurfaces — §11.2. One
// pr_merged event in scope, 5 days old, surfaces as one example
// whose kind matches; the prompt body contains "[ACCEPTED] kind=..."
// and the URL appears in the audit-side url list.
func TestDiscoveryProposerLearning_AcceptedExampleSurfaces(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	mergedAt := time.Now().UTC().Add(-5 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/142", mergedAt)

	exs, urls, err := f.bridge.AssembleAcceptedRecommendations(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(exs) != 1 {
		t.Fatalf("expected 1 example, got %d", len(exs))
	}
	if exs[0].RecommendationKind != "rds-pi-em" {
		t.Errorf("kind = %q, want rds-pi-em", exs[0].RecommendationKind)
	}
	if exs[0].PRURL != "https://github.com/octo/widgets/pull/142" {
		t.Errorf("pr_url = %q", exs[0].PRURL)
	}
	if len(urls) != 1 || urls[0] != "https://github.com/octo/widgets/pull/142" {
		t.Errorf("urls = %v", urls)
	}

	// Render the prompt with this example and assert the
	// "[ACCEPTED] kind=rds-pi-em" line is present.
	ctx := ai.DiscoveryScanContext{
		ScanID:                  "scan-abc",
		AccountID:               f.accountID,
		Regions:                 []string{f.region},
		AcceptedRecommendations: exs,
	}
	prompt := ai.BuildDiscoveryUserMessageForTest(ctx)
	if !strings.Contains(prompt, "[ACCEPTED] kind=rds-pi-em") {
		t.Errorf("prompt missing accepted line:\n%s", prompt)
	}
}

// TestDiscoveryProposerLearning_ScopeFilter — §11.3. With five
// pr_merged events distributed across (C,A,R), (C,A,R'),
// (C,A',R) plus (C',A,R), only the one matching the full scope
// tuple surfaces. The other four do not leak.
func TestDiscoveryProposerLearning_ScopeFilter(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	otherConnID := "other-conn-id"
	// Seed a second connection in the store so the cross-connection
	// row is in-scope of the iacconnstore lookup but out-of-scope
	// of the bridge call's scope tuple.
	otherConn := &iacconnstore.IaCConnection{
		Provider: iacconnstore.ProviderGitHub, AuthKind: iacconnstore.AuthKindPAT,
		RepoFullName: "octo/other", DefaultBranch: "main",
		RepoLayout: iacconnstore.RepoLayoutMono, CredCiphertext: []byte("opaque"),
	}
	if err := f.conns.Create(context.Background(), otherConn); err != nil {
		t.Fatalf("seed other connection: %v", err)
	}
	otherConnID = otherConn.ConnectionID

	now := time.Now().UTC().Add(-3 * 24 * time.Hour)
	// (C, A, R) — the only row that should match.
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/1", now)
	// (C, A, R') — different region.
	f.seedPRMerged(t, f.connectionID, f.accountID, "us-west-2", "rds-pi-em",
		"https://github.com/octo/widgets/pull/2", now)
	f.seedPRMerged(t, f.connectionID, f.accountID, "eu-west-1", "rds-pi-em",
		"https://github.com/octo/widgets/pull/3", now)
	// (C, A', R) — different account.
	f.seedPRMerged(t, f.connectionID, "999999999999", f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/4", now)
	// (C', A, R) — different connection.
	f.seedPRMerged(t, otherConnID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/5", now)

	exs, urls, err := f.bridge.AssembleAcceptedRecommendations(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(exs) != 1 {
		t.Fatalf("expected exactly 1 example (scope-matched), got %d", len(exs))
	}
	if urls[0] != "https://github.com/octo/widgets/pull/1" {
		t.Errorf("leaked the wrong row: %v", urls)
	}
}

// TestDiscoveryProposerLearning_OptOutFlagRespected — §11.4. With
// LearnFromAcceptedRecommendations=false on the connection, even 3
// matching pr_merged events return empty.
func TestDiscoveryProposerLearning_OptOutFlagRespected(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, false)
	now := time.Now().UTC().Add(-3 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/1", now)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "eks-observability-addon",
		"https://github.com/octo/widgets/pull/2", now.Add(-time.Hour))
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "lambda-otel-layer",
		"https://github.com/octo/widgets/pull/3", now.Add(-2*time.Hour))

	exs, urls, err := f.bridge.AssembleAcceptedRecommendations(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(exs) != 0 {
		t.Errorf("opt-out: expected empty slice, got %d examples", len(exs))
	}
	if urls != nil && len(urls) != 0 {
		t.Errorf("opt-out: expected empty URL slice, got %v", urls)
	}
	// Prompt must NOT contain the accepted block.
	ctx := ai.DiscoveryScanContext{
		ScanID: "scan-abc", AccountID: f.accountID, Regions: []string{f.region},
		AcceptedRecommendations: exs,
	}
	prompt := ai.BuildDiscoveryUserMessageForTest(ctx)
	if strings.Contains(prompt, "[ACCEPTED]") {
		t.Errorf("opt-out prompt contains accepted block:\n%s", prompt)
	}
}

// TestDiscoveryProposerLearning_RecencyWindow — §11.5. A pr_merged
// event 31 days old falls outside the 30d window. The bridge returns
// empty.
func TestDiscoveryProposerLearning_RecencyWindow(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/old", old)

	exs, urls, err := f.bridge.AssembleAcceptedRecommendations(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(exs) != 0 {
		t.Errorf("31d-old row should be filtered; got %d examples", len(exs))
	}
	if len(urls) != 0 {
		t.Errorf("31d-old row should be filtered; got urls %v", urls)
	}
}
