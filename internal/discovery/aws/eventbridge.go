// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

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

// EventBridgeXRayTraceHeaderName is the AWS-defined string that names the
// X-Ray trace ID in EventBridge event metadata. Slice 2 chunk 1 of the
// Event source tier arc (v0.89.105, #741 Stream 139) detects per-rule
// trace propagation by inspecting whether an InputTransformer template
// references this string. The check is case-insensitive — the template
// authors in the wild sometimes use the upper-case AWS canonical
// (X-Amzn-Trace-Id) and sometimes the lower-case HTTP-header convention.
//
// See docs/proposals/event-source-tier-slice2.md §3.1 for the detection
// logic and §12 for the false-positive risk note (operators using a
// different mechanism to preserve trace context won't match the
// heuristic).
const EventBridgeXRayTraceHeaderName = "x-amzn-trace-id"

// EventBridgeTraceparentHeaderName is the W3C trace context header name
// some operators use INSTEAD of the AWS X-Ray header for
// OpenTelemetry-native flows. Slice 2 chunk 1 detects either header
// substring in an InputTransformer template — the proposer's
// recommendation kind (chunk 5) covers both posture conventions without
// forcing operators onto one or the other.
const EventBridgeTraceparentHeaderName = "traceparent"

// rulePreservesTracePropagation applies the slice 2 chunk 1 per-target
// propagation detection rule. Returns (preserved, note). preserved is
// true when the target's InputPath / InputTransformer config preserves
// trace context end-to-end; note is empty when preserved or a
// human-readable per-issue string when broken.
//
// Detection rules (docs/proposals/event-source-tier-slice2.md §3.1):
//
//  1. No InputPath AND no InputTransformer → the full event flows
//     including the X-Ray trace header. PROPAGATION PRESERVED.
//  2. InputPath == "$" → same as no path; full event flows.
//     PROPAGATION PRESERVED.
//  3. InputPath set to anything other than "$" (e.g. "$.detail") →
//     a narrowed projection that strips the top-level X-Ray trace
//     header. PROPAGATION BROKEN.
//  4. InputTransformer present → check the InputTemplate for a
//     literal substring match against either the X-Ray header
//     (x-amzn-trace-id) or the W3C traceparent header. Match →
//     PRESERVED; no match → BROKEN. The substring match is the
//     heuristic the design doc §3.1 names; §12 documents the
//     false-positive risk for transformers that preserve trace
//     context via a different (e.g. JSON-encoded <traceparent>)
//     mechanism — the chunk-5 exclusion table absorbs those.
//
// The check is per-target. A rule with multiple targets is preserved
// when EVERY target is preserved; a single broken target on a rule
// produces a broken note for that rule. The bus-level
// HasPropagationConfig axis (computed by the caller) is the AND across
// every target of every rule — propagation is a worst-case axis.
func rulePreservesTracePropagation(ruleName string, inputPath *string, inputTransformer *ebtypes.InputTransformer) (bool, string) {
	// Case 1: no InputPath and no InputTransformer → full event flows.
	if inputPath == nil && inputTransformer == nil {
		return true, ""
	}

	// Case 2: InputPath = "$" → same as no path; full event flows.
	if inputPath != nil && awssdk.ToString(inputPath) == "$" {
		// An InputTransformer may still be set alongside — but the
		// EventBridge API documents these as mutually exclusive in
		// practice (the rule's input config picks one). Treat the
		// dollar-path as authoritative when both are set; the
		// transformer pathway below would re-apply if it weren't.
		if inputTransformer == nil {
			return true, ""
		}
		// Defensive: both are set. Fall through to the transformer
		// check below; the transformer's template is the load-bearing
		// shape when AWS evaluates the rule.
	}

	// Case 3: InputPath set to anything other than "$" → strips the
	// top-level event metadata (where the X-Ray header lives).
	if inputPath != nil && awssdk.ToString(inputPath) != "$" && inputTransformer == nil {
		return false, fmt.Sprintf("rule %q has InputPath %q that strips trace header",
			ruleName, awssdk.ToString(inputPath))
	}

	// Case 4: InputTransformer present → heuristic substring match.
	if inputTransformer != nil {
		template := awssdk.ToString(inputTransformer.InputTemplate)
		lowerTemplate := strings.ToLower(template)
		if strings.Contains(lowerTemplate, EventBridgeXRayTraceHeaderName) ||
			strings.Contains(lowerTemplate, EventBridgeTraceparentHeaderName) {
			return true, ""
		}
		return false, fmt.Sprintf("rule %q has InputTransformer template omitting trace header",
			ruleName)
	}

	// Defensive fallback: should not reach here given the cases above,
	// but if any path slips through, default to preserved (don't emit
	// a false-positive recommendation against an unrecognized config).
	return true, ""
}

