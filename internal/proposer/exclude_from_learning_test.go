// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// v0.89.26 (#642 Stream 43) acceptance tests for the per-rollout
// exclude-from-learning toggle (#531 slice 2 §10 Q3). Three tests
// live here because they exercise bridge.assembleVerdicts directly:
//
//   - TestExcludeFromLearning_ExcludedRolloutNotInVerdictExamples
//   - TestExcludeFromLearning_ChangedAfterCreation_AffectsNextProposal
//   - TestExcludeFromLearning_GroupOptOutStillRespected
//
// The HTTP / audit acceptance tests
// (TestExcludeFromLearning_Toggle_Persists,
// TestExcludeFromLearning_AuditEventEmittedOnToggle) live in
// internal/api/handlers/rollouts_exclude_from_learning_test.go. Two
// file layout keeps each test in the package that owns the function
// it asserts on.

package proposer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// newBridgeForVerdictsTest builds a Bridge with only the store wired —
// we exercise assembleVerdicts directly so the proposer / rollouts /
// audit collaborators are intentionally nil. The poll interval is
// irrelevant because we never call Start.
func newBridgeForVerdictsTest(store *fakeStore) *Bridge {
	return &Bridge{
		store:    store,
		logger:   zap.NewNop(),
		seen:     map[string]struct{}{},
		shutdown: make(chan struct{}),
	}
}

// TestExcludeFromLearning_ExcludedRolloutNotInVerdictExamples is
// acceptance test #2. Seed two AI-originated approved rollouts in
// group G, both within the 30-day window. Mark one
// ExcludeFromLearning=true. Call assembleVerdicts(G). Assert that
// only the non-excluded rollout appears in the returned
// VerdictExample slice and the verdict_ids list — the excluded one
// is filtered out by the bridge's per-rollout filter (the fake's
// ListAIVerdictsForGroup mirrors the SQLite `exclude_from_learning =
// 0` predicate).
func TestExcludeFromLearning_ExcludedRolloutNotInVerdictExamples(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	now := time.Now()
	// Two approved AI rollouts, both within the 30-day window. The
	// first is excluded; the second stays in the learning loop.
	excluded := approvedRollout(
		"rlt_excluded", gid,
		"contains a customer name in the rejecter note",
		"customer: AcmeCorp — PII risk",
		now.Add(-2*time.Hour),
	)
	excluded.ExcludeFromLearning = true
	included := approvedRollout(
		"rlt_included", gid,
		"safe reasoning, no PII",
		"good plan, ship it",
		now.Add(-3*time.Hour),
	)
	store.verdicts = map[string][]*types.Rollout{gid: {excluded, included}}

	b := newBridgeForVerdictsTest(store)
	examples, ids, err := b.assembleVerdicts(context.Background(), gid)
	require.NoError(t, err)

	// Only the included rollout makes it through.
	require.Len(t, examples, 1, "excluded rollout must be filtered out by the bridge")
	assert.Equal(t, "rlt_included", examples[0].RolloutID,
		"the non-excluded rollout is the one that surfaces")
	assert.Equal(t, ai.VerdictStateApproved, examples[0].State)
	assert.Equal(t, []string{"rlt_included"}, ids,
		"the audit verdict_examples_used list must NOT carry the excluded rollout id")
}

// TestExcludeFromLearning_ChangedAfterCreation_AffectsNextProposal
// is acceptance test #3. Seed an AI-originated approved rollout
// (default exclude_from_learning=false). First assembleVerdicts(G)
// returns 1 example. Flip ExcludeFromLearning=true on the rollout.
// Second assembleVerdicts(G) returns 0 examples. Proves the bridge
// re-reads on every call — no stale cache between AI proposals.
func TestExcludeFromLearning_ChangedAfterCreation_AffectsNextProposal(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	now := time.Now()
	r := approvedRollout(
		"rlt_toggle", gid,
		"reasoning that's initially fine",
		"good plan, ship it",
		now.Add(-1*time.Hour),
	)
	// Default-false at creation matches the storage column default.
	r.ExcludeFromLearning = false
	store.verdicts = map[string][]*types.Rollout{gid: {r}}

	b := newBridgeForVerdictsTest(store)

	// First call: rollout is included.
	examples, ids, err := b.assembleVerdicts(context.Background(), gid)
	require.NoError(t, err)
	require.Len(t, examples, 1, "first call: rollout must surface")
	assert.Equal(t, "rlt_toggle", examples[0].RolloutID)
	assert.Equal(t, []string{"rlt_toggle"}, ids)

	// Operator flips the flag — simulating the
	// POST /api/v1/rollouts/:id/exclude-from-learning handler having
	// updated the same row's column.
	r.ExcludeFromLearning = true

	// Second call: rollout is excluded. The bridge must re-read the
	// store; if there's a stale cache between calls the test sees a
	// non-empty result and fails.
	examples, ids, err = b.assembleVerdicts(context.Background(), gid)
	require.NoError(t, err)
	assert.Empty(t, examples,
		"second call: rollout must be filtered out — proves no stale cache")
	assert.Empty(t, ids,
		"audit verdict_examples_used must be empty when the only candidate is excluded")
}

// TestExcludeFromLearning_GroupOptOutStillRespected is acceptance
// test #5. Group G has LearnFromVerdicts=false. Seed an approved
// rollout with exclude_from_learning=false (the rollout itself is
// NOT individually suppressed; only the group is). assembleVerdicts
// returns 0 examples. Proves the two filters compose correctly:
// the group-level check short-circuits BEFORE the per-rollout
// filter, so the per-rollout flag is irrelevant when the group flag
// is off.
//
// The bridge's existing v0.89.17 code is the load-bearing piece
// here — this test pins the ordering so a refactor that
// accidentally flips it (per-rollout filter first, then group
// check) breaks loudly.
func TestExcludeFromLearning_GroupOptOutStillRespected(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	// Group is explicitly opted OUT of the learning loop.
	store.groups[gid].LearnFromVerdicts = false

	now := time.Now()
	r := approvedRollout(
		"rlt_group_off", gid,
		"perfectly safe reasoning",
		"good plan, ship it",
		now.Add(-2*time.Hour),
	)
	// The rollout itself is NOT individually suppressed — only the
	// group is. If the per-rollout filter were applied before the
	// group check, this row would survive and the test would fail.
	r.ExcludeFromLearning = false
	store.verdicts = map[string][]*types.Rollout{gid: {r}}

	b := newBridgeForVerdictsTest(store)
	examples, ids, err := b.assembleVerdicts(context.Background(), gid)
	require.NoError(t, err)
	assert.Empty(t, examples,
		"group-level opt-out must short-circuit before the per-rollout filter — "+
			"a non-suppressed rollout on an opted-out group must still surface zero examples")
	assert.Empty(t, ids,
		"audit verdict_examples_used must be empty when the group is opted out")

	// Sanity check: the fake's ListAIVerdictsForGroup must NOT have
	// been called — the group check short-circuits before the
	// storage query. If the group filter were applied after the
	// storage query, the fake would have captured a groupID and the
	// assertion below would fail.
	assert.Empty(t, store.lastVerdictsGroupID,
		"group-level opt-out should short-circuit before the storage query")
}
