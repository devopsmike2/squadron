// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package pipelinehealth

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSortedKV_EmptyIsNonNilSlice pins the serialization contract behind the
// agent-detail drawer: a label-less sample (e.g. the otelcol_process_*
// self-metrics emitted when collector self-telemetry is enabled) must marshal
// its `labels` field as `[]`, never `null`. The UI iterates MetricRow.labels
// directly, so a null there previously crashed the whole agent-detail view.
func TestSortedKV_EmptyIsNonNilSlice(t *testing.T) {
	for _, in := range []map[string]string{nil, {}} {
		got := sortedKV(in)
		if got == nil {
			t.Fatalf("sortedKV(%v) returned nil slice; want non-nil empty slice", in)
		}
		if len(got) != 0 {
			t.Fatalf("sortedKV(%v) = %v; want empty slice", in, got)
		}
	}
}

// TestMetricRow_LabelLessMarshalsAsEmptyArray asserts the end-to-end JSON shape
// the pipeline-health endpoint returns for a label-less sample.
func TestMetricRow_LabelLessMarshalsAsEmptyArray(t *testing.T) {
	row := MetricRow{
		Labels: sortedKV(nil), // label-less otelcol_process_* sample
		Value:  12.5,
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal MetricRow: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"labels":[]`) {
		t.Fatalf("MetricRow JSON = %s; want labels serialized as []", got)
	}
	if strings.Contains(got, `"labels":null`) {
		t.Fatalf("MetricRow JSON = %s; labels must not be null", got)
	}
}
