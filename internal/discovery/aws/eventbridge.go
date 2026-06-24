// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// eventBridgeEventSourceSurface is the Surface discriminator string for
// AWS EventBridge snapshots. The proposer's recommendation-kind prefix
// routing switches on "eventbridge" → AWS, "pubsub" → GCP, "servicebus"
// → Azure, "streaming" → OCI. Slice 1 chunk 1 of the Event source tier
// arc (v0.89.100, #734 Stream 132).
const eventBridgeEventSourceSurface = "eventbridge"

// eventBridgeSourceTypeBus is the SourceType discriminator string for
// EventBridge event buses. Mirrors the per-cloud "bus" / "topic" /
// "queue" / "namespace" / "stream" SourceType convention documented on
// scanner.EventSourceInstanceSnapshot.
const eventBridgeSourceTypeBus = "bus"

// eventBridgeDefaultRegion is the fallback region the event source
// scanner uses when the supplied ScanScope carries no Regions and the
// scanner's own configured region list is empty. us-east-1 is AWS's
// canonical EventBridge endpoint and matches the slice 1 single-region
// scan posture.
const eventBridgeDefaultRegion = "us-east-1"

// cloudWatchLogsARNPrefix identifies a CloudWatch Logs log-group ARN.
// EventBridge rule targets that point at CloudWatch Logs flip both
// HasLogAxis (structured-logging destination wired) AND HasTraceAxis
// (CloudWatch Logs can carry the X-Ray trace header — the slice 1
// chunk 1 fallback proxy for trace observability readiness when the
// Schemas Discoverer API is deferred to slice 2).
//
// The substring match against the canonical "arn:aws:logs:" prefix is
// region-agnostic and partition-agnostic-enough: the prefix is the
// same across us-east-1, eu-west-2, ap-south-1, etc. Partition-aware
// matching (arn:aws-us-gov:logs:, arn:aws-cn:logs:) is reserved for
// slice 2 when GovCloud + China-region operators surface; the slice 1
// commercial-region posture is the explicit chunk-1 scope.
const cloudWatchLogsARNPrefix = "arn:aws:logs:"

// SchemasDiscovererStateActive is the documented value of the
// EventBridge Schemas Discoverer State field signaling that the
// Discoverer is actively populating the Schemas registry from incoming
// events. Per design doc §3.1 of event-source-tier-slice1.md, an Active
// Discoverer is the proxy for HasTraceAxis (the Schemas registry is
// part of the X-Ray-adjacent observability story on EventBridge).
//
// Slice 1 chunk 1 does NOT consume this constant directly because the
// Schemas API lives in a separate SDK package
// (github.com/aws/aws-sdk-go-v2/service/schemas) and wiring the
// additional client + per-bus DescribeDiscoverer fan-out + IAM action
// set extension would push chunk-1 past its ~1300 LOC budget. The
// constant is kept here as the canonical sentinel for slice 2's direct
// Schemas Discoverer detection — slice 2 will pattern-match against
// this constant in the schemas.ListDiscoverers response.
//
// The chunk-1 fallback (log-target proxy for both HasTraceAxis AND
// HasLogAxis) is documented on scanner.EventSourceInstanceSnapshot
// and threaded through ScanEventBridge below.
const SchemasDiscovererStateActive = "ACTIVE"

// ScanEventSources is the AWS scanner's event-source-tier entry point.
// Slice 1 chunk 1 only covers EventBridge; future slices may add other
// AWS event source primitives (SNS, SQS, IoT events). The method is
// kept narrow so chunk-1 callers see a single dispatch point even as
// the per-surface coverage grows. Mirrors the orchestration tier's
// ScanOrchestrations / ScanStepFunctions layout.
//
// Scope semantics: the scope's Regions[0] (when set) selects the target
// region; an empty Regions list falls back to the scanner's configured
// first region (slice 1 ships single-region scans). The scope's
// AccountID overrides the per-snapshot AccountID stamped on every row;
// empty falls back to the scanner's configured account.
//
// IAM contract per docs/proposals/event-source-tier-slice1.md §12:
// events:ListEventBuses + events:ListRules + events:ListTargetsByRule.
// All three read-only; Squadron never executes an EventBridge mutation
// API.
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	return s.ScanEventBridge(ctx, scope)
}

