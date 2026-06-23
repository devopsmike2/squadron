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
	"github.com/devopsmike2/squadron/internal/proposer/verdictsel"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// discoveryBridgeFixture wires the DiscoveryBridge with a real memory
// store + a real in-memory iacconnstore. The acceptance tests seed
// pr_merged + pr_closed_not_merged audit events on the store and a
// connection row on the iacconnstore, then call the bridge method and
// assert on the result.
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
// supplied scope tuple and timestamp.
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

// seedPRClosedNotMerged inserts a recommendation.pr_closed_not_merged
// audit row — v0.89.36 (#655 Stream 53) — at the supplied scope tuple
// and timestamp. Mirrors seedPRMerged exactly with closed_at/closed_by
// replacing merged_at/merged_by, matching the webhook handler's emit
// shape.
func (f *discoveryBridgeFixture) seedPRClosedNotMerged(t *testing.T, connID, accountID, region, kind, prURL string, closedAt time.Time) {
	t.Helper()
	ev := &types.AuditEvent{
		ID:         "audit-" + prURL,
		Timestamp:  closedAt,
		Actor:      "github_webhook",
		EventType:  "recommendation.pr_closed_not_merged",
		TargetType: "iac_recommendation",
		TargetID:   connID,
		Action:     "pr_closed_not_merged",
		Payload: map[string]any{
			"repo_full_name":      "octo/widgets",
			"pr_number":           99,
			"pr_url":              prURL,
			"branch":              "squadron/rec/" + kind + "/" + accountID + "/" + region + "/closed-0",
			"closed_at":           closedAt.UTC().Format(time.RFC3339),
			"closed_by":           "bob",
			"recommendation_kind": kind,
			"connection_id":       connID,
			"account_id":          accountID,
			"region":              region,
		},
	}
	if err := f.store.CreateAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("seed pr_closed_not_merged: %v", err)
	}
}

