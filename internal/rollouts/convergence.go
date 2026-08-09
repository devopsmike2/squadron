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
