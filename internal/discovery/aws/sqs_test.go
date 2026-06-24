// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSQS is the test double satisfying SQSClient. Mirrors fakeSNS's
// shape — a map of pre-populated ListQueues pages plus a per-URL
// attribute response map drives the per-test behaviour.
type fakeSQS struct {
	listQueuesPages   []*sqs.ListQueuesOutput
	listQueuesCallIdx int
	listQueuesErr     error
	listQueuesCalls   int

	// attrsByURL maps queue URL → attribute map response. Missing →
	// returns an empty attributes map (no error). attrsErrByURL
	// overrides with a per-URL error when set.
	attrsByURL    map[string]map[string]string
	attrsErrByURL map[string]error
	getAttrsCalls int
}

func (f *fakeSQS) ListQueues(_ context.Context, _ *sqs.ListQueuesInput, _ ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	f.listQueuesCalls++
	if f.listQueuesErr != nil {
		return nil, f.listQueuesErr
	}
	if f.listQueuesCallIdx >= len(f.listQueuesPages) {
		return &sqs.ListQueuesOutput{}, nil
	}
	out := f.listQueuesPages[f.listQueuesCallIdx]
	f.listQueuesCallIdx++
	if out == nil {
		return &sqs.ListQueuesOutput{}, nil
	}
	return out, nil
}

func (f *fakeSQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	f.getAttrsCalls++
	url := awssdk.ToString(in.QueueUrl)
	if err, ok := f.attrsErrByURL[url]; ok {
		return nil, err
	}
	if attrs, ok := f.attrsByURL[url]; ok {
		return &sqs.GetQueueAttributesOutput{Attributes: attrs}, nil
	}
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{}}, nil
}

// Compile-time check that fakeSQS satisfies the SQSClient interface.
var _ SQSClient = (*fakeSQS)(nil)

// runSQSScan wires a fake SQS client into a fresh Scanner via the test
// factory builder and calls ScanSQSQueues against us-east-1.
func runSQSScan(t *testing.T, fakeClient *fakeSQS) []scanner.EventSourceInstanceSnapshot {
	t.Helper()
	factory := &fakeFactory{sqs: fakeClient}
	s := newTestScanner(t, factory)
	out, err := s.ScanSQSQueues(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	return out
}

// makeRedrivePolicyJSON returns the JSON-encoded RedrivePolicy
// attribute string with the supplied target ARN + maxReceiveCount.
func makeRedrivePolicyJSON(targetARN string, maxReceive int) string {
	return `{"deadLetterTargetArn":"` + targetARN + `","maxReceiveCount":` + itoa(maxReceive) + `}`
}

// itoa avoids the strconv import in tests — keeps the test file free
// of helper imports beyond what the SDK packages already pull in.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// --- Tests ------------------------------------------------------------

// TestScanSQSQueues_ListReturnsQueues_Paginated — slice 4 acceptance
// test 1: a multi-page ListQueues response is walked to completion;
// every queue surfaces with the universal columns populated.
func TestScanSQSQueues_ListReturnsQueues_Paginated(t *testing.T) {
	const (
		urlA = "https://sqs.us-east-1.amazonaws.com/123456789012/queue-a"
		urlB = "https://sqs.us-east-1.amazonaws.com/123456789012/queue-b"
		urlC = "https://sqs.us-east-1.amazonaws.com/123456789012/queue-c"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{urlA, urlB}, NextToken: awssdk.String("page2")},
			{QueueUrls: []string{urlC}},
		},
		attrsByURL: map[string]map[string]string{
			urlA: {SQSQueueArnAttr: "arn:aws:sqs:us-east-1:123456789012:queue-a"},
			urlB: {SQSQueueArnAttr: "arn:aws:sqs:us-east-1:123456789012:queue-b"},
			urlC: {SQSQueueArnAttr: "arn:aws:sqs:us-east-1:123456789012:queue-c"},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 3, "pagination must walk every queue across both pages")
	assert.Equal(t, 2, fakeClient.listQueuesCalls, "both list pages must be requested")
	assert.Equal(t, "queue-a", out[0].ResourceName)
	assert.Equal(t, "queue-b", out[1].ResourceName)
	assert.Equal(t, "queue-c", out[2].ResourceName)
}

