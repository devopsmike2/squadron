// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// HA S3d (ADR 0035) convergence-gating defaults.
const (
	// defaultCoverageGrace mirrors the S3b reconcile-heartbeat grace (2× the
	// 30s reconcile interval): a live owning instance refreshes each owned
	// agent's ownership row every reconcile tick, so a row older than this has
	// no live owner.
	defaultCoverageGrace = 60 * time.Second

	// defaultConvergenceTimeout is the extra budget beyond a stage's dwell that
	// the engine waits for the stage's canary set to converge before aborting.
	// Generous on purpose — the happy path converges within a tick or two of
	// delivery; this only catches agents whose owning instance is dead or that
	// never apply the target.
	defaultConvergenceTimeout = 10 * time.Minute
)

// ConnectionRegistry is the read subset of the S3b connection registry the
// engine uses for convergence COVERAGE diagnostics (ADR 0035): which agents are
// currently owned by a live instance. It is intentionally tiny (just the list
// read) so a test fake need not reimplement the whole store, and nil-able so
// single-instance / unwired deployments skip the diagnostic entirely.
type ConnectionRegistry interface {
	ListConnectionOwners(ctx context.Context) ([]*applicationstore.ConnectionOwner, error)
}

// writeDesiredStateForAgents persists a per-agent desired-state config
// assignment (an agent-scoped configs row carrying content) for each agent, so
// determineConfigIntent / resolveStoredConfig resolve that agent to content and
// the owning instance's S3a reconcile loop delivers it (ADR 0035). It is
// idempotent: an agent whose latest agent-scoped config already carries this
// content is left untouched (no version churn on percent-superset re-applies or
// re-ticks). Returns the ids durably assigned (written or already-assigned) —
// the caller folds these into r.PushedAgentIDs as the rollout's real pushed-set.
//
// The content is required to already exist, validated, in the configs table (the
// caller passes the target/previous config's content), preserving the existing
// "only ever deliver validated config" guarantee: the agent-scoped row is a new
// pointer at already-validated content, not new unvalidated config.
func (e *Engine) writeDesiredStateForAgents(ctx context.Context, r *services.Rollout, agents []*services.Agent, content string) []uuid.UUID {
	if e.agentService == nil || len(agents) == 0 {
		return nil
	}
	desiredHash := confignorm.Hash(content)
	assigned := make([]uuid.UUID, 0, len(agents))
	for _, a := range agents {
		version := 1
		if latest, err := e.agentService.GetLatestConfigForAgent(ctx, a.ID); err == nil && latest != nil {
			// Idempotent: the agent already desires this exact content.
			if confignorm.Hash(latest.Content) == desiredHash {
				assigned = append(assigned, a.ID)
				continue
			}
			version = latest.Version + 1
		}
		agentID := a.ID
		cfg := &services.Config{
			ID:         uuid.NewString(),
			AgentID:    &agentID,
			ConfigHash: desiredHash,
			Content:    content,
			Version:    version,
			CreatedAt:  time.Now().UTC(),
		}
		if err := e.agentService.CreateConfig(ctx, cfg); err != nil {
			// Best-effort: a write failure just means this agent isn't assigned
			// yet; it stays out of the pushed-set and the next stage/tick retries.
			e.logger.Warn("rollout engine: failed to write per-agent desired state",
				zap.String("rollout_id", r.ID),
				zap.String("agent_id", agentID.String()),
				zap.Error(err))
			continue
		}
		assigned = append(assigned, agentID)
	}
	return assigned
}

