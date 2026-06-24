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

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Slice 3 dispatcher tests ----------------------------------------
//
// ScanEventSources fans out across EventBridge + SNS with a
// partial-scan posture. Tests 7 / 8 / 9 of the slice 3 design doc pin
// the contract: both surfaces dispatched independently; either failing
// does NOT block the other; only when BOTH fail does the dispatcher
// return a non-nil error wrapping the per-surface causes.

// TestScanEventSources_DispatchesToBothEventBridgeAndSNS — slice 3
// acceptance test 7: the dispatcher returns BOTH buses AND topics
// when both surfaces produce data.
func TestScanEventSources_DispatchesToBothEventBridgeAndSNS(t *testing.T) {
	const (
		busARN   = "arn:aws:events:us-east-1:123456789012:event-bus/default"
		topicARN = "arn:aws:sns:us-east-1:123456789012:orders"
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
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	require.Len(t, out, 2, "dispatcher must return BOTH bus and topic")

	// Surface order: EventBridge first, then SNS.
	assert.Equal(t, "eventbridge", out[0].Surface)
	assert.Equal(t, busARN, out[0].ResourceARN)
	assert.Equal(t, "sns", out[1].Surface)
	assert.Equal(t, topicARN, out[1].ResourceARN)
}

// TestScanEventSources_EventBridgeFailureSNSStillSurfaces — slice 3
// acceptance test 8: the partial-scan posture in the
// EventBridge-fails-but-SNS-succeeds direction. The SNS topic rows
// still surface; the dispatcher returns no error because at least one
// surface succeeded.
func TestScanEventSources_EventBridgeFailureSNSStillSurfaces(t *testing.T) {
	const topicARN = "arn:aws:sns:us-east-1:123456789012:still-here"
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
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err, "partial-scan posture: only one surface failed")
	require.Len(t, out, 1, "SNS topic still surfaces")
	assert.Equal(t, "sns", out[0].Surface)
	assert.Equal(t, topicARN, out[0].ResourceARN)
}

// TestScanEventSources_SNSFailureEventBridgeStillSurfaces — slice 3
// acceptance test 9: the partial-scan posture in the
// SNS-fails-but-EventBridge-succeeds direction. The EventBridge bus
// rows still surface; the dispatcher returns no error because at
// least one surface succeeded.
func TestScanEventSources_SNSFailureEventBridgeStillSurfaces(t *testing.T) {
	const busARN = "arn:aws:events:us-east-1:123456789012:event-bus/default"
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
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err, "partial-scan posture: only one surface failed")
	require.Len(t, out, 1, "EventBridge bus still surfaces")
	assert.Equal(t, "eventbridge", out[0].Surface)
	assert.Equal(t, busARN, out[0].ResourceARN)
}

// TestScanEventSources_BothFailReturnsErrorWithBothMessages — the
// dispatcher's only error-returning path: BOTH surfaces fail. The
// returned error must mention both eventbridge and sns so the
// operator-facing error message captures the full failure envelope.
func TestScanEventSources_BothFailReturnsErrorWithBothMessages(t *testing.T) {
	fakeEB := &fakeEventBridge{
		listBusesErr: errors.New("eventbridge boom"),
	}
	fakeS := &fakeSNS{
		listTopicsErr: errors.New("sns kaboom"),
	}
	s := newTestScanner(t, &fakeFactory{eventbridge: fakeEB, sns: fakeS})
	out, err := s.ScanEventSources(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "eventbridge")
	assert.Contains(t, err.Error(), "sns")
	assert.Empty(t, out, "both surfaces failed; no rows surface")
}
