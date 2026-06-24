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
