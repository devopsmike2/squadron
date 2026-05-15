// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRolloutTemplates_AllWellFormed(t *testing.T) {
	// Same shape as the recipe well-formedness test — empty fields
	// would render a broken picker entry, and duplicate IDs would
	// let RolloutTemplateByID return the wrong template.
	templates := RolloutTemplates()
	require.NotEmpty(t, templates, "template gallery must ship at least one entry")

	seen := map[string]bool{}
	for _, tpl := range templates {
		t.Run(tpl.ID, func(t *testing.T) {
			assert.NotEmpty(t, tpl.ID)
			assert.NotEmpty(t, tpl.Name)
			assert.NotEmpty(t, tpl.Description)
			assert.NotEmpty(t, tpl.WhenToUse)
			assert.NotEmpty(t, tpl.DefaultName)
			require.NotEmpty(t, tpl.Stages, "template must have at least one stage")

			assert.False(t, seen[tpl.ID], "duplicate template id %q", tpl.ID)
			seen[tpl.ID] = true
		})
	}
}

func TestRolloutTemplates_PassValidation(t *testing.T) {
	// Every template must produce a RolloutInput that passes the
	// validator. If a template ever fails validation, picking it from
	// the UI would yield a 400 — exactly the footgun the gallery is
	// supposed to prevent.
	for _, tpl := range RolloutTemplates() {
		t.Run(tpl.ID, func(t *testing.T) {
			in := RolloutInput{
				Name:           tpl.DefaultName,
				GroupID:        "group-validation-fixture",
				TargetConfigID: "cfg-validation-fixture",
				Stages:         tpl.Stages,
				AbortCriteria:  tpl.AbortCriteria,
			}
			err := validateRolloutInput(in)
			assert.NoError(t, err, "template %q must produce a valid RolloutInput", tpl.ID)
		})
	}
}

func TestRolloutTemplateByID_Found(t *testing.T) {
	// Spot-check a known ID so a future rename doesn't silently break
	// wire compat with deployed UIs.
	tpl, err := RolloutTemplateByID("standard-percent-ramp")
	require.NoError(t, err)
	assert.Equal(t, "Standard percent ramp", tpl.Name)
	assert.Equal(t, 3, len(tpl.Stages))
}

func TestRolloutTemplateByID_NotFound(t *testing.T) {
	_, err := RolloutTemplateByID("does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no rollout template")
}

func TestRolloutTemplates_SingleModePerTemplate(t *testing.T) {
	// validateRolloutInput rejects mixed-mode rollouts. The
	// validation test above would already catch a mixed-mode
	// template, but we assert the property explicitly here so a
	// future maintainer hits a clearer failure than "unexpected 400".
	for _, tpl := range RolloutTemplates() {
		t.Run(tpl.ID, func(t *testing.T) {
			modes := map[RolloutStageMode]bool{}
			for _, st := range tpl.Stages {
				modes[st.Mode] = true
			}
			assert.Equal(t, 1, len(modes), "template %q must use one stage mode (mixed modes are rejected at create time)", tpl.ID)
		})
	}
}
