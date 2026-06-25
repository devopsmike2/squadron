// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package incidents wires Squadron's v0.53 action runner to the v0.54
// AI incident drafter (internal/ai.DraftIncidentFromAction). The
// Bridge is a small background goroutine that polls completed action
// requests, asks the drafter for a postmortem-style ticket draft,
// and persists each result as an IncidentDraft the operator reviews
// in the UI.
//
// Safety properties preserved by design:
//
//   - The bridge NEVER publishes a draft to an external ticketing
//     system. It only persists; the operator decides whether to
//     publish via the provider plug-in (the API endpoint that
//     lands in the next chunk).
//   - The bridge dedups on action_request_id at the storage layer
//     (GetIncidentDraftByActionRequestID). The in-memory seen set
//     is a fast path; the storage check is the source of truth on
//     restart.
//   - When the drafter declines, the bridge logs and records an
//     incident.draft_declined audit event but persists nothing. A
//     dry-run that produced no signal should not create a ticket.
//   - Disabled paths return cleanly: with the AI service disabled or
//     the bridge unconfigured, every tick is a no-op.
//
// Part of Squadron Move 3 (auto-drafted incident ticket). Wired
// from main.go in the next chunk.
package incidents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Drafter is the subset of *ai.Service the bridge consumes. Stated
// as an interface so tests can substitute a fake without spinning
// up an HTTP server.
type Drafter interface {
	DraftIncidentFromAction(ctx context.Context, in ai.IncidentDraftInput) (*ai.IncidentDraftResult, error)
	Enabled() bool
}

// Store is the slice of the application store the bridge reads from
// and writes to. The bridge lists completed action requests, looks
// up the (optional) originating rollout's data through rollouts and
// the group context through GetGroup, and persists drafts.
type Store interface {
	ListActionRequests(ctx context.Context, filter types.ActionRequestFilter) ([]*types.ActionRequest, error)
	GetActionRequest(ctx context.Context, id string) (*types.ActionRequest, error)
	GetIncidentDraftByActionRequestID(ctx context.Context, actionRequestID string) (*types.IncidentDraft, error)
	CreateIncidentDraft(ctx context.Context, d *types.IncidentDraft) error
	GetGroup(ctx context.Context, id string) (*types.Group, error)
}

// Rollouts is the subset of services.RolloutService the bridge reads
// from. The bridge looks up the originating rollout (when the action
// has a ProposalID) so the ticket can carry the trigger summary and
// the AI proposer's reasoning forward.
type Rollouts interface {
	Get(ctx context.Context, id string) (*services.Rollout, error)
}

// Audit is the subset of services.AuditService the bridge uses to
// emit incident.* events. Stated as an interface so tests can
// substitute a fake and assert. Nil is a valid runtime state; the
// bridge treats audit as best-effort and keeps running if it fails.
type Audit interface {
	Record(ctx context.Context, entry services.AuditEntry) error
}

// Config controls the bridge's cadence and behavior.
type Config struct {
	// PollInterval is how often the bridge sweeps recently-completed
	// action requests. 60s is a reasonable default: action runs are
	// not high-frequency events, and the dedup at the storage layer
	// means a long interval just means the operator waits a bit
	// longer for a draft to appear in the inbox.
	PollInterval time.Duration

	// Lookback is the maximum age of an action request the bridge
	// will draft for. Older requests are assumed to have already
	// been handled (the bridge missed them due to downtime, or
	// they predate Squadron 0.54). Defaults to 24 hours; a
	// long-running install can crank this up at boot to catch up.
	Lookback time.Duration
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		PollInterval: 60 * time.Second,
		Lookback:     24 * time.Hour,
	}
}

// Bridge is the daemon. Construct with New, then Start + Stop.
type Bridge struct {
	drafter  Drafter
	store    Store
	rollouts Rollouts // optional; bridge functions without it
	audit    Audit    // optional; nil is fine, events are best-effort
	cfg      Config
	logger   *zap.Logger

	// seen records action_request_ids we've already drafted for or
	// declined on. In-memory only; the storage-side dedup via
	// GetIncidentDraftByActionRequestID is the source of truth on
	// restart.
	mu   sync.Mutex
	seen map[string]struct{}

	shutdown chan struct{}
	wg       sync.WaitGroup

	// now is overridable for tests that need to drive the clock.
	now func() time.Time
}

