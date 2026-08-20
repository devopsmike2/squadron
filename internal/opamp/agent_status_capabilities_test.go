// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
)

// TestUpdateStatus_RefreshesCapabilitiesLive pins the capability-liveness fix:
// the restart / remote-config capability gates must reflect what the CURRENTLY
// connected agent advertises, not a first-connect snapshot.
//
// Before the fix, agent.Status.Capabilities was captured only on the first
// status report (updateAgentDescription never touched Capabilities), so an agent
// that started WITHOUT AcceptsRestartCommand and later advertised it — or the
// reverse — kept its first-reported bitmask forever, and HasCapability lied.
func TestUpdateStatus_RefreshesCapabilitiesLive(t *testing.T) {
	agent := NewAgent(uuid.New(), &mockConnection{})

	// First report: remote-config only, no restart capability.
	agent.UpdateStatus(&protobufs.AgentToServer{
		SequenceNum:  1,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
	}, &protobufs.ServerToAgent{})

	assert.True(t, agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
		"remote-config capability should be present after first report")
	assert.False(t, agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand),
		"restart capability must NOT be present when the agent did not advertise it")

	// Second report: the agent (e.g. after a supervisor upgrade) now advertises
	// AcceptsRestartCommand. The gate must pick this up live.
	agent.UpdateStatus(&protobufs.AgentToServer{
		SequenceNum: 2,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand),
	}, &protobufs.ServerToAgent{})

	assert.True(t, agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand),
		"restart capability must be observed live once the agent advertises it")
}

// TestUpdateStatus_CapabilityLessHeartbeatDoesNotWipe guards the non-zero refresh
// rule: a heartbeat that omits Capabilities (proto3 scalar 0, indistinguishable
// from unset) must NOT clear a previously-advertised capability.
func TestUpdateStatus_CapabilityLessHeartbeatDoesNotWipe(t *testing.T) {
	agent := NewAgent(uuid.New(), &mockConnection{})

	agent.UpdateStatus(&protobufs.AgentToServer{
		SequenceNum: 1,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand),
	}, &protobufs.ServerToAgent{})

	// A follow-up report that does not resend the bitmask (Capabilities == 0).
	agent.UpdateStatus(&protobufs.AgentToServer{
		SequenceNum:  2,
		Capabilities: 0,
	}, &protobufs.ServerToAgent{})

	assert.True(t, agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand),
		"a capability-less heartbeat must not wipe a previously-advertised restart capability")
	assert.True(t, agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
		"a capability-less heartbeat must not wipe a previously-advertised remote-config capability")
}
