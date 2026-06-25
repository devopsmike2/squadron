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

// --- Event source tier slice 4 chunk 2 (v0.89.142, #782 Stream 180) -

// TestPickSQSRedrivePolicyPattern_IncludesDLQResource — event source
// tier slice 4 chunk 2. The Terraform snippet MUST include a separate
// aws_sqs_queue resource block for the dead-letter queue (named
// "<source>_dlq") with the 14-day (1209600s) message retention and
// the alias/aws/sqs KMS master key per §8 of the design doc
// (docs/proposals/event-source-tier-slice4.md). The DLQ is what
// captures messages once consumer retries are exhausted; without it,
// the redrive_policy on the source queue has nowhere to send them.
func TestPickSQSRedrivePolicyPattern_IncludesDLQResource(t *testing.T) {
	tf, _ := PickSQSRedrivePolicyPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "order_processing",
	})
	if !strings.Contains(tf, `resource "aws_sqs_queue" "order_processing_dlq"`) {
		t.Errorf("expected aws_sqs_queue dead-letter queue resource block for order_processing, got:\n%s", tf)
	}
	if !strings.Contains(tf, "message_retention_seconds  = 1209600") {
		t.Errorf("expected 14-day retention (1209600s) on the DLQ, got:\n%s", tf)
	}
	if !strings.Contains(tf, `kms_master_key_id          = "alias/aws/sqs"`) {
		t.Errorf("expected alias/aws/sqs KMS master key on the DLQ, got:\n%s", tf)
	}
	// The source queue's redrive policy must reference the DLQ's ARN.
	if !strings.Contains(tf, "deadLetterTargetArn = aws_sqs_queue.order_processing_dlq.arn") {
		t.Errorf("expected source queue's redrive policy to reference the DLQ arn, got:\n%s", tf)
	}
}

// TestPickSQSRedrivePolicyPattern_IncludesRedrivePolicyJSONEncode — the
// Terraform snippet MUST emit the redrive_policy attribute on the
// source queue using jsonencode({...}) per §8 of the design doc. The
// jsonencode wrapper is the standard Terraform shape for the AWS
// provider's aws_sqs_queue.redrive_policy attribute — it expects a
// JSON-encoded string, NOT a native HCL object. The maxReceiveCount
// defaults to 5 (operator-tunable per the comment).
func TestPickSQSRedrivePolicyPattern_IncludesRedrivePolicyJSONEncode(t *testing.T) {
	tf, _ := PickSQSRedrivePolicyPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "events",
	})
	if !strings.Contains(tf, "redrive_policy = jsonencode({") {
		t.Errorf("expected redrive_policy with jsonencode wrapper, got:\n%s", tf)
	}
	if !strings.Contains(tf, "maxReceiveCount     = 5") {
		t.Errorf("expected maxReceiveCount = 5 default in the redrive policy, got:\n%s", tf)
	}
	// The aws_sqs_queue source block itself must be present.
	if !strings.Contains(tf, `resource "aws_sqs_queue" "events"`) {
		t.Errorf("expected source aws_sqs_queue resource block for events, got:\n%s", tf)
	}
}

// TestPickSQSRedrivePolicyPattern_ReasoningMentionsDeclinePath — the
// reasoning string MUST surface the slice 4 honest-framing pattern:
// operators using a custom retry coordinator (Step Functions retry
// handler, EventBridge Pipes with error handling, etc.) should
// decline. The verdict learning loop records the decline so the
// per-resource exclusion table can suppress repeat drafts. The
// reasoning also flags the framing that motivates the recommendation
// — silent message drop is the single most common AWS messaging
// production failure.
func TestPickSQSRedrivePolicyPattern_ReasoningMentionsDeclinePath(t *testing.T) {
	_, reasoning := PickSQSRedrivePolicyPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "topic_a",
	})
	for _, token := range []string{
		"Decline",
		"custom retry coordinator",
		"Step Functions",
		"EventBridge Pipes",
		"verdict learning loop",
		"single most common AWS messaging production failure",
	} {
		if !strings.Contains(reasoning, token) {
			t.Errorf("reasoning missing %q, got: %s", token, reasoning)
		}
	}
}

