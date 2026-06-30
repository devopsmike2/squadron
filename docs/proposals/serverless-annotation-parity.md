# Serverless cold-start + error-rate annotation parity (GCP/OCI/Azure)

**Status:** design — implementation in slices. Follows the sampling-rate
activation arc (#295, v0.89.336–340), which surfaced this sibling gap.

## The gap

The per-cloud discovery scan handlers populate the serverless inventory rows'
detection fields by running annotation passes after the scanner returns. There
are three such passes, all in `internal/api/handlers`:

- `AnnotateServerlessWithColdStart` → `cold_start_p95_ms` + `cold_start_exceeds_threshold`
- `AnnotateServerlessWithErrorRate` → `current_error_rate` + `error_rate_exceeds_threshold`
- `AnnotateServerlessWithSampling` → `sampling_ratio` + `sampling_exceeds_floor`

**Only the AWS handler (`discovery.go`) runs all three.** Before the #295 arc,
GCP/OCI/Azure ran *none*; #295 added the sampling pass to GCP + OCI. That leaves
**cold-start + error-rate annotation still AWS-only** — GCP, OCI, and Azure
serverless rows render "—" for cold-start latency and error rate in the UI,
even though the data exists.

## Why this is real, not inert

The annotation passes read the persisted observation tables
(`cold_start_observation`, `error_rate_observation`) via the handler's
`ColdStartObservationReader` / `ErrorRateObservationStore`. Those tables **are
written by the GCP/OCI/Azure scanners**:

- `internal/discovery/gcp/{cold_start,error_rate}.go` → `SaveColdStartObservation` / `SaveErrorRateObservation`
- `internal/discovery/oci/{cold_start,error_rate}.go` → same
- `internal/discovery/azure/error_rate.go` (+ cold-start) → same

The writes ride `serverless_metric_detection.enabled` for GCP/OCI (the #300
metric clients) and the commercial App Insights / Lambda Insights tier for
Azure. And the readers are already threaded into the GCP/OCI/Azure handlers —
the regression-recommendation path (`WithGCPRegressionStores` etc.) receives
`s.coldStartObservationReader` + `s.errorRateObservationReader`, the same server
fields the AWS annotation reads from. So the data is persisted and the readers
are in hand; the only missing piece is the annotation call + a store field/setter
on each handler. Non-inert by construction.

## Plan (mirror the sampling slices)

Each slice, per cloud, adds to the handler: a `coldStartStore` +
`coldStartConstants` + `errorRateStore` field, `WithXColdStartObservationStore`
+ `WithXErrorRateObservationStore` setters (reusing the shared
`ColdStartObservationReader` / `ColdStartAnnotationThresholds` /
`ErrorRateObservationStore` types), the two annotation calls after the scan
(beside the sampling block), the server-trampoline wiring from
`s.coldStartObservationReader` / `s.errorRateObservationReader` (with the same
`NewStaticColdStartDetectionConstants(24,168,1.5,500.0)` AWS uses, so thresholds
stay single-sourced), and a handler test.

1. **GCP** — reference slice.
2. **OCI** — mirror.
3. **Azure** — mirror (data is commercial-gated; nil-safe → "—" when the add-on
   is off). This arc added Azure cold-start + error-rate (App-Insights-sourced);
   Azure sampling was activated separately (Option 2, native
   `FunctionExecutionCount` — see `sampling-rate-activation.md`).
   Docs reconciliation + full gate.

## Landing

All three slices shipped together (the wiring is identical per cloud and each
handler already carried the observation stores via its `WithXRegressionStores`
setter — only a `coldStartConstants` field/setter + the two annotation calls +
the server-trampoline constants line were missing):

- **GCP / OCI** — cold-start + error-rate rows populate when
  `serverless_metric_detection` is on (the scanner persists the observations).
- **Azure** — rows populate when the App Insights commercial add-on is on (its
  observation source); nil-safe → "—" otherwise.
- Each handler's annotation block sits beside the existing trace-emission
  annotations; the shared, provider-agnostic `AnnotateServerlessWith{ColdStart,
  ErrorRate}` helpers (already tested across all four surfaces) do the work.
- Tests: `discovery_serverless_annotation_parity_test.go` pins that each handler
  carries the stores + thresholds and that a wired error-rate store populates
  `CurrentErrorRate` on the cloud's serverless surface.

With this, the three serverless annotation passes (cold-start, error-rate,
sampling) are AWS-parity across all four clouds (Azure sampling was activated
natively in a follow-up — Option 2, see `sampling-rate-activation.md`).

## Non-goals

- No new config flag — rides the existing detection gates.
- No change to the detection math, thresholds, or the persisted observation
  schema — purely projecting already-persisted observations onto the scan rows.
