# Span quality slice 2 — W3C trace context parsing

**Status:** design doc, locked for slice 2 implementation.
Closes the explicit slice 1 deferral (span quality slice 1
§2 non-goal: "W3C trace context validation — that's slice
2"). Composes with both span quality slice 1 (which catches
orphan-span SYMPTOMS) and event source tier slice 2 (which
catches broken propagation CONFIG) into a complete
diagnostic picture.

**See also:**
[Span quality slice 1](./span-quality-slice1.md),
[Trace integration slice 1](./trace-integration-slice1.md),
[Event source tier slice 2](./event-source-tier-slice2.md).

## 1. Problem

Span quality slice 1 (v0.89.84-88) ships three pathology
detectors on the OTLP receiver hot path:

- **Orphan spans** — `parent_span_id` is non-zero but no span
  with that span_id has been observed in the 5-minute window.
- **Missing required resource attributes** — per-tier fixed
  set (service.name / cloud.provider / etc.).
- **Attribute placeholder/mismatch** — host.name=localhost,
  cloud.account.id=000000000000, etc.

These detectors catch real problems but leave a specific gap:
when a span arrives, does its W3C trace context (the
`traceparent` header value) actually conform to the W3C spec?

This matters because malformed traceparent is the canonical
root-cause for "span chain visibly broken in the dashboard
but no obvious upstream config issue." Squadron's existing
orphan-span detector catches the consequence (parent_span_id
unresolvable in the 5-minute window) but can't distinguish
between:

- **Case A: Parent context never propagated.** The upstream
  service didn't emit a span; the downstream service generated
  a fresh parent_span_id locally that points at nothing. The
  orphan-span detector flags this; event source slice 2's
  propagation config detector flags the upstream config gap.
- **Case B: Parent context propagated but malformed.** The
  upstream service emitted a span and propagated a traceparent
  header, but the header is malformed (wrong version segment,
  trace_id with non-hex characters, etc.). The downstream
  SDK parsed it, found it invalid, and generated a fresh
  parent_span_id. The orphan-span detector ALSO flags this,
  but no current Squadron detector can distinguish it from
  case A.
- **Case C: Parent context propagated but absent attribute.**
  The upstream service emitted a span and the downstream
  service's library DID receive a valid header, but for
  some reason the SDK didn't attach `traceparent` to the
  span's attributes. The span carries parent_span_id but no
  traceparent — suggests an SDK bug or instrumentation gap
  (especially common with custom auto-instrumentation
  patches).

Slice 2 surfaces cases B and C with two new pathology
detectors at the same Quality observer hot path:

1. **HasMalformedTraceparent** — span carries a `traceparent`
   attribute but its value doesn't match the W3C format
   `00-{32hex}-{16hex}-{2hex}`.
2. **HasMissingTraceparentOnChild** — span has a non-zero
   `parent_span_id` but no `traceparent` attribute.

Two new recommendation kinds correspond. The operator can
distinguish "your upstream isn't propagating" (slice 1 +
event source slice 2) from "your upstream is propagating
broken values" (slice 2 W3C parsing).

## 2. Non-goals (slice 2)

- **`tracestate` parsing.** The W3C trace context spec
  defines two headers: `traceparent` (required for trace
  identity) and `tracestate` (optional vendor-specific
  metadata). Slice 2 parses traceparent only. Tracestate
  parsing is slice 3 candidate.
- **Sampling decision propagation.** The traceparent header
  includes a `trace-flags` segment that carries the sampling
  decision. Slice 2 validates the segment is well-formed
  but does NOT inspect whether downstream sampling decisions
  honor it. That's a separate diagnostic.
- **B3 / Jaeger / DataDog / Zipkin context propagation.**
  These are alternative trace context formats. Operators
  who use them (rather than W3C) get false positives on the
  HasMissingTraceparentOnChild detector. Slice 2 detects
  the dominant case; slice 3 may add format-agnostic
  detection.
