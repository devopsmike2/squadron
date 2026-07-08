// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package duckdb

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// parseMemoryLimitBytes converts DuckDB's current_setting('memory_limit')
// string (e.g. "476.8 MiB", "512.0 MiB", "6.9 GiB") into a byte count so a
// test can assert a bounded ceiling regardless of DuckDB's normalization.
func parseMemoryLimitBytes(t *testing.T, s string) float64 {
	t.Helper()
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) != 2 {
		t.Fatalf("unexpected memory_limit format: %q", s)
	}
	val, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		t.Fatalf("parse memory_limit value %q: %v", fields[0], err)
	}
	mult := map[string]float64{
		"KiB": 1 << 10,
		"MiB": 1 << 20,
		"GiB": 1 << 30,
		"TiB": 1 << 40,
		"KB":  1e3,
		"MB":  1e6,
		"GB":  1e9,
		"TB":  1e12,
		"B":   1,
	}[fields[1]]
	if mult == 0 {
		t.Fatalf("unknown memory_limit unit in %q", s)
	}
	return val * mult
}

func currentMemoryLimit(t *testing.T, s *Storage) string {
	t.Helper()
	var got string
	if err := s.db.QueryRow("SELECT current_setting('memory_limit')").Scan(&got); err != nil {
		t.Fatalf("read current_setting('memory_limit'): %v", err)
	}
	return got
}

// TestMemoryLimitBounded confirms that passing a memoryLimit actually caps the
// DuckDB engine: with "512MB" the effective setting must parse to <= ~1GB,
// which is well below the unbounded default (~80% of host RAM).
func TestMemoryLimitBounded(t *testing.T) {
	s, err := NewStorage(filepath.Join(t.TempDir(), "telemetry.db"), "512MB", zap.NewNop())
	if err != nil {
		t.Fatalf("NewStorage with memoryLimit: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got := currentMemoryLimit(t, s)
	byteVal := parseMemoryLimitBytes(t, got)
	const oneGiB = float64(1 << 30)
	if byteVal > oneGiB {
		t.Fatalf("memory_limit not bounded: current_setting=%q (%.0f bytes) exceeds 1GiB", got, byteVal)
	}
	t.Logf("bounded memory_limit current_setting=%q (%.0f bytes)", got, byteVal)
}

// TestMemoryLimitDefault confirms empty memoryLimit is a no-op: the store opens
// cleanly and DuckDB keeps its (large, host-derived) default ceiling.
func TestMemoryLimitDefault(t *testing.T) {
	s, err := NewStorage(filepath.Join(t.TempDir(), "telemetry.db"), "", zap.NewNop())
	if err != nil {
		t.Fatalf("NewStorage with empty memoryLimit: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got := currentMemoryLimit(t, s)
	if strings.TrimSpace(got) == "" {
		t.Fatalf("expected a default memory_limit, got empty")
	}
	t.Logf("default memory_limit current_setting=%q", got)
}