// TestDiscoveryProposerLearning_ColdStartParity — §12 acceptance test 2.
// With zero pr_merged + zero pr_closed_not_merged events on the
// connection, the bridge returns four zero values and the prompt is
// byte-for-byte identical to a context built without any verdict
// fields set.
func TestDiscoveryProposerLearning_ColdStartParity(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 0 || len(rejected) != 0 || len(urls) != 0 {
		t.Fatalf("cold-start expected all empty; got approved=%d rejected=%d urls=%d",
			len(approved), len(rejected), len(urls))
	}
	// Byte-identity assertion: build the user message twice — once
	// with VerdictBlock unset, once with it explicitly set to "" —
	// they must be identical.
	base := ai.DiscoveryScanContext{
		ScanID:    "scan-abc",
		AccountID: f.accountID,
		Regions:   []string{f.region},
	}
	withField := base
	withField.VerdictBlock = ""
	withField.AcceptedRecommendations = nil
	if got, want := ai.BuildDiscoveryUserMessageForTest(withField), ai.BuildDiscoveryUserMessageForTest(base); got != want {
		t.Fatalf("cold-start prompt differs:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestDiscoveryProposerLearning_AcceptedExampleSurfaces — §12 acceptance
// test 4-ish. One pr_merged event in scope, 5 days old, surfaces as
// one approved verdict whose kind matches; the rendered prompt block
// contains "[ACCEPTED] kind=..." and the URL appears in the URL list.
func TestDiscoveryProposerLearning_AcceptedExampleSurfaces(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	mergedAt := time.Now().UTC().Add(-5 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/142", mergedAt)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("expected 1 approved verdict, got %d", len(approved))
	}
	if len(rejected) != 0 {
		t.Errorf("expected 0 rejected verdicts, got %d", len(rejected))
	}
	if approved[0].Kind != "rds-pi-em" {
		t.Errorf("kind = %q, want rds-pi-em", approved[0].Kind)
	}
	if approved[0].ID != "https://github.com/octo/widgets/pull/142" {
		t.Errorf("verdict ID = %q", approved[0].ID)
	}
	if approved[0].State != verdictsel.StateMerged {
		t.Errorf("state = %q, want %q", approved[0].State, verdictsel.StateMerged)
	}
	if len(urls) != 1 || urls[0] != "https://github.com/octo/widgets/pull/142" {
		t.Errorf("urls = %v", urls)
	}

	block := f.bridge.RenderDiscoveryVerdictBlock(approved, rejected)
	if !strings.Contains(block, "[ACCEPTED] kind=rds-pi-em") {
		t.Errorf("rendered block missing accepted line:\n%s", block)
	}

	// Render the prompt with the block threaded through VerdictBlock
	// and assert that line shows up downstream too.
	ctx := ai.DiscoveryScanContext{
		ScanID:       "scan-abc",
		AccountID:    f.accountID,
		Regions:      []string{f.region},
		VerdictBlock: block,
	}
	prompt := ai.BuildDiscoveryUserMessageForTest(ctx)
	if !strings.Contains(prompt, "[ACCEPTED] kind=rds-pi-em") {
		t.Errorf("prompt missing accepted line:\n%s", prompt)
	}
}

// TestDiscoveryProposerLearning_ScopeFilter — connection_id +
// account_id + region tuple narrows the bridge to exactly the
// matching audit row. The four cross-scope rows are seeded to verify
// they do NOT leak into the result. Preserved from v0.89.28.
func TestDiscoveryProposerLearning_ScopeFilter(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	// Seed a second connection so the cross-connection row is in-
	// scope of the iacconnstore lookup but out-of-scope of the bridge
	// call's scope tuple.
	otherConn := &iacconnstore.IaCConnection{
		Provider: iacconnstore.ProviderGitHub, AuthKind: iacconnstore.AuthKindPAT,
		RepoFullName: "octo/other", DefaultBranch: "main",
		RepoLayout: iacconnstore.RepoLayoutMono, CredCiphertext: []byte("opaque"),
	}
	if err := f.conns.Create(context.Background(), otherConn); err != nil {
		t.Fatalf("seed other connection: %v", err)
	}
	otherConnID := otherConn.ConnectionID

	now := time.Now().UTC().Add(-3 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/1", now)
	f.seedPRMerged(t, f.connectionID, f.accountID, "us-west-2", "rds-pi-em",
		"https://github.com/octo/widgets/pull/2", now)
	f.seedPRMerged(t, f.connectionID, f.accountID, "eu-west-1", "rds-pi-em",
		"https://github.com/octo/widgets/pull/3", now)
	f.seedPRMerged(t, f.connectionID, "999999999999", f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/4", now)
	f.seedPRMerged(t, otherConnID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/5", now)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 1 || len(rejected) != 0 {
		t.Fatalf("expected exactly 1 approved + 0 rejected; got %d / %d",
			len(approved), len(rejected))
	}
	if urls[0] != "https://github.com/octo/widgets/pull/1" {
		t.Errorf("leaked the wrong row: %v", urls)
	}
}

// TestDiscoveryProposerLearning_OptOutFlagRespected — §12 acceptance
// test 8. With LearnFromAcceptedRecommendations=false on the
// connection, even matching pr_merged + pr_closed_not_merged events
// return four zero values. The opt-out short-circuits BEFORE the
// storage query.
func TestDiscoveryProposerLearning_OptOutFlagRespected(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, false)
	now := time.Now().UTC().Add(-3 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/1", now)
	f.seedPRClosedNotMerged(t, f.connectionID, f.accountID, f.region, "eks-observability-addon",
		"https://github.com/octo/widgets/pull/2", now.Add(-time.Hour))

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 0 || len(rejected) != 0 || len(urls) != 0 {
		t.Errorf("opt-out: expected all empty; got approved=%d rejected=%d urls=%d",
			len(approved), len(rejected), len(urls))
	}
	block := f.bridge.RenderDiscoveryVerdictBlock(approved, rejected)
	if block != "" {
		t.Errorf("opt-out: expected empty block, got:\n%s", block)
	}
}

// TestDiscoveryProposerLearning_RecencyWindow — §12 acceptance test 9.
// A pr_merged event 31 days old falls outside the 30d window. The
// bridge returns four zero values.
func TestDiscoveryProposerLearning_RecencyWindow(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/old", old)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 0 || len(rejected) != 0 || len(urls) != 0 {
		t.Errorf("31d-old row should be filtered; got approved=%d rejected=%d urls=%d",
			len(approved), len(rejected), len(urls))
	}
}

// TestAssembleDiscoveryVerdicts_ClosedNotMergedSurfacesAsRejected —
// v0.89.36 (#655 Stream 53) §12 acceptance test 5. One
// pr_closed_not_merged event in scope, 1 day old, surfaces as a
// single rejected-bucket verdict carrying StateClosedNotMerged.
func TestAssembleDiscoveryVerdicts_ClosedNotMergedSurfacesAsRejected(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	closedAt := time.Now().UTC().Add(-24 * time.Hour)
	f.seedPRClosedNotMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/145", closedAt)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 0 {
		t.Errorf("approved bucket should be empty; got %d", len(approved))
	}
	if len(rejected) != 1 {
		t.Fatalf("rejected bucket should have 1 verdict, got %d", len(rejected))
	}
	if rejected[0].State != verdictsel.StateClosedNotMerged {
		t.Errorf("state = %q, want %q", rejected[0].State, verdictsel.StateClosedNotMerged)
	}
	if rejected[0].Kind != "rds-pi-em" {
		t.Errorf("kind = %q, want rds-pi-em", rejected[0].Kind)
	}
	if rejected[0].ID != "https://github.com/octo/widgets/pull/145" {
		t.Errorf("verdict ID = %q", rejected[0].ID)
	}
	if len(urls) != 1 || urls[0] != "https://github.com/octo/widgets/pull/145" {
		t.Errorf("urls = %v", urls)
	}
}

// TestAssembleDiscoveryVerdicts_RenderedBlockContainsClosedNotMerged —
// v0.89.36 (#655 Stream 53) §12 acceptance test 5 (prompt-side
// pin). Verifies the [CLOSED_NOT_MERGED] line shape from §7.2 with
// `reference: pr_closed=<URL>` lands in the rendered block.
func TestAssembleDiscoveryVerdicts_RenderedBlockContainsClosedNotMerged(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	closedAt := time.Now().UTC().Add(-24 * time.Hour)
	f.seedPRClosedNotMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/145", closedAt)

	approved, rejected, _, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	block := f.bridge.RenderDiscoveryVerdictBlock(approved, rejected)
	if !strings.Contains(block, "[CLOSED_NOT_MERGED] kind=rds-pi-em") {
		t.Errorf("rendered block missing closed_not_merged line:\n%s", block)
	}
	if !strings.Contains(block, "pr_closed=https://github.com/octo/widgets/pull/145") {
		t.Errorf("rendered block missing pr_closed reference line:\n%s", block)
	}
	// Block must also flow through DiscoveryScanContext.VerdictBlock
	// to surface in the prompt body.
	scanCtx := ai.DiscoveryScanContext{
		ScanID:       "scan-xyz",
		AccountID:    f.accountID,
		Regions:      []string{f.region},
		VerdictBlock: block,
	}
	prompt := ai.BuildDiscoveryUserMessageForTest(scanCtx)
	if !strings.Contains(prompt, "[CLOSED_NOT_MERGED] kind=rds-pi-em") {
		t.Errorf("prompt missing closed_not_merged line:\n%s", prompt)
	}
}

// seedExcludedRecommendation upserts a row in the new
// iac_recommendation_verdicts table for the supplied scope tuple
// with exclude_from_learning=true.
func (f *discoveryBridgeFixture) seedExcludedRecommendation(
	t *testing.T,
	recID, connID, accountID, region, kind, resourceID string,
	excludedAt time.Time,
) {
	t.Helper()
	rec := types.ExcludedRecommendation{
		RecommendationID:   recID,
		ConnectionID:       connID,
		AccountID:          accountID,
		Region:             region,
		RecommendationKind: kind,
		ResourceID:         resourceID,
		ExcludedAt:         excludedAt,
		ExcludedBy:         "alice",
	}
	if _, err := f.store.SetRecommendationExclusion(context.Background(), rec, true); err != nil {
		t.Fatalf("seed excluded recommendation: %v", err)
	}
}

// TestAssembleDiscoveryVerdicts_OperatorExcludedSurfacesInPrompt —
// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) §12 acceptance test 6.
// One merged PR + one operator-excluded recommendation in scope —
// the merged row lands in the approved bucket and the excluded row
// lands in the rejected bucket; the rendered prompt contains both
// stanzas and the rejected lines appear first per
// verdictsel.Select's documented ordering.
func TestAssembleDiscoveryVerdicts_OperatorExcludedSurfacesInPrompt(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	mergedAt := time.Now().UTC().Add(-3 * 24 * time.Hour)
	excludedAt := time.Now().UTC().Add(-2 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/142", mergedAt)
	f.seedExcludedRecommendation(t,
		"rec_eks_addon_abc", f.connectionID, f.accountID, f.region,
		"eks-observability-addon", "", excludedAt)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("approved bucket should have 1 verdict, got %d", len(approved))
	}
	if approved[0].State != verdictsel.StateMerged {
		t.Errorf("approved[0].State = %q, want %q", approved[0].State, verdictsel.StateMerged)
	}
	if len(rejected) != 1 {
		t.Fatalf("rejected bucket should have 1 verdict, got %d", len(rejected))
	}
	if rejected[0].State != verdictsel.StateOperatorExcluded {
		t.Errorf("rejected[0].State = %q, want %q", rejected[0].State, verdictsel.StateOperatorExcluded)
	}
	if rejected[0].Kind != "eks-observability-addon" {
		t.Errorf("rejected[0].Kind = %q, want eks-observability-addon", rejected[0].Kind)
	}
	if rejected[0].ID != "rec_eks_addon_abc" {
		t.Errorf("rejected[0].ID = %q, want rec_eks_addon_abc", rejected[0].ID)
	}
	// urls list must contain BOTH identifiers; rejected first per
	// verdictsel.Select's documented ordering.
	if len(urls) != 2 {
		t.Fatalf("urls = %v, want 2 entries", urls)
	}
	if urls[0] != "rec_eks_addon_abc" {
		t.Errorf("urls[0] = %q, want rejected first", urls[0])
	}
	if urls[1] != "https://github.com/octo/widgets/pull/142" {
		t.Errorf("urls[1] = %q, want approved second", urls[1])
	}

	block := f.bridge.RenderDiscoveryVerdictBlock(approved, rejected)
	if !strings.Contains(block, "[OPERATOR_EXCLUDED] kind=eks-observability-addon") {
		t.Errorf("rendered block missing operator_excluded line:\n%s", block)
	}
	if !strings.Contains(block, "[ACCEPTED] kind=rds-pi-em") {
		t.Errorf("rendered block missing accepted line:\n%s", block)
	}
	// Verify the prompt body picks up the operator_excluded stanza.
	scanCtx := ai.DiscoveryScanContext{
		ScanID:       "scan-excluded",
		AccountID:    f.accountID,
		Regions:      []string{f.region},
		VerdictBlock: block,
	}
	prompt := ai.BuildDiscoveryUserMessageForTest(scanCtx)
	if !strings.Contains(prompt, "[OPERATOR_EXCLUDED] kind=eks-observability-addon") {
		t.Errorf("prompt missing operator_excluded line:\n%s", prompt)
	}
}

// TestAssembleDiscoveryVerdicts_ExcludedRecommendationRespectsScope —
// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4). Three excluded
// rows across (C, A, R), (C, A, R-different), (C, A-different, R).
// Assemble on (C, A, R). Assert exactly one surfaces.
func TestAssembleDiscoveryVerdicts_ExcludedRecommendationRespectsScope(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	now := time.Now().UTC()
	// In-scope row.
	f.seedExcludedRecommendation(t, "rec_in_scope",
		f.connectionID, f.accountID, f.region,
		"eks-observability-addon", "", now.Add(-time.Hour))
	// Different region.
	f.seedExcludedRecommendation(t, "rec_other_region",
		f.connectionID, f.accountID, "us-west-2",
		"eks-observability-addon", "", now.Add(-2*time.Hour))
	// Different account.
	f.seedExcludedRecommendation(t, "rec_other_account",
		f.connectionID, "999999999999", f.region,
		"eks-observability-addon", "", now.Add(-3*time.Hour))

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 0 {
		t.Errorf("approved should be empty; got %d", len(approved))
	}
	if len(rejected) != 1 {
		t.Fatalf("rejected should have exactly 1 verdict; got %d", len(rejected))
	}
	if rejected[0].ID != "rec_in_scope" {
		t.Errorf("rejected[0].ID = %q, want rec_in_scope (leaked the wrong row)", rejected[0].ID)
	}
	if len(urls) != 1 || urls[0] != "rec_in_scope" {
		t.Errorf("urls = %v, want [rec_in_scope]", urls)
	}
}

// TestAssembleDiscoveryVerdicts_ExcludedRecommendationOptOutShortCircuits
// — v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4). The
// LearnFromAcceptedRecommendations=false opt-out short-circuits
// BEFORE the storage queries for BOTH the audit-rows pool and the
// new exclusion table, so an excluded row in scope yields an empty
// pool.
func TestAssembleDiscoveryVerdicts_ExcludedRecommendationOptOutShortCircuits(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, false)
	f.seedExcludedRecommendation(t, "rec_in_scope",
		f.connectionID, f.accountID, f.region,
		"eks-observability-addon", "", time.Now().UTC().Add(-time.Hour))

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 0 || len(rejected) != 0 || len(urls) != 0 {
		t.Errorf("opt-out should empty the pool; got approved=%d rejected=%d urls=%d",
			len(approved), len(rejected), len(urls))
	}
}

// --- v0.89.48 (#671 Stream 69) GCP discovery slice 1 chunk 5 tests ---

// seedPRMergedGCP — chunk 5 — inserts a recommendation.pr_merged
// audit row with the GCP payload shape: project_id is populated,
// account_id is empty, provider="gcp". The branch encodes
// gce-otel-label per §9.1.
func (f *discoveryBridgeFixture) seedPRMergedGCP(t *testing.T, connID, projectID, region, kind, prURL string, mergedAt time.Time) {
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
			"pr_number":           99,
			"pr_url":              prURL,
			"branch":              "squadron/rec/" + kind + "/" + projectID + "/" + region + "/gcp-0",
			"merged_at":           mergedAt.UTC().Format(time.RFC3339),
			"merged_by":           "alice",
			"recommendation_kind": kind,
			"connection_id":       connID,
			"provider":            "gcp",
			"project_id":          projectID,
			"account_id":          "",
			"region":              region,
		},
	}
	if err := f.store.CreateAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("seed pr_merged (gcp): %v", err)
	}
}

