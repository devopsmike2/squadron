# Environment → Terraform (adopt un-managed resources)

Squadron can turn what it discovers in your cloud into Terraform
`import {}` blocks, so resources that exist but aren't yet in any
Terraform come under IaC management. Squadron emits the import *targets*;
your Terraform provider writes the actual configuration.

## How it works

1. Run a discovery scan (as usual).
2. Ask Squadron for import blocks:
   - **Preview:** `POST /api/v1/discovery/aws/connections/{id}/terraform-import`
     with `{"scan_result": <scan response>}` → returns the rendered
     `.tf` plus a `skipped` list and `block_count`.
   - **Open a PR:** `POST /api/v1/iac/github/connections/{id}/terraform-import-pr`
     with `{"scan_result": <scan response>}` → opens a PR adding (or
     appending to) `squadron_imports.tf` on the connected repo.
3. In the repo, let the provider generate the config and adopt the
   resources (requires Terraform >= 1.5):

   ```
   terraform plan -generate-config-out=generated.tf
   terraform apply   # adopts the resources; no changes if config matches
   ```

   Review `generated.tf` before applying.

## Why import blocks (not generated HCL)

Squadron's scan captures a summary (IDs, type, region), not every
attribute. Rather than reproduce each provider's full schema (brittle,
perpetually incomplete), Squadron produces the part it can do
precisely — the resource type + a sane address + the provider-specific
import ID — and lets the provider serialize the live config via
`-generate-config-out`. The generated config can then flow through the
`terraform validate` merge-ready gate.

## Idempotency + safety

- The PR flow dedups by **cloud import ID** against any existing
  `squadron_imports.tf`: re-running never re-emits a block for a
  resource already listed, and returns `already_imported` when there's
  nothing new.
- Squadron only emits an import block when it knows the resource type's
  exact import-ID format. Anything else is **skipped with a reason** —
  it never guesses an import ID that would fail at `terraform import`.

## Coverage

| cloud | status | notes |
|-------|--------|-------|
| AWS   | supported | aws_instance, aws_s3_bucket, aws_lambda_function, aws_db_instance, aws_lb |
| GCP / Azure / OCI | not yet | The scan currently stores operator-readable names (e.g. a VM name) rather than the canonical import ID those providers require (full ARM resource ID, OCID, or `projects/…/zones/…/instances/…`). Supporting them needs a scanner enrichment to also capture the canonical ID per resource. |

AWS is supported because its scanned `resource_id` already equals the
Terraform import ID (e.g. `i-0abc…`). The other clouds need the scanner
change above before their import blocks can be generated safely.
