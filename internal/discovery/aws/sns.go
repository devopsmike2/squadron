// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// SNSSurface is the Surface discriminator string for AWS SNS topic
// snapshots. The proposer's recommendation-kind prefix routing switches
// on "sns-" → AWS to disambiguate from the slice 1 "eventbridge" surface
// and the UI's per-cloud Inventory tab keys off this when rendering
// rows. Slice 3 chunk 1 of the Event source tier arc (v0.89.138, #778
// Stream 176).
const SNSSurface = "sns"

// snsSourceTypeTopic is the SourceType discriminator string for SNS
// topics. Mirrors the per-cloud "bus" / "topic" / "queue" / "namespace"
// / "stream" SourceType convention documented on
// scanner.EventSourceInstanceSnapshot.
const snsSourceTypeTopic = "topic"

// snsDefaultRegion is the fallback region the SNS scanner uses when the
// supplied ScanScope carries no Regions and the scanner's own
// configured region list is empty. us-east-1 mirrors the EventBridge
// slice 1 single-region scan posture.
const snsDefaultRegion = "us-east-1"

// SNSDeliveryFeedbackProtocols enumerates the per-protocol delivery
// feedback role ARN attribute names SNS exposes via GetTopicAttributes.
// Squadron checks each one to determine the delivery logging axis per
// §3 of docs/proposals/event-source-tier-slice3.md.
//
// When ANY of these attributes is set on a topic, the topic has at
// least some per-protocol delivery status logging configured —
// HasLogAxis flips true. Six per-protocol pairs (success + failure) ×
// five protocols (HTTP / SQS / Lambda / Application / Firehose) cover
// the entire SNS delivery-feedback surface as of the SDK version this
// chunk imports.
var SNSDeliveryFeedbackProtocols = []string{
	"HTTPSuccessFeedbackRoleArn",
	"HTTPFailureFeedbackRoleArn",
	"SQSSuccessFeedbackRoleArn",
	"SQSFailureFeedbackRoleArn",
	"LambdaSuccessFeedbackRoleArn",
	"LambdaFailureFeedbackRoleArn",
	"ApplicationSuccessFeedbackRoleArn",
	"ApplicationFailureFeedbackRoleArn",
	"FirehoseSuccessFeedbackRoleArn",
	"FirehoseFailureFeedbackRoleArn",
}

// ScanSNSTopics walks the supplied region's SNS topics and returns the
// mapped event source snapshots. Slice 3 chunk 1 of the event-source-
// tier arc (v0.89.138, #778 Stream 176).
//
// Paginates ListTopics via NextToken; per-topic GetTopicAttributes fans
// out one call per topic to surface the SubscriptionsConfirmed +
// per-protocol delivery feedback attributes the detection axes key
// off.
//
// Detection per docs/proposals/event-source-tier-slice3.md §3:
//
//   - HasTraceAxis  ← SubscriptionsConfirmed > 0. A topic with at
//     least one confirmed subscription is the proxy for the "messages
//     actually flow downstream" audit signal — operators with zero
//     confirmed subs publish into the void; the audit-only
//     sns-subscriptions-attach recommendation (chunk 2) surfaces those
//     orphans.
//   - HasLogAxis   ← ANY of the per-protocol delivery feedback role
//     ARN attributes set (see SNSDeliveryFeedbackProtocols). The
//     per-protocol pair (success + failure) drives the
//     sns-delivery-logging-enable recommendation (chunk 2) when off
//     and the topic has at least one subscription.
//
// Per-topic GetTopicAttributes failures are non-fatal: the topic row
// still surfaces with its universal columns and a Detail
// "attribute_fetch_failed" marker, but both axes default to false. The
// list-pass failure is the only error a caller will see propagated up
// — bus + topic dispatcher (ScanEventSources) treats that as one
// surface's contribution to the partial-scan posture and lets the
// EventBridge result still surface.
//
// IAM contract per docs/proposals/event-source-tier-slice3.md §9 + §12:
// sns:ListTopics + sns:GetTopicAttributes. Both read-only; Squadron
// never calls any SNS mutation API.
func (s *Scanner) ScanSNSTopics(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	region := snsDefaultRegion
	if len(scope.Regions) > 0 && scope.Regions[0] != "" {
		region = scope.Regions[0]
	}
	factory, err := s.ensureFactory(ctx, region)
	if err != nil {
		return nil, err
	}
	accountID := scope.AccountID
	if accountID == "" {
		accountID = s.accountID
	}
	return s.scanRegionSNS(ctx, factory, region, accountID)
}

