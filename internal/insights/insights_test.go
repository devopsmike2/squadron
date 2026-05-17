// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"math"
	"math/big"
	"testing"
)

// TestInt64Of_HandlesBigInt is a regression guard for the v0.24 bug
// where /api/v1/insights/volume returned bytes:0 even though
// otlp_batches had real rows. Root cause: DuckDB widens SUM(BIGINT)
// to HUGEINT, which the marcboeker/go-duckdb driver returns as
// *big.Int — and the original int64Of switch fell through to 0.
// We now CAST to BIGINT in SQL *and* accept *big.Int here defensively.
// If a future driver upgrade or query shape sneaks a HUGEINT through,
// this test catches it before it ships.
func TestInt64Of_HandlesBigInt(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int64
	}{
		{"int64 passthrough", int64(42), 42},
		{"int32 widening", int32(7), 7},
		{"int widening", int(99), 99},
		{"float64 floor", float64(3.7), 3},
		{"uint64 to int64", uint64(1234), 1234},
		{"nil to zero", nil, 0},
		{"unknown type to zero", "not-a-number", 0},
		// The real regression cases:
		{"*big.Int fits", big.NewInt(34400624), 34400624},
		{"*big.Int zero", big.NewInt(0), 0},
		{"big.Int (value, not pointer)", *big.NewInt(123), 123},
		{"*big.Int negative", big.NewInt(-500), -500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := int64Of(tc.in)
			if got != tc.want {
				t.Errorf("int64Of(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestInt64Of_BigIntSaturates verifies that a HUGEINT exceeding
// int64 range doesn't silently wrap or panic — it saturates to
// MaxInt64. In practice telemetry byte counts won't exceed 2^63,
// but the saturation behavior should be predictable.
func TestInt64Of_BigIntSaturates(t *testing.T) {
	huge := new(big.Int)
	huge.SetString("99999999999999999999999999", 10)
	got := int64Of(huge)
	if got != math.MaxInt64 {
		t.Errorf("int64Of(huge) = %d, want MaxInt64 (%d)", got, math.MaxInt64)
	}
}

// TestWindow_AsDuration covers the three supported windows plus an
// unknown value. The handler 400s on err — verify err is non-nil.
func TestWindow_AsDuration(t *testing.T) {
	if d, err := Window5m.AsDuration(); err != nil || d.Minutes() != 5 {
		t.Errorf("5m: got (%v, %v)", d, err)
	}
	if d, err := Window1h.AsDuration(); err != nil || d.Hours() != 1 {
		t.Errorf("1h: got (%v, %v)", d, err)
	}
	if d, err := Window24h.AsDuration(); err != nil || d.Hours() != 24 {
		t.Errorf("24h: got (%v, %v)", d, err)
	}
	if _, err := Window("7d").AsDuration(); err == nil {
		t.Errorf("expected error for unsupported window")
	}
}

// TestSignalOrder anchors the stable ordering the API contract
// promises: traces, then metrics, then logs.
func TestSignalOrder(t *testing.T) {
	if signalOrder(SignalTraces) >= signalOrder(SignalMetrics) {
		t.Errorf("traces should sort before metrics")
	}
	if signalOrder(SignalMetrics) >= signalOrder(SignalLogs) {
		t.Errorf("metrics should sort before logs")
	}
}

// TestAccumulateKeyBytes_Basic verifies the minimal JSON scanner
// produces sensible byte attributions for the shapes we see in
// real OTLP attribute payloads.
func TestAccumulateKeyBytes_Basic(t *testing.T) {
	dst := map[string]int64{}
	accumulateKeyBytes(`{"http.url":"https://example.com/foo","http.status_code":200}`, dst)
	if dst["http.url"] == 0 {
		t.Errorf("expected http.url to be attributed, got 0")
	}
	if dst["http.status_code"] == 0 {
		t.Errorf("expected http.status_code to be attributed, got 0")
	}
	// http.url's value is much longer than the status code's, so it
	// should dominate.
	if dst["http.url"] <= dst["http.status_code"] {
		t.Errorf("http.url (%d) should outsize http.status_code (%d)",
			dst["http.url"], dst["http.status_code"])
	}
}

func TestAccumulateKeyBytes_MalformedSilent(t *testing.T) {
	// Garbage in: tally stays empty rather than panicking.
	dst := map[string]int64{}
	accumulateKeyBytes(`this is not json`, dst)
	accumulateKeyBytes(``, dst)
	accumulateKeyBytes(`{`, dst)
	if len(dst) != 0 {
		t.Errorf("expected empty tally from malformed inputs, got %v", dst)
	}
}
