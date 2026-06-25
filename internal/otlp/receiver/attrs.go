// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package receiver

import (
	"context"
	"encoding/hex"
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
//     round-trippable representation)
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

// QualityObserver is the minimal surface the receiver needs from the
// traceindex Quality observer for the slice-1 span-quality chunk-1
// wiring. The full traceindex.Quality type satisfies it; tests use a
// recording fake that just appends each SpanObservation. Keeping the
// interface receiver-local matches the TraceObserver pattern above —
// future evolution of Quality (batched observe, per-span metrics)
// doesn't ripple through receiver tests.
type QualityObserver interface {
	Observe(obs traceindex.SpanObservation)
}

// observeQualitySpans is the §3 span-quality detection hot path. It
// runs in addition to (not instead of) observeResourceSpans because
// the two observers consume the OTLP payload at different
// granularities — Index aggregates per ResourceSpan, Quality classifies
// per-span. Both are called with the same ResourceSpans pointer; we
// re-walk the ScopeSpans only for the Quality pass when qual != nil so
// the index-only deployment shape (SPANQUALITY_DISABLED) pays nothing
// for the quality detection.
//
// Hot-path budget: ~200ns per span (§11 threat model). The function
// hex-encodes trace_id / span_id / parent_span_id once per span via
// encoding/hex (allocates a small string per encoding; this is the
// dominant per-span cost) and reuses the resource attribute map
// pointer for every span on the same ResourceSpan — span-level
// attributes are flattened into a fresh map only when the span
// actually carries any, so the common "resource-attrs-only" case
// stays allocation-bounded.
func observeQualitySpans(qual QualityObserver, resourceSpans []*tracepb.ResourceSpans) {
	if qual == nil || len(resourceSpans) == 0 {
		return
	}
	for _, rs := range resourceSpans {
		if rs == nil || rs.Resource == nil {
			continue
		}
		resourceAttrs := extractResourceAttributes(rs.Resource.Attributes)
		key, provider, _, _, _, _, ok := traceindex.ComputeResourceKey(resourceAttrs)
		if !ok {
			continue
		}
		tier := tierFromAttrs(resourceAttrs)
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			for _, sp := range ss.Spans {
				if sp == nil {
					continue
				}
				// Span-level attributes get merged on top of resource
				// attributes ONLY when the span actually carries any.
				// The dispatcher's intended common case is "no span
				// attrs" (the resource carries the identity); we keep
				// that path free of the merge allocation.
				attrs := resourceAttrs
				if len(sp.Attributes) > 0 {
					attrs = mergeSpanAttrs(resourceAttrs, sp.Attributes)
				}
				qual.Observe(traceindex.SpanObservation{
					Key:          key,
					Provider:     provider,
					TraceID:      hex.EncodeToString(sp.TraceId),
					SpanID:       hex.EncodeToString(sp.SpanId),
					ParentSpanID: hex.EncodeToString(sp.ParentSpanId),
					Tier:         tier,
					Attrs:        attrs,
				})
			}
		}
	}
}

// tierFromAttrs returns the §3.2 tier label for the supplied resource
// attribute map. Precedence: k8s wins over db wins over compute. This
// matches the keying-chain precedence (tier 3 k8s before tier 4 db
// before tier 2 host.id) so a span with BOTH k8s.* and db.* attrs
// gets classified as k8s for required-attrs purposes — the same call
// the discovery side makes for the inventory-row tier label.
//
// An unknown tier is "compute" by default: that keeps the "no
// k8s/db attrs" case classified as compute (the most common) without
// returning a sentinel value the firstMissingRequired table would
// have to handle separately.
func tierFromAttrs(attrs map[string]string) string {
	for k := range attrs {
		if len(k) > 4 && k[:4] == "k8s." {
			return "k8s"
		}
	}
	if attrs["db.system"] != "" {
		return "db"
	}
	return "compute"
}

// mergeSpanAttrs returns a new map containing resource attrs plus the
// supplied span attrs flattened into string form. Span attrs override
// resource attrs on key collision — a span carries a more specific
// identity than its resource by convention. Allocates a fresh map of
// size (resource + span attr count) so the resource map remains
// reusable for sibling spans on the same ResourceSpan.
//
// The span-attr extraction follows the same AnyValue handling as
// extractResourceAttributes (StringValue / IntValue / DoubleValue /
// BoolValue; array + kvlist dropped).
func mergeSpanAttrs(resourceAttrs map[string]string, spanAttrs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(resourceAttrs)+len(spanAttrs))
	for k, v := range resourceAttrs {
		out[k] = v
	}
	for _, kv := range spanAttrs {
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
