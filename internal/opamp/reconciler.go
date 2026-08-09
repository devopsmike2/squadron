package opamp

import (
	"bytes"
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// reconcilePushConcurrency bounds the parallel per-agent pushes within one
// reconcile tick. Mirrors the rollout engine's pushConcurrency (v0.89 scale
// pass): pushes to distinct agents are independent WebSocket sends, so
// overlapping their ack waits is safe, and the bound keeps goroutine count and
// OpAMP server load sane on a large per-instance fleet.
const reconcilePushConcurrency = 128

// configPusher is the delivery seam the reconcile loop pushes through. Satisfied
// by *ConfigSender in production; faked in tests. Kept minimal (just the
// context-aware push) so a fake need not reimplement group/restart fan-out.
type configPusher interface {
	SendConfigToAgentWithContext(ctx context.Context, agentID uuid.UUID, configContent string) error
}

// desiredConfigResolver resolves an agent's desired stored config. Production
// wires it to the shared resolveStoredConfig over the AgentService, so the loop
// and the connect path cannot drift; tests supply a fake. The (content, found)
// shape lets the loop skip agents with no stored config — it never synthesizes
// (never delivers DefaultOTelConfig).
type desiredConfigResolver func(ctx context.Context, agent *Agent) (content string, found bool)

// Reconciler is the per-instance OpAMP reconcile loop (HA S3a, ADR 0035).
//
// Config push resolves an agent through the in-memory registry of the process
// that owns its WebSocket. In a multi-instance deployment an agent whose socket
// landed on instance B is invisible to a push driven on instance A, so a
// leader-driven push (rollout, direct) can return "agent not found" for it. The
// reconcile loop closes that gap: it runs on EVERY instance (NOT under the
// leader elector) and reconciles ONLY the agents connected to THIS instance
// toward their durable desired config — so whichever instance owns an agent's
// socket delivers its config.
//
// SCOPE = DELIVERY, not effective-drift auto-heal. Each agent is pushed based on
// the DELIVERED signal (RemoteConfigStatus.LastRemoteConfigHash): if the agent
// has not been delivered the desired config, push it. The loop deliberately does
// NOT consult EffectiveConfig (the APPLIED signal), so an agent that was
// delivered the right config but reports a divergent effective config is left
// alone — drift stays surfaced, not auto-healed (a deferred policy choice).
//
// In a single-instance/SQLite deployment the loop is a steady-state NO-OP: the
// connect path and rollout push already deliver desired config, so every agent's
// LastRemoteConfigHash already matches, and calcRemoteConfig idempotency means
// even a redundant push produces zero wire sends.
type Reconciler struct {
	agents   *Agents
	resolve  desiredConfigResolver
	sender   configPusher
	interval time.Duration
	logger   *zap.Logger
}

// NewReconciler builds the production reconcile loop, wiring the desired-config
// resolver to the shared resolveStoredConfig over agentService so the loop and
// the connect path resolve identically.
func NewReconciler(agents *Agents, agentService services.AgentService, sender *ConfigSender, interval time.Duration, logger *zap.Logger) *Reconciler {
	return &Reconciler{
		agents: agents,
		resolve: func(ctx context.Context, agent *Agent) (string, bool) {
			return resolveStoredConfig(ctx, agentService, agent)
		},
		sender:   sender,
		interval: interval,
		logger:   logger,
	}
}

// Start runs the reconcile loop until ctx is cancelled. It blocks, so callers
// run it in its own goroutine. A random startup delay (up to one interval)
// staggers instances so a fleet-wide restart does not fire every instance's
// first tick at the same instant (thundering herd).
func (r *Reconciler) Start(ctx context.Context) {
	if r.interval <= 0 {
		r.interval = 30 * time.Second
	}

	// Startup jitter: sleep a random fraction of one interval before the first
	// tick so co-restarting instances spread their first sweep.
	startupJitter := time.Duration(rand.Int63n(int64(r.interval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupJitter):
	}

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		r.reconcileOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileOnce sweeps every agent connected to THIS instance once, with
// bounded concurrency and per-agent jitter to spread pushes across the tick.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	// GetAllAgentsReadonlyClone returns clones of ONLY this instance's connected
	// agents (its in-memory registry), which is exactly the reconcile scope.
	agents := r.agents.GetAllAgentsReadonlyClone()
	if len(agents) == 0 {
		return
	}

	// Per-agent jitter is bounded to a fraction of the interval so a large
	// per-instance fleet's pushes spread out rather than firing in lockstep.
	maxJitter := r.interval / 4

	sem := make(chan struct{}, reconcilePushConcurrency)
	var wg sync.WaitGroup
	for _, agent := range agents {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(agent *Agent) {
			defer wg.Done()
			defer func() { <-sem }()

			if maxJitter > 0 {
				jitter := time.Duration(rand.Int63n(int64(maxJitter)))
				select {
				case <-ctx.Done():
					return
				case <-time.After(jitter):
				}
			}
			r.reconcileAgent(ctx, agent)
		}(agent)
	}
	wg.Wait()
}

// reconcileAgent delivers the desired config to a single connected agent when it
// has not already been delivered. It is the delivery-scoped decision point.
func (r *Reconciler) reconcileAgent(ctx context.Context, agent *Agent) {
	if agent == nil {
		return
	}

	// Only agents that advertise remote-config support are reconcilable; the
	// sender would reject the rest anyway.
	if !agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) {
		return
	}

	content, found := r.resolve(ctx, agent)
	if !found {
		// No stored agent/group config. The connect path would layer the
		// synthesized DefaultOTelConfig on, but the reconcile loop must never
		// deliver synthesized config — there is nothing to converge here.
		return
	}

	desiredHash := wireConfigHash(content)

	// DELIVERED signal: LastRemoteConfigHash is the hash the agent echoes for the
	// last remote config it was delivered. If it already equals the desired hash,
	// the desired config has been delivered — no push. This is delivery-scoped:
	// we deliberately do NOT compare EffectiveConfig (the APPLIED signal), so
	// effective-config drift is surfaced elsewhere, not auto-healed here.
	if bytes.Equal(deliveredConfigHash(agent), desiredHash) {
		return
	}

	// Not delivered — push. Store lookups and the push are keyed by the fleet id
	// (storeID). calcRemoteConfig idempotency means that if the content already
	// matches the agent's CustomInstanceConfig (e.g. the connect path delivered
	// it and the status echo just hasn't been observed yet), this produces ZERO
	// wire sends.
	if err := r.sender.SendConfigToAgentWithContext(ctx, agent.storeID(), content); err != nil {
		r.logger.Debug("reconcile delivery push failed",
			zap.String("agentId", agent.storeID().String()),
			zap.Error(err))
	}
}

// deliveredConfigHash returns the hash of the remote config last delivered to
// the agent (RemoteConfigStatus.LastRemoteConfigHash), or nil if the agent has
// not yet reported one — in which case nothing has been delivered and the
// desired config will be pushed.
func deliveredConfigHash(agent *Agent) []byte {
	if agent.Status == nil || agent.Status.RemoteConfigStatus == nil {
		return nil
	}
	return agent.Status.RemoteConfigStatus.LastRemoteConfigHash
}