// TestScanSQSQueues_QueueWithRedrivePolicyAndReachableDLQ_BothAxesTrue
// — slice 4 acceptance test 2: a queue with a RedrivePolicy whose
// deadLetterTargetArn matches another queue's ARN in the same scan
// flips BOTH HasTraceAxis (redrive policy present) AND HasLogAxis (DLQ
// reachable in same account+region).
func TestScanSQSQueues_QueueWithRedrivePolicyAndReachableDLQ_BothAxesTrue(t *testing.T) {
	const (
		mainURL = "https://sqs.us-east-1.amazonaws.com/123456789012/main"
		mainARN = "arn:aws:sqs:us-east-1:123456789012:main"
		dlqURL  = "https://sqs.us-east-1.amazonaws.com/123456789012/main-dlq"
		dlqARN  = "arn:aws:sqs:us-east-1:123456789012:main-dlq"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{mainURL, dlqURL}},
		},
		attrsByURL: map[string]map[string]string{
			mainURL: {
				SQSQueueArnAttr:      mainARN,
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqARN, 5),
			},
			dlqURL: {SQSQueueArnAttr: dlqARN},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 2)
	// Main queue: both axes true.
	assert.Equal(t, mainARN, out[0].ResourceARN)
	assert.True(t, out[0].HasTraceAxis, "redrive policy flips trace axis")
	assert.True(t, out[0].HasLogAxis, "reachable DLQ flips log axis")
	assert.Equal(t, dlqARN, out[0].Detail["redrive_policy_target_arn"])
	assert.Equal(t, 5, out[0].Detail["redrive_policy_max_receive_count"])
}

// TestScanSQSQueues_QueueWithoutRedrivePolicy_BothAxesFalse — slice 4
// acceptance test 3: a queue with NO RedrivePolicy attribute set
// leaves both axes false. The chunk-2 sqs-redrive-policy-enable
// recommendation engine consumes this state.
func TestScanSQSQueues_QueueWithoutRedrivePolicy_BothAxesFalse(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/orphan-queue"
		arn = "arn:aws:sqs:us-east-1:123456789012:orphan-queue"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {SQSQueueArnAttr: arn},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis, "no redrive policy → trace axis stays false")
	assert.False(t, out[0].HasLogAxis, "no redrive policy → log axis stays false")
	_, present := out[0].Detail["redrive_policy_target_arn"]
	assert.False(t, present, "no redrive target ARN recorded when absent")
}

// TestScanSQSQueues_QueueWithRedrivePolicyButUnresolvableDLQ_TraceAxisTrueLogAxisFalse
// — slice 4 acceptance test 4: the audit path. A queue with a
// RedrivePolicy whose deadLetterTargetArn does NOT match any queue
// ARN in the scan flips HasTraceAxis true (policy exists) but leaves
// HasLogAxis false (DLQ unreachable — cross-account / cross-region /
// dangling reference). The chunk-2 sqs-deadletter-queue-attach
// recommendation engine consumes this state.
func TestScanSQSQueues_QueueWithRedrivePolicyButUnresolvableDLQ_TraceAxisTrueLogAxisFalse(t *testing.T) {
	const (
		mainURL          = "https://sqs.us-east-1.amazonaws.com/123456789012/main"
		mainARN          = "arn:aws:sqs:us-east-1:123456789012:main"
		dlqInOtherRegion = "arn:aws:sqs:us-west-2:999999999999:cross-account-dlq"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{mainURL}},
		},
		attrsByURL: map[string]map[string]string{
			mainURL: {
				SQSQueueArnAttr:      mainARN,
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqInOtherRegion, 3),
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasTraceAxis, "redrive policy present → trace axis flips true")
	assert.False(t, out[0].HasLogAxis, "DLQ unresolvable → log axis stays false")
	assert.Equal(t, dlqInOtherRegion, out[0].Detail["redrive_policy_target_arn"])
}

// TestScanSQSQueues_QueueWithKMS_DetailRecordsKmsKeyId — slice 4
// acceptance test 5: when KmsMasterKeyId is set, the snapshot Detail
// records the kms_master_key_id flag.
func TestScanSQSQueues_QueueWithKMS_DetailRecordsKmsKeyId(t *testing.T) {
	const (
		url    = "https://sqs.us-east-1.amazonaws.com/123456789012/encrypted-queue"
		arn    = "arn:aws:sqs:us-east-1:123456789012:encrypted-queue"
		kmsKey = "alias/aws/sqs"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:        arn,
				SQSKmsMasterKeyIdAttr:  kmsKey,
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, kmsKey, out[0].Detail["kms_master_key_id"])
}

