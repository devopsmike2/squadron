// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// SQSSurface identifies SQS queues in the EventSourceInstanceSnapshot
// Surface field. The proposer's recommendation-kind prefix routing
// switches on "sqs-" → AWS to disambiguate from the slice 1
// "eventbridge" surface and the slice 3 "sns" surface; the per-cloud
// Inventory tab keys off this when rendering rows. Slice 4 chunk 1 of
// the Event source tier arc (v0.89.141, #781 Stream 179).
const SQSSurface = "sqs"

// sqsSourceTypeQueue is the SourceType discriminator string for SQS
// queues. Mirrors the per-cloud "bus" / "topic" / "queue" / "namespace"
// / "stream" SourceType convention documented on
// scanner.EventSourceInstanceSnapshot.
const sqsSourceTypeQueue = "queue"

// sqsDefaultRegion is the fallback region the SQS scanner uses when the
// supplied ScanScope carries no Regions. us-east-1 mirrors the
// EventBridge slice 1 + SNS slice 3 single-region scan posture.
const sqsDefaultRegion = "us-east-1"

// SQSRedrivePolicyAttr is the SQS attribute name that holds the
// JSON-encoded redrive policy. Squadron parses this to extract the
// deadLetterTargetArn per §3 of docs/proposals/event-source-tier-slice4.md.
const SQSRedrivePolicyAttr = "RedrivePolicy"

// SQSQueueArnAttr is the SQS attribute name returned by GetQueueAttributes
// that carries the queue's ARN. Squadron uses this in the second pass
// to resolve DLQ references — the deadLetterTargetArn parsed out of
// RedrivePolicy is matched against the set of ARNs collected from the
// first-pass walk to detect "DLQ reachable in same account+region".
const SQSQueueArnAttr = "QueueArn"

// SQSKmsMasterKeyIdAttr signals at-rest encryption — informational only
// per §3 (not gated by either axis).
const SQSKmsMasterKeyIdAttr = "KmsMasterKeyId"

// SQSFifoQueueAttr is "true" when the queue is FIFO.
const SQSFifoQueueAttr = "FifoQueue"

// SQSContentBasedDeduplicationAttr is "true" when FIFO content-based
// deduplication is on.
const SQSContentBasedDeduplicationAttr = "ContentBasedDeduplication"

// redrivePolicyShape is the wire shape Squadron unmarshals from the
// RedrivePolicy attribute (which SQS returns as a JSON-encoded string
// alongside the rest of the attribute map). Only the two fields the
// detection axes consume are captured; AWS may add more keys in the
// future and the decoder ignores them.
type redrivePolicyShape struct {
	DeadLetterTargetArn string `json:"deadLetterTargetArn"`
	MaxReceiveCount     int    `json:"maxReceiveCount"`
}

