// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Runner is the long-lived state of the daemon: pinned issuer key,
// authenticated HTTP client, executor, dedup of already-handled
// request IDs. One Runner per process.
type Runner struct {
	cfg      *Config
	verifier *actions.Verifier
	client   *http.Client
	exec     Executor
	logger   *zap.Logger

	// DryRunOnly, when true, refuses phase=execute requests by
	// reporting them as denied with reason "dry-run-only mode".
	// Operators set this on canary nodes that should never take a
	// real action.
	DryRunOnly bool

	// PollInterval is copied from cfg so tests can override it.
	pollInterval time.Duration

	// seen caches request IDs the runner has already answered. The
	// API returns a request as long as Status=pending; the runner
	// updates server-side state by posting a result, but a slow
	// post or transient network hiccup can lead to a duplicate
	// poll. seen makes that idempotent.
	seen map[string]struct{}

	// now is overridable for tests that need to drive the clock.
	now func() time.Time
}

// Executor is the contract between the protocol layer and the
// per-action machinery. Implementations decide what the action
// actually does on the node. The MVP has one (systemd); future
// actions add more entries to the type switch.
type Executor interface {
	ExecuteRequest(ctx context.Context, req *actions.Request) *actions.Result
}

// NewRunner constructs a Runner. The issuer key is loaded from the
// config; the http.Client is constructed here so tests can substitute
// a roundtripper.
func NewRunner(cfg *Config, exec Executor, logger *zap.Logger) (*Runner, error) {
	if cfg == nil {
		return nil, errors.New("nil config")
	}
	if exec == nil {
		return nil, errors.New("nil executor")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	verifier, err := actions.NewVerifierFromPEM([]byte(cfg.SquadronPublicKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("parse squadron_public_key_pem: %w", err)
	}
	r := &Runner{
		cfg:          cfg,
		verifier:     verifier,
		client:       &http.Client{Timeout: 30 * time.Second},
		exec:         exec,
		logger:       logger,
		pollInterval: cfg.PollInterval,
		seen:         map[string]struct{}{},
		now:          func() time.Time { return time.Now().UTC() },
	}
	return r, nil
}

// SetHTTPClient swaps the http.Client. Tests use this to plug an
// httptest server into the runner.
func (r *Runner) SetHTTPClient(c *http.Client) { r.client = c }

// SetClock overrides the time source. Tests use this to age out a
// signed request without sleeping.
func (r *Runner) SetClock(now func() time.Time) { r.now = now }

// Run blocks until ctx is cancelled. It registers once and then polls
// in a loop. Network errors do not stop the loop; they get logged and
// retried on the next tick. Only a context cancellation exits cleanly.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Register(ctx); err != nil {
		// Squadron may be coming up after the runner. Log and keep
		// polling — the registration is idempotent and we retry on
		// each successful tick that finds the runner unregistered.
		r.logger.Warn("initial registration failed; will retry on poll", zap.Error(err))
	} else {
		r.logger.Info("registered with squadron",
			zap.String("runner_id", r.cfg.RunnerID),
			zap.String("squadron_url", r.cfg.SquadronURL),
		)
	}

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	// First tick right away so demos do not wait the full interval.
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("runner stopping", zap.Error(ctx.Err()))
			return nil
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// Register enrolls the runner with Squadron. Idempotent server side.
func (r *Runner) Register(ctx context.Context) error {
	// Derive the runner's own public key for the registration. The
	// MVP runner doesn't use it for auth (HTTPS+token does that),
	// but Squadron stores it so future mutual-auth modes have what
	// they need.
	publicPEM := ""
	if r.cfg.PrivateKeyPEM != "" {
		if priv, err := privateKeyFromPEM(r.cfg.PrivateKeyPEM); err == nil {
			publicPEM = encodePublicKeyPEM(priv)
		}
	}

	body := map[string]any{
		"runner_id":      r.cfg.RunnerID,
		"hostname":       r.cfg.Hostname,
		"public_key_pem": publicPEM,
		"capabilities":   r.cfg.Capabilities,
	}
	return r.postJSON(ctx, "/api/v1/runners/register", body, nil)
}

// tick performs one poll cycle. Errors are logged but do not stop
// the runner.
func (r *Runner) tick(ctx context.Context) {
	requests, err := r.fetchPending(ctx)
	if err != nil {
		r.logger.Warn("poll failed", zap.Error(err))
		return
	}
	for _, stored := range requests {
		if _, done := r.seen[stored.ID]; done {
			continue
		}
		r.handleOne(ctx, stored)
		r.seen[stored.ID] = struct{}{}
	}
}

// fetchPending calls GET /runners/:id/pending and decodes the wire
// shape. Returns the storage-typed list because that is what the
// API returns directly.
func (r *Runner) fetchPending(ctx context.Context) ([]*types.ActionRequest, error) {
	var resp struct {
		Requests []*types.ActionRequest `json:"requests"`
	}
	path := fmt.Sprintf("/api/v1/runners/%s/pending", r.cfg.RunnerID)
	if err := r.getJSON(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.Requests, nil
}

// handleOne processes a single request: verify, execute, post result.
// Each step that fails has a defined outcome so a malformed request
// becomes a "denied" result rather than a silent no-op.
func (r *Runner) handleOne(ctx context.Context, stored *types.ActionRequest) {
	log := r.logger.With(
		zap.String("request_id", stored.ID),
		zap.String("action_type", stored.ActionType),
		zap.String("phase", stored.Phase),
	)
	log.Info("handling pending action request")

	// Reconstruct the signed wire request from storage.
	wire := &actions.Request{
		RequestID:  stored.ID,
		ProposalID: stored.ProposalID,
		RunnerID:   stored.RunnerID,
		Action: actions.ActionPayload{
			Type:       stored.ActionType,
			Parameters: json.RawMessage(stored.ParametersJSON),
		},
		IssuedAt:  stored.IssuedAt,
		ExpiresAt: stored.ExpiresAt,
		Phase:     actions.Phase(stored.Phase),
		Signature: stored.Signature,
	}

	// 1. Verify signature against the pinned issuer key. A failure
	//    here is the single most security-sensitive event the runner
	//    handles; we deny and report.
	if err := r.verifier.Verify(wire, r.now()); err != nil {
		log.Warn("signature verification failed; denying", zap.Error(err))
		r.postResult(ctx, stored.ID, &actions.Result{
			RequestID: stored.ID,
			Phase:     wire.Phase,
			Status:    actions.StatusDenied,
			DeniedFor: "signature: " + err.Error(),
		})
		return
	}

	// 2. Dry-run-only nodes refuse phase=execute regardless of
	//    signature validity.
	if r.DryRunOnly && wire.Phase == actions.PhaseExecute {
		log.Info("dry-run-only mode; denying execute phase")
		r.postResult(ctx, stored.ID, &actions.Result{
			RequestID: stored.ID,
			Phase:     wire.Phase,
			Status:    actions.StatusDenied,
			DeniedFor: "dry-run-only mode",
		})
		return
	}

	// 3. Capability check (defense in depth — Squadron also enforces).
	if !r.actionAllowed(wire) {
		log.Warn("action falls outside declared capabilities; denying")
		r.postResult(ctx, stored.ID, &actions.Result{
			RequestID: stored.ID,
			Phase:     wire.Phase,
			Status:    actions.StatusDenied,
			DeniedFor: "out_of_policy",
		})
		return
	}

	// 4. Execute through the registered executor and post the result.
	result := r.exec.ExecuteRequest(ctx, wire)
	if result == nil {
		log.Error("executor returned nil result; treating as failure")
		result = &actions.Result{
			RequestID: stored.ID,
			Phase:     wire.Phase,
			Status:    actions.StatusFailure,
			Stderr:    "executor returned no result",
		}
	}
	result.RequestID = stored.ID
	result.Phase = wire.Phase
	r.postResult(ctx, stored.ID, result)
}

// actionAllowed checks the wire request against the runner's locally
// declared capabilities. Squadron already checked at dispatch time,
// but the runner repeats the check so a compromised Squadron cannot
// quietly widen what the node will do.
func (r *Runner) actionAllowed(wire *actions.Request) bool {
	// Find a capability declaration for the action type.
	for _, cap := range r.cfg.Capabilities {
		if cap.Type != wire.Action.Type {
			continue
		}
		// Use the registered action type's capability matcher.
		at, ok := actions.Default.Get(wire.Action.Type)
		if !ok {
			return false
		}
		ok, _ = at.MatchesCapability(wire.Action.Parameters, cap)
		if ok {
			return true
		}
	}
	return false
}

// postResult sends the result back to Squadron. The runner does not
// retry a failed post (the request will appear as pending again on
// the next poll, and seen dedup keeps us from re-executing).
func (r *Runner) postResult(ctx context.Context, id string, result *actions.Result) {
	// Translate the wire shape to the API's PostResultRequest.
	body := map[string]any{
		"status":     string(result.Status),
		"denied_for": result.DeniedFor,
	}
	out := map[string]any{
		"stdout":    result.Stdout,
		"stderr":    result.Stderr,
		"exit_code": result.ExitCode,
	}
	if len(result.ResultData) > 0 {
		out["result_data"] = result.ResultData
	}
	js, err := json.Marshal(out)
	if err == nil {
		if result.Phase == actions.PhaseDryRun {
			body["dry_run_output_json"] = string(js)
		} else {
			body["execution_output_json"] = string(js)
		}
	}
	path := fmt.Sprintf("/api/v1/actions/%s/result", id)
	if err := r.postJSON(ctx, path, body, nil); err != nil {
		r.logger.Warn("post result failed", zap.String("request_id", id), zap.Error(err))
		return
	}
	r.logger.Info("posted result",
		zap.String("request_id", id),
		zap.String("status", string(result.Status)),
		zap.String("phase", string(result.Phase)),
	)
}

// ---- HTTP plumbing ---------------------------------------------------------

func (r *Runner) url(path string) string {
	return strings.TrimRight(r.cfg.SquadronURL, "/") + path
}

func (r *Runner) authHeader() string {
	if r.cfg.AuthToken == "" {
		return ""
	}
	return "Bearer " + r.cfg.AuthToken
}

func (r *Runner) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(path), nil)
	if err != nil {
		return err
	}
	if h := r.authHeader(); h != "" {
		req.Header.Set("Authorization", h)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %d %s", req.Method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (r *Runner) postJSON(ctx context.Context, path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url(path), bytes.NewReader(buf))
	if err != nil {
		return err
	}
	if h := r.authHeader(); h != "" {
		req.Header.Set("Authorization", h)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %d %s", req.Method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