// TestPickSQSRedrivePolicyPattern_EmptyResourceName_FallsBack — when
// the proposer cannot recover the Terraform resource name from the
// operator's repo, the snippet falls back to "<name>" so the operator
// can substitute it during review. Mirrors the slice 3 chunk 2
// PickSNSDeliveryLoggingPattern fallback shape.
func TestPickSQSRedrivePolicyPattern_EmptyResourceName_FallsBack(t *testing.T) {
	tf, _ := PickSQSRedrivePolicyPattern(RecommendationContext{
		Provider:       "aws",
		ResourceTFName: "",
	})
	if !strings.Contains(tf, `resource "aws_sqs_queue" "<name>_dlq"`) {
		t.Errorf("expected fallback aws_sqs_queue <name>_dlq DLQ block, got:\n%s", tf)
	}
	if !strings.Contains(tf, `resource "aws_sqs_queue" "<name>"`) {
		t.Errorf("expected fallback aws_sqs_queue <name> source block, got:\n%s", tf)
	}
	if !strings.Contains(tf, "deadLetterTargetArn = aws_sqs_queue.<name>_dlq.arn") {
		t.Errorf("expected fallback redrive policy DLQ reference, got:\n%s", tf)
	}
}

// --- Event source tier slice 5 chunk 2 (v0.89.145, #785 Stream 183) -

// TestPickCloudTasksRetryPolicyPattern_IncludesRetryConfigBlock — event
// source tier slice 5 chunk 2. The Terraform snippet MUST emit the
// retry_config nested block on the google_cloud_tasks_queue resource
// per §8 of the design doc (docs/proposals/event-source-tier-slice5.md).
// Without retry_config, the Cloud Tasks queue silently drops tasks on
// HTTP target failure — the GCP equivalent of an SQS queue without a
// redrive policy.
func TestPickCloudTasksRetryPolicyPattern_IncludesRetryConfigBlock(t *testing.T) {
	tf, _ := PickCloudTasksRetryPolicyPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "webhook_dispatch",
	})
	if !strings.Contains(tf, `resource "google_cloud_tasks_queue" "webhook_dispatch"`) {
		t.Errorf("expected google_cloud_tasks_queue resource block for webhook_dispatch, got:\n%s", tf)
	}
	if !strings.Contains(tf, "retry_config {") {
		t.Errorf("expected retry_config nested block, got:\n%s", tf)
	}
	for _, attr := range []string{"min_backoff", "max_backoff", "max_doublings", "max_retry_duration"} {
		if !strings.Contains(tf, attr) {
			t.Errorf("expected %q in retry_config, got:\n%s", attr, tf)
		}
	}
}

// TestPickCloudTasksRetryPolicyPattern_MaxAttemptsDefaultIs5 — the
// snippet MUST set max_attempts = 5 as the default ("retry a few times
// with exponential backoff before giving up" semantics). Operators
// tune based on consumer retry tolerance per §8 of the design doc.
func TestPickCloudTasksRetryPolicyPattern_MaxAttemptsDefaultIs5(t *testing.T) {
	tf, _ := PickCloudTasksRetryPolicyPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "tasks",
	})
	if !strings.Contains(tf, "max_attempts       = 5") {
		t.Errorf("expected max_attempts = 5 default, got:\n%s", tf)
	}
}

// TestPickCloudTasksRetryPolicyPattern_ReasoningMentionsDeclinePath —
// the reasoning string MUST surface the slice 5 honest-framing pattern:
// operators intentionally configuring fire-and-forget semantics
// (single attempt, drop on failure) should decline. The verdict
// learning loop records the decline so the per-resource exclusion
// table can suppress repeat drafts.
func TestPickCloudTasksRetryPolicyPattern_ReasoningMentionsDeclinePath(t *testing.T) {
	_, reasoning := PickCloudTasksRetryPolicyPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "tasks",
	})
	for _, token := range []string{
		"Decline",
		"fire-and-forget",
		"verdict learning loop",
		"exponential backoff",
	} {
		if !strings.Contains(reasoning, token) {
			t.Errorf("reasoning missing %q, got: %s", token, reasoning)
		}
	}
}

// TestPickCloudTasksRetryPolicyPattern_EmptyResourceName_FallsBack —
// when the proposer cannot recover the Terraform resource name from
// the operator's repo, the snippet falls back to "<name>" so the
// operator can substitute it during review.
func TestPickCloudTasksRetryPolicyPattern_EmptyResourceName_FallsBack(t *testing.T) {
	tf, _ := PickCloudTasksRetryPolicyPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "",
	})
	if !strings.Contains(tf, `resource "google_cloud_tasks_queue" "<name>"`) {
		t.Errorf("expected fallback google_cloud_tasks_queue <name> block, got:\n%s", tf)
	}
}

// TestPickCloudTasksLoggingPattern_IncludesStackdriverLoggingConfig —
// event source tier slice 5 chunk 2. The Terraform snippet MUST emit
// the stackdriver_logging_config nested block on the
// google_cloud_tasks_queue resource per §8 of the design doc. Without
// Stackdriver Logging the operator has no per-task delivery audit
// trail.
func TestPickCloudTasksLoggingPattern_IncludesStackdriverLoggingConfig(t *testing.T) {
	tf, _ := PickCloudTasksLoggingPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "tasks",
	})
	if !strings.Contains(tf, `resource "google_cloud_tasks_queue" "tasks"`) {
		t.Errorf("expected google_cloud_tasks_queue resource block for tasks, got:\n%s", tf)
	}
	if !strings.Contains(tf, "stackdriver_logging_config {") {
		t.Errorf("expected stackdriver_logging_config nested block, got:\n%s", tf)
	}
}