// ScanEventSources is the AWS scanner's event-source-tier entry point.
// Slice 1 chunk 1 (v0.89.100) shipped EventBridge alone; slice 3 chunk
// 1 (v0.89.138, #778 Stream 176) extends the dispatcher to fan out
// across BOTH EventBridge AND SNS topics, with a partial-scan posture
// that lets either surface fail independently without aborting the
// other.
//
// Scope semantics: the scope's Regions[0] (when set) selects the target
// region; an empty Regions list falls back to the scanner's configured
// first region (slice 1 ships single-region scans). The scope's
// AccountID overrides the per-snapshot AccountID stamped on every row;
// empty falls back to the scanner's configured account. Both surfaces
// receive the identical scope.
//
// Partial-scan posture per docs/proposals/event-source-tier-slice3.md
// §5: when ONE surface fails (e.g. an IAM gap on the SNS read
// permissions while the EventBridge permissions are already wired)
// the OTHER surface's results still surface. Only when BOTH surfaces
// fail does the dispatcher return a non-nil error — wrapping both
// per-surface errors so the operator can see the full failure
// envelope. The §12 threat model treats this as load-bearing: the
// dispatcher's both-directions partial-scan posture is pinned by
// acceptance tests 7 / 8 / 9 of the slice 3 design doc.
//
// IAM contract per docs/proposals/event-source-tier-slice1.md §12 +
// event-source-tier-slice3.md §12:
//   - events:ListEventBuses + events:ListRules + events:ListTargetsByRule
//   - sns:ListTopics + sns:GetTopicAttributes
//
// All five read-only. Squadron never executes an EventBridge or SNS
// mutation API.
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
	var all []scanner.EventSourceInstanceSnapshot

	buses, ebErr := s.ScanEventBridge(ctx, scope)
	if ebErr == nil {
		all = append(all, buses...)
	}

	topics, snsErr := s.ScanSNSTopics(ctx, scope)
	if snsErr == nil {
		all = append(all, topics...)
	}

	// Partial-scan posture: only return an error when BOTH surfaces
	// failed. Either-direction-failure is silenced at this layer so a
	// single-surface IAM gap doesn't drop the inventory the operator
	// actually CAN see. Tests 8 + 9 of the slice 3 design doc pin
	// both directions.
	if ebErr != nil && snsErr != nil {
		return all, fmt.Errorf("event sources scan failures: eventbridge=%w sns=%v", ebErr, snsErr)
	}

	return all, nil
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

	ruleCount, hasLogTarget, propagationOK, propagationNotes := scanBusRulesForLogTargets(ctx, client, name)
	if hasLogTarget {
		snap.HasLogAxis = true
		// Slice 1 chunk 1 fallback: a bus with a log target is the
		// proxy for trace observability readiness since CloudWatch
		// Logs can carry the X-Ray trace header. The Schemas
		// Discoverer-based detection lands in slice 2 and will
		// separate this from HasLogAxis.
		snap.HasTraceAxis = true
	}
	// Slice 2 chunk 1 (v0.89.105, #741 Stream 139): per-rule propagation
	// detection. The bus-level axis is the AND across every target of
	// every rule — propagation is a worst-case axis (any broken target
	// fails the whole bus). A bus with no rules / no targets defaults
	// to true (vacuously preserved — there's nothing to break
	// propagation), which is what scanBusRulesForLogTargets returns
	// when the per-rule walk finds no broken cases.
	snap.HasPropagationConfig = propagationOK
	if len(propagationNotes) > 0 {
		snap.PropagationNotes = propagationNotes
	}
	snap.Detail = map[string]any{
		"rule_count": ruleCount,
	}
	return snap
}

