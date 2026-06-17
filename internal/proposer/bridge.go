// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package proposer wires Squadron's v0.29 cost-spike detector to the
// v0.53 AI proposer (internal/ai). The Bridge is a small background
// goroutine that polls open cost-spike events, asks the AI proposer
// to draft a staged rollout for each, and posts the draft through
// the existing rollout service so it lands in pending_approval with
// proposed_by=ai. A human approves; Squadron's rollout engine
// stages the change; the audit trail records the full chain.
//
// Safety properties preserved by design:
//   - The bridge NEVER applies a rollout. It only calls
//     RolloutService.Create, which honors group policy, change
//     windows, and the existing require_approval gate.
//   - AI proposals always set require_approval=true. The validator
//     in internal/ai catches drift; the bridge double-checks here.
//   - In-memory dedup prevents the same spike from generating
//     duplicate proposals while the bridge is running. A future
//     v0.54 will persist this link so restarts are idempotent.
//   - Disabled paths return cleanly: with the AI service disabled
//     or the bridge unconfigured, every tick is a no-op.
//
// Part of Squadron Move 1 (the demo loop). Wired from main.go.
package proposer

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Proposer is the subset of *ai.Service the bridge consumes. Stated
// as an interface so tests can substitute a fake without spinning
// up an HTTP server.
type Proposer interface {
	ProposeFromCostSpike(ctx context.Context, in ai.CostSpikeContext) (*ai.ProposalResult, error)
	Enabled() bool
}

// Store is the slice of the application store the bridge reads
// from. Stated as an interface so tests can substitute a fake.
// Everything we read is by ID or via simple list filters.
type Store interface {
	ListCostSpikeEvents(ctx context.Context, filter types.CostSpikeFilter) ([]*types.CostSpikeEvent, error)
	GetAgent(ctx context.Context, id uuid.UUID) (*types.Agent, error)
	GetLatestConfigForGroup(ctx context.Context, groupID string) (*types.Config, error)
	GetGroup(ctx context.Context, id string) (*types.Group, error)
}

// Rollouts is the subset of services.RolloutService the bridge
// posts to. Stated as an interface for testability.
type Rollouts interface {
	Create(ctx context.Context, input services.RolloutInput) (*services.Rollout, error)
}

// Audit is the subset of services.AuditService the bridge uses to
// emit proposal.* events. Stated as an interface so tests can
// substitute a fake and assert on what was recorded. Nil is a
// valid runtime state (the bridge treats audit as best-effort and
// keeps running if it fails).
type Audit interface {
	Record(ctx context.Context, entry services.AuditEntry) error
}

// Config controls the bridge's cadence and behavior.
type Config struct {
	// PollInterval is how often the bridge sweeps open cost spikes.
	// 30s is a reasonable default: spike detection is already
	// debounced, and humans appreciate fast feedback on demos.
	PollInterval time.Duration
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{PollInterval: 30 * time.Second}
}

// Bridge is the daemon. Construct with New, then Start + Stop.
type Bridge struct {
	proposer Proposer
	store    Store
	rollouts Rollouts
	audit    Audit // optional; nil is fine, events are best-effort
	cfg      Config
	logger   *zap.Logger

	// seen records spike IDs we've already proposed for (or
	// declined on). In-memory only; a restart will retry every
	// open spike. v0.54 will persist this back-pointer on the
	// cost_spike_events row so the link is durable.
	mu   sync.Mutex
	seen map[string]struct{}

	shutdown chan struct{}
	wg       sync.WaitGroup
}

// New constructs a Bridge. proposer, store, and rollouts are
// required. audit may be nil (events are best-effort). logger nil
// is allowed (no-op logger is used).
func New(proposer Proposer, store Store, rollouts Rollouts, audit Audit, cfg Config, logger *zap.Logger) *Bridge {
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultConfig().PollInterval
	}
	return &Bridge{
		proposer: proposer,
		store:    store,
		rollouts: rollouts,
		audit:    audit,
		cfg:      cfg,
		logger:   logger,
		seen:     map[string]struct{}{},
		shutdown: make(chan struct{}),
	}
}

// Start begins the poll loop in a goroutine. Returns immediately.
// Safe to call once per Bridge; subsequent calls are no-ops.
func (b *Bridge) Start(ctx context.Context) {
	if !b.proposer.Enabled() {
		b.logger.Info("AI proposer bridge: ai service disabled, bridge is a no-op")
		return
	}
	b.wg.Add(1)
	go b.run(ctx)
}