// scanRegionSNS runs the per-region list + per-topic describe pass.
// Extracted from ScanSNSTopics so tests can drive the inner loop
// against a fakeFactory without the ensureFactory indirection.
//
// Pagination follows out.NextToken; an empty token signals "no more
// pages" per the AWS SDK convention. A nil token is the sentinel "this
// is the first page" used to skip setting the input's NextToken on the
// initial call.
//
// Per-topic GetTopicAttributes failures are caught at the inner err
// check and the topic surfaces with its universal columns plus a
// Detail "attribute_fetch_failed" marker. This matches the
// partial-scan posture documented on §5 of the design doc.
func (s *Scanner) scanRegionSNS(ctx context.Context, factory ClientFactory, region, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	client, err := factory.SNS(ctx, region)
	if err != nil {
		return nil, err
	}
	if client == nil {
		// Graceful: matches the existing scanner posture for unwired
		// clients on the validation path.
		return nil, nil
	}
	var (
		out       []scanner.EventSourceInstanceSnapshot
		nextToken *string
	)
	for {
		in := &sns.ListTopicsInput{}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		var listOut *sns.ListTopicsOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			listOut, e = client.ListTopics(ctx, in)
			return e
		})
		if callErr != nil {
			return out, fmt.Errorf("list sns topics: %w", callErr)
		}
		for _, topic := range listOut.Topics {
			snap := s.describeSNSTopic(ctx, client, topic, accountID, region)
			out = append(out, snap)
		}
		if listOut.NextToken == nil || *listOut.NextToken == "" {
			break
		}
		nextToken = listOut.NextToken
	}
	return out, nil
}

// describeSNSTopic fetches the topic's attributes and folds the result
// into a fully-populated EventSourceInstanceSnapshot. Extracted as a
// standalone helper so the per-axis detection logic is independently
// testable: the slice 3 acceptance tests hit buildSNSSnapshot directly
// with fixture attribute maps, asserting the HasTraceAxis / HasLogAxis
// outcome without spinning up a full scanner.
//
// On a GetTopicAttributes failure, returns the partial-snapshot
// produced by buildSNSPartialSnapshot — the row surfaces with its
// universal columns plus Detail["attribute_fetch_failed"] = true so
// the operator sees the inventory even when the per-topic walk fails.
// Both axes default to false in that path; the chunk-2 recommendation
// engine declines to fire against a row marked with the failure flag
// (the operator can fix the IAM gap and re-scan).
func (s *Scanner) describeSNSTopic(ctx context.Context, client SNSClient, topic snstypes.Topic, accountID, region string) scanner.EventSourceInstanceSnapshot {
	arn := awssdk.ToString(topic.TopicArn)
	attrsOut, err := client.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{
		TopicArn: topic.TopicArn,
	})
	if err != nil {
		return buildSNSPartialSnapshot(accountID, region, arn)
	}
	return buildSNSSnapshot(accountID, region, arn, attrsOut.Attributes)
}

// buildSNSPartialSnapshot returns the bare-info row a topic surfaces
// with when GetTopicAttributes failed. The Detail flag carries the
// reason so the proposer's chunk-2 recommendation engine can decline
// to fire against it (the operator should fix the IAM gap and re-scan
// before Squadron decides whether the topic is properly configured).
func buildSNSPartialSnapshot(accountID, region, arn string) scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		Provider:     string(credstore.ProviderAWS),
		Surface:      SNSSurface,
		AccountID:    accountID,
		Region:       region,
		ResourceName: snsTopicNameFromARN(arn),
		ResourceARN:  arn,
		SourceType:   snsSourceTypeTopic,
		Detail: map[string]any{
			"attribute_fetch_failed": true,
		},
	}
}

