# Orchestration tier slice 2 — OCI Resource Manager

**Status:** design doc, locked for slice 2 implementation.
Closes the qualification on the orchestration tier in
Squadron's universal claim. After this arc, every tier
(compute / database / kubernetes / serverless / orchestration
/ event sources) is cleanly 4-cloud without an asterisk.

**See also:**
[Orchestration tier slice 1](./orchestration-tier-slice1.md),
[Serverless tier slice 1](./serverless-tier-slice1.md),
[Event source tier slice 1](./event-source-tier-slice1.md),
[Workload Health panel](./workload-health-panel-slice1.md).

## 1. Problem

Orchestration tier slice 1 (v0.89.94-98) shipped AWS Step
Functions + GCP Workflows + Azure Logic Apps. OCI
orchestration was honestly deferred to slice 2 because OCI's
orchestration primitives are shape-different from the
AWS/GCP/Azure trio:

- Step Functions / Workflows / Logic Apps are **workflow
  orchestration** — state machine engines that sequence
  callouts to other resources.
- OCI's closest primitives are:
  - **Resource Manager** — Terraform-as-a-service. Stacks
    (Terraform configurations) and Jobs (apply/destroy
    operations on stacks). **Infrastructure** orchestration.
  - **Process Automation** — BPMN business process engine.
    True workflow orchestration but newer product with
    smaller adoption.

The orchestration tier slice 1 runbook and the universal
claim's strategic frame both honestly noted this:

> "OCI orchestration deferred to slice 2 because Resource
> Manager + Process Automation are shape-different from
> the AWS/GCP/Azure trio."

This qualification has lived in the universal claim for
multiple arcs since v0.89.98. Closing it cleans the
narrative — after slice 2, the universal claim reads
cleanly:

> "Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
> AND EVENT SOURCES..."

Slice 2 picks **Resource Manager** as the OCI orchestration
surface because:

1. **Stacks + Jobs map clearly to the orchestration pattern.**
   A Stack represents a long-lived orchestration definition
   (the Terraform config); a Job represents a single
   orchestration execution (the apply/destroy run). The
   parallel to a Step Functions state machine + execution
   is concrete.
2. **Operators care about telemetry from RM Jobs.** A failed
   apply on a stack should emit logs and ideally OTel
   spans; today most teams have no visibility into whether
   RM is logging at all.
3. **The OCI Logging service integrates with RM.** The
   detection axes mirror OCI Streaming's pattern from
   v0.89.101 — Stack has Logging configured + Job logs OCID
   set.

Slice 2 explicitly defers Process Automation:

> Process Automation is the **closer semantic match** to
> Step Functions / Workflows / Logic Apps (it's BPMN-based
> workflow orchestration). However, Process Automation:
> - Is a 2022+ product with smaller adoption than RM
> - Requires its own scanner substrate (different API
>   surface than Resource Manager)
> - Doesn't map cleanly to Squadron's existing scanner
>   pattern
>
> Slice 3 may add Process Automation when adoption justifies
> the substrate cost. For now, slice 2 closes the qualified
> claim with Resource Manager as the operator-meaningful
> surface.

## 2. Non-goals (slice 2)

- **OCI Process Automation.** The semantically closest match
  to Step Functions / Workflows / Logic Apps but with
  smaller adoption. Slice 3 candidate.
- **Per-execution Job log content inspection.** Slice 2
  detects whether the Stack has Logging configured, NOT
  what the Job logs contain. Inspecting per-Job log content
  for trace context is a slice 3 concern (PII surface).
- **Stack state file inspection.** Resource Manager
  manages Terraform state. Squadron does NOT inspect state
  contents. Stack metadata only.
- **Job-level cold-start / latency / error rate
  analysis.** The substrate's three diagnostics
  (cold-start, sampling, error rate) target serverless
  surfaces. Resource Manager Jobs are infrastructure
  operations; the substrate diagnostics don't apply.
  Slice 3+ may add RM-specific diagnostics (Job duration,
  Job failure rate) when needed.
- **Auto-fix.** Squadron remains a recommender.

## 3. Per-cloud detection surface (OCI only)

The slice 1 design doc §3.1-3.3 covers AWS Step Functions /
GCP Workflows / Azure Logic Apps detection. Slice 2 adds
§3.4:

### 3.4 OCI Resource Manager

API: `resourcemanager.ListStacks`,
`resourcemanager.GetStack`. Required OCI policy:
`inspect orm-stacks in compartment`.

Detection axes:

| Axis                | Source                                                  | Recommendation kind             |
|---------------------|---------------------------------------------------------|----------------------------------|
| Logging configured  | Stack has `lifecycle_state = ACTIVE` AND a Logging service log group attached to the compartment that includes RM as a source | `resmgr-logging-enable`          |
| Stack drift status  | Stack `latest_job.detect_drift_state` is `OK` or `NOT_CHECKED`               | informational only               |
| Stack last job state | `latest_job.lifecycle_state` is `SUCCEEDED` or `FAILED` | informational only               |

