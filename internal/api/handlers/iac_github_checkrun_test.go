// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/proposer/checkrunprompt"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeChecksClient is the test-side ChecksAPI implementation. Per-test
// canned response + an error + a single recorded request so tests can
// assert the wire shape that landed on the wrapper.
type fakeChecksClient struct {
	mu      sync.Mutex
	calls   []iacgithub.CheckRunCreate
	pats    []string
	respRef iacgithub.CheckRunRef
	respErr error
}

func (f *fakeChecksClient) CreateCheckRun(_ context.Context, pat string, req iacgithub.CheckRunCreate) (iacgithub.CheckRunRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	f.pats = append(f.pats, pat)
	if f.respErr != nil {
		return iacgithub.CheckRunRef{}, f.respErr
	}
	if f.respRef.CheckID == 0 {
		return iacgithub.CheckRunRef{
			Owner:   req.Owner,
			Repo:    req.Repo,
			CheckID: 7777,
			HeadSHA: req.HeadSHA,
		}, nil
	}
	return f.respRef, nil
}

// fakeCheckRunStore is the test-side CheckRunStore implementation.
// Records every SetCheckRunForRecommendation call.
type fakeCheckRunStore struct {
	mu    sync.Mutex
	calls []fakeCheckRunStoreCall
	err   error
}

type fakeCheckRunStoreCall struct {
	Rec        types.ExcludedRecommendation
	Ref        types.CheckRunRef
	Status     string
	Conclusion string
}

func (s *fakeCheckRunStore) SetCheckRunForRecommendation(
	_ context.Context,
	rec types.ExcludedRecommendation,
	ref types.CheckRunRef,
	status, conclusion string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, fakeCheckRunStoreCall{Rec: rec, Ref: ref, Status: status, Conclusion: conclusion})
	return s.err
}

// newCheckRunTestHandler builds an IaCGitHubHandlers with the
// checks-API + check-run-store + audit recorder all wired. The four
// optional fields (checksClient, checkRunStore, squadronHost,
// auditService) are the chunk-2 wiring surface; tests pin them
// directly without going through the full server.go trampoline.
func newCheckRunTestHandler(checks ChecksAPI, store CheckRunStore, audit services.AuditService) *IaCGitHubHandlers {
	h := NewIaCGitHubHandlers(iacconnstore.NewMemoryStore(), zap.NewNop()).
		WithSquadronHost("https://squadron.acme.example")
	if checks != nil {
		h.WithChecksClient(checks)
	}
	if store != nil {
		h.WithCheckRunStore(store)
	}
	if audit != nil {
		h.WithAuditService(audit)
	}
	return h
}

// baselineCheckRunArgs builds a checkRunOpenedPRArgs with the §9.1
// happy-path scope tuple + a non-empty VerdictsByState bucket.
func baselineCheckRunArgs() checkRunOpenedPRArgs {
	return checkRunOpenedPRArgs{
		Connection: &iacconnstore.IaCConnection{
			ConnectionID: "conn-abc",
			RepoFullName: "octo/widgets",
		},
		Request: &iacGitHubOpenPRRequest{
			ResourceKind:      "rds-pi-em",
			ProposerReasoning: "Enable Performance Insights.",
			AccountID:         "111111111111",
			Region:            "us-east-1",
			RecommendationID:  "rec-xyz",
			VerdictExamplesUsedByState: map[string][]string{
				checkrunprompt.VerdictStateMerged: {"https://github.com/octo/widgets/pull/100"},
			},
		},
		PRURL:   "https://github.com/octo/widgets/pull/142",
		HeadSHA: "abc123",
		Owner:   "octo",
		Repo:    "widgets",
		PAT:     "pat-chk-write",
		VerdictExamplesUsedByState: map[string][]string{
			checkrunprompt.VerdictStateMerged: {"https://github.com/octo/widgets/pull/100"},
		},
	}
}

