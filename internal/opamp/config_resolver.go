package opamp

import (
	"bytes"
	"context"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/services"
)

// resolveStoredConfig resolves an agent's desired config from the store,
// applying the agent-specific > group precedence. It returns the config content
// and true when a stored config exists, or ("", false) when neither an
// agent-specific nor a group config is present for this agent.
//
// This is the ONE shared desired-config resolver: both the connect path
// (Server.getConfigForAgent) and the per-instance reconcile loop
// (Reconciler.reconcileAgent) resolve through it, so their notion of "desired
// config" cannot drift. Store lookups are keyed by the Squadron fleet id
// (agent.storeID()), matching every other store-facing OpAMP caller.
//
// It deliberately does NOT apply the DefaultOTelConfig fallback. The connect
// path layers that synthesized default on for a brand-new agent that has no
// stored config; the reconcile loop must never deliver synthesized config — it
// only ever re-delivers content already in the configs table (validated at
// create/update time). The (content, found) shape lets each caller decide:
// getConfigForAgent falls back to the default when found is false, the reconcile
// loop skips the agent.
func resolveStoredConfig(ctx context.Context, svc services.AgentService, agent *Agent) (content string, found bool) {
	if agent == nil {
		return "", false
	}

	// Prefer agent-specific config.
	if agentConfig, err := svc.GetLatestConfigForAgent(ctx, agent.storeID()); err == nil && agentConfig != nil {
		return agentConfig.Content, true
	}

	// Fall back to group-level config.
	if agent.GroupID != nil && *agent.GroupID != "" {
		if groupConfig, err := svc.GetLatestConfigForGroup(ctx, *agent.GroupID); err == nil && groupConfig != nil {
			return groupConfig.Content, true
		}
	}

	return "", false
}

// appliedConfigHashFromDesired derives the DELIVERED/APPLIED confignorm hash for
// a supervised agent from the STORE, without depending on the in-memory staged
// remoteConfig that Agent.appliedConfigHash requires.
//
// It reuses the exact "delivered" equivalence the HA reconciler keys on
// (Reconciler.reconcileAgent, internal/opamp/reconciler.go): the agent's reported
// RemoteConfigStatus.LastRemoteConfigHash byte-equals wireConfigHash(desired
// stored config). When they match, the agent is running exactly the bytes
// Squadron assigned, so this returns (confignorm.Hash(desired), true) — the same
// canonical hash the stored intent carries (ConfigIntent.Hash), directly
// comparable in computeConfigDrift.
//
// Why this exists (ADR 0040 follow-up). Agent.appliedConfigHash only fires while
// agent.remoteConfig is staged IN MEMORY (set by calcRemoteConfig on a
// connect/group-change or a fresh push). An already-applied, already-converged
// supervised agent that merely heartbeats after a control-plane upgrade has no
// staged remoteConfig — the exact state TestAppliedConfigHash's "no remote config
// staged -> not applied" case captures — so appliedConfigHash returns false and
// delivered_config_hash stays empty, leaving the agent permanently "drifted" in
// computeConfigDrift even though the reconciler already considers it fully
// delivered. This store-derived path closes that asymmetry: it fires on every
// status report that echoes the matching wire hash, with no fresh push and no
// live in-memory staging. Genuine divergence (a different/older
// LastRemoteConfigHash, or no stored config) still returns ("", false) -> the
// agent stays drifted. Gated on accepts_remote_config so report-only agents are
// untouched, mirroring appliedConfigHash / agentAcceptsRemoteConfig.
func appliedConfigHashFromDesired(ctx context.Context, svc services.AgentService, agent *Agent) (string, bool) {
	if agent == nil || svc == nil {
		return "", false
	}
	if !agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) {
		return "", false
	}

	// The agent's reported delivered/remote-config wire hash (nil if it has never
	// reported a RemoteConfigStatus). Same signal the reconciler reads.
	acked := deliveredConfigHash(agent)
	if len(acked) == 0 {
		return "", false
	}

	content, found := resolveStoredConfig(ctx, svc, agent)
	if !found {
		return "", false
	}

	// Wire-hash vs wire-hash — computed the same way calcRemoteConfig stamps the
	// push and the reconciler computes the desired hash, so no staging event is
	// needed for them to be comparable.
	if !bytes.Equal(acked, wireConfigHash(content)) {
		return "", false
	}

	return confignorm.Hash(content), true
}
