// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAbortCriteriaRecipes_AllWellFormed(t *testing.T) {
	// Every recipe must be self-describing — the picker UI displays
	// name/description verbatim, so empty values would render a broken
	// list item.
	recipes := AbortCriteriaRecipes()
	require.NotEmpty(t, recipes, "cookbook must ship at least one recipe")

	seen := map[string]bool{}
	for _, r := range recipes {
		t.Run(r.ID, func(t *testing.T) {
			assert.NotEmpty(t, r.ID, "recipe id must not be empty")
			assert.NotEmpty(t, r.Name, "recipe name must not be empty")
			assert.NotEmpty(t, r.Description, "recipe description must not be empty")
			assert.NotEmpty(t, r.WhenToUse, "recipe when_to_use must not be empty")

			// Non-negative criteria values — the validator on
			// RolloutInput will reject negatives, so a recipe with
			// a negative field is dead-on-arrival.
			assert.GreaterOrEqual(t, r.Criteria.MaxDriftedAgents, 0)
			assert.GreaterOrEqual(t, r.Criteria.MaxErrorLogsPerMinute, 0)
			assert.GreaterOrEqual(t, r.Criteria.MinDwellSecondsBeforeAbort, 0)

			// Duplicate IDs would make AbortCriteriaRecipeByID
			// return the wrong recipe — the UI passes IDs back as
			// the operator's choice, so we need them to be unique.
			assert.False(t, seen[r.ID], "duplicate recipe id %q", r.ID)
			seen[r.ID] = true
		})
	}
}

func TestAbortCriteriaRecipeByID_Found(t *testing.T) {
	// Spot-check one well-known recipe ID so a future rename doesn't
	// silently break wire compat with already-deployed UIs.
	r, err := AbortCriteriaRecipeByID("standard-production")
	require.NoError(t, err)
	assert.Equal(t, "Standard production", r.Name)
}

func TestAbortCriteriaRecipeByID_NotFound(t *testing.T) {
	_, err := AbortCriteriaRecipeByID("does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no abort-criteria recipe")
}

func TestAbortCriteriaRecipes_PassesRolloutValidation(t *testing.T) {
	// Every recipe must produce criteria that pass our own validator.
	// If a recipe ever fails this, an operator picking it from the UI
	// would get a 400 from Create — a self-inflicted footgun we want
	// to catch at build time.
	for _, r := range AbortCriteriaRecipes() {
		t.Run(r.ID, func(t *testing.T) {
			in := RolloutInput{
				Name:           "validation-test",
				GroupID:        "group-a",
				TargetConfigID: "cfg-1",
				Stages: []RolloutStage{
					{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 30},
				},
				AbortCriteria: r.Criteria,
			}
			// validateRolloutInput is package-private and exactly
			// what Create() runs; calling it directly avoids
			// needing a fake store for this assertion.
			err := validateRolloutInput(in)
			assert.NoError(t, err, "recipe %q must produce criteria that pass validateRolloutInput", r.ID)
		})
	}
}