// New constructs a Bridge. drafter and store are required. rollouts,
// audit, and logger may all be nil. cfg uses DefaultConfig when
// zero.
func New(drafter Drafter, store Store, rollouts Rollouts, audit Audit, cfg Config, logger *zap.Logger) (*Bridge, error) {
	if drafter == nil {
		return nil, fmt.Errorf("incidents.New: drafter is required")
	}
	if store == nil {
		return nil, fmt.Errorf("incidents.New: store is required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultConfig().PollInterval
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = DefaultConfig().Lookback
	}
	return &Bridge{
		drafter:  drafter,
		store:    store,
		rollouts: rollouts,
		audit:    audit,
		cfg:      cfg,
		logger:   logger,
		seen:     map[string]struct{}{},
		shutdown: make(chan struct{}),
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// SetClock overrides the time source. Tests use this to drive the
// lookback window without sleeping.
func (b *Bridge) SetClock(now func() time.Time) { b.now = now }

// Start begins the poll loop. Returns immediately. Run Stop to
// shut down cleanly.
func (b *Bridge) Start(ctx context.Context) {
	b.wg.Add(1)
	go b.run(ctx)
}

// Stop signals the loop to exit and waits for it. Idempotent.
func (b *Bridge) Stop() {
	select {
	case <-b.shutdown:
		// already closed
	default:
		close(b.shutdown)
	}
	b.wg.Wait()
}

// Tick runs one poll cycle synchronously. Exposed for tests so they
// do not have to depend on the goroutine's wall-clock cadence.
func (b *Bridge) Tick(ctx context.Context) { b.tick(ctx) }

func (b *Bridge) run(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	// First tick right away so demos do not wait the full interval.
	b.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.shutdown:
			return
		case <-ticker.C:
			b.tick(ctx)
		}
	}
}

// tick performs one poll cycle. Errors are logged but do not stop
// the loop; transient storage failures should not take the bridge
// down.
func (b *Bridge) tick(ctx context.Context) {
	if !b.drafter.Enabled() {
		// AI disabled; nothing to do.
		return
	}

	// List recently-completed action requests of any successful or
	// failed status; denied stays out because Squadron refused to
	// run, which is its own audit story, not a ticket worth drafting
	// without operator framing.
	cutoff := b.now().Add(-b.cfg.Lookback)
	for _, status := range []string{"success", "failure"} {
		reqs, err := b.store.ListActionRequests(ctx, types.ActionRequestFilter{
			Status: status,
			Limit:  100,
		})
		if err != nil {
			b.logger.Warn("incidents: list action requests failed", zap.Error(err), zap.String("status", status))
			continue
		}
		for _, req := range reqs {
			if req.CompletedAt == nil || req.CompletedAt.Before(cutoff) {
				continue
			}
			if b.alreadySeen(req.ID) {
				continue
			}
			b.handleOne(ctx, req)
			b.markSeen(req.ID)
		}
	}
}

// handleOne is the per-request flow: storage dedup, build input,
// call drafter, persist draft (or record decline), emit audit.
func (b *Bridge) handleOne(ctx context.Context, req *types.ActionRequest) {
	log := b.logger.With(
		zap.String("action_request_id", req.ID),
		zap.String("status", req.Status),
	)

	// Storage-side dedup. The seen map is fast but in-memory; the
	// storage check survives restarts.
	existing, err := b.store.GetIncidentDraftByActionRequestID(ctx, req.ID)
	if err != nil {
		log.Warn("incidents: lookup existing draft failed", zap.Error(err))
		return
	}
	if existing != nil {
		// Already drafted on a previous tick or a previous boot.
		return
	}

	in := b.buildDraftInput(ctx, req)

	res, err := b.drafter.DraftIncidentFromAction(ctx, in)
	if err != nil {
		log.Warn("incidents: drafter call failed", zap.Error(err))
		// Mark seen so we don't retry on every tick. A future fix
		// can clear this if the failure pattern needs retry.
		return
	}

	if res.Declined {
		log.Info("incidents: drafter declined", zap.String("reason", res.Reason))
		b.recordAudit(ctx, services.AuditEntry{
			EventType:  services.AuditEventIncidentDraftDeclined,
			TargetType: services.AuditTargetIncidentDraft,
			TargetID:   req.ID,
			Action:     "declined",
			Payload: map[string]any{
				"action_request_id": req.ID,
				"reason":            res.Reason,
				"model":             res.Model,
				"tokens_in":         res.TokensIn,
				"tokens_out":        res.TokensOut,
			},
		})
		return
	}

	draft := &types.IncidentDraft{
		ID:               uuid.NewString(),
		ActionRequestID:  req.ID,
		RolloutID:        req.ProposalID, // proposal_id on the action carries the rollout
		Status:           "draft",
		Title:            res.Title,
		BodyMarkdown:     ai.RenderIncidentMarkdown(res, in),
		DraftContentJSON: marshalOrEmpty(res),
	}
	if err := b.store.CreateIncidentDraft(ctx, draft); err != nil {
		log.Warn("incidents: persist draft failed", zap.Error(err))
		return
	}
	log.Info("incidents: drafted",
		zap.String("draft_id", draft.ID),
		zap.String("title", res.Title),
	)
	b.recordAudit(ctx, services.AuditEntry{
		EventType:  services.AuditEventIncidentDrafted,
		TargetType: services.AuditTargetIncidentDraft,
		TargetID:   draft.ID,
		Action:     "drafted",
		Payload: map[string]any{
			"action_request_id": req.ID,
			"rollout_id":        req.ProposalID,
			"title":             res.Title,
			"model":             res.Model,
			"tokens_in":         res.TokensIn,
			"tokens_out":        res.TokensOut,
		},
	})
}

// buildDraftInput translates an action request and (optional)
// rollout context into the structured input the drafter consumes.
// Errors looking up the rollout or group are non-fatal; the input is
// still useful without them, just less rich.
func (b *Bridge) buildDraftInput(ctx context.Context, req *types.ActionRequest) ai.IncidentDraftInput {
	in := ai.IncidentDraftInput{
		ActionRequestID: req.ID,
		RolloutID:       req.ProposalID,
		ActionType:      req.ActionType,
		Phase:           req.Phase,
		Status:          req.Status,
		StateChanged:    req.Phase == "execute" && req.Status == "success",
		ActionSummary:   summarizeAction(req),
		OutcomeBullets:  outcomeBullets(req),
		AuditReferences: map[string]string{
			"action_request": req.ID,
		},
	}
	if req.StartedAt != nil {
		in.StartedAt = *req.StartedAt
	}
	if req.CompletedAt != nil {
		in.CompletedAt = *req.CompletedAt
	}

	if req.ProposalID != "" && b.rollouts != nil {
		if r, err := b.rollouts.Get(ctx, req.ProposalID); err == nil && r != nil {
			in.TriggerSummary = r.Name
			in.ProposalReasoning = r.ProposalReasoning
			in.AuditReferences["rollout"] = r.ID
			if g, err := b.store.GetGroup(ctx, r.GroupID); err == nil && g != nil {
				in.GroupName = g.Name
			}
		}
	}
	return in
}

// summarizeAction renders a single-line description of the action
// without exposing operator-sensitive payload. Parameters are
// already type-validated and constrained at dispatch time
// (capability matcher), but we keep the renderer defensive here so
// any new action types added later get a reasonable default.
func summarizeAction(req *types.ActionRequest) string {
	switch req.ActionType {
	case "restart-systemd-service":
		var p struct {
			UnitName        string `json:"unit_name"`
			RestartStrategy string `json:"restart_strategy"`
		}
		if err := json.Unmarshal([]byte(req.ParametersJSON), &p); err == nil {
			strategy := p.RestartStrategy
			if strategy == "" {
				strategy = "restart"
			}
			return fmt.Sprintf("systemctl %s %s", strategy, p.UnitName)
		}
	}
	return req.ActionType
}

// outcomeBullets pulls a short list of factual outcome lines out of
// the result data JSON. We deliberately do NOT include stdout — that
// can contain a lot, including operator-sensitive content. The
// operator can drill into the audit timeline for the unredacted view.
func outcomeBullets(req *types.ActionRequest) []string {
	raw := req.ExecutionOutputJSON
	if raw == "" {
		raw = req.DryRunOutputJSON
	}
	if raw == "" {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil
	}
	var bullets []string
	if v, ok := parsed["ran_command"].(string); ok && v != "" {
		bullets = append(bullets, "ran_command: "+v)
	}
	if v, ok := parsed["planned_command"].(string); ok && v != "" {
		bullets = append(bullets, "planned_command: "+v)
	}
	if v, ok := parsed["unit_name"].(string); ok && v != "" {
		bullets = append(bullets, "unit_name: "+v)
	}
	if v, ok := parsed["strategy"].(string); ok && v != "" {
		bullets = append(bullets, "strategy: "+v)
	}
	if v, ok := parsed["exit_code"].(float64); ok {
		bullets = append(bullets, fmt.Sprintf("exit_code: %d", int(v)))
	}
	return bullets
}

// marshalOrEmpty marshals the drafter result into JSON for storage;
// returns "" if marshaling fails so the column stays NULL-safe.
func marshalOrEmpty(res *ai.IncidentDraftResult) string {
	if res == nil {
		return ""
	}
	b, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// recordAudit is best-effort: failures are logged but do not stop
// the bridge.
func (b *Bridge) recordAudit(ctx context.Context, entry services.AuditEntry) {
	if b.audit == nil {
		return
	}
	if err := b.audit.Record(ctx, entry); err != nil {
		b.logger.Warn("incidents: audit record failed", zap.Error(err), zap.String("event_type", entry.EventType))
	}
}

func (b *Bridge) alreadySeen(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.seen[id]
	return ok
}

func (b *Bridge) markSeen(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen[id] = struct{}{}
}
