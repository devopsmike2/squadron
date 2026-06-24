// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// stepFunctionsOrchestrationSurface is the Surface discriminator string
// for AWS Step Functions snapshots. The proposer's recommendation-kind
// prefix routing switches on "stepfunc" → AWS, "workflows" → GCP,
// "logicapps" → Azure. Slice 1 chunk 1 of the Orchestration tier arc
// (v0.89.95, #728 Stream 126).
const stepFunctionsOrchestrationSurface = "stepfunc"

// stepFunctionsDefaultRegion is the fallback region the orchestration
// scanner uses when the supplied ScanScope carries no Regions and the
// scanner's own configured region list is empty. us-east-1 is AWS's
// canonical Step Functions endpoint and matches the slice 1 single-
// region scan posture.
const stepFunctionsDefaultRegion = "us-east-1"

// ScanOrchestrations is the AWS scanner's orchestration-tier entry
// point. Slice 1 chunk 1 only covers Step Functions; future slices may
// add other AWS orchestration primitives (Amazon MWAA, EventBridge
// Scheduler). The method is kept narrow so chunk-1 callers see a
// single dispatch point even as the per-surface coverage grows.
//
// Mirrors the OCI scanner's ScanFunctions / ScanServerless layout: a
// standalone Scanner method that returns the slice of snapshots
// directly rather than threading them through the existing Scan()
// per-region loop. This keeps the chunk-1 wiring small and lets the
// handler dispatch orchestration scans on the tier filter alone.
//
// Scope semantics: the scope's Regions[0] (when set) selects the target
// region; an empty Regions list falls back to the scanner's configured
// first region (slice 1 ships single-region scans). The scope's
// AccountID overrides the per-snapshot AccountID stamped on every row;
// empty falls back to the scanner's configured account.
//
// Returns the snapshots verbatim; per-machine describe failures are
// swallowed inside the inner loop (the slice 1 acceptance test 8
// contract: a single failing DescribeStateMachine call must not abort
// the whole scan).
//
// IAM contract per docs/proposals/orchestration-tier-slice1.md §3.1:
// states:ListStateMachines + states:DescribeStateMachine. Both
// read-only; Squadron never executes a state-machine mutation API.
func (s *Scanner) ScanOrchestrations(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
	return s.ScanStepFunctions(ctx, scope)
}

// ScanStepFunctions walks the supplied region's Step Functions state
// machines and returns the mapped orchestration snapshots. Slice 1
// chunk 1 of the orchestration-tier arc (v0.89.95, #728 Stream 126).
//
// Paginates ListStateMachines via NextToken; per-machine
// DescribeStateMachine fans out one call per state machine to capture
// the per-axis configuration (TracingConfiguration for HasTraceAxis,
// LoggingConfiguration for HasLogAxis). The describe fan-out is
// unavoidable: the list response is intentionally slim (ARN + Name +
// Type only) so AWS doesn't have to hydrate per-machine config for
// callers that only need the inventory.
//
// Detection per docs/proposals/orchestration-tier-slice1.md §3.1:
//
//   - HasTraceAxis ← TracingConfiguration != nil &&
//     TracingConfiguration.Enabled == true. Matches X-Ray active
//     tracing on a Lambda. A nil TracingConfiguration is the
//     documented Step Functions default (no X-Ray); we treat it as
//     "not active" without conflating "unset" with "disabled".
//   - HasLogAxis  ← LoggingConfiguration != nil &&
//     LoggingConfiguration.Level != OFF && Level != "". The empty-
//     string guard catches state machines created before AWS pinned
//     the Level enum default — Squadron treats unset as "off" rather
//     than as a partial match. AWS publishes four LogLevel values:
//     ALL, ERROR, FATAL, OFF; any of the first three flips the axis.
//
// EXPRESS coverage caveat (design doc §12): EXPRESS workflows surface
// the same TracingConfiguration + LoggingConfiguration fields as
// STANDARD workflows. Slice 1 treats them identically; if operator
// feedback shows EXPRESS-shop coverage is over- or under-counted, the
// slice 2 prompt extension can introduce a surface-type-aware variant.
// The per-machine WorkflowType ("STANDARD" or "EXPRESS") is carried
// through to the snapshot so the proposer can route to the right
// recommendation kind regardless.
//
// IAM contract: states:ListStateMachines (list pass) + states:DescribeStateMachine
// (per-machine pass). Both read-only.
func (s *Scanner) ScanStepFunctions(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error) {
	region := stepFunctionsDefaultRegion
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
	return s.scanRegionStepFunctions(ctx, factory, region, accountID)
}