- **traceparent in the span content (not attributes).**
  Some SDKs propagate context via HTTP headers but don't
  attach traceparent to the resulting span's attributes
  (they just use it during span creation). Squadron's
  Quality observer reads attributes — if the SDK doesn't
  attach traceparent to the span, slice 2 can't detect it
  even when propagation worked. The operator sees a false
  positive; the runbook documents.
- **Per-language SDK fingerprinting.** Different SDKs attach
  traceparent under slightly different attribute names
  (some use `traceparent`, some use OTel semantic
  conventions). Slice 2 checks both common forms but does
  NOT fingerprint per-language SDK. Slice 3 candidate.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection rules

Two new pathology classes, both at the
`receiver/attrs.go::observeQualitySpans` hot path.

### 3.1 Malformed traceparent detection

The W3C trace context spec defines traceparent as:

```
{version}-{trace_id}-{parent_id}-{trace_flags}

  version:     2 hex chars, must be "00" for current spec
  trace_id:    32 hex chars, non-zero, MUST NOT be all zeros
  parent_id:   16 hex chars, non-zero, MUST NOT be all zeros
  trace_flags: 2 hex chars
```

Total expected length: 55 characters
(2 + 1 + 32 + 1 + 16 + 1 + 2 + 1 hyphens = 55).

Detection logic:

```go
func isWellFormedTraceparent(value string) bool {
    if len(value) != 55 {
        return false
    }
    // Check hyphen positions
    if value[2] != '-' || value[35] != '-' || value[52] != '-' {
        return false
    }
    // Check version segment is "00"
    if value[0:2] != "00" {
        return false
    }
    // Check trace_id segment (positions 3-34) is hex AND non-zero
    traceID := value[3:35]
    if !isHex(traceID) {
        return false
    }
    if isAllZeros(traceID) {
        return false
    }
    // Check parent_id segment (positions 36-51) is hex AND non-zero
    parentID := value[36:52]
    if !isHex(parentID) {
        return false
    }
    if isAllZeros(parentID) {
        return false
    }
    // Check trace_flags segment (positions 53-54) is hex
    if !isHex(value[53:55]) {
        return false
    }
    return true
}
```

A span with a `traceparent` attribute whose value fails this
check increments the `MalformedTraceparentSpans` counter for
the resource. Threshold: > 1% of spans (intentionally lower
than the slice 1 thresholds because ANY malformed traceparent
is unusual — most SDKs either propagate correctly or not at
all).

### 3.2 Missing traceparent on child detection

A span is "child" when its `parent_span_id` is non-zero AND
not the all-zero placeholder (reusing the slice 1
`isNonRootSpan` helper). A child span SHOULD carry a
`traceparent` attribute under the OTel semantic conventions
when the SDK received one from the upstream caller.

Detection logic:

```go
if isNonRootSpan(obs.ParentSpanID) {
    traceparent := lookupTraceparent(obs.Attrs)
    if traceparent == "" {
        // Child span with no traceparent → suggests SDK
        // didn't propagate context (case C from §1)
        counters.MissingTraceparentOnChildSpans++
    }
}
```

The `lookupTraceparent` helper checks both common attribute
names:
- `traceparent` (most common, used by most OTel SDKs)
- `http.request.header.traceparent` (less common; sometimes
  used when the SDK preserves the raw HTTP header)

A resource with > 5% of its child spans missing traceparent
fires `span-quality-traceparent-missing`. The threshold is
between the 1% (malformed) and 10% (slice 1 orphan)
thresholds because SDK propagation is mostly an "always or
never" pattern but some legitimate cases (purely internal
spans that don't represent an inbound boundary) may lack
traceparent.

### 3.3 Counter integration

Extend the existing `QualityCounters` from slice 1:

```go
type QualityCounters struct {
    OrphanSpans                  uint64
    MissingAttrSpans             uint64
    AttrMismatchSpans            uint64
    
    // Slice 2 additions
    MalformedTraceparentSpans    uint64
    MissingTraceparentOnChildSpans uint64
    
    // Note: ChildSpans denominator for traceparent-missing
    // ratio is tracked separately
    ChildSpans                   uint64
    
    TotalSpans                   uint64
    WindowStart                  time.Time
}
```

