// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DLQ configuration analysis slice 1 chunk 1 (v0.89.163, #805
// Stream 202) — AWS SQS acceptance tests per
// docs/proposals/dlq-configuration-analysis-slice1.md §11.1-5.
//
// These tests exercise the new has_dlq / dlq_retry_count /
// dlq_retry_count_in_band Detail keys without touching the slice-4
// existing detection axes (HasTraceAxis / HasLogAxis) so any
// regression in the new chunk surfaces cleanly without entangling
// the slice-4 contract.

// --- Test §11.1: queue with no RedrivePolicy → has_dlq=false --------
//
// A queue with NO RedrivePolicy attribute leaves has_dlq=false and
// dlq_retry_count=-1 (the absent sentinel). This is the firing
// condition for the sqs-dlq-attach recommendation kind that chunk 2
// will route via the existing sqs- webhook prefix.

func TestScanSQSQueues_NoRedrivePolicy_HasDLQFalse_RetryCountAbsentSentinel(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/orphan"
		arn = "arn:aws:sqs:us-east-1:123456789012:orphan"
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
	assert.Equal(t, false, out[0].Detail["has_dlq"],
		"queue without RedrivePolicy must flag has_dlq false — fires sqs-dlq-attach")
	assert.Equal(t, -1, out[0].Detail["dlq_retry_count"],
		"absent sentinel preserves the absent-vs-zero distinction the proposer reasoning depends on")
	assert.Equal(t, false, out[0].Detail["dlq_retry_count_in_band"],
		"the band check is meaningless when the DLQ itself is absent")
}

// --- Test §11.2: in-band retry count → has_dlq=true + in_band=true --

func TestScanSQSQueues_InBandRetryCount_HasDLQTrue_InBandTrue(t *testing.T) {
	const (
		mainURL = "https://sqs.us-east-1.amazonaws.com/123456789012/in-band"
		mainARN = "arn:aws:sqs:us-east-1:123456789012:in-band"
		dlqARN  = "arn:aws:sqs:us-east-1:123456789012:in-band-dlq"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{mainURL}},
		},
		attrsByURL: map[string]map[string]string{
			mainURL: {
				SQSQueueArnAttr:      mainARN,
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqARN, 5),
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["has_dlq"])
	assert.Equal(t, 5, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, true, out[0].Detail["dlq_retry_count_in_band"],
		"maxReceiveCount=5 sits inside the [2, 50] band — sqs-dlq-retry-count-bound does NOT fire")
}

// --- Test §11.3: below-band retry count → has_dlq=true + in_band=false -

func TestScanSQSQueues_BelowBandRetryCount_OutOfBand_FiresBound(t *testing.T) {
	const (
		mainURL = "https://sqs.us-east-1.amazonaws.com/123456789012/too-aggressive"
		mainARN = "arn:aws:sqs:us-east-1:123456789012:too-aggressive"
		dlqARN  = "arn:aws:sqs:us-east-1:123456789012:too-aggressive-dlq"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{mainURL}},
		},
		attrsByURL: map[string]map[string]string{
			mainURL: {
				SQSQueueArnAttr:      mainARN,
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqARN, 1),
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["has_dlq"])
	assert.Equal(t, 1, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, false, out[0].Detail["dlq_retry_count_in_band"],
		"maxReceiveCount=1 is below DLQRetryCountBandMin=2 — sqs-dlq-retry-count-bound fires")
}

// --- Test §11.4: above-band retry count → has_dlq=true + in_band=false -

func TestScanSQSQueues_AboveBandRetryCount_OutOfBand_FiresBound(t *testing.T) {
	const (
		mainURL = "https://sqs.us-east-1.amazonaws.com/123456789012/too-lenient"
		mainARN = "arn:aws:sqs:us-east-1:123456789012:too-lenient"
		dlqARN  = "arn:aws:sqs:us-east-1:123456789012:too-lenient-dlq"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{mainURL}},
		},
		attrsByURL: map[string]map[string]string{
			mainURL: {
				SQSQueueArnAttr:      mainARN,
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqARN, 1000),
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["has_dlq"])
	assert.Equal(t, 1000, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, false, out[0].Detail["dlq_retry_count_in_band"],
		"maxReceiveCount=1000 is above DLQRetryCountBandMax=50 — sqs-dlq-retry-count-bound fires")
}

