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

  enabled_log {
    category = "PublishFailures"
  }
  enabled_log {
    category = "PublishSuccess"
  }
  enabled_log {
    category = "DeliveryFailures"
  }
  enabled_log {
    category = "DeliverySuccess"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}
`, name, name, name)

	reasoning = "Event Grid Topics without diagnostic settings have no per-event delivery audit trail. The PR configures Microsoft.Insights/diagnosticSettings routing to a Log Analytics workspace with the 4 Event Grid log categories (PublishFailures, PublishSuccess, DeliveryFailures, DeliverySuccess) + AllMetrics. Decline if your team uses a non-Insights destination — the verdict learning loop records."

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
