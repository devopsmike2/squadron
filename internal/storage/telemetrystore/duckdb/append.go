// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package duckdb

import (
	"context"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/marcboeker/go-duckdb"
	"go.uber.org/zap"
)

// appendRows runs fill against a DuckDB Appender on table, inside an
// explicit transaction on a single pinned pool connection.
//
// Why the Appender: the v0.89 OTLP ingest stress pass
// (docs/stress-tests/otlp-ingest-v0.89.md, finding 3) measured the
// per-row prepared-statement path at ~6-11k items/s — every row was a
// cgo round-trip, and go-duckdb v1.8.3's parameter bind is quadratic
// in the number of placeholders, so multi-row VALUES doesn't scale
// either. The Appender writes columnar 2048-row chunks and is
// DuckDB's documented bulk path for exactly this shape of load.
//
// Why the explicit transaction (load-bearing for the worker pool's
// retry logic): in go-duckdb v1.8.3 BOTH Appender.Close and the
// appender's internal auto-flush push buffered rows into the
// database — there is no "discard without flushing", and Close must
// always be called to free the C-side memory. Pinning the connection
// and wrapping the whole append in BEGIN/COMMIT means every flush
// (including the mandatory one in Close on the error path) lands
// inside the transaction: ROLLBACK discards it all. So on ANY error
// zero rows persist and the caller can retry the entire batch
// without duplicating data — regardless of batch size or flush
// boundaries. TestAppendRows_ErrorLeavesZeroRows pins this.
func (s *Storage) appendRows(ctx context.Context, table string, fill func(ap *duckdb.Appender) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	appendErr := conn.Raw(func(driverConn any) error {
		dc, ok := driverConn.(driver.Conn)
		if !ok {
			return fmt.Errorf("unexpected driver connection type %T", driverConn)
		}
		ap, err := duckdb.NewAppenderFromConn(dc, "", table)
		if err != nil {
			return fmt.Errorf("failed to create appender for %s: %w", table, err)
		}
		if err := fill(ap); err != nil {
			// Close is mandatory (frees C memory) and WILL flush the
			// buffered rows — into the open transaction, which the
			// rollback below discards.
			_ = ap.Close()
			return err
		}
		if err := ap.Close(); err != nil {
			return fmt.Errorf("failed to flush appender for %s: %w", table, err)
		}
		return nil
	})

	if appendErr != nil {
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			s.logger.Warn("appender rollback failed", zap.String("table", table), zap.Error(err))
		}
		return appendErr
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("failed to commit append to %s: %w", table, err)
	}
	return nil
}

// nullIfEmpty preserves the writers' existing NULL semantics: empty
// string columns (parent_span_id, trace_id, span_id) are stored as
// SQL NULL, not "". The appender maps untyped nil to NULL for any
// column type.
func nullIfEmpty(v string) driver.Value {
	if v == "" {
		return nil
	}
	return v
}