// TestAssembleDiscoveryVerdicts_GCPScope_QueriesProjectIDField —
// chunk 5 acceptance. Seed a recommendation.pr_merged event with
// payload {project_id: "my-project", region: "us-central1"}; call
// AssembleDiscoveryVerdicts with the GCP project_id as the scope_id.
// Assert: 1 approved verdict surfaces. The bridge passes accountID
// through to ListDiscoveryVerdicts which OR-matches account_id OR
// project_id, so GCP payloads round-trip cleanly without changing
// the public signature.
func TestAssembleDiscoveryVerdicts_GCPScope_QueriesProjectIDField(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	projectID := "my-sandbox-project"
	region := "us-central1"
	mergedAt := time.Now().UTC().Add(-5 * 24 * time.Hour)
	f.seedPRMergedGCP(t, f.connectionID, projectID, region, "gce-otel-label",
		"https://github.com/octo/widgets/pull/200", mergedAt)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, projectID, region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("expected 1 approved verdict (GCP), got %d", len(approved))
	}
	if len(rejected) != 0 {
		t.Errorf("expected 0 rejected verdicts, got %d", len(rejected))
	}
	if approved[0].Kind != "gce-otel-label" {
		t.Errorf("kind = %q, want gce-otel-label", approved[0].Kind)
	}
	if approved[0].ID != "https://github.com/octo/widgets/pull/200" {
		t.Errorf("verdict ID = %q", approved[0].ID)
	}
	if approved[0].State != verdictsel.StateMerged {
		t.Errorf("state = %q, want %q", approved[0].State, verdictsel.StateMerged)
	}
	if len(urls) != 1 || urls[0] != "https://github.com/octo/widgets/pull/200" {
		t.Errorf("urls = %v", urls)
	}
}

