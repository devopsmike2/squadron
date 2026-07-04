// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// TestAgent_ConcurrentAccessorsVsUpdateStatus_NoRace pins the fix for the
// data race where the rollout config-sender / API handlers read agent.Status
// and agent.EffectiveConfig on their own goroutines while the agent's OpAMP
// connection goroutine mutates those fields under agent.mux in UpdateStatus.
//
// The writer goroutine drives the real UpdateStatus path (which takes the write
// lock); the reader goroutines use the locked accessors the external callers now
// use — HasCapability, EffectiveConfigSnapshot, and SendRestartCommand's locked
// Status read. Run under `go test -race`: before the fix (external callers using
// the unlocked hasCapability / raw field reads) this fails with a reported data
// race; with the locked accessors it is clean.
func TestAgent_ConcurrentAccessorsVsUpdateStatus_NoRace(t *testing.T) {
	agent := NewAgent(uuid.New(), &mockConnection{})

	// Establish an initial Status so UpdateStatus takes the steady-state
	// mutate-existing path rather than the first-report path.
	agent.UpdateStatus(newRaceStatus(0), &protobufs.ServerToAgent{})

	const iters = 300
	var wg sync.WaitGroup

	// Writer: the connection goroutine mutating Status + EffectiveConfig under
	// the write lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= iters; i++ {
			agent.UpdateStatus(newRaceStatus(uint64(i)), &protobufs.ServerToAgent{})
		}
	}()

	// Readers: external goroutines going through the locked accessors.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)
				_ = agent.EffectiveConfigSnapshot()
				agent.SendRestartCommand()
			}
		}()
	}

	wg.Wait()
}

func newRaceStatus(seq uint64) *protobufs.AgentToServer {
	return &protobufs.AgentToServer{
		SequenceNum:  seq,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand),
		EffectiveConfig: &protobufs.EffectiveConfig{
			ConfigMap: &protobufs.AgentConfigMap{
				ConfigMap: map[string]*protobufs.AgentConfigFile{
					"": {Body: []byte("receivers:\n  otlp:\n")},
				},
			},
		},
	}
}
