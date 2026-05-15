// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/storage/telemetrystore"
)

// TelemetryAdapter wraps a telemetrystore.Reader to satisfy the engine's
// TelemetryReader interface. Kept in the rollouts package so the
// telemetrystore.Reader interface doesn't need to grow for a single
// caller's specialty query.
type TelemetryAdapter struct {
	reader telemetrystore.Reader
}

// NewTelemetryAdapter wraps the given reader.
func NewTelemetryAdapter(reader telemetrystore.Reader) *TelemetryAdapter {
	return &TelemetryAdapter{reader: reader}
}

// CanaryErrorLogsPerMinute returns the rate of ERROR-or-higher log records
// per minute produced by the given agent ids since the given timestamp.
// "ERROR or higher" is defined as severity_number >= 17 per the OTLP spec
// (17 = ERROR, 21 = FATAL).
//
// Implementation note: DuckDB has the rows; we query directly via QueryRaw
// rather than the typed Reader.QueryLogs API because we want an aggregate
// COUNT, not the row data. The Reader interface doesn't currently expose
// an aggregate path.
func (a *TelemetryAdapter) CanaryErrorLogsPerMinute(ctx context.Context, agentIDs []uuid.UUID, since time.Time) (float64, error) {
	if len(agentIDs) == 0 {
		return 0, nil
	}
	minutes := time.Since(since).Minutes()
	if minutes < 0.05 {
		// Less than 3 seconds of window — rate would be artificially huge
		// from any single record. Wait for more data.
		return 0, nil
	}

	// Build an `agent_id IN (...)` clause with positional parameters.
	placeholders := make([]string, len(agentIDs))
	args := make([]any, 0, len(agentIDs)+1)
	for i, id := range agentIDs {
		placeholders[i] = "?"
		args = append(args, id.String())
	}
	args = append(args, since)

	q := fmt.Sprintf(`
		SELECT COUNT(*) AS n
		FROM logs
		WHERE agent_id IN (%s)
		  AND timestamp >= ?
		  AND severity_number >= 17
	`, strings.Join(placeholders, ","))

	rows, err := a.reader.QueryRaw(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("canary error log query: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	// QueryRaw returns []map[string]any; DuckDB may surface the count as
	// int64, uint64, or float64 depending on driver settings. Coerce
	// defensively.
	var count float64
	switch v := rows[0]["n"].(type) {
	case int64:
		count = float64(v)
	case uint64:
		count = float64(v)
	case float64:
		count = v
	case int:
		count = float64(v)
	default:
		return 0, nil
	}
	return count / minutes, nil
}