// TestAssembleDiscoveryVerdicts_AWSScope_QueriesAccountIDField —
// chunk 5 acceptance: AWS callers (the v0.89.47 path) still round-
// trip through the OR-matched storage query. Backward compat — the
// existing AWS test pattern works without modification because the
// OR predicate matches account_id when project_id is empty.
func TestAssembleDiscoveryVerdicts_AWSScope_QueriesAccountIDField(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	mergedAt := time.Now().UTC().Add(-3 * 24 * time.Hour)
	f.seedPRMerged(t, f.connectionID, f.accountID, f.region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/300", mergedAt)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, f.accountID, f.region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("expected 1 approved verdict (AWS), got %d", len(approved))
	}
	if len(rejected) != 0 {
		t.Errorf("expected 0 rejected verdicts, got %d", len(rejected))
	}
	if approved[0].Kind != "rds-pi-em" {
		t.Errorf("kind = %q, want rds-pi-em", approved[0].Kind)
	}
	if len(urls) != 1 || urls[0] != "https://github.com/octo/widgets/pull/300" {
		t.Errorf("urls = %v", urls)
	}
}

// TestAssembleDiscoveryVerdicts_CrossProviderIsolation — chunk 5
// negative acceptance. Seed one AWS pr_merged + one GCP pr_merged
// under the SAME connection_id and region but DIFFERENT scope_ids.
// Call assemble with the GCP project_id — only the GCP row surfaces.
// Call again with the AWS account_id — only the AWS row surfaces.
// Confirms the OR-match isn't a "match everything" leakage path.
func TestAssembleDiscoveryVerdicts_CrossProviderIsolation(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	region := "us-central1"
	mergedAt := time.Now().UTC().Add(-2 * 24 * time.Hour)
	projectID := "my-project"
	awsAccount := "123456789012"
	f.seedPRMerged(t, f.connectionID, awsAccount, region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/aws", mergedAt)
	f.seedPRMergedGCP(t, f.connectionID, projectID, region, "gce-otel-label",
		"https://github.com/octo/widgets/pull/gcp", mergedAt)

	// Call with GCP scope.
	gcpApproved, _, gcpURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, projectID, region,
	)
	if err != nil {
		t.Fatalf("assemble gcp: %v", err)
	}
	if len(gcpApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on GCP scope, got %d", len(gcpApproved))
	}
	if gcpURLs[0] != "https://github.com/octo/widgets/pull/gcp" {
		t.Errorf("GCP scope leaked AWS row: %v", gcpURLs)
	}

	// Call with AWS scope.
	awsApproved, _, awsURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, awsAccount, region,
	)
	if err != nil {
		t.Fatalf("assemble aws: %v", err)
	}
	if len(awsApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on AWS scope, got %d", len(awsApproved))
	}
	if awsURLs[0] != "https://github.com/octo/widgets/pull/aws" {
		t.Errorf("AWS scope leaked GCP row: %v", awsURLs)
	}
}

