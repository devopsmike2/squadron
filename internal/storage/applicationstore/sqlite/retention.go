// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"fmt"
	"time"
)

// retention.go — GC predicates for operator-activity tables that
// otherwise grow forever. cost_spike_events gains a row per detected
// anomaly and recommendation_outcomes a row per Apply click; neither
// had a prune path, so a long-lived deployment accumulates them without
// bound (and GET /savings/realized scans the full outcomes table). A
// background sweep in cmd/all-in-one calls these on a 24h ticker.

// DeleteClosedCostSpikeEventsBefore removes RESOLVED cost-spike rows
// (ended_at IS NOT NULL) whose ended_at predates the cutoff. Open spikes
// (ended_at IS NULL) are NEVER deleted regardless of age — an unresolved
// anomaly must stay visible on the alerts panel until the detector closes
// it. Returns the number of rows removed.
func (s *Storage) DeleteClosedCostSpikeEventsBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM cost_spike_events WHERE ended_at IS NOT NULL AND ended_at < ?`,
		before.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("delete closed cost_spike_events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteRecommendationOutcomesBefore removes recommendation-outcome rows
// applied before the cutoff. These are the persisted "operator clicked
// Apply" records the Savings/realized surface reads; pruning old ones
// bounds both table size and that endpoint's full-table scan. Returns the
// number of rows removed.
func (s *Storage) DeleteRecommendationOutcomesBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM recommendation_outcomes WHERE applied_at < ?`,
		before.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("delete recommendation_outcomes: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
