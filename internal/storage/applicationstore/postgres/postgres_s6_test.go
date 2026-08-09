// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// postgres_s6_test.go — TEST_POSTGRES_DSN-gated integration tests for the ADR 0033
// slice 6 entity ports. Each test asserts the sqlite-mirrored semantics:
// create→get roundtrip, missing-get=(nil,nil), update persists, update/delete-
// missing per-entity behavior, list ordering/filters, plus the special behaviors
// (webhook dedupe idempotency, cost-spike open/closed, exclusion transitions,
// check-run cold-start, incident-draft dedup). They skip cleanly when the DSN is
// unset via the shared testStore helper.

func TestPostgres_APITokenCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	tok := &types.APIToken{
		ID:        "t1",
		Label:     "ci",
		Hash:      "hash-abc",
		Scopes:    []string{"agents:read", "rollouts:write"},
		CreatedAt: now,
	}
	if err := s.CreateAPIToken(ctx, tok); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetAPITokenByHash(ctx, "hash-abc")
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Label != "ci" || len(got.Scopes) != 2 || got.Scopes[0] != "agents:read" || got.TenantID != "default" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Missing lookup must be (nil, nil).
	if miss, err := s.GetAPITokenByHash(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil): %v %v", miss, err)
	}

	// Empty-scopes token round-trips to nil scopes (legacy full-access sentinel).
	full := &types.APIToken{ID: "t2", Label: "legacy", Hash: "hash-legacy", CreatedAt: now}
	if err := s.CreateAPIToken(ctx, full); err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	if gf, _ := s.GetAPITokenByHash(ctx, "hash-legacy"); gf == nil || gf.Scopes != nil {
		t.Fatalf("empty scopes should decode to nil: %+v", gf)
	}

	used := now.Add(time.Hour)
	if err := s.UpdateAPITokenLastUsed(ctx, "t1", used); err != nil {
		t.Fatalf("last used: %v", err)
	}
	// Best-effort: updating a missing token is not an error.
	if err := s.UpdateAPITokenLastUsed(ctx, "missing", used); err != nil {
		t.Fatalf("last used missing should be no-op: %v", err)
	}
	if g2, _ := s.GetAPITokenByHash(ctx, "hash-abc"); g2 == nil || g2.LastUsedAt == nil {
		t.Fatalf("last_used_at did not persist: %+v", g2)
	}

	if err := s.RevokeAPIToken(ctx, "t1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Idempotent: revoking again / a missing token is not an error.
	if err := s.RevokeAPIToken(ctx, "t1", now.Add(3*time.Hour)); err != nil {
		t.Fatalf("re-revoke: %v", err)
	}
	if err := s.RevokeAPIToken(ctx, "missing", now); err != nil {
		t.Fatalf("revoke missing should be no-op: %v", err)
	}
	g3, _ := s.GetAPITokenByHash(ctx, "hash-abc")
	if g3 == nil || g3.RevokedAt == nil || !g3.RevokedAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("revoked_at should keep original stamp: %+v", g3)
	}

	list, err := s.ListAPITokens(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}
	// Newest-first by created_at (both share now; just assert both present).
}

