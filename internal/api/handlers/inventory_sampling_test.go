// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/proposer"
)

// inventory_sampling_test.go — Sampling rate analysis slice 1 chunk 2
// (v0.89.123, #763 Stream 161). Pins
// AnnotateServerlessWithSampling against §6.2 acceptance test 13.

// stubAnnotator — programmable SamplingAnnotator.
type stubAnnotator struct {
	results map[string]proposer.SamplingRateDetectionResult
	err     error
}

func (s *stubAnnotator) AnnotateSampling(
	_ context.Context,
	resourceARN string,
	_ string,
	_ string,
) (proposer.SamplingRateDetectionResult, error) {
	if s.err != nil {
		return proposer.SamplingRateDetectionResult{}, s.err
	}
	if r, ok := s.results[resourceARN]; ok {
		return r, nil
	}
	return proposer.SamplingRateDetectionResult{}, nil
}

// stubKeyResolver — deterministic key resolution.
type stubKeyResolver struct{}

func (stubKeyResolver) TraceindexKeyFor(surface, resourceARN string) string {
	return surface + "/" + resourceARN
}

// TestInventoryAnnotation_AddsSamplingRatioToLambdaRow — acceptance
// test 13. Lambda row with a fired detection result gets
// SamplingRatio + SamplingExceedsFloor stamped.
func TestInventoryAnnotation_AddsSamplingRatioToLambdaRow(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123:function:order-processor"
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{
			Provider:    "aws",
			Surface:     "lambda",
			ResourceARN: arn,
		},
	}
	ann := &stubAnnotator{
		results: map[string]proposer.SamplingRateDetectionResult{
			arn: {
				ResourceARN:               arn,
				Surface:                   "lambda",
				ObservedSpanCount:         245,
				ExpectedInvocationCount:   5000,
				Ratio:                     0.049,
				ExceedsFloor:              true,
				ExceedsMinimumInvocations: true,
			},
		},
	}
	AnnotateServerlessWithSampling(context.Background(), ann, stubKeyResolver{}, snapshots, nil)
	if snapshots[0].SamplingRatio == nil {
		t.Fatalf("SamplingRatio is nil; want populated")
	}
	if got := *snapshots[0].SamplingRatio; got < 0.04 || got > 0.05 {
		t.Errorf("SamplingRatio = %v, want ~0.049", got)
	}
	if snapshots[0].SamplingExceedsFloor == nil {
		t.Fatalf("SamplingExceedsFloor is nil; want populated")
	}
	if !*snapshots[0].SamplingExceedsFloor {
		t.Error("SamplingExceedsFloor = false, want true (fired result)")
	}
}

// TestInventoryAnnotation_NullWhenInsufficientData — when both
// observed and expected are zero, the annotator leaves the pointers
// nil so the UI renders "—".
func TestInventoryAnnotation_NullWhenInsufficientData(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123:function:no-data"
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{
			Provider:    "aws",
			Surface:     "lambda",
			ResourceARN: arn,
		},
	}
	ann := &stubAnnotator{
		results: map[string]proposer.SamplingRateDetectionResult{
			arn: {
				ResourceARN: arn,
				Surface:     "lambda",
				// All zero — no observations.
			},
		},
	}
	AnnotateServerlessWithSampling(context.Background(), ann, stubKeyResolver{}, snapshots, nil)
	if snapshots[0].SamplingRatio != nil {
		t.Errorf("SamplingRatio = %v, want nil for no-data row", *snapshots[0].SamplingRatio)
	}
	if snapshots[0].SamplingExceedsFloor != nil {
		t.Errorf("SamplingExceedsFloor = %v, want nil for no-data row", *snapshots[0].SamplingExceedsFloor)
	}
}

// TestInventoryAnnotation_NilAnnotator_NoOp — nil annotator
// short-circuits the entire call.
func TestInventoryAnnotation_NilAnnotator_NoOp(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "lambda", ResourceARN: "arn:x"},
	}
	AnnotateServerlessWithSampling(context.Background(), nil, stubKeyResolver{}, snapshots, nil)
	if snapshots[0].SamplingRatio != nil {
		t.Error("SamplingRatio populated on nil annotator path; want nil")
	}
}

// TestInventoryAnnotation_NilResolver_NoOp — nil resolver
// short-circuits the entire call.
func TestInventoryAnnotation_NilResolver_NoOp(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "lambda", ResourceARN: "arn:x"},
	}
	AnnotateServerlessWithSampling(context.Background(), &stubAnnotator{}, nil, snapshots, nil)
	if snapshots[0].SamplingRatio != nil {
		t.Error("SamplingRatio populated on nil resolver path; want nil")
	}
}

// TestInventoryAnnotation_NonAnnotatableSurface_Skips — surfaces not
// in the {lambda / cloudrun / cloudfunc / azfunc / ocifunc} set are
// skipped silently.
func TestInventoryAnnotation_NonAnnotatableSurface_Skips(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "ec2", ResourceARN: "arn:x"},
	}
	AnnotateServerlessWithSampling(context.Background(), &stubAnnotator{}, stubKeyResolver{}, snapshots, nil)
	if snapshots[0].SamplingRatio != nil {
		t.Error("SamplingRatio populated on non-annotatable surface; want nil")
	}
}

// TestInventoryAnnotation_EmptyResourceARN_Skips — degenerate rows
// with empty ResourceARN can't be joined; skip silently.
func TestInventoryAnnotation_EmptyResourceARN_Skips(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "lambda", ResourceARN: ""},
	}
	AnnotateServerlessWithSampling(context.Background(), &stubAnnotator{}, stubKeyResolver{}, snapshots, nil)
	if snapshots[0].SamplingRatio != nil {
		t.Error("SamplingRatio populated on empty-ARN row; want nil")
	}
}

// TestIsSamplingAnnotatableSurface pins the 5-surface set: lambda /
// cloudrun / cloudfunc / azfunc / ocifunc all true, everything else
// false.
func TestIsSamplingAnnotatableSurface(t *testing.T) {
	for _, surface := range []string{"lambda", "cloudrun", "cloudfunc", "azfunc", "ocifunc"} {
		if !isSamplingAnnotatableSurface(surface) {
			t.Errorf("isSamplingAnnotatableSurface(%q) = false, want true", surface)
		}
	}
	for _, surface := range []string{"", "ec2", "stepfunc", "unknown", "eventbridge"} {
		if isSamplingAnnotatableSurface(surface) {
			t.Errorf("isSamplingAnnotatableSurface(%q) = true, want false", surface)
		}
	}
}