// SQSClient is the narrow AWS SQS surface the event source scanner
// depends on. Added in v0.89.141 (#781 Stream 179, slice 4 chunk 1 of
// the Event source tier arc — AWS SQS as the third AWS event source
// surface alongside EventBridge + SNS). The real *sqs.Client satisfies
// it.
//
// The two methods cover the slice 4 chunk 1 detection contract:
// ListQueues surfaces every queue the assumed-role principal can see
// in a region (paginated via NextToken); GetQueueAttributes returns
// the per-queue attribute map the scanner reads for the RedrivePolicy
// (HasTraceAxis) + DLQ reachability (HasLogAxis) per
// docs/proposals/event-source-tier-slice4.md §3. Both APIs are
// read-only.
//
// IAM contract per docs/proposals/event-source-tier-slice4.md §12:
// sqs:ListQueues + sqs:GetQueueAttributes. Both read-only. Squadron
// does NOT call any SQS mutation API.
type SQSClient interface {
	ListQueues(ctx context.Context, params *sqs.ListQueuesInput, optFns ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error)
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

// queueAttributes carries the data the scanner needs from a single
// GetQueueAttributes call — the ARN for the second-pass DLQ
// resolution + the parsed redrive policy + miscellaneous detail. The
// failed flag flips when the per-queue attribute fetch failed so the
// snapshot builder can emit the universal columns plus the
// attribute_fetch_failed Detail marker without re-issuing the call.
type queueAttributes struct {
	URL              string
	ARN              string
	Attributes       map[string]string
	RedrivePolicy    *redrivePolicyShape
	HasRedrivePolicy bool
	Failed           bool
}

// ScanSQSQueues lists SQS queues in the supplied region and for each
// queue fetches its attributes. Performs a TWO-PASS walk per §3 of
// docs/proposals/event-source-tier-slice4.md:
//
//	Pass 1: list queues + fetch attributes; collect queue ARNs for
//	        DLQ resolution.
//	Pass 2: for queues with a RedrivePolicy, check whether the
//	        deadLetterTargetArn matches an ARN from pass 1. If yes,
//	        HasLogAxis = true (DLQ reachable in same account+region).
//	        If no, HasLogAxis = false (cross-account DLQ OR dangling
//	        reference — operator review via the audit-only
//	        sqs-deadletter-queue-attach kind).
//
// Complexity: O(2N) in queue count — the first pass is the
// list+describe walk, the second pass is N string lookups against the
// ARN set built during pass 1. Per the design doc §12 the second
// pass's in-memory cost is negligible for typical fleets.
//
// Detection per §3 of the design doc:
//
//   - HasTraceAxis ← RedrivePolicy attribute is set AND parses with
//     non-empty deadLetterTargetArn. A queue with a redrive policy is
//     the proxy for "failed messages get captured for post-mortem".
//   - HasLogAxis  ← the DLQ ARN parsed from RedrivePolicy matches an
//     ARN collected from the first-pass walk. Cross-account or dangling
//     references leave the axis false.
//
// Per-queue GetQueueAttributes failures are non-fatal: the queue row
// still surfaces with its universal columns plus the
// attribute_fetch_failed Detail marker, but both axes default to
// false. The list-pass failure is the only error a caller will see
// propagated up — the three-way ScanEventSources dispatcher treats
// that as one surface's contribution to the partial-scan posture and
// lets the EventBridge + SNS results still surface.
//
// IAM contract per docs/proposals/event-source-tier-slice4.md §12:
// sqs:ListQueues + sqs:GetQueueAttributes. Both read-only.
func (s *Scanner) ScanSQSQueues(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	region := sqsDefaultRegion
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
	return s.scanRegionSQS(ctx, factory, region, accountID)
}

// scanRegionSQS runs the per-region two-pass list + per-queue
// describe walk. Extracted from ScanSQSQueues so tests can drive the
// inner loop against a fakeFactory without the ensureFactory
// indirection.
//
// Pagination follows out.NextToken; an empty token signals "no more
// pages" per the AWS SDK convention. A nil token is the sentinel
// "this is the first page" used to skip setting the input's
// NextToken on the initial call.
//
// The function is intentionally written as two visible passes — the
// first pass appends to a queueAttributes slice plus an arnSet map
// keyed by every observed queue ARN, the second pass walks the slice
// to build the EventSourceInstanceSnapshot rows. The split keeps the
// O(2N) complexity explicit and matches the design doc §3 prose.
func (s *Scanner) scanRegionSQS(ctx context.Context, factory ClientFactory, region, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	client, err := factory.SQS(ctx, region)
	if err != nil {
		return nil, err
	}
	if client == nil {
		// Graceful: matches the existing scanner posture for unwired
		// clients on the validation path.
		return nil, nil
	}

	// Pass 1: list queues + per-queue attribute fetch.
	pass1, err := s.listAndDescribeQueues(ctx, client)
	if err != nil {
		return nil, err
	}

	// Build the ARN set the second pass walks against. Only ARNs that
	// the first-pass GetQueueAttributes call surfaced count — queues
	// whose attribute fetch failed contribute nothing to the set
	// (their own DLQ axis stays false, AND their ARN can't be matched
	// as another queue's DLQ).
	arnSet := make(map[string]struct{}, len(pass1))
	for _, qa := range pass1 {
		if qa.ARN != "" {
			arnSet[qa.ARN] = struct{}{}
		}
	}

	// Pass 2: build snapshots with DLQ reachability detection.
	out := make([]scanner.EventSourceInstanceSnapshot, 0, len(pass1))
	for _, qa := range pass1 {
		out = append(out, buildSQSSnapshot(accountID, region, qa, arnSet))
	}
	return out, nil
}

// listAndDescribeQueues is the first pass of the two-pass walk. Drives
// the paginated ListQueues call, then per-queue GetQueueAttributes
// fan-out. Per-queue describe failures are non-fatal and produce a
// queueAttributes row with Failed=true so the second pass can emit the
// universal columns plus attribute_fetch_failed Detail marker.
//
// The only error this function propagates is a ListQueues failure
// (caller's responsibility to treat as the surface-level failure
// signal for the three-way dispatcher's partial-scan posture).
func (s *Scanner) listAndDescribeQueues(ctx context.Context, client SQSClient) ([]queueAttributes, error) {
	var (
		pass1     []queueAttributes
		nextToken *string
	)
	for {
		in := &sqs.ListQueuesInput{}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		var listOut *sqs.ListQueuesOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			listOut, e = client.ListQueues(ctx, in)
			return e
		})
		if callErr != nil {
			return pass1, fmt.Errorf("list sqs queues: %w", callErr)
		}
		for _, queueURL := range listOut.QueueUrls {
			pass1 = append(pass1, describeSQSQueue(ctx, client, queueURL))
		}
		if listOut.NextToken == nil || *listOut.NextToken == "" {
			break
		}
		nextToken = listOut.NextToken
	}
	return pass1, nil
}