// --- v0.89.53 (#678 Stream 76) Azure discovery slice 1 chunk 5 tests ---

// seedPRMergedAzure — chunk 5 — inserts a recommendation.pr_merged
// audit row with the Azure payload shape: subscription_id is
// populated, account_id + project_id are empty, provider="azure".
// The branch encodes vm-otel-tag per §10.
func (f *discoveryBridgeFixture) seedPRMergedAzure(t *testing.T, connID, subscriptionID, region, kind, prURL string, mergedAt time.Time) {
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
			"pr_number":           201,
			"pr_url":              prURL,
			"branch":              "squadron/rec/" + kind + "/" + subscriptionID + "/" + region + "/azure-0",
			"merged_at":           mergedAt.UTC().Format(time.RFC3339),
			"merged_by":           "alice",
			"recommendation_kind": kind,
			"connection_id":       connID,
			"provider":            "azure",
			"subscription_id":     subscriptionID,
			"account_id":          "",
			"project_id":          "",
			"region":              region,
		},
	}
	if err := f.store.CreateAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("seed pr_merged (azure): %v", err)
	}
}

// TestAssembleDiscoveryVerdicts_AzureScope_QueriesSubscriptionIDField
// — chunk 5 acceptance. Seed a recommendation.pr_merged event with
// payload {subscription_id: "abc", region: "eastus"}; call
// AssembleDiscoveryVerdicts with the Azure subscription_id as the
// scope_id. Assert: 1 approved verdict surfaces. The bridge passes
// scope_id through to ListDiscoveryVerdicts which OR-matches
// account_id OR project_id OR subscription_id, so Azure payloads
// round-trip cleanly without changing the public signature.
func TestAssembleDiscoveryVerdicts_AzureScope_QueriesSubscriptionIDField(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	subscriptionID := "abc"
	region := "eastus"
	mergedAt := time.Now().UTC().Add(-5 * 24 * time.Hour)
	f.seedPRMergedAzure(t, f.connectionID, subscriptionID, region, "vm-otel-tag",
		"https://github.com/octo/widgets/pull/210", mergedAt)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, subscriptionID, region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("expected 1 approved verdict (Azure), got %d", len(approved))
	}
	if len(rejected) != 0 {
		t.Errorf("expected 0 rejected verdicts, got %d", len(rejected))
	}
	if approved[0].Kind != "vm-otel-tag" {
		t.Errorf("kind = %q, want vm-otel-tag", approved[0].Kind)
	}
	if approved[0].ID != "https://github.com/octo/widgets/pull/210" {
		t.Errorf("verdict ID = %q", approved[0].ID)
	}
	if approved[0].State != verdictsel.StateMerged {
		t.Errorf("state = %q, want %q", approved[0].State, verdictsel.StateMerged)
	}
	if len(urls) != 1 || urls[0] != "https://github.com/octo/widgets/pull/210" {
		t.Errorf("urls = %v", urls)
	}
}

