# Detection data-source add-ons — recommend enabling them (arc)

Status: building v0.89.258 (slice 1). Author: autonomous session. Decisions by
Michael (this session).

## Decisions

1. **Serverless cold-start + error detection (#152 AWS, #153 Azure).** The
   native metric doesn't exist; the signal lives only in a paid add-on (AWS
   **Lambda Insights** `init_duration`; Azure **Application Insights**). Decision:
   **recommend enabling the add-on**, and in the reasoning explain WHY it matters
   (without it Squadron is blind to cold-start latency + per-function error rate)
   and the COST (paid add-on). For AWS, also offer the cheaper CloudWatch Logs
   metric-filter alternative.
2. **Queue poison-rate (#156 AWS SQS, #159 OCI).** No native "moved to DLQ"
   counter. Decision (operator's recommendation): depth/presence-based detection
   — honest "N messages in the DLQ", never a fabricated rate; for OCI derive a
   rate from `deadLetterQueueDeliveryCount` deltas across the now-persisted scan
   history.

## Key finding (shapes the serverless approach)

The deterministic cold-start/error RECOMMENDATION branches in
internal/proposer/cold_start.go + error_rate.go are **dormant** — defined +
unit-tested but never called from the live recs flow. Only the per-row
ANNOTATION pass (AnnotateServerlessWithColdStart) is wired, and it just
populates inventory-row fields when data already exists.

The WIRED recommendation path is the **LLM discovery proposer**, which already
receives serverless inventory including the trace-axis signal and already emits
serverless tier kinds (lambda-otel-layer, azfunc-appinsights-enable, …). So the
right lever for "recommend enabling the add-on" is the proposer PROMPT, not the
dormant deterministic branch.

## Slice plan

- **Slice 1 (this) — serverless add-on enablement in the proposer prompt.**
  Add a high-priority "detection-prerequisite add-ons" framing to the serverless
  section; add the missing AWS **lambda-insights-enable** kind (the Lambda
  cold-start/error prerequisite, with the Logs-metric-filter alternative); enrich
  the existing **azfunc-appinsights-enable** kind to state the cold-start/error
  rationale + cost. Layer ARNs are framed as "resolve the current published ARN"
  (not hardcoded) per the #109/#111 freshness fix.
- **Slice 2 (shipped v0.89.259) — AWS SQS DLQ depth detection (#156):** the
  source queue reads its DLQ's current ApproximateNumberOfMessages directly from
  the scan's attribute walk (no extra call, no CloudWatch; same-account/region
  DLQs). Surfaces poison_dlq_depth + poison_dlq_nonempty. Honest proxy: a
  drained DLQ reads empty.
- **Slice 3 — OCI queue poison via deadLetterQueueDeliveryCount delta over scan
  history (#159):** first consumer of persisted scan history for a derived rate.

## Honest framing

- Slice 1 makes the proposer RECOMMEND the add-on; it does not itself enable it
  (the operator merges the IaC PR). Detection still can't fire until the add-on
  is on — which is exactly why recommending it is the unblock.
- Depth/presence (slice 2/3) is a proxy: a drained DLQ reads empty, so it can
  under-report. Documented as such.