// TestScanSQSQueues_FifoQueueWithContentDedup_DetailRecordsDedup —
// slice 4 acceptance test 6: a FIFO queue with ContentBasedDeduplication
// records both fifo_queue and content_based_deduplication flags.
func TestScanSQSQueues_FifoQueueWithContentDedup_DetailRecordsDedup(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/fifo-queue.fifo"
		arn = "arn:aws:sqs:us-east-1:123456789012:fifo-queue.fifo"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:                  arn,
				SQSFifoQueueAttr:                 "true",
				SQSContentBasedDeduplicationAttr: "true",
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["fifo_queue"])
	assert.Equal(t, true, out[0].Detail["content_based_deduplication"])
}

// TestScanSQSQueues_FifoQueueWithoutDedup_NoDedupFlag — a FIFO queue
// without content-based dedup records only the fifo_queue flag; the
// content_based_deduplication key is absent rather than false.
func TestScanSQSQueues_FifoQueueWithoutDedup_NoDedupFlag(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/fifo-only.fifo"
		arn = "arn:aws:sqs:us-east-1:123456789012:fifo-only.fifo"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:  arn,
				SQSFifoQueueAttr: "true",
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["fifo_queue"])
	_, present := out[0].Detail["content_based_deduplication"]
	assert.False(t, present, "absent rather than false when dedup is not set")
}

// TestScanSQSQueues_GetAttributesFailureNonFatal — partial-scan
// posture on the per-queue level. When GetQueueAttributes fails on one
// queue, that queue surfaces with the attribute_fetch_failed Detail
// flag set; remaining queues surface normally. Slice 4 §5 contract.
func TestScanSQSQueues_GetAttributesFailureNonFatal(t *testing.T) {
	const (
		urlGood = "https://sqs.us-east-1.amazonaws.com/123456789012/good"
		urlBad  = "https://sqs.us-east-1.amazonaws.com/123456789012/bad"
		arnGood = "arn:aws:sqs:us-east-1:123456789012:good"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{urlGood, urlBad}},
		},
		attrsByURL: map[string]map[string]string{
			urlGood: {SQSQueueArnAttr: arnGood},
		},
		attrsErrByURL: map[string]error{
			urlBad: errors.New("simulated GetQueueAttributes failure"),
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 2, "both rows surface; failing one still produces a snapshot")
	// Good row has the ARN populated.
	assert.Equal(t, arnGood, out[0].ResourceARN)
	_, badFailed := out[0].Detail["attribute_fetch_failed"]
	assert.False(t, badFailed, "good row does not carry the failure flag")
	// Bad row carries the failure flag.
	assert.Equal(t, "bad", out[1].ResourceName)
	assert.Equal(t, true, out[1].Detail["attribute_fetch_failed"])
	assert.False(t, out[1].HasTraceAxis)
	assert.False(t, out[1].HasLogAxis)
}

// TestScanSQSQueues_RedrivePolicyJSONParseFailure_NoTraceAxis — a
// RedrivePolicy attribute that doesn't parse as JSON leaves the trace
// axis false rather than panicking. Defensive against API drift /
// operator-injected malformed values.
func TestScanSQSQueues_RedrivePolicyJSONParseFailure_NoTraceAxis(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/malformed"
		arn = "arn:aws:sqs:us-east-1:123456789012:malformed"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:      arn,
				SQSRedrivePolicyAttr: "not-valid-json{",
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis, "malformed RedrivePolicy must not flip trace axis")
	assert.False(t, out[0].HasLogAxis)
}

// TestScanSQSQueues_ResourceNamePopulatedFromURL — the ResourceName
// field carries the trailing URL segment so the UI doesn't have to
// parse SQS URLs.
func TestScanSQSQueues_ResourceNamePopulatedFromURL(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/my-named-queue"
		arn = "arn:aws:sqs:us-east-1:123456789012:my-named-queue"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {SQSQueueArnAttr: arn},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, "my-named-queue", out[0].ResourceName)
}

