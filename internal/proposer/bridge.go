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
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/proposer/verdictprompt"
	"github.com/devopsmike2/squadron/internal/proposer/verdictsel"
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
	// v0.89.17 (#633) — prior-verdicts few-shot loop. The bridge
	// sweeps this on every cost-spike proposal to assemble §5's
	// selection (≤2 approved + ≤2 rejected within 30 days, newest
	// first). Cold-start (zero rows) is the empty-block path.
	ListAIVerdictsForGroup(ctx context.Context, groupID string, since time.Time, limit int) ([]*types.Rollout, error)
}

// Rollouts is the subset of services.RolloutService the bridge
// posts to. Stated as an interface for testability.
type Rollouts interface {
	Create(ctx context.Context, input services.RolloutInput) (*services.Rollout, error)
	// v0.79 — plan create. Bridge dispatches to this when the
	// proposer emits ProposalKindPlan. Returns the assigned plan
	// id + the created step rollouts in step order.
	CreatePlan(ctx context.Context, steps []services.RolloutInput) ([]*services.Rollout, string, error)
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

	// v0.89.17 (#633) / v0.89.35 (#654) — prior-verdicts few-shot
	// loop. Pull the recent operator decisions on AI-originated
	// rollouts for this group, run them through the shared
	// verdictsel.Select policy, and render the prompt block via
	// the shared verdictprompt.Render. The IDs flow into the audit
	// payload below; the formatted block flows into the prompt
	// via cs.VerdictBlock. Empty block on cold start, opt-out, or
	// recency-window empty — those cases all short-circuit inside
	// assembleVerdicts (returning four zero values) or inside
	// verdictprompt.Render (empty pool → empty string).
	approved, rejected, verdictIDs, verdictIDsByState, vErr := b.assembleVerdicts(ctx, cs.GroupID)
	if vErr != nil {
		// Non-fatal: a verdicts query failure shouldn't sink the
		// whole spike. Log and proceed with empty examples — the
		// proposer still works, it just loses the learning signal
		// for this one tick.
		b.logger.Warn("AI proposer bridge: assembleVerdicts failed; proceeding without examples",
			zap.String("group_id", cs.GroupID), zap.Error(vErr))
		approved = nil
		rejected = nil
		verdictIDs = nil
		verdictIDsByState = nil
	}
	cs.VerdictBlock = verdictprompt.Render(approved, rejected, verdictprompt.RenderOpts{
		Surface:         verdictprompt.SurfaceCostSpike,
		Now:             time.Now().UTC(),
		Header:          verdictprompt.CostSpikeHeader,
		InstructionTail: verdictprompt.CostSpikeInstructionTail,
	})

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

	// v0.79 — dispatch on result.Kind. Empty Kind decodes as
	// rollout (backwards compat with pre-v0.79 model outputs);
	// plan kind invokes the v0.78 plan create path with inline
	// snippets per step.
	switch result.Kind {
	case ai.ProposalKindPlan:
		b.handlePlanSpike(ctx, spike, result, cs, verdictIDs, verdictIDsByState)
	case ai.ProposalKindRollout, "":
		b.handleRolloutSpike(ctx, spike, result, cs, verdictIDs, verdictIDsByState)
	default:
		b.logger.Warn("AI proposer bridge: unknown proposal kind; skipping spike",
			zap.String("spike_id", spike.ID),
			zap.String("kind", string(result.Kind)))
		return
	}
}

// tenantForGroup returns ctx wrapped with the owning group's tenant so
// the per-owner write (rollout / plan) lands in the group's tenant
// rather than the bridge loop's system default. ADR 0013 D6-a. The
// group is loaded under the (system) ctx the bridge runs under, so the
// read returns the row regardless of tenant. Empty/absent anchor
// (group deleted, no tenant on disk) → leave ctx unstamped so the
// write falls back to `default` — the legit fallback. Inert in OSS
// where every group resolves to `default` and WithTenant("default")
// is a no-op.
func (b *Bridge) tenantForGroup(ctx context.Context, groupID string) context.Context {
	if groupID == "" {
		return ctx
	}
	group, err := b.store.GetGroup(ctx, groupID)
	if err != nil || group == nil || group.TenantID == "" {
		return ctx
	}
	return identity.WithTenant(ctx, group.TenantID)
}