// ScanEventBridge walks the supplied region's EventBridge event buses
// and returns the mapped event source snapshots. Slice 1 chunk 1 of the
// event-source-tier arc (v0.89.100, #734 Stream 132).
//
// Paginates ListEventBuses via NextToken; per-bus ListRules paginates
// independently; per-rule ListTargetsByRule completes the detection
// chain. Three nested loops are unavoidable here: the EventBridge API
// is intentionally narrow — there is no "describe-all" batch endpoint
// that returns buses + rules + targets in a single call.
//
// Detection per docs/proposals/event-source-tier-slice1.md §3.1
// (slice 1 chunk 1 log-target fallback):
//
//   - HasLogAxis  ← any rule on the bus has a target whose ARN starts
//     with "arn:aws:logs:" (a CloudWatch Logs log-group ARN). A bus
//     with no rules, or rules with no log-group targets, leaves the
//     axis false.
//   - HasTraceAxis ← same as HasLogAxis. The chunk-1 Schemas Discoverer
//     deferral (see the EventBridgeClient godoc and the constant
//     SchemasDiscovererStateActive godoc above) means the log-target
//     proxy carries BOTH axes. Slice 2 will separate the two axes via
//     a direct Schemas Discoverer API call.
//
// Per-bus / per-rule / per-target failures are swallowed inside the
// inner loops — a single failing ListRules or ListTargetsByRule call
// must not abort the whole scan. The bus row still surfaces with its
// universal columns; the axes default to false when the inner walks
// fail to find a positive signal. This matches the orchestration tier's
// "single failing describe must not abort the whole scan" contract.
//
// IAM contract: events:ListEventBuses (bus list pass) + events:ListRules
// (per-bus pass) + events:ListTargetsByRule (per-rule pass). All three
// read-only.
func (s *Scanner) ScanEventBridge(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	region := eventBridgeDefaultRegion
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
	return s.scanRegionEventBridge(ctx, factory, region, accountID)
}

// scanRegionEventBridge runs the per-region list + per-bus per-rule
// describe pass. Extracted from ScanEventBridge so tests can drive the
// inner loop against a fakeFactory without the ensureFactory indirection.
//
// Pagination follows out.NextToken; an empty token signals "no more
// pages" per the AWS SDK convention. A nil token is the sentinel "this
// is the first page" used to skip setting the input's NextToken on the
// initial call.
func (s *Scanner) scanRegionEventBridge(ctx context.Context, factory ClientFactory, region, accountID string) ([]scanner.EventSourceInstanceSnapshot, error) {
	client, err := factory.EventBridge(ctx, region)
	if err != nil {
		return nil, err
	}
	var (
		out       []scanner.EventSourceInstanceSnapshot
		nextToken *string
	)
	for {
		in := &eventbridge.ListEventBusesInput{}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		var listOut *eventbridge.ListEventBusesOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			listOut, e = client.ListEventBuses(ctx, in)
			return e
		})
		if callErr != nil {
			return out, callErr
		}
		for _, bus := range listOut.EventBuses {
			if bus.Arn == nil || *bus.Arn == "" {
				continue
			}
			snap := s.describeEventBus(ctx, client, bus.Arn, bus.Name, accountID, region)
			out = append(out, snap)
		}
		if listOut.NextToken == nil || *listOut.NextToken == "" {
			break
		}
		nextToken = listOut.NextToken
	}
	return out, nil
}

