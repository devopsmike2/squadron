// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import "fmt"

// PickSNSDeliveryLoggingPattern emits the Terraform snippet for the
// sns-delivery-logging-enable recommendation per event source tier
// slice 3 chunk 2 (v0.89.139, #779 Stream 177). Configures an IAM
// role for SNS delivery status logging + per-protocol feedback role
// attachments on the topic resource per §8 of the design doc
// (docs/proposals/event-source-tier-slice3.md).
//
// Slice 3 widens the AWS event source surface count from 1
// (EventBridge) to 2 (EventBridge + SNS). Mirrors the slice 1
// EventBridge log-target proxy pattern — SNS has no direct OTel
// integration, so the per-protocol delivery feedback role attachment
// is the canonical "is delivery being audited?" signal.
//
// Slice 3 picks ALL 5 protocols (http/sqs/lambda/application/
// firehose) — the role is the same per protocol; the operator can
// prune the protocols their topic doesn't use during PR review.
//
// row.ResourceTFName is the best-effort Terraform resource name the
// proposer extracted from the operator's repo. When empty, the
// snippet falls back to "<name>" so the operator can substitute the
// real topic name during review (matches the slice 2 chunk 2
// pickResourceManagerLoggingPattern fallback shape).
//
// The reasoning text reuses the slice 1 honest-framing pattern: the
// operator may have intentionally chosen a non-CloudWatch delivery
// audit destination (custom Lambda processor, SNS-to-Datadog
// integration, etc.), and the verdict learning loop records the
// decline.
//
// There's NO Terraform pattern for sns-subscriptions-attach — it's an
// audit-only recommendation per §8 of the design doc. The operator
// decides what to subscribe (or whether to delete the topic). The
// proposer prompt extension documents this; no iacpicker entry
// needed.
func PickSNSDeliveryLoggingPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	terraform = fmt.Sprintf(`data "aws_iam_policy_document" "sns_delivery_logging_%s_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["sns.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "sns_delivery_logging_%s" {
  name               = "sns-delivery-logging-${aws_sns_topic.%s.name}"
  assume_role_policy = data.aws_iam_policy_document.sns_delivery_logging_%s_assume.json
}

resource "aws_iam_role_policy_attachment" "sns_delivery_logging_%s" {
  role       = aws_iam_role.sns_delivery_logging_%s.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonSNSRole"
}

resource "aws_sns_topic" "%s" {
  # ... existing fields ...

  http_success_feedback_role_arn        = aws_iam_role.sns_delivery_logging_%s.arn
  http_failure_feedback_role_arn        = aws_iam_role.sns_delivery_logging_%s.arn
  sqs_success_feedback_role_arn         = aws_iam_role.sns_delivery_logging_%s.arn
  sqs_failure_feedback_role_arn         = aws_iam_role.sns_delivery_logging_%s.arn
  lambda_success_feedback_role_arn      = aws_iam_role.sns_delivery_logging_%s.arn
  lambda_failure_feedback_role_arn      = aws_iam_role.sns_delivery_logging_%s.arn
  application_success_feedback_role_arn = aws_iam_role.sns_delivery_logging_%s.arn
  application_failure_feedback_role_arn = aws_iam_role.sns_delivery_logging_%s.arn
  firehose_success_feedback_role_arn    = aws_iam_role.sns_delivery_logging_%s.arn
  firehose_failure_feedback_role_arn    = aws_iam_role.sns_delivery_logging_%s.arn
}
`, name, name, name, name, name, name,
		name, name, name, name, name,
		name, name, name, name, name, name)

	reasoning = "AWS SNS topics need per-protocol delivery feedback IAM role attachments for CloudWatch Logs to record per-message delivery success/failure. Slice 3 configures all 5 protocols (http/sqs/lambda/application/firehose); prune the protocols you don't use. Decline if your team uses a non-CloudWatch destination for delivery audit (custom Lambda processor, SNS-to-Datadog integration, etc.) — the verdict learning loop records."

	return
}

