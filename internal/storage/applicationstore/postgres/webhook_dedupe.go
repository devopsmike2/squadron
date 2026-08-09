// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"
	"time"
)

// webhook_dedupe.go — the Postgres port of webhook replay protection (v0.89.30,
// #649), ADR 0033 slice 6. Semantics mirror the sqlite backend EXACTLY:
//
//   - RecordWebhookDelivery INSERTs the delivery_id and reads back received_at.
//     The DB stamps received_at (column DEFAULT now()) on the fresh insert. The
//     INSERT ... ON CONFLICT DO NOTHING + RowsAffected is the fresh-vs-replay
//     signal: RowsAffected==1 → fresh (firstTime=true, receivedAt = the just-
//     stamped time); RowsAffected==0 → replay (firstTime=false, receivedAt = the
//     ORIGINAL delivery's timestamp, read back after the no-op insert). The
//     lookup-after-insert satisfies the receiver's "I need the prior timestamp on
//     replay" contract. ON CONFLICT DO NOTHING is atomic, so two concurrent
//     replays of the same delivery_id can't both observe firstTime=true.
//   - GCWebhookDeliveries deletes rows older than the cutoff and returns the
//     count; re-running with a stale cutoff is a clean no-op (deleted=0).
//
// This table is GLOBAL (no tenant column), mirroring the sqlite schema — it
// carries no per-tenant data.

func (s *Storage) RecordWebhookDelivery(ctx context.Context, deliveryID, eventType string) (bool, time.Time, error) {
	if deliveryID == "" {
		return false, time.Time{}, fmt.Errorf("delivery_id required")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_delivery_dedupe (delivery_id, event_type) VALUES ($1,$2)
		 ON CONFLICT (delivery_id) DO NOTHING`, deliveryID, eventType)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("record webhook delivery: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, time.Time{}, fmt.Errorf("rows affected: %w", err)
	}
	// Always read back received_at so both branches return an honest timestamp: the
	// just-written row on the fresh path, the original delivery's stamp on replay.
	var receivedAt time.Time
	if err := s.db.QueryRowContext(ctx,
		`SELECT received_at FROM webhook_delivery_dedupe WHERE delivery_id = $1`, deliveryID).Scan(&receivedAt); err != nil {
		return false, time.Time{}, fmt.Errorf("read received_at: %w", err)
	}
	return rows == 1, receivedAt, nil
}

func (s *Storage) GCWebhookDeliveries(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhook_delivery_dedupe WHERE received_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("gc webhook deliveries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}
