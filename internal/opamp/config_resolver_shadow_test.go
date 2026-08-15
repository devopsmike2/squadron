// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/services"
)

// southernPilotGroupConfig is the REAL-pipeline config assigned to the
// southern-pilot group: host logs via filelog and host metrics via hostmetrics,
// both exported to otel-cs over otlphttp — the wired config a group-config-only
// pilot host actually runs. It is deliberately NOT a bare otlp->otlp skeleton, so
// an agent that reverts to a skeleton (or the DefaultOTelConfig) is trivially
// distinguishable from one correctly resolving to the group config.
const southernPilotGroupConfig = `receivers:
  filelog:
    include: [/var/log/app/*.log]
  hostmetrics:
    scrapers:
      cpu:
      memory:
processors:
  batch:
exporters:
  otlphttp/otelcs:
    endpoint: https://otel-cs.example.com
service:
  pipelines:
    logs:
      receivers: [filelog]
      processors: [batch]
      exporters: [otlphttp/otelcs]
    metrics:
      receivers: [hostmetrics]
      processors: [batch]
      exporters: [otlphttp/otelcs]
`

// supervisedNoGroupReconnect builds the in-memory connection agent for a
// supervised (accepts_remote_config) host reconnecting on a FRESH socket
// (GroupID nil, as after a control-plane restart) that reports a non-empty
// effective config but NO group.* attribute. The non-empty effective config arms
// the adopt-on-supervise path; the nil GroupID + no-group report is what
// processAgentGrouping resolves to an EMPTY in-memory group before config
// resolution runs.
func supervisedNoGroupReconnect(id uuid.UUID) *Agent {
	return &Agent{
		InstanceId:      id,
		InstanceIdStr:   id.String(),
		FleetId:         id,
		FleetIdStr:      id.String(),
		Status:          &protobufs.AgentToServer{Capabilities: acceptsRemoteConfig},
		EffectiveConfig: brownfieldEffectiveConfig,
	}
}

// TestReconnect_GroupConfigOnlyAgent_NoAdoptShadow reproduces the EXACT
// Southern-pilot 302vd regression (v0.89.471). A supervised, already-known agent
// that is a member of a group carrying a real-pipeline config, with NO
// agent-scoped config, reconnects reporting no group. The reconnect must NOT mint
// an agent-scoped skeleton intent (adopt-on-supervise) that shadows the group
// config, and the agent must still RESOLVE to the GROUP config (real pipelines),
// not a skeleton or the default.
//
// Mechanism reproduced: processAgentGrouping clobbers the in-memory GroupID to
// the empty reported value (server.go) BEFORE resolving config, and the #36
// membership restore runs later in persistAgent. So resolveStoredConfig used to
// see an empty in-memory group, miss the group config, return found=false, and
// fall through to tryAdoptEffectiveConfig — which minted a skeleton agent-scoped
// intent that then took precedence over the still-present group config.
//
// RED on unfixed main (an agent-scoped intent is minted); GREEN after the
// resolver falls back to the persisted group membership.
func TestReconnect_GroupConfigOnlyAgent_NoAdoptShadow(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	// 302vd: persisted, member of southern-pilot, NO agent-scoped config.
	createStoredAgent(t, svc, id, sptr("group-southern-pilot"), sptr("southern-pilot"))
	// The group carries a REAL-pipeline config (filelog/hostmetrics -> otel-cs).
	require.NoError(t, svc.CreateConfig(ctx, &services.Config{
		ID:        uuid.New().String(),
		GroupID:   sptr("group-southern-pilot"),
		Content:   southernPilotGroupConfig,
		Version:   1,
		CreatedAt: time.Now(),
	}))

	agent := supervisedNoGroupReconnect(id)

	// The reconnect grouping + config-application path — where adopt wrongly fired.
	server.processAgentGrouping(ctx, agent, descNoGroup())

	// (1) No agent-scoped skeleton intent was minted for the group-config agent.
	agentCfg, err := svc.GetLatestConfigForAgent(ctx, id)
	require.NoError(t, err)
	require.Nil(t, agentCfg,
		"reconnect minted an agent-scoped intent that shadows the group config (302vd regression)")

	// (2) The agent still resolves to the REAL-pipeline group config — not a
	// skeleton, not the DefaultOTelConfig.
	resolved := server.getConfigForAgent(ctx, agent)
	require.Equal(t, southernPilotGroupConfig, resolved,
		"group-config-only agent must resolve to its real-pipeline group config on reconnect")
	require.NotEqual(t, DefaultOTelConfig, resolved)
}

// TestReconnect_UnconfiguredAgent_StillAdopts asserts ADR 0039 stays intact: a
// supervised agent with NO agent-scoped config AND NO group (persisted ungrouped)
// that reports an effective config still adopts-on-supervise. The reconnect fix
// must suppress adopt ONLY for agents that RESOLVE to a group config, never for a
// genuinely-unconfigured agent. Passes on unfixed main and after the fix.
func TestReconnect_UnconfiguredAgent_StillAdopts(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, nil, nil) // persisted, genuinely ungrouped

	agent := supervisedNoGroupReconnect(id)

	server.processAgentGrouping(ctx, agent, descNoGroup())

	// Adopt fired: an agent-scoped config was seeded from the reported effective
	// config (opamp extension stripped), not a skeleton.
	agentCfg, err := svc.GetLatestConfigForAgent(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, agentCfg,
		"genuinely unconfigured agent must still adopt-on-supervise (ADR 0039)")
	require.NotEqual(t, DefaultOTelConfig, agentCfg.Content)
	require.Contains(t, agentCfg.Content, "filelog")
	require.Contains(t, agentCfg.Content, "hostmetrics")
}