// PickSQSRedrivePolicyPattern emits the Terraform snippet for a
// sqs-redrive-policy-enable recommendation per event source tier
// slice 4 chunk 2 (v0.89.142, #782 Stream 180). Configures a
// dead-letter queue + redrive_policy on the source queue per §8 of
// docs/proposals/event-source-tier-slice4.md.
//
// The DLQ retention is 14 days (1209600s) — maximum SQS retention —
// to give operators the longest window for post-mortem. The
// maxReceiveCount defaults to 5; operators tune based on consumer
// retry tolerance.
//
// Slice 4 widens the AWS event source surface count from 2
// (EventBridge + SNS) to 3 (EventBridge + SNS + SQS). The redrive
// policy + DLQ pair is the canonical "failed messages get captured
// for post-mortem" signal; a queue without it silently drops messages
// once the retention window expires — the single most common AWS
// messaging production failure.
//
// row.ResourceTFName is the best-effort Terraform resource name the
// proposer extracted from the operator's repo. When empty, the
// snippet falls back to "<name>" so the operator can substitute the
// real queue name during review (matches the slice 3 chunk 2
// PickSNSDeliveryLoggingPattern fallback shape).
//
// There's NO Terraform pattern for sqs-deadletter-queue-attach — it's
// an audit-only recommendation per §8 of the design doc. The operator
// confirms intent (cross-account intentional vs DLQ deleted by
// mistake). The proposer prompt extension documents this; no iacpicker
// entry needed.
func PickSQSRedrivePolicyPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "aws_sqs_queue" "%s_dlq" {
  name                       = "${aws_sqs_queue.%s.name}-dlq"
  message_retention_seconds  = 1209600  # 14 days
  kms_master_key_id          = "alias/aws/sqs"
}

resource "aws_sqs_queue" "%s" {
  # ... existing fields ...

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.%s_dlq.arn
    maxReceiveCount     = 5  # operator tunes
  })
}
`, name, name, name, name)

	reasoning = "AWS SQS queues without a RedrivePolicy + dead-letter queue silently drop messages on consumer failure (after the queue's retention window expires). This is the single most common AWS messaging production failure. The PR configures a DLQ + redrive policy. Decline if your team uses a custom retry coordinator (Step Functions, EventBridge Pipes with error handling, etc.) — the verdict learning loop records."

	return
}

// PickCloudTasksRetryPolicyPattern emits the Terraform snippet for a
// cloudtasks-retry-policy-enable recommendation per event source tier
// slice 5 chunk 2 (v0.89.145, #785 Stream 183). Configures a retry_config
// block on the google_cloud_tasks_queue resource per §8 of
// docs/proposals/event-source-tier-slice5.md.
//
// max_attempts defaults to 5 — typical "retry a few times with
// exponential backoff before giving up" semantics. Operators tune
// based on consumer retry tolerance. The backoff doubles from 10s up
// to 300s; max_retry_duration is 0s (unlimited duration, bounded by
// max_attempts).
//
// Slice 5 widens the GCP event source surface count from 1 (Pub/Sub)
// to 2 (Pub/Sub + Cloud Tasks). A queue without retry config silently
// drops tasks when the HTTP target returns non-2xx — the GCP
// equivalent of an SQS queue without a redrive policy (slice 4).
//
// row.ResourceTFName is the best-effort Terraform resource name the
// proposer extracted from the operator's repo. When empty, the
// snippet falls back to "<name>" so the operator can substitute the
// real queue name during review (matches the slice 4 chunk 2
// PickSQSRedrivePolicyPattern fallback shape).
func PickCloudTasksRetryPolicyPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "google_cloud_tasks_queue" "%s" {
  # ... existing fields ...

  retry_config {
    max_attempts       = 5     # operator tunes
    min_backoff        = "10s"
    max_backoff        = "300s"
    max_retry_duration = "0s"  # unlimited duration; bounded by max_attempts
    max_doublings      = 5
  }
}
`, name)

	reasoning = "Cloud Tasks queues without a retry_config silently drop tasks when the HTTP target returns non-2xx. The PR configures retry with exponential backoff (max_attempts = 5, doubling backoff from 10s to 300s). Decline if your team intentionally wants single-attempt fire-and-forget semantics — the verdict learning loop records."

	return
}

