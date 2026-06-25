// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Consumer lag detection slice 2 chunk 1 (v0.89.168, #810
// Stream 207) — AWS SQS acceptance tests per
// docs/proposals/consumer-lag-detection-slice2.md §11.1-4.

// --- Test §11.1: small backlog + young oldest → both axes low.

func TestScanSQSQueues_SmallBacklogYoungOldest_NoLagFiring(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/healthy"
		arn = "arn:aws:sqs:us-east-1:123456789012:healthy"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:                       arn,
				SQSApproximateNumberOfMessagesAttr:    strconv.Itoa(500),
				SQSApproximateAgeOfOldestMessageAttr:  strconv.Itoa(60),
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, 500, out[0].Detail["lag_backlog_depth"])
	assert.Equal(t, false, out[0].Detail["lag_backlog_depth_high"],
		"backlog=500 sits below BacklogDepthHighThreshold=1000 — sqs-backlog-monitor-add does NOT fire")
	assert.Equal(t, 60, out[0].Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, false, out[0].Detail["lag_consumer_silence_high"],
		"oldest=60s sits below ConsumerSilenceHighThreshold=300s — sqs-consumer-silence-investigate does NOT fire")
}

// --- Test §11.2: large backlog + old oldest → both axes fire.

func TestScanSQSQueues_LargeBacklogOldOldest_BothAxesFire(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/stalled"
		arn = "arn:aws:sqs:us-east-1:123456789012:stalled"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:                       arn,
				SQSApproximateNumberOfMessagesAttr:    strconv.Itoa(2000),
				SQSApproximateAgeOfOldestMessageAttr:  strconv.Itoa(400),
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, 2000, out[0].Detail["lag_backlog_depth"])
	assert.Equal(t, true, out[0].Detail["lag_backlog_depth_high"],
		"backlog=2000 ≥ BacklogDepthHighThreshold=1000 — sqs-backlog-monitor-add fires")
	assert.Equal(t, 400, out[0].Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, true, out[0].Detail["lag_consumer_silence_high"],
		"oldest=400s ≥ ConsumerSilenceHighThreshold=300s — sqs-consumer-silence-investigate fires")
}

// --- Test §11.3: large backlog + young oldest → only backlog axis fires.

func TestScanSQSQueues_LargeBacklogYoungOldest_OnlyBacklogFires(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/burst"
		arn = "arn:aws:sqs:us-east-1:123456789012:burst"
	)
	fakeClient := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{url}},
		},
		attrsByURL: map[string]map[string]string{
			url: {
				SQSQueueArnAttr:                       arn,
				SQSApproximateNumberOfMessagesAttr:    strconv.Itoa(2000),
				SQSApproximateAgeOfOldestMessageAttr:  strconv.Itoa(60),
			},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["lag_backlog_depth_high"])
	assert.Equal(t, false, out[0].Detail["lag_consumer_silence_high"],
		"young oldest message means the consumer IS draining — only the backlog signal fires (could be a burst)")
}

// --- Test §11.4: missing attributes → absent sentinels (defensive).

func TestScanSQSQueues_MissingLagAttributes_AbsentSentinels(t *testing.T) {
	const (
		url = "https://sqs.us-east-1.amazonaws.com/123456789012/no-attrs"
		arn = "arn:aws:sqs:us-east-1:123456789012:no-attrs"
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
	assert.Equal(t, -1, out[0].Detail["lag_backlog_depth"],
		"missing ApproximateNumberOfMessages → -1 absent sentinel")
	assert.Equal(t, false, out[0].Detail["lag_backlog_depth_high"])
	assert.Equal(t, -1, out[0].Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, false, out[0].Detail["lag_consumer_silence_high"])
}

// --- Boundary check: in/out-of-band edges -----------------------

func TestScanSQSQueues_LagThresholds_BoundaryEdges(t *testing.T) {
	cases := []struct {
		name                 string
		backlogDepth         int
		ageSeconds           int
		expectBacklogHigh    bool
		expectSilenceHigh    bool
	}{
		{"backlog-just-below-1000", 999, 0, false, false},
		{"backlog-equal-1000-inclusive", 1000, 0, true, false},
		{"silence-just-below-300", 0, 299, false, false},
		{"silence-equal-300-inclusive", 0, 300, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const (
				url = "https://sqs.us-east-1.amazonaws.com/123456789012/edge"
				arn = "arn:aws:sqs:us-east-1:123456789012:edge"
			)
			fakeClient := &fakeSQS{
				listQueuesPages: []*sqs.ListQueuesOutput{
					{QueueUrls: []string{url}},
				},
				attrsByURL: map[string]map[string]string{
					url: {
						SQSQueueArnAttr:                       arn,
						SQSApproximateNumberOfMessagesAttr:    strconv.Itoa(tc.backlogDepth),
						SQSApproximateAgeOfOldestMessageAttr:  strconv.Itoa(tc.ageSeconds),
					},
				},
			}
			out := runSQSScan(t, fakeClient)
			require.Len(t, out, 1)
			assert.Equal(t, tc.expectBacklogHigh, out[0].Detail["lag_backlog_depth_high"],
				"inclusive threshold BacklogDepthHighThreshold=%d — count %d must produce backlog_high=%v",
				BacklogDepthHighThreshold, tc.backlogDepth, tc.expectBacklogHigh)
			assert.Equal(t, tc.expectSilenceHigh, out[0].Detail["lag_consumer_silence_high"],
				"inclusive threshold ConsumerSilenceHighThreshold=%d — age %d must produce silence_high=%v",
				ConsumerSilenceHighThreshold, tc.ageSeconds, tc.expectSilenceHigh)
		})
	}
}

// --- Cold-start parity: existing slice-4 + slice-1 DLQ keys preserved.

func TestScanSQSQueues_LagAxisAdditive_PreservesPriorKeys(t *testing.T) {
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
				SQSQueueArnAttr:                       mainARN,
				SQSRedrivePolicyAttr:                  makeRedrivePolicyJSON(dlqARN, 7),
				SQSApproximateNumberOfMessagesAttr:    strconv.Itoa(1500),
				SQSApproximateAgeOfOldestMessageAttr:  strconv.Itoa(450),
			},
			dlqURL: {SQSQueueArnAttr: dlqARN},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 2)

	// Slice-4 keys preserved byte-identically.
	assert.Equal(t, dlqARN, out[0].Detail["redrive_policy_target_arn"])
	assert.Equal(t, 7, out[0].Detail["redrive_policy_max_receive_count"])
	assert.True(t, out[0].HasTraceAxis)
	assert.True(t, out[0].HasLogAxis)

	// Slice-1 DLQ keys preserved byte-identically.
	assert.Equal(t, true, out[0].Detail["has_dlq"])
	assert.Equal(t, 7, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, true, out[0].Detail["dlq_retry_count_in_band"])

	// Slice-2 lag axis keys also present.
	assert.Equal(t, 1500, out[0].Detail["lag_backlog_depth"])
	assert.Equal(t, true, out[0].Detail["lag_backlog_depth_high"])
	assert.Equal(t, 450, out[0].Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, true, out[0].Detail["lag_consumer_silence_high"])
}