func TestPostgres_RecommendationDismissals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if d, err := s.IsRecommendationDismissed(ctx, "r1"); err != nil || d {
		t.Fatalf("cold: %v %v", d, err)
	}
	if err := s.DismissRecommendation(ctx, &types.RecommendationDismissal{
		RecommendationID: "r1", DismissedAt: now, DismissedBy: "operator:a", Reason: "noise",
	}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if d, err := s.IsRecommendationDismissed(ctx, "r1"); err != nil || !d {
		t.Fatalf("should be dismissed: %v %v", d, err)
	}
	// Repeat dismissal refreshes rather than erroring.
	if err := s.DismissRecommendation(ctx, &types.RecommendationDismissal{
		RecommendationID: "r1", DismissedAt: now.Add(time.Hour), DismissedBy: "operator:b", Reason: "still noise",
	}); err != nil {
		t.Fatalf("re-dismiss: %v", err)
	}
	list, err := s.ListRecommendationDismissals(ctx)
	if err != nil || len(list) != 1 || list[0].DismissedBy != "operator:b" || list[0].Reason != "still noise" {
		t.Fatalf("list mismatch: %+v err=%v", list, err)
	}
	if err := s.RestoreRecommendation(ctx, "r1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Restore is idempotent.
	if err := s.RestoreRecommendation(ctx, "r1"); err != nil {
		t.Fatalf("restore idempotent: %v", err)
	}
	if d, _ := s.IsRecommendationDismissed(ctx, "r1"); d {
		t.Fatal("should be restored")
	}
}

func TestPostgres_RecommendationOutcomes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	o := &types.RecommendationOutcome{
		ID: "o1", RecommendationID: "r1", AppliedAt: now, AppliedBy: "operator:a",
		Title: "drop noisy attr", Category: "noisy_attribute", Signal: "logs",
		BaselineBytesPerHour: 1000, EstSavingsPerMonthUSDAtApply: 12.5, Status: "pending",
	}
	if err := s.CreateRecommendationOutcome(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	o2 := &types.RecommendationOutcome{
		ID: "o2", RecommendationID: "r2", AppliedAt: now.Add(time.Hour),
		Title: "b", Category: "c", Status: "pending",
	}
	if err := s.CreateRecommendationOutcome(ctx, o2); err != nil {
		t.Fatalf("create2: %v", err)
	}

	// Update mutates only the observation columns.
	o.LastObservedBytesPerHour = 400
	o.LastObservedAt = now.Add(2 * time.Hour)
	o.RealizedSavingsPerMonthUSD = 8.0
	o.Status = "realized"
	if err := s.UpdateRecommendationOutcome(ctx, o); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Update-missing is a no-op (mirrors sqlite).
	if err := s.UpdateRecommendationOutcome(ctx, &types.RecommendationOutcome{ID: "missing"}); err != nil {
		t.Fatalf("update missing should be no-op: %v", err)
	}

	list, err := s.ListRecommendationOutcomes(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}
	// Newest apply first.
	if list[0].ID != "o2" {
		t.Fatalf("expected o2 first, got %s", list[0].ID)
	}
	var got *types.RecommendationOutcome
	for _, r := range list {
		if r.ID == "o1" {
			got = r
		}
	}
	if got == nil || got.Status != "realized" || got.LastObservedBytesPerHour != 400 ||
		got.Title != "drop noisy attr" || got.BaselineBytesPerHour != 1000 {
		t.Fatalf("update roundtrip mismatch: %+v", got)
	}
}

func TestPostgres_CostSpikeEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	e := &types.CostSpikeEvent{
		ID: "cs1", StartedAt: now, Severity: "warn", Signal: "logs",
		BaselineMonthlyUSD: 100, PeakMonthlyUSD: 150, PeakPctAboveBaseline: 50,
		AttributionJSON: `{"top_agents":["a"]}`,
	}
	if err := s.CreateCostSpikeEvent(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	}
	if miss, err := s.GetCostSpikeEvent(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil): %v %v", miss, err)
	}

	// LatestOpenCostSpike returns the open row.
	open, err := s.LatestOpenCostSpike(ctx)
	if err != nil || open == nil || open.ID != "cs1" {
		t.Fatalf("latest open: err=%v got=%v", err, open)
	}

	// Escalate + acknowledge in place.
	e.Severity = "critical"
	e.PeakMonthlyUSD = 300
	e.PeakPctAboveBaseline = 200
	ackAt := now.Add(time.Hour)
	e.AcknowledgedAt = &ackAt
	e.AcknowledgedBy = "operator:a"
	if err := s.UpdateCostSpikeEvent(ctx, e); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := s.GetCostSpikeEvent(ctx, "cs1")
	if got == nil || got.Severity != "critical" || got.PeakMonthlyUSD != 300 ||
		got.AcknowledgedAt == nil || got.AcknowledgedBy != "operator:a" {
		t.Fatalf("update roundtrip mismatch: %+v", got)
	}

	// Close the spike.
	endedAt := now.Add(2 * time.Hour)
	e.EndedAt = &endedAt
	if err := s.UpdateCostSpikeEvent(ctx, e); err != nil {
		t.Fatalf("close: %v", err)
	}
	if open, _ := s.LatestOpenCostSpike(ctx); open != nil {
		t.Fatalf("no open spike expected, got %v", open)
	}

	// Filters.
	if openList, _ := s.ListCostSpikeEvents(ctx, types.CostSpikeFilter{Status: "open"}); len(openList) != 0 {
		t.Fatalf("open filter should be empty, got %d", len(openList))
	}
	if closedList, _ := s.ListCostSpikeEvents(ctx, types.CostSpikeFilter{Status: "closed"}); len(closedList) != 1 {
		t.Fatalf("closed filter should have 1")
	}
	if all, _ := s.ListCostSpikeEvents(ctx, types.CostSpikeFilter{}); len(all) != 1 {
		t.Fatalf("all filter should have 1")
	}
}