// PickCloudTasksLoggingPattern emits the Terraform snippet for a
// cloudtasks-logging-enable recommendation per event source tier
// slice 5 chunk 2 (v0.89.145, #785 Stream 183). Configures a
// stackdriver_logging_config block on the google_cloud_tasks_queue
// resource per §8 of docs/proposals/event-source-tier-slice5.md.
//
// sampling_ratio defaults to 1.0 (full sampling). Operators tune
// downward for very-high-throughput queues where full sampling is
// expensive. Without Stackdriver Logging, the operator has no per-task
// delivery audit trail — successful AND failed dispatches both flow
// into the void.
//
// row.ResourceTFName fallback shape mirrors PickCloudTasksRetryPolicyPattern.
func PickCloudTasksLoggingPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "google_cloud_tasks_queue" "%s" {
  # ... existing fields ...

  stackdriver_logging_config {
    sampling_ratio = 1.0  # full sampling; operator tunes for high-throughput
  }
}
`, name)

	reasoning = "Cloud Tasks queues without Stackdriver Logging have no per-task audit trail. The PR configures full sampling (1.0). For very-high-throughput queues where full sampling is expensive, tune the ratio downward. Decline if your team uses a non-Stackdriver destination for task audit — the verdict learning loop records."

	return
}

// PickEventGridDiagnosticsPattern emits the Terraform snippet for an
// eventgrid-diagnostics-enable recommendation per event source tier
// slice 6 chunk 2 (v0.89.148, #788 Stream 186). Configures diagnostic
// settings routing to a Log Analytics workspace (operator provides
// workspace ID via variable) with the 4 enabled_log categories Event
// Grid supports per §8 of docs/proposals/event-source-tier-slice6.md.
//
// Slice 6 widens the Azure event source surface count from 1 (Service
// Bus) to 2 (Service Bus + Event Grid). The diagnostic-settings axis
// mirrors the slice 1 Service Bus servicebus-diagnostics-enable
// pattern verbatim — same Microsoft.Insights/diagnosticSettings child
// resource shape.
//
// row.ResourceTFName is the best-effort Terraform resource name the
// proposer extracted from the operator's repo. When empty, the
// snippet falls back to "<name>" so the operator can substitute the
// real topic name during review (matches the slice 5 chunk 2
// PickCloudTasksRetryPolicyPattern fallback shape).
func PickEventGridDiagnosticsPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "azurerm_monitor_diagnostic_setting" "%s_diag" {
  name                       = "${azurerm_eventgrid_topic.%s.name}-diag"
  target_resource_id         = azurerm_eventgrid_topic.%s.id
  log_analytics_workspace_id = var.log_analytics_workspace_id  # operator provides

  # Event Grid topics support DeliveryFailures / PublishFailures /
  # DataPlaneRequests only. The success-variant categories are NOT valid and
  # make terraform apply fail with "category not supported".
  enabled_log {
    category = "DeliveryFailures"
  }
  enabled_log {
    category = "PublishFailures"
  }
  enabled_log {
    category = "DataPlaneRequests"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}
`, name, name, name)

	reasoning = "Event Grid Topics without diagnostic settings have no per-event delivery audit trail. The PR configures Microsoft.Insights/diagnosticSettings routing to a Log Analytics workspace with the valid Event Grid log categories (DeliveryFailures, PublishFailures, DataPlaneRequests) + AllMetrics. Decline if your team uses a non-Insights destination — the verdict learning loop records."

	return
}