// TestAssembleDiscoveryVerdicts_TriProviderIsolation — chunk 5
// negative acceptance. Seed one AWS + one GCP + one Azure pr_merged
// under the SAME connection_id and region but DIFFERENT scope_ids.
// Call assemble with each provider's scope_id — only the matching
// provider's row surfaces. Confirms the three-way OR-match in
// ListDiscoveryVerdicts isn't a "match everything" leakage path.
func TestAssembleDiscoveryVerdicts_TriProviderIsolation(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	// Use a region all three accept literally to focus the test on
	// the scope_id discrimination. Mix-and-match region values would
	// pass via the region predicate; we want failures here to be
	// scope_id-driven only.
	region := "common-region"
	mergedAt := time.Now().UTC().Add(-2 * 24 * time.Hour)
	awsAccount := "123456789012"
	projectID := "my-project"
	subscriptionID := "my-subscription"
	f.seedPRMerged(t, f.connectionID, awsAccount, region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/aws", mergedAt)
	f.seedPRMergedGCP(t, f.connectionID, projectID, region, "gce-otel-label",
		"https://github.com/octo/widgets/pull/gcp", mergedAt)
	f.seedPRMergedAzure(t, f.connectionID, subscriptionID, region, "vm-otel-tag",
		"https://github.com/octo/widgets/pull/azure", mergedAt)

	// Call with Azure scope.
	azureApproved, _, azureURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, subscriptionID, region,
	)
	if err != nil {
		t.Fatalf("assemble azure: %v", err)
	}
	if len(azureApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on Azure scope, got %d", len(azureApproved))
	}
	if azureURLs[0] != "https://github.com/octo/widgets/pull/azure" {
		t.Errorf("Azure scope leaked another provider's row: %v", azureURLs)
	}
	if azureApproved[0].Kind != "vm-otel-tag" {
		t.Errorf("Azure scope verdict kind = %q, want vm-otel-tag", azureApproved[0].Kind)
	}

	// Call with GCP scope.
	gcpApproved, _, gcpURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, projectID, region,
	)
	if err != nil {
		t.Fatalf("assemble gcp: %v", err)
	}
	if len(gcpApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on GCP scope, got %d", len(gcpApproved))
	}
	if gcpURLs[0] != "https://github.com/octo/widgets/pull/gcp" {
		t.Errorf("GCP scope leaked another provider's row: %v", gcpURLs)
	}

	// Call with AWS scope.
	awsApproved, _, awsURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, awsAccount, region,
	)
	if err != nil {
		t.Fatalf("assemble aws: %v", err)
	}
	if len(awsApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on AWS scope, got %d", len(awsApproved))
	}
	if awsURLs[0] != "https://github.com/octo/widgets/pull/aws" {
		t.Errorf("AWS scope leaked another provider's row: %v", awsURLs)
	}
}

