# Audit finding: metric-based serverless detection is not wired in the production scanner factories

**Status:** ✅ RESOLVED via **option 2** (opt-in flag). AWS v0.89.330, OCI
v0.89.331, GCP v0.89.332. See "Resolution" below.
**Confidence:** high (static evidence below; grep + read across the scanner
packages and the production factory).
**Severity:** high — a whole detection feature class was dormant in production
for GCP/OCI, and for AWS unless the commercial flag was on.

## Resolution (option 2 — opt-in flag, default off)

The maintainer chose **option 2**: a single config switch,
**`serverless_metric_detection.enabled`** (default false), that constructs the
per-cloud metric client and activates the **native-metric** serverless
detectors. The OSS default stays at zero billed metric reads; the operator opts
in (and grants the metric IAM/scope) to turn them on. The add-on-dependent
detectors (AWS Lambda **cold-start** via Lambda Insights; **all** Azure Functions
detection via Application Insights) remain under `commercial_detectors.enabled`
— they need a paid telemetry add-on, not just a native metric, so they are out
of scope for this flag.

Shipped in three slices:

- **AWS (v0.89.330).** Decoupled Lambda **error-rate** from the commercial gate:
  it runs on the native `AWS/Lambda` `Errors`+`Invocations` metrics under the new
  flag (building a per-region CloudWatch client on demand), no Lambda Insights
  required. Cold-start stays commercial (InitDuration only lives in the Lambda
  Insights namespace).
- **OCI (v0.89.331).** Folded the OCI Functions walk + cold-start/error-rate
  detection passes into `Scan()` (the slice-1 chunk-4 deferral) and wired the
  already-implemented `signedMonitoringClient` + observation stores + connection
  id in `OCIFactory`. Gated on the flag, so a default scan is unchanged.
- **GCP (v0.89.332).** Wrote the production Cloud Monitoring V3 adapter
  (`gcp/metrics_sdk.go`, the deferred chunk-2 SDK adapter) + wired the stores +
  connection id in `GCPFactory`; the `monitoring.read` OAuth scope is requested
  only when the flag is on (least privilege).
  **Live-verified ✅ (v0.89.335, 2026-06-30).** The production adapter was run
  against a real Cloud Monitoring backend via ADC (the gated harness in
  `internal/discovery/gcp/metrics_live_test.go`). A timeSeries round-trip
  confirmed: ADC auth through `buildOAuthHTTPClient` (with the `monitoring.read`
  scope) + `buildMonitoringClient`, a real `timeSeries.list` request returning
  200, correct `TypedValue` DoubleValue parsing (returned the exact written
  value), the cross-period rollup, and the SampleCount proxy + interval parsing.
  The SampleCount proxy is 1 per populated 5m period (see the metrics_sdk.go
  header), so the cold-start gate (`ColdStartBaselineMinimumSamples` = 50) needs
  >= 50 populated buckets across the 168h baseline window.

  Two sub-paths remain canned-only (low risk, same adapter code): the Int64Value
  (ALIGN_DELTA count) decode and the end-to-end against a deployed Cloud Run
  service's `request_latencies` distribution. The harness runs them when a real
  Cloud Run service with traffic is available:

  ```sh
  SQUADRON_GCP_LIVE=1 \
  SQUADRON_GCP_SA_JSON="$HOME/.config/gcloud/application_default_credentials.json" \
  SQUADRON_GCP_PROJECT=my-project \
  SQUADRON_GCP_LOCATION=us-central1 \
  SQUADRON_GCP_SERVICE=my-cloud-run-service \
  go test ./internal/discovery/gcp/ -run TestGCPLiveMonitoring -v
  ```

  (Or pass `SQUADRON_GCP_FILTER`/`SQUADRON_GCP_ALIGNER` to point the harness at
  any metric with data — how the v0.89.335 verification was run, against a custom
  metric round-tripped through Cloud Monitoring on an otherwise-empty project.)
  The SA/ADC principal needs `roles/monitoring.viewer`.

Follow-up (✅ resolved, v0.89.334): OCI Functions **inventory** discovery is now
**unconditional** — `scanServerlessTier` always walks Functions (an inventory
tier like compute/db/OKE) and populates `result.Serverless`; only the
native-metric cold-start/error-rate **detection** passes stay gated on the
monitoring client. This also un-inerts the structural OCI serverless
recommendations (apm-enable / otel-distro), which key off the discovered rows
rather than metrics.

The original finding (unchanged) follows.

## Summary

The serverless **regression detectors** — cold-start latency (24h vs 168h P95)
and error-rate spike (24h vs 168h error ratio) — run inside each cloud
scanner's `Scan()` pass, but every one of them is **nil-tolerant on its metric
client and short-circuits when that client is nil**. In production the metric
client is nil for GCP and OCI (always) and for AWS (unless
`commercial_detectors.enabled`), because the **production scanner factory never
constructs a metric client** — only the test suites do, via the `With*Client`
seams. So in a normal deployment these detectors never run, never write the
`cold_start_observation` / `error_rate_observation` tables, and therefore
nothing downstream of them produces data.

## Evidence

Production scanner construction — `internal/discovery/scannerfactory/factory.go`:

