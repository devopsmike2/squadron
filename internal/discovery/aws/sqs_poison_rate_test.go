// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Poison-message rate analysis slice 3 chunk 1 (v0.89.173,
// #815 Stream 212) — AWS SQS acceptance tests per design
// doc §11.1-2.

// --- Test §11.1: any queue shape → poison_rate_per_hour=-1.

func TestScanSQSQueues_PoisonRate_AlwaysHonestFramingState(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		arn        string
		extraAttrs map[string]string
	}{
		{
			name: "queue with no extra attrs",
			url:  "https://sqs.us-east-1.amazonaws.com/123456789012/bare",
			arn:  "arn:aws:sqs:us-east-1:123456789012:bare",
		},
		{
			name: "queue with redrive policy + large backlog",
			url:  "https://sqs.us-east-1.amazonaws.com/123456789012/loaded",
			arn:  "arn:aws:sqs:us-east-1:123456789012:loaded",
			extraAttrs: map[string]string{
				SQSRedrivePolicyAttr: makeRedrivePolicyJSON(
					"arn:aws:sqs:us-east-1:123456789012:loaded-dlq", 5),
				SQSApproximateNumberOfMessagesAttr: "5000",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrs := map[string]string{SQSQueueArnAttr: tc.arn}
			for k, v := range tc.extraAttrs {
				attrs[k] = v
			}
			fakeClient := &fakeSQS{
				listQueuesPages: []*sqs.ListQueuesOutput{
					{QueueUrls: []string{tc.url}},
				},
				attrsByURL: map[string]map[string]string{tc.url: attrs},
			}
			out := runSQSScan(t, fakeClient)
			require.Len(t, out, 1)
			assert.Equal(t, -1, out[0].Detail["poison_rate_per_hour"],
				"slice 3 §3.3 invariant: substrate MetricQuerier integration deferred — always absent sentinel")
			assert.Equal(t, false, out[0].Detail["poison_rate_high_band"])
		})
	}
}

// --- Cold-start parity: existing slice-4 + slice-1-DLQ + slice-2-lag keys preserved.

func TestScanSQSQueues_PoisonRateAxisAdditive_PreservesPriorKeys(t *testing.T) {
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
				SQSQueueArnAttr:                      mainARN,
				SQSRedrivePolicyAttr:                 makeRedrivePolicyJSON(dlqARN, 7),
				SQSApproximateNumberOfMessagesAttr:   "1500",
				SQSApproximateAgeOfOldestMessageAttr: "450",
			},
			dlqURL: {SQSQueueArnAttr: dlqARN},
		},
	}
	out := runSQSScan(t, fakeClient)
	require.Len(t, out, 2)

	// Slice-4 keys preserved.
	assert.Equal(t, dlqARN, out[0].Detail["redrive_policy_target_arn"])
	assert.True(t, out[0].HasTraceAxis)

	// Slice-1-DLQ keys preserved.
	assert.Equal(t, true, out[0].Detail["has_dlq"])
	assert.Equal(t, true, out[0].Detail["dlq_retry_count_in_band"])

	// Slice-2-lag keys preserved.
	assert.Equal(t, 1500, out[0].Detail["lag_backlog_depth"])
	assert.Equal(t, true, out[0].Detail["lag_backlog_depth_high"])
	assert.Equal(t, true, out[0].Detail["lag_consumer_silence_high"])

	// Slice-3 poison-rate axis keys also present.
	assert.Equal(t, -1, out[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, out[0].Detail["poison_rate_high_band"])
}