// Stop signals the poll loop to exit and waits up to the supplied
// timeout for it to do so. Returns the context error from the wait
// or nil if the loop exited cleanly.
func (b *Bridge) Stop(timeout time.Duration) error {
	close(b.shutdown)
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return context.DeadlineExceeded
	}
}

func (b *Bridge) run(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()
	// Fire one tick immediately so demos don't wait for the full
	// interval before producing visible output.
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

// tick is the per-interval body. Pulled out so tests can drive it
// deterministically without waiting on a real ticker.
func (b *Bridge) tick(ctx context.Context) {
	spikes, err := b.store.ListCostSpikeEvents(ctx, types.CostSpikeFilter{
		Status: "open",
		Limit:  100,
	})
	if err != nil {
		b.logger.Warn("AI proposer bridge: list cost spikes failed", zap.Error(err))
		return
	}
	for _, spike := range spikes {
		if b.markSeen(spike.ID) {
			continue
		}
		b.handleSpike(ctx, spike)
	}
}

// markSeen returns true if the supplied ID is already in the seen
// set (and the bridge should skip it). Otherwise inserts and
// returns false.
func (b *Bridge) markSeen(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[id]; ok {
		return true
	}
	b.seen[id] = struct{}{}
	return false
}

// handleSpike assembles context, calls the proposer, and either
// posts the resulting rollout or logs the decline.
func (b *Bridge) handleSpike(ctx context.Context, spike *types.CostSpikeEvent) {
	cs, ok := b.buildContext(ctx, spike)
	if !ok {
		// buildContext logs the specific reason. We skip without
		// erroring so a poorly-attributed spike doesn't block the
		// rest of the queue.
		return
	}

	result, err := b.proposer.ProposeFromCostSpike(ctx, cs)
	if err != nil {
		b.logger.Warn("AI proposer bridge: proposer failed; skipping spike",
			zap.String("spike_id", spike.ID), zap.Error(err))
		return
	}
	if result.Declined {
		b.logger.Info("AI proposer bridge: proposer declined",
			zap.String("spike_id", spike.ID),
			zap.String("reason", result.Reason))
		b.emitDeclined(ctx, spike, result)
		return
	}

	input := candidateToInput(result, cs.GroupID)
	rollout, err := b.rollouts.Create(ctx, input)
	if err != nil {
		b.logger.Warn("AI proposer bridge: rollout create failed; skipping spike",
			zap.String("spike_id", spike.ID), zap.Error(err))
		return
	}
	b.logger.Info("AI proposer bridge: posted rollout proposal",
		zap.String("spike_id", spike.ID),
		zap.String("rollout_id", rollout.ID),
		zap.String("group_id", cs.GroupID),
		zap.Int("tokens_in", result.TokensIn),
		zap.Int("tokens_out", result.TokensOut))
	b.emitCreated(ctx, spike, rollout, result, cs)
	b.emitEvidenceLinked(ctx, spike, rollout, result)
}

// emitCreated records proposal.created at the cost-spike target.
// The audit timeline groups proposal events on the spike so an
// operator reviewing the spike sees the AI's reasoning, the model,
// and the resulting rollout in one place. Compliance evidence
// trail for NIST AI RMF MAP 4.1 and SOC 2 CC8.1.
func (b *Bridge) emitCreated(ctx context.Context, spike *types.CostSpikeEvent, rollout *services.Rollout, result *ai.ProposalResult, cs ai.CostSpikeContext) {
	if b.audit == nil {
		return
	}
	if err := b.audit.Record(ctx, services.AuditEntry{
		Actor:      "ai-proposer",
		EventType:  "proposal.created",
		TargetType: "cost_spike",
		TargetID:   spike.ID,
		Action:     "created",
		Payload: map[string]any{
			"origin":             "ai",
			"rollout_id":         rollout.ID,
			"group_id":           cs.GroupID,
			"target_config_id":   rollout.TargetConfigID,
			"reasoning_summary":  summarize(result.Reasoning, 240),
			"evidence_count":     len(result.Evidence),
			"model":              result.Model,
			"tokens_in":          result.TokensIn,
			"tokens_out":         result.TokensOut,
			"require_approval":   true,
		},
	}); err != nil {
		b.logger.Warn("AI proposer bridge: proposal.created audit emit failed",
			zap.String("spike_id", spike.ID), zap.Error(err))
	}
}

// emitEvidenceLinked records proposal.evidence_linked with the
// full evidence list attached. One event per proposal carrying the
// list, rather than one event per ref, so the audit log stays
// readable. Compliance evidence trail for NIST AI RMF MEASURE 2.5
// (evidence-based decision tracking).
func (b *Bridge) emitEvidenceLinked(ctx context.Context, spike *types.CostSpikeEvent, rollout *services.Rollout, result *ai.ProposalResult) {
	if b.audit == nil || len(result.Evidence) == 0 {
		return
	}
	refs := make([]map[string]any, len(result.Evidence))
	for i, e := range result.Evidence {
		refs[i] = map[string]any{
			"kind":        e.Kind,
			"id":          e.ID,
			"url":         e.URL,
			"description": e.Description,
		}
	}
	if err := b.audit.Record(ctx, services.AuditEntry{
		Actor:      "ai-proposer",
		EventType:  "proposal.evidence_linked",
		TargetType: "cost_spike",
		TargetID:   spike.ID,
		Action:     "evidence_linked",
		Payload: map[string]any{
			"rollout_id": rollout.ID,
			"evidence":   refs,
		},
	}); err != nil {
		b.logger.Warn("AI proposer bridge: proposal.evidence_linked audit emit failed",
			zap.String("spike_id", spike.ID), zap.Error(err))
	}
}

// emitDeclined records the model's decision not to propose. Useful
// evidence trail even on the negative path: an auditor can see
// that the AI evaluated the spike and chose not to act, with the
// reason captured.
func (b *Bridge) emitDeclined(ctx context.Context, spike *types.CostSpikeEvent, result *ai.ProposalResult) {
	if b.audit == nil {
		return
	}
	if err := b.audit.Record(ctx, services.AuditEntry{
		Actor:      "ai-proposer",
		EventType:  "proposal.declined",
		TargetType: "cost_spike",
		TargetID:   spike.ID,
		Action:     "declined",
		Payload: map[string]any{
			"origin":    "ai",
			"reason":    result.Reason,
			"model":     result.Model,
			"tokens_in": result.TokensIn,
			"tokens_out": result.TokensOut,
		},
	}); err != nil {
		b.logger.Warn("AI proposer bridge: proposal.declined audit emit failed",
			zap.String("spike_id", spike.ID), zap.Error(err))
	}
}

// summarize truncates a long string to a maximum length and adds
// an ellipsis. Used to keep audit payloads small while still
// readable in the timeline. Empty input returns empty.
func summarize(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// buildContext assembles the CostSpikeContext the proposer needs.
// Returns false (and logs the reason) when essential pieces are
// missing; the bridge then skips this spike rather than send a
// malformed prompt.
func (b *Bridge) buildContext(ctx context.Context, spike *types.CostSpikeEvent) (ai.CostSpikeContext, bool) {
	topAgents, topAttributes := parseAttribution(spike.AttributionJSON)
	groupID, groupName := b.inferGroup(ctx, topAgents)
	if groupID == "" {
		b.logger.Info("AI proposer bridge: skipping spike; could not infer group",
			zap.String("spike_id", spike.ID))
		return ai.CostSpikeContext{}, false
	}
	cfg, err := b.store.GetLatestConfigForGroup(ctx, groupID)
	if err != nil || cfg == nil {
		b.logger.Info("AI proposer bridge: skipping spike; no current config for group",
			zap.String("spike_id", spike.ID),
			zap.String("group_id", groupID))
		return ai.CostSpikeContext{}, false
	}
	return ai.CostSpikeContext{
		SpikeID:              spike.ID,
		Signal:               spike.Signal,
		Severity:             spike.Severity,
		BaselineMonthlyUSD:   spike.BaselineMonthlyUSD,
		PeakMonthlyUSD:       spike.PeakMonthlyUSD,
		PeakPctAboveBaseline: spike.PeakPctAboveBaseline,
		StartedAt:            spike.StartedAt,
		TopAgents:            topAgents,
		TopAttributes:        topAttributes,
		GroupID:              groupID,
		GroupName:            groupName,
		// RecentLintFindings + RecentRecommendations left empty for
		// the first cut. v0.54 polish: query audit_events for recent
		// config.lint_evaluated rows on this group, and the
		// recommendations engine for open recs.
	}, true
}

// inferGroup picks the group_id that most of the supplied top
// agents belong to. Ties resolved by sort order so the choice is
// deterministic on retries. Returns ("", "") if no agent has a
// group, so the bridge can skip rather than guess.
func (b *Bridge) inferGroup(ctx context.Context, agentIDs []string) (string, string) {
	if len(agentIDs) == 0 {
		return "", ""
	}
	counts := map[string]int{}
	names := map[string]string{}
	for _, raw := range agentIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		agent, err := b.store.GetAgent(ctx, id)
		if err != nil || agent == nil || agent.GroupID == nil {
			continue
		}
		counts[*agent.GroupID]++
		if agent.GroupName != nil {
			names[*agent.GroupID] = *agent.GroupName
		}
	}
	if len(counts) == 0 {
		return "", ""
	}
	// Pick the most common; tiebreak by group ID for determinism.
	type pair struct {
		id    string
		count int
	}
	var pairs []pair
	for k, v := range counts {
		pairs = append(pairs, pair{id: k, count: v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].id < pairs[j].id
	})
	winner := pairs[0].id
	return winner, names[winner]
}

// parseAttribution lifts top_agents and top_attributes out of the
// cost spike's AttributionJSON. Tolerant: returns empty slices on
// any decode error so a malformed payload doesn't block the bridge.
func parseAttribution(raw string) (agents, attributes []string) {
	if raw == "" {
		return nil, nil
	}
	type body struct {
		TopAgents     []string `json:"top_agents"`
		TopAttributes []string `json:"top_attributes"`
	}
	var b body
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, nil
	}
	return b.TopAgents, b.TopAttributes
}