// --- v0.89.58 (#685 Stream 83) OCI discovery slice 1 chunk 5 tests ---

// seedPRMergedOCI — chunk 5 — inserts a recommendation.pr_merged
// audit row with the OCI payload shape: tenancy_ocid is populated;
// account_id, project_id, and subscription_id are empty;
// provider="oci". The branch encodes compute-otel-tag per §10.
func (f *discoveryBridgeFixture) seedPRMergedOCI(t *testing.T, connID, tenancyOCID, region, kind, prURL string, mergedAt time.Time) {
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
			"pr_number":           301,
			"pr_url":              prURL,
			"branch":              "squadron/rec/" + kind + "/" + tenancyOCID + "/" + region + "/oci-0",
			"merged_at":           mergedAt.UTC().Format(time.RFC3339),
			"merged_by":           "alice",
			"recommendation_kind": kind,
			"connection_id":       connID,
			"provider":            "oci",
			"tenancy_ocid":        tenancyOCID,
			"account_id":          "",
			"project_id":          "",
			"subscription_id":     "",
			"region":              region,
		},
	}
	if err := f.store.CreateAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("seed pr_merged (oci): %v", err)
	}
}

// TestAssembleDiscoveryVerdicts_OCIScope_QueriesTenancyOCIDField —
// chunk 5 acceptance. Seed a recommendation.pr_merged event with
// payload {tenancy_ocid: "ocid1.tenancy.oc1..xyz", region:
// "us-phoenix-1"}; call AssembleDiscoveryVerdicts with the OCI
// tenancy_ocid as the scope_id. Assert: 1 approved verdict surfaces.
// The bridge passes scope_id through to ListDiscoveryVerdicts which
// OR-matches account_id OR project_id OR subscription_id OR
// tenancy_ocid, so OCI payloads round-trip cleanly without changing
// the public signature.
func TestAssembleDiscoveryVerdicts_OCIScope_QueriesTenancyOCIDField(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	tenancyOCID := "ocid1.tenancy.oc1..aaaaaaaa"
	region := "us-phoenix-1"
	mergedAt := time.Now().UTC().Add(-5 * 24 * time.Hour)
	f.seedPRMergedOCI(t, f.connectionID, tenancyOCID, region, "compute-otel-tag",
		"https://github.com/octo/widgets/pull/310", mergedAt)

	approved, rejected, urls, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, tenancyOCID, region,
	)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("expected 1 approved verdict (OCI), got %d", len(approved))
	}
	if len(rejected) != 0 {
		t.Errorf("expected 0 rejected verdicts, got %d", len(rejected))
	}
	if approved[0].Kind != "compute-otel-tag" {
		t.Errorf("kind = %q, want compute-otel-tag", approved[0].Kind)
	}
	if approved[0].ID != "https://github.com/octo/widgets/pull/310" {
		t.Errorf("verdict ID = %q", approved[0].ID)
	}
	if approved[0].State != verdictsel.StateMerged {
		t.Errorf("state = %q, want %q", approved[0].State, verdictsel.StateMerged)
	}
	if len(urls) != 1 || urls[0] != "https://github.com/octo/widgets/pull/310" {
		t.Errorf("urls = %v", urls)
	}
}