func TestPostgres_WebhookDedupe(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, ra1, err := s.RecordWebhookDelivery(ctx, "delivery-1", "push")
	if err != nil || !first {
		t.Fatalf("first delivery should be fresh: first=%v err=%v", first, err)
	}
	// Replay: firstTime=false, receivedAt = the ORIGINAL stamp.
	again, ra2, err := s.RecordWebhookDelivery(ctx, "delivery-1", "push")
	if err != nil || again {
		t.Fatalf("replay should not be fresh: again=%v err=%v", again, err)
	}
	if !ra1.Equal(ra2) {
		t.Fatalf("replay received_at should equal original: %v vs %v", ra1, ra2)
	}
	// Empty delivery id errors.
	if _, _, err := s.RecordWebhookDelivery(ctx, "", "push"); err == nil {
		t.Fatal("empty delivery_id should error")
	}
	// GC with a future cutoff deletes the row; re-running is a clean no-op.
	del, err := s.GCWebhookDeliveries(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil || del != 1 {
		t.Fatalf("gc: del=%d err=%v", del, err)
	}
	if del2, _ := s.GCWebhookDeliveries(ctx, time.Now().UTC().Add(time.Hour)); del2 != 0 {
		t.Fatalf("stale gc should delete 0, got %d", del2)
	}
}

func TestPostgres_RecommendationExclusions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	rec := types.ExcludedRecommendation{
		RecommendationID: "rec-1", ConnectionID: "conn", AccountID: "acct", Region: "us-east-1",
		RecommendationKind: "rds-pi-em", ExcludedBy: "operator:a",
	}

	// Insert with excluded=true: prevExcluded=false, row stamped.
	prev, err := s.SetRecommendationExclusion(ctx, rec, true)
	if err != nil || prev {
		t.Fatalf("first exclude: prev=%v err=%v", prev, err)
	}
	list, err := s.ListExcludedRecommendations(ctx, "conn", "acct", "us-east-1", 0)
	if err != nil || len(list) != 1 || list[0].RecommendationID != "rec-1" || list[0].ExcludedBy != "operator:a" {
		t.Fatalf("list excluded mismatch: %+v err=%v", list, err)
	}
	if list[0].ExcludedAt.IsZero() {
		t.Fatal("excluded_at should be stamped on transition to true")
	}

	// No-op toggle true→true: prevExcluded=true, still listed.
	prev, err = s.SetRecommendationExclusion(ctx, rec, true)
	if err != nil || !prev {
		t.Fatalf("noop exclude: prev=%v err=%v", prev, err)
	}

	// Transition true→false: prevExcluded=true, row drops out of the excluded list.
	prev, err = s.SetRecommendationExclusion(ctx, rec, false)
	if err != nil || !prev {
		t.Fatalf("unexclude: prev=%v err=%v", prev, err)
	}
	if l2, _ := s.ListExcludedRecommendations(ctx, "conn", "acct", "us-east-1", 0); len(l2) != 0 {
		t.Fatalf("unexcluded should drop from list, got %d", len(l2))
	}

	// Empty scope tuple returns nil.
	if l3, err := s.ListExcludedRecommendations(ctx, "", "", "", 0); err != nil || l3 != nil {
		t.Fatalf("empty scope should be nil: %v %v", l3, err)
	}
}