- `GCPFactory.Build` → `&gcp.Scanner{ProjectID, SAJSON, Region}` — no
  `metricsClient`.
- `OCIFactory.Build` → `&oci.Scanner{TenancyOCID, ...}` — no `monitoringClient`.
- `AzureFactory.Build` / AWS factory — no metric client either; AWS's CloudWatch
  client is built only on the commercial path (below).

The metric-client setters are **test-only** (no production call site anywhere):

- `gcp.Scanner.WithMetricsClient` — referenced only in `gcp/*_test.go`.
- `oci.Scanner.WithMonitoringClient` — referenced only in `oci/*_test.go`.
- `aws.Scanner.WithCloudWatchClient` — referenced only in `aws/*_test.go`.

There is no production constructor of a Cloud Monitoring / OCI Monitoring client
in `internal/discovery/{gcp,oci}` (grep for `monitoring.NewMetricClient` etc.
returns only test files).

The detection passes run but short-circuit on the nil client:

- `gcp/error_rate.go:141` — `if s.metricsClient == nil || s.errorRateStore == nil || s.connectionID == "" { return }`
- `gcp/cold_start.go:249` — same shape.
- `oci/error_rate.go:128`, `oci/cold_start.go:227` — `if s.monitoringClient == nil || ...`
- `aws/error_rate.go:158`, `aws/cold_start.go:260` — `if !s.commercialDetectors && s.cwClient == nil { return }`

`gcp/scanner.go:347,356` call `runColdStartDetectionForServerless` /
`runErrorRateDetectionForServerless`, with the in-code comment explicitly noting
the passes are "nil-tolerant on metricsClient" — i.e. the nil case is the
designed-for path, not an accident.

The AWS exception: `aws/commercial_activation.go` builds the CloudWatch client
(`cloudWatchForRegion`) only inside the commercial path
(`EnableCommercialDetectors`, gated on `config.CommercialDetectors.Enabled`). So
AWS cold-start + error-rate run **only** when the commercial flag is on — which
is also the only reason the earlier live-verifications passed.

## Per-cloud production state

| Cloud | Cold-start | Error-rate | Why |
|-------|-----------|-----------|-----|
| AWS Lambda | runs iff `commercial_detectors.enabled` | runs iff `commercial_detectors.enabled` | cwClient built only on the commercial path |
| GCP Cloud Run / Functions | **dormant** | **dormant** | `metricsClient` never wired in the factory |
| OCI Functions | **dormant** | **dormant** | `monitoringClient` never wired in the factory |
| Azure Functions | runs iff `commercial_detectors.enabled` | runs iff `commercial_detectors.enabled` | App Insights component path (commercial) |

## Downstream impact

Because no observations are written, everything that reads them is inert in
production for the dormant paths:

- The cold-start + error-rate **regression recommendations** (v0.89.315–319) —
  the recs only fire when an observation exists.
- The **Workload Health** panel's cold-start + error-rate axes (v0.89.323) read
  the persisted annotations — zero when detection never ran.
- The **per-resource** cold-start / error-rate detail endpoints (now wired,
  v0.89.325) return "no observation".

(The structural/config detections — trace-coverage presence, OTel-axis
presence, poison-message DLQ depth via data-plane attributes — are NOT affected;
those don't depend on a metric client. Only the metric-based regression
detectors are.)

## Why this looks intentional (and why that matters)

The nil-tolerant design, the AWS commercial gating, and the cost note in
`detection-coverage.md` ("CloudWatch `GetMetricStatistics` is billed per
request") together suggest the production metric-client wiring was deliberately
deferred / made opt-in for **cost** reasons: activating it issues live metric
API reads per serverless resource on every scan (CloudWatch is billed per
request; Cloud Monitoring / OCI Monitoring have free tiers then bill). That is a
product + cost decision, which is why this is filed as a finding rather than
auto-wired.

## `detection-coverage.md` accuracy

The matrix currently marks GCP / OCI serverless error-rate and GCP serverless
cold-start as ✅ ("Works on native cloud metrics, no extra setup"). Given the
above they are not wired in the stock all-in-one binary, so the ✅ overstates
the out-of-the-box state. A caveat pointer to this audit has been added; the
matrix verdicts should be reconciled once the decision below is made.

## Recommended options (maintainer decision)

1. **Wire the metric clients in the factories (activate).** Build a Cloud
   Monitoring / OCI Monitoring / CloudWatch client from the connection creds in
   each `Build`, decouple AWS error-rate from the cold-start commercial gate
   (error-rate uses the native `AWS/Lambda Errors` metric — no add-on), and
   accept the per-scan metric-read cost. Highest value (activates the whole
   detection→rec→Workload-Health pipeline); adds cost.

2. **Make it an explicit opt-in** (mirror `commercial_detectors.enabled`): a
   single config/env switch that wires the metric clients for all clouds.
   Preserves the cost-conscious default; makes the capability reachable in the
   stock binary (it currently is not, for GCP/OCI, at all).

3. **Keep deferred + make docs fully honest.** Reconcile the
   `detection-coverage.md` matrix to mark the metric-based serverless detectors
   as not-wired-by-default, and document the wiring as a deployment step.

Option 2 is the closest analogue to how AWS/Azure commercial detection already
works, and the smallest behavior-preserving change.