// describeEventBus walks the per-bus rule + target detection chain and
// folds the result into a fully-populated EventSourceInstanceSnapshot.
// Extracted as a standalone helper so the per-axis detection logic is
// independently testable: the slice 1 acceptance tests hit this method
// (and the scanRegionEventBridge wrapper) directly with fixture
// ListRulesOutput + ListTargetsByRuleOutput values, asserting the
// HasTraceAxis / HasLogAxis outcome without spinning up a full scanner.
//
// Per design doc §3.1 + the chunk-1 Schemas Discoverer deferral, the
// detection rule is: any rule on the bus with a target whose ARN starts
// with the CloudWatch Logs ARN prefix ("arn:aws:logs:") flips BOTH
// HasLogAxis and HasTraceAxis. Slice 2 separates the two axes.
//
// The intermediate snapshot is pre-filled from the list-pass fields
// (Provider, Surface, ResourceName, ResourceARN, SourceType, etc.) so a
// failed ListRules or ListTargetsByRule call still leaves the row
// populated with universal columns. Failures DO NOT drop the row — the
// operator sees the bus inventory even when the per-rule detection
// failed; the axes simply default to false.
//
// Pagination follows out.NextToken for ListRules; per-rule
// ListTargetsByRule does not paginate in slice 1 chunk 1 (the API
// supports it but the chunk-1 detection rule short-circuits on the
// first log-target hit, so paginating the targets surface is
// unnecessary work). Slice 2 may add target-list pagination if a per-
// target attribute is needed.
func (s *Scanner) describeEventBus(ctx context.Context, client EventBridgeClient, arn, name *string, accountID, region string) scanner.EventSourceInstanceSnapshot {
	snap := scanner.EventSourceInstanceSnapshot{
		Provider:    string(credstore.ProviderAWS),
		Surface:     eventBridgeEventSourceSurface,
		AccountID:   accountID,
		Region:      region,
		ResourceARN: awssdk.ToString(arn),
		SourceType:  eventBridgeSourceTypeBus,
	}
	if name != nil {
		snap.ResourceName = *name
	}

	ruleCount, hasLogTarget := scanBusRulesForLogTargets(ctx, client, name)
	if hasLogTarget {
		snap.HasLogAxis = true
		// Slice 1 chunk 1 fallback: a bus with a log target is the
		// proxy for trace observability readiness since CloudWatch
		// Logs can carry the X-Ray trace header. The Schemas
		// Discoverer-based detection lands in slice 2 and will
		// separate this from HasLogAxis.
		snap.HasTraceAxis = true
	}
	snap.Detail = map[string]any{
		"rule_count": ruleCount,
	}
	return snap
}

// scanBusRulesForLogTargets walks every rule on the supplied bus and
// returns the total rule count plus whether any rule has a CloudWatch
// Logs target. Extracted as a free function so the inner detection
// logic is straightforward to test in isolation.
//
// Per-rule ListTargetsByRule failures short-circuit to the next rule —
// a single failing target list must not abort the whole bus walk.
// Pagination follows out.NextToken on the ListRules pass; the
// per-rule target walk does NOT paginate (the detection short-circuits
// on the first log-target hit, so pagination of the targets surface
// is unnecessary work in chunk 1).
//
// Returns (ruleCount, hasLogTarget). When the ListRules call itself
// fails, returns (0, false) — the bus snapshot still surfaces with its
// universal columns and the axes default to false.
func scanBusRulesForLogTargets(ctx context.Context, client EventBridgeClient, busName *string) (int, bool) {
	var (
		ruleCount    int
		hasLogTarget bool
		nextToken    *string
	)
	for {
		in := &eventbridge.ListRulesInput{
			EventBusName: busName,
		}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		var rulesOut *eventbridge.ListRulesOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			rulesOut, e = client.ListRules(ctx, in)
			return e
		})
		if callErr != nil {
			return ruleCount, hasLogTarget
		}
		for _, rule := range rulesOut.Rules {
			ruleCount++
			if hasLogTarget {
				// Short-circuit: we already proved the axis; keep
				// counting rules for the Detail bag but skip the
				// per-rule target call.
				continue
			}
			targetsOut, terr := client.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{
				Rule:         rule.Name,
				EventBusName: busName,
			})
			if terr != nil {
				continue
			}
			for _, target := range targetsOut.Targets {
				if target.Arn == nil {
					continue
				}
				if strings.HasPrefix(*target.Arn, cloudWatchLogsARNPrefix) {
					hasLogTarget = true
					break
				}
			}
		}
		if rulesOut.NextToken == nil || *rulesOut.NextToken == "" {
			break
		}
		nextToken = rulesOut.NextToken
	}
	return ruleCount, hasLogTarget
}