func TestPostgres_CheckRunState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Cold start: no row → exists=false.
	if _, _, _, exists, err := s.GetCheckRunForRecommendation(ctx, "rec-x"); err != nil || exists {
		t.Fatalf("cold start should be exists=false: exists=%v err=%v", exists, err)
	}

	rec := types.ExcludedRecommendation{
		RecommendationID: "rec-x", ConnectionID: "conn", AccountID: "acct", Region: "us-east-1",
		RecommendationKind: "eks-observability-addon",
	}
	// In-progress: status set, conclusion empty.
	ref := types.CheckRunRef{Owner: "o", Repo: "r", CheckID: 42, HeadSHA: "abc123"}
	if err := s.SetCheckRunForRecommendation(ctx, rec, ref, "in_progress", ""); err != nil {
		t.Fatalf("set check run: %v", err)
	}
	gotRef, status, concl, exists, err := s.GetCheckRunForRecommendation(ctx, "rec-x")
	if err != nil || !exists {
		t.Fatalf("get: exists=%v err=%v", exists, err)
	}
	if gotRef.CheckID != 42 || gotRef.Owner != "o" || gotRef.HeadSHA != "abc123" ||
		status != "in_progress" || concl != "" {
		t.Fatalf("check run roundtrip mismatch: ref=%+v status=%q concl=%q", gotRef, status, concl)
	}
	// The check-run insert did NOT set exclusion.
	if l, _ := s.ListExcludedRecommendations(ctx, "conn", "acct", "us-east-1", 0); len(l) != 0 {
		t.Fatalf("check run insert must not exclude, got %d", len(l))
	}

	// Complete the run (update path on the existing row).
	if err := s.SetCheckRunForRecommendation(ctx, rec, ref, "completed", "success"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_, status, concl, _, _ = s.GetCheckRunForRecommendation(ctx, "rec-x")
	if status != "completed" || concl != "success" {
		t.Fatalf("completed roundtrip mismatch: status=%q concl=%q", status, concl)
	}

	// A row created by the exclusion path (no check run) reports exists=true with a
	// zero-value ref.
	rec2 := types.ExcludedRecommendation{
		RecommendationID: "rec-y", ConnectionID: "conn", AccountID: "acct", Region: "us-east-1",
		RecommendationKind: "k", ExcludedBy: "operator:a",
	}
	if _, err := s.SetRecommendationExclusion(ctx, rec2, true); err != nil {
		t.Fatalf("exclude rec-y: %v", err)
	}
	ref2, st2, cc2, ex2, err := s.GetCheckRunForRecommendation(ctx, "rec-y")
	if err != nil || !ex2 || ref2.CheckID != 0 || st2 != "" || cc2 != "" {
		t.Fatalf("exclusion-only row should exist with zero check run: ref=%+v st=%q cc=%q ex=%v err=%v", ref2, st2, cc2, ex2, err)
	}
}

func TestPostgres_IncidentDrafts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	d := &types.IncidentDraft{
		ID: "d1", ActionRequestID: "ar1", RolloutID: "ro1",
		Title: "Postmortem", BodyMarkdown: "# incident", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateIncidentDraft(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Status != "draft" {
		t.Fatalf("status should default to draft, got %q", d.Status)
	}
	if miss, err := s.GetIncidentDraft(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil): %v %v", miss, err)
	}

	// Dedup path by action_request_id.
	byAR, err := s.GetIncidentDraftByActionRequestID(ctx, "ar1")
	if err != nil || byAR == nil || byAR.ID != "d1" {
		t.Fatalf("by action request: err=%v got=%v", err, byAR)
	}
	if empty, err := s.GetIncidentDraftByActionRequestID(ctx, ""); err != nil || empty != nil {
		t.Fatalf("empty action id should be (nil,nil): %v %v", empty, err)
	}

	// Update persists + publishing fields.
	d.Status = "published"
	d.Provider = "github"
	d.ExternalID = "42"
	d.ExternalURL = "https://gh/issues/42"
	if err := s.UpdateIncidentDraft(ctx, d); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := s.GetIncidentDraft(ctx, "d1")
	if got == nil || got.Status != "published" || got.Provider != "github" || got.ExternalURL != "https://gh/issues/42" {
		t.Fatalf("update roundtrip mismatch: %+v", got)
	}
	// Update-missing errors.
	if err := s.UpdateIncidentDraft(ctx, &types.IncidentDraft{ID: "missing"}); err == nil {
		t.Fatal("update missing should error")
	}

	// List filters.
	if l, _ := s.ListIncidentDrafts(ctx, types.IncidentDraftFilter{Status: "published"}); len(l) != 1 {
		t.Fatalf("status filter should have 1")
	}
	if l, _ := s.ListIncidentDrafts(ctx, types.IncidentDraftFilter{Status: "draft"}); len(l) != 0 {
		t.Fatalf("draft filter should be empty")
	}
	if l, _ := s.ListIncidentDrafts(ctx, types.IncidentDraftFilter{RolloutID: "ro1"}); len(l) != 1 {
		t.Fatalf("rollout filter should have 1")
	}
}

