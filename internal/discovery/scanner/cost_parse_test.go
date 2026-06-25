// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Cost-correlation substrate slice 6 chunk 5 (v0.89.187) — tests for
// the canonical ParseDecimalToMicroUSD shared by every per-cloud cost
// reader.

func TestParseDecimalToMicroUSD(t *testing.T) {
	cases := []struct {
		in   string
		want MicroUSD
		err  bool
	}{
		{"0", 0, false},
		{"1", 1_000_000, false},
		{"0.01", 10_000, false},
		{"12.345678", 12_345_678, false},
		{"12.3456789", 12_345_678, false}, // 7th decimal truncated
		{"100.5", 100_500_000, false},
		{".5", 500_000, false},
		{"-3.25", -3_250_000, false},
		{"+4.00", 4_000_000, false},
		{"  4.00 ", 4_000_000, false},
		{`"42.50"`, 42_500_000, false}, // JSON raw-number token with quotes
		{` "0.000001" `, 1, false},
		{"123456.789012", 123_456_789_012, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1.2x", 0, true},
	}
	for _, c := range cases {
		got, err := ParseDecimalToMicroUSD(c.in)
		if c.err {
			assert.Error(t, err, "input %q should error", c.in)
			continue
		}
		assert.NoError(t, err, "input %q", c.in)
		assert.Equal(t, c.want, got, "input %q", c.in)
	}
}

// Round-trip sanity: micro-USD math stays exact (no float drift) for a
// sum of many small amounts.
func TestParseDecimalToMicroUSD_ExactSummation(t *testing.T) {
	var total MicroUSD
	for i := 0; i < 1000; i++ {
		v, err := ParseDecimalToMicroUSD("0.01") // a cent
		assert.NoError(t, err)
		total += v
	}
	assert.Equal(t, MicroUSD(10_000_000), total, "1000 x $0.01 = exactly $10.00, no drift")
}
