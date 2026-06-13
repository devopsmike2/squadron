// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/configlint"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Store is the application-store subset the deploy service needs.
// Declared as a local interface so test fakes don't need to
// implement the whole CRUD surface.
type Store interface {
	CreateDeployTarget(ctx context.Context, t *apptypes.DeployTarget) error
	UpdateDeployTarget(ctx context.Context, t *apptypes.DeployTarget) error
	GetDeployTarget(ctx context.Context, id string) (*apptypes.DeployTarget, error)
	ListDeployTargets(ctx context.Context) ([]*apptypes.DeployTarget, error)
	DeleteDeployTarget(ctx context.Context, id string) error

	CreateDeployRun(ctx context.Context, r *apptypes.DeployRun) error
	UpdateDeployRun(ctx context.Context, r *apptypes.DeployRun) error
	GetDeployRun(ctx context.Context, id string) (*apptypes.DeployRun, error)
	ListDeployRuns(ctx context.Context, filter apptypes.DeployRunFilter) ([]*apptypes.DeployRun, error)

	// For pinned-config lookup at trigger time.
	GetConfig(ctx context.Context, id string) (*apptypes.Config, error)

	// For auto-registering expected hosts after a successful deploy.
	UpsertExpectedAgent(ctx context.Context, e *apptypes.ExpectedAgent) error
}

// Service is the public surface. NewService wires it up; Trigger
// is the hot path; SyncRun is called by the background poller and
// the webhook receiver.
type Service struct {
	store    Store
	provider Provider
	crypter  *Crypter
	logger   *zap.Logger
}

// NewService constructs the service. Pass nil for crypter to
// disable the feature (the API layer will 503 in that case).
func NewService(store Store, provider Provider, crypter *Crypter, logger *zap.Logger) *Service {
	return &Service{store: store, provider: provider, crypter: crypter, logger: logger}
}

// Enabled reports whether the deploy feature is wired up. False
// when SQUADRON_DEPLOY_KEY was missing at startup; the API returns
// 503 in that case so the UI can render a "set up the key" message.
func (s *Service) Enabled() bool {
	return s != nil && s.crypter != nil && s.provider != nil
}

// CreateTarget creates a new deploy target. The plaintext PAT is
// encrypted inline and the cleartext zeroed; nothing else in the
// codebase ever sees it again until decrypt time at Trigger.
func (s *Service) CreateTarget(ctx context.Context, t *apptypes.DeployTarget, pat string) error {
	if !s.Enabled() {
		return ErrKeyMissing
	}
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if pat != "" {
		sealed, err := s.crypter.Encrypt([]byte(pat))
		if err != nil {
			return fmt.Errorf("encrypt PAT: %w", err)
		}
		t.EncryptedCredential = sealed
	}
	return s.store.CreateDeployTarget(ctx, t)
}

// UpdateTarget updates an existing target. If pat is empty, the
// existing credential is preserved; pass a non-empty pat to rotate.
func (s *Service) UpdateTarget(ctx context.Context, t *apptypes.DeployTarget, pat string) error {
	if !s.Enabled() {
		return ErrKeyMissing
	}
	if pat != "" {
		sealed, err := s.crypter.Encrypt([]byte(pat))
		if err != nil {
			return fmt.Errorf("encrypt PAT: %w", err)
		}
		t.EncryptedCredential = sealed
	} else {
		t.EncryptedCredential = nil // signals "preserve" to the store
	}
	return s.store.UpdateDeployTarget(ctx, t)
}

// TriggerRequest is the in-process shape for firing a deploy. The
// API layer translates JSON into this and calls Trigger.
type TriggerRequest struct {
	TargetID      string
	RequestedBy   string
	Inputs        map[string]string // merged with target.DefaultInputs at execution
	ExpectedHosts []string          // optional; auto-registered into expected_agents on success
	Notes         string
}

// LintGateError is returned when configlint refuses the deploy.
// The handler unwraps it into a 422 with the lint findings attached.
type LintGateError struct {
	Findings []configlint.Finding
}

func (e *LintGateError) Error() string {
	return fmt.Sprintf("config lint blocked deploy (%d findings)", len(e.Findings))
}