// scanRegionStepFunctions runs the per-region list + per-machine
// describe pass. Extracted from ScanStepFunctions so tests can drive
// the inner loop against a fakeFactory without the ensureFactory
// indirection.
//
// Pagination follows out.NextToken; an empty token signals "no more
// pages" per the AWS SDK convention. A nil token is a sentinel "this
// is the first page" used to skip setting the input's NextToken on
// the initial call (the SDK rejects an explicit empty-string token).
//
// Per-machine describe failures are caught at the inner err check and
// the loop continues with the next machine. This matches the slice 1
// acceptance test 8 contract: a single failing describe must not
// abort the whole scan.
func (s *Scanner) scanRegionStepFunctions(ctx context.Context, factory ClientFactory, region, accountID string) ([]scanner.OrchestrationInstanceSnapshot, error) {
	client, err := factory.SFN(ctx, region)
	if err != nil {
		return nil, err
	}
	var (
		out       []scanner.OrchestrationInstanceSnapshot
		nextToken *string
	)
	for {
		in := &sfn.ListStateMachinesInput{}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		var listOut *sfn.ListStateMachinesOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			listOut, e = client.ListStateMachines(ctx, in)
			return e
		})
		if callErr != nil {
			return out, callErr
		}
		for _, sm := range listOut.StateMachines {
			if sm.StateMachineArn == nil {
				continue
			}
			snap, derr := s.describeStateMachine(ctx, client, sm, accountID, region)
			if derr != nil {
				// Per-machine describe failure: skip the row and
				// continue. The slice 1 acceptance test 8 contract
				// explicitly excludes a single failing describe from
				// aborting the whole scan — the operator gets a
				// short-by-one inventory rather than a 500. Future
				// slices can surface a per-machine failed_resources
				// counter for the proposer's learning loop.
				continue
			}
			out = append(out, snap)
		}
		if listOut.NextToken == nil || *listOut.NextToken == "" {
			break
		}
		nextToken = listOut.NextToken
	}
	return out, nil
}

// describeStateMachine pulls the per-machine TracingConfiguration +
// LoggingConfiguration via DescribeStateMachine and folds the result
// into a fully-populated OrchestrationInstanceSnapshot. Extracted as a
// standalone helper so the per-axis detection logic is independently
// testable: the slice 1 acceptance tests 1-5 hit applyStepFunctionsDescription
// directly with fixture DescribeStateMachineOutput values, asserting
// the HasTraceAxis / HasLogAxis outcome without spinning up a full
// scanner.
//
// The intermediate snapshot is pre-filled from the list-pass fields
// (ARN, Name, Type) so a describe failure still leaves the slot
// populated with the universal columns — but per the
// scanRegionStepFunctions contract, the caller drops the row on a
// non-nil err return, so callers do not observe the partial snapshot.
func (s *Scanner) describeStateMachine(ctx context.Context, client SFNClient, sm sfntypes.StateMachineListItem, accountID, region string) (scanner.OrchestrationInstanceSnapshot, error) {
	snap := scanner.OrchestrationInstanceSnapshot{
		Provider:  string(credstore.ProviderAWS),
		Surface:   stepFunctionsOrchestrationSurface,
		AccountID: accountID,
		Region:    region,
	}
	if sm.Name != nil {
		snap.ResourceName = *sm.Name
	}
	if sm.StateMachineArn != nil {
		snap.ResourceARN = *sm.StateMachineArn
	}
	if sm.Type != "" {
		snap.WorkflowType = string(sm.Type)
	}
	var desc *sfn.DescribeStateMachineOutput
	err := retryWithBackoff(ctx, func() error {
		var e error
		desc, e = client.DescribeStateMachine(ctx, &sfn.DescribeStateMachineInput{
			StateMachineArn: sm.StateMachineArn,
		})
		return e
	})
	if err != nil {
		return snap, err
	}
	applyStepFunctionsDescription(&snap, desc)
	return snap, nil
}

// applyStepFunctionsDescription folds a DescribeStateMachineOutput into
// the snapshot, populating the WorkflowType + both detection axes + the
// Detail bag.
//
// Axis 1 (HasTraceAxis): TracingConfiguration.Enabled == true.
// Documented Step Functions default is "no X-Ray", so a nil
// TracingConfiguration is treated as "off". Matches the Lambda
// scanner's posture on the TracingConfig.Mode field.
//
// Axis 2 (HasLogAxis): LoggingConfiguration.Level != OFF. AWS publishes
// four LogLevel enum values: ALL, ERROR, FATAL, OFF. Any of the first
// three flips the axis; the empty-string guard catches state machines
// created before AWS pinned the enum default (treated as "off" so the
// operator's enablement recommendation surfaces). FATAL is included
// because operators using only FATAL logs are still emitting
// structured logs that the chunk-5 dashboard can surface — the
// detection axis is "has a logging destination", not "logs everything".
//
// WorkflowType from the describe overwrites the value populated from
// the list pass — they should agree, but the describe-pass value is
// the canonical source of truth.
//
// Detail bag carries the {workflow_type} pair for the per-cloud
// Inventory tab's drilldown. Empty for any future surface that
// chooses not to populate per-row detail.
func applyStepFunctionsDescription(snap *scanner.OrchestrationInstanceSnapshot, desc *sfn.DescribeStateMachineOutput) {
	if desc == nil {
		return
	}
	if desc.Type != "" {
		snap.WorkflowType = string(desc.Type)
	}
	if desc.TracingConfiguration != nil && desc.TracingConfiguration.Enabled {
		snap.HasTraceAxis = true
	}
	if desc.LoggingConfiguration != nil {
		lvl := string(desc.LoggingConfiguration.Level)
		if lvl != "" && lvl != string(sfntypes.LogLevelOff) {
			snap.HasLogAxis = true
		}
	}
	snap.Detail = map[string]any{
		"workflow_type": snap.WorkflowType,
	}
}
