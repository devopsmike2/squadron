// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// v0.59 added proposal.skipped emissions at the two points where
// bridge.buildContext refuses to call the LLM. These tests pin the
// behavior so a future refactor that silently moves either return
// path will trip a clear failure here.

// TestBridge_SkipsWhenGroupInferenceFails asserts an audit event
// fires when the attribution does not resolve to any known group.
func TestBridge_SkipsWhenGroupInferenceFails(t *testing.T) {
	store, spike := baselineFixture()
	// Drop every agent so the bridge cannot infer a group from
	// the attribution's top_agents.
	store.agents = nil
	spike.AttributionJSON = `{"top_agents":["b1d29c4e-1234-1234-1234-123456789012"],"top_attributes":["container.id"]}`

	prop := &fakeProposer{enabled: true}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}

	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	// Proposer must not be called; rollouts must not be posted.
	assert.Zero(t, prop.calls, "proposer should not have been called")
	assert.Empty(t, rollouts.inputs, "no rollout should be dispatched")

	// One proposal.skipped event with reason=group_inference_failed.
	require.Len(t, audit.entries, 1)
	e := audit.entries[0]
	assert.Equal(t, services.AuditEventProposalSkipped, e.EventType)
	assert.Equal(t, "cost_spike", e.TargetType)
	assert.Equal(t, "spike-1", e.TargetID)
	assert.Equal(t, "ai-proposer", e.Actor)
	assert.Equal(t, "skipped", e.Action)
	assert.Equal(t, "ai", e.Payload["origin"])
	assert.Equal(t, "group_inference_failed", e.Payload["reason"])
	// Detail fields the explain endpoint will read.
	assert.Equal(t, true, e.Payload["attribution_present"])
	assert.Equal(t, 1, e.Payload["top_agents_count"])
	assert.Equal(t, 1, e.Payload["top_attributes_count"])
}

// TestBridge_SkipsWhenMissingCurrentConfig asserts an audit event
// fires when the bridge resolves a group but the group has no
// current config to propose against.
func TestBridge_SkipsWhenMissingCurrentConfig(t *testing.T) {
	store, _ := baselineFixture()
	// Drop the config so GetLatestConfigForGroup returns nil.
	store.cfgs = nil

	prop := &fakeProposer{enabled: true}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}

	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	assert.Zero(t, prop.calls)
	assert.Empty(t, rollouts.inputs)

	require.Len(t, audit.entries, 1)
	e := audit.entries[0]
	assert.Equal(t, services.AuditEventProposalSkipped, e.EventType)
	assert.Equal(t, "missing_current_config", e.Payload["reason"])
	assert.Equal(t, "prod-utility-fleet", e.Payload["group_id"])
	assert.Equal(t, "Prod Utility Fleet", e.Payload["group_name"])
}

// TestBridge_SkipsWhenAttributionMalformed asserts the malformed
// JSON path lands in the group_inference_failed bucket (parseAttribution
// returns nil, inferGroup returns empty, emitSkipped fires).
func TestBridge_SkipsWhenAttributionMalformed(t *testing.T) {
	store, spike := baselineFixture()
	spike.AttributionJSON = `{"top_agents":"not-an-array"`

	prop := &fakeProposer{enabled: true}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}

	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	require.Len(t, audit.entries, 1)
	assert.Equal(t, "group_inference_failed", audit.entries[0].Payload["reason"])
	assert.Equal(t, true, audit.entries[0].Payload["attribution_present"])
	assert.Equal(t, 0, audit.entries[0].Payload["top_agents_count"])
}