// PickEventGridCloudEventSchemaPattern emits the Terraform snippet for
// an eventgrid-cloudevent-schema-enforce recommendation per event
// source tier slice 6 chunk 2 (v0.89.148, #788 Stream 186) per §8 of
// docs/proposals/event-source-tier-slice6.md.
//
// CRITICAL: this is a BREAKING CHANGE for existing subscribers — the
// wire format changes from EventGridSchema / CustomEventSchema to
// CloudEvents 1.0. The reasoning text emphasizes coordination with
// subscribers before merging — Squadron drafts the PR but the
// operator's review catches the breakage risk. The Terraform also
// carries an inline WARNING comment so the PR review surfaces the
// breakage even when the operator hasn't read the reasoning.
//
// row.ResourceTFName fallback shape mirrors PickEventGridDiagnosticsPattern.
func PickEventGridCloudEventSchemaPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "azurerm_eventgrid_topic" "%s" {
  # ... existing fields ...

  # WARNING: changing input_schema is a BREAKING CHANGE for
  # existing subscribers — they must consume the new wire
  # format. Coordinate before merging.
  input_schema = "CloudEventSchemaV1_0"  # was: EventGridSchema or CustomEventSchema
}
`, name)

	reasoning = "Event Grid Topics with proprietary input_schema (EventGridSchema or CustomEventSchema) lose cross-vendor interoperability AND skip the W3C CloudEvents distributed tracing extension (traceparent). Switching to CloudEventSchemaV1_0 is a BREAKING CHANGE for existing subscribers — they must consume the new wire format. Coordinate before merging. Decline if your team has standardized on the proprietary Azure schema."

	return
}

// PickONSLoggingPattern emits the Terraform snippet for an
// ons-logging-enable recommendation per event source tier slice 7
// chunk 2 (v0.89.151, #793 Stream 190). Configures an OCI Logging
// service log routing the topic's delivery events to a log group per
// §8 of docs/proposals/event-source-tier-slice7.md.
//
// Slice 7 closes the cross-cloud event source widening pass by
// adding OCI Notification Service (ONS) as the second OCI event
// source surface (alongside Streaming, slice 1). The Logging axis on
// ONS mirrors the slice 1 streaming-logging-enable pattern exactly —
// same oci_logging_log resource shape, same configuration block,
// same retention default. The only differences are
// service = "notification" (vs. "streaming") and source_type still
// "OCISERVICE" for both.
//
// row.ResourceTFName is the best-effort Terraform resource name the
// proposer extracted from the operator's repo. When empty, the
// snippet falls back to "<name>" so the operator can substitute the
// real topic name during review (matches every slice in the
// event-source family).
//
// The log_group_id reference is var.default_log_group_id — the
// operator's existing log group is reused when set; when unset, the
// operator extends var declarations during PR review. This keeps the
// emitted Terraform compatible with operators who manage the log
// group destination outside Squadron's PR scope.
func PickONSLoggingPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "oci_logging_log" "%s_delivery_log" {
  display_name = "${oci_ons_notification_topic.%s.name}-delivery-log"
  log_group_id = var.default_log_group_id  # operator provides
  log_type     = "SERVICE"

  configuration {
    source {
      category    = "all"
      resource    = oci_ons_notification_topic.%s.id
      service     = "notification"
      source_type = "OCISERVICE"
    }
    compartment_id = oci_ons_notification_topic.%s.compartment_id
  }

  is_enabled         = true
  retention_duration = 30  # operator may tune
}
`, name, name, name, name)

	reasoning = "ONS Topics without OCI Logging configured have no audit trail for which alarms / notifications were delivered to which subscribers — the first question in any incident postmortem where the operator needs to confirm 'did the page actually get sent?'. Mirrors the slice 1 streaming-logging-enable pattern. The PR configures an oci_logging_log routing delivery events to var.default_log_group_id; operators using a different log group can swap the variable. Decline if your team routes ONS audit through a non-OCI-Logging destination (Cloud Guard custom recipe, OCI Streaming capture, third-party SIEM connector) — the verdict learning loop records."

	return
}

