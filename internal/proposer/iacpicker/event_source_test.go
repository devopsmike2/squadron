// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// TestPickSNSDeliveryLoggingPattern_IncludesAssumeRolePolicy — event
// source tier slice 3 chunk 2 (v0.89.139, #779 Stream 177). The
// Terraform snippet MUST include the aws_iam_policy_document
// "sns_delivery_logging_<name>_assume" data source allowing the
// sns.amazonaws.com service to AssumeRole. SNS delivery status
// logging needs an IAM role attached to the topic via per-protocol
// success/failure feedback role ARNs; that role's assume policy must
// trust SNS as the principal per §8 of the design doc
// (docs/proposals/event-source-tier-slice3.md).
func TestPickSNSDeliveryLoggingPattern_IncludesAssumeRolePolicy(t *testing.T) {
	tf, _ := PickSNSDeliveryLoggingPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "order_events",
	})
	if !strings.Contains(tf, `data "aws_iam_policy_document" "sns_delivery_logging_order_events_assume"`) {
		t.Errorf("expected aws_iam_policy_document assume data source for order_events, got:\n%s", tf)
	}
	if !strings.Contains(tf, `identifiers = ["sns.amazonaws.com"]`) {
		t.Errorf("expected sns.amazonaws.com as the trusted principal, got:\n%s", tf)
	}
	if !strings.Contains(tf, `actions = ["sts:AssumeRole"]`) {
		t.Errorf("expected sts:AssumeRole action, got:\n%s", tf)
	}
	if !strings.Contains(tf, `resource "aws_iam_role" "sns_delivery_logging_order_events"`) {
		t.Errorf("expected aws_iam_role resource block for order_events, got:\n%s", tf)
	}
	if !strings.Contains(tf, `policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonSNSRole"`) {
		t.Errorf("expected AmazonSNSRole policy attachment, got:\n%s", tf)
	}
}

// TestPickSNSDeliveryLoggingPattern_IncludesAllFiveProtocolFeedbackRoles
// — the Terraform snippet MUST attach a success + failure feedback
// role ARN on the aws_sns_topic resource for ALL 5 SNS delivery
// protocols (http / sqs / lambda / application / firehose) per §8 of
// the design doc. The picker emits all 5 unconditionally so the
// operator can prune the protocols their topic doesn't use during PR
// review.
func TestPickSNSDeliveryLoggingPattern_IncludesAllFiveProtocolFeedbackRoles(t *testing.T) {
	tf, _ := PickSNSDeliveryLoggingPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "events",
	})
	for _, attr := range []string{
		"http_success_feedback_role_arn",
		"http_failure_feedback_role_arn",
		"sqs_success_feedback_role_arn",
		"sqs_failure_feedback_role_arn",
		"lambda_success_feedback_role_arn",
		"lambda_failure_feedback_role_arn",
		"application_success_feedback_role_arn",
		"application_failure_feedback_role_arn",
		"firehose_success_feedback_role_arn",
		"firehose_failure_feedback_role_arn",
	} {
		if !strings.Contains(tf, attr) {
			t.Errorf("expected per-protocol feedback role attribute %q in the snippet, got:\n%s", attr, tf)
		}
	}
	// The aws_sns_topic block itself must be present.
	if !strings.Contains(tf, `resource "aws_sns_topic" "events"`) {
		t.Errorf("expected aws_sns_topic resource block for events, got:\n%s", tf)
	}
}

// TestPickSNSDeliveryLoggingPattern_ReasoningMentionsDeclinePath — the
// reasoning string MUST surface the slice 1 honest-framing pattern:
// operators using non-CloudWatch destinations for delivery audit
// (custom Lambda processor, SNS-to-Datadog integration, etc.) should
// decline. The verdict learning loop records the decline so the
// per-resource exclusion table can suppress repeat drafts.
func TestPickSNSDeliveryLoggingPattern_ReasoningMentionsDeclinePath(t *testing.T) {
	_, reasoning := PickSNSDeliveryLoggingPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "topic_a",
	})
	for _, token := range []string{
		"Decline",
		"non-CloudWatch destination",
		"verdict learning loop",
		"per-protocol delivery feedback",
	} {
		if !strings.Contains(reasoning, token) {
			t.Errorf("reasoning missing %q, got: %s", token, reasoning)
		}
	}
}

// TestPickSNSDeliveryLoggingPattern_EmptyResourceName_FallsBack — when
// the proposer cannot recover the Terraform resource name from the
// operator's repo, the snippet falls back to "<name>" so the operator
// can substitute it during review. Mirrors pickAWSDB / pickAWSK8s in
// picker.go and PickResourceManagerLoggingPattern in orchestration.go.
func TestPickSNSDeliveryLoggingPattern_EmptyResourceName_FallsBack(t *testing.T) {
	tf, _ := PickSNSDeliveryLoggingPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "",
	})
	if !strings.Contains(tf, "sns_delivery_logging_<name>") {
		t.Errorf("expected fallback sns_delivery_logging_<name> label, got:\n%s", tf)
	}
	if !strings.Contains(tf, `resource "aws_sns_topic" "<name>"`) {
		t.Errorf("expected fallback aws_sns_topic <name> block, got:\n%s", tf)
	}
}
