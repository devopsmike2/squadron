// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// cold_start_test.go — Cold-start latency analysis slice 1 chunk 3
// (v0.89.115, #753 Stream 151). Pins the CheckLambdaColdStart
// detection branch's ShouldFire-gated emit + the per-row exclusion
// filter against §8 / §11 acceptance tests.

// newColdStartRow is the test fixture for the cold-start detection
// branch. Tests vary one field at a time.
func newColdStartRow() ColdStartInventoryRow {
	return ColdStartInventoryRow{
		RecommendationID: "rec-aws-lambda-order-processor",
		Provider:         "aws",
		Surface:          "lambda",
		ResourceID:       "arn:aws:lambda:us-east-1:123:function:order-processor",
		ResourceTFName:   "order_processor",
		Region:           "us-east-1",
	}
}

func newColdStartScope() ColdStartScope {
	return ColdStartScope{
		ConnectionID: "conn-1",
		ScopeID:      "123456789012",
		Region:       "us-east-1",
	}
}

// TestLambdaColdStart_ShouldFire_EmitsRecommendation — the canonical
// happy path: the chunk-2 finding's ShouldFire predicate held, so the
// detection branch emits a lambda-cold-start-baseline draft with the
// per-row reasoning + Terraform snippet from the picker.
func TestLambdaColdStart_ShouldFire_EmitsRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:          true,
		CurrentP95Ms:        4230,
		BaselineP95Ms:       2820,
		Ratio:               1.5,
		CurrentSampleCount:  142,
		BaselineSampleCount: 1086,
	}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft == nil {
		t.Fatalf("draft == nil; want non-nil")
	}
	if draft.Kind != ColdStartRecommendationKind {
		t.Errorf("Kind = %q, want %q", draft.Kind, ColdStartRecommendationKind)
	}
	if draft.Kind != "lambda-cold-start-baseline" {
		t.Errorf("Kind = %q, want lambda-cold-start-baseline (the §8 const)", draft.Kind)
	}
	if !strings.HasSuffix(draft.RecommendationID, ".cold_start") {
		t.Errorf("RecommendationID = %q, want .cold_start suffix", draft.RecommendationID)
	}
	if !strings.Contains(draft.Reasoning, "4230ms") {
		t.Errorf("Reasoning missing current p95 4230ms; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "1.50x") {
		t.Errorf("Reasoning missing ratio 1.50x; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "2820ms") {
		t.Errorf("Reasoning missing baseline 2820ms; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "order-processor") {
		t.Errorf("Reasoning missing resource ARN slug; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "Init script regression") {
		t.Errorf("Reasoning missing three-failure-mode framing cause 1; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "frequency increase") {
		t.Errorf("Reasoning missing three-failure-mode framing cause 2; got: %s", draft.Reasoning)
	}
	if !strings.Contains(draft.Reasoning, "Architecture change") {
		t.Errorf("Reasoning missing three-failure-mode framing cause 3; got: %s", draft.Reasoning)
	}
	if draft.Terraform == "" {
		t.Errorf("Terraform empty; want the picker's snippet")
	}
	if !strings.Contains(draft.Terraform, "aws_lambda_provisioned_concurrency_config") {
		t.Errorf("Terraform missing the provisioned concurrency resource; got: %s", draft.Terraform)
	}
	if draft.ResourceID != row.ResourceID {
		t.Errorf("ResourceID = %q, want %q", draft.ResourceID, row.ResourceID)
	}
}

// TestLambdaColdStart_DoesNotShouldFire_NoRecommendation — when the
// chunk-2 finding's ShouldFire predicate didn't hold (any of the three
// sub-rules — ratio, floor, baseline samples), the detection branch
// emits nothing.
func TestLambdaColdStart_DoesNotShouldFire_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:    false,
		CurrentP95Ms:  4230,
		BaselineP95Ms: 2820,
		Ratio:         1.5,
	}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on !ShouldFire; got: %+v", draft)
	}
}

// TestLambdaColdStart_NilFinding_NoRecommendation — a row with no
// chunk-2 finding (e.g. a Lambda younger than the first scan window,
// or a row the scanner couldn't query) produces no draft.
func TestLambdaColdStart_NilFinding_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	draft, err := CheckLambdaColdStart(context.Background(), row, nil, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on nil finding; got: %+v", draft)
	}
}