// TestAssembleDiscoveryVerdicts_FourProviderIsolation — chunk 5
// negative acceptance. Seed one AWS + one GCP + one Azure + one OCI
// pr_merged under the SAME connection_id and region but DIFFERENT
// scope_ids. Call assemble with each provider's scope_id — only the
// matching provider's row surfaces. Confirms the four-way OR-match in
// ListDiscoveryVerdicts isn't a "match everything" leakage path.
func TestAssembleDiscoveryVerdicts_FourProviderIsolation(t *testing.T) {
	f := newDiscoveryBridgeFixture(t, true)
	region := "common-region"
	mergedAt := time.Now().UTC().Add(-2 * 24 * time.Hour)
	awsAccount := "123456789012"
	projectID := "my-project"
	subscriptionID := "my-subscription"
	tenancyOCID := "ocid1.tenancy.oc1..my-tenancy"
	f.seedPRMerged(t, f.connectionID, awsAccount, region, "rds-pi-em",
		"https://github.com/octo/widgets/pull/aws", mergedAt)
	f.seedPRMergedGCP(t, f.connectionID, projectID, region, "gce-otel-label",
		"https://github.com/octo/widgets/pull/gcp", mergedAt)
	f.seedPRMergedAzure(t, f.connectionID, subscriptionID, region, "vm-otel-tag",
		"https://github.com/octo/widgets/pull/azure", mergedAt)
	f.seedPRMergedOCI(t, f.connectionID, tenancyOCID, region, "compute-otel-tag",
		"https://github.com/octo/widgets/pull/oci", mergedAt)

	// Call with OCI scope.
	ociApproved, _, ociURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, tenancyOCID, region,
	)
	if err != nil {
		t.Fatalf("assemble oci: %v", err)
	}
	if len(ociApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on OCI scope, got %d", len(ociApproved))
	}
	if ociURLs[0] != "https://github.com/octo/widgets/pull/oci" {
		t.Errorf("OCI scope leaked another provider's row: %v", ociURLs)
	}
	if ociApproved[0].Kind != "compute-otel-tag" {
		t.Errorf("OCI scope verdict kind = %q, want compute-otel-tag", ociApproved[0].Kind)
	}

	// Call with Azure scope.
	azureApproved, _, azureURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, subscriptionID, region,
	)
	if err != nil {
		t.Fatalf("assemble azure: %v", err)
	}
	if len(azureApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on Azure scope, got %d", len(azureApproved))
	}
	if azureURLs[0] != "https://github.com/octo/widgets/pull/azure" {
		t.Errorf("Azure scope leaked another provider's row: %v", azureURLs)
	}

	// Call with GCP scope.
	gcpApproved, _, gcpURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, projectID, region,
	)
	if err != nil {
		t.Fatalf("assemble gcp: %v", err)
	}
	if len(gcpApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on GCP scope, got %d", len(gcpApproved))
	}
	if gcpURLs[0] != "https://github.com/octo/widgets/pull/gcp" {
		t.Errorf("GCP scope leaked another provider's row: %v", gcpURLs)
	}

	// Call with AWS scope.
	awsApproved, _, awsURLs, err := f.bridge.AssembleDiscoveryVerdicts(
		context.Background(), f.connectionID, awsAccount, region,
	)
	if err != nil {
		t.Fatalf("assemble aws: %v", err)
	}
	if len(awsApproved) != 1 {
		t.Fatalf("expected 1 approved verdict on AWS scope, got %d", len(awsApproved))
	}
	if awsURLs[0] != "https://github.com/octo/widgets/pull/aws" {
		t.Errorf("AWS scope leaked another provider's row: %v", awsURLs)
	}
}

