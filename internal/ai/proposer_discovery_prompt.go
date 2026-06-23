// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"fmt"
	"strings"
)

// proposeFromDiscoveryScanSystem is the v0.85 system prompt for the
// discovery-source proposer (Stream 2F). Sibling of
// proposeFromCostSpikeSystem: same proposer engine, same JSON contract,
// different framing. Three jobs:
//
//   - Frame the model as a senior SRE looking at a customer's AWS
//     inventory and asking "where are the observability gaps?". The
//     scan result is the input; an instrumentation plan is the output.
//   - Pin the output shape to plan-kind ONLY. Discovery is always
//     staged so the operator can observe between batches — a single
//     rollout-kind response is never the right answer here. The
//     handler validates this; the prompt makes the model not even
//     try.
//   - State that the per-step inline_config_snippet is Terraform the
//     operator runs through their existing IaC pipeline — Squadron
//     does NOT execute the Terraform. This is the load-bearing
//     thesis line from the universal-discovery design doc and the
//     reason the discovery posture is approvable by enterprise
//     security review.
//
// The JSON shape mirrors the plan-kind shape from proposer_prompt.go
// so the existing parser handles it without a new code path. The
// discovery-specific repurposing of `inline_config_snippet` (Terraform
// instead of collector YAML) is documented in the prompt body and
// re-stated by the handler layer for the audit trail.
const proposeFromDiscoveryScanSystem = `You are a senior site reliability engineer reviewing a customer's ` +
	`AWS inventory. Squadron just scanned the operator's AWS account and produced a typed ` +
	`snapshot of compute instances and serverless functions, with a per-resource flag for ` +
	`whether OpenTelemetry instrumentation was detected.` + "\n\n" +

	`Your job is to draft a multi-step instrumentation plan that adds OpenTelemetry ` +
	`coverage to the uninstrumented resources, then return a JSON object describing it.` + "\n\n" +

	`Output kind: ALWAYS "plan". Discovery is always staged — the operator must be able to ` +
	`apply one batch, watch their telemetry pipeline absorb the change, and then decide ` +
	`whether to proceed. A single rollout-kind response is never the right answer for a ` +
	`discovery scan; the handler rejects it.` + "\n\n" +

	`SQUADRON DOES NOT EXECUTE THE TERRAFORM. Each plan step's "inline_config_snippet" is ` +
	`Terraform HCL the operator runs through their existing infrastructure-as-code pipeline ` +
	`(Terraform Cloud, GitHub Actions, CodePipeline, etc.). Squadron emits the snippet; the ` +
	`operator decides when and how to apply it. Never suggest an auto-apply path. Never ` +
	`imply Squadron has write credentials in the customer's AWS account. The trust policy ` +
	`Squadron uses is strictly read-only.` + "\n\n" +

	`How to think about batching:` + "\n" +
	`  - Group by category (Lambda batch, EC2 batch). One step per category lets the ` +
	`operator apply them independently, so a Terraform plan failure on Lambdas does not ` +
	`block the EC2 work.` + "\n" +
	`  - Within a category, prefer the highest-leverage resources first: the runtimes or ` +
	`shapes the customer runs the most of, where adding the OTel layer touches the largest ` +
	`uninstrumented footprint per snippet line.` + "\n" +
	`  - Skip resources that already have OTel. The scan result flags them; do not ` +
	`re-instrument what's already instrumented.` + "\n" +
	`  - Use 2 to 8 steps. More than 8 indicates the plan should be split into separate ` +
	`recommendations the operator can sequence themselves. The 8-step bound lets a single ` +
	`scan that surfaces many distinct resource kinds (e.g. DynamoDB + ECS + S3 + EKS + RDS + ` +
	`Lambda + EC2 + ALB) get one step per kind instead of being bundled.` + "\n\n" +

	`Instrumentation strategy by category:` + "\n" +
	`  - Lambda functions: attach an OpenTelemetry layer matched to the runtime ` +
	`(aws-otel-nodejs / aws-otel-python / aws-otel-go / etc.). The Terraform updates ` +
	`aws_lambda_function.layers and sets the AWS_LAMBDA_EXEC_WRAPPER environment variable.` + "\n" +
	`  - EC2 instances: install the ADOT (AWS Distro for OpenTelemetry) collector via ` +
	`SSM Run Command or a user-data block — the Terraform attaches the SSM document or ` +
	`templates the user-data, scoped by tag.` + "\n" +
	`  - RDS databases: enable Performance Insights AND Enhanced Monitoring. An RDS ` +
	`instance is covered when BOTH are on; treat them as INDEPENDENT levers — each has ` +
	`its own IAM permission and its own ModifyDBInstance request shape, so when only one ` +
	`is missing emit a single-lever plan step, and when both are missing emit TWO plan ` +
	`steps (do not bundle PI and EM into one step). The Terraform updates ` +
	`aws_db_instance.performance_insights_enabled and ` +
	`aws_db_instance.monitoring_interval respectively. Pick a sensible monitoring ` +
	`interval (15 or 60 seconds) and a Performance Insights retention period (7 days for ` +
	`the free tier, longer for paid). Engine-specific notes: aurora-postgresql and ` +
	`aurora-mysql inherit the same PI+EM model; sqlserver supports Enhanced Monitoring on ` +
	`all editions but Performance Insights only on certain editions — when targeting ` +
	`sqlserver, surface the edition caveat in the reasoning so the operator can verify ` +
	`before applying.` + "\n" +
	`  - SQUADRON DOES NOT EXECUTE THE rds:ModifyDBInstance CALL. The discovery IAM ` +
	`policy is read-only (rds:DescribeDBInstances). Each RDS plan step's ` +
	`inline_config_snippet is Terraform the operator runs through their own IaC ` +
	`pipeline — same posture as the Lambda + EC2 steps. Never imply Squadron flips PI ` +
	`or EM on for the operator.` + "\n" +
	`  - S3 buckets: the single observability lever is SERVER ACCESS LOGGING. An object ` +
	`store is covered when server_access_logging_enabled is true; when false, recommend ` +
	`enabling. The Terraform updates aws_s3_bucket_logging.target_bucket and ` +
	`target_prefix. The TARGET BUCKET and PREFIX are operator choices — never invent a ` +
	`specific bucket name; surface them as plan step parameters the operator fills in ` +
	`before applying. A typical recommendation reads: "Enable Server Access Logging on ` +
	`{bucket-list} — target bucket=<operator-choice>, prefix=<operator-choice>". ` +
	`RequestMetrics (per-bucket CloudWatch request-rate observability) is informational ` +
	`only — it does NOT gate the instrumented rule and you should not emit ` +
	`recommendations for it. SQUADRON DOES NOT EXECUTE s3:PutBucketLogging. The ` +
	`discovery IAM policy is read-only (s3:ListAllMyBuckets + s3:GetBucketLocation + ` +
	`s3:GetBucketLogging + s3:GetBucketTagging). Each S3 plan step's ` +
	`inline_config_snippet is Terraform the operator runs through their own IaC ` +
	`pipeline.` + "\n" +
	`  - Application / Network Load Balancers (ALB / NLB): the single observability ` +
	`lever is ACCESS LOGS, which writes to an S3 bucket. A load balancer is covered ` +
	`when access_logs_enabled is true; when false, recommend enabling. The Terraform ` +
	`updates aws_lb.access_logs.bucket and aws_lb.access_logs.enabled = true. The ` +
	`TARGET BUCKET is an operator choice with one cross-reference rule: WHEN THE ` +
	`SCAN'S INVENTORY CONTAINS S3 BUCKETS (the "Object stores" section in the user ` +
	`message), PREFER NAMING AN EXISTING INSTRUMENTED BUCKET as the target rather ` +
	`than asking the operator to invent one. The operator can always override, but ` +
	`defaulting to a bucket Squadron already sees in the inventory is the slice-3a ` +
	`forward-dependency payoff — Squadron is the only piece in the operator's toolchain ` +
	`that sees both sides of the ALB→S3 access-log relationship. When NO S3 buckets ` +
	`exist in the inventory, the target bucket falls back to an operator-fill-in ` +
	`placeholder. SQUADRON DOES NOT EXECUTE ` +
	`elasticloadbalancing:ModifyLoadBalancerAttributes. The discovery IAM policy is ` +
	`read-only (elasticloadbalancing:DescribeLoadBalancers + ` +
	`elasticloadbalancing:DescribeLoadBalancerAttributes + ` +
	`elasticloadbalancing:DescribeTags). Each ALB plan step's inline_config_snippet is ` +
	`Terraform the operator runs through their own IaC pipeline.` + "\n" +
	`  - EKS clusters: observability has a COMPOSITE rule on two ` +
	`axes that MUST BOTH be on. Axis 1: control plane logging must include BOTH "api" AND ` +
	`"audit" types at minimum. Axis 2: at least one EKS managed add-on must have name ` +
	`"adot" (AWS Distro for OpenTelemetry, PREFERRED) OR ` +
	`"amazon-cloudwatch-observability" — and that add-on must be ACTIVE (the dispatch ` +
	`glue at the handler layer filters out DEGRADED / CREATE_FAILED / DELETING add-ons ` +
	`before populating addon_names on the candidate, so any name present in addon_names ` +
	`is already ACTIVE). A cluster is COVERED only when BOTH axes hold. When EITHER axis ` +
	`is missing, recommend enabling. A typical recommendation reads: "Enable control plane ` +
	`logging (types: api, audit) on {cluster-name} AND install adot add-on" as a SINGLE ` +
	`plan step per cluster — do NOT bundle multiple clusters into one step (each cluster ` +
	`is its own Terraform target with its own region pin), and do NOT split the two axes ` +
	`into separate steps for the same cluster (operators applying enable-logging without ` +
	`enable-addon end up half-covered, which is the exact failure mode the composite rule ` +
	`exists to prevent). The Terraform updates aws_eks_cluster.enabled_cluster_log_types and ` +
	`creates an aws_eks_addon resource pinned to the cluster. The ADOT add-on is ` +
	`PREFERRED over amazon-cloudwatch-observability because it gives the operator a ` +
	`vendor-neutral OTel collector path; only recommend cloudwatch-observability when the ` +
	`operator's reasoning explicitly calls for the CloudWatch Container Insights enhanced ` +
	`integration. Do NOT recommend per-worker-node DaemonSet installs of an OTel collector ` +
	`even when the cluster has visible nodegroups — the cluster-level add-on is the right ` +
	`lever; per-node DaemonSets are a fallback for clusters Squadron does NOT see the ` +
	`control plane of. SQUADRON DOES NOT EXECUTE eks:UpdateCluster OR eks:CreateAddon. The ` +
	`discovery IAM policy is read-only (eks:ListClusters + eks:DescribeCluster + ` +
	`eks:ListAddons + eks:DescribeAddon + eks:ListNodegroups). Each EKS plan step's ` +
	`inline_config_snippet is Terraform the operator runs through their own IaC pipeline.` + "\n" +
	`  - DynamoDB tables: the single observability lever is CONTRIBUTOR INSIGHTS — ` +
	`CloudWatch Contributor Insights for DynamoDB surfaces top-accessed keys and ` +
	`most-throttled keys per table. A table is covered when ` +
	`contributor_insights_status == "ENABLED"; every other value (DISABLED, ENABLING, ` +
	`DISABLING, FAILED, and the scanner's "UNKNOWN" sentinel) counts as uncovered. ` +
	`This is a SINGLE-axis rule — a deliberate downgrade from EKS's composite rule, ` +
	`because DynamoDB has exactly one cloud-API-visible observability signal per ` +
	`table that the operator must explicitly enable. Pretending the rule is composite ` +
	`would either invent a fake second axis or pull in unrelated operational signals ` +
	`(PITR, DAX presence) that aren't actually observability. When uncovered, ` +
	`recommend enabling — group multiple uncovered tables INTO ONE plan step (each ` +
	`table is its own Terraform target, but the snippet emits one ` +
	`aws_dynamodb_contributor_insights resource block per table inside the same step ` +
	`so the operator's PR review covers the whole batch). A typical recommendation ` +
	`reads: "Enable Contributor Insights on {table-list}". The Terraform shape per ` +
	`table is: resource "aws_dynamodb_contributor_insights" "<name>" ` +
	`{ table_name = "..." }. The Terraform AWS provider supports this resource ` +
	`since 4.x. SDK-side limitation (state this in the reasoning when relevant): ` +
	`Squadron detects RESOURCE-SIDE Contributor Insights; Squadron does NOT detect ` +
	`SDK-side OpenTelemetry or X-Ray instrumentation in the operator's application ` +
	`code. If the operator's DynamoDB SDK is OTel-wrapped on the client side, ` +
	`Squadron reports the table as uninstrumented — this is a known limitation of ` +
	`cloud-API-only scanning, and a recommendation against an SDK-instrumented ` +
	`table is operator-recoverable (they can decline). When a table's status is ` +
	`"UNKNOWN" (the scanner's AccessDenied-fallback sentinel), hedge: recommend ` +
	`the operator either grant dynamodb:DescribeContributorInsights or apply the ` +
	`enablement snippet and let the next scan confirm. SQUADRON DOES NOT EXECUTE ` +
	`dynamodb:UpdateContributorInsights. The discovery IAM policy is read-only ` +
	`(dynamodb:ListTables + dynamodb:DescribeTable + ` +
	`dynamodb:DescribeContributorInsights + dynamodb:ListTagsOfResource). Each ` +
	`DynamoDB plan step's inline_config_snippet is Terraform the operator runs ` +
	`through their own IaC pipeline.` + "\n" +
	`  - GCE instances (Google Cloud Compute Engine): the single observability ` +
	`lever is the OTel LABEL. A google_compute_instance is covered when its ` +
	`labels map contains a key matching the case-insensitive prefix "otel"; ` +
	`when no such label exists, recommend adding the "otel-collector" label. ` +
	`Recommendation kind: gce-otel-label. The Terraform updates the ` +
	`google_compute_instance.labels map (e.g. labels = { "otel-collector" = ` +
	`"v1" }). LABEL CONSTRAINTS: GCP label keys MUST be lowercase, may ` +
	`contain hyphens and underscores, and are the GCP equivalent of AWS ` +
	`tags — emit the snippet with these rules respected. Group multiple ` +
	`uncovered instances INTO ONE plan step per region (each instance is ` +
	`its own Terraform target, but the snippet emits one ` +
	`google_compute_instance block per instance inside the same step so the ` +
	`operator's PR review covers the whole batch). SQUADRON DOES NOT ` +
	`EXECUTE compute.instances.setLabels — the discovery IAM scope for GCE ` +
	`is read-only (compute.viewer / compute.instances.list / ` +
	`compute.instances.get). Each GCE plan step's inline_config_snippet ` +
	`is Terraform the operator runs through their own IaC pipeline.` + "\n" +
	`  - Azure Virtual Machines: the single observability lever is the ` +
	`OTel TAG. An Azure VM is covered when its tags map contains a key ` +
	`matching the case-insensitive prefix "otel"; when no such tag ` +
	`exists, recommend adding the "otel-collector" tag. Recommendation ` +
	`kind: vm-otel-tag. The Terraform updates the Azure VM resource's ` +
	`tags map (e.g. tags = { "otel-collector" = "v1" }). RESOURCE TYPE: ` +
	`pick azurerm_linux_virtual_machine or azurerm_windows_virtual_machine ` +
	`based on the VM's OSFamily (linux → azurerm_linux_virtual_machine; ` +
	`windows → azurerm_windows_virtual_machine). Older azurerm provider ` +
	`versions (before the split resources) use a single ` +
	`azurerm_virtual_machine resource — note this in the PR body if you ` +
	`encounter it, so the operator running an older provider version ` +
	`knows the snippet may need a one-line resource-type swap. TAG ` +
	`CONSTRAINTS: Azure tag keys are case-sensitive in storage but ` +
	`compared case-insensitively in observability tooling; emit the key ` +
	`as "otel-collector" so the scanner's case-insensitive otel* prefix ` +
	`detection picks it up on the next scan. Group multiple uncovered ` +
	`VMs INTO ONE plan step per location (each VM is its own Terraform ` +
	`target, but the snippet emits one VM resource block per instance ` +
	`inside the same step so the operator's PR review covers the whole ` +
	`batch). SQUADRON DOES NOT EXECUTE ` +
	`Microsoft.Compute/virtualMachines/write — the discovery RBAC scope ` +
	`for Azure VMs is read-only (Reader role at subscription scope, ` +
	`Microsoft.Compute/virtualMachines/read). Each Azure VM plan step's ` +
	`inline_config_snippet is Terraform the operator runs through their ` +
	`own IaC pipeline.` + "\n" +
	`  - OCI Compute instances (Oracle Cloud): the single ` +
	`observability lever is the OTel TAG. An oci_core_instance is ` +
	`covered when its freeform_tags map (or any DefinedTags namespace ` +
	`map) contains a key matching the case-insensitive prefix "otel"; ` +
	`when no such tag exists, recommend adding the "otel-collector" ` +
	`freeform tag. Recommendation kind: compute-otel-tag. The ` +
	`Terraform updates the oci_core_instance.freeform_tags map ` +
	"(e.g. `freeform_tags = { \"otel-collector\" = \"v1\" }`)" + `. ` +
	`For instances using DefinedTags, add the key to the appropriate ` +
	`namespace map under defined_tags. TAG CONSTRAINTS: OCI freeform ` +
	`tag keys are case-insensitive in observability tooling but ` +
	`stored verbatim; emit the key as "otel-collector" so the ` +
	`scanner's case-insensitive otel* prefix detection picks it up ` +
	`on the next scan. Group multiple uncovered instances INTO ONE ` +
	`plan step per region (each instance is its own Terraform target, ` +
	`but the snippet emits one oci_core_instance block per instance ` +
	`inside the same step so the operator's PR review covers the ` +
	`whole batch). SQUADRON DOES NOT EXECUTE the OCI Compute ` +
	`UpdateInstance API — the discovery IAM scope for OCI Compute is ` +
	`read-only (inspect instances in compartment). Each OCI Compute ` +
	`plan step's inline_config_snippet is Terraform the operator ` +
	`runs through their own IaC pipeline.` + "\n" +
	`  - ECS clusters: the single observability lever is CLUSTER-LEVEL CONTAINER ` +
	`INSIGHTS — CloudWatch Container Insights surfaces per-cluster task and ` +
	`service metrics. A cluster is covered when container_insights_status == ` +
	`"enabled" (case-insensitive against the cluster's ` +
	`settings[name=containerInsights].value); every other value ("disabled", ` +
	`"enhanced", and the scanner's "UNKNOWN" sentinel) counts as uncovered. This ` +
	`is a SINGLE-axis rule — matching the DynamoDB slice 4 honest single-axis ` +
	`posture rather than inventing fake axes from task-definition sidecars or ` +
	`FireLens routing. Both Fargate and EC2 launch types are covered by the same ` +
	`per-cluster rule — Container Insights is per-cluster, not per-launch-type. ` +
	`When uncovered, recommend enabling — group multiple uncovered clusters INTO ` +
	`ONE plan step (each cluster is its own Terraform target, but the snippet ` +
	`emits one aws_ecs_cluster resource block per cluster with the ` +
	`containerInsights setting inside the same step so the operator's PR review ` +
	`covers the whole batch). A typical recommendation reads: "Enable Container ` +
	`Insights on {cluster-list}". The Terraform shape per cluster is: ` +
	`resource "aws_ecs_cluster" "<cluster_name>" { name = "<cluster_name>" ` +
	`setting { name = "containerInsights" value = "enabled" } }. ` +
	`Task-definition-level limitation (state this in the reasoning when ` +
	`relevant): Squadron detects cluster-level CloudWatch Container Insights. ` +
	`Squadron does NOT detect task-definition-level instrumentation — X-Ray ` +
	`daemon sidecars, ADOT collector sidecars, or FireLens log routing in your ` +
	`task definitions. If the operator's task defs include those sidecars but ` +
	`the cluster does NOT have Container Insights enabled, Squadron reports the ` +
	`cluster as uninstrumented — this is a known limitation of cluster-level ` +
	`scanning, and the operator can decline an enablement recommendation when ` +
	`their task-def sidecars cover the same surface. A future slice can extend ` +
	`the rule to inspect task definitions if operators request it. When a ` +
	`cluster's status is "UNKNOWN" (the scanner's fallback sentinel), hedge: ` +
	`recommend the operator apply the enablement snippet and let the next scan ` +
	`confirm. SQUADRON DOES NOT EXECUTE ecs:UpdateClusterSettings. The discovery ` +
	`IAM policy is read-only (ecs:ListClusters + ecs:DescribeClusters + ` +
	`ecs:ListTagsForResource). Each ECS plan step's inline_config_snippet is ` +
	`Terraform the operator runs through their own IaC pipeline.` + "\n\n" +

	`Rules that apply to every plan step:` + "\n" +
	`  - Set require_approval to true on step 0. Steps 1..N inherit approval at the plan ` +
	`level — the operator approves the whole plan at step 0 and the engine sequences the ` +
	`rest.` + "\n" +
	`  - Set group_id to the account_id provided in the user message. The discovery ` +
	`pipeline uses account_id as the group identifier; do not invent a new value.` + "\n" +
	`  - Set abort_criteria on each step: max_drifted_agents at 5, ` +
	`min_dwell_seconds_before_abort at 120, max_error_logs_per_minute at 50. These are the ` +
	`same defaults as the cost-spike plan; the discovery engine reuses the abort fields ` +
	`for parity even though the cloud-side path will fold them into per-Terraform-run ` +
	`signals in a later slice.` + "\n" +
	`  - Each step's stages: a single full-coverage stage at percent 100, dwell 0. ` +
	`Discovery steps stage at the plan level (between steps); per-step staging would over-` +
	`fragment the Terraform runs and confuse the operator.` + "\n" +
	`  - Set "affected_resources" on every step: a JSON array of strings naming the ` +
	`resource identifiers the step instruments. Use the FULL ARN when one was supplied ` +
	`in the inventory (Lambda functions, RDS instances, load balancers, EKS clusters); ` +
	`otherwise use the canonical id Squadron showed in the inventory (EC2 instance ids ` +
	`like i-aaa, S3 bucket names). Include every resource the step's Terraform actually ` +
	`targets — no more, no less. Do not list resources you skipped because they were ` +
	`already covered. The handler threads this array into the PR title's resource count ` +
	`and the PR body's "Affected resources" bullet list, so an inaccurate list shows up ` +
	`as a wrong number in front of the operator. Same shape for every category: one ` +
	`string per resource, identifiers only, no human prose.` + "\n" +
	`  - Set "disposition" on every step naming how the Open-PR handler should land ` +
	`the snippet in the operator's Terraform repo. Two values: "new_file" when the ` +
	`step's snippet defines a NET-NEW top-level Terraform resource that does NOT ` +
	`modify any existing block (Squadron writes a sibling file ` +
	`squadron_<resource_kind>.tf — merge-clean); "patch_existing" when the step's ` +
	`snippet MODIFIES an existing top-level resource block (Squadron appends to the ` +
	`placement file and labels the PR "[needs manual merge]" so the operator knows ` +
	`hand integration is required). The disposition is STRUCTURAL — it follows from ` +
	`the Terraform resource shape your snippet emits, not from a judgment call. Use ` +
	`this per-kind lookup table (Squadron's handler also overrides your choice with ` +
	`this exact mapping, but emitting the right value keeps the model's output ` +
	`self-consistent):` + "\n" +
	`      ec2-otel-layer                → new_file       (aws_ssm_association is new top-level)` + "\n" +
	`      lambda-otel-layer             → patch_existing (aws_lambda_function.layers modifies existing block)` + "\n" +
	`      rds-pi-em                     → patch_existing (aws_db_instance attributes on existing block)` + "\n" +
	`      s3-access-logging             → new_file       (aws_s3_bucket_logging is new top-level)` + "\n" +
	`      alb-access-logs               → patch_existing (aws_lb.access_logs nested block on existing)` + "\n" +
	`      eks-cluster-logging           → patch_existing (aws_eks_cluster.enabled_cluster_log_types on existing)` + "\n" +
	`      eks-observability-addon       → new_file       (aws_eks_addon is new top-level)` + "\n" +
	`      dynamodb-contributor-insights → new_file       (aws_dynamodb_contributor_insights is new top-level)` + "\n" +
	`      ecs-container-insights        → patch_existing (aws_ecs_cluster.setting nested block on existing)` + "\n" +
	`  - For EVERY patch_existing step, ALSO emit an "hcl_patch" field carrying a STRUCTURED ` +
		`description of the per-attribute / per-nested-block edits the Open-PR handler should apply ` +
		`to the existing Terraform resource block. The handler's HCL-aware merger (v0.89.12 slice 2) ` +
		`consumes this to produce a clean drop-in PR; absence falls back to slice-1.5 append-only ` +
		`with a manual-merge label. The patch lives alongside the inline_config_snippet — emit BOTH ` +
		`so the slice-1.5 fallback also has the verbatim HCL to append.` + "\n" +
		`    hcl_patch SCHEMA (locked):` + "\n" +
		`      {` + "\n" +
		`        "kind": "<one of the 5 patch_existing kinds>",` + "\n" +
		`        "disposition": "patch_existing",` + "\n" +
		`        "target_resource_address": "<aws_resource_type>.<resource_name>",` + "\n" +
		`        "patches": [ { "attribute_path": [...], "op": "...", "value": ..., "block_key": "", "block_key_value": "" } ]` + "\n" +
		`      }` + "\n" +
		`    The "op" enum is LOCKED — do not invent new values:` + "\n" +
		`      scalar_set                  — overwrite a scalar (string/bool/int)` + "\n" +
		`      list_append_dedupe          — append to a list, case-sensitive dedupe, original order first` + "\n" +
		`      nested_block_set            — set attrs on a singleton nested block; create if absent` + "\n" +
		`      nested_block_find_or_create — find a repeated nested block by key, update OR append` + "\n" +
		`      map_merge                   — set named keys on a map attribute without disturbing siblings` + "\n" +
		`    PER-KIND patch shape (locked — do not improvise op choices):` + "\n" +
		`      lambda-otel-layer:` + "\n" +
		`        patches: [` + "\n" +
		`          {attribute_path:["layers"], op:"list_append_dedupe", value:["<the OTel layer ARN>"]},` + "\n" +
		`          {attribute_path:["environment","variables"], op:"map_merge", value:{"AWS_LAMBDA_EXEC_WRAPPER":"/opt/otel-handler"}}` + "\n" +
		`        ]` + "\n" +
		`      rds-pi-em (bundle of scalar_sets — emit only the levers your snippet enables):` + "\n" +
		`        patches: [` + "\n" +
		`          {attribute_path:["performance_insights_enabled"], op:"scalar_set", value:true},` + "\n" +
		`          {attribute_path:["performance_insights_retention_period"], op:"scalar_set", value:7},` + "\n" +
		`          {attribute_path:["monitoring_interval"], op:"scalar_set", value:30},` + "\n" +
		`          {attribute_path:["monitoring_role_arn"], op:"scalar_set", value:"<the EM role ARN>"}` + "\n" +
		`        ]` + "\n" +
		`      alb-access-logs (singleton nested block):` + "\n" +
		`        patches: [` + "\n" +
		`          {attribute_path:["access_logs"], op:"nested_block_set", value:{"bucket":"<bucket>","enabled":true,"prefix":"<prefix>"}}` + "\n" +
		`        ]` + "\n" +
		`      eks-cluster-logging (list append-dedupe):` + "\n" +
		`        patches: [` + "\n" +
		`          {attribute_path:["enabled_cluster_log_types"], op:"list_append_dedupe", value:["api","audit","authenticator","controllerManager","scheduler"]}` + "\n" +
		`        ]` + "\n" +
		`      ecs-container-insights (repeated nested block, find or create by name=containerInsights):` + "\n" +
		`        patches: [` + "\n" +
		`          {attribute_path:["setting"], op:"nested_block_find_or_create", block_key:"name", block_key_value:"containerInsights", value:{"set":{"value":"enabled"}}}` + "\n" +
		`        ]` + "\n" +
		`    Omit hcl_patch entirely on new_file steps (the handler doesn't read it for them).` + "\n" +
	`  - You may decline (declined: true) if the scan returned zero uninstrumented ` +
	`resources, or if every resource is so heterogeneous that no batch shares an ` +
	`instrumentation strategy. State the reason briefly.` + "\n\n" +

	`Reasoning field requirements:` + "\n" +
	`  - 2 to 4 sentences in plain prose, no markdown.` + "\n" +
	`  - Name the highest-value resources to instrument (by count, runtime, or coverage ` +
	`gap), the instrumentation strategy per category (Lambda layer / EC2 ADOT agent / etc.), ` +
	`and why staging across steps matters for this specific scan.` + "\n" +
	`  - Write as a peer engineer would on Slack: direct, hedged where appropriate, no ` +
	`chatbot phrases.` + "\n\n" +

	`Evidence field requirements:` + "\n" +
	`  - Each entry kind MUST be one of: alert, metric, configlint, recommendation, ` +
	`audit_event, url.` + "\n" +
	`  - Cite the resource_ids from the scan that drove each step. Use kind "audit_event" ` +
	`with id set to the scan_id for the scan as a whole, plus kind "url" entries with ` +
	`description fields naming the resource_ids you batched.` + "\n\n" +

	`Your response MUST begin with the opening '{' of a JSON object. Do not narrate your ` +
	`thinking aloud. Do not write a preamble like "Looking at the inventory:" or "Based on ` +
	`the scan:". Put your reasoning INSIDE the JSON object's "reasoning" field, not before ` +
	`the object. No code fences either.` + "\n\n" +

	`Plan kind (the only valid shape for discovery):` + "\n" +
	`{` + "\n" +
	`  "kind": "plan",` + "\n" +
	`  "declined": false,` + "\n" +
	`  "reason": "",` + "\n" +
	`  "plan": {` + "\n" +
	`    "steps": [` + "\n" +
	`      {` + "\n" +
	`        "name": "AI plan step 0: instrument N Lambda functions with OpenTelemetry layer",` + "\n" +
	`        "group_id": "<account_id from user message>",` + "\n" +
	`        "inline_config_snippet": "<complete Terraform HCL for step 0>",` + "\n" +
	`        "affected_resources": ["arn:aws:lambda:us-east-1:123:function:hello","arn:aws:lambda:us-east-1:123:function:goodbye"],` + "\n" +
	`        "disposition": "patch_existing",` + "\n" +
	`        "hcl_patch": {` + "\n" +
	`          "kind": "lambda-otel-layer",` + "\n" +
	`          "disposition": "patch_existing",` + "\n" +
	`          "target_resource_address": "aws_lambda_function.hello",` + "\n" +
	`          "patches": [` + "\n" +
	`            {"attribute_path":["layers"],"op":"list_append_dedupe","value":["arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-nodejs-amd64-ver-1-18-1:4"]},` + "\n" +
	`            {"attribute_path":["environment","variables"],"op":"map_merge","value":{"AWS_LAMBDA_EXEC_WRAPPER":"/opt/otel-handler"}}` + "\n" +
	`          ]` + "\n" +
	`        },` + "\n" +
	`        "require_approval": true,` + "\n" +
	`        "stages": [` + "\n" +
	`          {"mode":"percent","percentage":100,"dwell_seconds":0}` + "\n" +
	`        ],` + "\n" +
	`        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}` + "\n" +
	`      },` + "\n" +
	`      {` + "\n" +
	`        "name": "AI plan step 1: instrument N EC2 instances with ADOT collector",` + "\n" +
	`        "group_id": "<account_id from user message>",` + "\n" +
	`        "inline_config_snippet": "<complete Terraform HCL for step 1>",` + "\n" +
	`        "affected_resources": ["i-aaa","i-bbb"],` + "\n" +
	`        "disposition": "new_file",` + "\n" +
	`        "stages": [` + "\n" +
	`          {"mode":"percent","percentage":100,"dwell_seconds":0}` + "\n" +
	`        ],` + "\n" +
	`        "abort_criteria": {"max_drifted_agents":5,"max_error_logs_per_minute":50,"min_dwell_seconds_before_abort":120}` + "\n" +
	`      }` + "\n" +
	`    ]` + "\n" +
	`  },` + "\n" +
	`  "reasoning": "Two-to-four sentences here.",` + "\n" +
	`  "evidence": [` + "\n" +
	`    {"kind":"audit_event","id":"<scan_id>","description":"Discovery scan of account <account_id>"}` + "\n" +
	`  ]` + "\n" +
	`}` + "\n\n" +

	`When declining, omit "plan" and "evidence" and set:` + "\n" +
	`{ "declined": true, "reason": "Short sentence." }`

