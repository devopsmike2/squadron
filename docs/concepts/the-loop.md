# How the loop works

Squadron's whole product is one loop run continuously: **discover** what's
running, **codify** the fix for anything under-instrumented or over-spending,
**roll it out** safely, and **audit** every step. Each hop is designed to be
reviewable — nothing reaches production without a `terraform validate` gate, a
human merge, and a staged rollout with auto-abort.

## The loop at a glance

```mermaid
flowchart LR
    A[Discover<br/>cloud resources] --> B{OTel gap or<br/>cost problem?}
    B -- no --> A
    B -- yes --> C[AI drafts fix<br/>Terraform or config]
    C --> D{terraform validate<br/>/ Squadron lint}
    D -- fails --> C
    D -- passes --> E[Merge-ready PR<br/>or staged config]
    E --> F[Human review<br/>+ merge]
    F --> G[Staged rollout]
    G --> H{Abort criteria<br/>during dwell?}
    H -- fired --> I[Auto-rollback]
    H -- clean --> J[Promote next stage]
    J --> K[Succeeded]
    I --> L[Audit event]
    K --> L
    L --> A
```

## Stage by stage

### 1. Discover

Two discovery surfaces run in parallel. **Cloud discovery** scanners inventory
AWS, GCP, Azure, and OCI — compute, databases, serverless, and more — and flag
resources with missing or broken OpenTelemetry. **Fleet discovery** tracks the
collectors themselves: managed agents arrive over OpAMP, and telemetry-only
agents are registered passively when their OTLP shows up without a control
channel. See [Discovery](../discovery.md).

### 2. Detect the gap

For an un-instrumented cloud resource, the gap is "no OTel here." For an
existing pipeline, the gap can be a **cost problem** — the cost-spike detector
compares the current $/month projection against a rolling baseline every minute
and opens an attributed event when spend jumps. Either way, Squadron now has a
concrete thing to fix.

### 3. Codify — AI drafts the fix

With `ANTHROPIC_API_KEY` set, Squadron drafts the remediation: a Terraform
fragment for an instrumentation gap, or a collector-config change for a cost
regression. The deterministic Terraform snippets are correctness-audited; the
free-form reasoning is explained in plain English so you can review it before
merging.

### 4. Gate — validate before it reaches you

Terraform fixes are **HCL-aware merged** into your existing config and gated on
`terraform validate`; config changes run through Squadron's lint engine, diff
preview, and rollout preview. A fix that doesn't validate loops back for a
redraft rather than landing in your inbox broken.

### 5. Merge — human in the loop

A validated Terraform fix becomes a **merge-ready pull request** against your
IaC repo. You review the diff and merge (or decline — a decline teaches future
scans). Squadron never merges for you; every change is a PR gated by your review
plus CI.

### 6. Roll out — staged, with auto-abort

Config changes ship through a [staged rollout](../rollouts.md): percent- or
label-based stages, a per-stage dwell, and abort criteria on drift and error
rate. During each dwell the engine ticks every few seconds; if a criterion
fires, the rollout flips to `aborted` and Squadron pushes the previous config
back automatically.

### 7. Audit — record everything

Every state change — config stored, PR opened, stage applied, rollout aborted,
approval granted — lands in the [append-only audit log](../audit-log.md). That
record is what closes the loop: it's the durable evidence of what happened, and
it feeds the next scan.

## One gap, end to end

Here is a single instrumentation gap being closed, as a sequence:

```mermaid
sequenceDiagram
    autonumber
    participant S as Squadron
    participant C as Cloud (AWS/GCP/Azure/OCI)
    participant AI as AI drafter
    participant R as IaC repo (GitHub)
    participant H as Operator
    participant F as OTel fleet

    S->>C: Scan inventory
    C-->>S: Resource with no OTel
    S->>AI: Draft Terraform fix
    AI-->>S: HCL fragment
    S->>S: terraform validate (gate)
    S->>R: Open merge-ready PR
    H->>R: Review + merge
    S->>F: Staged rollout (stage 1 canary)
    F-->>S: Drift + error rate within criteria
    S->>F: Promote remaining stages
    S->>S: Record audit events (created, stage_applied, succeeded)
```

If the canary had drifted past the abort threshold, step 8 would instead be an
auto-abort and rollback — and the audit log would carry `rollout.aborted`
followed by `rollout.rolled_back` with the reason. Either way the loop returns
to discovery, now aware of the change it just made.