The ChildSpans counter is the denominator for the
HasMissingTraceparentOnChild axis (rather than using
TotalSpans). This is honest framing: a span without
parent_span_id (root span) can't be missing-traceparent-on-child,
so it shouldn't be in the denominator.

For the malformed-traceparent axis, the denominator is the
count of spans that HAVE a traceparent attribute (regardless
of whether it's well-formed). A span with no traceparent
isn't malformed; it's missing-on-child if it's a child span,
or correctly-rooted if it's a root span.

```go
type QualityCounters struct {
    // ... existing + new uint64 counters ...
    
    SpansWithTraceparent         uint64 // denominator for malformed_pct
    ChildSpans                   uint64 // denominator for missing_on_child_pct
}
```

## 4. Storage

No migration. The QualityCounters are still in-memory only;
the rolling 1h window per resource still applies. The new
counters add ~24 bytes per resource (3 × uint64); at the
existing 100K-key cap, ~2.4MB additional memory at full load.
Acceptable.

The audit payload from the existing background flusher gains
two new fields:
- `malformed_traceparent_pct`
- `missing_traceparent_on_child_pct`

Both fields use omitempty so cold-start audits stay
byte-identical.

## 5. Hot-path budget

Per-span overhead from slice 2 additions:

- isWellFormedTraceparent on attribute presence: O(55) string
  scan (5 hyphen checks + 50 hex/zero checks). Measured
  ~50ns.
- lookupTraceparent for child spans: 2 map lookups. ~30ns.

Combined: ~80ns per span when traceparent attribute exists,
~30ns per child span without traceparent. Slice 1's hot path
was ~200ns/span; slice 2 adds ~40% overhead in the worst case
(every span carries a traceparent + is a child). For typical
fleets (mix of root and child spans, only some carrying
traceparent), overhead is ~15-25%.

The §11 threat model from slice 1 (hot-path latency budget)
remains: total per-span overhead stays under 1µs.

## 6. API surface

### 6.1 Per-provider span_quality endpoint extension

The existing
`GET /api/v1/discovery/{provider}/inventory/{kind}/{id}/span_quality`
endpoint (v0.89.86) extends its `QualityCountersSnapshot`
response:

```json
{
  "resource_id": "...",
  "total_spans": 1234,
  "orphan_pct": 3.2,
  "missing_attr_pct": 6.3,
  "attr_mismatch_pct": 2.0,
  
  "malformed_traceparent_pct": 0.8,
  "missing_traceparent_on_child_pct": 4.1,
  
  "has_issues": true,
  "placeholders": [...]
}
```

The cross-provider aggregated endpoint
`GET /api/v1/discovery/span_quality` gains corresponding
aggregate fields.

### 6.2 No new endpoints

Slice 2 reuses existing endpoints. The new pathology fields
flow through the existing JSON shape; consumers ignore
unknown fields per the existing contract.

## 7. UI

The existing SPAN QUALITY panel from v0.89.87 currently shows
a 3-column health grid (Orphan trace / Missing attrs /
Attribute mismatch). Slice 2 extends to a 5-column grid:

```
  Orphan trace      Missing attrs    Attr mismatch    Malformed traceparent    Missing on child
       3.2%              6.3%             2.0%               0.8%                  4.1%
   12 resources      18 resources       6 resources      3 resources           14 resources
```

The panel remains hidden when all 5 percentages are zero.

Per-Inventory-row Quality dot logic from slice 1 stays the
same (any non-zero pathology → yellow/red). Two new pathology
classes get counted; the dot tooltip extends to show all 5
percentages on hover.

## 8. Recommendation kinds

Two new kinds:

```
span-quality-traceparent-missing
span-quality-traceparent-malformed
```

Both reuse the existing `span-quality-` webhook prefix (added
in v0.89.86); NO new webhook routing changes needed.

Reasoning templates:

- **span-quality-traceparent-missing**: "N% of child spans
  from this resource arrive without a traceparent attribute.
  The most common cause is the SDK's HTTP server
  instrumentation not extracting the context propagator
  on the inbound request. This Terraform PR enables explicit
  context propagator middleware on the application's HTTP
  server."

- **span-quality-traceparent-malformed**: "N% of spans from
  this resource carry a traceparent attribute that doesn't
  conform to the W3C trace context spec. The most common
  causes are: (1) an upstream service propagating a
  custom-format trace ID, (2) an SDK version mismatch where
  the upstream emits a 'next-version' (01) traceparent and
  the downstream rejects it. This Terraform PR updates the
  application's SDK to the latest version with W3C
  compatibility."

## 9. Slice 2 contract

**In:**

1. QualityCounters gains 3 new fields:
   MalformedTraceparentSpans, MissingTraceparentOnChildSpans,
   SpansWithTraceparent + ChildSpans denominators.
2. Hot-path observation extends with traceparent parsing.
3. isWellFormedTraceparent + lookupTraceparent helpers.
4. Per-resource snapshot endpoint exposes the new pct fields.
5. Cross-provider span_quality endpoint aggregates them.
6. SPAN QUALITY dashboard panel grows from 3 to 5 columns.
7. 2 new recommendation kinds in the proposer prompt with
   per-cause Terraform patterns.
8. Operator runbook covering all the above.
9. Acceptance tests covering both pathology detectors, the
   denominators (child vs total), the W3C format edge cases
   (all-zero trace_id, version "01", etc.), the new
   endpoint shape, and cold-start parity.

**Out:**

- tracestate parsing.
- Sampling decision validation.
- B3 / Jaeger / DataDog / Zipkin format detection.
- traceparent inspection on HTTP headers (slice 2 reads
  attributes only).
- Per-language SDK fingerprinting.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Foundation + hot-path detection.**
  isWellFormedTraceparent helper, lookupTraceparent helper,
  QualityCounters extension, observation hot path extension.
  Unit tests covering all W3C format edge cases. ~700-900
  lines. **v0.89.109.**
- **Chunk 2: API endpoint extension + proposer prompt +
  UI panel extension.** Snapshot fields, dashboard 5-column
  grid, 2 new recommendation kinds in proposer prompt.
  ~700-900 lines. **v0.89.110.**
- **Chunk 3: Operator runbook + README index update.**
  Extends docs/span-quality-operator-guide.md (in-place)
  with the slice 2 sections. README entry extends to cover
  v0.89.108-v0.89.111. ~300-400 lines. **v0.89.111.**

Total: 3 release tags. Smaller arc than recent tier work
because the detection extends an existing observer rather
than adding a new scanner surface.

## 11. Acceptance tests

1. **isWellFormedTraceparent — canonical example**.
   `"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"`
   → true.
2. **isWellFormedTraceparent — wrong length**.
   `"00-too-short"` → false.
3. **isWellFormedTraceparent — non-hex character in trace_id**.
   `"00-0123456789abcdef0123456789abcdeg-0123456789abcdef-01"`
   (note `g`) → false.
4. **isWellFormedTraceparent — all-zero trace_id**.
   `"00-00000000000000000000000000000000-0123456789abcdef-01"`
   → false (spec prohibits).
5. **isWellFormedTraceparent — all-zero parent_id**.
   `"00-0123456789abcdef0123456789abcdef-0000000000000000-01"`
   → false.
6. **isWellFormedTraceparent — version "ff" (future
   reserved)**.
   `"ff-...-..."` → false (slice 2 only accepts version "00";
   slice 3 may relax for forward-compat).
7. **isWellFormedTraceparent — version "01" (next-version
   reserved)**. Returns false. Operator may see this in real
   traffic when SDKs ship the next spec version.
8. **HasMalformedTraceparent counter increments on bad span**.
   Span with traceparent attribute = "invalid". Quality
   observer's MalformedTraceparentSpans goes from 0 to 1.
9. **HasMalformedTraceparent counter does NOT increment when
   no traceparent**. Span without traceparent attribute.
   MalformedTraceparentSpans stays at 0.
10. **HasMissingTraceparentOnChild counter increments on
    child span without traceparent**. Span with non-zero
    parent_span_id and no traceparent attribute.
    MissingTraceparentOnChildSpans goes from 0 to 1.
11. **HasMissingTraceparentOnChild counter does NOT increment
    on root span without traceparent**. Span with all-zero
    parent_span_id and no traceparent attribute. Counter
    stays at 0.
12. **HasMissingTraceparentOnChild denominator counts child
    spans only**. 100 spans (50 root, 50 child) all without
    traceparent. ChildSpans = 50; pct = 100%.
13. **Per-resource pct calculation correct**. Resource with
    1000 child spans, 50 missing traceparent → pct = 5%.
14. **Per-resource pct denominator correct for malformed**.
    Resource with 1000 spans total, 200 with traceparent,
    8 malformed → malformed_pct = 4% (denominator is
    SpansWithTraceparent = 200, not 1000).
15. **Recommendation fires at threshold**. 6% of child
    spans missing traceparent (above 5% threshold) → fires
    `span-quality-traceparent-missing`.
16. **Recommendation does NOT fire below threshold**. 4%
    of child spans missing traceparent → no recommendation.
17. **Cold-start parity preserved**. All 4 providers
    cold-start prompts byte-identical to v0.89.107 when no
    traceparent rows trigger recommendations.
18. **Hot-path budget held**. Benchmark verifies per-span
    overhead from slice 2 stays under 100ns on the canonical
    path (traceparent present, child span).

## 12. Threat model

**No new external surface.** Slice 2 extends the existing
OTLP receiver hot path. No new API calls to any cloud.

**Hot-path latency budget.** §5 documents per-span overhead.
The §11 threat model from slice 1 (total per-span overhead
under 1µs) remains. Slice 2 adds ~50-80ns; the budget holds.

**Counter overflow.** The 1h rolling window per resource
from slice 1 remains. The new counters reset on window
rollover.

**False positives on non-W3C SDKs.** Operators using B3 /
Jaeger / DataDog / Zipkin propagation see false positives
on HasMissingTraceparentOnChild. The exclusion table from
#531 slice 2 chunk 4 handles. Runbook documents.

**No span content logging.** Slice 1's PII posture continues:
the W3C parsing inspects attribute presence + value format,
never logs attribute values to audit. Audit payload carries
percentages only.

## 13. Slice 3 candidates

- tracestate parsing (per-vendor metadata validation).
- Sampling decision propagation analysis.
- B3 / Jaeger / DataDog / Zipkin context detection.
- Per-language SDK fingerprinting.
- HTTP header inspection (when SDK doesn't attach to
  attributes).
- Per-vendor traceparent format extensions (some SDKs use
  non-canonical 32-char trace_id).
- Auto-fix for trivial cases (SDK version pin update).

---

**Strategic frame:**

Slice 2 doesn't grow the universal claim — it makes the
existing span quality claim more rigorous. Operators who run
Squadron's discovery + verification now get an answer at
THREE levels for the "where did my trace go?" question:

1. Is the cloud-native trace primitive enabled at the event
   source? (event source slice 1)
2. Does the event source's CONFIG preserve trace context
   end-to-end? (event source slice 2)
3. Does the trace context that DOES arrive at Squadron's
   OTLP receiver conform to the W3C spec? (span quality
   slice 2 — this arc)

These three diagnostic layers cover the full "request → orchestration
→ execution" chain Squadron scans. The Tuesday LinkedIn
drumbeat narrative gains the most specific answer yet to
"where did my trace go?":

> "Your downstream Lambda received a traceparent header from
> the EventBridge bus, but the header is malformed — version
> segment is '01' (a future-spec reserved value). The SDK
> rejected it and generated a fresh parent_span_id. Your
> trace chain looks broken because the propagation is broken
> at the SDK-version-mismatch layer. Squadron just drafted
> the PR to pin the upstream SDK to the W3C-compliant version."

This is exactly the diagnosis operators struggle to find
themselves — buried in SDK changelogs, hidden in version-pin
files, never surfaced by the cloud console or by typical
observability backends.