The Logging-axis detection mirrors the OCI Streaming logging
proxy pattern from v0.89.101 — the OCI Logging service
absorbs the role of "trace primitive" since OCI doesn't
expose a direct OTel integration for RM.

Coverage caveat: a Stack with Logging configured at the
compartment level but NOT specifically routed for RM
sources will still get `has_log_axis = true` from Squadron.
Operators who want stricter detection should ensure
log groups have explicit RM source mappings; slice 3 may
add per-source-mapping inspection.

The slice 1 honest-framing pattern carries through: Squadron
detects what's tractable now (compartment-level Logging
presence) and surfaces a slice 3 deferral for tighter
correlation.

## 4. Storage schema

NO migration. The existing `orchestration_instance` table
from slice 1 (v0.89.95) carries the right shape — provider
+ surface + has_trace_axis + has_log_axis + last_seen_at.
Slice 2 just adds rows with `provider = "oci"` and
`surface = "resmgr"`.

Schema stays at v15 (or whatever the current value is).

## 5. Scanner contract

The OCI scanner from v0.89.96 chunk 4 has
`ScanOrchestrations(ctx, scope) ([]OrchestrationInstanceSnapshot, error)`
returning `nil, nil` (the slice 1 contract for OCI). Slice 2
replaces the nil with a real implementation:

```go
func (s *Scanner) ScanOrchestrations(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
    return s.ScanResourceManagerStacks(ctx, scope)
}

func (s *Scanner) ScanResourceManagerStacks(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
    // List Stacks via:
    //   GET https://resourcemanager.{region}.oci.oraclecloud.com/20180917/stacks?compartmentId=...
    //   Paginated via opc-next-page header
    //
    // For each Stack:
    //   1. ResourceName = stack.display_name
    //   2. ResourceARN = stack.id (OCID)
    //   3. SourceType = "stack" — wait, no, that's event source. Use WorkflowType = "Stack"
    //   4. Detect Logging axis via:
    //      - GET /20200531/logs?compartmentId=... (OCI Logging service)
    //      - Match log_group entries where configuration.source.service = "resourcemanager"
    //      - If any match, HasLogAxis = true
    //      - HasTraceAxis = false (RM doesn't have an OTel-native trace primitive;
    //        slice 3 may add when OCI ships APM integration for RM)
    //   5. Detail map records latest job state + drift state
}
```

The OCI signing pattern from v0.89.91 (Functions scanner)
and v0.89.101 (Streaming scanner) carries through.

## 6. API surface

The slice 1 per-provider scan + inventory endpoints already
handle OCI orchestrations field correctly — slice 1 shipped
empty `orchestrations: []` for OCI per the contract. Slice 2
just populates it.

The Discovery summary endpoint's `orchestration_count` for
OCI starts surfacing non-zero values.

The trace coverage endpoint's `orchestration_pct` for OCI
starts surfacing non-zero values when OCI traceindex
correlates spans.

## 7. UI

The DiscoveryOCI page's Orchestration sub-tab is **hidden
conditional** in slice 1 (per v0.89.97 chunk 4) — hidden
when `orchestrations[]` is empty. Slice 2 doesn't change
the rendering logic; the tab simply starts rendering when
slice 2 populates the field.

No new UI work. The slice 1 tab structure handles slice 2
data correctly.

