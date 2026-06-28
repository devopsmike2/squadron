# Arc: Environment → Terraform (adopt un-managed resources via import blocks)

Status: proposed (2026-06-28)
Owner: autonomous
Approach: import blocks (operator-confirmed). Builds on the discovery
scan inventory + the IaC-PR machinery + the repo-context/dedup work
from the context-aware-merge-ready-prs arc.

## Problem

Squadron discovers live cloud resources. Some are already managed by the
operator's Terraform (instrument those — the merge-ready arc). Many are
NOT in any Terraform at all. For those, the operator wants Squadron to
"see the environment and produce the matching Terraform" so the resource
comes under IaC management.

## Approach: import blocks (not from-scratch codegen)

Squadron's scan captures a *summary* (resource IDs, type, region,
instrumentation flags) — not the full resource configuration. Apply-able
Terraform needs the full config. Rather than reinvent every provider's
attribute mapping (terraformer-style, brittle, 4 clouds, perpetually
incomplete), Squadron emits Terraform `import {}` blocks — which it CAN
produce accurately from the scan (it has the resource type + cloud ID) —
and the operator runs:

    terraform plan -generate-config-out=generated.tf

The provider reads the REAL resource and writes accurate config. Squadron
does the part it can do precisely (identify + address + import-ID); the
provider does the part it does best (serialise the live config). The
generated config can then flow through slice 3's `terraform validate`
gate from the previous arc.

Requires Terraform 1.5+ (import blocks + -generate-config-out).

## The crux: per-type import-ID format

Each Terraform resource type has a specific import-ID format. Squadron's
scan `resource_id`/`name` must map to that format. Slice 1 covers AWS:

| category       | tf type               | import ID source            | verified |
|----------------|-----------------------|-----------------------------|----------|
| compute        | aws_instance          | resource_id (i-...)         | yes (live scan) |
| object_store   | aws_s3_bucket         | resource_id (bucket name)   | convention |
| function       | aws_lambda_function   | name (function name)        | convention |
| database       | aws_db_instance        | resource_id (db identifier)| convention; ARN-guard |
| load_balancer  | aws_lb                | resource_id (ARN)           | convention |

Unsupported categories/types are SKIPPED with a reason (never emit a
guessed import ID that would fail).

## Slices

### Slice 1 — Deterministic AWS import-block generation + preview
`internal/iac/tfimport`: a pure package mapping scanned resources
(Category, Provider, ResourceID, Name, Region) → ImportBlock
{TFType, TFAddress, ImportID, Region} with per-type AWS mappers; Render
emits valid HCL `import {}` blocks + a header explaining the
-generate-config-out workflow + the required provider/version. A
preview endpoint returns the rendered HCL for a scan_result (no PR yet).
Tests: per-type mapping + render + skip-unsupported + address sanitising.

### Slice 2 — Dedup vs existing TF + PR delivery
Use the repo-context summariser to skip resources already managed (by
resource address heuristic) and open a PR adding `squadron_imports.tf`
via the existing IaC-PR client. Idempotent + comment-exclusion aware.

### Slice 3 — GCP / Azure / OCI coverage + UI + docs
Add per-cloud mappers (google_compute_instance: project/zone/name;
azurerm_*: full resource ID; oci_*: OCID). UI button on the inventory
("Generate Terraform to adopt"). Operator docs.

## Non-goals
- From-scratch full-attribute HCL codegen (the provider does this via
  -generate-config-out).
- Running terraform inside Squadron (the operator runs the one command,
  same posture as the validate gate).
