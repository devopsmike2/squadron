// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// TestMarshalScanResult_ForwardsServerlessErrorRate pins the v0.89.324 fix:
// the AWS serverless wire row (awsServerlessRow) now forwards the error-rate
// annotation the AnnotateServerlessWithErrorRate pass populated, which was
// previously dropped at marshal — hiding it from the AWS serverless inventory
// UI and zeroing the AWS error-rate axis in the Workload Health rollup. The
// sampling annotation is forwarded too but stays nil-elided (its annotator is
// dormant fleet-wide).
func TestMarshalScanResult_ForwardsServerlessErrorRate(t *testing.T) {
	exceeds := true
	rate := 0.27
	r := &scanner.Result{
		ScanID:    "s1",
		Provider:  "aws",
		AccountID: "123456789012",
		Regions:   []string{"us-east-1"},
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:                  "aws",
			Surface:                   "lambda",
			AccountID:                 "123456789012",
			Region:                    "us-east-1",
			ResourceName:              "checkout",
			ResourceARN:               "arn:aws:lambda:us-east-1:123456789012:function:checkout",
			CurrentErrorRate:          &rate,
			ErrorRateExceedsThreshold: &exceeds,
		}},
	}

	out := marshalScanResult(r)
	if len(out.Serverless) != 1 {
		t.Fatalf("serverless rows on wire = %d, want 1", len(out.Serverless))
	}
	row := out.Serverless[0]

	if row.ErrorRateExceedsThreshold == nil || !*row.ErrorRateExceedsThreshold {
		t.Error("error_rate_exceeds_threshold dropped on wire (the bug)")
	}
	if row.CurrentErrorRate == nil || *row.CurrentErrorRate != 0.27 {
		t.Errorf("current_error_rate lost on wire: got %v", row.CurrentErrorRate)
	}
	// Sampling annotator is dormant, so the row carries nil → omitempty elides.
	if row.SamplingExceedsFloor != nil || row.SamplingRatio != nil {
		t.Error("sampling fields should be nil (annotator dormant)")
	}

	// JSON shape: error-rate present, sampling absent (omitempty).
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"error_rate_exceeds_threshold":true`) {
		t.Errorf("JSON missing error_rate_exceeds_threshold: %s", js)
	}
	if strings.Contains(js, "sampling_exceeds_floor") {
		t.Errorf("JSON should omit sampling (nil): %s", js)
	}
}