The Workload Health panel from v0.89.132 doesn't include
orchestration in slice 1 (it's serverless-only). No change.

## 8. Recommendation kinds

1 new kind:

```
resmgr-logging-enable
```

The `resmgr-` prefix is NEW. Webhook routing extends:

```
resmgr-       → oci
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

Reasoning template:

> "This OCI Resource Manager Stack does NOT have OCI
> Logging configured to capture its Job events. Without
> Logging, failed apply/destroy operations leave no audit
> trail beyond the OCI console. This Terraform PR
> configures an OCI Logging log group with Resource
> Manager as a source for the Stack's compartment.
>
> Operators who use a non-Logging observability destination
> (custom processor pulling from OCI Streaming, etc.)
> should decline; the verdict learning loop records."

Terraform pattern:

```hcl
resource "oci_logging_log_group" "resmgr_<name>" {
  compartment_id = var.compartment_ocid
  display_name   = "resmgr-stack-logs"
}

resource "oci_logging_log" "resmgr_<name>" {
  log_group_id = oci_logging_log_group.resmgr_<name>.id
  display_name = "stack-events"
  log_type     = "SERVICE"
  
  configuration {
    source {
      category = "all"
      resource = oci_resourcemanager_stack.<name>.id
      service  = "resourcemanager"
      source_type = "OCISERVICE"
    }
    compartment_id = var.compartment_ocid
  }
}
```

## 9. Slice 2 contract

**In:**

1. OCI `ScanResourceManagerStacks` implementation
   populating the existing `orchestration_instance` table.
2. OCI Logging axis detection via compartment-level log
   group + source mapping inspection.
3. Existing `ScanOrchestrations` dispatcher returns real
   stacks instead of `nil, nil`.
4. 1 new recommendation kind `resmgr-logging-enable`.
5. Webhook routing extends with `resmgr-` → oci.
6. iacpicker per-cloud emitter for the OCI Logging
   Terraform pattern.
7. Operator runbook section (extend
   docs/orchestration-tier-operator-guide.md).
8. README index entry updated to remove the qualification.
9. Acceptance tests covering Stack detection, Logging
   axis, webhook routing, OCI inventory population,
   cold-start parity.

**Out:**

- OCI Process Automation (slice 3).
- Per-execution Job log content inspection.
- Stack state file inspection.
- Job-level cold-start / latency / error rate analysis
  (the substrate's three diagnostics target serverless).
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: OCI Resource Manager scanner + Logging axis
  detection.** ~700-900 lines. **v0.89.135.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update.** ~700-900 lines. **v0.89.136.**

Total: 2 release tags. Smallest arc shipped in a while —
slice 2 is purely additive on top of the slice 1 scaffolding
(no new tier, no new substrate, no new UI shape).

## 11. Acceptance tests

1. **OCI ScanResourceManagerStacks returns Stacks** —
   paginated list response is walked.
2. **Stack with Logging compartment + RM source mapping →
   has_log_axis = true**.
3. **Stack with Logging compartment but NO RM source
   mapping → has_log_axis = false** (operator-strict
   detection per §3.4 caveat).
4. **Stack with NO Logging at compartment level →
   has_log_axis = false**.
5. **ScanOrchestrations dispatcher delegates correctly to
   ScanResourceManagerStacks**.
6. **Webhook routes resmgr-logging-enable to oci**.
7. **Discovery summary OCI orchestration_count surfaces
   non-zero when Stacks exist**.
8. **DiscoveryOCI Orchestration sub-tab renders when
   orchestrations[] populated** (regression — the slice 1
   conditional render path).
9. **Cold-start parity preserved** — all 4 providers
   cold-start prompts byte-identical to v0.89.133 when no
   resmgr rows trigger recommendations.

## 12. Threat model

**Wider OCI policy.** Slice 2 adds `inspect orm-stacks in
compartment` to the OCI scanner policy template. Read-only.
Operators get the in-product policy upgrade path (#590).

**Logging source-mapping detection latency.** The OCI
Logging API call adds ~1 query per Stack to detect source
mappings. For a fleet of 1000 Stacks across a compartment,
that's 1000 queries against the OCI Logging API. The
substrate's existing 10 TPS rate limit absorbs this —
~100 seconds added to the scan duration.

**Cost surface.** OCI Logging queries are free for metric
+ source mapping reads. No new operator-facing cost
decisions.

**False positives on operators using non-Logging
destinations.** A team that pulls RM events from OCI
Streaming directly (custom processor) would see false
positives on `resmgr-logging-enable`. Exclusion table +
verdict learning loop handle. Runbook documents.

**No span content logging.** Slice 2 reads Stack metadata
+ Logging service configuration only. No Job log content
flows through Squadron's audit chain. PII surface stays
at zero.

## 13. Slice 3 candidates

- OCI Process Automation (BPMN workflow orchestration).
- Per-execution Job log content inspection (PII
  concerns; deferred).
- Per-source-mapping inspection (tighter than
  compartment-level).
- Stack drift detection correlation with proposer
  recommendations.
- Job-level cold-start / latency / error rate analysis
  (substrate diagnostics extended to RM Jobs).
- Cross-stack dependency mapping (which Stacks reference
  resources from other Stacks).

---

**Strategic frame:**

Slice 2 removes the only remaining asterisk on Squadron's
universal claim. Every tier — compute / database /
kubernetes / serverless / orchestration / event sources —
is now cleanly 4-cloud.

> "Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
> AND EVENT SOURCES for observability gaps, verifies
> telemetry is actually flowing, validates the spans
> Squadron receives are healthy, MEASURES cold-start
> latency + sampling rate + error rate across all four
> clouds against expected baselines, AND drafts the IaC
> PRs that close the gaps it finds."

**Four clouds × six tiers × five verbs.** No qualifications,
no asterisks.

The honest framing in this design doc preserves the
operator-meaningful "OCI's primitives are shape-different
but RM is the operator-meaningful surface" narrative. The
runbook will lay out:

- Why Resource Manager and not Process Automation
- What "Logging axis" detection means for RM specifically
- The decline path for operators using non-Logging
  destinations
- Slice 3 deferrals (Process Automation, per-Job content,
  per-source-mapping)

The Tuesday LinkedIn drumbeat narrative gains a clean
universal claim that's easier to compress into a one-line
elevator pitch: "We cover every observable layer of every
cloud, no asterisks."