// TestEmitCheckRunForOpenedPR_HappyPath — chunk-2 happy path. The
// fake CreateCheckRun returns a non-zero ref; assert (a) the
// iac.check_run.created audit event fires with the expected
// payload shape and (b) SetCheckRunForRecommendation persisted the
// ref with status=in_progress.
func TestEmitCheckRunForOpenedPR_HappyPath(t *testing.T) {
	checks := &fakeChecksClient{
		respRef: iacgithub.CheckRunRef{Owner: "octo", Repo: "widgets", CheckID: 9001, HeadSHA: "abc123"},
	}
	store := &fakeCheckRunStore{}
	audit := &discoveryRecordingAudit{}

	h := newCheckRunTestHandler(checks, store, audit)
	h.emitCheckRunForOpenedPR(context.Background(), baselineCheckRunArgs())

	// CreateCheckRun called once with the expected wire shape.
	if len(checks.calls) != 1 {
		t.Fatalf("CreateCheckRun calls = %d, want 1", len(checks.calls))
	}
	got := checks.calls[0]
	if got.Owner != "octo" || got.Repo != "widgets" {
		t.Errorf("owner/repo = %q/%q", got.Owner, got.Repo)
	}
	if got.HeadSHA != "abc123" {
		t.Errorf("head_sha = %q", got.HeadSHA)
	}
	if got.Name != defaultCheckRunName {
		t.Errorf("name = %q, want %q", got.Name, defaultCheckRunName)
	}
	if got.Status != iacgithub.CheckRunStatusInProgress {
		t.Errorf("status = %q", got.Status)
	}
	if !strings.Contains(got.Output.Title, "rds-pi-em") {
		t.Errorf("title = %q", got.Output.Title)
	}
	if !strings.Contains(got.Output.Summary, "Verdict learning context") {
		t.Errorf("summary missing verdict-learning section:\n%s", got.Output.Summary)
	}
	// PAT routed through verbatim.
	if checks.pats[0] != "pat-chk-write" {
		t.Errorf("PAT = %q", checks.pats[0])
	}

	// SetCheckRunForRecommendation called once with status=in_progress
	// and the ref returned by CreateCheckRun.
	if len(store.calls) != 1 {
		t.Fatalf("store calls = %d, want 1", len(store.calls))
	}
	sc := store.calls[0]
	if sc.Rec.RecommendationID != "rec-xyz" {
		t.Errorf("stored recommendation_id = %q", sc.Rec.RecommendationID)
	}
	if sc.Rec.ConnectionID != "conn-abc" {
		t.Errorf("stored connection_id = %q", sc.Rec.ConnectionID)
	}
	if sc.Rec.AccountID != "111111111111" || sc.Rec.Region != "us-east-1" {
		t.Errorf("stored scope tuple = %+v", sc.Rec)
	}
	if sc.Rec.RecommendationKind != "rds-pi-em" {
		t.Errorf("stored recommendation_kind = %q", sc.Rec.RecommendationKind)
	}
	if sc.Ref.CheckID != 9001 {
		t.Errorf("stored check_run_id = %d", sc.Ref.CheckID)
	}
	if sc.Status != iacgithub.CheckRunStatusInProgress {
		t.Errorf("stored status = %q", sc.Status)
	}

	// iac.check_run.created audit event fires with the expected
	// payload fields.
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventIaCCheckRunCreated {
		t.Errorf("EventType = %q, want %q", e.EventType, services.AuditEventIaCCheckRunCreated)
	}
	if e.Payload["check_run_id"].(int64) != 9001 {
		t.Errorf("payload check_run_id = %v", e.Payload["check_run_id"])
	}
	if e.Payload["status"] != iacgithub.CheckRunStatusInProgress {
		t.Errorf("payload status = %v", e.Payload["status"])
	}
	if e.Payload["error_kind"] != nil {
		t.Errorf("happy-path payload should not carry error_kind: %v", e.Payload["error_kind"])
	}
}

