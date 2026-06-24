// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSNS is the test double satisfying SNSClient. Mirrors fakeEventBridge's
// shape — a map of pre-populated ListTopics pages plus a per-ARN
// attribute response map drives the per-test behaviour.
type fakeSNS struct {
	listTopicsPages   []*sns.ListTopicsOutput
	listTopicsCallIdx int
	listTopicsErr     error
	listTopicsCalls   int

	// attrsByARN maps topic ARN → attribute map response. Missing →
	// returns an empty attributes map (no error). attrsErrByARN
	// overrides with a per-ARN error when set.
	attrsByARN    map[string]map[string]string
	attrsErrByARN map[string]error
	getAttrsCalls int
}

func (f *fakeSNS) ListTopics(_ context.Context, _ *sns.ListTopicsInput, _ ...func(*sns.Options)) (*sns.ListTopicsOutput, error) {
	f.listTopicsCalls++
	if f.listTopicsErr != nil {
		return nil, f.listTopicsErr
	}
	if f.listTopicsCallIdx >= len(f.listTopicsPages) {
		return &sns.ListTopicsOutput{}, nil
	}
	out := f.listTopicsPages[f.listTopicsCallIdx]
	f.listTopicsCallIdx++
	if out == nil {
		return &sns.ListTopicsOutput{}, nil
	}
	return out, nil
}

func (f *fakeSNS) GetTopicAttributes(_ context.Context, in *sns.GetTopicAttributesInput, _ ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error) {
	f.getAttrsCalls++
	arn := awssdk.ToString(in.TopicArn)
	if err, ok := f.attrsErrByARN[arn]; ok {
		return nil, err
	}
	if attrs, ok := f.attrsByARN[arn]; ok {
		return &sns.GetTopicAttributesOutput{Attributes: attrs}, nil
	}
	return &sns.GetTopicAttributesOutput{Attributes: map[string]string{}}, nil
}

// Compile-time check that fakeSNS satisfies the SNSClient interface.
var _ SNSClient = (*fakeSNS)(nil)

// makeSNSTopic produces a single snstypes.Topic for the ListTopics page.
func makeSNSTopic(arn string) snstypes.Topic {
	return snstypes.Topic{TopicArn: awssdk.String(arn)}
}

// runSNSScan is the shared harness — wires a fake SNS client into a
// fresh Scanner via the test factory builder and calls ScanSNSTopics
// against us-east-1.
func runSNSScan(t *testing.T, fakeClient *fakeSNS) []scanner.EventSourceInstanceSnapshot {
	t.Helper()
	factory := &fakeFactory{sns: fakeClient}
	s := newTestScanner(t, factory)
	out, err := s.ScanSNSTopics(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	return out
}

// --- Tests ------------------------------------------------------------

// TestScanSNSTopics_ListReturnsTopics_Paginated — slice 3 acceptance
// test 1: a multi-page ListTopics response is walked to completion;
// every topic surfaces with the universal columns populated.
func TestScanSNSTopics_ListReturnsTopics_Paginated(t *testing.T) {
	const (
		arnA = "arn:aws:sns:us-east-1:123456789012:topic-a"
		arnB = "arn:aws:sns:us-east-1:123456789012:topic-b"
		arnC = "arn:aws:sns:us-east-1:123456789012:topic-c"
	)
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arnA), makeSNSTopic(arnB)}, NextToken: awssdk.String("page2")},
			{Topics: []snstypes.Topic{makeSNSTopic(arnC)}},
		},
		attrsByARN: map[string]map[string]string{
			arnA: {},
			arnB: {},
			arnC: {},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 3, "pagination must walk every topic across both pages")
	assert.Equal(t, 2, fakeClient.listTopicsCalls, "both list pages must be requested")
	assert.Equal(t, arnA, out[0].ResourceARN)
	assert.Equal(t, arnB, out[1].ResourceARN)
	assert.Equal(t, arnC, out[2].ResourceARN)
}