// handleRolloutSpike is the v0.58 path — single rollout create.
// Extracted from handleSpike during the v0.79 refactor so the
// dispatch branch reads cleanly. verdictIDs flows from
// assembleVerdicts up at handleSpike and is stamped onto the
// proposal.created audit payload (v0.89.17 #633).
func (b *Bridge) handleRolloutSpike(ctx context.Context, spike *types.CostSpikeEvent, result *ai.ProposalResult, cs ai.CostSpikeContext, verdictIDs []string, verdictIDsByState map[string][]string) {
	input := candidateToInput(result, cs.GroupID)
	// ADR 0013 D6-a: anchor the write on the OWNING group's tenant, not
	// the loop's system ctx (which would resolve to `default`). NB anchor
	// on the GROUP — an AI rollout is itself default-created, so the
	// rollout's own tenant is meaningless here. Inert in OSS (every group
	// is `default`; the stamp is a no-op).
	writeCtx := b.tenantForGroup(ctx, cs.GroupID)
	rollout, err := b.rollouts.Create(writeCtx, input)
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
	b.emitCreated(ctx, spike, rollout, result, cs, verdictIDs, verdictIDsByState)
	b.emitEvidenceLinked(ctx, spike, rollout, result)
}

// handlePlanSpike is the v0.79 path — multi step plan create. Wraps
// each PlanStepCandidate into a services.RolloutInput with the
// step's inline snippet, then calls CreatePlan which materializes
// configs + creates rollouts in one server-side transaction.
//
// Failure modes:
//   - CreatePlan returns an error if any step's snippet fails lint
//     or any storage write fails. We log + skip the spike; the
//     audit event records the decline reason via emitDeclined.
//   - On success, audit events fire against the first step's
//     rollout id so the timeline anchors the plan to a concrete row.
func (b *Bridge) handlePlanSpike(ctx context.Context, spike *types.CostSpikeEvent, result *ai.ProposalResult, cs ai.CostSpikeContext, verdictIDs []string, verdictIDsByState map[string][]string) {
	steps := candidateToPlanInputs(result, cs.GroupID)
	// ADR 0013 D6-a: anchor the write on the owning group's tenant (see
	// handleRolloutSpike). Inert in OSS.
	writeCtx := b.tenantForGroup(ctx, cs.GroupID)
	createdSteps, planID, err := b.rollouts.CreatePlan(writeCtx, steps)
	if err != nil {
		b.logger.Warn("AI proposer bridge: plan create failed; skipping spike",
			zap.String("spike_id", spike.ID), zap.Error(err))
		return
	}
	if len(createdSteps) == 0 {
		b.logger.Warn("AI proposer bridge: plan create returned no steps; skipping spike",
			zap.String("spike_id", spike.ID))
		return
	}
	b.logger.Info("AI proposer bridge: posted plan proposal",
		zap.String("spike_id", spike.ID),
		zap.String("plan_id", planID),
		zap.Int("step_count", len(createdSteps)),
		zap.String("group_id", cs.GroupID),
		zap.Int("tokens_in", result.TokensIn),
		zap.Int("tokens_out", result.TokensOut))
	// Reuse the per-rollout audit emission against the first
	// step's rollout. proposal.created + evidence.linked events
	// anchor on step 0; the plan.created event already fires from
	// services.CreatePlan itself with the plan_id payload.
	b.emitCreated(ctx, spike, createdSteps[0], result, cs, verdictIDs, verdictIDsByState)
	b.emitEvidenceLinked(ctx, spike, createdSteps[0], result)
}

