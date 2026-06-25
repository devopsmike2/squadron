# Consumer-Lag Substrate Integration — slice 5 (closes the slice-2 lag deferrals)

Status: chunk 1 shipping in v0.89.182 (#824 Stream 221).

## 1. Why this arc exists

Consumer lag detection slice 2 (v0.89.167-171) shipped the lag
axis (four Detail keys: `lag_backlog_depth`,
`lag_backlog_depth_high`, `lag_consumer_silence_seconds`,
`lag_consumer_silence_high`) across all four clouds, but two of
them shipped as honest framing rather than real detection:

- **GCP Cloud Tasks (slice 2 chunk 2, §3.1):** the lag axis is
  hard-coded absent (`-1`) because the Cloud Tasks admin API
  doesn't surface task count as a directly-queryable field during
  the scan walk.
- **Azure Service Bus (slice 2 chunk 3, §3.2):** the lag axis is
  hard-coded absent because the namespace-level scanner doesn't
  reach the per-queue `activeMessageCount` sub-resource.

AWS SQS (slice 2 chunk 1) and OCI Queue Service (slice 2 chunk 4)
already ship real lag detection off already-read scanner fields.

This arc closes the two honest-framing deferrals the same way the
poison-rate substrate arc (slice 4, v0.89.177-181) closed the
§3.3 poison-rate deferrals: by reading the backlog metric from the
per-cloud MetricQuerier substrate the cold-start latency arc
built. It's the same proven pattern, now applied to the lag axis.

## 2. Scope — backlog axis, not silence

This arc closes the **backlog-depth** half of the lag axis
(`lag_backlog_depth` + `lag_backlog_depth_high`). The
**consumer-silence** half (`lag_consumer_silence_seconds` +
`lag_consumer_silence_high`) stays honest-framed for the two
metric-backed clouds, because neither Cloud Tasks nor Service Bus
exposes a clean per-queue "oldest message age" / "time since last
dispatch" metric the way SQS's `ApproximateAgeOfOldestMessage`
does. Honest framing: backlog becomes real; silence remains a
documented deferral. A high backlog is the primary lag signal
operators act on, so this closes the operationally important half.

## 3. Chunk 1 (this slice) — GCP Cloud Tasks backlog

Reads `cloudtasks.googleapis.com/queue/depth` (a GAUGE: number of
tasks currently in the queue) via Cloud Monitoring, scoped to the
queue with `resource.labels.queue_id`. Because it's a gauge, the
query uses the `ALIGN_MAX` per-series aligner and the substrate's
MAX cross-period rollup — i.e. the **peak backlog over the trailing
window** — rather than the `ALIGN_DELTA` + SUM the count metrics
(poison-rate, sampling-rate) use.

```
lag_backlog_depth      = MAX(queue/depth) over the trailing 1h window
lag_backlog_depth_high = lag_backlog_depth >= 1000
```

`1000` matches the AWS `BacklogDepthHighThreshold` + OCI
`OCIBacklogDepthHighThreshold` for cross-cloud consistency.

### 3.1 Real-zero vs absent

Same contract as the poison-rate arc: `SampleCount == 0` (no
datapoints) → keep the honest-framing absent sentinel (`-1`); a
non-empty series → real reading (even a real `0` = empty queue).
If the metric name doesn't match GCP's current reference, the
query returns no datapoints and the detector degrades safely to
`-1` — never false data.

## 4. Chunk 2 (future) — Azure Service Bus backlog

Reads the `ActiveMessages` metric per queue via the **EntityName
dimension split** the poison-rate chunk 3b (v0.89.180) already
built (`$filter="EntityName eq '*'"`). This closes the slice-2
§3.2 lag gap reusing existing infrastructure — one metric name
change from the dead-letter path. Worst-queue attribution, same as
the poison-rate per-queue path.

## 5. Cold-start parity

Additive at the Detail-bag level: the enrichment **overwrites**
the two backlog keys the projection already wrote and touches no
other keys (including the two silence keys, which keep their
honest-framing `-1`). A deployment without the metric client wired
sees the enrichment as a no-op and observes byte-identical output
to the pre-enrichment projection.

## 6. IAM

Unchanged. The same `monitoring.timeSeries.list` (GCP) /
`microsoft.insights` (Azure) read the cold-start + poison-rate
substrate already uses covers the backlog metrics — no new
permission.

## 7. Chunk map

- **Chunk 1 (this slice, v0.89.182): GCP Cloud Tasks backlog real**
  (`queue/depth`, gauge / ALIGN_MAX). Silence stays honest-framed.
- Chunk 2: Azure Service Bus backlog real (`ActiveMessages`
  per-queue via EntityName split). Silence stays honest-framed.

## 8. Acceptance (chunk 1)

1. `queue/depth` routes as a gauge: `ALIGN_MAX` aligner, MAX
   rollup, scoped to `resource.labels.queue_id`.
2. `DetectCloudTasksBacklog` returns the real peak depth + high
   band when datapoints exist; absent (`-1`) on `SampleCount == 0`.
3. `enrichCloudTasksLag` overwrites `lag_backlog_depth` +
   `lag_backlog_depth_high`, leaves the two silence keys at their
   honest-framing `-1`, and is a no-op when the metric client is
   unwired.