func TestPostgres_DiscoveryScans(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	rec := &types.ScanRecord{
		ScanID: "scan-1", Provider: "aws", ScopeID: "acct-1",
		Regions:   []string{"us-east-1", "us-west-2"},
		StartedAt: now, CompletedAt: now.Add(time.Minute),
		Partial: true, PartialReason: "throttled",
		Summary:    map[string]int{"ec2": 5, "rds": 2},
		ResultJSON: `{"big":"inventory"}`, CreatedAt: now,
	}
	if err := s.SaveDiscoveryScan(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Second, newer scan for ordering.
	rec2 := &types.ScanRecord{
		ScanID: "scan-2", Provider: "aws", ScopeID: "acct-1",
		Regions: []string{"eu-west-1"}, StartedAt: now.Add(time.Hour), CompletedAt: now.Add(time.Hour),
		Summary: map[string]int{"lambda": 1}, ResultJSON: `{"more":"data"}`, CreatedAt: now.Add(time.Hour),
	}
	if err := s.SaveDiscoveryScan(ctx, rec2); err != nil {
		t.Fatalf("save2: %v", err)
	}

	if miss, err := s.GetDiscoveryScan(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil): %v %v", miss, err)
	}

	// Get includes result_json.
	got, err := s.GetDiscoveryScan(ctx, "scan-1")
	if err != nil || got == nil || got.ResultJSON != `{"big":"inventory"}` ||
		!got.Partial || got.PartialReason != "throttled" ||
		len(got.Regions) != 2 || got.Summary["ec2"] != 5 {
		t.Fatalf("get roundtrip mismatch: %+v err=%v", got, err)
	}

	// List omits result_json and orders newest started_at first.
	list, err := s.ListDiscoveryScans(ctx, "aws", "acct-1", 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}
	if list[0].ScanID != "scan-2" {
		t.Fatalf("expected scan-2 first, got %s", list[0].ScanID)
	}
	if list[0].ResultJSON != "" {
		t.Fatalf("list should omit result_json, got %q", list[0].ResultJSON)
	}
	// Blank scope lists all for provider.
	if all, _ := s.ListDiscoveryScans(ctx, "aws", "", 0); len(all) != 2 {
		t.Fatalf("blank scope should list all 2, got %d", len(all))
	}

	// Upsert on scan_id.
	rec.PartialReason = "re-run"
	rec.Summary = map[string]int{"ec2": 9}
	if err := s.SaveDiscoveryScan(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if g2, _ := s.GetDiscoveryScan(ctx, "scan-1"); g2 == nil || g2.PartialReason != "re-run" || g2.Summary["ec2"] != 9 {
		t.Fatalf("upsert did not persist: %+v", g2)
	}
}

func TestPostgres_SiemDestinations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	d := &types.SiemDestination{
		ID: "s1", Name: "splunk", Type: "splunk_hec", URL: "https://hec",
		Secret: []byte{0x01, 0x02, 0x03}, Enabled: true,
		EventTypePrefixesJSON: `["config."]`, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateSiemDestination(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	if miss, err := s.GetSiemDestination(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil): %v %v", miss, err)
	}
	got, err := s.GetSiemDestination(ctx, "s1")
	if err != nil || got == nil || got.Type != "splunk_hec" || len(got.Secret) != 3 ||
		!got.Enabled || got.EventTypePrefixesJSON != `["config."]` {
		t.Fatalf("roundtrip mismatch: %+v err=%v", got, err)
	}

	// Update persists.
	d.Name = "splunk-2"
	d.Enabled = false
	if err := s.UpdateSiemDestination(ctx, d); err != nil {
		t.Fatalf("update: %v", err)
	}
	if g2, _ := s.GetSiemDestination(ctx, "s1"); g2 == nil || g2.Name != "splunk-2" || g2.Enabled {
		t.Fatalf("update roundtrip mismatch: %+v", g2)
	}
	// Update-missing errors.
	if err := s.UpdateSiemDestination(ctx, &types.SiemDestination{ID: "missing"}); err == nil {
		t.Fatal("update missing should error")
	}

	// Narrow status update (dispatcher path).
	sentAt := now.Add(time.Hour)
	errAt := now.Add(2 * time.Hour)
	if err := s.UpdateSiemDestinationStatus(ctx, "s1", &sentAt, "401 unauthorized", &errAt); err != nil {
		t.Fatalf("status update: %v", err)
	}
	g3, _ := s.GetSiemDestination(ctx, "s1")
	if g3 == nil || g3.LastEventSentAt == nil || g3.LastError != "401 unauthorized" || g3.LastErrorAt == nil {
		t.Fatalf("status did not persist: %+v", g3)
	}
	// Status update on a missing row is a no-op.
	if err := s.UpdateSiemDestinationStatus(ctx, "missing", &sentAt, "", nil); err != nil {
		t.Fatalf("status missing should be no-op: %v", err)
	}

	if list, _ := s.ListSiemDestinations(ctx); len(list) != 1 {
		t.Fatalf("list should have 1")
	}
	if err := s.DeleteSiemDestination(ctx, "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Delete-missing errors.
	if err := s.DeleteSiemDestination(ctx, "s1"); err == nil {
		t.Fatal("delete missing should error")
	}
}

func TestPostgres_TraceResources(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	// Empty batch is a no-op.
	if ev, err := s.UpsertTraceResources(ctx, nil); err != nil || ev != 0 {
		t.Fatalf("empty batch: ev=%d err=%v", ev, err)
	}

	r1 := traceindex.ResourceRow{
		ResourceKey: "k1", Provider: "aws", ScopeID: "acct-1", ResourceIDHint: "i-abc",
		ServiceName: "svc", FirstSeenAt: now, LastSeenAt: now,
		SpanCount24h: 10, RootSpanCount24h: 3, AttributesJSON: `{"host.id":"i-abc"}`,
		MatchConfidence: traceindex.MatchConfidenceStrong, UpdatedAt: now,
	}
	if ev, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{r1}); err != nil || ev != 0 {
		t.Fatalf("first upsert: ev=%d err=%v", ev, err)
	}
	got, err := s.GetTraceResource(ctx, "k1")
	if err != nil || got == nil || got.SpanCount24h != 10 || got.MatchConfidence != traceindex.MatchConfidenceStrong ||
		got.ResourceIDHint != "i-abc" || got.AttributesJSON != `{"host.id":"i-abc"}` {
		t.Fatalf("get roundtrip mismatch: %+v err=%v", got, err)
	}
	// Missing get is (nil, nil).
	if miss, err := s.GetTraceResource(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil): %v %v", miss, err)
	}

	// Re-observe: span counts accumulate, first_seen_at pinned, empty hint preserved.
	later := now.Add(time.Hour)
	r1b := r1
	r1b.SpanCount24h = 5
	r1b.RootSpanCount24h = 2
	r1b.ResourceIDHint = "" // empty must preserve the prior hint
	r1b.LastSeenAt = later
	r1b.UpdatedAt = later
	if _, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{r1b}); err != nil {
		t.Fatalf("re-observe: %v", err)
	}
	got, _ = s.GetTraceResource(ctx, "k1")
	if got.SpanCount24h != 15 || got.RootSpanCount24h != 5 || got.ResourceIDHint != "i-abc" ||
		!got.FirstSeenAt.Equal(now) || !got.LastSeenAt.Equal(later) {
		t.Fatalf("accumulation mismatch: %+v", got)
	}

	// Scope queries.
	if n, err := s.CountTraceResourcesByScope(ctx, "aws", "acct-1"); err != nil || n != 1 {
		t.Fatalf("count: n=%d err=%v", n, err)
	}
	list, err := s.ListTraceResourcesByScope(ctx, "aws", "acct-1", now.Add(-time.Minute), 0)
	if err != nil || len(list) != 1 || list[0].ResourceKey != "k1" {
		t.Fatalf("list by scope: err=%v list=%+v", err, list)
	}
	// since filter excludes rows older than the cutoff.
	if l2, _ := s.ListTraceResourcesByScope(ctx, "aws", "acct-1", later.Add(time.Minute), 0); len(l2) != 0 {
		t.Fatalf("since filter should exclude, got %d", len(l2))
	}
	// Empty provider short-circuits.
	if l3, _ := s.ListTraceResourcesByScope(ctx, "", "", now, 0); l3 != nil {
		t.Fatalf("empty provider should be nil, got %+v", l3)
	}
	if n, _ := s.CountTraceResourcesByScope(ctx, "", ""); n != 0 {
		t.Fatalf("empty provider count should be 0, got %d", n)
	}
}
