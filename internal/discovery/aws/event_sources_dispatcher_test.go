// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Slice 4 three-way dispatcher tests ------------------------------
//
// ScanEventSources fans out across EventBridge + SNS + SQS with a
// three-way partial-scan posture. Tests 7 / 8 / 9 / 10 / 11 of the
// slice 4 design doc pin the contract: all three surfaces dispatched
// independently; any one or two failing does NOT block the others;
// only when ALL THREE fail does the dispatcher return a non-nil error
// wrapping every per-surface cause.
//
// Slice 4 replaces the slice 3 two-way variants — the two-way
// dispatcher contract is a strict subset of the three-way contract,
// so keeping both around would yield duplicated assertions that drift
// as the dispatcher evolves.

// TestScanEventSources_DispatchesToAllThreeSurfaces — slice 4
// acceptance test 7: the dispatcher returns buses, topics, AND
// queues when all three surfaces produce data.
func TestScanEventSources_DispatchesToAllThreeSurfaces(t *testing.T) {
	const (
		busARN   = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		topicARN = "arn:aws:sns:us-east-1:123456789012:orders"
		queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/jobs"
		queueARN = "arn:aws:sqs:us-east-1:123456789012:jobs"
	)
	fakeEB := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{
				{Name: awssdk.String("default"), Arn: awssdk.String(busARN)},
			}},
		},
	}
	fakeS := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{{TopicArn: awssdk.String(topicARN)}}},
		},
		attrsByARN: map[string]map[string]string{
			topicARN: {"SubscriptionsConfirmed": "1"},
		},
	}
	fakeQ := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{queueURL}},
		},
		attrsByURL: map[string]map[string]string{
			queueURL: {SQSQueueArnAttr: queueARN},
		},
	}
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS, sqs: fakeQ})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	require.Len(t, out, 3, "dispatcher must return bus, topic, AND queue")

	// Surface order: EventBridge first, then SNS, then SQS.
	assert.Equal(t, "eventbridge", out[0].Surface)
	assert.Equal(t, busARN, out[0].ResourceARN)
	assert.Equal(t, "sns", out[1].Surface)
	assert.Equal(t, topicARN, out[1].ResourceARN)
	assert.Equal(t, "sqs", out[2].Surface)
	assert.Equal(t, queueARN, out[2].ResourceARN)
}

// TestScanEventSources_EventBridgeFails_SNSAndSQSStillSurface — slice 4
// acceptance test 8: the partial-scan posture in the EventBridge-fails
// direction. The SNS topic + SQS queue rows still surface; the
// dispatcher returns no error because at least one surface succeeded.
func TestScanEventSources_EventBridgeFails_SNSAndSQSStillSurface(t *testing.T) {
	const (
		topicARN = "arn:aws:sns:us-east-1:123456789012:still-here"
		queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/still-here-queue"
		queueARN = "arn:aws:sqs:us-east-1:123456789012:still-here-queue"
	)
	fakeEB := &fakeEventBridge{
		listBusesErr: errors.New("simulated eventbridge ListEventBuses failure"),
	}
	fakeS := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{{TopicArn: awssdk.String(topicARN)}}},
		},
		attrsByARN: map[string]map[string]string{
			topicARN: {"SubscriptionsConfirmed": "2"},
		},
	}
	fakeQ := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{queueURL}},
		},
		attrsByURL: map[string]map[string]string{
			queueURL: {SQSQueueArnAttr: queueARN},
		},
	}
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS, sqs: fakeQ})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err, "three-way partial-scan posture: only EventBridge failed")
	require.Len(t, out, 2, "SNS topic AND SQS queue still surface")
	assert.Equal(t, "sns", out[0].Surface)
	assert.Equal(t, topicARN, out[0].ResourceARN)
	assert.Equal(t, "sqs", out[1].Surface)
	assert.Equal(t, queueARN, out[1].ResourceARN)
}

