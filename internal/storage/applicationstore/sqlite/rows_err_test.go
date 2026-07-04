// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestListAlertRules_ReturnsAllRows is a happy-path guard for the row-iteration
// sweep that made the List/Query storage methods return rows.Err() instead of a
// bare nil (so a mid-iteration driver error can no longer masquerade as a
// complete result set). ListAlertRules is the control-path case: a silently
// truncated rule set would stop some alerts from firing. This test pins that a
// full, clean iteration still returns every row and a nil error — i.e. the
// nil -> rows.Err() change did not regress the normal read path.
//
// Note: forcing rows.Err() != nil against the real SQLite driver would require a
// fault-injecting wrapper driver (the repo has no sql-mock dependency); that is
// disproportionate for a one-token change whose happy-path behavior is provably
// identical (rows.Err() is nil after a successful scan). This guards the edit
// against an accidental variable/return-shape mistake and documents the intent.
func TestListAlertRules_ReturnsAllRows(t *testing.T) {
	store, err := NewSQLiteStorage(makeTempDB(t), zap.NewNop())
	require.NoError(t, err)
	ctx := context.Background()

	const n = 5
	for i := 0; i < n; i++ {
		rule := &types.AlertRule{
			ID:                fmt.Sprintf("rule-%02d", i),
			Name:              fmt.Sprintf("rule-%02d", i),
			Query:             "avg(cpu)",
			ThresholdOperator: types.ThresholdGreater,
			ThresholdValue:    float64(i),
			IntervalSeconds:   60,
			Severity:          types.AlertSeverityWarning,
			Enabled:           true,
		}
		require.NoError(t, store.CreateAlertRule(ctx, rule))
	}

	rules, err := store.ListAlertRules(ctx)
	require.NoError(t, err)
	require.Len(t, rules, n, "every inserted rule must be returned; a truncated list would silently disable alerts")

	// Confirm the rows are the ones we inserted (ordered by name ASC).
	for i, r := range rules {
		require.Equal(t, fmt.Sprintf("rule-%02d", i), r.ID)
	}
}
