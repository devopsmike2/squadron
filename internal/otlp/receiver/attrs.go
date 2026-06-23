// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package receiver

import (
	"context"
	"strconv"
	"time"

	"github.com/devopsmike2/squadron/internal/traceindex"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// extractResourceAttributes flattens OTLP KeyValue resource attributes
// into the map[string]string shape the traceindex Index.Observe path
// consumes. The Index in turn feeds this map straight to
// ComputeResourceKey which uses string equality / non-empty checks on
// each key, so coercing every AnyValue variant to its string form
// keeps the keying chain simple.
//
// Variant handling:
//   - StringValue → as-is
//   - IntValue    → base-10 decimal
//   - DoubleValue → strconv.FormatFloat 'g' precision -1 (shortest
//                   round-trippable representation)
//   - BoolValue   → "true" / "false"
//   - ArrayValue, KvListValue → SKIPPED. Slice 1's six-tier keying
//     chain (design doc §3) consumes only scalar identifiers
//     (cloud.resource_id, host.id, host.name, cloud.account.id,
//     k8s.cluster.name, db.system, db.name, service.name); none of
//     those are array-shaped in practice, so the slice-1 receiver
//     drops the complex shapes silently rather than carry a string
//     representation a downstream consumer might misread. Slice 2's
//     attribute-quality analysis will surface arrays explicitly.
//
// The function is small, allocation-light, and called once per
// ResourceSpan on the hot path — keep it cheap. The receiver tests
// (TestExtractResourceAttributes_*) pin the variant coverage so a
// future change to the helper keeps the contract honest.
func extractResourceAttributes(kvs []*commonpb.KeyValue) map[string]string {
	if len(kvs) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if kv == nil || kv.Key == "" || kv.Value == nil {
			continue
		}
		switch v := kv.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			out[kv.Key] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			out[kv.Key] = strconv.FormatInt(v.IntValue, 10)
		case *commonpb.AnyValue_DoubleValue:
			out[kv.Key] = strconv.FormatFloat(v.DoubleValue, 'g', -1, 64)
		case *commonpb.AnyValue_BoolValue:
			out[kv.Key] = strconv.FormatBool(v.BoolValue)
		}
	}
	return out
}

// observeResourceSpans iterates an OTLP ExportTraceServiceRequest's
// ResourceSpans and dispatches one Index.Observe call per resource.
// Both the HTTP handler (handleOTLPTraces) and the gRPC handler
// (TraceService.Export) call through here so the keying + counting
// logic stays one place — drift between the two transports would
// surface as inventory-correlation gaps that are painful to debug.
//
// Counts (SpanCount, RootSpanCount) come from a single linear scan
// of the ScopeSpans → Spans nesting; root vs child is identified by
// the empty ParentSpanId per OTel semantic conventions. A
// ResourceSpan with zero spans (possible for malformed exporters or
// metadata-only batches) is skipped — observing it would produce a
// row with span_count_24h=0 that the dashboard would render as
// "seen but not emitting", which is the wrong shape for the slice-1
// coverage panel.
//
// `now` is supplied by the caller so HTTP and gRPC use the receiver-
// clock (rather than the per-span clock) as design doc §3 specifies
// — a malformed exporter cannot backdate the index by lying about
// its span timestamps.
func observeResourceSpans(ctx context.Context, idx TraceObserver, resourceSpans []*tracepb.ResourceSpans, now time.Time) {
	if idx == nil || len(resourceSpans) == 0 {
		return
	}
	now = now.UTC()
	for _, rs := range resourceSpans {
		if rs == nil || rs.Resource == nil {
			continue
		}
		attrs := extractResourceAttributes(rs.Resource.Attributes)
		spanCount := 0
		rootSpanCount := 0
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			for _, sp := range ss.Spans {
				if sp == nil {
					continue
				}
				spanCount++
				if len(sp.ParentSpanId) == 0 {
					rootSpanCount++
				}
			}
		}
		if spanCount == 0 {
			continue
		}
		idx.Observe(ctx, traceindex.ResourceObservation{
			Attributes:    attrs,
			SpanCount:     spanCount,
			RootSpanCount: rootSpanCount,
			Timestamp:     now,
		})
	}
}