// PickEventHubsDiagnosticsPattern emits the Terraform snippet for an
// eventhubs-diagnostics-enable recommendation per event source tier
// slice 8 chunk 2 (v0.89.154, #796 Stream 193). Configures diagnostic
// settings routing to a Log Analytics workspace (operator provides
// workspace ID via variable) with the 5 enabled_log categories Event
// Hubs supports per §8 of docs/proposals/event-source-tier-slice8.md.
//
// Slice 8 closes the cross-cloud event source widening pass by adding
// Event Hubs as the third Azure surface, matching AWS's 3-surface
// count. The diagnostic settings axis mirrors the slice 1 Service Bus
// + slice 6 Event Grid patterns exactly — same
// azurerm_monitor_diagnostic_setting resource shape.
//
// row.ResourceTFName fallback shape mirrors PickEventGridDiagnosticsPattern.
func PickEventHubsDiagnosticsPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "azurerm_monitor_diagnostic_setting" "%s_diag" {
  name                       = "${azurerm_eventhub_namespace.%s.name}-diag"
  target_resource_id         = azurerm_eventhub_namespace.%s.id
  log_analytics_workspace_id = var.log_analytics_workspace_id  # operator provides

  enabled_log {
    category = "ArchiveLogs"
  }
  enabled_log {
    category = "OperationalLogs"
  }
  enabled_log {
    category = "AutoScaleLogs"
  }
  enabled_log {
    category = "KafkaCoordinatorLogs"
  }
  enabled_log {
    category = "KafkaUserErrorLogs"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}
`, name, name, name)

	reasoning = "Event Hubs Namespaces without diagnostic settings have no per-namespace visibility into delivery health, capture status, or throughput unit utilization. The PR configures Microsoft.Insights/diagnosticSettings routing to a Log Analytics workspace with the 5 Event Hubs log categories (ArchiveLogs, OperationalLogs, AutoScaleLogs, KafkaCoordinatorLogs, KafkaUserErrorLogs) + AllMetrics. Decline if your team uses a non-Insights destination (custom capture pipeline, third-party SIEM connector) — the verdict learning loop records."

	return
}

// PickEventHubsCapturePattern emits the Terraform snippet for an
// eventhubs-capture-enable recommendation per event source tier
// slice 8 chunk 2 (v0.89.154, #796 Stream 193). Enables Capture on
// ONE event hub in the namespace (operator picks which during PR
// review based on which hub carries the durability-critical stream)
// per §8 of docs/proposals/event-source-tier-slice8.md.
//
// CRITICAL: the emitted snippet uses <hub_name> as the resource
// identifier — operators MUST replace this with the actual hub name
// during PR review. The reasoning text emphasizes that Squadron does
// not prescribe WHICH hub to enable Capture on; the operator picks
// based on per-hub durability requirements that Squadron cannot
// infer from the ARM API surface alone.
//
// Capture auto-archives events to Blob Storage or Azure Data Lake
// Storage at configurable intervals (default 5min / 300MB). Without
// Capture, events expire after the namespace's configured retention
// window (1 day default; 7 days max on Standard; 90 days max on
// Premium). The recommendation's framing is operator-prescriptive
// without auto-deciding which hub.
//
// row.ResourceTFName carries the NAMESPACE name when present; the
// hub name is left as <hub_name> for operator substitution.
func PickEventHubsCapturePattern(row RecommendationContext) (terraform, reasoning string) {
	_ = row.ResourceTFName // namespace name is operator context; emitted snippet targets a hub
	terraform = `resource "azurerm_eventhub" "<hub_name>" {
  # ... existing fields ...

  # NOTE: Squadron does NOT prescribe WHICH hub to enable Capture on.
  # Operator picks based on which hub carries durability-critical
  # streams. Replace <hub_name> with the chosen hub's Terraform
  # resource identifier.
  capture_description {
    enabled             = true
    encoding            = "Avro"
    interval_in_seconds = 300       # 5 minutes default
    size_limit_in_bytes = 314572800 # 300 MB default
    skip_empty_archives = true
    destination {
      name                = "EventHubArchive.AzureBlockBlob"
      storage_account_id  = var.capture_storage_account_id  # operator provides
      blob_container_name = "eventhub-capture"
      archive_name_format = "{Namespace}/{EventHub}/{PartitionId}/{Year}/{Month}/{Day}/{Hour}/{Minute}/{Second}"
    }
  }
}
`

	reasoning = "Event Hubs Namespaces with NO event hub having Capture enabled lose event content after the retention window expires (1 day default; 7 days max on Standard; 90 days max on Premium). The operator has no event-content audit trail beyond the retention window for incident postmortems. The PR enables Capture on ONE event hub in the namespace — OPERATOR PICKS WHICH during PR review based on which hub carries the durability-critical stream. Squadron does not prescribe the selection. Decline if your team has an out-of-band consumer pipeline doing archival (Databricks + Delta Lake ingestion, Stream Analytics persisting to its own destination) — the verdict learning loop records."

	return
}

// PickQueuesLoggingPattern emits the Terraform snippet for a
// queues-logging-enable recommendation per event source tier slice 9
// chunk 2 (v0.89.157, #799 Stream 196). Configures an OCI Logging
// service log routing the queue's delivery events to a log group per
// §8 of docs/proposals/event-source-tier-slice9.md.
//
// Slice 9 brings OCI to parity with AWS + Azure at 3 event source
// surfaces (Streaming + Notification Service + Queue Service). The
// Logging axis on Queue Service mirrors the slice 1 Streaming
// streaming-logging-enable and slice 7 ONS ons-logging-enable
// patterns exactly — same oci_logging_log resource shape, same
// configuration block. The only differences are service = "queue"
// and the resource type reference points at oci_queue_queue.
//
// row.ResourceTFName is the best-effort Terraform resource name the
// proposer extracted from the operator's repo. When empty, the
// snippet falls back to "<name>" so the operator can substitute the
// real queue name during review.
//
// The log_group_id reference is var.default_log_group_id —
// operator's existing log group is reused when set; when unset, the
// operator extends var declarations during PR review. This keeps the
// emitted Terraform compatible with operators who manage the log
// group destination outside Squadron's PR scope.
func PickQueuesLoggingPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "oci_logging_log" "%s_queue_log" {
  display_name = "${oci_queue_queue.%s.display_name}-delivery-log"
  log_group_id = var.default_log_group_id  # operator provides
  log_type     = "SERVICE"

  configuration {
    source {
      category    = "all"
      resource    = oci_queue_queue.%s.id
      service     = "queue"
      source_type = "OCISERVICE"
    }
    compartment_id = oci_queue_queue.%s.compartment_id
  }

  is_enabled         = true
  retention_duration = 30  # operator may tune
}
`, name, name, name, name)

	reasoning = "OCI Queues without OCI Logging configured have no audit trail for which messages were dequeued, processed, or sent to the DLQ — critical for postmortem analysis of consumer-side failures and poison-message investigation. When a message lands in the DLQ at 2am the operator has no record of which consumer attempted it — only that the DLQ count incremented. Mirrors the slice 1 streaming-logging-enable and slice 7 ons-logging-enable patterns. The PR configures an oci_logging_log routing queue delivery events to var.default_log_group_id; operators using a different log group can swap the variable. Decline if your team routes queue audit through a non-OCI-Logging destination (Cloud Guard custom recipe, OCI Streaming capture, third-party SIEM connector) — the verdict learning loop records."

	return
}

