// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestAdapterCrossCloudCitation_WiredPath exercises the production verdict
// assembly path — discoveryAcceptedAssemblerAdapter.AssembleVerdictBlockWithByState,
// the same entry the recommendations handler calls — rather than the bridge in
// isolation.
//
// The realistic shape it pins: ONE IaC GitHub repo serves PRs for every cloud,
// so an AWS decline and a later GCP scan share the same connection_id. v0.89.248
// excluded cross-scope verdicts by connection_id, which silently dropped this
// case (the feature was inert with a single connected repo). The fix keys the
// exclusion on SCOPE, so the AWS decline (account 123456789012) surfaces,
// origin-labeled, when assembling for the GCP project scope.
//
// It also pins the opt-in gate: with cross-cloud citations OFF (the default),
// per-provider isolation holds and the GCP scope's block is empty.
func TestAdapterCrossCloudCitation_WiredPath(t *testing.T) {
	store := memory.NewStore()
	conns := iacconnstore.NewMemoryStore()

	// A single IaC GitHub connection — serves PRs for all clouds.
	conn := &iacconnstore.IaCConnection{
		Provider:                         iacconnstore.ProviderGitHub,
		AuthKind:                         iacconnstore.AuthKindPAT,
		RepoFullName:                     "octo/infra",
		DefaultBranch:                    "main",
		RepoLayout:                       iacconnstore.RepoLayoutMono,
		CredCiphertext:                   []byte("opaque"),
		LearnFromAcceptedRecommendations: true,
	}
	if err := conns.Create(context.Background(), conn); err != nil {
		t.Fatalf("seed iac connection: %v", err)
	}

	// An AWS decline recorded against THAT connection (account scope).
	awsAccount := "123456789012"
	prURL := "https://github.com/octo/infra/pull/aws-decline"
	closedAt := time.Now().UTC().Add(-2 * 24 * time.Hour)
	ev := &types.AuditEvent{
		ID:         "audit-" + prURL,
		Timestamp:  closedAt,
		Actor:      "github_webhook",
		EventType:  "recommendation.pr_closed_not_merged",
		TargetType: "iac_recommendation",
		TargetID:   conn.ConnectionID,
		Action:     "pr_closed_not_merged",
		Payload: map[string]any{
			"repo_full_name":      "octo/infra",
			"pr_number":           99,
			"pr_url":              prURL,
			"branch":              "squadron/rec/metrics-volume-drop/" + awsAccount + "/us-east-1/closed-0",
			"closed_at":           closedAt.Format(time.RFC3339),
			"closed_by":           "bob",
			"recommendation_kind": "metrics-volume-drop",
			"connection_id":       conn.ConnectionID,
			"account_id":          awsAccount,
			"region":              "us-east-1",
		},
	}
	if err := store.CreateAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("seed aws decline: %v", err)
	}

	gcpProject := "squadron-demo-project"
	gcpRegion := "europe-west1"

	// --- Default (cross-cloud OFF): per-provider isolation holds. ---
	off := &discoveryAcceptedAssemblerAdapter{appStore: store, connections: conns}
	block, urls, _, err := off.AssembleVerdictBlockWithByState(context.Background(), gcpProject, gcpRegion)
	if err != nil {
		t.Fatalf("assemble (off): %v", err)
	}
	if block != "" || len(urls) != 0 {
		t.Fatalf("isolation broken with flag off: block=%q urls=%v", block, urls)
	}

	// --- Cross-cloud ON: the AWS decline surfaces on the GCP scope. ---
	on := &discoveryAcceptedAssemblerAdapter{appStore: store, connections: conns, crossCloudCitations: true}
	block, urls, byState, err := on.AssembleVerdictBlockWithByState(context.Background(), gcpProject, gcpRegion)
	if err != nil {
		t.Fatalf("assemble (on): %v", err)
	}
	if block == "" {
		t.Fatal("expected a non-empty verdict block with cross-cloud citations on")
	}
	if !strings.Contains(block, "[seen on aws / "+awsAccount+"]") {
		t.Errorf("rendered block missing origin label; block=%q", block)
	}
	if !strings.Contains(block, "metrics-volume-drop") {
		t.Errorf("rendered block missing the cross-cloud pattern kind; block=%q", block)
	}
	foundURL := false
	for _, u := range urls {
		if u == prURL {
			foundURL = true
		}
	}
	if !foundURL {
		t.Errorf("cross-cloud citation URL not in verdict_examples; urls=%v", urls)
	}
	if got := byState["closed_not_merged"]; len(got) == 0 || got[0] != prURL {
		t.Errorf("closed_not_merged by-state bucket = %v, want [%s]", got, prURL)
	}
}