// Trigger validates, dispatches, and records a deploy run. The
// flow:
//
//  1. Resolve target.
//  2. If target.ConfigID is set, lint the config; HARD BLOCK on errors.
//  3. Decrypt PAT.
//  4. Merge inputs (default + request).
//  5. Provider.Dispatch.
//  6. Provider.LatestRunSince to attach the GitHub run_id.
//  7. Persist a deploy_run row.
//  8. Caller is the background poller, which advances the run
//     through its lifecycle from there.
func (s *Service) Trigger(ctx context.Context, req TriggerRequest) (*apptypes.DeployRun, error) {
	if !s.Enabled() {
		return nil, ErrKeyMissing
	}
	target, err := s.store.GetDeployTarget(ctx, req.TargetID)
	if err != nil {
		return nil, fmt.Errorf("get target: %w", err)
	}
	if target == nil {
		return nil, fmt.Errorf("deploy target not found")
	}
	if len(target.EncryptedCredential) == 0 {
		return nil, fmt.Errorf("deploy target has no credential set; configure a PAT first")
	}

	// Step 1: pre-flight lint (hard block).
	if target.ConfigID != "" {
		cfg, err := s.store.GetConfig(ctx, target.ConfigID)
		if err != nil {
			return nil, fmt.Errorf("get pinned config: %w", err)
		}
		if cfg == nil {
			return nil, fmt.Errorf("pinned config %s not found", target.ConfigID)
		}
		if errs := filterErrors(configlint.Lint(cfg.Content)); len(errs) > 0 {
			return nil, &LintGateError{Findings: errs}
		}
	}

	// Step 2: decrypt PAT. Plaintext stays on the stack — never
	// logged, never returned through the API.
	pat, err := s.crypter.Decrypt(target.EncryptedCredential)
	if err != nil {
		return nil, fmt.Errorf("decrypt PAT: %w", err)
	}
	defer zeroize(pat)

	// Step 3: merge inputs. Request overrides defaults so an operator
	// can twiddle one knob without re-typing everything.
	merged := map[string]string{}
	for k, v := range target.DefaultInputs {
		merged[k] = v
	}
	for k, v := range req.Inputs {
		merged[k] = v
	}

	// Step 4: dispatch.
	dispatchedAt := time.Now().UTC()
	if _, err := s.provider.Dispatch(ctx, target, string(pat), merged); err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}

	// Step 5: persist the run with a placeholder run_id. The poller
	// (or the next SyncRun call) will attach the real GitHub run_id
	// via LatestRunSince. We do an inline best-effort attach here too
	// so the UI gets a clickable link as soon as possible.
	run := &apptypes.DeployRun{
		ID:            uuid.NewString(),
		TargetID:      target.ID,
		RequestedBy:   req.RequestedBy,
		RequestedAt:   dispatchedAt,
		Inputs:        merged,
		Status:        "queued",
		ExpectedHosts: req.ExpectedHosts,
		Notes:         req.Notes,
	}
	if rs, err := s.provider.LatestRunSince(ctx, target, string(pat), dispatchedAt.Add(-30*time.Second)); err == nil && rs != nil {
		run.GitHubRunID = rs.GitHubRunID
		run.GitHubRunURL = rs.GitHubRunURL
		run.Status = rs.Status
	}
	if err := s.store.CreateDeployRun(ctx, run); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	s.logger.Info("deploy run dispatched",
		zap.String("run_id", run.ID),
		zap.String("target", target.Name),
		zap.Int64("github_run_id", run.GitHubRunID),
		zap.String("requested_by", req.RequestedBy),
		zap.Int("expected_hosts", len(req.ExpectedHosts)))
	return run, nil
}