// scanBusRulesForLogTargets walks every rule on the supplied bus and
// returns (1) the total rule count, (2) whether any rule has a
// CloudWatch Logs target (the slice-1 log-axis proxy), (3) whether
// every target on every rule preserves trace propagation (the slice-2
// chunk-1 propagation axis), and (4) the per-issue propagation notes
// accumulated across broken targets.
//
// Per-rule ListTargetsByRule failures short-circuit to the next rule —
// a single failing target list must not abort the whole bus walk.
// Pagination follows out.NextToken on the ListRules pass; the per-rule
// target walk does NOT paginate (slice 1's first-log-target-hit short-
// circuit is preserved on the log axis; slice 2's propagation check
// still iterates every target on the page, but the chunk-1 budget
// keeps target-list pagination as slice-3 work).
//
// Returns (ruleCount, hasLogTarget, propagationOK, propagationNotes).
// When the ListRules call itself fails, returns
// (0, false, true, nil) — the bus snapshot still surfaces with its
// universal columns; the log axis stays false; the propagation axis
// defaults to true (vacuously preserved, no rules observed).
//
// SLICE 2 COMPOSITION NOTE: the log-target short-circuit (once
// hasLogTarget flips true, the LogAxis is proven) is preserved on the
// log axis only — the slice 2 propagation pass DOES iterate every
// rule's targets even after the log axis flips, because per-target
// propagation is a worst-case AND across the whole bus. The previous
// chunk-1 implementation skipped the ListTargetsByRule call entirely
// for subsequent rules once hasLogTarget proved true; slice 2 has to
// remove that short-circuit because propagation cannot be proven
// without inspecting every target. The cost is one extra API call per
// rule per bus per scan — measured in the design doc §12 threat model
// as acceptable (EventBridge rule counts in production deployments
// rarely exceed low double-digits per bus).
func scanBusRulesForLogTargets(ctx context.Context, client EventBridgeClient, busName *string) (int, bool, bool, []string) {
	var (
		ruleCount        int
		hasLogTarget     bool
		propagationOK    = true // vacuously true; flips false on first broken target
		propagationNotes []string
		nextToken        *string
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
			return ruleCount, hasLogTarget, propagationOK, propagationNotes
		}
		for _, rule := range rulesOut.Rules {
			ruleCount++
			targetsOut, terr := client.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{
				Rule:         rule.Name,
				EventBusName: busName,
			})
			if terr != nil {
				continue
			}
			ruleName := awssdk.ToString(rule.Name)
			for _, target := range targetsOut.Targets {
				// Slice-1 log-axis check: any target on any rule with
				// a CloudWatch Logs ARN flips the axis. Cheap string
				// prefix match; no SDK call needed.
				if !hasLogTarget && target.Arn != nil &&
					strings.HasPrefix(*target.Arn, cloudWatchLogsARNPrefix) {
					hasLogTarget = true
				}
				// Slice-2 propagation check: per-target inspection of
				// InputPath / InputTransformer. The bus-level axis is
				// the AND across every target of every rule, so each
				// broken target contributes a note AND flips the bus
				// axis to false. Multiple broken targets on the same
				// rule each emit their own note (the proposer's chunk
				// 5 reasoning text walks the full notes list).
				preserved, note := rulePreservesTracePropagation(
					ruleName, target.InputPath, target.InputTransformer)
				if !preserved {
					propagationOK = false
					if note != "" {
						propagationNotes = append(propagationNotes, note)
					}
				}
			}
		}
		if rulesOut.NextToken == nil || *rulesOut.NextToken == "" {
			break
		}
		nextToken = rulesOut.NextToken
	}
	return ruleCount, hasLogTarget, propagationOK, propagationNotes
}