// TestScanSQSQueues_SourceTypeIsQueue — the SourceType discriminator
// is "queue" so the UI renders SQS rows under the canonical sub-type
// column.
func TestScanSQSQueues_SourceTypeIsQueue(t *testing.T) {
	const url = "https://sqs.us-east-1.amazonaws.com/123456789012/any"
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, "queue", out[0].SourceType)
}

// TestScanSQSQueues_SurfaceIsSqs — the Surface discriminator is "sqs"
// so the proposer's webhook router (chunk 2) routes sqs- prefixes to
// AWS.
func TestScanSQSQueues_SurfaceIsSqs(t *testing.T) {
	const url = "https://sqs.us-east-1.amazonaws.com/123456789012/any"
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, "sqs", out[0].Surface)
	assert.Equal(t, "aws", out[0].Provider)
}

// TestScanSQSQueues_TwoPassWalkResolvesDLQViaARNSet — exercises the
// load-bearing two-pass walk: a queue whose RedrivePolicy points at
// ANOTHER queue (not itself) must have its log axis flipped when both
// queues surface from the same ListQueues page. The first pass
// collects every ARN; the second pass walks the queueAttributes slice
// and matches against the ARN set. Order-independent: even if the DLQ
// surfaces AFTER the main queue in the list response, the second pass
// still resolves the reference.
func TestScanSQSQueues_TwoPassWalkResolvesDLQViaARNSet(t *testing.T) {
	const (
		mainURL = "https://sqs.us-east-1.amazonaws.com/123456789012/main-after"
		mainARN = "arn:aws:sqs:us-east-1:123456789012:main-after"
		dlqURL  = "https://sqs.us-east-1.amazonaws.com/123456789012/dlq-after"
		dlqARN  = "arn:aws:sqs:us-east-1:123456789012:dlq-after"
	)
	// Main surfaces FIRST in the list; DLQ surfaces second. A
	// single-pass naive walk would fail to resolve the reference at
	// the time main's snapshot is built. The two-pass walk completes
	// pass 1 first (both ARNs collected), then pass 2 resolves.
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{mainURL, dlqURL}},
		},
		attrsByURL: map[string]map[string]string{
			mainURL: {
				SQSQueueArnAttr:      mainARN,
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqARN, 5),
			},
			dlqURL: {SQSQueueArnAttr: dlqARN},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 2)
	// Main row is at index 0 (list order), and the log axis flipped
	// despite the DLQ entry surfacing later in the list — proof the
	// second pass walked after the first pass completed.
	assert.Equal(t, mainARN, out[0].ResourceARN)
	assert.True(t, out[0].HasTraceAxis)
	assert.True(t, out[0].HasLogAxis, "two-pass walk resolves DLQ via ARN set regardless of list order")
	// DLQ row itself has no redrive policy.
	assert.Equal(t, dlqARN, out[1].ResourceARN)
	assert.False(t, out[1].HasTraceAxis)
}

// TestScanSQSQueues_SQSClientNil_GracefullyReturnsEmpty — when the
// factory's SQS client returns nil (no client wired), the scan
// gracefully returns an empty result rather than nil-panicking.
// Matches the existing scanner posture for unwired clients.
func TestScanSQSQueues_SQSClientNil_GracefullyReturnsEmpty(t *testing.T) {
	factory := &nilSQSFactory{}
	s := newTestScanner(t, factory)
	out, err := s.ScanSQSQueues(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	assert.Empty(t, out)
}

// nilSQSFactory wraps the default fake factory but explicitly returns
// nil for the SQS client to exercise the graceful-nil branch.
type nilSQSFactory struct {
	fakeFactory
}

func (f *nilSQSFactory) SQS(_ context.Context, _ string) (SQSClient, error) {
	return nil, nil
}

// TestSQSQueueNameFromURL_ExtractsLastSegment — the bare helper for
// the URL → queue name extraction. Defensive against empty input.
func TestSQSQueueNameFromURL_ExtractsLastSegment(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"canonical", "https://sqs.us-east-1.amazonaws.com/123456789012/my-queue", "my-queue"},
		{"fifo suffix preserved", "https://sqs.us-east-1.amazonaws.com/123456789012/orders.fifo", "orders.fifo"},
		{"empty", "", ""},
		{"no slashes", "bare-name", "bare-name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sqsQueueNameFromURL(tc.in))
		})
	}
}
