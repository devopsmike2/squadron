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
	`Terraform the operator runs through their own IaC pipeline.` + "\n" +
	`  - GCP Cloud SQL instances (database tier slice 2): the single ` +
	`observability lever is QUERY INSIGHTS. A google_sql_database_instance is ` +
	`covered when settings.insightsConfig.queryInsightsEnabled == true; when ` +
	`false (or insightsConfig is absent), recommend enabling. Recommendation ` +
	`kind: cloudsql-pi-enable. The Terraform updates ` +
	`google_sql_database_instance.settings[0].insights_config[0].query_insights_enabled = true. ` +
	`Group multiple uncovered instances INTO ONE plan step per region (each ` +
	`instance is its own Terraform target, but the snippet emits one ` +
	`google_sql_database_instance block per instance inside the same step so ` +
	`the operator's PR review covers the whole batch). SQUADRON DOES NOT ` +
	`EXECUTE the Cloud SQL UpdateInstance API — the discovery IAM scope for ` +
	`Cloud SQL is read-only (roles/cloudsql.viewer). Each Cloud SQL plan ` +
	`step's inline_config_snippet is Terraform the operator runs through ` +
	`their own IaC pipeline.` + "\n" +
	`  - Azure SQL databases (database tier slice 2): the single observability ` +
	`lever is the SQLInsights DIAGNOSTIC SETTING. An azurerm_mssql_database is ` +
	`covered when at least one Diagnostic Setting routes the SQLInsights log ` +
	`category to a destination (Log Analytics workspace, Storage, or Event ` +
	`Hub); when no SQLInsights routing exists, recommend adding one. ` +
	`Recommendation kind: azsql-diag-enable. The Terraform creates an ` +
	`azurerm_monitor_diagnostic_setting resource with an enabled_log block of ` +
	`category = "SQLInsights" targeting the database. Group multiple uncovered ` +
	`databases INTO ONE plan step per subscription (each database is its own ` +
	`Terraform target, but the snippet emits one azurerm_monitor_diagnostic_setting ` +
	`block per database inside the same step so the operator's PR review ` +
	`covers the whole batch). SQUADRON DOES NOT EXECUTE the Azure Monitor ` +
	`Diagnostic Settings write API — the discovery RBAC scope for Azure SQL ` +
	`is read-only (Reader role at subscription scope, ` +
	`Microsoft.Sql/servers/databases/read + microsoft.insights/diagnosticSettings/read). ` +
	`Each Azure SQL plan step's inline_config_snippet is Terraform the ` +
	`operator runs through their own IaC pipeline.` + "\n" +
	`  - OCI Database instances (database tier slice 2): the single ` +
	`observability lever is OPERATIONS INSIGHTS / DATABASE MANAGEMENT ` +
	`enrollment. An OCI DB System or Autonomous Database is covered when ` +
	`databaseManagementConfig.databaseManagementStatus == "ENABLED"; when ` +
	`the status is any other value (DISABLED, NEEDS_ATTENTION, FAILED_ENABLING, ` +
	`absent), recommend enabling. Recommendation kind: ocidb-perfhub-enable. ` +
	`The Terraform updates oci_database_db_systems_management for DB Systems ` +
	`or the equivalent management block on oci_database_autonomous_database. ` +
	`Group multiple uncovered instances INTO ONE plan step per region (each ` +
	`instance is its own Terraform target, but the snippet emits one ` +
	`management resource block per instance inside the same step so the ` +
	`operator's PR review covers the whole batch). SQUADRON DOES NOT EXECUTE ` +
	`the OCI Database Management enable API — the discovery IAM scope for ` +
	`OCI Databases is read-only (read database-family in tenancy). Each OCI ` +
	`Database plan step's inline_config_snippet is Terraform the operator ` +
	`runs through their own IaC pipeline.` + "\n" +
	`  - GCP GKE clusters (Kubernetes tier slice 2): the single ` +
	`observability lever is MANAGED PROMETHEUS. A google_container_cluster ` +
	`is covered when monitoringConfig.managedPrometheusConfig.enabled == ` +
	`true; when false (or managedPrometheusConfig is absent), recommend ` +
	`enabling. Recommendation kind: gke-mp-enable. The Terraform updates ` +
	`google_container_cluster.monitoring_config[0].managed_prometheus[0].enabled = true. ` +
	`Group multiple uncovered clusters INTO ONE plan step per region (each ` +
	`cluster is its own Terraform target, but the snippet emits one ` +
	`google_container_cluster block per cluster inside the same step so ` +
	`the operator's PR review covers the whole batch). SQUADRON DOES NOT ` +
	`EXECUTE container.projects.locations.clusters.update — the discovery ` +
	`IAM scope for GKE is read-only (roles/container.viewer). Each GKE ` +
	`plan step's inline_config_snippet is Terraform the operator runs ` +
	`through their own IaC pipeline.` + "\n" +
	`  - Azure AKS clusters (Kubernetes tier slice 2): the single ` +
	`observability lever is AZURE MONITOR (Container Insights, Managed ` +
	`Prometheus, or the legacy omsagent addon — a three-way disjunction ` +
	`mirroring EKS's "ADOT OR CloudWatch observability" pattern so ` +
	`operators on either the legacy or newer addon get credit). An ` +
	`azurerm_kubernetes_cluster is covered when ANY of ` +
	`addonProfiles.omsagent.enabled, azureMonitorProfile.metrics.enabled, ` +
	`or azureMonitorProfile.containerInsights.enabled is true; when all ` +
	`three observability flags are false, recommend enabling. ` +
	`Recommendation kind: aks-monitor-enable. The Terraform adds the ` +
	`azurerm_kubernetes_cluster.monitor_metrics block (for Managed ` +
	`Prometheus) or the oms_agent block (for legacy Container Insights ` +
	`on operators still on the older addon). Group multiple uncovered ` +
	`clusters INTO ONE plan step per subscription (each cluster is its ` +
	`own Terraform target, but the snippet emits one ` +
	`azurerm_kubernetes_cluster block per cluster inside the same step so ` +
	`the operator's PR review covers the whole batch). SQUADRON DOES NOT ` +
	`EXECUTE the Microsoft.ContainerService managedClusters PUT API — the ` +
	`discovery RBAC scope for AKS is read-only (Reader role at ` +
	`subscription scope). Each AKS plan step's inline_config_snippet is ` +
	`Terraform the operator runs through their own IaC pipeline.` + "\n" +
	`  - OCI OKE clusters (Kubernetes tier slice 2): the single ` +
	`observability lever is the OPERATIONS INSIGHTS FREEFORM TAG. An ` +
	`oci_containerengine_cluster is covered when its freeform_tags map ` +
	`contains a key matching the case-insensitive name ` +
	`"operations-insights-enabled" with value "true"; when the tag is ` +
	`missing or any other value, recommend adding it. Recommendation ` +
	`kind: oke-ops-insights-enable. The Terraform updates the ` +
	`oci_containerengine_cluster.freeform_tags map ` +
	"(e.g. `freeform_tags = { \"operations-insights-enabled\" = \"true\" }`)" + `. ` +
	`Slice 2 uses the tag convention because OCI does not expose a ` +
	`single "cluster enrolled in Operations Insights" boolean as cleanly ` +
	`as GCP/Azure; slice 3 may move to a direct Operations Insights API ` +
	`call. Group multiple uncovered clusters INTO ONE plan step per ` +
	`region (each cluster is its own Terraform target, but the snippet ` +
	`emits one oci_containerengine_cluster block per cluster inside the ` +
	`same step so the operator's PR review covers the whole batch). ` +
	`SQUADRON DOES NOT EXECUTE the OCI Container Engine UpdateCluster ` +
	`API — the discovery IAM scope for OKE is read-only (read ` +
	`cluster-family in tenancy). Each OKE plan step's ` +
	`inline_config_snippet is Terraform the operator runs through their ` +
	`own IaC pipeline.` + "\n\n" +

	traceEmissionKindsPromptSection +

	spanQualityKindsPromptSection +

	spanQualityTraceparentKindsPromptSection +

	samplingRateKindsPromptSection +

	errorRateKindsPromptSection +

	serverlessTierKindsPromptSection +

	orchestrationTierKindsPromptSection +

	eventSourceTierKindsPromptSection +

	eventSourceTierPropagationKindsPromptSection +

	eventSourceTierSlice3SNSKindsPromptSection +

	eventSourceTierSlice4SQSKindsPromptSection +

	eventSourceTierSlice5CloudTasksKindsPromptSection +

	eventSourceTierSlice6EventGridKindsPromptSection +

	eventSourceTierSlice7ONSKindsPromptSection +

	eventSourceTierSlice8EventHubsKindsPromptSection +

	eventSourceTierSlice9QueuesKindsPromptSection +

	eventSourceTierSlice10PubSubLiteKindsPromptSection +

	dlqConfigSlice1KindsPromptSection +

	consumerLagSlice2KindsPromptSection +
	consumerLagSubstrateSlice5KindsPromptSection +
	costCorrelationSlice6KindsPromptSection +
	poisonRateSlice3KindsPromptSection +
	poisonRateSubstrateSlice4KindsPromptSection +

	coldStartKindsPromptSection +

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
		// Database tier slice 2 (v0.89.66, #695 Stream 93) — the
		// coverage shorthand is provider-specific. The AWS path
		// (Provider="" or "aws") stays byte-identical to v0.89.65
		// because the row format string is unchanged and only the
		// coverage selector logic branches on Provider before
		// reading the new fields. GCP / Azure / OCI rows use the
		// matching per-cloud axis: QueryInsightsEnabled,
		// SQLInsightsDiagEnabled, DatabaseManagementEnabled. Each
		// renders as "covered" or "uncovered" — single-axis, no
		// pi-only / em-only intermediate state because the three
		// new providers only expose one observability lever each
		// at slice 2.
		coverage := "uncovered"
		switch d.Provider {
		case "gcp":
			if d.QueryInsightsEnabled {
				coverage = "covered"
			}
		case "azure":
			if d.SQLInsightsDiagEnabled {
				coverage = "covered"
			}
		case "oci":
			if d.DatabaseManagementEnabled {
				coverage = "covered"
			}
		default: // "" or "aws" — cold-start parity preserved.
			switch {
			case d.PerformanceInsightsEnabled && d.EnhancedMonitoringEnabled:
				coverage = "covered"
			case d.PerformanceInsightsEnabled:
				coverage = "pi-only"
			case d.EnhancedMonitoringEnabled:
				coverage = "em-only"
			}
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
	//
	// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100) — the
	// coverage shorthand is provider-specific. The AWS path
	// (Provider="" or "aws") stays byte-identical to v0.89.70 because
	// the row format string is unchanged and only the coverage
	// selector branches on Provider before reading the new fields.
	// GCP / Azure / OCI rows use the matching per-cloud axis:
	// ManagedPrometheusEnabled, AzureMonitorEnabled,
	// OperationsInsightsEnabled. Each renders as "covered" or
	// "uncovered" — single-axis, no logs-only / addon-only
	// intermediate state because the three new providers only expose
	// one observability lever each at slice 2.
	fmt.Fprintf(&b, "Clusters (%d total):\n", len(in.Clusters))
	csample := in.Clusters
	if len(csample) > 20 {
		csample = csample[:20]
	}
	for _, c := range csample {
		coverage := "uncovered"
		switch c.Provider {
		case "gcp":
			if c.ManagedPrometheusEnabled {
				coverage = "covered"
			}
		case "azure":
			if c.AzureMonitorEnabled {
				coverage = "covered"
			}
		case "oci":
			if c.OperationsInsightsEnabled {
				coverage = "covered"
			}
		default: // "" or "aws" — cold-start parity preserved.
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
			switch {
			case logsOn && hasObsAddon:
				coverage = "covered"
			case logsOn:
				coverage = "logs-only"
			case hasObsAddon:
				coverage = "addon-only"
			}
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

// DiscoverySystemPromptForTest is a test-only export of the discovery
// proposer's system prompt const. Trace integration slice 2 chunk 1
// (v0.89.80, #711 Stream 109) — cross-package tests in
// internal/proposer assert the 12 new trace-emission-* kind strings
// appear in the system prompt, and that the §11-style reasoning
// template is pinned. Re-declaring the const in the test file would
// drift; this exported helper keeps the substrate single-sourced.
func DiscoverySystemPromptForTest() string {
	return proposeFromDiscoveryScanSystem
}

// traceEmissionKindsPromptSection — trace integration slice 2 chunk 1
// (v0.89.80, #711 Stream 109). Twelve new recommendation kinds — one
// per (provider, tier) — fire when a resource has its observability
// primitive enabled but Squadron's traceindex has seen no spans from
// it in the last 24 hours. Section is appended to the discovery system
// prompt between the per-cloud kind list and the per-step rules so the
// model reads it alongside the other kinds.
//
// COLD-START PARITY INVARIANT: when the discovery scan context carries
// no inventory rows that trigger trace-emission kinds, the rendered
// user message stays byte-identical to v0.89.78 because this section
// lives ONLY in the system prompt and the detection branch on the
// proposer bridge controls when to surface the kinds via inventory
// rows. The 4-provider cold-start parity tests pin this invariant.
const traceEmissionKindsPromptSection = `TRACE EMISSION KINDS (slice 2 of trace integration):

These kinds fire when a resource has its observability primitive
enabled but Squadron's traceindex has seen no spans from it in
the last 24 hours. Always pair with iacpicker.Pick output for
the Terraform pattern.

For AWS:
- trace-emission-aws-compute: EC2 instance with otel-collector
  tag but no recent spans. Terraform pattern: aws_ssm_association
  with AWS-ConfigureAWSPackage installing the CloudWatch Agent
  (which includes the ADOT collector binary).
- trace-emission-aws-db: RDS instance with PerformanceInsights
  enabled but no spans from connecting workloads. Terraform:
  performance_insights_retention_period = 731 (LTR tier).
- trace-emission-aws-k8s: EKS cluster with ADOT addon active but
  no workload spans. Terraform: aws_eks_addon adot.

For GCP:
- trace-emission-gcp-compute: GCE instance with otel-collector
  label but no recent spans. Terraform: metadata enabling
  enable-osconfig + google-logging-enabled + google-monitoring-enabled.
- trace-emission-gcp-db: Cloud SQL with Query Insights but no
  application correlation. Terraform: record_application_tags +
  record_client_address.
- trace-emission-gcp-k8s: GKE with Managed Prometheus but no
  workload spans. Terraform: google_gke_hub_feature service mesh.

For Azure:
- trace-emission-azure-compute: VM with otel-collector tag but
  no recent spans. Terraform: AzureMonitorLinuxAgent extension.
- trace-emission-azure-db: Azure SQL with SQLInsights routing
  but no application correlation. Terraform:
  extended_auditing_policy log_monitoring_enabled.
- trace-emission-azure-k8s: AKS with Azure Monitor enabled but
  no workload spans. Terraform: monitor_metrics.annotations_allowed.

For OCI:
- trace-emission-oci-compute: Instance with otel tag but no
  recent spans. Terraform: cloud-init script in user_data
  (note: only runs on first boot — flag as
  upgrade-during-maintenance).
- trace-emission-oci-db: Autonomous DB with Database Management
  but no application correlation. Terraform:
  oci_database_management_managed_database_group.
- trace-emission-oci-k8s: OKE with Operations Insights tag but
  no workload spans. Terraform: OCI Service Operator via
  kubernetes_manifest.

REASONING TEMPLATE for trace-emission recommendations:

"This resource has the observability primitive enabled but
Squadron's traceindex has received no spans from it in the last
24 hours. Three failure modes are possible:
1. SDK not deployed: the most common cause. This Terraform PR
   targets this case by installing the cloud-native
   auto-instrumentation agent.
2. SDK deployed but exporter misconfigured: less common.
   Check the agent's endpoint configuration.
3. Resource attribute mismatch: the agent is emitting but with
   identifiers that don't match Squadron's expectation.

If case (2) or (3) applies, decline this PR and note the actual
case in the decline reason — the verdict learning loop will
record it for future recommendations."

The proposer should:
- Pick the matching trace-emission-* kind based on the
  (provider, tier) the inventory row belongs to.
- Include the picker's PrimaryTerraform as the patch contents.
- Include the picker's Reasoning + the failure mode template
  in the recommendation reasoning field.

` + "\n"

// spanQualityKindsPromptSection — span quality slice 1 chunk 2
// (v0.89.86, #717 Stream 115). Three new recommendation kinds that
// fire when Squadron's traceindex Quality observer detects a
// pathology in incoming spans from a resource. Detection runs on the
// existing OTLP receiver hot path; no new data collection.
//
// COLD-START PARITY INVARIANT: same as the trace-emission section
// above, this lives ONLY in the system prompt; the user-message
// renderer is unchanged. When the discovery scan context carries no
// inventory rows that trigger span-quality kinds (because no Quality
// observations are present yet, or all per-resource snapshots are
// below the §3 thresholds), the rendered user message stays byte-
// identical to v0.89.85. The 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSpanQualitySlice1
// pins this invariant.
const spanQualityKindsPromptSection = `SPAN QUALITY KINDS (slice 3 of trace integration):

These kinds fire when Squadron's traceindex Quality observer
detects a pathology in incoming spans from a resource. The
detection runs on the existing OTLP receiver hot path; no new
data collection.

- span-quality-orphan-trace: > 10% of spans from this resource
  in the last hour have parent_span_id values that don't
  resolve to any span in the same trace. Cause: broken W3C
  trace context propagation. Terraform pattern: enable
  tracecontext + baggage propagators on the SDK config.

- span-quality-missing-resource-attrs: > 25% of spans missing
  one or more required resource attributes (service.name,
  cloud.provider, cloud.account.id, cloud.region, and
  tier-specific identifiers). Cause: resource detector running
  with insufficient permissions or before metadata service
  reachable. Terraform pattern: IAM permission adjustment +
  env var to wait for metadata.

- span-quality-attribute-mismatch: > 5% of spans with
  placeholder values in required attributes (host.name=localhost,
  cloud.account.id=000000000000, service.name=unknown_service,
  etc.). Cause: SDK fell back to defaults when resource detector
  failed silently. Terraform pattern: explicit
  OTEL_RESOURCE_ATTRIBUTES env var injection hardcoding the
  correct values from the inventory row.

REASONING TEMPLATE for span-quality recommendations:

"Squadron's traceindex Quality observer has observed N% of
spans from this resource in the last hour with [pathology].
The most common cause is [cause]. This Terraform PR targets
[case] by [intervention].

If the actual cause is different, decline this PR — the
verdict learning loop will record the decline."

` + "\n"

// spanQualityTraceparentKindsPromptSection — span quality slice 2
// chunk 2 (v0.89.110, #748 Stream 146). Two new recommendation kinds
// that fire when Squadron's Quality observer detects W3C trace
// context anomalies on the existing OTLP receiver hot path. Both
// kinds reuse the existing span-quality- webhook prefix; NO new
// webhook routing changes are needed.
//
// COLD-START PARITY INVARIANT: same as the slice 1 span-quality
// section above, this lives ONLY in the system prompt; the user-
// message renderer is unchanged. When the discovery scan context
// carries no inventory rows that trigger the traceparent kinds
// (because no Quality observations exceeded the §3 thresholds), the
// rendered user message stays byte-identical to v0.89.107. The
// 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSpanQualitySlice2
// pins this invariant.
const spanQualityTraceparentKindsPromptSection = `SPAN QUALITY TRACEPARENT KINDS (slice 2):

These kinds fire when Squadron's Quality observer detects
W3C trace context anomalies. Both reuse the existing
span-quality- webhook prefix.

- span-quality-traceparent-missing: > 5% of CHILD spans
  from this resource arrive without a traceparent
  attribute. The most common cause is the SDK's HTTP server
  instrumentation not extracting the W3C context propagator
  on the inbound request. Possible causes:
  1. SDK was deployed but the context propagator middleware
     wasn't enabled in the application's HTTP server config.
  2. Custom middleware in front of the SDK consumes the
     traceparent header before the SDK reads it.
  3. The resource is a worker/background-job pod (no inbound
     HTTP) and child spans here are intra-process — in that
     case, decline the recommendation; the verdict learning
     loop records.
  Terraform: add OpenTelemetry context propagator middleware
  to the application via env var OTEL_PROPAGATORS=tracecontext,baggage
  injection (per-cloud pattern same as
  span-quality-orphan-trace from v0.89.86).

- span-quality-traceparent-malformed: > 1% of spans with
  a traceparent attribute carry values that don't conform
  to the W3C spec (version 00, 32-char trace_id non-zero,
  16-char parent_id non-zero, hex lowercase only). The 1%
  threshold is intentionally low — ANY malformed traceparent
  is unusual. Possible causes:
  1. Upstream service emits a CUSTOM trace ID format that
     doesn't fit W3C constraints (some legacy SDKs).
  2. SDK version mismatch — upstream emits a 'next-version'
     (01) traceparent and the downstream rejects it.
  3. The header is being rewritten by a proxy / load
     balancer in transit (rare; check ALB X-Amzn-Trace-Id
     handling).
  Terraform: pin the upstream SDK version to the latest
  W3C-compliant release. The specific Terraform pattern
  depends on the deployment shape (Lambda layer version,
  Kubernetes Deployment image tag, etc.) — the recommendation
  reasoning explains case-by-case.

REASONING TEMPLATE for traceparent recommendations:

"Squadron's Quality observer has observed N% of this
resource's [child] spans with [pathology] in the last hour.
The most common cause is [cause from the 3 above]. This
Terraform PR targets the SDK-side fix.

If your actual case is different (the runbook describes the
three failure modes), decline this PR — the verdict learning
loop will record the decline."

` + "\n"

// samplingRateKindsPromptSection — sampling rate analysis slice 1
// chunk 2 (v0.89.123, #763 Stream 161). One new recommendation kind —
// span-quality-sampling-too-aggressive — fires when the per-resource
// ratio of observed_span_count / expected_invocation_count is below
// 5% AND the resource processed at least 1000 invocations in the 24h
// window. Reuses the existing span-quality- webhook prefix from
// v0.89.86 (NO new webhook routing) and the cold-start-style
// 3-failure-mode reasoning framing.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no sampling rows that trigger the kind (because
// no resource has crossed the §3 ratio + minimum thresholds), the
// rendered user message stays byte-identical to v0.89.122 across
// all four providers. The 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostSamplingSlice1
// pins this invariant.
const samplingRateKindsPromptSection = `SAMPLING RATE KIND (slice 1 of sampling rate analysis):

- span-quality-sampling-too-aggressive: per-resource
  ratio of observed_span_count (Squadron traceindex 24h) /
  expected_invocation_count (cloud-native metric 24h) is
  below 5% AND the resource processed at least 1000
  invocations in the window. Reuses the span-quality-
  webhook prefix from v0.89.86 — NO new webhook routing.

  3-FAILURE-MODE REASONING (uniform across all 5 surfaces):
  (1) Default sampler too aggressive — many OTel SDKs default
      to TRACEIDRATIO_BASED at 0.1; check SDK config.
  (2) Adaptive sampling throttling — Application Insights and
      some OTel exporters throttle under load. The ratio is
      operator-experienced, not configured. Decline if intentional.
  (3) Tail-sampling collector — if a collector in front of
      Squadron selectively keeps spans, observed_span_count
      legitimately undercounts. Decline if intentional.

  Terraform per-cloud: OTEL_TRACES_SAMPLER_ARG=0.5 env var
  injection (default raise target; OPERATOR TUNES from there).
  Same pattern across AWS Lambda / Cloud Run / Cloud Functions /
  Azure Functions / OCI Functions — each cloud's env
  injection mechanism. The 0.5 starting point is a suggestion,
  not a prescription; operators tune based on cost tolerance
  + signal-to-noise. State this in the PR body so the operator
  reading the recommendation knows the value is an opening
  position.

CAVEAT FOR ALL SAMPLING RECOMMENDATIONS:
The exclusion table from #531 slice 2 chunk 4 handles operators
who deliberately run aggressive sampling for cost reasons.
The verdict learning loop records declines for tail-sampling
and adaptive-sampling cases.

` + "\n"

// errorRateKindsPromptSection — error rate correlation slice 1
// chunk 2 (v0.89.128, #768 Stream 166). One new recommendation
// kind — span-quality-error-rate-spike — fires when the per-resource
// current/baseline error rate ratio exceeds 2.0x AND the resource
// processed at least 1000 invocations AND at least 50 errors in the
// 24h window. Reuses the existing span-quality- webhook prefix from
// v0.89.86 (NO new webhook routing) and the cold-start-style
// 3-failure-mode reasoning framing.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no error-rate rows that trigger the kind (because
// no resource has crossed the §3 ratio + minimum thresholds), the
// rendered user message stays byte-identical to v0.89.127 across
// all four providers. The 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostErrorRateSlice1
// pins this invariant.
const errorRateKindsPromptSection = `ERROR RATE CORRELATION KIND (slice 1 of error rate analysis):

- span-quality-error-rate-spike: per-resource current/baseline
  error rate ratio exceeds 2.0x AND the resource processed at
  least 1000 invocations AND at least 50 errors in the 24h
  window. Baseline is 168h (7d). Reuses the span-quality-
  webhook prefix from v0.89.86 — NO new webhook routing.

  Per-cloud error metrics: AWS Lambda Errors, GCP Cloud Run
  request_count{5xx}, GCP Cloud Functions execution_count{error},
  Azure Functions FunctionErrors, OCI Functions
  function_invocation_count{error}.

  3-FAILURE-MODE REASONING (uniform across all 5 surfaces):
  (1) Recent deploy regression — MORE COMMON. Check the
      function's deployment timeline. If errors started after
      a deploy, revert or fix the regression at the application
      layer. This Terraform PR does NOT fix application bugs.
      DECLINE if your cause is (1).
  (2) Downstream dependency failure — MORE COMMON. If the
      function calls a database / API / queue that's failing,
      errors propagate. Investigate the downstream first.
      DECLINE if your cause is (2).
  (3) Resource exhaustion under load — throttling, memory
      pressure, connection-pool exhaustion. The Terraform PR
      raises memory + concurrency limits to give the function
      headroom. MERGE if your cause is (3).

  CASES (1) AND (2) ARE THE MORE COMMON CAUSES. The Terraform
  PR targets case (3). Operators whose actual cause is (1) or
  (2) decline the PR; the verdict learning loop records the
  decline so Squadron's future runs learn the distribution
  for the fleet.

  Terraform per-cloud (slice 1 ships case (3) only):
  - AWS Lambda: aws_lambda_function memory_size = 1024,
    reserved_concurrent_executions = 100.
  - GCP Cloud Run: google_cloud_run_service
    template.spec.container_concurrency = 80,
    resources.limits.memory = "1Gi".
  - GCP Cloud Functions: google_cloudfunctions2_function
    service_config.available_memory = "1Gi".
  - Azure Functions: azurerm_service_plan sku_name = "EP2"
    (Premium plan tier bump from EP1).
  - OCI Functions: oci_functions_function memory_in_mbs = 1024.

  Near-zero baseline guard: when the baseline error rate is
  below the 0.01% floor (essentially zero — no errors in the
  168h window), Squadron substitutes the floor as the
  comparison denominator to avoid spurious large ratios on
  tiny absolute counts. The per-resource API endpoint surfaces
  the substitution via baseline_adjusted = true so operators
  know the ratio is computed against a floor, not the raw
  baseline.

CAVEAT FOR ALL ERROR RATE RECOMMENDATIONS:
The exclusion table from #531 slice 2 chunk 4 handles
operators running chaos engineering tests who intentionally
spike errors. The verdict learning loop records declines for
deploy-regression and downstream-failure cases — those are
the MORE COMMON causes and the substrate counts on operators
declining the PR when the actual cause is (1) or (2).

` + "\n"

// serverlessTierKindsPromptSection — serverless tier slice 1 chunk 5
// (v0.89.92, #725 Stream 123). Eleven new recommendation kinds across
// five surfaces (Lambda / Cloud Run / Cloud Functions / Azure Functions
// / OCI Functions). Section is appended to the discovery system prompt
// alongside the trace-emission and span-quality sections so the model
// reads it as part of the universal kind catalog.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no serverless rows the rendered user message stays
// byte-identical to v0.89.88. The 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostServerlessTier
// pins this invariant.
const serverlessTierKindsPromptSection = `SERVERLESS TIER KINDS (slice 1 of serverless tier):

These kinds fire when an inventory row in the serverless tier
has its observability axis disabled. Each (provider, surface)
pair has 1-3 kinds:

For AWS Lambda:
- lambda-xray-active: function with tracing_config.mode set to
  PassThrough or absent. Terraform: aws_lambda_function
  tracing_config { mode = "Active" }.
- lambda-otel-layer: function without the AWS Distro for
  OpenTelemetry layer attached. Terraform: aws_lambda_function
  layers = [...existing, "arn:aws:lambda:<region>:901920570463:layer:aws-otel-{lang}-{ver}"]
- lambda-otel-wrapper: function missing AWS_LAMBDA_EXEC_WRAPPER
  env var. Terraform: aws_lambda_function environment {
  variables { AWS_LAMBDA_EXEC_WRAPPER = "/opt/otel-instrument" } }.

For GCP Cloud Run:
- cloudrun-trace-enable: service without the
  run.googleapis.com/trace annotation. Terraform:
  google_cloud_run_service metadata { annotations = {
  "run.googleapis.com/trace" = "true" } }.
- cloudrun-otel-sidecar: service without a sidecar container
  matching the OTel collector pattern. Terraform: add a
  containers block with name = "otel-collector" and the
  upstream collector image.
- cloudrun-otel-export-endpoint: service missing
  OTEL_EXPORTER_OTLP_ENDPOINT env on the user's container.
  Terraform: add env { name = "OTEL_EXPORTER_OTLP_ENDPOINT"
  value = "http://localhost:4318" } (pointing at the sidecar).

For GCP Cloud Functions:
- cloudfunc-trace-enable: function without GOOGLE_CLOUD_TRACE
  env var. Terraform: google_cloudfunctions_function
  environment_variables { GOOGLE_CLOUD_TRACE = "true" }.
- cloudfunc-otel-layer: function whose runtime supports OTel
  auto-instrumentation but env var
  OTEL_INSTRUMENTATION_AUTO_ENABLED is unset. Terraform:
  same environment_variables block with the OTel flag.

For Azure Functions:
- azfunc-appinsights-enable: function app without
  APPLICATIONINSIGHTS_CONNECTION_STRING app_setting.
  Terraform: azurerm_linux_function_app app_settings = {
  APPLICATIONINSIGHTS_CONNECTION_STRING = "..." }.
- azfunc-otel-distro: function app without OTEL_DOTNET_AUTO_HOME
  or OTEL_PYTHON_DISTRO app_setting. Terraform: same
  app_settings block with the matching distro env var.

For OCI Functions:
- ocifunc-apm-enable: function with config[OCI_APM_ENABLED]
  not set to "true". Terraform: oci_functions_function
  config = { OCI_APM_ENABLED = "true" }.
- ocifunc-otel-distro: function without OTEL_DISTRO config
  set. Terraform: same config block with OTEL_DISTRO = "auto".

REASONING TEMPLATE for serverless tier recommendations:

"This [surface] has [axis] disabled. The most common cause is
that the function/service was deployed before the team adopted
OpenTelemetry, or the IaC was authored from an older template.
This Terraform PR adds the missing primitive; once merged and
applied, Squadron's traceindex will populate the Last seen
column for this resource within ~5 minutes of the first
invocation."

` + "\n"

// orchestrationTierKindsPromptSection — orchestration tier slice 1
// chunk 4 (v0.89.97, #731 Stream 129). Six new recommendation kinds
// across three surfaces (Step Functions / Workflows / Logic Apps). OCI
// orchestration is deferred to slice 2; slice 1 covers AWS / GCP /
// Azure only. Section is appended to the discovery system prompt
// alongside the trace-emission, span-quality, and serverless tier
// sections so the model reads it as part of the universal kind catalog.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no orchestration rows the rendered user message
// stays byte-identical to v0.89.93 / v0.89.88 across all four
// providers. The 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostOrchestrationSlice1
// pins this invariant.
const orchestrationTierKindsPromptSection = `ORCHESTRATION TIER KINDS (slice 1):

These kinds fire when an inventory row in the orchestration
tier has its observability axis disabled. OCI orchestration is
deferred to slice 2; slice 1 covers AWS / GCP / Azure only.

For AWS Step Functions:
- stepfunc-xray-active: state machine with
  tracingConfiguration.enabled = false (or absent). Terraform:
  aws_sfn_state_machine tracing_configuration { enabled = true }.
  Caveat: EXPRESS state machines emit X-Ray segments only for
  per-state Lambda invocations, not the orchestration runtime
  itself. The recommendation reasoning notes this for EXPRESS
  workflows.
- stepfunc-logging-enable: state machine with
  loggingConfiguration.level = "OFF". Terraform: enable
  CloudWatch Logs destination via logging_configuration block.

For GCP Workflows:
- workflows-trace-enable: workflow with callLogLevel !=
  LOG_ALL_CALLS. Terraform: google_workflows_workflow
  call_log_level = "LOG_ALL_CALLS".
- workflows-logging-enable: workflow with callLogLevel =
  CALL_LOG_LEVEL_UNSPECIFIED. Terraform: same block setting
  call_log_level to at least LOG_ERRORS_ONLY.

For Azure Logic Apps:
- logicapps-appinsights-enable: Standard tier workflowapp
  without APPLICATIONINSIGHTS_CONNECTION_STRING app_setting.
  Terraform: azurerm_logic_app_standard app_settings = {
  APPLICATIONINSIGHTS_CONNECTION_STRING = "..." }.
- logicapps-diagnostics-enable: Consumption tier workflow
  without a Microsoft.Insights/diagnosticSettings child.
  Terraform: azurerm_monitor_diagnostic_setting attached to
  the azurerm_logic_app_workflow resource.

REASONING TEMPLATE for orchestration tier recommendations:

"This [surface] has [axis] disabled. Orchestration workflows
sit above the serverless / compute layer Squadron already
scans; without this axis on, Squadron's traceindex receives
no spans correlating the workflow execution back to its
per-state resource invocations. This Terraform PR enables
the missing primitive. After merge + apply + first execution,
the Last seen column populates for this orchestration within
~5 minutes."

ORCHESTRATION TIER OCI EXTENSION (slice 2 — v0.89.134-v0.89.136):

OCI's orchestration primitives are shape-different from
Step Functions / Workflows / Logic Apps:
- Resource Manager (Stacks + Jobs) — Terraform-as-a-service;
  INFRASTRUCTURE orchestration
- Process Automation (BPMN) — true workflow orchestration
  but smaller adoption; deferred to slice 3

Slice 2 picks Resource Manager as the operator-meaningful
surface. The Logging axis detection mirrors the OCI Streaming
logging proxy pattern from v0.89.101.

- resmgr-logging-enable: Resource Manager Stack does NOT have
  OCI Logging configured at the compartment level with
  service=resourcemanager source. Without Logging, failed
  apply/destroy operations leave no audit trail beyond the
  OCI console.
  Terraform: oci_logging_log_group + oci_logging_log with
  configuration.source.service = "resourcemanager",
  source_type = "OCISERVICE".

DECLINE PATH for resmgr-logging-enable: operators using a
non-OCI-Logging observability destination (custom processor
pulling from OCI Streaming with RM as a source, etc.) should
decline. The verdict learning loop records.

OCI orchestration coverage caveat: slice 2 detection is
COMPARTMENT-LEVEL (per §3.4 of the design doc) — a
compartment with Logging configured but NOT specifically
routed for RM gets has_log_axis=true. Operators who want
stricter detection should ensure log resources have explicit
RM source mappings; slice 3 may add per-source-mapping
inspection.

` + "\n"

// eventSourceTierKindsPromptSection — event source tier slice 1
// chunk 5 (v0.89.102, #738 Stream 136). Seven new recommendation
// kinds across four surfaces (EventBridge / Pub/Sub / Service Bus /
// Streaming). Unlike orchestration where OCI was deferred, the event
// source tier ships all four clouds in slice 1 because OCI Streaming
// is shape-compatible with the cross-cloud detection (a log-group
// proxy for the trace axis). Section is appended to the discovery
// system prompt alongside the trace-emission, span-quality, serverless,
// and orchestration tier sections so the model reads it as part of
// the universal kind catalog.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no event source rows the rendered user message
// stays byte-identical to v0.89.98 across all four providers. The
// 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice1
// pins this invariant.
const eventSourceTierKindsPromptSection = `EVENT SOURCE TIER KINDS (slice 1):

These kinds fire when an inventory row in the event source
tier has its observability axis disabled. Event sources are
the root of trace continuity — the layer where the trace ID
is created or fails to be created. Slice 1 detects at the
SOURCE level only; per-message propagation analysis is slice 2.

For AWS EventBridge:
- eventbridge-xray-enable: event bus with EventBridge Schemas
  Discoverer disabled (slice 1 uses log-target rules as a proxy
  for trace readiness; Schemas Discoverer detection is slice 2).
  Terraform: aws_schemas_discoverer description = "Squadron-recommended discoverer for bus X".
- eventbridge-schemas-discover: event bus without a Schemas
  Discoverer attached. Same Terraform pattern as eventbridge-xray-enable.
- eventbridge-logging-enable: event bus without any rule whose
  target points at a CloudWatch Logs log group. Terraform:
  aws_cloudwatch_event_target with arn = aws_cloudwatch_log_group.bus.arn.

For GCP Pub/Sub:
- pubsub-trace-enable: topic with tracingConfig.samplingRatio
  absent or set to 0. Terraform: google_pubsub_topic
  tracing_config { sampling_ratio = 1.0 } (or operator-tuned floor).
- pubsub-schema-attach: topic without a schema_settings.schema
  reference. Terraform: google_pubsub_topic schema_settings {
  schema = google_pubsub_schema.<name>.id encoding = "JSON" }.

For Azure Service Bus:
- servicebus-diagnostics-enable: namespace without diagnostic
  settings routing to App Insights OR Log Analytics workspace.
  Terraform: azurerm_monitor_diagnostic_setting target_resource_id =
  azurerm_servicebus_namespace.<name>.id with workspace_id OR
  application_insights_id set.

For OCI Streaming:
- streaming-logging-enable: stream without Logging service
  log group attached. Terraform: oci_logging_log resource with
  configuration.source.resource = stream.id and
  configuration.source.service = "streaming".

REASONING TEMPLATE for event source tier recommendations:

"This event source has [axis] disabled. Event sources are the
root of trace continuity — without this axis on, Squadron's
traceindex can't correlate the workflow execution back to the
inbound request. This Terraform PR enables the missing
primitive. After merge + apply + first event flow, Squadron's
last_seen_at column populates for this event source within
~5 minutes."

` + "\n"

// eventSourceTierPropagationKindsPromptSection — event source tier
// slice 2 chunk 5 (v0.89.107, #745 Stream 143). Adds the 5 per-message
// propagation recommendation kinds atop the slice 1 source-level axis
// kinds. These kinds fire when the slice 2 scanners (chunks 1-4,
// v0.89.105-v0.89.106) determine that an event source's CONFIG would
// drop trace context end-to-end even though the source-level trace
// axis is on. The kinds reuse the slice 1 prefixes
// (eventbridge-/pubsub-/servicebus-/streaming-) so the webhook
// routing in iac_github_webhook.go does not need new prefix matchers.
//
// Cold-start invariant: the user-message renderer is unchanged, so
// when the scan context carries no event source rows the rendered
// user message stays byte-identical to v0.89.103 across all four
// providers. The 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice2
// pins this invariant.
const eventSourceTierPropagationKindsPromptSection = `EVENT SOURCE TIER PROPAGATION KINDS (slice 2):

These kinds fire when an inventory row in the event source
tier has its source-level trace axis enabled BUT the
control-plane config would strip the trace context before
the downstream consumer receives it. Slice 1 detected the
SOURCE-LEVEL primitive; slice 2 detects the per-message
propagation config gap. Reuse the slice 1 prefixes — no new
webhook routing prefixes are introduced.

For AWS EventBridge:
- eventbridge-rule-preserves-trace: bus has at least one
  rule whose InputPath narrows past the X-Ray trace header
  (e.g. "$.detail") OR whose InputTransformer template omits
  the x-amzn-trace-id / traceparent literal. Terraform:
  aws_cloudwatch_event_target with input_path removed (or
  input_path = "$") and input_transformer including the
  x-amzn-trace-id header in its input_template.

For GCP Pub/Sub:
- pubsub-schema-includes-traceparent: topic with
  schema_settings.schema attached whose schema definition
  omits a traceparent / googclient_OpenTelemetryTraceparent
  field. Terraform: google_pubsub_schema definition adding
  the traceparent field (string, optional) at the topic's
  schema reference.
- pubsub-subscription-preserves-attrs: subscription with
  push delivery configured AND an attribute filter excluding
  the traceparent attribute key. Terraform:
  google_pubsub_subscription removing the offending filter
  or extending it to include traceparent.

For Azure Service Bus:
- servicebus-policy-preserves-traceparent: namespace whose
  authorization rules restrict ApplicationProperties via a
  property-restricting RBAC role assignment, blocking
  publishers from attaching traceparent. Terraform:
  azurerm_servicebus_namespace_authorization_rule rights =
  ["Listen", "Send"] without the property restriction, OR
  azurerm_role_assignment removing the restrictive custom
  role at the namespace scope.

For OCI Streaming:
- streaming-config-preserves-headers: stream with
  retentionInHours < 24, which may truncate Kafka headers
  carrying traceparent in some OCI Streaming versions.
  Terraform: oci_streaming_stream retention_in_hours = 24
  (or operator-tuned floor at or above 24).

REASONING TEMPLATE for event source tier propagation
recommendations:

"This event source has its source-level trace axis enabled,
but the propagation config would drop trace context before
the downstream consumer receives it: [specific note]. Even
though the cloud-native primitive is on, the
publisher-to-consumer trace correlation breaks at the
[rule / schema / policy / retention] boundary. This
Terraform PR adjusts the config so the next message carries
trace context end-to-end. Downstream consumer spans
correlate to the upstream source span after merge + apply +
first event flow."

` + "\n"

// eventSourceTierSlice3SNSKindsPromptSection — event source tier
// slice 3 chunk 2 (v0.89.139, #779 Stream 177). Adds 2 new SNS
// recommendation kinds atop the slice 1 + slice 2 event source
// catalog. Slice 3 widens the AWS surface count from 1 (EventBridge)
// to 2 (EventBridge + SNS); slices 4-7 will add the corresponding
// second surfaces per cloud (SQS / Cloud Tasks / Event Grid + Event
// Hubs / Notification Service). The sns- prefix is NEW — the webhook
// router gains an sns- → aws case in
// internal/api/handlers/iac_github_webhook.go.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no sns rows the rendered user message stays
// byte-identical to v0.89.136 across all four providers. The
// 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice3
// pins this invariant.
const eventSourceTierSlice3SNSKindsPromptSection = `EVENT SOURCE TIER SLICE 3 — AWS SNS (v0.89.137-139):

Adds AWS SNS as a second AWS event source surface alongside
EventBridge. Mirrors the slice 1 EventBridge log-target proxy
pattern — SNS doesn't have a direct OTel integration, so
Squadron uses the per-protocol delivery feedback role
attachment as the canonical "is delivery being audited?"
signal.

- sns-subscriptions-attach: SNS topic has zero confirmed
  subscriptions. Messages published to the topic are
  dropped on the floor. Either the topic is a leftover
  from a refactor (and should be deleted) OR a downstream
  consumer needs to subscribe.

  AUDIT-ONLY recommendation — no Terraform pattern. The
  operator decides what to subscribe (or delete the topic).
  This recommendation surfaces the topic for review.

- sns-delivery-logging-enable: SNS topic does NOT have
  CloudWatch Logs delivery status feedback configured for
  any protocol. Without it, the operator has no visibility
  into per-message delivery success/failure for the topic's
  HTTPS / SQS / Lambda / Application / Firehose
  subscriptions.

  Terraform: aws_iam_role for SNS delivery status logging +
  http_success_feedback_role_arn / sqs_success_feedback_role_arn /
  lambda_success_feedback_role_arn etc. attached to the
  aws_sns_topic resource (per-protocol feedback role
  attachment per §8 of the design doc).

DECLINE PATH for sns-delivery-logging-enable: operators
using a non-CloudWatch destination for delivery audit
(custom Lambda processor, SNS-to-Datadog integration, etc.)
should decline. The verdict learning loop records.

DECLINE PATH for sns-subscriptions-attach: operators who
intentionally keep an SNS topic with zero subscriptions as a
documentation placeholder should decline. The verdict
learning loop records; slice 4 may add a per-resource
"intentional dead topic" flag.

CAVEAT FOR ALL SNS RECOMMENDATIONS:
SNS rate limits at ~30 TPS per region per account. Squadron's
existing AWS substrate rate limiter absorbs but the per-cloud
runbook documents the scan-duration cost for large fleets.

` + "\n"

// eventSourceTierSlice4SQSKindsPromptSection — event source tier
// slice 4 chunk 2 (v0.89.142, #782 Stream 180). Adds 2 new SQS
// recommendation kinds atop the slice 1 + slice 2 + slice 3 event
// source catalog. Slice 4 widens the AWS surface count from 2
// (EventBridge + SNS) to 3 (EventBridge + SNS + SQS); slices 5-7 will
// add the corresponding second surfaces per cloud (Cloud Tasks /
// Event Grid + Event Hubs / Notification Service). The sqs- prefix is
// NEW — the webhook router gains an sqs- → aws case in
// internal/api/handlers/iac_github_webhook.go.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no sqs rows the rendered user message stays
// byte-identical to v0.89.139 across all four providers. The
// 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice4
// pins this invariant.
const eventSourceTierSlice4SQSKindsPromptSection = `EVENT SOURCE TIER SLICE 4 — AWS SQS (v0.89.140-142):

Adds AWS SQS as the third AWS event source surface alongside
EventBridge + SNS. SQS completes the canonical AWS pub/sub
fan-out architecture: EventBridge | SNS → SQS → consumer.

The detection axes mirror the slice 1 log-target proxy
pattern. SQS doesn't have a direct OTel integration — Squadron
uses the operational signal (redrive policy → DLQ) as the
trace primitive proxy. A queue with a redrive policy +
reachable DLQ means failed messages get captured for
post-mortem; without the redrive policy, failures vanish
silently.

- sqs-redrive-policy-enable: SQS queue has NO RedrivePolicy
  configured. When a message fails processing (consumer
  throws / times out), the message gets retried until it
  expires from the queue's retention window and vanishes
  silently — the SINGLE MOST COMMON AWS messaging production
  failure.

  Terraform: aws_sqs_queue resource for the DLQ + redrive_policy
  jsonencode block on the source queue with deadLetterTargetArn
  + maxReceiveCount per §8 of the design doc.

- sqs-deadletter-queue-attach: SQS queue has RedrivePolicy
  set with deadLetterTargetArn pointing at a queue ARN
  Squadron could NOT resolve in the same account+region.
  The DLQ may be cross-account/region (verify the source
  queue's IAM policy permits send) OR the DLQ doesn't exist
  (policy is dangling).

  AUDIT-ONLY recommendation — no Terraform pattern. The
  operator needs to confirm the intent (cross-account
  intentional vs DLQ deleted by mistake).

DECLINE PATH for sqs-redrive-policy-enable: operators using
a custom retry coordinator (Step Functions retry handler,
EventBridge Pipes with error handling, etc.) should decline.
The verdict learning loop records.

DECLINE PATH for sqs-deadletter-queue-attach: operators with
intentional cross-account DLQ setups should decline. Slice
5 may add a per-resource "cross-account intentional" flag.

CAVEAT FOR ALL SQS RECOMMENDATIONS:
SQS rate limits at ~30 TPS per region per account. Squadron's
existing AWS substrate rate limiter absorbs but the per-cloud
runbook documents the scan-duration cost for large fleets.

` + "\n"

// eventSourceTierSlice5CloudTasksKindsPromptSection — event source
// tier slice 5 chunk 2 (v0.89.145, #785 Stream 183). Adds 2 new GCP
// Cloud Tasks recommendation kinds atop the slice 1-4 event source
// catalog. Slice 5 widens the GCP surface count from 1 (Pub/Sub) to 2
// (Pub/Sub + Cloud Tasks); slices 6-7 will add the corresponding
// second + third surfaces per Azure / OCI. The cloudtasks- prefix is
// NEW — the webhook router gains a cloudtasks- → gcp case in
// internal/api/handlers/iac_github_webhook.go.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no cloudtasks rows the rendered user message stays
// byte-identical to v0.89.142 across all four providers. The
// 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice5
// pins this invariant.
const eventSourceTierSlice5CloudTasksKindsPromptSection = `EVENT SOURCE TIER SLICE 5 — GCP CLOUD TASKS (v0.89.143-145):

Adds GCP Cloud Tasks as the second GCP event source surface
alongside Pub/Sub. Architectural parity with the slice 4 SQS
pattern — Cloud Tasks is the GCP equivalent of SQS for
guaranteed delivery with retry semantics on HTTP failures.

The canonical GCP pub/sub-with-retry architecture:
Pub/Sub → Cloud Tasks → HTTP target.

- cloudtasks-retry-policy-enable: Cloud Tasks queue has
  retryConfig.maxAttempts = 0 (or retry config unset). When
  a task's HTTP target returns non-2xx, the task is dropped
  SILENTLY after the first attempt — no retry, no
  operator-visible audit signal. Equivalent to an SQS queue
  without a redrive policy.

  Terraform: google_cloud_tasks_queue retry_config block
  with max_attempts = 5 + min_backoff/max_backoff + max_doublings
  per §8 of the design doc. Operator tunes max_attempts based
  on consumer retry tolerance.

- cloudtasks-logging-enable: Cloud Tasks queue has
  stackdriverLoggingConfig.samplingRatio = 0 (or unset).
  Without Stackdriver Logging, the operator has no per-task
  delivery audit trail — successful AND failed dispatches
  both flow into the void.

  Terraform: google_cloud_tasks_queue stackdriver_logging_config
  block with sampling_ratio = 1.0 (full sampling). Operator
  tunes for high-throughput queues.

DECLINE PATH for cloudtasks-retry-policy-enable: operators
intentionally configuring fire-and-forget semantics (single
attempt, drop on failure) should decline. The verdict learning
loop records.

DECLINE PATH for cloudtasks-logging-enable: operators using a
non-Stackdriver destination for task audit (custom HTTP logger
sidecar, etc.) should decline.

CAVEAT FOR ALL CLOUD TASKS RECOMMENDATIONS:
The maxAttempts = -1 sentinel means unlimited retry; slice 5
treats this as configured retry (HasTraceAxis = true).

Cloud Tasks rate limits at ~10 RPS for listQueues per project;
substrate's existing GCP rate limiter absorbs the per-region
scan iteration.

` + "\n"

// eventSourceTierSlice6EventGridKindsPromptSection — event source
// tier slice 6 chunk 2 (v0.89.148, #788 Stream 186). Adds 2 new Azure
// Event Grid recommendation kinds atop the slice 1-5 event source
// catalog. Slice 6 widens the Azure surface count from 1 (Service Bus)
// to 2 (Service Bus + Event Grid); slice 7 will add Event Hubs +
// OCI Notification Service. The eventgrid- prefix is NEW — the
// webhook router gains an eventgrid- → azure case in
// internal/api/handlers/iac_github_webhook.go.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no eventgrid rows the rendered user message stays
// byte-identical to v0.89.145 across all four providers. The
// 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice6
// pins this invariant.
const eventSourceTierSlice6EventGridKindsPromptSection = `EVENT SOURCE TIER SLICE 6 — AZURE EVENT GRID (v0.89.146-148):

Adds Azure Event Grid as the second Azure event source surface
alongside Service Bus. Event Grid is Azure's fan-out
distribution layer for cloud events; Service Bus is the queue
pattern. After slice 6, Azure has 2 event source surfaces
matching GCP's count (Pub/Sub + Cloud Tasks).

The canonical Azure event distribution architecture:
Event Grid → Service Bus / Functions / Logic Apps.

- eventgrid-diagnostics-enable: Event Grid Topic has no
  diagnostic settings configured. Without diagnostic settings
  routing to App Insights OR a Log Analytics workspace, the
  operator has no visibility into per-event delivery
  success/failure for the topic's subscriptions.

  Mirrors the Service Bus servicebus-diagnostics-enable
  pattern from slice 1. Terraform: azurerm_monitor_diagnostic_setting
  with enabled_log categories (PublishFailures, PublishSuccess,
  DeliveryFailures, DeliverySuccess) + AllMetrics.

- eventgrid-cloudevent-schema-enforce: Event Grid Topic has
  inputSchema = "EventGridSchema" (Azure proprietary) OR
  "CustomEventSchema" (operator-defined). CloudEvents 1.0 —
  the W3C standard — is the canonical format for cross-vendor
  event interoperability AND includes the distributed tracing
  extension (traceparent in event extensions).

  Terraform: azurerm_eventgrid_topic input_schema =
  "CloudEventSchemaV1_0".

  WARNING: this is a BREAKING CHANGE for existing subscribers
  — they must consume the CloudEvents wire format. The
  reasoning text emphasizes coordination with subscribers
  before merging. Squadron drafts the PR but the operator's
  review catches the breakage risk.

DECLINE PATH for eventgrid-diagnostics-enable: operators
using a non-Insights destination (custom webhook capture,
etc.) should decline. The verdict learning loop records.

DECLINE PATH for eventgrid-cloudevent-schema-enforce:
operators standardized on the proprietary Azure schema for
ecosystem reasons should decline. Verdict learning loop
records.

CAVEAT FOR ALL EVENT GRID RECOMMENDATIONS:
Slice 6 covers Custom Topics. Event Grid System Topics
(auto-created by Azure services like Blob Storage) are slice
8+ candidate. Event Grid Domains (multi-tenant pattern) are
also slice 8+.

` + "\n"

// eventSourceTierSlice7ONSKindsPromptSection — event source tier
// slice 7 chunk 2 (v0.89.151, #793 Stream 190). One new
// recommendation kind: ons-logging-enable. Closes the cross-cloud
// event source widening pass at 3-2-2-2 surfaces.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no ONS observations the rendered user message
// stays byte-identical to v0.89.148 across all four providers.
// Pinned by TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice7.
const eventSourceTierSlice7ONSKindsPromptSection = `EVENT SOURCE TIER SLICE 7 — OCI NOTIFICATION SERVICE (v0.89.149-151):

Adds OCI Notification Service (ONS) as the second OCI event
source surface alongside Streaming. ONS is OCI's pub/sub
fan-out primitive — the analog of AWS SNS + GCP Pub/Sub on
the alert distribution side. After slice 7, OCI has 2 event
source surfaces matching Azure (Service Bus + Event Grid).

The cross-cloud event source widening pass closes at 3-2-2-2:
AWS 3 (EventBridge + SNS + SQS), GCP 2 (Pub/Sub + Cloud
Tasks), Azure 2 (Service Bus + Event Grid), OCI 2 (Streaming
+ Notification Service).

The canonical OCI alerting + integration architecture:
Monitoring alarm -> ONS topic -> subscribers (HTTP(S),
email, PagerDuty, Slack, Functions, etc.).

- ons-logging-enable: ONS Topic has no OCI Logging
  configuration. Without a log group capturing topic
  delivery events, the operator has no audit trail for
  which alarms / notifications were delivered to which
  subscribers — the first question in any incident
  postmortem where the operator needs to confirm 'did the
  page actually get sent?'.

  Mirrors the Streaming streaming-logging-enable pattern
  from slice 1. Terraform: oci_logging_log routing
  delivery events to a log group (operator's existing log
  group via var.default_log_group_id; new log group created
  if no operator-default is provided).

DECLINE PATH for ons-logging-enable: operators routing ONS
audit through a non-OCI-Logging destination (Cloud Guard
custom recipe, OCI Streaming capture, third-party SIEM
connector) should decline. The verdict learning loop records.

CAVEAT FOR ALL ONS RECOMMENDATIONS:
Slice 7 covers per-topic Logging axis. Per-subscription
detection (HTTP -> HTTPS, retry policy tuning) is slice 8+
candidate. Per-delivery audit reconstruction from the
Logging stream is slice 8+ candidate. ONS Subscription
confirmation lag detection (PENDING > 24h) is slice 8+.

` + "\n"

// eventSourceTierSlice8EventHubsKindsPromptSection — event source
// tier slice 8 chunk 2 (v0.89.154, #796 Stream 193). Two new
// recommendation kinds: eventhubs-diagnostics-enable +
// eventhubs-capture-enable. Brings Azure to parity with AWS at 3
// event source surfaces; cross-cloud count lands at 3-2-3-2 / 10
// surfaces total.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no Event Hubs observations the rendered user
// message stays byte-identical to v0.89.151 across all four
// providers. Pinned by
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostEventSourceSlice8.
const eventSourceTierSlice8EventHubsKindsPromptSection = `EVENT SOURCE TIER SLICE 8 — AZURE EVENT HUBS (v0.89.152-154):

Adds Azure Event Hubs as the third Azure event source
surface alongside Service Bus + Event Grid. Event Hubs is
Azure's big-data event ingestion primitive — a partitioned
log analogous to Kafka, distinct from the messaging
primitives. After slice 8, Azure has 3 event source
surfaces matching AWS's count.

Cross-cloud after slice 8: AWS 3 (EventBridge + SNS + SQS),
GCP 2 (Pub/Sub + Cloud Tasks), Azure 3 (Service Bus + Event
Grid + Event Hubs), OCI 2 (Streaming + Notification
Service). Total: 10 event source surfaces across 4 clouds.

The canonical Azure analytics ingestion architecture:
Event Hubs namespace -> Capture -> Blob Storage / ADLS,
with parallel Event Hubs namespace -> Stream Analytics /
Databricks consumption.

- eventhubs-diagnostics-enable: Event Hubs Namespace has no
  diagnostic settings configured. Without diagnostic
  settings routing to App Insights OR a Log Analytics
  workspace, the operator has no visibility into
  per-namespace delivery health, capture status, or
  throughput unit utilization.

  Mirrors the Service Bus servicebus-diagnostics-enable
  (slice 1) and Event Grid eventgrid-diagnostics-enable
  (slice 6) patterns. Terraform: azurerm_monitor_diagnostic_setting
  with the 5 Event Hubs log categories (ArchiveLogs,
  OperationalLogs, AutoScaleLogs, KafkaCoordinatorLogs,
  KafkaUserErrorLogs) + AllMetrics.

- eventhubs-capture-enable: Event Hubs Namespace has NO
  event hub with Capture enabled. Without Capture, events
  expire after the namespace's configured retention window
  (1 day default; 7 days max on Standard; 90 days max on
  Premium). The operator has no event-content audit trail
  beyond the retention window for incident postmortems.

  Capture auto-archives events to Blob Storage or Azure
  Data Lake Storage at configurable intervals (default
  5min / 300MB). Terraform: azurerm_eventhub with
  capture_description block enabling Capture on ONE hub
  (operator picks which during PR review).

  IMPORTANT: Squadron does NOT prescribe WHICH hub to
  enable Capture on. The proposer's reasoning text MUST
  emphasize this so reviewers see the intent explicitly —
  the operator picks based on which hub carries the
  durability-critical stream.

DECLINE PATH for eventhubs-diagnostics-enable: operators
using a non-Insights destination (custom capture pipeline,
third-party SIEM connector) should decline. The verdict
learning loop records.

DECLINE PATH for eventhubs-capture-enable: operators with
an out-of-band consumer pipeline doing archival (Databricks
+ Delta Lake ingestion, Stream Analytics persisting to its
own destination) should decline.

CAVEAT FOR ALL EVENT HUBS RECOMMENDATIONS:
Slice 8 covers per-namespace Logging + Capture axes.
Event Hubs Geo-DR (paired namespaces for disaster
recovery) is slice 9+ candidate. Per-consumer-group lag
detection is slice 9+. Per-partition throughput-unit
utilization / auto-inflate analysis requires substrate
MetricQuerier integration; slice 9+. Schema Registry
validation is slice 9+. Private endpoint configuration
validation is slice 9+.

` + "\n"

// eventSourceTierSlice9QueuesKindsPromptSection — event source tier
// slice 9 chunk 2 (v0.89.157, #799 Stream 196). One new
// recommendation kind: queues-logging-enable. Brings OCI to parity
// with AWS + Azure at 3 event source surfaces; cross-cloud count
// lands at 3-2-3-3 / 11 surfaces total.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no Queue observations the rendered user message
// stays byte-identical to v0.89.154 across all four providers.
const eventSourceTierSlice9QueuesKindsPromptSection = `EVENT SOURCE TIER SLICE 9 — OCI QUEUE SERVICE (v0.89.155-157):

Adds OCI Queue Service as the third OCI event source
surface alongside Streaming and Notification Service.
Queue Service is the transactional FIFO message queue
primitive analogous to AWS SQS — distinct from ONS pub/sub
fan-out (one consumer per message vs. many-consumer
fan-out) and from Streaming partitioned log analytics
intake. After slice 9, OCI has 3 event source surfaces
matching AWS + Azure.

Cross-cloud after slice 9: AWS 3 (EventBridge + SNS + SQS),
GCP 2 (Pub/Sub + Cloud Tasks), Azure 3 (Service Bus + Event
Grid + Event Hubs), OCI 3 (Streaming + Notification Service
+ Queue Service). Total: 11 event source surfaces across 4
clouds. Only GCP at 2 surfaces remains for slice 10+ to
close at 3-3-3-3 / 12 surfaces.

The canonical OCI task-processing architecture:
Queue service -> Functions / OKE consumers with
dead-letter queues for poison-message isolation.

- queues-logging-enable: OCI Queue has no OCI Logging
  configuration. Without a log group capturing queue
  delivery events, the operator has no audit trail for
  which messages were dequeued, processed, or sent to the
  DLQ — critical for postmortem analysis of consumer-side
  failures and poison-message investigation. When a
  message lands in the DLQ at 2am the operator has no
  record of which consumer attempted it — only that the
  DLQ count incremented.

  Mirrors the Streaming streaming-logging-enable
  (slice 1) and ONS ons-logging-enable (slice 7) patterns.
  Terraform: oci_logging_log routing queue delivery events
  to a log group (operator's existing log group via
  var.default_log_group_id; new log group created if no
  operator-default is provided).

DECLINE PATH for queues-logging-enable: operators routing
queue audit through a non-OCI-Logging destination (Cloud
Guard custom recipe, OCI Streaming capture, third-party
SIEM connector) should decline. The verdict learning loop
records.

CAVEAT FOR ALL QUEUE RECOMMENDATIONS:
Slice 9 covers per-queue Logging axis. DLQ configuration
inspection (per-queue deadLetterQueueDeliveryCount +
redelivery policy analysis) is slice 10+ candidate.
Per-message visibility timeout analysis requires
substrate-level consumer-processing-lag detection;
slice 10+. Channel-level inspection (OCI Queue per-channel
routing) is slice 10+. Per-queue CMEK / vault key rotation
validation is slice 10+.

` + "\n"

// eventSourceTierSlice10PubSubLiteKindsPromptSection — event source
// tier slice 10 chunk 2 (v0.89.160, #802 Stream 199). Two new
// recommendation kinds: pubsublite-logging-enable +
// pubsublite-reservation-attach. CLOSES the cross-cloud event source
// widening pass at 3-3-3-3 / 12 surfaces.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no Pub/Sub Lite observations the rendered user
// message stays byte-identical to v0.89.157 across all four
// providers.
const eventSourceTierSlice10PubSubLiteKindsPromptSection = `EVENT SOURCE TIER SLICE 10 — GCP PUB/SUB LITE (v0.89.158-160):

Adds GCP Pub/Sub Lite as the third GCP event source surface
alongside Pub/Sub and Cloud Tasks. CLOSES the cross-cloud
event source widening pass — after slice 10, all four
clouds carry 3 event source surfaces each.

CROSS-CLOUD AFTER SLICE 10: AWS 3 (EventBridge + SNS + SQS),
GCP 3 (Pub/Sub + Cloud Tasks + Pub/Sub Lite), Azure 3
(Service Bus + Event Grid + Event Hubs), OCI 3 (Streaming
+ Notification Service + Queue Service). Total: 12 event
source surfaces across 4 clouds at 3-3-3-3 grid.

Pub/Sub Lite is GCP's partitioned-log primitive, the
structural analog of AWS Kinesis Data Streams and Azure
Event Hubs. Distinct from full Pub/Sub in that Lite trades
managed routing + global delivery for cost efficiency at
high volume — operators self-manage partition capacity via
reservations. Zone-pinned by design.

The canonical GCP high-throughput analytics architecture:
Pub/Sub Lite topic -> Dataflow / Cloud Run consumers, with
reservations managing per-partition capacity.

- pubsublite-logging-enable: Pub/Sub Lite topic has no
  Cloud Logging sink configured filtering on
  resource.type="pubsublite_topic" + the topic's ID. Without
  the sink, the operator has no audit trail for publish
  failures, per-partition throughput exhaustion events, or
  reservation-related throttling — the failure modes unique
  to the Lite tier.

  Mirrors the slice 1 Pub/Sub pattern via the Cloud Logging
  sink primitive. Terraform: google_logging_project_sink
  filtering on the topic's ID with destination defaulting
  to a BigQuery dataset.

- pubsublite-reservation-attach: Pub/Sub Lite topic has NO
  reservation attached OR the referenced reservation does
  not exist in the topic's zone. Without a reservation, the
  topic is throttled to the bare minimum publish + subscribe
  throughput per partition — typically becoming a silent
  bottleneck under peak load.

  CRITICAL: this recommendation CREATES A BILLABLE RESOURCE.
  The proposer's reasoning text MUST emphasize this so PR
  reviewers see the cost implication explicitly. Default
  sizing is conservative (4 publish + subscribe units) but
  the operator MUST validate against ACTUAL peak throughput
  before merging — under-sized reservations re-create the
  throttling problem the recommendation solves.

  This is the FIRST recommendation in the event source tier
  that creates a billable resource. Prior kinds only
  configured Logging sinks or attached to existing
  resources.

DECLINE PATH for pubsublite-logging-enable: operators
routing Lite topic audit through a non-Cloud-Logging
destination (Stackdriver custom exporter, third-party SIEM)
should decline. The verdict learning loop records.

DECLINE PATH for pubsublite-reservation-attach: operators
who deliberately run Lite topics at the minimum-throughput
floor because the topic is BELOW the per-zone reservation
breakeven should decline. The verdict learning loop records.

CAVEAT FOR ALL PUB/SUB LITE RECOMMENDATIONS:
Slice 10 covers per-topic Logging + Reservation axes.
Per-subscription consumer-side lag detection is slice 11+
candidate. Cross-region disaster-recovery analysis is
out of scope (Pub/Sub Lite is zone-pinned by design).
Schema enforcement is slice 11+. Pub/Sub-to-Lite migration
recommendations require substrate-level cost modeling;
slice 11+. Per-message trace context propagation analysis
is slice 11+ candidate via substrate MetricQuerier.

STRATEGIC NOTE: after slice 10, the event source tier's
widening pass is COMPLETE. Future event source work is
per-axis depth (consumer lag, cost modeling, cross-surface
correlation), NOT per-cloud breadth.

` + "\n"

// dlqConfigSlice1KindsPromptSection — DLQ configuration analysis
// slice 1 chunk 4 (v0.89.166, #808 Stream 205). FIRST per-axis-depth
// slice; ships DLQ detection across all 4 clouds' queue tier
// surfaces. Recommendation kinds added (7 total — one cloud uses
// honest-framing namespace-level prerequisite kind):
//
//	AWS SQS:        sqs-dlq-attach, sqs-dlq-retry-count-bound
//	GCP Cloud Tasks: cloudtasks-dlq-pattern-add,
//	                 cloudtasks-retry-count-bound (§3.1 honest framing)
//	Azure Service Bus: servicebus-dlq-queue-walk-prerequisite
//	                   (§3.2 honest framing — scanner-coverage-gap)
//	OCI Queue Service: queues-dlq-attach, queues-dlq-retry-count-bound
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no DLQ-axis observations the rendered user
// message stays byte-identical to v0.89.165 across all four
// providers.
const dlqConfigSlice1KindsPromptSection = `DLQ CONFIGURATION ANALYSIS SLICE 1 — QUEUE TIER PER-AXIS DEPTH (v0.89.162-166):

FIRST PER-AXIS DEPTH SLICE following the cross-cloud event
source widening pass close at 3-3-3-3 / 12 surfaces. Squadron
graduates from "Squadron sees N surfaces per cloud" to "for
each surface, Squadron audits M axes of operational quality."

Slice 1 ships DLQ (Dead-Letter Queue) detection across all
4 clouds' queue tier surfaces. The DLQ axis matters for any
asynchronous workload: queues without DLQ destinations leak
poison messages back into the consumer worker pool
indefinitely, wasting consumer-side processing budget and
silently dropping work that should route for human review.

CROSS-CLOUD MAPPING:

  AWS   | SQS queue        | RedrivePolicy.deadLetterTargetArn (slice 4)
  GCP   | Cloud Tasks queue| retryConfig.maxAttempts        (slice 5)
  Azure | Service Bus      | namespace-level walk           (slice 1+2)
  OCI   | Queue Service    | deadLetterQueueDeliveryCount   (slice 9)

DETECTION RULES (each cloud's chunk pinned the band [2, 50]):

- Missing DLQ: poison messages redeliver indefinitely.
- Inappropriate retry count: maxReceiveCount / maxAttempts /
  deadLetterQueueDeliveryCount outside [2, 50] inclusive.
  Below 2 sends transient failures straight to the DLQ;
  above 50 defers DLQ routing past the typical consumer
  restart-and-retry horizon.

RECOMMENDATION KINDS (7):

- sqs-dlq-attach: SQS queue with no RedrivePolicy.
  Terraform creates a sibling SQS DLQ + attaches via
  redrive_policy with conservative maxReceiveCount=5.

- sqs-dlq-retry-count-bound: SQS queue with DLQ but
  maxReceiveCount outside [2, 50]. Terraform updates the
  existing redrive_policy block to a conservative value.

- cloudtasks-dlq-pattern-add: ALWAYS fires on Cloud Tasks
  queues (§3.1 HONEST FRAMING — GCP Cloud Tasks has NO
  managed DLQ primitive; the canonical pattern is consumer-
  side dead-letter routing the operator must wire from the
  HTTP target's final-retry failure handler). Terraform
  creates a sibling Cloud Tasks queue named
  ${original}-dlq + reasoning text EXPLICITLY calls out
  that Squadron CANNOT verify the consumer-side wiring —
  the operator PR review is load-bearing.

- cloudtasks-retry-count-bound: Cloud Tasks queue with
  retryConfig.maxAttempts outside [2, 50] OR set to -1
  (unlimited — operationally indistinguishable from absent
  for DLQ purposes). Terraform updates retryConfig.maxAttempts
  to a conservative value.

- servicebus-dlq-queue-walk-prerequisite: ALWAYS fires on
  Service Bus namespaces (§3.2 HONEST FRAMING —
  scanner-coverage-gap. The slice 1 + slice 2 Azure scanner
  walks Microsoft.ServiceBus/namespaces but NOT the per-queue
  Microsoft.ServiceBus/namespaces/queues sub-resource where
  forwardDeadLetteredMessagesTo,
  enableDeadLetteringOnMessageExpiration, and
  maxDeliveryCount actually live). Reasoning text EXPLICITLY
  calls out that the per-queue DLQ detection requires a
  future slice that adds the queue walk; until then the
  operator should manually audit per-queue DLQ config in the
  Azure portal.

- queues-dlq-attach: OCI Queue Service queue with
  deadLetterQueueDeliveryCount == 0 (or negative —
  defensive). Terraform updates the queue's
  dead_letter_queue_delivery_count to a conservative
  positive value (default 5).

- queues-dlq-retry-count-bound: OCI Queue Service queue with
  deadLetterQueueDeliveryCount outside [2, 50]. Terraform
  updates the value to a conservative number.

HONEST FRAMING PATTERNS ESTABLISHED:

  §3.1 — managed-primitive-absence (Cloud Tasks).
  §3.2 — scanner-coverage-gap (Azure Service Bus namespace).

Both patterns are load-bearing for slice 12+ substrate-
dependent depth work where Squadron will repeatedly hit
detection rules it cannot prove from its current scan view.
The honest framing IS the value: rather than pretending the
scanner sees more than it does, Squadron explicitly scopes
its recommendation reasoning to what it can prove, surfaces
operator review as load-bearing, and records decline-path
reasoning the verdict learning loop can cite.

DECLINE PATHS:

- Operators with deliberately tight (≤1) retry counts route
  transient failures straight to DLQ on the first failure —
  honest decline case for retry-count-bound recommendations.
- Operators with manual replay tooling / consumer-side
  side-channel write-out can decline DLQ-attach kinds.
- Cloud Tasks operators using non-Cloud-Tasks dead-letter
  destinations (Pub/Sub topic, Cloud Storage, BigQuery
  streaming) decline cloudtasks-dlq-pattern-add.

STRATEGIC NOTE: per-axis depth is the post-widening horizon.
After slice 1 (DLQ), candidate axes include consumer lag,
poison-message rate over time, cross-surface message-loss
estimation, and per-queue cost-per-message correlation. Each
axis can re-traverse the 4 clouds independently of the
others.

` + "\n"

// consumerLagSlice2KindsPromptSection — Consumer lag detection slice 2
// chunk 4 (v0.89.171, #813 Stream 210). SECOND per-axis-depth slice
// of the post-widening horizon, closes the consumer lag arc.
//
//	AWS SQS:           sqs-backlog-monitor-add,
//	                   sqs-consumer-silence-investigate
//	GCP Cloud Tasks:   cloudtasks-backlog-monitor-add (§3.1 honest framing)
//	Azure Service Bus: servicebus-backlog-queue-walk-prerequisite (§3.2 inherited)
//	OCI Queue Service: queues-backlog-monitor-add,
//	                   queues-consumer-silence-investigate
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged.
const consumerLagSlice2KindsPromptSection = `CONSUMER LAG DETECTION SLICE 2 — QUEUE TIER PER-AXIS DEPTH (v0.89.167-171):

SECOND per-axis-depth slice after DLQ slice 1 closed at
v0.89.166. Same playbook: pick one operational axis that
matters for every queue, detect on already-read scanner
fields where possible, ship honest-framing recommendations
for substrate gaps, route via existing per-cloud webhook
prefixes.

Consumer lag matters for every asynchronous workload: when
the consumer-side dequeue rate falls below the producer-side
enqueue rate, messages pile up. The retention window
eventually expires; downstream business logic latency
balloons; DLQ destinations (slice 1's axis) flood with
retry-exhausted messages, masking the underlying capacity
problem. Lag is the leading indicator. DLQ is the lagging
consequence.

CROSS-CLOUD MAPPING:

  AWS   | SQS queue        | ApproximateNumberOfMessages + ApproximateAgeOfOldestMessage (slice 4 GetQueueAttributes — read more fields from same response)
  GCP   | Cloud Tasks      | n/a — §3.3 honest framing (admin API does not surface task count as metric)
  Azure | Service Bus      | n/a — §3.4 inherited §3.2 scanner-coverage-gap (per-queue activeMessageCount lives at unwalked sub-resource)
  OCI   | Queue Service    | runtimeMetadata.visibleMessages + runtimeMetadata.timeStateLastChanged (slice 9 list response — read more fields from same payload)

DETECTION RULES (combined signal — backlog OR silence alone
is normal; both together is the firing condition):

- Backlog depth: backlog field ≥ 1000 →
  lag_backlog_depth_high=true.
- Consumer silence: oldest-message age (or
  timeStateLastChanged delta) ≥ 300s →
  lag_consumer_silence_high=true.

Same thresholds across AWS + OCI for cross-cloud consistency;
future per-cloud tuning slices can shift bounds
independently.

RECOMMENDATION KINDS (6):

- sqs-backlog-monitor-add: SQS queue with ApproximateNumberOfMessages
  ≥ 1000. Terraform creates CloudWatch alarm on the backlog
  metric.
- sqs-consumer-silence-investigate: SQS queue with
  ApproximateAgeOfOldestMessage ≥ 300s. Terraform creates
  CloudWatch alarm on the age metric.

- cloudtasks-backlog-monitor-add: ALWAYS fires (§3.3 honest
  framing — admin API gap). Terraform creates Cloud
  Monitoring alerting policy on
  cloudtasks.googleapis.com/queue/task_count. Reasoning text
  EXPLICITLY calls out that Squadron CANNOT verify the
  consumer is keeping up — the monitoring policy is the
  operator's load-bearing surrogate.

- servicebus-backlog-queue-walk-prerequisite: ALWAYS fires
  on Service Bus namespaces (§3.4 inherited §3.2 scanner-
  coverage-gap). Reasoning text EXPLICITLY calls out that
  per-queue activeMessageCount sits at the unwalked
  Microsoft.ServiceBus/namespaces/queues sub-resource. A
  future per-queue walk slice closes BOTH the DLQ slice 1
  chunk 3 deferrals AND the slice 2 chunk 3 deferrals.

- queues-backlog-monitor-add: OCI Queue Service with
  runtimeMetadata.visibleMessages ≥ 1000. Terraform creates
  OCI Monitoring alarm on visibleMessages metric.
- queues-consumer-silence-investigate: OCI Queue Service
  with timeStateLastChanged ≥ 300s ago. Terraform creates
  OCI Monitoring alarm on the silence proxy.

HONEST FRAMING REUSE — slice 2 is the FOURTH application:

  §3.1 — managed-primitive-absence (DLQ slice 1 chunk 2:
         Cloud Tasks; slice 2 chunk 2: Cloud Tasks again)
  §3.2 — scanner-coverage-gap (DLQ slice 1 chunk 3: Service
         Bus; slice 2 chunk 3: Service Bus inheriting)

The patterns now have repeated reuse across two per-axis-
depth slices. The pattern's load-bearing role is validated
+ the per-axis-depth horizon's shape is now clearly
understood:
  - AWS + OCI consistently ship real detection.
  - GCP + Azure consistently ship honest framing for the
    queue tier.

Each new axis adds 4 Detail keys per surface and 2-6
recommendation kinds. The SHAPE of the work is fixed.
Going forward, the per-axis-depth horizon scales by axis
count, not by cloud or by cloud-specific complexity.

DECLINE PATHS:

- Operators with deliberately batch-processing consumers
  (e.g. nightly drain) decline backlog-monitor-add — the
  signal is expected during quiet windows.
- Cloud Tasks operators using non-Cloud-Monitoring alerting
  surfaces decline cloudtasks-backlog-monitor-add.

STRATEGIC NOTE: slice 2 closes the second of N per-axis-
depth slices. The horizon ahead: throughput inversion
(substrate-dependent, slice 3+), poison-message rate over
time, cross-surface message-loss estimation, per-queue
cost-per-message correlation. Each axis re-traverses 4
clouds independently.

` + "\n"

// poisonRateSlice3KindsPromptSection — Poison-message rate
// analysis slice 3 chunk 4 (v0.89.176, #818 Stream 215). THIRD
// per-axis-depth slice of the post-widening horizon, closes the
// poison-message rate arc. ALL FOUR clouds ship §3.3
// substrate-metric-dependence honest framing (no mixed shape).
//
//	AWS SQS:           sqs-poison-rate-monitor-add        (§3.3 honest framing)
//	GCP Cloud Tasks:   cloudtasks-poison-rate-monitor-add (§3.3 honest framing)
//	Azure Service Bus: servicebus-poison-rate-monitor-add (§3.3 honest framing)
//	OCI Queue Service: queues-poison-rate-monitor-add     (§3.3 honest framing)
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged.
const poisonRateSlice3KindsPromptSection = `POISON-MESSAGE RATE ANALYSIS SLICE 3 — QUEUE TIER PER-AXIS DEPTH (v0.89.172-176):

THIRD per-axis-depth slice after DLQ slice 1 closed at
v0.89.166 + consumer lag slice 2 closed at v0.89.171. Same
playbook: pick one operational axis that matters for every
queue, detect where the scanner pass already has the data,
ship honest-framing recommendations for substrate gaps,
route via existing per-cloud webhook prefixes.

Poison-message RATE is a leading indicator distinct from
the DLQ slice 1 axis (does a DLQ EXIST?) and the consumer
lag slice 2 axis (is the consumer KEEPING UP?). A spiking
poison-message rate signals schema drift, a downstream
dependency outage, or a code regression on one message
shape — high rates exhaust consumer-side processing budget
BEFORE the messages land in the DLQ. DLQ presence is
structural; poison RATE is temporal.

§3.3 SUBSTRATE-METRIC-DEPENDENCE — THE UNIFYING SHAPE:
Unlike slices 1 + 2 (where AWS + OCI ship real detection
and GCP + Azure ship honest framing), slice 3 is uniform:
EVERY cloud's per-queue poison rate requires a time-series
metric delta that the single-pass scanner does NOT read.
There is NO mixed shape across chunks.

  AWS   | SQS          | DLQ ApproximateNumberOfMessages delta via CloudWatch GetMetricStatistics
  GCP   | Cloud Tasks  | cloudtasks.googleapis.com/queue/task_attempt_count via Cloud Monitoring
  Azure | Service Bus  | DeadletteredMessages delta via Azure Monitor metrics
  OCI   | Queue Service| dead-letter delivery delta via OCI Monitoring SummarizeMetricsData

DETECTION (all four clouds, slice 3):

- poison_rate_per_hour ALWAYS = -1 (absent sentinel).
- poison_rate_high_band ALWAYS = false.
- The future firing bound is 60/hour (1/minute), heuristic
  per design doc §4, shared across clouds for consistency.

RECOMMENDATION KINDS (4 — one per cloud, identical shape):

- sqs-poison-rate-monitor-add: ALWAYS fires per SQS queue
  (§3.3 honest framing). Terraform creates a CloudWatch
  alarm on the DLQ's ApproximateNumberOfMessages metric
  (threshold 60, 5-minute window). Reasoning text EXPLICITLY
  calls out that Squadron CANNOT yet compute the rate from
  the scanner pass — the alarm is the operator's load-bearing
  surrogate until the CloudWatch GetMetricStatistics
  integration lands.
- cloudtasks-poison-rate-monitor-add: ALWAYS fires per
  Cloud Tasks queue (§3.3). Terraform creates a Cloud
  Monitoring alerting policy on task_attempt_count.
- servicebus-poison-rate-monitor-add: ALWAYS fires per
  Service Bus namespace (§3.3). Terraform creates an Azure
  Monitor metric alert on DeadletteredMessages.
- queues-poison-rate-monitor-add: ALWAYS fires per OCI
  Queue Service queue (§3.3). Terraform creates an OCI
  Monitoring alarm on the dead-letter delivery metric.

All four route via the EXISTING per-cloud webhook prefixes
(sqs-, cloudtasks-, servicebus-, queues-) — NO new prefix
routing.

HONEST FRAMING — slice 3 is the FIFTH application + the
FIRST where a single variant (§3.3) covers all four clouds:

  §3.1 — managed-primitive-absence (DLQ slice 1 chunk 2 +
         lag slice 2 chunk 2: Cloud Tasks).
  §3.2 — scanner-coverage-gap (DLQ slice 1 chunk 3 + lag
         slice 2 chunk 3: Service Bus).
  §3.3 — substrate-metric-dependence (slice 3 ALL chunks):
         the data exists in a per-cloud time-series metric
         the scanner does not yet query.

§3.3 is the cleanest variant: a future substrate
MetricQuerier slice closes ALL FOUR clouds' deferrals at
once — the recommended next arc, mirroring how the
cold-start latency slice 1 -> slice 2 arc built the
MetricQuerier per cloud.

DECLINE PATHS:

- Operators who already monitor poison-message rates via a
  different surface (Datadog metric, SignalFx detector,
  existing observability stack) decline the monitor-add for
  their cloud.

STRATEGIC NOTE: slice 3 closes the THIRD per-axis-depth
slice. The horizon ahead: a substrate MetricQuerier
integration that closes ALL §3.3 deferrals at once
(recommended), cross-surface message-loss estimation, and
per-queue cost-per-message correlation. The per-axis-depth
shape is fixed: each axis adds 2-4 Detail keys per surface
and 2-6 recommendation kinds across the 4 clouds.

` + "\n"

// poisonRateSubstrateSlice4KindsPromptSection — Poison-rate substrate
// integration slice 4 chunk 1 (v0.89.177, #819 Stream 216). Converts
// the AWS SQS poison-rate axis from slice-3 §3.3 honest framing into
// REAL CloudWatch-backed detection. The other three clouds stay on
// §3.3 honest framing until their substrate chunks (4.2 GCP, 4.3
// Azure, 4.4 OCI) land — the model MUST NOT claim measured rates for
// them.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged.
const poisonRateSubstrateSlice4KindsPromptSection = `POISON-RATE SUBSTRATE INTEGRATION SLICE 4 — CLOSING THE §3.3 DEFERRALS (v0.89.177+):

Slice 3 shipped the poison-rate axis across all four clouds as
§3.3 substrate-metric-dependence HONEST FRAMING: the rate was
never measured (poison_rate_per_hour = -1 always). Slice 4
builds the per-cloud MetricQuerier integration that actually
reads the metric, one cloud per chunk — mirroring how the
cold-start latency arc built the CloudWatch / Cloud Monitoring
/ Azure Monitor / OCI Monitoring substrate per cloud.

CHUNK 1 (this release) — AWS SQS is now REAL:

- Squadron reads the dead-letter queue's NumberOfMessagesSent
  SUM over a trailing 1-hour window via CloudWatch
  GetMetricStatistics (AWS/SQS namespace, QueueName dimension),
  reusing the cold-start substrate's rate limiter +
  throttle-retry.
- poison_rate_per_hour now carries the MEASURED rate for AWS
  SQS source queues whose DLQ is reachable in the scanned
  account. poison_rate_high_band is the real
  rate >= 60/hour (1/min) verdict.
- Real-zero vs absent: a measured 0 (poison_rate_per_hour = 0)
  means "zero poison messages this hour" — a genuine green
  signal. -1 STILL means "not measured" (DLQ too new / no
  datapoints / unreachable cross-account DLQ / CloudWatch not
  wired). NEVER read -1 as zero.
- sqs-poison-rate-monitor-add reasoning text should now REPORT
  the measured rate when poison_rate_per_hour >= 0, instead of
  disclaiming the §3.3 gap. When poison_rate_per_hour = -1, fall
  back to the slice-3 §3.3 honest-framing reasoning.

CHUNK 2 (v0.89.178) — GCP Cloud Tasks is now REAL:

- Squadron reads the queue's FAILED task_attempt_count
  (response_code != "OK") SUM over a trailing 1-hour window via
  Cloud Monitoring timeSeries.list. Cloud Tasks has no DLQ
  primitive, so the failed-delivery-attempt rate is the poison
  signal — measured on the queue itself (no reachability gate).
- poison_rate_per_hour now carries the MEASURED failed-attempt
  rate for Cloud Tasks queues; poison_rate_high_band is the real
  rate >= 60/hour verdict. Same real-zero (0) vs absent (-1)
  contract as AWS.
- cloudtasks-poison-rate-monitor-add reasoning should REPORT the
  measured rate when poison_rate_per_hour >= 0, else fall back to
  the §3.3 reasoning.

CHUNK 3a/3b (v0.89.179-180) — Azure Service Bus is now REAL
(per-queue attribution):

- Squadron reads the namespace's DeadletteredMessages gauge and
  derives the poison rate as the max(Maximum) - min(Minimum)
  delta (net dead-letter accumulation) over a trailing 1-hour
  window via Azure Monitor. DeadletteredMessages is a gauge, so
  the delta — NOT a sum — is the rate; a constant backlog with no
  new arrivals reads 0 (real zero), not high.
- poison_rate_per_hour now carries the MEASURED namespace-level
  rate; poison_rate_high_band is the real rate >= 60/hour verdict.
  Same real-zero (0) vs absent (-1) contract as AWS + GCP.
- CHUNK 3b (v0.89.180) closes §3.2 (per-queue attribution): the
  DeadletteredMessages metric is split by the EntityName dimension,
  so poison_rate_per_hour now carries the WORST-offending queue's
  rate and two extra Detail keys are added when per-queue data
  exists: poison_rate_worst_queue (the queue name) and
  poison_rate_measured_queue_count. servicebus-poison-rate-monitor-add
  reasoning SHOULD now name the worst queue (poison_rate_worst_queue)
  when present — that is the actionable per-queue attribution. When
  no per-entity data is returned it falls back to the namespace-
  aggregated reading (no worst-queue key); report it as a
  namespace-level signal in that case.

CHUNK 4 (v0.89.181) — OCI Queue Service is now REAL. This is
the FINAL cloud — the substrate arc is CLOSED and §3.3 is fully
retired:

- Squadron reads the queue's dead-letter depth gauge
  (MessagesInDlq) via OCI Monitoring summarizeMetricsData and
  derives the rate as the max-min delta (net accumulation) over a
  trailing 1-hour window — the same gauge-delta shape as Azure.
- poison_rate_per_hour now carries the MEASURED rate for OCI
  queues; poison_rate_high_band is the real rate >= 60/hour
  verdict. Same real-zero (0) vs absent (-1) contract.
- queues-poison-rate-monitor-add reasoning should REPORT the
  measured rate when poison_rate_per_hour >= 0, else fall back to
  the §3.3 reasoning.

ALL FOUR CLOUDS ARE NOW REAL — no cloud remains on §3.3
poison-rate honest framing:

- AWS SQS (DLQ NumberOfMessagesSent, counter-sum),
- GCP Cloud Tasks (failed task_attempt_count, counter-sum),
- Azure Service Bus (DeadletteredMessages per-queue via
  EntityName split, gauge-delta),
- OCI Queue Service (MessagesInDlq, gauge-delta).

When poison_rate_per_hour == -1 it now means a genuine
"not measured" condition (metric client unwired, queue too new,
or no datapoints) — NOT a deferred cloud. Report the measured
rate whenever poison_rate_per_hour >= 0.

NO new IAM (cloudwatch:GetMetricStatistics already granted for
the Lambda metric paths covers AWS/SQS — the permission is
namespace-agnostic). NO new webhook prefix. Cold-start parity
preserved: the enrichment overwrites two existing Detail keys
and is a no-op when CloudWatch is not wired.

` + "\n"

// consumerLagSubstrateSlice5KindsPromptSection — Consumer-lag
// substrate integration slice 5 chunk 1 (v0.89.182, #824 Stream 221).
// Closes the slice-2 consumer-lag honest-framing deferrals via the
// proven MetricQuerier substrate, one cloud per chunk. Chunk 1 makes
// the GCP Cloud Tasks BACKLOG axis real.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged.
const consumerLagSubstrateSlice5KindsPromptSection = `CONSUMER-LAG SUBSTRATE INTEGRATION SLICE 5 — CLOSING THE SLICE-2 LAG DEFERRALS (v0.89.182+):

Consumer lag slice 2 shipped the lag axis across four clouds, but
GCP Cloud Tasks (§3.1) and Azure Service Bus (§3.2) shipped the
backlog as honest framing (lag_backlog_depth = -1 always). Slice 5
closes those deferrals by reading the backlog metric from the
per-cloud MetricQuerier substrate — the same pattern the poison-rate
substrate arc used. AWS SQS + OCI Queue Service lag were already
real in slice 2.

CHUNK 1 (this release) — GCP Cloud Tasks BACKLOG is now REAL:

- Squadron reads cloudtasks.googleapis.com/queue/depth (a gauge:
  number of tasks in the queue) via Cloud Monitoring with the
  ALIGN_MAX aligner — the peak backlog over the trailing 1-hour
  window. lag_backlog_depth now carries the measured peak;
  lag_backlog_depth_high is the real depth >= 1000 verdict.
- Same real-zero (0 = empty queue) vs absent (-1 = not measured)
  contract as the poison-rate arc.
- cloudtasks-backlog-monitor-add reasoning should REPORT the
  measured backlog when lag_backlog_depth >= 0, instead of
  disclaiming the §3.1 gap.

SILENCE AXIS STAYS HONEST-FRAMED (do NOT claim measurement): the
consumer-silence keys (lag_consumer_silence_seconds /
lag_consumer_silence_high) stay at -1 for Cloud Tasks — there is no
clean per-queue oldest-task-age metric. Backlog is the primary lag
signal; silence remains a documented deferral.

STILL HONEST-FRAMED: Azure Service Bus lag backlog (§3.2) until
chunk 2 (ActiveMessages per-queue via the EntityName split the
poison-rate chunk 3b built).

NO new IAM (monitoring.timeSeries.list already granted). NO new
webhook prefix. Cold-start parity preserved.

` + "\n"

// costCorrelationSlice6KindsPromptSection — Cost-correlation substrate
// slice 6 chunk 3 (v0.89.185, #827 Stream 224). When the cost
// substrate is wired, DLQ-bearing AWS SQS snapshots carry service-level
// cost in their Detail bag. This section tells the model how to surface
// it HONESTLY.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged.
const costCorrelationSlice6KindsPromptSection = `COST CORRELATION (cost-correlation substrate slice 6, v0.89.185+):

When the read-only cost substrate is wired, DLQ-bearing AWS SQS
queue snapshots AND Azure Service Bus namespace snapshots carry
these Detail keys:

  - service_cost_monthly_micro_usd: the account's Amazon SQS spend
    over the trailing 30 days, in MICRO-USD (divide by 1,000,000 for
    dollars; e.g. 42_500_000 = $42.50).
  - service_cost_currency: the billing currency (usually USD).
  - service_cost_scope: always "service" — this is the SERVICE total,
    NOT a per-queue cost.

HOW TO SURFACE IT (honest framing, load-bearing):

  - Report the figure plainly as supporting context on a DLQ /
    poison-rate recommendation: e.g. "Amazon SQS is costing ~$42.50/mo
    on this account; draining this DLQ reduces wasted spend."
  - ALWAYS label it as SERVICE-LEVEL cost, never attribute the whole
    figure to a single queue (service_cost_scope = "service" is the
    honest scope). Resource-level per-queue cost is a paid Cost
    Explorer opt-in Squadron deliberately does not use.
  - DO NOT editorialize about whether the cost is high or low, do not
    moralize, do not inflate. State the number and the actionable next
    step (drain the DLQ / fix the poison source) — nothing more.
  - When the keys are ABSENT (cost substrate not wired, over budget, or
    not measured), say nothing about cost — never guess a figure.

AWS SQS and Azure Service Bus carry cost keys today (both at
service level). GCP / OCI cost readers land in later chunks.

` + "\n"

// coldStartKindsPromptSection — Cold-start latency analysis slice 1
// chunk 3 (v0.89.115, #753 Stream 151). One new recommendation kind —
// lambda-cold-start-baseline — fires when the metric correlation
// substrate (slice 1 chunk 1/2) detects a 24h P95 InitDuration
// regression vs. the 7-day baseline on an AWS Lambda. Section is
// appended to the discovery system prompt alongside the trace-emission,
// span-quality, serverless, orchestration, and event source tier
// sections so the model reads it as part of the universal kind catalog.
//
// COLD-START PARITY INVARIANT: the section lives ONLY in the system
// prompt. The user-message renderer is unchanged, so when the scan
// context carries no cold-start observations the rendered user message
// stays byte-identical to v0.89.111 across all four providers. The
// 4-provider cold-start parity test
// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostColdStartSlice1
// pins this invariant.
const coldStartKindsPromptSection = `SERVERLESS COLD-START KINDS (cold-start latency analysis slice 1 + slice 2):

These kinds fire when Squadron's metric correlation substrate
detects cold-start latency regressions on serverless
resources. Slice 1 covered AWS Lambda; slice 2 extends to
GCP Cloud Run, GCP Cloud Functions, Azure Functions, OCI
Functions.

- lambda-cold-start-baseline: Lambda function with current
  24-hour P95 InitDuration that exceeds the 7-day baseline
  P95 by >= 1.5x AND exceeds 500ms absolute floor. The 1.5x
  ratio + 500ms floor combination filters out naturally-low
  cold-start functions hitting ratio thresholds on small
  absolute numbers.

  Three common causes the operator should evaluate:
  1. Init script regression: a recent deployment added heavy
     imports or startup work. Compare deployment timeline to
     regression onset.
  2. Cold-start frequency increase: reduced invocation rate
     means more invocations hit the cold path. Consider
     provisioned concurrency for predictable traffic.
  3. Architecture change: migration between architectures
     (x86_64 -> arm64) or runtime updates can shift
     cold-start behavior.

  Terraform: aws_lambda_provisioned_concurrency_config with
  provisioned_concurrent_executions = 1 (operator tunes the
  floor based on their traffic). Decline if the cause is (1)
  or (3) and trace the regression in deployment history /
  architecture change intent.

- cloudrun-cold-start-baseline: Cloud Run service with
  24-hour P95 request_latency exceeding the 7-day baseline
  by >= 1.5x AND above 500ms. CAVEAT: Cloud Run's
  request_latencies includes warm-path invocations;
  permanently-warm services may show false positives.
  Decline if your service uses min-instances and stays warm.
  Terraform: google_cloud_run_service annotations
  autoscaling.knative.dev/minScale = 1.

- cloudfunc-cold-start-baseline: Cloud Functions execution
  time exceeds baseline + floor. Same warm-path caveat as
  Cloud Run. Terraform: google_cloudfunctions2_function
  service_config min_instance_count = 1.

- azfunc-cold-start-baseline: Azure Function P95 execution
  duration exceeds baseline + floor. When the function
  runtime emits the IsAfterColdStart dimension (2023+
  runtimes), Squadron filters to cold-start invocations.
  Older runtimes get an unfiltered query with an
  informational note. Terraform: Premium Plan migration
  (azurerm_service_plan sku_name = "EP1") OR disable
  placeholder mode (WEBSITE_USE_PLACEHOLDER = "0").

- ocifunc-cold-start-baseline: OCI Function P95
  function_duration exceeds baseline + floor AND
  cold_start_count > 0 in the current window. Squadron
  skips detection when no cold starts happened in the
  window. Terraform: WARMUP_DELAY adjustment;
  provisioned_concurrent_executions when OCI exits preview.

3-FAILURE-MODE REASONING applies to all 4 kinds (same as
slice 1 lambda-cold-start-baseline):
(1) Init script regression — decline + fix in app layer
(2) Cold-start frequency increase — merge the PR
(3) Architecture change — decline + accept new baseline

Per-cloud caveats applied to the reasoning text:
- Cloud Run + Cloud Functions: warm-path inclusion
- Azure Functions: fallback when runtime doesn't emit IsAfterColdStart
- OCI Functions: function_duration not cold-start-isolated

REASONING TEMPLATE for cold-start recommendations:

"This Lambda function's 24-hour P95 cold-start duration is
{current_p95}ms, {ratio}x its 7-day baseline of
{baseline_p95}ms. Squadron flags this when the ratio
exceeds 1.5x AND the absolute value exceeds 500ms. Three
common causes - pick the one matching your deployment
history."

` + "\n"

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
