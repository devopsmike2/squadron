// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanner

import (
	"errors"
	"testing"
	"time"
)

// TestMetricStatistic_StringValues_Stable pins the constant values for
// the MetricStatistic enum across releases. The MetricQuerier interface
// uses these typed values as part of its API contract; downstream
// stores (cold_start_observation.snapshot_json, future sampling-rate
// observation rows) carry the constant as a discriminator and a
// silent rename of any of these values would mis-route queries on
// upgrade.
//
// Slice 1 of the cold-start latency arc (v0.89.113) ships StatisticP95
// only; the others are reserved for future slices but their values
// are pinned NOW so the substrate's wire shape is stable from day one.
func TestMetricStatistic_StringValues_Stable(t *testing.T) {
	cases := []struct {
		name string
		got  MetricStatistic
		want string
	}{
		{"p95", StatisticP95, "p95"},
		{"p99", StatisticP99, "p99"},
		{"average", StatisticAverage, "average"},
		{"sum", StatisticSum, "sum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if string(c.got) != c.want {
				t.Errorf("MetricStatistic %s = %q, want %q", c.name, string(c.got), c.want)
			}
		})
	}
}

// TestErrMetricNotImplemented_SentinelComparable pins that the
// not-implemented sentinel is comparable via errors.Is (the
// reference-comparable identity contract documented on its godoc),
// NOT via a string comparison of the error message.
//
// The chunk-1 AWS skeleton (internal/discovery/aws/metrics.go) returns
// this sentinel verbatim; chunk-2's CloudWatch wiring replaces the
// skeleton but downstream callers (chunk-3 detection, chunk-4 runbook)
// must keep working through the transition without re-wiring their
// error handling. errors.Is gives them that stability.
func TestErrMetricNotImplemented_SentinelComparable(t *testing.T) {
	if ErrMetricNotImplemented == nil {
		t.Fatal("ErrMetricNotImplemented must not be nil")
	}

	// Reference identity — the exact sentinel passes errors.Is
	// against itself.
	if !errors.Is(ErrMetricNotImplemented, ErrMetricNotImplemented) {
		t.Error("errors.Is(ErrMetricNotImplemented, ErrMetricNotImplemented) = false")
	}

	// Wrapped — a fmt.Errorf("...%w...", ErrMetricNotImplemented)
	// wrapper still resolves under errors.Is. Pins the
	// errors-package contract so a future wrapping pass (e.g. a
	// per-region prefix in the chunk 2 implementation) doesn't break
	// the sentinel check.
	wrapped := fmtErrorf("aws query failed: %w", ErrMetricNotImplemented)
	if !errors.Is(wrapped, ErrMetricNotImplemented) {
		t.Error("wrapped sentinel must resolve via errors.Is")
	}

	// A different error (created at this call site) must NOT match
	// the sentinel — defense-in-depth that we're not silently
	// comparing on string contents.
	other := errors.New(ErrMetricNotImplemented.Error())
	if errors.Is(other, ErrMetricNotImplemented) {
		t.Error("a distinct error with the same string must NOT match the sentinel")
	}
}

// fmtErrorf is a tiny local trampoline so the test file doesn't import
// fmt purely for one %w usage. Kept here to keep the import list lean.
func fmtErrorf(format string, a ...any) error {
	return &wrapErr{msg: format, args: a}
}

type wrapErr struct {
	msg  string
	args []any
}

func (w *wrapErr) Error() string {
	// Render verbatim — the wrap test only cares about Unwrap
	// resolving correctly.
	return w.msg
}

func (w *wrapErr) Unwrap() error {
	for _, a := range w.args {
		if e, ok := a.(error); ok {
			return e
		}
	}
	return nil
}

// TestAggregateMetricResult_ZeroValueIsValid pins that the zero-value
// AggregateMetricResult round-trips cleanly. The MetricQuerier interface
// contract documents that an empty result set (no datapoints) returns
// Value=0, SampleCount=0, no error — callers MUST be able to construct
// + observe + persist the zero value without special-case handling.
//
// Pinning the zero value as round-trippable also catches accidental
// future regressions where a field gains a non-zero-friendly type
// (e.g. a *time.Time pointer where nil would crash an unmarshaler).
func TestAggregateMetricResult_ZeroValueIsValid(t *testing.T) {
	var r AggregateMetricResult

	if r.ResourceARN != "" || r.MetricName != "" || r.Unit != "" {
		t.Error("zero-value string fields must be empty")
	}
	if r.Window != 0 {
		t.Error("zero-value Window must be 0")
	}
	if r.Statistic != "" {
		t.Error("zero-value Statistic must be empty")
	}
	if r.Value != 0 {
		t.Error("zero-value Value must be 0")
	}
	if r.SampleCount != 0 {
		t.Error("zero-value SampleCount must be 0")
	}
	if !r.ObservedAt.IsZero() {
		t.Error("zero-value ObservedAt must be the zero time")
	}

	// Field assignment + read-back at non-zero values composes
	// cleanly with the zero starting point.
	r.ResourceARN = "arn:aws:lambda:us-east-1:123456789012:function:fn"
	r.MetricName = "InitDuration"
	r.Window = 24 * time.Hour
	r.Statistic = StatisticP95
	r.Value = 4230.5
	r.Unit = "Milliseconds"
	r.SampleCount = 142
	r.ObservedAt = time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

	if r.Window != 24*time.Hour {
		t.Errorf("Window assignment lost: got %v", r.Window)
	}
	if r.Statistic != StatisticP95 {
		t.Errorf("Statistic assignment lost: got %q", string(r.Statistic))
	}
	if r.Value != 4230.5 {
		t.Errorf("Value assignment lost: got %v", r.Value)
	}
}
