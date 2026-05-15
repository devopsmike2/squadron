// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "fmt"

// RolloutTemplate is a reusable rollout shape — stages + abort criteria
// + a default name — that an operator can pick from in the create form
// instead of building everything from scratch.
//
// Templates are one step bigger than AbortCriteriaRecipe: a recipe
// prefills the criteria fields, a template prefills criteria AND the
// stage list AND a sensible name. The operator only has to supply the
// group_id and target_config_id before clicking Start.
//
// Templates are server-defined and curated. Adding one is a code
// change, same rationale as the recipe cookbook: the value of the list
// comes from being short and meaningful.
//
// v1 ships only percent-mode templates. Label-mode rollouts depend on
// the operator's actual label scheme, so templating them would either
// require placeholder fields (clunky) or assume label conventions we
// don't enforce (fragile). Operators reaching for label mode already
// know what they want; the template picker is for operators who don't
// want to think about dwells and percentages.
type RolloutTemplate struct {
	// ID is the stable machine identifier (lowercase kebab-case). UI
	// passes this back as the operator's choice so the picker
	// highlight survives re-renders.
	ID string `json:"id"`

	// Name is the human-readable label shown in the picker.
	Name string `json:"name"`

	// Description: one sentence explaining the template at a glance.
	Description string `json:"description"`

	// WhenToUse: longer paragraph for tooltips/helper text.
	WhenToUse string `json:"when_to_use"`

	// DefaultName is what the create-form's name field is prefilled
	// with when the operator picks this template. Operators are
	// expected to edit it (typically appending a version or target
	// config id) before submitting.
	DefaultName string `json:"default_name"`

	// Stages is the full stage list, in order. Each stage carries its
	// own mode/percentage/dwell. All stages in a template must share
	// a mode (validateRolloutInput rejects mixed modes).
	Stages []RolloutStage `json:"stages"`

	// AbortCriteria is the criteria bundle the template ships with.
	// Operators can override individual fields in the form, same as
	// when picking from the recipe cookbook.
	AbortCriteria RolloutAbortCriteria `json:"abort_criteria"`
}

// RolloutTemplates returns the built-in template gallery in a stable
// order (from cautious to fast). Adding a template is a code change so
// the list stays curated.
func RolloutTemplates() []RolloutTemplate {
	return []RolloutTemplate{
		{
			ID:          "cautious-percent-ramp",
			Name:        "Cautious percent ramp",
			Description: "1% → 10% → 50% → 100% with long dwells. Pairs with strict-canary criteria.",
			WhenToUse: "Use for production pushes where a regression has cross-team " +
				"blast radius. The 1% first stage gives a single agent's worth " +
				"of signal before widening; long dwells (5-10 minutes each) let " +
				"you catch slow-burn issues like memory creep or queue backup.",
			DefaultName: "cautious rollout",
			Stages: []RolloutStage{
				{Mode: RolloutStageModePercent, Percentage: 1, DwellSeconds: 300},
				{Mode: RolloutStageModePercent, Percentage: 10, DwellSeconds: 300},
				{Mode: RolloutStageModePercent, Percentage: 50, DwellSeconds: 600},
				{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 300},
			},
			AbortCriteria: RolloutAbortCriteria{
				MaxDriftedAgents:           0,
				MaxErrorLogsPerMinute:      5,
				MinDwellSecondsBeforeAbort: 30,
			},
		},
		{
			ID:          "standard-percent-ramp",
			Name:        "Standard percent ramp",
			Description: "10% → 50% → 100% with medium dwells. The default sane production rollout.",
			WhenToUse: "Use for routine production config changes: receiver tuning, " +
				"processor batch adjustments, label-routing tweaks. Two-minute " +
				"dwells give the canary time to stabilize and report drift " +
				"without making the whole rollout feel sluggish.",
			DefaultName: "standard rollout",
			Stages: []RolloutStage{
				{Mode: RolloutStageModePercent, Percentage: 10, DwellSeconds: 120},
				{Mode: RolloutStageModePercent, Percentage: 50, DwellSeconds: 180},
				{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 120},
			},
			AbortCriteria: RolloutAbortCriteria{
				MaxDriftedAgents:           1,
				MaxErrorLogsPerMinute:      20,
				MinDwellSecondsBeforeAbort: 60,
			},
		},
		{
			ID:          "fast-percent-ramp",
			Name:        "Fast percent ramp",
			Description: "25% → 100% with short dwells. For staging or hotfixes.",
			WhenToUse: "Use for non-prod environments or for prod hotfixes where " +
				"you've already verified the config in staging and want to " +
				"close the regression window quickly. The 25%/30s first stage " +
				"still gives a useful early-signal point without the rollout " +
				"feeling like it crawled.",
			DefaultName: "fast rollout",
			Stages: []RolloutStage{
				{Mode: RolloutStageModePercent, Percentage: 25, DwellSeconds: 30},
				{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 30},
			},
			AbortCriteria: RolloutAbortCriteria{
				MaxDriftedAgents:           3,
				MaxErrorLogsPerMinute:      100,
				MinDwellSecondsBeforeAbort: 30,
			},
		},
		{
			ID:          "big-bang",
			Name:        "Big-bang push",
			Description: "Single 100% stage, no auto-abort. Use for emergency rollbacks or trivial changes.",
			WhenToUse: "Use when speed beats safety: rolling back a broken config to " +
				"the previous known-good, or pushing a one-character fix that " +
				"can't fail in any interesting way. The single stage means " +
				"no staged signal, and the manual-abort-only criteria mean " +
				"the operator is the safety net. Pair with a tight feedback " +
				"loop (logs/metrics open in another tab).",
			DefaultName: "emergency push",
			Stages: []RolloutStage{
				{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
			},
			AbortCriteria: RolloutAbortCriteria{
				MaxDriftedAgents:           999999,
				MaxErrorLogsPerMinute:      0,
				MinDwellSecondsBeforeAbort: 0,
			},
		},
	}
}

// RolloutTemplateByID returns the template with the given ID, or an
// error if no such template is registered. The UI passes the picked
// template's ID back so the form's "selected" state survives renders.
func RolloutTemplateByID(id string) (RolloutTemplate, error) {
	for _, t := range RolloutTemplates() {
		if t.ID == id {
			return t, nil
		}
	}
	return RolloutTemplate{}, fmt.Errorf("no rollout template with id %q", id)
}
