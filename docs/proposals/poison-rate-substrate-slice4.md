# Poison-Rate Substrate Integration — slice 4 (closes the §3.3 deferrals)

Status: chunk 1 shipping in v0.89.177 (#819 Stream 216).

## 1. Why this arc exists

Poison-message rate analysis slice 3 (v0.89.172-176) shipped
the poison-rate axis across all four clouds under §3.3
**substrate-metric-dependence** honest framing: every
per-cloud `detect*PoisonRate` helper returns the absent
sentinel (`poison_rate_per_hour = -1`,
`poison_rate_high_band = false`), and the four
`*-poison-rate-monitor-add` recommendation kinds fire with
reasoning text that explicitly tells the operator Squadron
cannot yet compute the rate from the scanner pass.

That was the honest thing to ship at the time, but it is a
deferral, not a capability. The §3.3 framing names exactly
one thing that closes it: a per-cloud MetricQuerier
integration that reads the time-series metric the
single-pass scanner does not. This arc builds that
integration, cloud by cloud, converting the hard-coded
absent sentinels into real detection.

The arc deliberately mirrors the cold-start latency slice 1
-> slice 2 progression, which built the CloudWatch /
Cloud Monitoring / Azure Monitor / OCI Monitoring
MetricQuerier substrate per cloud. The AWS substrate from
that arc (`Scanner.QueryAggregate`, the `CloudWatchClient`
interface, the per-account rate limiter, the throttle-retry
loop) already exists and is reused here verbatim — this arc
adds metric names and a routing branch, not a new substrate.

## 2. Scope of chunk 1 (this slice) — AWS SQS

Chunk 1 closes the AWS §3.3 deferral only. The other three
clouds keep their honest-framing absent sentinels until
their own chunks land (chunk 2 GCP, chunk 3 Azure, chunk 4
OCI), exactly as slice 3 traversed the four clouds one chunk
at a time.

**In:**

1. AWS/SQS CloudWatch metric path in `metrics.go`
   (`SQSMetricNamespace`, `SQSNumberOfMessagesSentMetricName`,
   `querySQSCounterSum`, `extractSQSQueueName`), reusing the
   existing rate limiter + throttle-retry scaffold.
2. `DetectSQSPoisonRate(ctx, dlqARN)` — a real
   CloudWatch-backed detection that reads the DLQ's
   `NumberOfMessagesSent` SUM over a rolling 1-hour window.
3. An enrichment pass in `scanRegionSQS` that, for every SQS
   source queue with a reachable in-account DLQ, overwrites
   the honest-framing `poison_rate_per_hour` +
   `poison_rate_high_band` Detail keys with the real reading.
4. Tests + prompt + docs updates.

**Out (future chunks / explicitly deferred):**

- GCP / Azure / OCI substrate chunks (slice 4 chunks 2-4).
- Cross-account / dangling DLQs: a source queue whose DLQ
  ARN is not in the scanned account's ARN set keeps the
  honest-framing absent sentinel (we cannot read metrics for
  a queue we did not enumerate).
- Persisted baselines / rolling history: poison rate is a
  single-window reading, not a baseline comparison like
  cold-start, so no storage adapter is added.

## 3. Detection rule

For a source SQS queue with a redrive policy pointing at a
reachable DLQ:

```
poison_rate_per_hour  = SUM(NumberOfMessagesSent) on the DLQ
                        over the trailing 1h window
poison_rate_high_band = poison_rate_per_hour >= 60   (1/min)
```

`NumberOfMessagesSent` on the DLQ is the proxy for "poison
messages arriving in the DLQ over the last hour."
`OCIPoisonRatePerHourHighThreshold` / `PoisonRatePerHourHighThreshold`
(60, 1/min) is the shared cross-cloud band from slice 3 §4 —
unchanged, now actually evaluated for AWS.

### 3.1 Real-zero vs absent

The MetricQuerier empty-result contract distinguishes the
two cases via `SampleCount`:

- `SampleCount > 0` → CloudWatch returned datapoints. Even a
  SUM of 0 is a **real** "zero poison messages this hour"
  reading: `poison_rate_per_hour = 0`,
  `poison_rate_high_band = false`.
- `SampleCount == 0` → CloudWatch returned no datapoints
  (queue too new, metric not yet emitted). We cannot assert
  a rate, so we keep the honest-framing absent sentinel
  (`poison_rate_per_hour = -1`).

This keeps the honest framing honest: -1 always means "not
measured," never "measured as zero."

## 4. Honest-framing transition

After chunk 1, AWS SQS no longer carries §3.3 framing for
the poison-rate axis — it ships **real detection**. The
`sqs-poison-rate-monitor-add` recommendation still fires
(operators still want the CloudWatch alarm wired by
Terraform), but its reasoning text now reports the measured
rate instead of disclaiming the gap. GCP / Azure / OCI keep
the slice-3 §3.3 reasoning until their chunks land. The
proposer prompt documents the mixed state explicitly so the
model does not over-claim measurement for the three clouds
that are still deferred.

## 5. Cold-start parity

Additive only at the Detail-bag level: the enrichment
**overwrites** two keys that the projection already wrote
(`poison_rate_per_hour`, `poison_rate_high_band`); it adds
no new keys and touches no slice-4 / slice-1-DLQ / slice-2-lag
keys. A deployment that has not wired the CloudWatch client
(`cwClient == nil`) sees the enrichment pass as a no-op and
observes byte-identical output to v0.89.176.

## 6. IAM

Unchanged from the slice-4 SQS scanner plus the cold-start
substrate: `cloudwatch:GetMetricStatistics` (already granted
for the Lambda metric paths) covers the AWS/SQS namespace —
GetMetricStatistics is namespace-agnostic at the IAM layer.
No new permission.

## 7. Chunk map

- **Chunk 1 (this slice, v0.89.177): AWS SQS real detection.**
- **Chunk 2 (v0.89.178): GCP Cloud Tasks real detection** —
  Cloud Monitoring `task_attempt_count` failed-attempt rate
  (`response_code != "OK"`), measured on the queue itself (no
  DLQ primitive).
- Chunk 3: Azure Service Bus (Azure Monitor
  `DeadletteredMessages`).
- Chunk 4: OCI Queue Service (OCI Monitoring
  dead-letter delivery metric) — CLOSES the substrate arc and
  retires the last §3.3 poison-rate deferral.

## 8. Acceptance (chunk 1)

1. `extractSQSQueueName` parses `queuename` from
   `arn:aws:sqs:<region>:<account>:<queuename>`; rejects
   non-SQS ARNs.
2. `querySQSCounterSum` sums `NumberOfMessagesSent` across
   per-period datapoints (AWS/SQS namespace, QueueName
   dimension), reusing the rate limiter + throttle retry.
3. `DetectSQSPoisonRate` returns a real rate + high band when
   datapoints exist; rate 0 on real-zero; absent (-1) on
   `SampleCount == 0`.
4. `enrichSQSPoisonRate` overwrites the two Detail keys for a
   queue with a reachable DLQ; is a no-op when
   `cwClient == nil`; skips queues whose DLQ is unreachable
   (keeps the absent sentinel).