// TestScanSNSTopics_TopicWithSubsConfirmed_HasTraceAxis — slice 3
// acceptance test 2 helper: a topic with SubscriptionsConfirmed > 0
// flips HasTraceAxis true. Subscriptions_confirmed Detail key carries
// the parsed integer.
func TestScanSNSTopics_TopicWithSubsConfirmed_HasTraceAxis(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:active-topic"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
		attrsByARN: map[string]map[string]string{
			arn: {"SubscriptionsConfirmed": "3"},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.True(t, out[0].HasTraceAxis, "trace axis flips on SubscriptionsConfirmed > 0")
	assert.Equal(t, 3, out[0].Detail["subscriptions_confirmed"])
}

// TestScanSNSTopics_TopicWithZeroSubs_NoTraceAxis — slice 3 acceptance
// test 3 helper: SubscriptionsConfirmed == 0 leaves HasTraceAxis false.
// The chunk-2 sns-subscriptions-attach recommendation engine consumes
// this state.
func TestScanSNSTopics_TopicWithZeroSubs_NoTraceAxis(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:orphan-topic"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
		attrsByARN: map[string]map[string]string{
			arn: {"SubscriptionsConfirmed": "0"},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasTraceAxis, "trace axis stays false when SubscriptionsConfirmed == 0")
	assert.Equal(t, 0, out[0].Detail["subscriptions_confirmed"])
}

// TestScanSNSTopics_TopicWithDeliveryFeedbackRole_HasLogAxis — slice 3
// acceptance test 4 helper: any per-protocol delivery feedback role
// ARN attribute flips HasLogAxis true.
func TestScanSNSTopics_TopicWithDeliveryFeedbackRole_HasLogAxis(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:logged-topic"
	cases := []struct {
		name string
		attr string
	}{
		{"http success", "HTTPSuccessFeedbackRoleArn"},
		{"sqs failure", "SQSFailureFeedbackRoleArn"},
		{"lambda success", "LambdaSuccessFeedbackRoleArn"},
		{"firehose failure", "FirehoseFailureFeedbackRoleArn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := &fakeSNS{
				listTopicsPages: []*sns.ListTopicsOutput{
					{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
				},
				attrsByARN: map[string]map[string]string{
					arn: {tc.attr: "arn:aws:iam::123456789012:role/SNSDeliveryLogger"},
				},
			}
			out := runSNSScan(t, fakeClient)
			require.Len(t, out, 1)
			assert.True(t, out[0].HasLogAxis, "log axis must flip when %s is set", tc.attr)
		})
	}
}

// TestScanSNSTopics_TopicWithoutDeliveryFeedbackRole_NoLogAxis — when
// none of the per-protocol feedback role ARN attributes are set, the
// log axis stays false. Subscriptions confirmed alone does NOT satisfy
// the log axis.
func TestScanSNSTopics_TopicWithoutDeliveryFeedbackRole_NoLogAxis(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:no-logging-topic"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
		attrsByARN: map[string]map[string]string{
			arn: {"SubscriptionsConfirmed": "5"},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.False(t, out[0].HasLogAxis, "log axis stays false without a delivery feedback role")
	assert.True(t, out[0].HasTraceAxis, "trace axis still flips on subscriptions confirmed")
}

// TestScanSNSTopics_TopicWithKMS_DetailRecordsKmsKeyId — slice 3
// acceptance test 5: when KmsMasterKeyId is set, the snapshot Detail
// records the kms_master_key_id flag.
func TestScanSNSTopics_TopicWithKMS_DetailRecordsKmsKeyId(t *testing.T) {
	const (
		arn    = "arn:aws:sns:us-east-1:123456789012:encrypted-topic"
		kmsKey = "alias/aws/sns"
	)
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
		attrsByARN: map[string]map[string]string{
			arn: {"KmsMasterKeyId": kmsKey},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, kmsKey, out[0].Detail["kms_master_key_id"])
}

// TestScanSNSTopics_FifoTopicWithContentDedup_DetailRecordsDedup —
// slice 3 acceptance test 6: a FIFO topic with ContentBasedDeduplication
// records both fifo_topic and content_based_deduplication flags.
func TestScanSNSTopics_FifoTopicWithContentDedup_DetailRecordsDedup(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:fifo-topic.fifo"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
		attrsByARN: map[string]map[string]string{
			arn: {
				"FifoTopic":                 "true",
				"ContentBasedDeduplication": "true",
			},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["fifo_topic"])
	assert.Equal(t, true, out[0].Detail["content_based_deduplication"])
}

// TestScanSNSTopics_FifoTopicWithoutDedup_NoDedupFlag — a FIFO topic
// without content-based dedup records only the fifo_topic flag; the
// content_based_deduplication key is absent rather than false.
func TestScanSNSTopics_FifoTopicWithoutDedup_NoDedupFlag(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:fifo-only.fifo"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
		attrsByARN: map[string]map[string]string{
			arn: {"FifoTopic": "true"},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0].Detail["fifo_topic"])
	_, present := out[0].Detail["content_based_deduplication"]
	assert.False(t, present, "absent rather than false when dedup is not set")
}