// candidateToInput maps the proposer's RolloutInputCandidate plus
// the natural-language reasoning into the service-layer
// RolloutInput shape. Sets ProposedBy=ai and forces
// RequireApproval=true regardless of what the model said.
func candidateToInput(result *ai.ProposalResult, expectedGroupID string) services.RolloutInput {
	p := result.Proposal
	stages := make([]services.RolloutStage, len(p.Stages))
	for i, st := range p.Stages {
		stages[i] = services.RolloutStage{
			Mode:          services.RolloutStageMode(st.Mode),
			Percentage:    st.Percentage,
			LabelSelector: copyMap(st.LabelSelector),
			DwellSeconds:  st.DwellSeconds,
		}
	}
	evidence := make([]services.EvidenceRef, len(result.Evidence))
	for i, e := range result.Evidence {
		evidence[i] = services.EvidenceRef{
			Kind:        e.Kind,
			ID:          e.ID,
			URL:         e.URL,
			Description: e.Description,
		}
	}
	return services.RolloutInput{
		Name:            p.Name,
		GroupID:         expectedGroupID, // ignore model's value; trust the bridge's inference
		TargetConfigID:  p.TargetConfigID,
		Stages:          stages,
		AbortCriteria:   servicesAbortCriteria(p.AbortCriteria),
		NotificationURL: p.NotificationURL,
		// AI proposals always force require_approval=true. This is
		// in the prompt and in the candidate; we set it explicitly
		// here too so a prompt regression cannot bypass approval.
		RequireApproval:   true,
		ProposedBy:        services.RolloutProposedByAI,
		ProposalReasoning: result.Reasoning,
		EvidenceRefs:      evidence,
		// RequestedBy is left empty by the bridge. The audit trail
		// records the AI as the originator via ProposedBy; future
		// SDK clients can supply a "requested_by_agent_id" if they
		// want that breadcrumb too.
	}
}

func servicesAbortCriteria(c ai.AbortCriteriaCandidate) services.RolloutAbortCriteria {
	return services.RolloutAbortCriteria{
		MaxDriftedAgents:           c.MaxDriftedAgents,
		MaxErrorLogsPerMinute:      c.MaxErrorLogsPerMinute,
		MinDwellSecondsBeforeAbort: c.MinDwellSecondsBeforeAbort,
	}
}

func copyMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Ensure the package compiles against the applicationstore alias
// surface so a developer reading this package can see which import
// matters. We pin Group because we use it through the Store
// interface.
var _ = applicationstore.Group{}