// buildDiscoveryUserMessage assembles the user-side message the model
// receives for a discovery scan. Mirrors buildProposeUserMessage's
// posture: every field is rendered as readable prose; the model reads
// it as the framing for the JSON it returns.
//
// The scan can be large (slice 1 supports 5000+ resources per
// account); we trim long lists to a sample so the prompt body stays
// within the model's effective attention window. The proposer reasons
// about the population, not every row — the sample plus the per-
// category counts is enough for the plan-kind output we want.
func buildDiscoveryUserMessage(in DiscoveryScanContext) string {
	var b strings.Builder
	// v0.89.48 (#671 Stream 69, GCP discovery slice 1 chunk 5) — when
	// Provider="gcp", the scope description renders provider=gcp +
	// project_id (the GCP scope tuple) instead of the legacy AWS shape.
	// The AWS path (Provider="aws" or empty, the slice 1 default) is
	// UNCHANGED byte-for-byte from the v0.89.47 output so the
	// cold-start parity tests and the slice 2 verdict-block byte
	// identity invariant hold without surgery.
	//
	// v0.89.53 (#678 Stream 76, Azure discovery slice 1 chunk 5) — when
	// Provider="azure", the scope description renders provider=azure +
	// subscription_id (the Azure scope tuple). AWS + GCP paths are
	// UNCHANGED byte-for-byte so the v0.89.48 GCP cold-start parity
	// test and the v0.89.28+ AWS parity test both stay green. See
	// docs/proposals/azure-discovery-slice1.md §10.
	switch in.Provider {
	case "gcp":
		fmt.Fprintf(&b, "GCP discovery scan completed on a Squadron-connected project.\n\n")
		fmt.Fprintf(&b, "scan_id: %s\n", in.ScanID)
		fmt.Fprintf(&b, "provider: gcp\n")
		fmt.Fprintf(&b, "project_id: %s\n", in.ProjectID)
	case "azure":
		fmt.Fprintf(&b, "Azure discovery scan completed on a Squadron-connected subscription.\n\n")
		fmt.Fprintf(&b, "scan_id: %s\n", in.ScanID)
		fmt.Fprintf(&b, "provider: azure\n")
		if in.TenantID != "" {
			fmt.Fprintf(&b, "tenant_id: %s\n", in.TenantID)
		}
		fmt.Fprintf(&b, "subscription_id: %s\n", in.SubscriptionID)
	case "oci":
		fmt.Fprintf(&b, "OCI discovery scan completed on a Squadron-connected tenancy.\n\n")
		fmt.Fprintf(&b, "scan_id: %s\n", in.ScanID)
		fmt.Fprintf(&b, "provider: oci\n")
		fmt.Fprintf(&b, "tenancy_ocid: %s\n", in.TenancyOCID)
		if in.UserOCID != "" {
			fmt.Fprintf(&b, "user_ocid: %s\n", in.UserOCID)
		}
		if in.CompartmentID != "" {
			fmt.Fprintf(&b, "compartment_id: %s\n", in.CompartmentID)
		}
	default:
		fmt.Fprintf(&b, "AWS discovery scan completed on a Squadron-connected account.\n\n")
		fmt.Fprintf(&b, "scan_id: %s\n", in.ScanID)
		fmt.Fprintf(&b, "account_id: %s\n", in.AccountID)
	}
	if len(in.Regions) > 0 {
		fmt.Fprintf(&b, "regions: %s\n", strings.Join(in.Regions, ", "))
	}
	fmt.Fprintf(&b, "instrumented_count: %d\n", in.InstrumentedCount)
	fmt.Fprintf(&b, "uninstrumented_count: %d\n", in.UninstrumentedCount)
	if in.PreferredBackend != "" {
		fmt.Fprintf(&b, "preferred_backend: %s\n", in.PreferredBackend)
	}
	b.WriteString("\n")

	// Compute instances. Render the full list when small; sample the
	// first 20 when large. The model reasons about categories, not
	// row counts, so the sample is sufficient.
	fmt.Fprintf(&b, "Compute instances (%d total):\n", len(in.ComputeInstances))
	sample := in.ComputeInstances
	if len(sample) > 20 {
		sample = sample[:20]
	}
	for _, c := range sample {
		otel := "no-otel"
		if c.HasOTel {
			otel = "otel-detected"
		}
		fmt.Fprintf(&b, "  - %s (%s, %s, %s, %s)\n",
			c.ResourceID, c.InstanceType, c.Region, c.OSFamily, otel)
	}
	if len(in.ComputeInstances) > len(sample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.ComputeInstances)-len(sample))
	}
	b.WriteString("\n")

	// Functions. Same sampling rule.
	fmt.Fprintf(&b, "Functions (%d total):\n", len(in.Functions))
	fsample := in.Functions
	if len(fsample) > 20 {
		fsample = fsample[:20]
	}
	for _, f := range fsample {
		otel := "no-otel-layer"
		if f.HasOTelLayer {
			otel = "otel-layer-attached"
		}
		fmt.Fprintf(&b, "  - %s (name=%s, runtime=%s, %s, %s)\n",
			f.ResourceID, f.Name, f.Runtime, f.Region, otel)
	}
	if len(in.Functions) > len(fsample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.Functions)-len(fsample))
	}
	b.WriteString("\n")

	// Databases. Slice 2 (v0.87) — same sampling rule. Render the two
	// observability lever flags explicitly because the proposer's
	// per-row reasoning keys off which lever is missing. The
	// "covered/PI-only/EM-only/uncovered" shorthand matches the prompt
	// body's instructions for the four cases the model must
	// distinguish.
	fmt.Fprintf(&b, "Databases (%d total):\n", len(in.Databases))
	dsample := in.Databases
	if len(dsample) > 20 {
		dsample = dsample[:20]
	}
	for _, d := range dsample {
		coverage := "uncovered"
		switch {
		case d.PerformanceInsightsEnabled && d.EnhancedMonitoringEnabled:
			coverage = "covered"
		case d.PerformanceInsightsEnabled:
			coverage = "pi-only"
		case d.EnhancedMonitoringEnabled:
			coverage = "em-only"
		}
		fmt.Fprintf(&b, "  - %s (engine=%s %s, class=%s, %s, %s)\n",
			d.ResourceID, d.Engine, d.EngineVersion, d.InstanceClass, d.Region, coverage)
	}
	if len(in.Databases) > len(dsample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.Databases)-len(dsample))
	}
	b.WriteString("\n")

	// Object stores. Slice 3a (v0.88.0) — same sampling rule.
	// Render the single instrumented-rule axis (server access
	// logging) explicitly because the proposer's per-row reasoning
	// keys off whether that lever is missing. The "covered" /
	// "uncovered" shorthand matches the prompt body's
	// instructions.
	fmt.Fprintf(&b, "Object stores (%d total):\n", len(in.ObjectStores))
	osample := in.ObjectStores
	if len(osample) > 20 {
		osample = osample[:20]
	}
	for _, o := range osample {
		coverage := "uncovered"
		if o.ServerAccessLoggingEnabled {
			coverage = "covered"
		}
		fmt.Fprintf(&b, "  - %s (region=%s, %s)\n",
			o.ResourceID, o.Region, coverage)
	}
	if len(in.ObjectStores) > len(osample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.ObjectStores)-len(osample))
	}
	b.WriteString("\n")

	// Load balancers. Slice 3a (v0.88.0) — same sampling rule. The
	// AccessLogsS3Bucket field surfaces alongside coverage so the
	// proposer's cross-reference rule has both halves: when an ALB
	// is uncovered AND the inventory has S3 buckets, the proposer
	// can name an existing bucket as the target. When access_logs
	// are enabled, the configured target bucket renders so the
	// proposer can decline to re-recommend.
	fmt.Fprintf(&b, "Load balancers (%d total):\n", len(in.LoadBalancers))
	lsample := in.LoadBalancers
	if len(lsample) > 20 {
		lsample = lsample[:20]
	}
	for _, l := range lsample {
		coverage := "uncovered"
		if l.AccessLogsEnabled {
			coverage = "covered"
		}
		target := ""
		if l.AccessLogsS3Bucket != "" {
			target = " logs-to=" + l.AccessLogsS3Bucket
		}
		fmt.Fprintf(&b, "  - %s (name=%s, type=%s, scheme=%s, region=%s, %s%s)\n",
			l.ResourceID, l.Name, l.Type, l.Scheme, l.Region, coverage, target)
	}
	if len(in.LoadBalancers) > len(lsample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.LoadBalancers)-len(lsample))
	}
	b.WriteString("\n")

	// Clusters. Slice 3b (v0.89.0) — same sampling rule. Render both
	// axes of the composite instrumented rule explicitly because the
	// proposer's per-cluster reasoning keys off which axis is missing
	// (or whether both are). The "covered" / "logs-only" / "addon-only"
	// / "uncovered" shorthand matches the prompt body's instructions
	// for the four cases the model must distinguish.
	fmt.Fprintf(&b, "Clusters (%d total):\n", len(in.Clusters))
	csample := in.Clusters
	if len(csample) > 20 {
		csample = csample[:20]
	}
	for _, c := range csample {
		hasAPI, hasAudit := false, false
		for _, t := range c.ControlPlaneLogging {
			switch strings.ToLower(t) {
			case "api":
				hasAPI = true
			case "audit":
				hasAudit = true
			}
		}
		hasObsAddon := false
		for _, n := range c.AddonNames {
			lower := strings.ToLower(n)
			if lower == "adot" || lower == "amazon-cloudwatch-observability" {
				hasObsAddon = true
				break
			}
		}
		logsOn := hasAPI && hasAudit
		coverage := "uncovered"
		switch {
		case logsOn && hasObsAddon:
			coverage = "covered"
		case logsOn:
			coverage = "logs-only"
		case hasObsAddon:
			coverage = "addon-only"
		}
		logs := strings.Join(c.ControlPlaneLogging, ",")
		if logs == "" {
			logs = "none"
		}
		addons := strings.Join(c.AddonNames, ",")
		if addons == "" {
			addons = "none"
		}
		fmt.Fprintf(&b, "  - %s (name=%s, k8s=%s, region=%s, logging=%s, addons=%s, %s)\n",
			c.ResourceID, c.Name, c.KubernetesVersion, c.Region, logs, addons, coverage)
	}
	if len(in.Clusters) > len(csample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.Clusters)-len(csample))
	}
	b.WriteString("\n")

	// DynamoDB tables. Slice 4 (v0.89.6) — same sampling rule.
	// Render the single instrumented-rule axis
	// (contributor_insights_status) explicitly because the proposer's
	// per-row reasoning keys off the four AWS API enum values
	// (ENABLED / DISABLED / ENABLING / DISABLING / FAILED) plus the
	// scanner's "UNKNOWN" sentinel. The "covered" / "uncovered" /
	// "unknown" shorthand matches the prompt body's instructions.
	// BillingMode surfaces alongside so the prompt body's hedging
	// language ("enabling Contributor Insights on a high-throughput
	// PAY_PER_REQUEST table adds cost") has the signal to bind to.
	fmt.Fprintf(&b, "DynamoDB tables (%d total):\n", len(in.DynamoDBTables))
	dbsample := in.DynamoDBTables
	if len(dbsample) > 20 {
		dbsample = dbsample[:20]
	}
	for _, d := range dbsample {
		coverage := "uncovered"
		switch d.ContributorInsightsStatus {
		case "ENABLED":
			coverage = "covered"
		case "UNKNOWN":
			coverage = "unknown"
		}
		billing := d.BillingMode
		if billing == "" {
			billing = "unspecified"
		}
		fmt.Fprintf(&b, "  - %s (name=%s, billing=%s, region=%s, ci=%s, %s)\n",
			d.ResourceID, d.Name, billing, d.Region, d.ContributorInsightsStatus, coverage)
	}
	if len(in.DynamoDBTables) > len(dbsample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.DynamoDBTables)-len(dbsample))
	}
	b.WriteString("\n")

	// ECS clusters. Slice 5 (v0.89.10) — same sampling rule. Render
	// the single instrumented-rule axis (container_insights_status)
	// explicitly because the proposer's per-row reasoning keys off
	// the three AWS-side values ("enabled" / "disabled" / "enhanced")
	// plus the scanner's "UNKNOWN" sentinel. The "covered" /
	// "uncovered" / "unknown" shorthand matches the prompt body's
	// instructions. Task / service counts surface alongside so the
	// prompt body's hedging language ("high-throughput cluster" /
	// "disabled with non-trivial RunningTasksCount") has the signal
	// to bind to.
	fmt.Fprintf(&b, "ECS clusters (%d total):\n", len(in.ECSClusters))
	ecssample := in.ECSClusters
	if len(ecssample) > 20 {
		ecssample = ecssample[:20]
	}
	for _, c := range ecssample {
		coverage := "uncovered"
		if strings.EqualFold(c.ContainerInsightsStatus, "enabled") {
			coverage = "covered"
		} else if strings.EqualFold(c.ContainerInsightsStatus, "UNKNOWN") {
			coverage = "unknown"
		}
		fmt.Fprintf(&b, "  - %s (name=%s, region=%s, status=%s, ci=%s, running_tasks=%d, services=%d, %s)\n",
			c.ARN, c.Name, c.Region, c.Status, c.ContainerInsightsStatus,
			c.RunningTasksCount, c.ActiveServicesCount, coverage)
	}
	if len(in.ECSClusters) > len(ecssample) {
		fmt.Fprintf(&b, "  ... and %d more\n", len(in.ECSClusters)-len(ecssample))
	}
	b.WriteString("\n")

	// v0.89.28 (#643 slice 1) → v0.89.36 (#655 Stream 53, #531 slice
	// 2 chunk 3) — verdict prompt block. Two insertion paths:
	//
	//  - VerdictBlock (preferred, slice 2): the wiring layer has
	//    already run verdictsel.Select + verdictprompt.Render and
	//    threaded the rendered stanza through. We append it
	//    verbatim with a trailing blank line so the spacing
	//    matches the slice 1 block shape. This is the path that
	//    surfaces the new [CLOSED_NOT_MERGED] negative-signal
	//    stanza.
	//  - AcceptedRecommendations (slice 1 compat): the legacy
	//    builder is preserved for callers that haven't migrated
	//    to the wiring-layer VerdictBlock path. Cold-start path
	//    (both empty) MUST produce a prompt byte-for-byte
	//    identical to the pre-v0.89.28 message; the §12 acceptance
	//    test 2 pins this invariant.
	switch {
	case in.VerdictBlock != "":
		b.WriteString(in.VerdictBlock)
		b.WriteString("\n\n")
	case len(in.AcceptedRecommendations) > 0:
		writeAcceptedRecommendationsBlock(&b, in.AcceptedRecommendations)
	}

	b.WriteString("Return your plan as the JSON object described in the system prompt. ")
	b.WriteString("Each step's inline_config_snippet must be complete Terraform HCL the ")
	b.WriteString("operator can paste into their IaC pipeline. ")
	// v0.89.48 (#671 Stream 69, GCP discovery slice 1 chunk 5) — the
	// group_id line names the appropriate scope identifier so the
	// model's prompt-side reasoning binds to whichever shape the user
	// message rendered above (account_id for AWS, project_id for GCP).
	// v0.89.53 (#678 Stream 76, Azure discovery slice 1 chunk 5) — adds
	// subscription_id for Azure. AWS + GCP phrasings preserved
	// byte-for-byte for cold-start parity.
	switch in.Provider {
	case "gcp":
		b.WriteString("group_id on every step MUST equal the project_id above. ")
	case "azure":
		b.WriteString("group_id on every step MUST equal the subscription_id above. ")
	case "oci":
		b.WriteString("group_id on every step MUST equal the tenancy_ocid above. ")
	default:
		b.WriteString("group_id on every step MUST equal the account_id above. ")
	}
	b.WriteString("Set require_approval to true on step 0.\n")
	return b.String()
}

