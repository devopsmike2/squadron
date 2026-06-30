// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/proposer"
)

// discovery_sampling_cache_test.go — #295 slice 5. Pins the in-memory
// last-result cache that backs the per-resource /sampling endpoint:
// the scan-annotation sink writes into it, and the endpoint reads it as
// both SamplingResourceLookup and SamplingDetector.

func firingResult(arn string) proposer.SamplingRateDetectionResult {
	// 50 spans / 2000 invocations = 0.025 ratio (< floor) @ >= 1000
	// invocations → a real, would-fire observation that must be cached.
	return proposer.SamplingRateDetectionResult{
		ResourceARN:               arn,
		ObservedSpanCount:         50,
		ExpectedInvocationCount:   2000,
		Ratio:                     0.025,
		ExceedsFloor:              true,
		ExceedsMinimumInvocations: true,
	}
}

func TestSamplingObservationCache_RecordThenServe(t *testing.T) {
	c := NewSamplingObservationCache()
	const arn = "arn:aws:lambda:us-east-1:1:function:checkout"
	c.record(arn, "lambda", arn, firingResult(arn))

	surface, key, ok := c.LookupSamplingResource("aws", arn)
	if !ok {
		t.Fatal("recorded resource must resolve via LookupSamplingResource")
	}
	if surface != "lambda" || key != arn {
		t.Errorf("lookup = (%q,%q), want (lambda,%q)", surface, key, arn)
	}

	res, err := c.DetectSampling(context.Background(), arn, surface, key)
	if err != nil {
		t.Fatalf("DetectSampling: %v", err)
	}
	if res.ExpectedInvocationCount != 2000 || res.ObservedSpanCount != 50 {
		t.Errorf("served result = %d spans / %d invocations, want 50/2000",
			res.ObservedSpanCount, res.ExpectedInvocationCount)
	}
	if !res.ShouldFireRecommendation() {
		t.Error("served result should preserve the would-fire verdict")
	}
}

// A zero/zero result is the "no data yet" shape; caching it would shadow
// the resource into a 200-with-empty endpoint response instead of 404.
func TestSamplingObservationCache_NoDataNotCached(t *testing.T) {
	c := NewSamplingObservationCache()
	c.record("arn:fn", "lambda", "arn:fn", proposer.SamplingRateDetectionResult{ResourceARN: "arn:fn"})
	if _, _, ok := c.LookupSamplingResource("aws", "arn:fn"); ok {
		t.Error("a zero/zero (no-data) result must not be cached")
	}
}

func TestSamplingObservationCache_EmptyARNDropped(t *testing.T) {
	c := NewSamplingObservationCache()
	c.record("", "lambda", "", firingResult(""))
	if _, _, ok := c.LookupSamplingResource("aws", ""); ok {
		t.Error("empty-ARN rows are un-joinable and must not be cached")
	}
}

func TestSamplingObservationCache_LatestWins(t *testing.T) {
	c := NewSamplingObservationCache()
	const arn = "arn:fn"
	first := firingResult(arn)
	first.ObservedSpanCount = 10
	c.record(arn, "lambda", arn, first)
	second := firingResult(arn)
	second.ObservedSpanCount = 99
	c.record(arn, "lambda", arn, second)

	res, _ := c.DetectSampling(context.Background(), arn, "lambda", arn)
	if res.ObservedSpanCount != 99 {
		t.Errorf("cache must hold the latest scan's result; got %d spans, want 99", res.ObservedSpanCount)
	}
}

func TestSamplingObservationCache_NilReceiverSafe(t *testing.T) {
	var c *SamplingObservationCache
	c.record("arn:fn", "lambda", "arn:fn", firingResult("arn:fn")) // must not panic
	if _, _, ok := c.LookupSamplingResource("aws", "arn:fn"); ok {
		t.Error("nil cache must report absent")
	}
	if _, err := c.DetectSampling(context.Background(), "arn:fn", "lambda", "arn:fn"); err != nil {
		t.Errorf("nil cache DetectSampling must be a clean empty result, got err=%v", err)
	}
}

// TestSamplingDetector_SinkCaptures proves the producer→endpoint join:
// AnnotateSampling (the scan path) records into the wired cache, so the
// endpoint then serves the same resource. A nil sink is a no-op.
func TestSamplingDetector_SinkCaptures(t *testing.T) {
	cache := NewSamplingObservationCache()
	const arn = "arn:aws:lambda:us-east-1:1:function:checkout"
	d := newSamplingDetector(&fakeSamplingQuerier{value: 2000}, &fakeSpanCounter{count: 50, ok: true}).withSink(cache)

	if _, err := d.AnnotateSampling(context.Background(), arn, "lambda", arn); err != nil {
		t.Fatalf("AnnotateSampling: %v", err)
	}
	if _, _, ok := cache.LookupSamplingResource("aws", arn); !ok {
		t.Fatal("AnnotateSampling must record the live result into the sink cache")
	}
	res, _ := cache.DetectSampling(context.Background(), arn, "lambda", arn)
	if !res.ShouldFireRecommendation() {
		t.Error("cached result must carry the would-fire verdict the scan computed")
	}
}

// A query error must NOT cache anything (the row degrades to "—").
func TestSamplingDetector_SinkSkipsOnError(t *testing.T) {
	cache := NewSamplingObservationCache()
	d := newSamplingDetector(&fakeSamplingQuerier{err: context.DeadlineExceeded}, &fakeSpanCounter{}).withSink(cache)
	_, _ = d.AnnotateSampling(context.Background(), "arn:fn", "lambda", "arn:fn")
	if _, _, ok := cache.LookupSamplingResource("aws", "arn:fn"); ok {
		t.Error("a failed annotation must not populate the endpoint cache")
	}
}

// withSink off a nil detector (nil querier) stays nil and is safe to
// pass to AnnotateServerlessWithSampling (treated as feature-not-wired).
func TestSamplingDetector_WithSinkNilDetector(t *testing.T) {
	if d := newSamplingDetector(nil, &fakeSpanCounter{}).withSink(NewSamplingObservationCache()); d != nil {
		t.Error("withSink off a nil detector must stay nil")
	}
}