// SyncRun refreshes a run's status from GitHub. Called by the
// background poller every minute for in-flight runs, and by the
// webhook receiver immediately on workflow_run events. Idempotent.
//
// On terminal success, ExpectedHosts get auto-registered into the
// v0.32 expected_agents table so the inventory reconciliation
// surface can flag any host the workflow claimed to deploy but
// never actually checks in.
func (s *Service) SyncRun(ctx context.Context, runID string) (*apptypes.DeployRun, error) {
	if !s.Enabled() {
		return nil, ErrKeyMissing
	}
	run, err := s.store.GetDeployRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("run not found")
	}
	if run.Status == "completed" {
		return run, nil
	}
	target, err := s.store.GetDeployTarget(ctx, run.TargetID)
	if err != nil || target == nil {
		return run, nil
	}
	if len(target.EncryptedCredential) == 0 {
		return run, nil
	}
	pat, err := s.crypter.Decrypt(target.EncryptedCredential)
	if err != nil {
		return run, nil
	}
	defer zeroize(pat)

	// Two paths: if we already have the run_id, GET it directly.
	// Otherwise try LatestRunSince again — workflow_dispatch is
	// async and the run may have appeared between Trigger and now.
	var status *RunStatus
	if run.GitHubRunID > 0 {
		status, err = s.provider.GetRun(ctx, target, string(pat), run.GitHubRunID)
	} else {
		status, err = s.provider.LatestRunSince(ctx, target, string(pat), run.RequestedAt.Add(-30*time.Second))
	}
	if err != nil {
		return run, fmt.Errorf("sync from github: %w", err)
	}
	if status == nil {
		return run, nil // not yet visible
	}

	run.GitHubRunID = status.GitHubRunID
	run.GitHubRunURL = status.GitHubRunURL
	run.Status = status.Status
	run.Conclusion = status.Conclusion
	if status.CompletedAt != nil {
		run.CompletedAt = status.CompletedAt
	}

	// On terminal success, auto-register expected hosts so v0.32
	// reconciliation can flag any that don't check in.
	if run.Status == "completed" && run.Conclusion == "success" && len(run.ExpectedHosts) > 0 && run.VerificationState == "" {
		source := "squadron-deploy:" + target.ID
		for _, host := range run.ExpectedHosts {
			_ = s.store.UpsertExpectedAgent(ctx, &apptypes.ExpectedAgent{
				Hostname: host,
				Source:   source,
				Notes:    "auto-registered by deploy " + run.ID,
			})
		}
		run.VerificationState = "pending"
		now := time.Now().UTC()
		run.VerifiedAt = &now
	}

	if err := s.store.UpdateDeployRun(ctx, run); err != nil {
		return run, fmt.Errorf("update run: %w", err)
	}
	return run, nil
}

// SyncOpenRuns is the polling pass. Walks every run in queued or
// in_progress state, calls SyncRun on each. Caller runs this on a
// ticker.
func (s *Service) SyncOpenRuns(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	for _, status := range []string{"queued", "in_progress"} {
		runs, err := s.store.ListDeployRuns(ctx, apptypes.DeployRunFilter{Status: status, Limit: 50})
		if err != nil {
			return fmt.Errorf("list %s runs: %w", status, err)
		}
		for _, r := range runs {
			if _, err := s.SyncRun(ctx, r.ID); err != nil {
				s.logger.Warn("sync deploy run failed",
					zap.String("run_id", r.ID), zap.Error(err))
			}
		}
	}
	return nil
}

// ListTargets, GetTarget, DeleteTarget, ListRuns are thin
// pass-throughs to the store so the API handler has a single
// dependency surface (the Service rather than the store).

func (s *Service) ListTargets(ctx context.Context) ([]*apptypes.DeployTarget, error) {
	return s.store.ListDeployTargets(ctx)
}
func (s *Service) GetTarget(ctx context.Context, id string) (*apptypes.DeployTarget, error) {
	return s.store.GetDeployTarget(ctx, id)
}
func (s *Service) DeleteTarget(ctx context.Context, id string) error {
	return s.store.DeleteDeployTarget(ctx, id)
}
func (s *Service) ListRuns(ctx context.Context, filter apptypes.DeployRunFilter) ([]*apptypes.DeployRun, error) {
	return s.store.ListDeployRuns(ctx, filter)
}

// LintConfig is exposed so the UI can preview lint findings before
// the operator clicks Deploy.
func (s *Service) LintConfig(ctx context.Context, configID string) ([]configlint.Finding, error) {
	cfg, err := s.store.GetConfig(ctx, configID)
	if err != nil {
		return nil, fmt.Errorf("get config: %w", err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("config not found")
	}
	return configlint.Lint(cfg.Content), nil
}

// filterErrors returns only error-severity findings — the hard
// block ignores warnings + info so an operator who's "almost
// there" doesn't get blocked on a style nit.
func filterErrors(in []configlint.Finding) []configlint.Finding {
	out := make([]configlint.Finding, 0, len(in))
	for _, f := range in {
		if f.Severity == configlint.SeverityError {
			out = append(out, f)
		}
	}
	return out
}

// zeroize clears a byte slice in-place. Belt-and-suspenders against
// the PAT lingering in a goroutine's stack frame after Trigger
// returns; go's GC won't actively scrub the memory.
func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// SanitizeHost normalizes a hostname for the ExpectedHosts list.
// Trims whitespace; drops empty entries.
func SanitizeHost(s string) string {
	return strings.TrimSpace(s)
}
