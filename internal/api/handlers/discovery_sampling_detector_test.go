// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// discovery_sampling_detector_test.go — slice 1 of the sampling-rate
// activation arc (#295). Pins the concrete adapter that dispatches to
// proposer.DetectSamplingRate, joining a cloud invocation-count querier
// with the traceindex span counter.

type fakeSamplingQuerier struct {
	value     float64
	err       error
	gotMetric string
	gotStat   scanner.MetricStatistic
}

func (f *fakeSamplingQuerier) QueryAggregate(
	_ context.Context, arn, metric string, _ time.Duration, stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	f.gotMetric = metric
	f.gotStat = stat
	if f.err != nil {
		return scanner.AggregateMetricResult{}, f.err
	}
	return scanner.AggregateMetricResult{ResourceARN: arn, Value: f.value}, nil
}

type fakeSpanCounter struct {
	count  uint64
	ok     bool
	gotKey string
}

func (f *fakeSpanCounter) SpanCountLast24h(key string) (uint64, bool) {
	f.gotKey = key
	return f.count, f.ok
}

// TestSamplingDetector_Fires: 50 spans / 2000 invocations = 0.025 ratio
// (< 0.05 floor) with invocations >= 1000 → should fire. Exercised via
// both consumer methods (they must behave identically).
func TestSamplingDetector_Fires(t *testing.T) {
	for _, via := range []string{"DetectSampling", "AnnotateSampling"} {
		t.Run(via, func(t *testing.T) {
			q := &fakeSamplingQuerier{value: 2000}
			sc := &fakeSpanCounter{count: 50, ok: true}
			d := newSamplingDetector(q, sc)
			if d == nil {
				t.Fatal("newSamplingDetector returned nil for a non-nil querier")
			}

			res, err := d.DetectSampling(context.Background(), "arn:fn", "lambda", "arn:fn")
			if via == "AnnotateSampling" {
				res, err = d.AnnotateSampling(context.Background(), "arn:fn", "lambda", "arn:fn")
			}
			if err != nil {
				t.Fatalf("%s: %v", via, err)
			}
			if !res.ShouldFireRecommendation() {
				t.Error("expected ShouldFireRecommendation=true for ratio 0.025 @ 2000 invocations")
			}
			if sc.gotKey != "arn:fn" {
				t.Errorf("span counter got key %q, want the ARN", sc.gotKey)
			}
			if q.gotStat != scanner.StatisticSum {
				t.Errorf("invocation query stat = %v, want StatisticSum", q.gotStat)
			}
			if q.gotMetric == "" {
				t.Error("expected a per-surface invocation metric name to be queried")
			}
		})
	}
}

// TestSamplingDetector_BelowMinimumNoFire: low ratio but only 500
// invocations (< 1000 minimum) → no fire (statistical-noise filter).
func TestSamplingDetector_BelowMinimumNoFire(t *testing.T) {
	d := newSamplingDetector(&fakeSamplingQuerier{value: 500}, &fakeSpanCounter{count: 1, ok: true})
	got, err := d.DetectSampling(context.Background(), "arn:fn", "lambda", "arn:fn")
	if err != nil {
		t.Fatal(err)
	}
	if got.ShouldFireRecommendation() {
		t.Error("must not fire below MinInvocationCount even with a low ratio")
	}
	if !got.ExceedsFloor {
		t.Error("ratio 0.002 should be below the floor (ExceedsFloor=true)")
	}
	if got.ExceedsMinimumInvocations {
		t.Error("500 invocations is below the 1000 minimum")
	}
}

// TestSamplingDetector_AcceptableRatioNoFire: ratio 0.10 (>= 0.05) → no
// fire even with plenty of invocations.
func TestSamplingDetector_AcceptableRatioNoFire(t *testing.T) {
	d := newSamplingDetector(&fakeSamplingQuerier{value: 2000}, &fakeSpanCounter{count: 200, ok: true})
	got, err := d.DetectSampling(context.Background(), "arn:fn", "lambda", "arn:fn")
	if err != nil {
		t.Fatal(err)
	}
	if got.ShouldFireRecommendation() || got.ExceedsFloor {
		t.Errorf("ratio 0.10 is within the acceptable band; want no fire, got fire=%v floor=%v",
			got.ShouldFireRecommendation(), got.ExceedsFloor)
	}
}

// TestSamplingDetector_QueryErrorPropagates: a flaky metric API surfaces
// as an error (callers degrade the row to "—").
func TestSamplingDetector_QueryErrorPropagates(t *testing.T) {
	d := newSamplingDetector(&fakeSamplingQuerier{err: errors.New("throttled")}, &fakeSpanCounter{})
	if _, err := d.DetectSampling(context.Background(), "arn:fn", "lambda", "arn:fn"); err == nil {
		t.Error("expected the querier error to propagate")
	}
}

// TestNewSamplingDetector_NilQuerier: no metric substrate → nil adapter,
// which the consumers treat as "feature not wired".
func TestNewSamplingDetector_NilQuerier(t *testing.T) {
	if d := newSamplingDetector(nil, &fakeSpanCounter{}); d != nil {
		t.Error("nil querier must yield a nil detector so the feature stays no-op")
	}
}

// TestSamplingARNKeyResolver: serverless join key is the ARN (tier-1).
func TestSamplingARNKeyResolver(t *testing.T) {
	r := samplingARNKeyResolver{}
	if got := r.TraceindexKeyFor("lambda", "arn:aws:lambda:us-east-1:1:function:checkout"); got != "arn:aws:lambda:us-east-1:1:function:checkout" {
		t.Errorf("TraceindexKeyFor = %q, want the ARN verbatim", got)
	}
}