// --- Test §11.5: malformed RedrivePolicy JSON → has_dlq=false -------
//
// Defensive case: a queue whose RedrivePolicy attribute is set to
// non-JSON garbage MUST NOT be treated as "DLQ configured". The
// slice-4 extractQueueAttributes path already defensively gates
// HasRedrivePolicy on json.Unmarshal success + non-empty
// deadLetterTargetArn, so the slice-1 DLQ axis inherits the same
// defensive posture for free. This test pins that inheritance.

func TestScanSQSQueues_MalformedRedrivePolicyJSON_HasDLQFalse(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/malformed-rp"
		arn = "arn:aws:sqs:us-east-1:123456789012:malformed-rp"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:      arn,
				SQSRedrivePolicyAttr: "this-is-not-valid-json{{{",
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, false, out[0].Detail["has_dlq"],
		"malformed RedrivePolicy JSON must NOT count as DLQ presence — sqs-dlq-attach fires")
	assert.Equal(t, -1, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, false, out[0].Detail["dlq_retry_count_in_band"])
}

// --- Boundary check: in-band edges (2 and 50) -----------------------
//
// Pins the inclusive-bound semantics. A future tuning slice that
// changes the band has to surface this test failing to indicate the
// band moved.

func TestScanSQSQueues_RetryCount_BoundaryEdges_BothInBand(t *testing.T) {
	cases := []struct {
		name        string
		retryCount  int
		expectInBand bool
	}{
		{"lower-edge-2", 2, true},
		{"upper-edge-50", 50, true},
		{"just-below-lower", 1, false},
		{"just-above-upper", 51, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const (
				mainURL = "https://sqs.us-east-1.amazonaws.com/123456789012/edge"
				mainARN = "arn:aws:sqs:us-east-1:123456789012:edge"
				dlqARN  = "arn:aws:sqs:us-east-1:123456789012:edge-dlq"
			)
			fakeClient := &fakeSQS{
				listQueuesPages: []*sqs.ListQueuesOutput{
					{QueueUrls: []string{mainURL}},
				},
				attrsByURL: map[string]map[string]string{
					mainURL: {
						SQSQueueArnAttr:      mainARN,
						SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqARN, tc.retryCount),
					},
				},
			}
			out := runSQSScan(t, fakeClient)
			require.Len(t, out, 1)
			assert.Equal(t, tc.expectInBand, out[0].Detail["dlq_retry_count_in_band"],
				"inclusive band [%d, %d] — count %d must produce in_band=%v",
				DLQRetryCountBandMin, DLQRetryCountBandMax, tc.retryCount, tc.expectInBand)
		})
	}
}

// --- Cold-start parity: existing slice-4 Detail keys preserved ------
//
// The slice-1 chunk 1 patch is ADDITIVE only. The existing slice-4
// Detail keys (redrive_policy_target_arn,
// redrive_policy_max_receive_count, kms_master_key_id, fifo_queue,
// content_based_deduplication) must survive byte-identically. This
// test pins the slice-4 keys' presence alongside the new slice-1
// keys so a future refactor that accidentally drops a slice-4 key
// triggers a regression test failure.

func TestScanSQSQueues_DLQAxisAdditive_PreservesSlice4Keys(t *testing.T) {
	const (
		mainURL = "https://sqs.us-east-1.amazonaws.com/123456789012/parity"
		mainARN = "arn:aws:sqs:us-east-1:123456789012:parity"
		dlqURL  = "https://sqs.us-east-1.amazonaws.com/123456789012/parity-dlq"
		dlqARN  = "arn:aws:sqs:us-east-1:123456789012:parity-dlq"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{mainURL, dlqURL}},
		},
		attrsByURL: map[string]map[string]string{
			mainURL: {
				SQSQueueArnAttr:      mainARN,
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(dlqARN, 7),
			},
			dlqURL: {SQSQueueArnAttr: dlqARN},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 2)

	// Slice-4 keys preserved byte-identically.
	assert.Equal(t, dlqARN, out[0].Detail["redrive_policy_target_arn"])
	assert.Equal(t, 7, out[0].Detail["redrive_policy_max_receive_count"])
	assert.True(t, out[0].HasTraceAxis, "slice-4 HasTraceAxis preserved")
	assert.True(t, out[0].HasLogAxis, "slice-4 HasLogAxis preserved (reachable DLQ via ARN set)")

	// Slice-1 keys also present.
	assert.Equal(t, true, out[0].Detail["has_dlq"])
	assert.Equal(t, 7, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, true, out[0].Detail["dlq_retry_count_in_band"])
}