// emitCreated records proposal.created at the cost-spike target.
// The audit timeline groups proposal events on the spike so an
// operator reviewing the spike sees the AI's reasoning, the model,
// and the resulting rollout in one place. Compliance evidence
// trail for NIST AI RMF MAP 4.1 and SOC 2 CC8.1.
func (b *Bridge) emitCreated(ctx context.Context, spike *types.CostSpikeEvent, rollout *services.Rollout, result *ai.ProposalResult, cs ai.CostSpikeContext, verdictIDs []string, verdictIDsByState map[string][]string) {
	if b.audit == nil {
		return
	}
	// v0.89.17 (#633) — verdict_examples_used carries the list of
	// rollout IDs from prior operator verdicts that informed this
	// proposal. ALWAYS present (never omitted) — empty array on
	// cold start so SIEM consumers can filter on the empty slice
	// to find cold-start cases. Spec §8.
	examplesUsed := verdictIDs
	if examplesUsed == nil {
		examplesUsed = []string{}
	}
	payload := map[string]any{
		"origin":                "ai",
		"rollout_id":            rollout.ID,
		"group_id":              cs.GroupID,
		"target_config_id":      rollout.TargetConfigID,
		"reasoning_summary":     summarize(result.Reasoning, 240),
		"evidence_count":        len(result.Evidence),
		"model":                 result.Model,
		"tokens_in":             result.TokensIn,
		"tokens_out":            result.TokensOut,
		"require_approval":      true,
		"verdict_examples_used": examplesUsed,
	}
	// v0.89.37 (#657 Stream 55, #531 slice 2 chunk 6) — extended
	// audit payload with verdict_examples_used_by_state. The new
	// field partitions the verdict_examples_used array into per-
	// state buckets so SIEM consumers + the humanizer can read the
	// approved/rejected mix without an N+1 lookup against the
	// rollouts table at humanize time. Spec §8 (c). Omitted entirely
	// when both buckets are empty so cold-start audit rows stay byte-
	// for-byte identical to the v0.89.17 shape (the existing
	// verdict_examples_used: [] field is the cold-start signal).
	if hasAnyByState(verdictIDsByState) {
		payload["verdict_examples_used_by_state"] = verdictIDsByState
	}
	if err := b.audit.Record(ctx, services.AuditEntry{
		Actor:      "ai-proposer",
		EventType:  "proposal.created",
		TargetType: "cost_spike",
		TargetID:   spike.ID,
		Action:     "created",
		Payload:    payload,
	}); err != nil {
		b.logger.Warn("AI proposer bridge: proposal.created audit emit failed",
			zap.String("spike_id", spike.ID), zap.Error(err))
	}
}

// hasAnyByState returns true when the supplied by-state map has any
// non-empty bucket. Used by emitCreated to gate emission of the
// verdict_examples_used_by_state field — cold-start audit rows
// (every bucket empty / nil map) omit the field so the v0.89.17
// payload shape is preserved byte-for-byte for SIEM consumers.
func hasAnyByState(m map[string][]string) bool {
	for _, ids := range m {
		if len(ids) > 0 {
			return true
		}
	}
	return false
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
			"origin":     "ai",
			"reason":     result.Reason,
			"model":      result.Model,
			"tokens_in":  result.TokensIn,
			"tokens_out": result.TokensOut,
		},
	}); err != nil {
		b.logger.Warn("AI proposer bridge: proposal.declined audit emit failed",
			zap.String("spike_id", spike.ID), zap.Error(err))
	}
}