// finalizeCanaryAssignments cleans up the per-agent desired-state config rows
// S3d wrote for a rollout's canary set once the rollout reaches a TERMINAL state,
// closing the "stuck canary" lifecycle gap: an agent-scoped configs row takes
// precedence over group config in determineConfigIntent, and configs are
// append-only, so without this an ex-canary agent stays pinned to its rollout
// assignment forever and never tracks future GROUP-config changes.
//
// RECON (load-bearing): a successful rollout's finish() does NOT promote the
// target to the group config, and the rollout never mutates the group config at
// any point. So naively deleting the agent-scoped rows would drop every ex-canary
// agent back to the OLD (pre-rollout) group config — the WRONG config. The
// correct final state therefore depends on the outcome:
//
//   - promoteTarget == true (SUCCESS) AND the rollout reached FULL coverage
//     (every group member received the target — see rolloutReachedFullCoverage):
//     first PROMOTE the target to the group config (a new group-scoped configs
//     row = the target content), then delete the agent-scoped rows. Each
//     ex-canary agent falls back to group == target (the config it converged to)
//     AND resumes tracking future group changes; the rest of the group converges
//     to the same target — the definition of a completed rollout. If promotion
//     fails, the agent-scoped rows are LEFT in place (pre-fix behavior) rather
//     than risk dropping an agent to a stale config.
//
//   - promoteTarget == true (SUCCESS) but the rollout reached only PARTIAL
//     coverage (it succeeded with a final stage below the whole group — e.g. a
//     rollout whose last stage is 50%): do NOT promote and do NOT delete the
//     agent-scoped rows. Promoting would push the target onto NON-canary group
//     members that were never meant to receive it (the over-apply this gate
//     closes); deleting the pins would drop the canary agents off the target they
//     intentionally hold. So both are skipped: the canary agents stay pinned to
//     the target (their intended partial state), the group config is untouched,
//     and non-canary members keep tracking the old group config.
//
//   - promoteTarget == false (ROLLBACK): the group config was never changed, so
//     it still equals the pre-rollout config (the PreviousConfigID snapshot). The
//     rollback push already delivered the previous content; deleting the
//     agent-scoped rows returns each agent to group-config tracking, whose value
//     IS the pre-rollout state — the correct revert.
//
// Gated by writeDesiredState (the S3d desired-state model): legacy
// direct-push-only struct-literal tests wrote no agent-scoped rows, so cleanup is
// skipped and their behavior is unchanged. Runs only on the elected leader
// (finish/rollback do). Best-effort + idempotent: deleting an already-gone row is
// a no-op and any failure is logged, never failing the terminal transition.
func (e *Engine) finalizeCanaryAssignments(ctx context.Context, r *services.Rollout, promoteTarget bool) {
	if !e.writeDesiredState || e.agentService == nil {
		return
	}
	if promoteTarget {
		// Gate promotion + pin-cleanup on FULL coverage. A rollout can reach
		// SUCCESS with a final stage below the whole group (a partial rollout);
		// promoting the target to the group config then would over-apply it to
		// non-canary members that never received it, and deleting the pins would
		// pull the canary agents off the target they intentionally hold. On
		// partial success we do neither — leave the group config and the canary
		// pins exactly as the rollout left them.
		if !e.rolloutReachedFullCoverage(ctx, r) {
			e.logger.Info("rollout engine: partial success — leaving canary pins and group config untouched (no full-coverage promotion)",
				zap.String("rollout_id", r.ID),
				zap.String("group_id", r.GroupID))
			return
		}
		if !e.promoteTargetToGroupConfig(ctx, r) {
			// Promotion failed: deleting the agent-scoped rows now would drop
			// ex-canary agents to a stale group config. Leave them assigned; a
			// later pass (or operator) can retry. Correctness beats un-sticking.
			return
		}
	}
	for _, idStr := range r.PushedAgentIDs {
		agentID, err := uuid.Parse(idStr)
		if err != nil {
			continue
		}
		if err := e.agentService.DeleteConfigsForAgent(ctx, agentID); err != nil {
			e.logger.Warn("rollout engine: failed to clean up per-agent desired state",
				zap.String("rollout_id", r.ID),
				zap.String("agent_id", idStr),
				zap.Error(err))
		}
	}
}