// PickPubSubLiteLoggingPattern emits the Terraform snippet for a
// pubsublite-logging-enable recommendation per event source tier
// slice 10 chunk 2 (v0.89.160, #802 Stream 199). Configures a Cloud
// Logging sink filtering on the topic's ID with destination defaulting
// to a BigQuery dataset via var.pubsublite_logging_dataset_id.
//
// Slice 10 CLOSES the cross-cloud event source widening pass at
// 3-3-3-3 / 12 surfaces. Pub/Sub Lite is GCP's partitioned-log
// primitive analogous to AWS Kinesis + Azure Event Hubs.
func PickPubSubLiteLoggingPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "google_logging_project_sink" "%s_lite_log_sink" {
  name        = "pubsublite-${google_pubsub_lite_topic.%s.name}-audit"
  destination = "bigquery.googleapis.com/projects/${var.project_id}/datasets/${var.pubsublite_logging_dataset_id}"

  filter = <<-EOT
    resource.type="pubsublite_topic"
    resource.labels.topic_id="${google_pubsub_lite_topic.%s.name}"
  EOT

  unique_writer_identity = true
}
`, name, name, name)

	reasoning = "Pub/Sub Lite topics without a Cloud Logging sink have no audit trail for publish failures, per-partition throughput exhaustion events, or reservation-related throttling — the failure modes unique to the Lite tier. Mirrors the slice 1 Pub/Sub pattern via the Cloud Logging sink primitive. The PR configures a google_logging_project_sink filtering on the topic's ID with destination defaulting to a BigQuery dataset (var.pubsublite_logging_dataset_id). Decline if your team routes Lite topic audit through a non-Cloud-Logging destination (Stackdriver custom exporter, third-party SIEM) — the verdict learning loop records."

	return
}

// PickPubSubLiteReservationPattern emits the Terraform snippet for a
// pubsublite-reservation-attach recommendation per event source tier
// slice 10 chunk 2 (v0.89.160, #802 Stream 199). Creates a NEW
// google_pubsub_lite_reservation resource AND updates the topic's
// reservation_config.throughput_reservation reference.
//
// CRITICAL: this recommendation creates a BILLABLE RESOURCE. The
// reasoning text emphasizes the cost implication so PR reviewers see
// it explicitly. Default sizing is conservative (4 publish + subscribe
// units) but the operator MUST validate against actual peak throughput
// before merging. This is the FIRST recommendation in the event source
// tier that creates a billable resource — prior kinds only configured
// Logging sinks or attached to existing resources. The decline path is
// load-bearing for operators who deliberately run below reservation
// breakeven.
func PickPubSubLiteReservationPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}

	terraform = fmt.Sprintf(`resource "google_pubsub_lite_reservation" "%s_reservation" {
  name    = "${google_pubsub_lite_topic.%s.name}-reservation"
  project = var.project_id
  region  = var.lite_region  # operator provides; must match topic zone

  # CONSERVATIVE DEFAULT: 4 publish + subscribe units. Operator MUST
  # tune throughput_capacity to match ACTUAL peak before merging.
  # Under-sized reservations re-create the throttling problem the
  # recommendation is meant to solve.
  throughput_capacity = 4
}

resource "google_pubsub_lite_topic" "%s" {
  # ... existing fields ...

  reservation_config {
    throughput_reservation = google_pubsub_lite_reservation.%s_reservation.name
  }
}
`, name, name, name, name)

	reasoning = "Pub/Sub Lite topics without a reservation attached are throttled to the bare minimum publish + subscribe throughput per partition — typically becoming a silent bottleneck under peak load. This PR CREATES a NEW google_pubsub_lite_reservation resource AND updates the topic to reference it. BILLABLE: the reservation is operator-incurred cost. Default sizing is conservative (4 publish + subscribe units) but the operator MUST validate against ACTUAL peak throughput before merging — under-sized reservations re-create the throttling problem the recommendation solves. Decline if your team intentionally runs Lite topics at the minimum-throughput floor because the topic is below the per-zone reservation breakeven — the verdict learning loop records."

	return
}