// TestScanEventSources_SNSFails_EventBridgeAndSQSStillSurface — slice 4
// acceptance test 9: the partial-scan posture in the SNS-fails
// direction. The EventBridge bus + SQS queue rows still surface; the
// dispatcher returns no error because at least one surface succeeded.
func TestScanEventSources_SNSFails_EventBridgeAndSQSStillSurface(t *testing.T) {
	const (
		busARN   = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/still-here-queue"
		queueARN = "arn:aws:sqs:us-east-1:123456789012:still-here-queue"
	)
	fakeEB := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{
				{Name: awssdk.String("default"), Arn: awssdk.String(busARN)},
			}},
		},
	}
	fakeS := &fakeSNS{
		listTopicsErr: errors.New("simulated sns ListTopics failure"),
	}
	fakeQ := &fakeSQS{
		listQueuesPages: []*sqs.ListQueuesOutput{
			{QueueUrls: []string{queueURL}},
		},
		attrsByURL: map[string]map[string]string{
			queueURL: {SQSQueueArnAttr: queueARN},
		},
	}
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS, sqs: fakeQ})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err, "three-way partial-scan posture: only SNS failed")
	require.Len(t, out, 2, "EventBridge bus AND SQS queue still surface")
	assert.Equal(t, "eventbridge", out[0].Surface)
	assert.Equal(t, busARN, out[0].ResourceARN)
	assert.Equal(t, "sqs", out[1].Surface)
	assert.Equal(t, queueARN, out[1].ResourceARN)
}

// TestScanEventSources_SQSFails_EventBridgeAndSNSStillSurface — slice 4
// acceptance test 10: the partial-scan posture in the SQS-fails
// direction. The EventBridge bus + SNS topic rows still surface; the
// dispatcher returns no error because at least one surface succeeded.
func TestScanEventSources_SQSFails_EventBridgeAndSNSStillSurface(t *testing.T) {
	const (
		busARN   = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		topicARN = "arn:aws:sns:us-east-1:123456789012:still-here"
	)
	fakeEB := &fakeEventBridge{
		listBusesPages: []*eventbridge.ListEventBusesOutput{
			{EventBuses: []ebtypes.EventBus{
				{Name: awssdk.String("default"), Arn: awssdk.String(busARN)},
			}},
		},
	}
	fakeS := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{{TopicArn: awssdk.String(topicARN)}}},
		},
		attrsByARN: map[string]map[string]string{
			topicARN: {"SubscriptionsConfirmed": "1"},
		},
	}
	fakeQ := &fakeSQS{
		listQueuesErr: errors.New("simulated sqs ListQueues failure"),
	}
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS, sqs: fakeQ})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err, "three-way partial-scan posture: only SQS failed")
	require.Len(t, out, 2, "EventBridge bus AND SNS topic still surface")
	assert.Equal(t, "eventbridge", out[0].Surface)
	assert.Equal(t, busARN, out[0].ResourceARN)
	assert.Equal(t, "sns", out[1].Surface)
	assert.Equal(t, topicARN, out[1].ResourceARN)
}

// TestScanEventSources_AllThreeFailReturnsErrorMentioningAllThreeSurfaces
// — slice 4 acceptance test 11: the dispatcher's only error-returning
// path. ALL THREE surfaces fail. The returned error must mention
// eventbridge, sns, AND sqs so the operator-facing error message
// captures the full failure envelope.
func TestScanEventSources_AllThreeFailReturnsErrorMentioningAllThreeSurfaces(t *testing.T) {
	fakeEB := &fakeEventBridge{
		listBusesErr: errors.New("eventbridge boom"),
	}
	fakeS := &fakeSNS{
		listTopicsErr: errors.New("sns kaboom"),
	}
	fakeQ := &fakeSQS{
		listQueuesErr: errors.New("sqs splat"),
	}
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS, sqs: fakeQ})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "eventbridge")
	assert.Contains(t, err.Error(), "sns")
	assert.Contains(t, err.Error(), "sqs")
	assert.Empty(t, out, "all three surfaces failed; no rows surface")
}