// TestPickCloudTasksLoggingPattern_SamplingRatioDefaultIs1 — the
// snippet MUST set sampling_ratio = 1.0 (full sampling) as the
// default. Operators tune downward for very-high-throughput queues
// where full sampling is expensive.
func TestPickCloudTasksLoggingPattern_SamplingRatioDefaultIs1(t *testing.T) {
	tf, _ := PickCloudTasksLoggingPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "tasks",
	})
	if !strings.Contains(tf, "sampling_ratio = 1.0") {
		t.Errorf("expected sampling_ratio = 1.0 default, got:\n%s", tf)
	}
}

// TestPickCloudTasksLoggingPattern_ReasoningMentionsDeclinePath — the
// reasoning string MUST surface the slice 5 honest-framing pattern:
// operators using a non-Stackdriver destination for task audit (custom
// HTTP logger sidecar, etc.) should decline. The verdict learning loop
// records.
func TestPickCloudTasksLoggingPattern_ReasoningMentionsDeclinePath(t *testing.T) {
	_, reasoning := PickCloudTasksLoggingPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "tasks",
	})
	for _, token := range []string{
		"Decline",
		"non-Stackdriver destination",
		"verdict learning loop",
		"full sampling",
	} {
		if !strings.Contains(reasoning, token) {
			t.Errorf("reasoning missing %q, got: %s", token, reasoning)
		}
	}
}

// TestPickCloudTasksLoggingPattern_EmptyResourceName_FallsBack — when
// the proposer cannot recover the Terraform resource name, the snippet
// falls back to "<name>".
func TestPickCloudTasksLoggingPattern_EmptyResourceName_FallsBack(t *testing.T) {
	tf, _ := PickCloudTasksLoggingPattern(RecommendationContext{
		Provider:       "gcp",
		ResourceTFName: "",
	})
	if !strings.Contains(tf, `resource "google_cloud_tasks_queue" "<name>"`) {
		t.Errorf("expected fallback google_cloud_tasks_queue <name> block, got:\n%s", tf)
	}
}

// --- Event source tier slice 6 chunk 2 (v0.89.148, #788 Stream 186) -

// TestPickEventGridDiagnosticsPattern_IncludesDiagnosticSettingResource
// — event source tier slice 6 chunk 2. The Terraform snippet MUST
// emit an azurerm_monitor_diagnostic_setting resource block targeting
// the Event Grid topic per §8 of the design doc
// (docs/proposals/event-source-tier-slice6.md). Mirrors the slice 1
// Service Bus diagnostic settings pattern.
func TestPickEventGridDiagnosticsPattern_IncludesDiagnosticSettingResource(t *testing.T) {
	tf, _ := PickEventGridDiagnosticsPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "order_events",
	})
	if !strings.Contains(tf, `resource "azurerm_monitor_diagnostic_setting" "order_events_diag"`) {
		t.Errorf("expected azurerm_monitor_diagnostic_setting resource block for order_events, got:\n%s", tf)
	}
	if !strings.Contains(tf, "target_resource_id         = azurerm_eventgrid_topic.order_events.id") {
		t.Errorf("expected target_resource_id pointing to the azurerm_eventgrid_topic, got:\n%s", tf)
	}
	if !strings.Contains(tf, "log_analytics_workspace_id = var.log_analytics_workspace_id") {
		t.Errorf("expected log_analytics_workspace_id wired through var.log_analytics_workspace_id, got:\n%s", tf)
	}
}

// TestPickEventGridDiagnosticsPattern_IncludesAllFourLogCategories — the
// Terraform snippet MUST include all 4 enabled_log categories Event
// Grid supports (PublishFailures, PublishSuccess, DeliveryFailures,
// DeliverySuccess) plus the AllMetrics metric block per §8 of the
// design doc. The picker emits all 4 unconditionally so the operator
// gets the complete delivery audit trail.
func TestPickEventGridDiagnosticsPattern_IncludesAllFourLogCategories(t *testing.T) {
	tf, _ := PickEventGridDiagnosticsPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "events",
	})
	for _, cat := range []string{
		`category = "PublishFailures"`,
		`category = "PublishSuccess"`,
		`category = "DeliveryFailures"`,
		`category = "DeliverySuccess"`,
		`category = "AllMetrics"`,
	} {
		if !strings.Contains(tf, cat) {
			t.Errorf("expected category line %q in the snippet, got:\n%s", cat, tf)
		}
	}
}