// buildSNSSnapshot translates a GetTopicAttributes response into a
// snapshot row per the §3 detection axes. The attrs map mirrors the
// AWS SDK attribute names verbatim — see SNSDeliveryFeedbackProtocols
// for the canonical per-protocol delivery feedback role ARN attribute
// keys.
//
// Axis rules:
//
//  1. HasTraceAxis ← SubscriptionsConfirmed integer > 0. The
//     "subscriptions_confirmed" Detail key carries the parsed integer
//     so the proposer's chunk-2 reasoning text can quote it. A
//     missing or non-integer SubscriptionsConfirmed attribute leaves
//     the axis false (defensive — SNS guarantees the field per the
//     API contract, but the parse guards against future enum changes).
//  2. HasLogAxis ← ANY of the SNSDeliveryFeedbackProtocols attribute
//     keys present and non-empty. The proposer's chunk-2
//     sns-delivery-logging-enable recommendation fires only when the
//     topic also has at least one subscription — surfacing the axis
//     as detection state rather than gating it in the scanner keeps
//     the recommendation engine the single owner of the firing rule.
//
// Detail-only axes (informational; do NOT gate either detection axis):
//
//   - kms_master_key_id ← KmsMasterKeyId attribute when set (per §3
//     informational encryption-at-rest signal).
//   - fifo_topic ← FifoTopic == "true". The accompanying
//     content_based_deduplication flag only surfaces when both
//     FifoTopic and ContentBasedDeduplication are "true" (per §3
//     informational FIFO-dedup signal).
func buildSNSSnapshot(accountID, region, arn string, attrs map[string]string) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:     string(credstore.ProviderAWS),
		Surface:      SNSSurface,
		AccountID:    accountID,
		Region:       region,
		ResourceName: snsTopicNameFromARN(arn),
		ResourceARN:  arn,
		SourceType:   snsSourceTypeTopic,
		Detail:       map[string]any{},
	}

	// Axis 1: has_delivery_subscriptions — SubscriptionsConfirmed > 0.
	if confStr, ok := attrs["SubscriptionsConfirmed"]; ok {
		if n, err := strconv.Atoi(confStr); err == nil {
			snap.Detail["subscriptions_confirmed"] = n
			snap.HasTraceAxis = n > 0
		}
	}

	// Axis 2: delivery status logging — ANY per-protocol feedback role
	// configured. Cheap loop; SNS exposes at most ten keys here.
	for _, protocolAttr := range SNSDeliveryFeedbackProtocols {
		if v := attrs[protocolAttr]; v != "" {
			snap.HasLogAxis = true
			break
		}
	}

	// Detail-only axes: encryption-at-rest + FIFO content dedup.
	if kms := attrs["KmsMasterKeyId"]; kms != "" {
		snap.Detail["kms_master_key_id"] = kms
	}
	if attrs["FifoTopic"] == "true" {
		snap.Detail["fifo_topic"] = true
		if attrs["ContentBasedDeduplication"] == "true" {
			snap.Detail["content_based_deduplication"] = true
		}
	}

	return snap
}

// snsTopicNameFromARN extracts the topic name from an SNS ARN.
//
//	arn:aws:sns:us-east-1:123456789012:my-topic-name → "my-topic-name"
//
// Defensive against empty / malformed ARNs: returns the input unchanged
// when the ARN has no colon segments. SNS ARNs are canonical 6-segment
// strings; the last segment is the topic name.
func snsTopicNameFromARN(arn string) string {
	if arn == "" {
		return ""
	}
	parts := strings.Split(arn, ":")
	if len(parts) == 0 {
		return arn
	}
	return parts[len(parts)-1]
}
