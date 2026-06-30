// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"sync"

	"github.com/devopsmike2/squadron/internal/proposer"
)

// discovery_sampling_cache.go — #295 slice 5. Backs the per-resource
// GET /…/serverless/{id}/sampling endpoint, which has no live scanner
// at request time (unlike the scan handler, which holds the just-
// scanned scanner in scope).
//
// Sampling is a LIVE join — recomputed every scan from the cloud
// invocation count ⋈ the OTLP span count, never a historically-
// persisted observation (see docs/proposals/sampling-rate-activation.md).
// So the correct backing for the endpoint is NOT a sqlite observation
// store (the cold-start / error-rate pattern, which records a time
// series) but a bounded in-memory cache of the MOST RECENT scan's
// result per resource. The endpoint reflects what the last scan
// computed; a resource no scan has observed — or any resource when
// serverless_metric_detection is off — is simply absent (404), which
// is the same "no observation yet" surface the handler already emits.
//
// One cache instance plays three roles:
//   - record(...)            — written by samplingDetector during the
//     scan-response annotation pass (the sink).
//   - SamplingResourceLookup — resolves the endpoint's :id to a
//     surface + traceindex key.
//   - SamplingDetector       — returns the stored result for the :id.
//
// The server holds ONE cache, wires it as the endpoint's lookup +
// detector, and threads it into each per-cloud scan handler as the
// annotation sink, so the producer (scan) and consumer (endpoint)
// share state without a database round-trip.
type SamplingObservationCache struct {
	mu      sync.RWMutex
	entries map[string]samplingCacheEntry
}

// samplingCacheEntry is the most-recent live result for one resource,
// keyed by ARN (== the endpoint's :id path param and the traceindex
// join key for serverless).
type samplingCacheEntry struct {
	surface string
	key     string
	result  proposer.SamplingRateDetectionResult
}

// NewSamplingObservationCache builds an empty cache.
func NewSamplingObservationCache() *SamplingObservationCache {
	return &SamplingObservationCache{entries: make(map[string]samplingCacheEntry)}
}

// record stores the latest live sampling result for a resource. Called
// by samplingDetector.AnnotateSampling during the scan-response pass.
//
// A zero/zero result (no invocations AND no observed spans) is the
// "no data yet" shape the annotator leaves as "—"; caching it would
// shadow the resource into a 200-with-empty-counts endpoint response
// instead of the correct 404, so it is dropped — exactly mirroring the
// annotator's own skip (inventory_sampling.go). Empty ARN rows (un-
// joinable) are dropped for the same reason. Nil-receiver-safe.
func (c *SamplingObservationCache) record(arn, surface, key string, result proposer.SamplingRateDetectionResult) {
	if c == nil || arn == "" {
		return
	}
	if result.ExpectedInvocationCount == 0 && result.ObservedSpanCount == 0 {
		return
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]samplingCacheEntry)
	}
	c.entries[arn] = samplingCacheEntry{surface: surface, key: key, result: result}
	c.mu.Unlock()
}

// LookupSamplingResource implements SamplingResourceLookup. provider is
// ignored — the resource ARN/OCID/name is globally unique across
// providers, and the endpoint already routes per-provider. ok=false
// (resource absent) drives the endpoint's 404. Nil-receiver-safe.
func (c *SamplingObservationCache) LookupSamplingResource(_ string, resourceID string) (surface string, traceindexKey string, ok bool) {
	if c == nil {
		return "", "", false
	}
	c.mu.RLock()
	e, present := c.entries[resourceID]
	c.mu.RUnlock()
	if !present {
		return "", "", false
	}
	return e.surface, e.key, true
}

// DetectSampling implements SamplingDetector. It returns the last
// recorded live result for the resource — the endpoint's lookup has
// already confirmed presence, so a concurrent eviction between lookup
// and here surfaces as a zero-value result + nil error (the handler
// renders the empty shape) rather than an error. Nil-receiver-safe.
func (c *SamplingObservationCache) DetectSampling(_ context.Context, resourceARN string, _ string, _ string) (proposer.SamplingRateDetectionResult, error) {
	if c == nil {
		return proposer.SamplingRateDetectionResult{}, nil
	}
	c.mu.RLock()
	e := c.entries[resourceARN]
	c.mu.RUnlock()
	return e.result, nil
}

var (
	_ SamplingResourceLookup = (*SamplingObservationCache)(nil)
	_ SamplingDetector       = (*SamplingObservationCache)(nil)
)
