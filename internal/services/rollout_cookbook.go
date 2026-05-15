// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "fmt"

// AbortCriteriaRecipe is one named bundle of abort-criteria settings.
//
// Recipes exist because operators rarely think in "max drifted agents +
// max errors per minute" — they think in failure modes ("we're paranoid
// about this push", "this is just a staging shake-out"). The cookbook
// gives a small, curated set of recipes operators can pick instead of
// hand-tuning every field. The picked recipe prefills the create form;
// operators can still tweak any value afterward.
//
// Recipes are NOT persisted, NOT operator-editable, and NOT tied to a
// specific rollout — they're a UI affordance that returns
// RolloutAbortCriteria values. Adding a new recipe is a code change so
// they stay curated.
type AbortCriteriaRecipe struct {
	// ID is the stable machine identifier. Lowercase kebab-case. UI
	// stores this when an operator picks a recipe so the choice
	// survives re-renders and form-state mutations.
	ID string `json:"id"`

	// Name is the human-readable label shown in the picker.
	Name string `json:"name"`

	// Description is one sentence explaining when to reach for this
	// recipe. Shown as helper text under the picker.
	Description string `json:"description"`

	// WhenToUse is a longer explanation suitable for a tooltip — the
	// scenarios this recipe was designed for and the rough tradeoff
	// vs. other recipes.
	WhenToUse string `json:"when_to_use"`

	// Criteria is the actual abort-criteria values the recipe sets.
	Criteria RolloutAbortCriteria `json:"criteria"`
}

// AbortCriteriaRecipes returns the built-in cookbook in a stable order.
// The list is intentionally short — too many choices makes the picker
// useless. New recipes should earn their slot by matching an
// operationally-recognizable failure mode that the existing recipes
// don't already cover.
//
// The order is roughly from most-conservative to most-permissive so the
// UI can render them top-to-bottom without further sorting.
func AbortCriteriaRecipes() []AbortCriteriaRecipe {
	return []AbortCriteriaRecipe{
		{
			ID:          "strict-canary",
			Name:        "Strict canary",
			Description: "Zero tolerance — any drift or non-trivial error spike aborts immediately.",
			WhenToUse: "Use for high-risk pushes touching production telemetry pipelines, " +
				"new exporter configurations, or anything where a regression has " +
				"cross-team blast radius. Pairs well with a small percent-mode " +
				"first stage (1-5%) or a single named canary host in label mode.",
			Criteria: RolloutAbortCriteria{
				MaxDriftedAgents:           0,
				MaxErrorLogsPerMinute:      5,
				MinDwellSecondsBeforeAbort: 30,
			},
		},
		{
			ID:          "standard-production",
			Name:        "Standard production",
			Description: "Tolerant of brief restart noise; aborts on sustained issues.",
			WhenToUse: "The default for routine production config changes — receiver " +
				"tuning, processor batch-size adjustments, label-routing tweaks. " +
				"Allows one transient drift (the agent reconnecting after a " +
				"config push commonly bounces) and a modest error budget while " +
				"new agents settle.",
			Criteria: RolloutAbortCriteria{
				MaxDriftedAgents:           1,
				MaxErrorLogsPerMinute:      20,
				MinDwellSecondsBeforeAbort: 60,
			},
		},
		{
			ID:          "permissive-staging",
			Name:        "Permissive staging",
			Description: "Loose criteria for non-prod environments where churn is expected.",
			WhenToUse: "Use in staging or pre-prod where agents may be intentionally " +
				"unstable (chaos testing, frequent restarts, partial deployments). " +
				"Still aborts on egregious failure but won't flap on the kind of " +
				"noise you'd see in a CI integration test.",
			Criteria: RolloutAbortCriteria{
				MaxDriftedAgents:           3,
				MaxErrorLogsPerMinute:      100,
				MinDwellSecondsBeforeAbort: 30,
			},
		},
		{
			ID:          "drift-only",
			Name:        "Drift-only",
			Description: "Aborts on any agent drifting; ignores error rates.",
			WhenToUse: "Use when telemetry error rates aren't a reliable signal " +
				"(e.g. the new config intentionally generates warnings, or this " +
				"is an OpAMP-only change with no log volume to compare). " +
				"Falls back to drift as the sole abort signal.",
			Criteria: RolloutAbortCriteria{
				MaxDriftedAgents:           0,
				MaxErrorLogsPerMinute:      0, // 0 disables the check
				MinDwellSecondsBeforeAbort: 30,
			},
		},
		{
			ID:          "manual-abort-only",
			Name:        "Manual abort only",
			Description: "No auto-abort. Operator must hit Abort if something goes wrong.",
			WhenToUse: "Use for highly experimental rollouts where the auto-abort " +
				"heuristics are likely to false-positive (test fixtures, " +
				"intentionally-broken configs for runbook drills). The operator " +
				"watches the rollout and aborts by hand if needed. Squadron " +
				"will still surface drift and errors in the dashboard.",
			Criteria: RolloutAbortCriteria{
				// A very large MaxDriftedAgents effectively disables the
				// drift check; 0 MaxErrorLogsPerMinute disables the log check.
				MaxDriftedAgents:           999999,
				MaxErrorLogsPerMinute:      0,
				MinDwellSecondsBeforeAbort: 0,
			},
		},
	}
}

// AbortCriteriaRecipeByID returns the recipe with the given ID, or an
// error if no such recipe is registered. Used by callers that want to
// resolve a stored picker value back to its criteria — the wire payload
// from the UI carries the recipe ID for round-tripping, but the actual
// criteria are server-of-record.
func AbortCriteriaRecipeByID(id string) (AbortCriteriaRecipe, error) {
	for _, r := range AbortCriteriaRecipes() {
		if r.ID == id {
			return r, nil
		}
	}
	return AbortCriteriaRecipe{}, fmt.Errorf("no abort-criteria recipe with id %q", id)
}