// TestScanSNSTopics_GetAttributesFailureNonFatal — partial-scan posture
// on the per-topic level. When GetTopicAttributes fails on one topic,
// that topic surfaces with the attribute_fetch_failed Detail flag set;
// remaining topics surface normally. Slice 3 §5 contract.
func TestScanSNSTopics_GetAttributesFailureNonFatal(t *testing.T) {
	const (
		arnGood = "arn:aws:sns:us-east-1:123456789012:good"
		arnBad  = "arn:aws:sns:us-east-1:123456789012:bad"
	)
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arnGood), makeSNSTopic(arnBad)}},
		},
		attrsByARN: map[string]map[string]string{
			arnGood: {"SubscriptionsConfirmed": "2"},
		},
		attrsErrByARN: map[string]error{
			arnBad: errors.New("simulated GetTopicAttributes failure"),
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 2, "both rows surface; failing one still produces a snapshot")
	// Good row has the parsed integer.
	assert.Equal(t, arnGood, out[0].ResourceARN)
	assert.True(t, out[0].HasTraceAxis)
	// Bad row carries the failure flag.
	assert.Equal(t, arnBad, out[1].ResourceARN)
	assert.Equal(t, true, out[1].Detail["attribute_fetch_failed"])
	assert.False(t, out[1].HasTraceAxis)
	assert.False(t, out[1].HasLogAxis)
}

// TestScanSNSTopics_ResourceNamePopulatedFromARN — the ResourceName
// field carries the trailing ARN segment so the UI doesn't have to
// parse ARNs.
func TestScanSNSTopics_ResourceNamePopulatedFromARN(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:my-named-topic"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, "my-named-topic", out[0].ResourceName)
}

// TestScanSNSTopics_SourceTypeIsTopic — the SourceType discriminator
// is "topic" so the UI renders SNS rows under the canonical sub-type
// column.
func TestScanSNSTopics_SourceTypeIsTopic(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:any"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, "topic", out[0].SourceType)
}

// TestScanSNSTopics_SurfaceIsSns — the Surface discriminator is "sns"
// so the proposer's webhook router (chunk 2) routes sns- prefixes to
// AWS.
func TestScanSNSTopics_SurfaceIsSns(t *testing.T) {
	const arn = "arn:aws:sns:us-east-1:123456789012:any"
	fakeClient := &fakeSNS{
		listTopicsPages: []*sns.ListTopicsOutput{
			{Topics: []snstypes.Topic{makeSNSTopic(arn)}},
		},
	}
	out := runSNSScan(t, fakeClient)
	require.Len(t, out, 1)
	assert.Equal(t, "sns", out[0].Surface)
	assert.Equal(t, "aws", out[0].Provider)
}

// TestScanSNSTopics_SNSClientNil_GracefullyReturnsEmpty — when the
// factory's SNS client returns nil (no client wired), the scan
// gracefully returns an empty result rather than nil-panicking. Matches
// the existing scanner posture for unwired clients.
func TestScanSNSTopics_SNSClientNil_GracefullyReturnsEmpty(t *testing.T) {
	factory := &nilSNSFactory{}
	s := newTestScanner(t, factory)
	out, err := s.ScanSNSTopics(context.Background(), scanner.ScanScope{
		Regions:   []string{"us-east-1"},
		AccountID: "123456789012",
	})
	require.NoError(t, err)
	assert.Empty(t, out)
}

// nilSNSFactory wraps the default fake factory but explicitly returns
// nil for the SNS client to exercise the graceful-nil branch.
type nilSNSFactory struct {
	fakeFactory
}

func (f *nilSNSFactory) SNS(_ context.Context, _ string) (SNSClient, error) {
	return nil, nil
}

// TestSNSTopicNameFromARN_ExtractsLastSegment — the bare helper for
// the ARN → topic name extraction. Defensive against empty input.
func TestSNSTopicNameFromARN_ExtractsLastSegment(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"canonical six-segment", "arn:aws:sns:us-east-1:123456789012:my-topic", "my-topic"},
		{"fifo suffix preserved", "arn:aws:sns:us-east-1:123456789012:orders.fifo", "orders.fifo"},
		{"empty", "", ""},
		{"no colons", "bare-name", "bare-name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, snsTopicNameFromARN(tc.in))
		})
	}
}

// TestBuildSNSSnapshot_NonIntegerSubscriptionsConfirmed_AxisStaysFalse
// — defensive: a SubscriptionsConfirmed value that doesn't parse as an
// integer leaves the axis false rather than panicking.
func TestBuildSNSSnapshot_NonIntegerSubscriptionsConfirmed_AxisStaysFalse(t *testing.T) {
	snap := buildSNSSnapshot("123456789012", "us-east-1",
		"arn:aws:sns:us-east-1:123456789012:weird",
		map[string]string{"SubscriptionsConfirmed": "not-a-number"})
	assert.False(t, snap.HasTraceAxis)
	// And the Detail subscriptions_confirmed key stays absent.
	_, present := snap.Detail["subscriptions_confirmed"]
	assert.False(t, present)
}