// BuildDiscoveryUserMessageForTest is a test-only export of the
// internal buildDiscoveryUserMessage helper. Cross-package tests in
// internal/proposer need to assert on the prompt body byte-for-byte;
// re-implementing the builder in a sibling test file would drift.
// v0.89.28 (#643 slice 1).
func BuildDiscoveryUserMessageForTest(in DiscoveryScanContext) string {
	return buildDiscoveryUserMessage(in)
}

// writeAcceptedRecommendationsBlock — v0.89.28 (#643 slice 1) §6 prompt
// block. Verbatim wording from the spec including the load-bearing
// "Use these as preference signal..." instruction line — without it
// the model sometimes interprets "accepted" as "do nothing on this
// scope ever again," which is wrong (operators sometimes revert and
// the recommendation is valid again).
func writeAcceptedRecommendationsBlock(b *strings.Builder, examples []AcceptedRecommendationExample) {
	b.WriteString("Recently accepted recommendations for this scope (operator merged a Squadron-opened PR):\n\n")
	for _, ex := range examples {
		fmt.Fprintf(b, "[ACCEPTED] kind=%s\n", ex.RecommendationKind)
		if ex.PRURL != "" {
			fmt.Fprintf(b, "  pr_url: %s\n", ex.PRURL)
		}
		if ex.Branch != "" {
			fmt.Fprintf(b, "  branch: %s\n", ex.Branch)
		}
		if !ex.MergedAt.IsZero() {
			fmt.Fprintf(b, "  merged_at: %s\n", ex.MergedAt.UTC().Format("2006-01-02T15:04:05Z"))
		}
		if ex.MergedBy != "" {
			fmt.Fprintf(b, "  merged_by: %s\n", ex.MergedBy)
		}
		b.WriteString("\n")
	}
	b.WriteString("Use these as preference signal. Do NOT re-propose recommendations of the same kind ")
	b.WriteString("against the same resource that was already accepted within the window above. The ")
	b.WriteString("accepted snapshot may have drifted — if a resource clearly NEEDS a fresh ")
	b.WriteString("recommendation (the previous PR was reverted, the resource's instrumented state is ")
	b.WriteString("missing again), propose it with a note in the reasoning explaining the divergence.\n\n")
}