// promoteTargetToGroupConfig makes a successful rollout's target the group's
// config of record: it writes a new group-scoped configs row carrying the target
// content at version = latest-group-version + 1. This is what makes removing the
// agent-scoped canary rows safe on SUCCESS — the group config the ex-canary
// agents fall back to now IS the target. Returns true when the group config is
// (now, or already was) the target — i.e. it is safe to delete the agent-scoped
// rows; false on a read/write failure so the caller preserves them.
//
// Idempotent: if the group's latest config already hashes to the target (a
// re-run, or an operator who set it directly) it does nothing and returns true.
func (e *Engine) promoteTargetToGroupConfig(ctx context.Context, r *services.Rollout) bool {
	if r.GroupID == "" {
		// No group to promote into (shouldn't happen for a group rollout). Nothing
		// to fall back to, so treat as safe — deletion just clears the pin.
		return true
	}
	target, err := e.configStore.GetConfig(ctx, r.TargetConfigID)
	if err != nil || target == nil {
		e.logger.Warn("rollout engine: cannot promote target to group config (target unreadable)",
			zap.String("rollout_id", r.ID),
			zap.String("target_config_id", r.TargetConfigID),
			zap.Error(err))
		return false
	}
	targetHash := confignorm.Hash(target.Content)
	version := 1
	if latest, err := e.agentService.GetLatestConfigForGroup(ctx, r.GroupID); err == nil && latest != nil {
		if confignorm.Hash(latest.Content) == targetHash {
			return true // already promoted — idempotent no-op
		}
		version = latest.Version + 1
	}
	groupID := r.GroupID
	cfg := &services.Config{
		ID:         uuid.NewString(),
		GroupID:    &groupID,
		ConfigHash: targetHash,
		Content:    target.Content,
		Version:    version,
		CreatedAt:  time.Now().UTC(),
	}
	if err := e.agentService.CreateConfig(ctx, cfg); err != nil {
		e.logger.Warn("rollout engine: failed to promote target to group config",
			zap.String("rollout_id", r.ID),
			zap.String("group_id", groupID),
			zap.Error(err))
		return false
	}
	e.logger.Info("rollout engine: promoted target to group config on success",
		zap.String("rollout_id", r.ID),
		zap.String("group_id", groupID),
		zap.String("config_id", cfg.ID),
		zap.Int("version", version))
	return true
}

// rolloutReachedFullCoverage reports whether a SUCCEEDED rollout brought its
// ENTIRE target group to the target — the precondition for promoting the target
// to the group config and clearing the canary pins. A rollout that succeeded
// with a final stage below the whole group (e.g. a last stage of 50%, or a
// label stage matching only a subset) is a PARTIAL rollout: its non-canary
// members were never meant to receive the target, so it must not promote.
//
// Signal (verified against the engine): finish() is reached the moment the LAST
// stage converges (engine.go advanceOrCheck: r.CurrentStage == len(Stages)-1 →
// finish). "Full coverage" is therefore "every current group member is in the
// rollout's cumulative pushed-set". r.PushedAgentIDs is the engine-managed union
// of every stage's canary set (ADR 0007; rollout_service.go Rollout.PushedAgentIDs)
// and canaryAgentsForStage only ever selects agents already in the target group,
// so pushed ⊆ group and the check reduces to "no group member is missing from
// pushed". This is mode-agnostic — for percent mode it is exactly the final stage
// reaching 100% (canaryAgentsForStage takes the first Percentage% of the group),
// and for label mode it is "the selector matched every member" — and it reflects
// the real coverage rather than re-deriving it from stage config.
//
// Fails CLOSED: an empty group or a ListAgents error returns false, so an
// indeterminate coverage read never triggers an unintended whole-group promotion
// (the conservative bias this package documents — leaving pins is always safe).
func (e *Engine) rolloutReachedFullCoverage(ctx context.Context, r *services.Rollout) bool {
	if e.agentService == nil {
		return false
	}
	all, err := e.agentService.ListAgents(ctx)
	if err != nil {
		return false
	}
	pushed := make(map[string]struct{}, len(r.PushedAgentIDs))
	for _, id := range r.PushedAgentIDs {
		pushed[id] = struct{}{}
	}
	groupCount := 0
	for _, a := range all {
		if a == nil || a.GroupID == nil || *a.GroupID != r.GroupID {
			continue
		}
		groupCount++
		if _, ok := pushed[a.ID.String()]; !ok {
			// A group member never received the target → partial coverage.
			return false
		}
	}
	// Full coverage iff the group is non-empty and every member was pushed.
	return groupCount > 0
}

