// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

	// For v0.35 live-host-status cross-referencing the inventory
	// preview against connected agents.
	ListAgents(ctx context.Context) ([]*apptypes.Agent, error)
}

// Service is the public surface. NewService wires it up; Trigger
// is the hot path; SyncRun is called by the background poller and
// the webhook receiver.
type Service struct {
	store    Store
	provider Provider
	crypter  *Crypter
	logger   *zap.Logger

	// v0.35: completion webhook. Empty means logging-only (still
	// useful in dev). Set via SetCompletionWebhook from main.go
	// based on deploy.completion_webhook_url in squadron.yaml.
	completionWebhookURL string
	httpClient           *http.Client
}

// NewService constructs the service. Pass nil for crypter to
// disable the feature (the API layer will 503 in that case).
func NewService(store Store, provider Provider, crypter *Crypter, logger *zap.Logger) *Service {
	return &Service{
		store:      store,
		provider:   provider,
		crypter:    crypter,
		logger:     logger,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SetCompletionWebhook configures the v0.35 deploy-completion
// webhook destination. Idempotent — call from main.go after
// reading squadron.yaml.
func (s *Service) SetCompletionWebhook(url string) {
	s.completionWebhookURL = url
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

	// Step 2.5 (v0.34.1): when the target has an inventory path, fetch
	// the file from GitHub and use the parsed hosts as the deploy's
	// expected-host list. This matches the SouthernCo-style workflow
	// where inventory.ini is checked in and the workflow's ansible
	// step reads it via -i. The caller's req.ExpectedHosts is
	// IGNORED in this mode — the checked-in file is the source of
	// truth, and silently allowing the request to override would let
	// the inventory view and the actual deploy diverge.
	if target.InventoryPath != "" {
		raw, fetchErr := s.provider.FetchFile(ctx, target, string(pat), target.InventoryPath)
		if fetchErr != nil {
			return nil, fmt.Errorf("fetch inventory %q: %w", target.InventoryPath, fetchErr)
		}
		req.ExpectedHosts = ParseInventoryHosts(raw)
	}

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

	// Detect the terminal-state transition so the completion
	// webhook fires exactly once even if SyncRun is called many
	// times after a run completes.
	wasOpen := run.Status != "completed"

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

	// v0.35: completion webhook on the queued/in_progress → completed
	// edge. Best-effort: a failed webhook never blocks the sync path.
	if wasOpen && run.Status == "completed" {
		s.fireCompletionWebhook(ctx, target, run)
	}
	return run, nil
}

// CompletionEvent is the payload Squadron POSTs on deploy
// terminal-state transitions. Shape mirrors the alerting +
// silent-agent payloads so a single webhook receiver can route by
// the `kind` field.
type CompletionEvent struct {
	Kind          string    `json:"kind"`           // "deploy_completed"
	State         string    `json:"state"`          // "success" | "failure" | "cancelled" | "timed_out" | "skipped"
	RunID         string    `json:"run_id"`
	TargetID      string    `json:"target_id"`
	TargetName    string    `json:"target_name"`
	RequestedBy   string    `json:"requested_by"`
	GitHubRunID   int64     `json:"github_run_id,omitempty"`
	GitHubRunURL  string    `json:"github_run_url,omitempty"`
	ExpectedHosts []string  `json:"expected_hosts,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	At            time.Time `json:"at"`
}

// fireCompletionWebhook dispatches the v0.35 completion event. Best
// effort — logging is the always-on channel; the HTTP channel runs
// only when a URL is configured.
func (s *Service) fireCompletionWebhook(ctx context.Context, target *apptypes.DeployTarget, run *apptypes.DeployRun) {
	state := run.Conclusion
	if state == "" {
		state = "completed"
	}
	completed := time.Now().UTC()
	if run.CompletedAt != nil {
		completed = *run.CompletedAt
	}
	evt := CompletionEvent{
		Kind:          "deploy_completed",
		State:         state,
		RunID:         run.ID,
		TargetID:      target.ID,
		TargetName:    target.Name,
		RequestedBy:   run.RequestedBy,
		GitHubRunID:   run.GitHubRunID,
		GitHubRunURL:  run.GitHubRunURL,
		ExpectedHosts: run.ExpectedHosts,
		StartedAt:     run.RequestedAt,
		CompletedAt:   completed,
		At:            time.Now().UTC(),
	}
	s.logger.Info("deploy completed",
		zap.String("run_id", evt.RunID),
		zap.String("target", evt.TargetName),
		zap.String("state", evt.State),
		zap.Int64("github_run_id", evt.GitHubRunID))
	if s.completionWebhookURL == "" {
		return
	}
	body, err := json.Marshal(evt)
	if err != nil {
		s.logger.Warn("deploy completion webhook marshal failed", zap.Error(err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.completionWebhookURL, bytes.NewReader(body))
	if err != nil {
		s.logger.Warn("deploy completion webhook build failed", zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Squadron/deploy-completion")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Warn("deploy completion webhook POST failed",
			zap.Error(err), zap.String("url", s.completionWebhookURL))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		s.logger.Warn("deploy completion webhook returned non-2xx",
			zap.Int("status", resp.StatusCode))
	}
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
func (s *Service) GetRun(ctx context.Context, id string) (*apptypes.DeployRun, error) {
	return s.store.GetDeployRun(ctx, id)
}

// ValidationResult is what the Validate endpoint returns. Each
// check is independent — a target with no pinned config still
// reports LintCheck.Skipped rather than a generic OK so the UI can
// be explicit about what was actually verified.
type ValidationResult struct {
	GitHubAuth     CheckStatus `json:"github_auth"`
	WorkflowExists CheckStatus `json:"workflow_exists"`
	Inventory      CheckStatus `json:"inventory"`
	LintCheck      CheckStatus `json:"lint_check"`
	OverallOK      bool        `json:"overall_ok"`
}

// CheckStatus is one validation finding. Status is one of "ok",
// "warn", "fail", or "skip" (when the check doesn't apply — e.g.
// no inventory_path configured). Message is short and human.
type CheckStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Validate exercises every read path on a target without firing a
// workflow_dispatch. Useful as a pre-flight before the first real
// deploy: catches PAT typos, wrong workflow names, unreadable
// inventory paths, broken pinned configs. UI renders the result as
// a checklist.
//
// Implementation notes:
//   - GitHub auth is verified by hitting /repos/{owner}/{repo} —
//     cheapest call that proves the PAT can see the repo.
//   - Workflow existence uses GET /repos/{owner}/{repo}/actions/
//     workflows/{file}, which 404s on the wrong file name.
//   - Inventory check pulls the file via the Contents API and
//     parses it; reports the host count.
//   - Lint check runs configlint on the pinned config if set.
//
// Failures don't abort each other — every check runs so the UI
// shows everything at once instead of "fix this, then run again,
// then see the next thing."
func (s *Service) Validate(ctx context.Context, targetID string) (*ValidationResult, error) {
	if !s.Enabled() {
		return nil, ErrKeyMissing
	}
	target, err := s.store.GetDeployTarget(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("get target: %w", err)
	}
	if target == nil {
		return nil, fmt.Errorf("target not found")
	}
	result := &ValidationResult{}

	if len(target.EncryptedCredential) == 0 {
		result.GitHubAuth = CheckStatus{Status: "fail", Message: "PAT not configured"}
		result.WorkflowExists = CheckStatus{Status: "skip", Message: "no PAT to test with"}
		result.Inventory = CheckStatus{Status: "skip", Message: "no PAT to test with"}
		// Lint can still run since it's local.
		result.LintCheck = s.lintCheckStatus(ctx, target)
		return result, nil
	}
	pat, err := s.crypter.Decrypt(target.EncryptedCredential)
	if err != nil {
		result.GitHubAuth = CheckStatus{Status: "fail", Message: "PAT decryption failed (key rotated?)"}
		result.WorkflowExists = CheckStatus{Status: "skip", Message: "no usable PAT"}
		result.Inventory = CheckStatus{Status: "skip", Message: "no usable PAT"}
		result.LintCheck = s.lintCheckStatus(ctx, target)
		return result, nil
	}
	defer zeroize(pat)

	// Check 1: GitHub auth via cheap repo metadata fetch.
	if probeErr := s.provider.ProbeAuth(ctx, target, string(pat)); probeErr != nil {
		result.GitHubAuth = CheckStatus{Status: "fail", Message: probeErr.Error()}
	} else {
		result.GitHubAuth = CheckStatus{Status: "ok", Message: "PAT can read the repo"}
	}

	// Check 2: workflow file exists (only meaningful if auth passed).
	if result.GitHubAuth.Status == "ok" {
		if probeErr := s.provider.ProbeWorkflow(ctx, target, string(pat)); probeErr != nil {
			result.WorkflowExists = CheckStatus{Status: "fail", Message: probeErr.Error()}
		} else {
			result.WorkflowExists = CheckStatus{
				Status:  "ok",
				Message: fmt.Sprintf("%s exists on branch %s", target.GitHubWorkflow, branchOrMain(target)),
			}
		}
	} else {
		result.WorkflowExists = CheckStatus{Status: "skip", Message: "auth failed; skipping"}
	}

	// Check 3: inventory readable (if path configured).
	if target.InventoryPath == "" {
		result.Inventory = CheckStatus{Status: "skip", Message: "no inventory_path configured"}
	} else if result.GitHubAuth.Status != "ok" {
		result.Inventory = CheckStatus{Status: "skip", Message: "auth failed; skipping"}
	} else {
		raw, ferr := s.provider.FetchFile(ctx, target, string(pat), target.InventoryPath)
		if ferr != nil {
			result.Inventory = CheckStatus{Status: "fail", Message: ferr.Error()}
		} else {
			hosts := ParseInventoryHosts(raw)
			if len(hosts) == 0 {
				result.Inventory = CheckStatus{
					Status:  "warn",
					Message: "file readable but parsed zero hosts",
				}
			} else {
				result.Inventory = CheckStatus{
					Status:  "ok",
					Message: fmt.Sprintf("parsed %d host(s)", len(hosts)),
				}
			}
		}
	}

	// Check 4: lint pinned config.
	result.LintCheck = s.lintCheckStatus(ctx, target)

	result.OverallOK = result.GitHubAuth.Status != "fail" &&
		result.WorkflowExists.Status != "fail" &&
		result.Inventory.Status != "fail" &&
		result.LintCheck.Status != "fail"
	return result, nil
}

// lintCheckStatus runs configlint over the target's pinned config
// and produces the per-check status. Pulled out so the no-PAT
// path can call it independently.
func (s *Service) lintCheckStatus(ctx context.Context, target *apptypes.DeployTarget) CheckStatus {
	if target.ConfigID == "" {
		return CheckStatus{Status: "skip", Message: "no pinned config"}
	}
	cfg, err := s.store.GetConfig(ctx, target.ConfigID)
	if err != nil || cfg == nil {
		return CheckStatus{Status: "fail", Message: "pinned config not found"}
	}
	findings := filterErrors(configlint.Lint(cfg.Content))
	if len(findings) > 0 {
		return CheckStatus{
			Status:  "fail",
			Message: fmt.Sprintf("%d lint error(s) — would block deploy", len(findings)),
		}
	}
	return CheckStatus{Status: "ok", Message: "lint passes"}
}

func branchOrMain(t *apptypes.DeployTarget) string {
	if t.GitHubBranch == "" {
		return "main"
	}
	return t.GitHubBranch
}

// DecryptedPAT exposes the PAT decryption path to other Squadron
// packages (notably v0.36.1's GHA walker). The plaintext is returned
// directly; the caller is responsible for not logging it. Don't
// expose this through the API layer — that's why there's no
// HTTP handler that wraps it.
func (s *Service) DecryptedPAT(ctx context.Context, target *apptypes.DeployTarget) (string, error) {
	if !s.Enabled() {
		return "", ErrKeyMissing
	}
	if target == nil {
		return "", fmt.Errorf("target nil")
	}
	if len(target.EncryptedCredential) == 0 {
		return "", fmt.Errorf("target has no credential")
	}
	pt, err := s.crypter.Decrypt(target.EncryptedCredential)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// FetchInventory pulls the configured inventory.ini from GitHub and
// returns the parsed host list. Used by the trigger sheet's preview
// (so the operator sees what hosts the deploy is about to hit) and
// by an explicit "Refresh" affordance.
//
// Returns ("", nil, nil) when the target has no inventory path
// configured — caller renders the manual host-entry UI in that case.
// Returns (path, nil, err) on a fetch/decrypt failure so the UI can
// distinguish "feature disabled" from "GitHub responded unhappy".
func (s *Service) FetchInventory(ctx context.Context, targetID string) (string, []string, error) {
	if !s.Enabled() {
		return "", nil, ErrKeyMissing
	}
	target, err := s.store.GetDeployTarget(ctx, targetID)
	if err != nil {
		return "", nil, fmt.Errorf("get target: %w", err)
	}
	if target == nil {
		return "", nil, fmt.Errorf("target not found")
	}
	if target.InventoryPath == "" {
		return "", nil, nil
	}
	if len(target.EncryptedCredential) == 0 {
		return target.InventoryPath, nil, fmt.Errorf("target has no credential set")
	}
	pat, err := s.crypter.Decrypt(target.EncryptedCredential)
	if err != nil {
		return target.InventoryPath, nil, fmt.Errorf("decrypt PAT: %w", err)
	}
	defer zeroize(pat)
	raw, err := s.provider.FetchFile(ctx, target, string(pat), target.InventoryPath)
	if err != nil {
		return target.InventoryPath, nil, err
	}
	return target.InventoryPath, ParseInventoryHosts(raw), nil
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

// HostLiveStatus is the v0.35 inventory-preview enrichment: for
// each hostname parsed from inventory.ini, the operator gets to
// see whether that host is currently checking in via OpAMP. Lets
// them spot "the deploy I'm about to fire would hit a host that's
// been silent for 30 minutes" before clicking Run.
type HostLiveStatus struct {
	Hostname  string     `json:"hostname"`
	Status    string     `json:"status"`    // "healthy" | "silent" | "never_seen"
	AgentID   string     `json:"agent_id,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	SilenceFor string    `json:"silence_for,omitempty"`
}

// SilentThresholdForInventory is the wall-clock gap after which a
// known-agent host is flagged "silent" in the inventory preview.
// Mirrors the v0.32 inventory reconciliation threshold so the two
// surfaces report the same verdict.
const SilentThresholdForInventory = 10 * time.Minute

// HostsWithLiveStatus pairs the parsed inventory hostnames with
// their current OpAMP status. Hostname normalization mirrors the
// v0.32 inventory reconciliation hostKey (lowercase + strip FQDN
// suffix) so a checked-in `GAXGPAP158UA` matches an OpAMP agent
// reporting as `gaxgpap158ua.example.com`.
func (s *Service) HostsWithLiveStatus(ctx context.Context, targetID string) (string, []HostLiveStatus, error) {
	path, hosts, err := s.FetchInventory(ctx, targetID)
	if err != nil {
		return path, nil, err
	}
	if len(hosts) == 0 {
		return path, nil, nil
	}
	agents, _ := s.store.ListAgents(ctx)
	byKey := map[string]*apptypes.Agent{}
	for _, a := range agents {
		if a == nil {
			continue
		}
		byKey[hostKey(a.Name)] = a
	}
	now := time.Now().UTC()
	out := make([]HostLiveStatus, 0, len(hosts))
	for _, h := range hosts {
		entry := HostLiveStatus{Hostname: h, Status: "never_seen"}
		if a, ok := byKey[hostKey(h)]; ok {
			ls := a.LastSeen
			entry.AgentID = a.ID.String()
			entry.LastSeen = &ls
			if now.Sub(a.LastSeen) > SilentThresholdForInventory {
				entry.Status = "silent"
				entry.SilenceFor = now.Sub(a.LastSeen).Round(time.Second).String()
			} else {
				entry.Status = "healthy"
			}
		}
		out = append(out, entry)
	}
	return path, out, nil
}

// hostKey duplicates internal/inventory.hostKey logic to avoid an
// import cycle. Lowercase + strip the first '.' onward so short
// names match FQDNs.
func hostKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if idx := strings.IndexByte(s, '.'); idx > 0 {
		return s[:idx]
	}
	return s
}

// SanitizeHost normalizes a hostname for the ExpectedHosts list.
// Trims whitespace; drops empty entries.
func SanitizeHost(s string) string {
	return strings.TrimSpace(s)
}
