// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"

	"github.com/devopsmike2/squadron/internal/proposer"
)

// discovery_sampling_detector.go — the concrete producer that activates
// the (previously dormant, #295) sampling-rate detection. See
// docs/proposals/sampling-rate-activation.md.
//
// Sampling is a cross-layer JOIN: the cloud-native invocation count
// (scanner-side, via QueryAggregate — the metric client #300 wired) ⋈
// the observed OTLP span count (server-side, traceindex.Quality). The
// handler is the only layer with access to both, so the detector is a
// thin handler-package adapter that holds one of each and dispatches to
// the already-built+tested proposer.DetectSamplingRate.
//
// One type satisfies BOTH consumer seams — the scan-response annotator
// (SamplingAnnotator, inventory_sampling.go) and the per-resource
// endpoint (SamplingDetector, discovery_serverless_sampling.go) — because
// their methods are identical but for the name.

// samplingDetector dispatches to proposer.DetectSamplingRate with a
// per-cloud invocation-count querier + the traceindex span counter.
//
//   - querier: a scanner's QueryAggregate. When the
//     serverless_metric_detection flag is off the scanner has no metric
//     client and QueryAggregate returns scanner.ErrMetricNotImplemented,
//     which both consumers degrade to "no observation" — so this whole
//     producer is implicitly gated on that flag with zero extra config.
//   - quality: *traceindex.Quality (the OTLP span index). Nil-tolerant in
//     the detector (DetectSamplingRate treats nil as zero observed spans).
type samplingDetector struct {
	querier proposer.SamplingRateMetricQuerier
	quality proposer.SamplingRateSpanCounter
}

var (
	_ SamplingAnnotator = samplingDetector{}
	_ SamplingDetector  = samplingDetector{}
)

// newSamplingDetector builds the adapter. Returns nil when the querier is
// nil (no metric substrate) so callers can pass the result straight to
// AnnotateServerlessWithSampling / the endpoint constructor, which treat
// a nil annotator/detector as "feature not wired" (no-op / 404).
func newSamplingDetector(querier proposer.SamplingRateMetricQuerier, quality proposer.SamplingRateSpanCounter) *samplingDetector {
	if querier == nil {
		return nil
	}
	return &samplingDetector{querier: querier, quality: quality}
}

// DetectSampling implements SamplingDetector (per-resource endpoint).
func (d samplingDetector) DetectSampling(
	ctx context.Context,
	resourceARN string,
	surface string,
	traceindexKey string,
) (proposer.SamplingRateDetectionResult, error) {
	return proposer.DetectSamplingRate(ctx, d.querier, d.quality, resourceARN, surface, traceindexKey)
}

// AnnotateSampling implements SamplingAnnotator (scan-response rows).
func (d samplingDetector) AnnotateSampling(
	ctx context.Context,
	resourceARN string,
	surface string,
	traceindexKey string,
) (proposer.SamplingRateDetectionResult, error) {
	return proposer.DetectSamplingRate(ctx, d.querier, d.quality, resourceARN, surface, traceindexKey)
}

// samplingARNKeyResolver maps a serverless (surface, ARN) to the
// traceindex key its spans land under. traceindex.ComputeResourceKey
// tier 1 keys verbatim on cloud.resource_id, which a properly-
// instrumented serverless function sets to its ARN / resource name /
// OCID — the same value the scanner stores as ResourceARN. So the key is
// the ARN. Functions that don't emit cloud.resource_id miss the join and
// surface as "no observation" (insufficient-data posture), which is
// correct.
type samplingARNKeyResolver struct{}

var _ SamplingKeyResolver = samplingARNKeyResolver{}

func (samplingARNKeyResolver) TraceindexKeyFor(_ string, resourceARN string) string {
	return resourceARN
}