// emitSkipped records proposal.skipped when buildContext refuses to
// call the LLM at all because the supporting context is incomplete.
// The reason argument is a stable short code ("group_inference_failed"
// or "missing_current_config") so the audit timeline can render a
// consistent badge and the audit-explain endpoint has a known
// vocabulary to narrate. Added in v0.59 after the v0.58 stress test
// surfaced these pre-LLM refusals as a blind spot.
//
// Compliance evidence trail: NIST AI RMF MEASURE 2.3 (transparency
// of model invocation decisions).
func (b *Bridge) emitSkipped(ctx context.Context, spike *types.CostSpikeEvent, reason string, details map[string]any) {
	if b.audit == nil {
		return
	}
	payload := map[string]any{
		"origin": "ai",
		"reason": reason,
	}
	for k, v := range details {
		payload[k] = v
	}
	if err := b.audit.Record(ctx, services.AuditEntry{
		Actor:      "ai-proposer",
		EventType:  services.AuditEventProposalSkipped,
		TargetType: "cost_spike",
		TargetID:   spike.ID,
		Action:     "skipped",
		Payload:    payload,
	}); err != nil {
		b.logger.Warn("AI proposer bridge: proposal.skipped audit emit failed",
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
		b.emitSkipped(ctx, spike, "group_inference_failed", map[string]any{
			"top_agents_count":     len(topAgents),
			"top_attributes_count": len(topAttributes),
			"attribution_present":  spike.AttributionJSON != "",
		})
		return ai.CostSpikeContext{}, false
	}
	cfg, err := b.store.GetLatestConfigForGroup(ctx, groupID)
	if err != nil || cfg == nil {
		b.logger.Info("AI proposer bridge: skipping spike; no current config for group",
			zap.String("spike_id", spike.ID),
			zap.String("group_id", groupID))
		b.emitSkipped(ctx, spike, "missing_current_config", map[string]any{
			"group_id":   groupID,
			"group_name": groupName,
		})
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

// candidateToPlanInputs maps the proposer's plan candidate (N
// PlanStepCandidates) into the []services.RolloutInput slice that
// services.RolloutService.CreatePlan accepts. v0.79.
//
// Each step's InlineConfigSnippet flows through to the materialized
// config. ProposedBy=ai, ProposalReasoning, and EvidenceRefs are
// stamped on step 0 only — the per-step provenance lives on the
// rollout records the engine creates from CreatePlan.
func candidateToPlanInputs(result *ai.ProposalResult, expectedGroupID string) []services.RolloutInput {
	steps := result.Plan.Steps
	out := make([]services.RolloutInput, 0, len(steps))
	evidence := make([]services.EvidenceRef, len(result.Evidence))
	for i, e := range result.Evidence {
		evidence[i] = services.EvidenceRef{
			Kind:        e.Kind,
			ID:          e.ID,
			URL:         e.URL,
			Description: e.Description,
		}
	}
	for i, st := range steps {
		// v0.89.14 (#630) — kind=action steps follow a separate
		// shape: no stages, no inline snippet, no abort
		// criteria; the Action block carries runner id +
		// action type + parameters + timeout. CreatePlan
		// recognizes kind=action and routes to the action-step
		// create path. Empty kind decodes as rollout for back-
		// compat with v0.79-v0.89.13 model outputs.
		kind := st.Kind
		if kind == "" {
			kind = services.StepKindRollout
		}
		if kind == services.StepKindAction {
			input := services.RolloutInput{
				Name:            st.Name,
				GroupID:         expectedGroupID,
				Kind:            services.StepKindAction,
				RequireApproval: i == 0 && st.RequireApproval,
				ProposedBy:      services.RolloutProposedByAI,
			}
			if st.Action != nil {
				input.Action = &services.ActionStepSpec{
					RunnerID:       st.Action.RunnerID,
					ActionType:     st.Action.ActionType,
					Parameters:     st.Action.Parameters,
					TimeoutSeconds: st.Action.TimeoutSeconds,
				}
			}
			if i == 0 {
				input.ProposalReasoning = result.Reasoning
				input.EvidenceRefs = evidence
				input.RequestedBy = "ai-proposer"
			}
			out = append(out, input)
			continue
		}
		stages := make([]services.RolloutStage, len(st.Stages))
		for j, s := range st.Stages {
			stages[j] = services.RolloutStage{
				Mode:          services.RolloutStageMode(s.Mode),
				Percentage:    s.Percentage,
				LabelSelector: copyMap(s.LabelSelector),
				DwellSeconds:  s.DwellSeconds,
			}
		}
		input := services.RolloutInput{
			Name:                st.Name,
			GroupID:             expectedGroupID, // ignore model's value; trust the bridge's inference
			InlineConfigSnippet: st.InlineConfigSnippet,
			Stages:              stages,
			AbortCriteria:       servicesAbortCriteria(st.AbortCriteria),
			// v0.79 — step 0 keeps the model's RequireApproval (the
			// plan's gate). Steps 1..N have it forced to false by
			// services.CreatePlan itself; setting it here would be
			// redundant but harmless. Mirroring the model's
			// intent for step 0 only keeps the code path tight.
			RequireApproval: i == 0 && st.RequireApproval,
			ProposedBy:      services.RolloutProposedByAI,
		}
		// Provenance fields fire on step 0 so the plan's approval
		// drawer surfaces the reasoning + evidence in one place.
		// Steps 1..N inherit the plan grouping; SIEM consumers can
		// fan out via the plan_id.
		if i == 0 {
			input.ProposalReasoning = result.Reasoning
			input.EvidenceRefs = evidence
			// v0.81.4 — set RequestedBy on step 0 so the
			// plan.created audit event lands with a meaningful
			// actor. services.CreatePlan emits plan.created with
			// Actor: steps[0].RequestedBy; without this the audit
			// row had actor="" while the same-instant
			// proposal.created event used "ai-proposer" (#546).
			// "ai-proposer" matches the actor string the bridge
			// uses for its own audit emissions (proposal.created,
			// proposal.evidence_linked, proposal.declined,
			// proposal.skipped) so SIEM consumers see a single
			// consistent actor for every AI-originated event.
			input.RequestedBy = "ai-proposer"
		}
		out = append(out, input)
	}
	return out
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

// assembleVerdicts pulls recent operator verdicts on AI-originated
// rollouts for the supplied group, runs them through the shared
// verdictsel.Select policy, and returns:
//   - approved: the curated approved/merged-state slice (cost-spike
//     side only emits StateApproved here),
//   - rejected: the curated rejected-state slice,
//   - exampleIDs: the audit-payload list of rollout IDs in the order
//     they appear in the rendered output (rejected first, then
//     approved, matching the prompt's emphasis-on-rejection ordering).
//
// v0.89.35 (#654) refactor: this function used to format the prompt
// block inline; that responsibility now lives in
// internal/proposer/verdictprompt. assembleVerdicts is the pure
// selection step — Render is invoked at the call site.
//
// Selection policy per docs/proposals/531-proposer-learning-slice2.md
// §6, driven by the verdictsel.Default* constants:
//   - same group only,
//   - since = now - DefaultWindow (30d hard-cliff),
//   - hot/cold tier inside the window (DefaultHotWindow = 7d),
//   - DefaultMaxTotal=4 across both buckets, DefaultMaxPerKind=2
//     inside each bucket,
//   - PreferNeg=true: rejection bucket fills first (cost-spike
//     biases toward rejection signal),
//   - cold start (zero matching rows) yields zero values across all
//     three slices — the prompt block omits entirely and the audit
//     array is empty.
//
// Opt-out: when Group.LearnFromVerdicts==false the function short-
// circuits before the storage query and returns four zero values
// regardless of stored verdicts. The prompt builder sees no examples
// block; the audit row carries the empty array (not omitted) so
// SIEM consumers can still detect the proposal happened.
//
// Redaction + truncation: both happen downstream in
// verdictprompt.Render via its reasonField helper (240-char cap
// with ai.RedactSecrets). assembleVerdicts passes raw reasoning +
// approval notes through; nothing in this function reads the
// rollout's StagesJSON or TargetConfigID — inline config bodies
// stay out of the prompt block by construction.
//
// v0.89.17 (#633), refactored v0.89.35 (#654), extended v0.89.37
// (#657 Stream 55, #531 slice 2 chunk 6) to also return the per-
// state ID bucket map that feeds the audit payload's
// verdict_examples_used_by_state field. The map is keyed by the
// cost-spike-surface state strings ("approved", "rejected"); other
// state strings are intentionally skipped (defensive — the cost-
// spike storage layer only writes those two states today, but the
// switch keeps a future StateMerged/StateClosedNotMerged row out of
// the cost-spike payload).
func (b *Bridge) assembleVerdicts(ctx context.Context, groupID string) (approved, rejected []verdictsel.Verdict, exampleIDs []string, exampleIDsByState map[string][]string, err error) {
	if groupID == "" {
		return nil, nil, nil, nil, nil
	}
	// Per-group opt-out check. GetGroup may return (nil, nil) when
	// the group has been deleted but a cost-spike row still
	// references it. Treat that the same as "no examples": there's
	// nothing meaningful to read off a non-existent group. The
	// opt-out short-circuit MUST run before any storage query so
	// the v0.89.18 opt-out invariant + the v0.89.26
	// TestExcludeFromLearning_GroupOptOutStillRespected pin both
	// hold: the fake's lastVerdictsGroupID must stay empty when
	// LearnFromVerdicts=false.
	group, err := b.store.GetGroup(ctx, groupID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("get group for verdicts: %w", err)
	}
	if group == nil || !group.LearnFromVerdicts {
		return nil, nil, nil, nil, nil
	}

	// Pull a small superset and let verdictsel.Select cap. The
	// storage query is ordered newest-verdict-first by the SQL index
	// (idx_ai_verdicts for SQLite; equivalent sort in the memory
	// store). Asking for DefaultMaxTotal*4 gives Select plenty of
	// headroom across tier+kind diversity caps without overpaying
	// on the wire.
	now := time.Now().UTC()
	since := now.Add(-verdictsel.DefaultWindow)
	rows, err := b.store.ListAIVerdictsForGroup(ctx, groupID, since, verdictsel.DefaultMaxTotal*4)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("list ai verdicts: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, nil, nil, nil
	}

	// Project rollouts into verdictsel.Verdict. State maps from the
	// approved_at / rejected_at non-NULL invariant the SQL predicate
	// guarantees; the default branch is defensive against a future
	// ListAIVerdictsForGroup that loosens the predicate.
	verdicts := make([]verdictsel.Verdict, 0, len(rows))
	for _, r := range rows {
		var state string
		var ts time.Time
		switch {
		case r.RejectedAt != nil:
			state = verdictsel.StateRejected
			ts = *r.RejectedAt
		case r.ApprovedAt != nil:
			state = verdictsel.StateApproved
			ts = *r.ApprovedAt
		default:
			continue
		}
		// Body combines reasoning + approval/rejection notes. The
		// downstream verdictprompt.reasonField redacts via
		// ai.RedactSecrets and truncates to 240 chars, so the raw
		// values flow through here unchanged — no summarize() or
		// RedactSecrets call duplicated at this layer.
		body := r.ProposalReasoning
		if r.ApprovalNotes != "" {
			if body != "" {
				body = body + "\n  notes: " + r.ApprovalNotes
			} else {
				body = "notes: " + r.ApprovalNotes
			}
		}
		verdicts = append(verdicts, verdictsel.Verdict{
			ID:        r.ID,
			Kind:      "cost-spike-rollout", // §7.2 canonical kind for this surface
			State:     state,
			Timestamp: ts,
			Body:      body,
			Excluded:  r.ExcludeFromLearning,
		})
	}

	// Run the shared selection policy. PreferNeg=true mirrors the
	// slice 1 rejection-weighted ordering — the rejection bucket
	// fills first up to MaxTotal/2 before approveds backfill.
	selected := verdictsel.Select(verdicts, verdictsel.SelectOpts{
		Now:        now,
		Window:     verdictsel.DefaultWindow,
		HotWindow:  verdictsel.DefaultHotWindow,
		MaxTotal:   verdictsel.DefaultMaxTotal,
		MaxPerKind: verdictsel.DefaultMaxPerKind,
		PreferNeg:  true,
	})
	if len(selected) == 0 {
		return nil, nil, nil, nil, nil
	}

	// Split selected into approved + rejected slices for
	// verdictprompt.Render. Walk in order to build exampleIDs so the
	// audit payload preserves Select's documented ordering
	// (rejected first, then approved). v0.89.37 (#657 Stream 55,
	// #531 slice 2 chunk 6) also builds the per-state ID bucket
	// map for the new verdict_examples_used_by_state field. The
	// cost-spike surface only emits StateApproved + StateRejected
	// in practice; the switch's other-state branches are defensive
	// no-ops so a future cross-surface row (impossible today —
	// ListAIVerdictsForGroup only returns rollouts) wouldn't pollute
	// the cost-spike audit payload.
	exampleIDsByState = map[string][]string{}
	for _, v := range selected {
		exampleIDs = append(exampleIDs, v.ID)
		switch v.State {
		case verdictsel.StateApproved:
			approved = append(approved, v)
			exampleIDsByState["approved"] = append(exampleIDsByState["approved"], v.ID)
		case verdictsel.StateMerged:
			approved = append(approved, v)
			// merged isn't a cost-spike state; skip from the by-state
			// map so the cost-spike audit payload only carries the
			// approved/rejected keys it documents.
		case verdictsel.StateRejected:
			rejected = append(rejected, v)
			exampleIDsByState["rejected"] = append(exampleIDsByState["rejected"], v.ID)
		case verdictsel.StateClosedNotMerged, verdictsel.StateOperatorExcluded:
			rejected = append(rejected, v)
			// Likewise: closed_not_merged / operator_excluded belong
			// to the discovery surface; skip from cost-spike by-state.
		}
	}
	return approved, rejected, exampleIDs, exampleIDsByState, nil
}