// TestLambdaColdStart_NonLambdaSurface_NoRecommendation — slice 1
// covers AWS Lambda only. Rows on cloudrun / cloudfunc / azfunc /
// ocifunc surfaces emit nothing even when ShouldFire is true.
func TestLambdaColdStart_NonLambdaSurface_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	row.Surface = "cloudrun"
	finding := &ColdStartDetectionFinding{ShouldFire: true}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), &fakeExclusionStore{})
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on non-lambda surface; got: %+v", draft)
	}
}

// TestLambdaColdStart_ExcludedRow_NoRecommendation — the operator's
// per-row "Don't propose this again" affordance is honored: when the
// exclusion store carries a row with RecommendationID matching the
// per-row.cold_start suffix, no draft emits.
func TestLambdaColdStart_ExcludedRow_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:          true,
		CurrentP95Ms:        4230,
		BaselineP95Ms:       2820,
		Ratio:               1.5,
		BaselineSampleCount: 1000,
	}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{
			RecommendationID:   row.RecommendationID + ".cold_start",
			RecommendationKind: ColdStartRecommendationKind,
		},
	}}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), exclusions)
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on excluded row; got: %+v", draft)
	}
}

// TestLambdaColdStart_ExcludedByKindOnly_NoRecommendation — the
// kind-only exclusion marker (RecommendationID="" + RecommendationKind
// set) covers the whole scope. Mirrors the trace-emission +
// span-quality posture.
func TestLambdaColdStart_ExcludedByKindOnly_NoRecommendation(t *testing.T) {
	row := newColdStartRow()
	finding := &ColdStartDetectionFinding{
		ShouldFire:          true,
		CurrentP95Ms:        4230,
		BaselineP95Ms:       2820,
		Ratio:               1.5,
		BaselineSampleCount: 1000,
	}
	exclusions := &fakeExclusionStore{rows: []applicationstore.ExcludedRecommendation{
		{
			RecommendationID:   "",
			RecommendationKind: ColdStartRecommendationKind,
		},
	}}
	draft, err := CheckLambdaColdStart(context.Background(), row, finding, newColdStartScope(), exclusions)
	if err != nil {
		t.Fatalf("CheckLambdaColdStart: %v", err)
	}
	if draft != nil {
		t.Errorf("draft != nil on kind-only exclusion; got: %+v", draft)
	}
}

// TestLambdaColdStartBatch_AccumulatesDrafts — the batch wrapper runs
// CheckLambdaColdStart over multiple rows + per-row findings, returns
// the accumulated drafts in order.
func TestLambdaColdStartBatch_AccumulatesDrafts(t *testing.T) {
	row1 := newColdStartRow()
	row1.RecommendationID = "rec-1"
	row2 := newColdStartRow()
	row2.RecommendationID = "rec-2"
	rows := []ColdStartInventoryRow{row1, row2}
	findings := []*ColdStartDetectionFinding{
		{ShouldFire: true, CurrentP95Ms: 1000, BaselineP95Ms: 500, Ratio: 2.0, BaselineSampleCount: 100},
		nil, // simulates "no observation yet for row 2"
	}
	drafts, errs := CheckLambdaColdStartBatch(context.Background(), rows, findings, newColdStartScope(), &fakeExclusionStore{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(drafts) != 1 {
		t.Fatalf("len(drafts) = %d, want 1 (only row 1 fires)", len(drafts))
	}
	if !strings.HasPrefix(drafts[0].RecommendationID, "rec-1") {
		t.Errorf("draft.RecommendationID = %q, want rec-1 prefix", drafts[0].RecommendationID)
	}
}