// describeSQSQueue fetches a single queue's attribute map and parses
// the redrive policy (when present). Per-queue failure is captured on
// the returned queueAttributes value via Failed=true; the second pass
// folds that into the attribute_fetch_failed Detail marker on the
// snapshot.
//
// Why the redrive parse lives here (alongside the fetch) rather than
// in the snapshot builder: the parse needs the raw attribute string
// AND the parse outcome feeds both the HasTraceAxis decision in the
// builder AND the DLQ ARN that the second pass matches against the
// ARN set. Folding the parse here keeps the builder a pure
// transformation against the captured queueAttributes shape, which
// makes the per-axis test cases independently readable.
func describeSQSQueue(ctx context.Context, client SQSClient, queueURL string) queueAttributes {
	qa := queueAttributes{URL: queueURL}
	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       awssdk.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
	})
	if err != nil {
		qa.Failed = true
		return qa
	}
	qa.Attributes = attrs.Attributes
	qa.ARN = attrs.Attributes[SQSQueueArnAttr]
	if rpStr, ok := attrs.Attributes[SQSRedrivePolicyAttr]; ok && rpStr != "" {
		var rp redrivePolicyShape
		if jerr := json.Unmarshal([]byte(rpStr), &rp); jerr == nil && rp.DeadLetterTargetArn != "" {
			qa.RedrivePolicy = &rp
			qa.HasRedrivePolicy = true
		}
	}
	return qa
}

// buildSQSSnapshot translates per-queue data into a snapshot row per
// the §3 detection axes. The arnSet is the union of every ARN observed
// in pass 1 — the DLQ reachability axis checks the queue's
// deadLetterTargetArn against this set.
//
// Axis rules (§3 of the slice 4 design doc):
//
//  1. HasTraceAxis ← qa.HasRedrivePolicy. A queue with a redrive
//     policy + non-empty deadLetterTargetArn is the proxy for "failed
//     messages get captured for post-mortem". Detail keys
//     "redrive_policy_target_arn" and "redrive_policy_max_receive_count"
//     surface the parsed values for the proposer's chunk-2 reasoning
//     text.
//  2. HasLogAxis  ← qa.RedrivePolicy.DeadLetterTargetArn matches an
//     ARN in arnSet. Cross-account / cross-region / dangling targets
//     leave the axis false — the chunk-2 sqs-deadletter-queue-attach
//     recommendation fires (audit-only; no Terraform pattern).
//
// Detail-only axes (informational; do NOT gate either detection axis):
//
//   - kms_master_key_id ← KmsMasterKeyId attribute when set (per §3
//     informational encryption-at-rest signal).
//   - fifo_queue ← FifoQueue == "true". The accompanying
//     content_based_deduplication flag only surfaces when both FifoQueue
//     and ContentBasedDeduplication are "true" (per §3 informational
//     FIFO-dedup signal).
func buildSQSSnapshot(accountID, region string, qa queueAttributes, arnSet map[string]struct{}) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:     string(credstore.ProviderAWS),
		Surface:      SQSSurface,
		AccountID:    accountID,
		Region:       region,
		ResourceName: sqsQueueNameFromURL(qa.URL),
		ResourceARN:  qa.ARN,
		SourceType:   sqsSourceTypeQueue,
		Detail:       map[string]any{},
	}
	if qa.Failed {
		snap.Detail["attribute_fetch_failed"] = true
		return snap
	}

	// §3 axis 1: has redrive policy → HasTraceAxis.
	if qa.HasRedrivePolicy {
		snap.HasTraceAxis = true
		snap.Detail["redrive_policy_target_arn"] = qa.RedrivePolicy.DeadLetterTargetArn
		snap.Detail["redrive_policy_max_receive_count"] = qa.RedrivePolicy.MaxReceiveCount

		// §3 axis 2: DLQ reachability via the second-pass ARN set
		// lookup. Cross-account / dangling targets leave the axis
		// false; the chunk-2 sqs-deadletter-queue-attach kind
		// surfaces the audit-only review path.
		if _, ok := arnSet[qa.RedrivePolicy.DeadLetterTargetArn]; ok {
			snap.HasLogAxis = true
		}
	}

	// Detail-only axes: encryption-at-rest + FIFO content dedup.
	if kms := qa.Attributes[SQSKmsMasterKeyIdAttr]; kms != "" {
		snap.Detail["kms_master_key_id"] = kms
	}
	if qa.Attributes[SQSFifoQueueAttr] == "true" {
		snap.Detail["fifo_queue"] = true
		if qa.Attributes[SQSContentBasedDeduplicationAttr] == "true" {
			snap.Detail["content_based_deduplication"] = true
		}
	}

	return snap
}

// sqsQueueNameFromURL extracts the queue name from an SQS URL.
//
//	https://sqs.us-east-1.amazonaws.com/123456789012/my-queue
//	→ "my-queue"
//
// Defensive against empty / malformed URLs: returns the input
// unchanged when the URL has no slash segments. SQS queue URLs are
// canonical four-segment-or-more strings; the last segment is the
// queue name.
func sqsQueueNameFromURL(url string) string {
	if url == "" {
		return ""
	}
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return url
	}
	return parts[len(parts)-1]
}
