// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStagesSpec_ValidThreeStages(t *testing.T) {
	stages, err := parseStagesSpec("10:120,50:180,100:120")
	require.NoError(t, err)
	require.Len(t, stages, 3)
	assert.Equal(t, "percent", stages[0].Mode)
	assert.Equal(t, 10, stages[0].Percentage)
	assert.Equal(t, 120, stages[0].DwellSeconds)
	assert.Equal(t, 100, stages[2].Percentage)
}

func TestParseStagesSpec_SingleStage(t *testing.T) {
	stages, err := parseStagesSpec("100:0")
	require.NoError(t, err)
	require.Len(t, stages, 1)
	assert.Equal(t, 100, stages[0].Percentage)
	assert.Equal(t, 0, stages[0].DwellSeconds)
}

func TestParseStagesSpec_RejectsMissingColon(t *testing.T) {
	_, err := parseStagesSpec("10,50")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "percent:dwell")
}

func TestParseStagesSpec_RejectsNonNumeric(t *testing.T) {
	_, err := parseStagesSpec("ten:120")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "percentage")

	_, err = parseStagesSpec("10:lots")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dwell")
}

func TestParseStagesSpec_IgnoresEmptyEntries(t *testing.T) {
	// Trailing commas (common from shell loops appending) shouldn't
	// blow up the parser.
	stages, err := parseStagesSpec("10:60,,50:120,")
	require.NoError(t, err)
	require.Len(t, stages, 2)
	assert.Equal(t, 10, stages[0].Percentage)
	assert.Equal(t, 50, stages[1].Percentage)
}

func TestMaskToken_ShowsPrefix(t *testing.T) {
	// maskToken's contract: enough chars to identify the token, but
	// not enough to authenticate as it from the terminal scrollback.
	masked := maskToken("sqd_abcdefghijklmnop")
	assert.Equal(t, "sqd_abcd…", masked)
}

func TestMaskToken_HandlesShortInput(t *testing.T) {
	assert.Equal(t, "<short>", maskToken("abc"))
}
