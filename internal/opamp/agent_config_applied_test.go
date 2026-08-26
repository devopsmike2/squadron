// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// TestAppliedConfigHash covers the DELIVERED/APPLIED signal helper (ADR 0040):
// it returns the confignorm hash of CustomInstanceConfig exactly when the agent
// has acked the remote-config hash Squadron staged for it, and ("", false)
// otherwise.
func TestAppliedConfigHash(t *testing.T) {
	const content = "receivers:\n  otlp:\n    protocols:\n      grpc:"
	wire := wireConfigHash(content)

	t.Run("acked current config returns confignorm hash", func(t *testing.T) {
		agent := &Agent{
			CustomInstanceConfig: content,
			remoteConfig:         &protobufs.AgentRemoteConfig{ConfigHash: wire},
			Status: &protobufs.AgentToServer{
				RemoteConfigStatus: &protobufs.RemoteConfigStatus{LastRemoteConfigHash: wire},
			},
		}
		got, ok := agent.appliedConfigHash()
		if !ok {
			t.Fatal("expected ok=true when the agent acked the staged config")
		}
		if want := confignorm.Hash(content); got != want {
			t.Fatalf("hash = %q, want %q (confignorm hash of applied content)", got, want)
		}
	})

	t.Run("acked a different config -> not applied", func(t *testing.T) {
		agent := &Agent{
			CustomInstanceConfig: content,
			remoteConfig:         &protobufs.AgentRemoteConfig{ConfigHash: wire},
			Status: &protobufs.AgentToServer{
				RemoteConfigStatus: &protobufs.RemoteConfigStatus{
					LastRemoteConfigHash: wireConfigHash("something else"),
				},
			},
		}
		if _, ok := agent.appliedConfigHash(); ok {
			t.Fatal("expected ok=false when the acked hash != staged config hash")
		}
	})

	t.Run("no remote config staged -> not applied", func(t *testing.T) {
		agent := &Agent{
			CustomInstanceConfig: content,
			Status: &protobufs.AgentToServer{
				RemoteConfigStatus: &protobufs.RemoteConfigStatus{LastRemoteConfigHash: wire},
			},
		}
		if _, ok := agent.appliedConfigHash(); ok {
			t.Fatal("expected ok=false when no remoteConfig is staged")
		}
	})

	t.Run("no remote config status reported -> not applied", func(t *testing.T) {
		agent := &Agent{
			CustomInstanceConfig: content,
			remoteConfig:         &protobufs.AgentRemoteConfig{ConfigHash: wire},
			Status:               &protobufs.AgentToServer{},
		}
		if _, ok := agent.appliedConfigHash(); ok {
			t.Fatal("expected ok=false when the agent reports no RemoteConfigStatus")
		}
	})

	t.Run("empty acked hash -> not applied", func(t *testing.T) {
		agent := &Agent{
			CustomInstanceConfig: content,
			remoteConfig:         &protobufs.AgentRemoteConfig{ConfigHash: wire},
			Status: &protobufs.AgentToServer{
				RemoteConfigStatus: &protobufs.RemoteConfigStatus{LastRemoteConfigHash: nil},
			},
		}
		if _, ok := agent.appliedConfigHash(); ok {
			t.Fatal("expected ok=false when the agent has not yet acked any config")
		}
	})

	// BUG 2 / WA4.2: a FAILED apply echoes back the SAME staged wire hash as a
	// successful one — the agent received the config but could not apply it (a
	// supervised collector rejects it and keeps its last-known-good effective
	// config). The DELIVERED/APPLIED signal must NOT be stamped from it, or drift
	// reads the rejected config as synced. RED on main (which ignores Status and
	// returns the hash), GREEN after the fix.
	t.Run("acked current config but FAILED -> not applied", func(t *testing.T) {
		agent := &Agent{
			CustomInstanceConfig: content,
			remoteConfig:         &protobufs.AgentRemoteConfig{ConfigHash: wire},
			Status: &protobufs.AgentToServer{
				RemoteConfigStatus: &protobufs.RemoteConfigStatus{
					LastRemoteConfigHash: wire,
					Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED,
				},
			},
		}
		if _, ok := agent.appliedConfigHash(); ok {
			t.Fatal("expected ok=false when the agent reported FAILED for the acked config (rejected, not applied)")
		}
	})

	// A successful (APPLIED) ack of the staged config still returns the hash — the
	// 300vd / linuxhost03 supervised-synced path must be preserved.
	t.Run("acked current config and APPLIED -> applied", func(t *testing.T) {
		agent := &Agent{
			CustomInstanceConfig: content,
			remoteConfig:         &protobufs.AgentRemoteConfig{ConfigHash: wire},
			Status: &protobufs.AgentToServer{
				RemoteConfigStatus: &protobufs.RemoteConfigStatus{
					LastRemoteConfigHash: wire,
					Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				},
			},
		}
		got, ok := agent.appliedConfigHash()
		if !ok {
			t.Fatal("expected ok=true when the agent APPLIED the staged config")
		}
		if want := confignorm.Hash(content); got != want {
			t.Fatalf("hash = %q, want %q", got, want)
		}
	})
}