// stageConverged reports whether the current stage's canary set has reached its
// convergence threshold — i.e. enough of the canary agents have REPORTED the
// rollout target as their effective config (the ADR 0035 "answered by reported
// state" contract). The second return is a human-readable shortfall detail used
// for the awaiting-convergence trace event and the convergence-timeout abort
// reason. An empty canary set converges trivially (matches applyStage's
// empty-canary soft-advance).
func (e *Engine) stageConverged(ctx context.Context, r *services.Rollout, stage services.RolloutStage) (bool, string) {
	target, err := e.configStore.GetConfig(ctx, r.TargetConfigID)
	if err != nil || target == nil {
		// Can't resolve the target — can't judge convergence, so don't advance
		// blindly. Not a timeout condition on its own; the next tick retries.
		return false, "target config unresolved"
	}
	targetHash := confignorm.Hash(target.Content)
	if targetHash == "" {
		return false, "target config normalizes to empty"
	}

	canary, err := e.canaryAgentsForStage(ctx, r, r.CurrentStage)
	if err != nil {
		return false, "canary set unresolved"
	}
	if len(canary) == 0 {
		return true, ""
	}

	convergedCount := 0
	for _, a := range canary {
		if agentConvergedToHash(a, targetHash) {
			convergedCount++
		}
	}
	required := convergenceRequired(len(canary), stage.ConvergencePercent)
	if convergedCount >= required {
		return true, ""
	}

	// Not there yet — enrich the shortfall with a coverage read so the operator
	// (and the abort reason, if we time out) can see how many canary agents have
	// no live owning instance to deliver to.
	uncovered := e.uncoveredCanaryCount(ctx, canary)
	return false, fmt.Sprintf(
		"stage %d converged %d/%d canary agent(s) to target (need %d; %d have no live owning instance)",
		r.CurrentStage, convergedCount, len(canary), required, uncovered)
}

// agentConvergedToHash reports whether the agent's reported effective config
// matches the target hash. confignorm.Hash normalizes then hashes, so this
// agrees byte-for-byte with the store's drift computation (DriftStatus==Synced
// against a target intent) without depending on the agent's ConfigIntent having
// already been recomputed to point at the target. An agent that reports no
// effective config hashes to the empty sentinel and never matches a real target.
func agentConvergedToHash(a *services.Agent, targetHash string) bool {
	if a == nil {
		return false
	}
	return confignorm.Hash(a.EffectiveConfig) == targetHash
}

// convergenceRequired maps a stage's ConvergencePercent to the minimum number of
// canary agents that must converge. 0 (unset / pre-S3d) and any value >= 100
// mean "all canary agents". A positive sub-100 percent is applied with ceiling
// rounding and a floor of 1, so a non-zero threshold always requires at least
// one converged agent.
func convergenceRequired(canarySize, percent int) int {
	if canarySize <= 0 {
		return 0
	}
	if percent <= 0 || percent >= 100 {
		return canarySize
	}
	required := (canarySize*percent + 99) / 100 // ceil
	if required < 1 {
		required = 1
	}
	if required > canarySize {
		required = canarySize
	}
	return required
}

// convergenceDeadlineExceeded reports whether the current stage has been open
// longer than its dwell plus the convergence timeout — the point past which the
// engine stops waiting for stragglers and aborts. Measured from StageStartedAt;
// a nil StageStartedAt (shouldn't happen for an in-progress stage) is never
// considered timed out.
func (e *Engine) convergenceDeadlineExceeded(r *services.Rollout, stage services.RolloutStage) bool {
	if r.StageStartedAt == nil {
		return false
	}
	timeout := e.convergenceTimeout
	if timeout <= 0 {
		timeout = defaultConvergenceTimeout
	}
	budget := time.Duration(stage.DwellSeconds)*time.Second + timeout
	return time.Since(*r.StageStartedAt) >= budget
}

// uncoveredCanaryCount returns how many of the given canary agents currently
// have NO live owning instance in the connection registry (ownership row absent
// or heartbeat older than the coverage grace). It is a DIAGNOSTIC only — it does
// not change the convergence numerator (an uncovered agent already fails the
// reported-effective-config check because nothing is delivering to it). A nil
// registry or a read error fails open (returns 0): coverage is unknown, so the
// engine reports nothing rather than over-claiming staleness.
func (e *Engine) uncoveredCanaryCount(ctx context.Context, canary []*services.Agent) int {
	if e.connRegistry == nil || len(canary) == 0 {
		return 0
	}
	owners, err := e.connRegistry.ListConnectionOwners(ctx)
	if err != nil {
		return 0
	}
	grace := e.coverageGrace
	if grace <= 0 {
		grace = defaultCoverageGrace
	}
	cutoff := time.Now().Add(-grace)
	live := make(map[uuid.UUID]struct{}, len(owners))
	for _, o := range owners {
		if o == nil {
			continue
		}
		if o.LastHeartbeatAt.After(cutoff) {
			live[o.AgentID] = struct{}{}
		}
	}
	uncovered := 0
	for _, a := range canary {
		if _, ok := live[a.ID]; !ok {
			uncovered++
		}
	}
	return uncovered
}