// TestEmitCheckRunForOpenedPR_ScopeMissingError_EmitsFailedAudit —
// the fake CreateCheckRun returns scope_missing. Assert:
//   - iac.check_run.failed audit fires with error_kind=scope_missing
//   - NO SetCheckRunForRecommendation call (the durable row is only
//     written on a live check-run create)
//   - NO iac.check_run.created event (the success-event row is
//     gated on a successful create)
func TestEmitCheckRunForOpenedPR_ScopeMissingError_EmitsFailedAudit(t *testing.T) {
	checks := &fakeChecksClient{
		respErr: &iacgithub.CheckRunError{
			Kind:    iacgithub.CheckRunErrorKindScopeMissing,
			Status:  403,
			Message: "PAT lacks checks:write scope",
		},
	}
	store := &fakeCheckRunStore{}
	audit := &discoveryRecordingAudit{}

	h := newCheckRunTestHandler(checks, store, audit)
	h.emitCheckRunForOpenedPR(context.Background(), baselineCheckRunArgs())

	if len(store.calls) != 0 {
		t.Errorf("store should NOT be written on scope_missing; got %d calls", len(store.calls))
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventIaCCheckRunFailed {
		t.Errorf("EventType = %q, want %q", e.EventType, services.AuditEventIaCCheckRunFailed)
	}
	if e.Payload["error_kind"] != iacgithub.CheckRunErrorKindScopeMissing {
		t.Errorf("payload error_kind = %v, want %q", e.Payload["error_kind"], iacgithub.CheckRunErrorKindScopeMissing)
	}
	if e.Payload["http_status"].(int) != 403 {
		t.Errorf("payload http_status = %v", e.Payload["http_status"])
	}
}

// TestEmitCheckRunForOpenedPR_RateLimit_EmitsFailedAudit —
// equivalent to the scope_missing test for the rate_limit branch.
// SIEM dashboards fan out on the error_kind discriminator; pin
// the rate_limit code-path explicitly.
func TestEmitCheckRunForOpenedPR_RateLimit_EmitsFailedAudit(t *testing.T) {
	checks := &fakeChecksClient{
		respErr: &iacgithub.CheckRunError{
			Kind:    iacgithub.CheckRunErrorKindRateLimit,
			Status:  403,
			Message: "GitHub API rate limit exceeded (reset=1734567890)",
		},
	}
	store := &fakeCheckRunStore{}
	audit := &discoveryRecordingAudit{}

	h := newCheckRunTestHandler(checks, store, audit)
	h.emitCheckRunForOpenedPR(context.Background(), baselineCheckRunArgs())

	if len(store.calls) != 0 {
		t.Errorf("store should NOT be written on rate_limit; got %d calls", len(store.calls))
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventIaCCheckRunFailed {
		t.Errorf("EventType = %q", e.EventType)
	}
	if e.Payload["error_kind"] != iacgithub.CheckRunErrorKindRateLimit {
		t.Errorf("payload error_kind = %v, want %q", e.Payload["error_kind"], iacgithub.CheckRunErrorKindRateLimit)
	}
	if msg, ok := e.Payload["error_message"].(string); !ok || !strings.Contains(msg, "rate limit") {
		t.Errorf("payload error_message = %v", e.Payload["error_message"])
	}
}

// TestEmitCheckRunForOpenedPR_NilClient_NoOps — when h.checksClient
// is nil (operator hasn't enabled the integration yet) the helper
// MUST short-circuit with no audit emit and no error. This is the
// slice-1 fail-open posture for deployments upgrading PAT scope per
// design doc §5.
func TestEmitCheckRunForOpenedPR_NilClient_NoOps(t *testing.T) {
	store := &fakeCheckRunStore{}
	audit := &discoveryRecordingAudit{}

	// Pass nil ChecksAPI — newCheckRunTestHandler does not call
	// WithChecksClient when the arg is nil.
	h := newCheckRunTestHandler(nil, store, audit)
	h.emitCheckRunForOpenedPR(context.Background(), baselineCheckRunArgs())

	if len(audit.entries) != 0 {
		t.Errorf("nil-client should emit no audit events; got %d", len(audit.entries))
	}
	if len(store.calls) != 0 {
		t.Errorf("nil-client should write no store rows; got %d", len(store.calls))
	}
}

// TestEmitCheckRunForOpenedPR_ColdStart_NoVerdictContextInSummary —
// when VerdictExamplesUsedByState is empty the check-run summary
// composed inside the helper MUST omit the "Verdict learning
// context" section. Pin this end-to-end so future refactors to
// the composer stay safe.
//
// Mirrors design doc §13 acceptance test 12.
func TestEmitCheckRunForOpenedPR_ColdStart_NoVerdictContextInSummary(t *testing.T) {
	checks := &fakeChecksClient{}
	store := &fakeCheckRunStore{}
	audit := &discoveryRecordingAudit{}

	h := newCheckRunTestHandler(checks, store, audit)
	args := baselineCheckRunArgs()
	args.VerdictExamplesUsedByState = nil
	args.Request.VerdictExamplesUsedByState = nil
	h.emitCheckRunForOpenedPR(context.Background(), args)

	if len(checks.calls) != 1 {
		t.Fatalf("CreateCheckRun calls = %d", len(checks.calls))
	}
	summary := checks.calls[0].Output.Summary
	if strings.Contains(summary, "Verdict learning context") {
		t.Errorf("cold-start summary should omit 'Verdict learning context' section:\n%s", summary)
	}
	// Sanity: the rest of the template still renders.
	if !strings.Contains(summary, "**Squadron recommendation: rds-pi-em**") {
		t.Errorf("cold-start summary missing template header:\n%s", summary)
	}
}

// TestParseOwnerRepoFromPRURL covers the small helper that
// reconstructs (owner, repo) from a stored PR URL. Used by chunks
// 3+ when the connection's RepoFullName isn't readily available.
func TestParseOwnerRepoFromPRURL(t *testing.T) {
	cases := []struct {
		name           string
		in             string
		wantOwn, wantR string
	}{
		{"happy", "https://github.com/octo/widgets/pull/42", "octo", "widgets"},
		{"trailing_slash", "https://github.com/octo/widgets/pull/42/", "octo", "widgets"},
		{"with_anchor", "https://github.com/acme/infra/pull/1#discussion", "acme", "infra"},
		{"empty", "", "", ""},
		{"missing_marker", "https://example.com/foo/bar/pull/1", "", ""},
		{"not_pull", "https://github.com/octo/widgets/issues/42", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOwn, gotR := parseOwnerRepoFromPRURL(tc.in)
			if gotOwn != tc.wantOwn || gotR != tc.wantR {
				t.Errorf("got (%q, %q), want (%q, %q)", gotOwn, gotR, tc.wantOwn, tc.wantR)
			}
		})
	}
}