// TestPickEventGridDiagnosticsPattern_ReasoningMentionsDeclinePath —
// the reasoning string MUST surface the slice 6 honest-framing pattern:
// operators using a non-Insights destination (custom webhook capture,
// etc.) should decline. The verdict learning loop records.
func TestPickEventGridDiagnosticsPattern_ReasoningMentionsDeclinePath(t *testing.T) {
	_, reasoning := PickEventGridDiagnosticsPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "topic_a",
	})
	for _, token := range []string{
		"Decline",
		"non-Insights destination",
		"verdict learning loop",
		"per-event delivery audit",
	} {
		if !strings.Contains(reasoning, token) {
			t.Errorf("reasoning missing %q, got: %s", token, reasoning)
		}
	}
}

// TestPickEventGridDiagnosticsPattern_EmptyResourceName_FallsBack — when
// the proposer cannot recover the Terraform resource name, the snippet
// falls back to "<name>".
func TestPickEventGridDiagnosticsPattern_EmptyResourceName_FallsBack(t *testing.T) {
	tf, _ := PickEventGridDiagnosticsPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "",
	})
	if !strings.Contains(tf, `resource "azurerm_monitor_diagnostic_setting" "<name>_diag"`) {
		t.Errorf("expected fallback azurerm_monitor_diagnostic_setting <name>_diag block, got:\n%s", tf)
	}
}

// TestPickEventGridCloudEventSchemaPattern_SetsInputSchemaToV1 — event
// source tier slice 6 chunk 2. The Terraform snippet MUST set
// input_schema = "CloudEventSchemaV1_0" on the azurerm_eventgrid_topic
// resource per §8 of the design doc. CloudEvents 1.0 is the W3C
// standard format that carries the distributed tracing extension
// (traceparent).
func TestPickEventGridCloudEventSchemaPattern_SetsInputSchemaToV1(t *testing.T) {
	tf, _ := PickEventGridCloudEventSchemaPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "orders",
	})
	if !strings.Contains(tf, `resource "azurerm_eventgrid_topic" "orders"`) {
		t.Errorf("expected azurerm_eventgrid_topic resource block for orders, got:\n%s", tf)
	}
	if !strings.Contains(tf, `input_schema = "CloudEventSchemaV1_0"`) {
		t.Errorf("expected input_schema = \"CloudEventSchemaV1_0\", got:\n%s", tf)
	}
}

// TestPickEventGridCloudEventSchemaPattern_TerraformIncludesBreakingChangeWarning
// — the Terraform snippet MUST include an inline WARNING comment
// flagging the BREAKING CHANGE for existing subscribers per §8 of the
// design doc. The comment is load-bearing — the operator's PR review
// must catch the breakage risk even when the reasoning text isn't
// surfaced.
func TestPickEventGridCloudEventSchemaPattern_TerraformIncludesBreakingChangeWarning(t *testing.T) {
	tf, _ := PickEventGridCloudEventSchemaPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "orders",
	})
	for _, token := range []string{
		"WARNING",
		"BREAKING CHANGE",
		"existing subscribers",
		"Coordinate before merging",
	} {
		if !strings.Contains(tf, token) {
			t.Errorf("Terraform snippet missing breaking-change warning token %q, got:\n%s", token, tf)
		}
	}
}

// TestPickEventGridCloudEventSchemaPattern_ReasoningMentionsCoordinationBeforeMerge
// — the reasoning string MUST emphasize coordination with subscribers
// before merging per §8 of the design doc. Squadron drafts the PR but
// the operator's review catches the breakage risk.
func TestPickEventGridCloudEventSchemaPattern_ReasoningMentionsCoordinationBeforeMerge(t *testing.T) {
	_, reasoning := PickEventGridCloudEventSchemaPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "orders",
	})
	for _, token := range []string{
		"BREAKING CHANGE",
		"existing subscribers",
		"Coordinate before merging",
		"CloudEventSchemaV1_0",
		"traceparent",
		"Decline",
	} {
		if !strings.Contains(reasoning, token) {
			t.Errorf("reasoning missing %q, got: %s", token, reasoning)
		}
	}
}

// TestPickEventGridCloudEventSchemaPattern_EmptyResourceName_FallsBack
// — when the proposer cannot recover the Terraform resource name, the
// snippet falls back to "<name>".
func TestPickEventGridCloudEventSchemaPattern_EmptyResourceName_FallsBack(t *testing.T) {
	tf, _ := PickEventGridCloudEventSchemaPattern(RecommendationContext{
		Provider:       "azure",
		ResourceTFName: "",
	})
	if !strings.Contains(tf, `resource "azurerm_eventgrid_topic" "<name>"`) {
		t.Errorf("expected fallback azurerm_eventgrid_topic <name> block, got:\n%s", tf)
	}
}
