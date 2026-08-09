package opamp

import (
	"context"

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
