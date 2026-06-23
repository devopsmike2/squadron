// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// ADOTLayerAccountSegment is the canonical AWS Distro for
// OpenTelemetry (ADOT) Lambda layer ARN segment. AWS publishes ADOT
// layers under account ID 901920570463 across every region with a
// layer name beginning "aws-otel-", so a layer ARN containing the
// substring ":901920570463:layer:aws-otel-" is a strong, AWS-blessed
// signal that the function has the ADOT distribution attached.
//
// Detection rule per docs/proposals/serverless-tier-slice1.md §3.1:
// "Layer ARN starts with arn:aws:lambda:*:901920570463:layer:aws-otel-".
// The full prefix encodes the region segment; the substring match
// keeps the rule region-agnostic without sacrificing precision.
//
// SLICE 1 THREAT MODEL (§12 of the design doc): AWS may publish new
// ADOT layer ARN families over time. A miss on this constant is a
// FALSE NEGATIVE — Squadron reports "no ADOT layer" when one is
// actually present, surfacing as the operator declining a
// lambda-otel-layer recommendation that was inappropriate. The
// verdict-learning loop (#531) records the decline. The chunk-6
// runbook documents how to keep the constant current as AWS publishes
// new layer families. A miss is recoverable; a false positive (a
// non-ADOT layer matching this constant) is much less likely because
// account 901920570463 is AWS's published canonical ADOT publisher.
const ADOTLayerAccountSegment = ":901920570463:layer:aws-otel-"

// OTelExecWrapperEnv is the environment variable name AWS Lambda uses
// to point a function at the ADOT exec wrapper. Presence of this env
// var signals the function was configured for OpenTelemetry
// instrumentation via the runtime-specific wrapper rather than via a
// layer attachment. Detected as an independent axis from the ADOT
// layer detection because operators can use the wrapper without a
// layer (Container Image Lambdas with the ADOT distro baked into the
// image, for example) and vice versa.
//
// Per docs/proposals/serverless-tier-slice1.md §3.1, the rule is:
// either the ADOT layer OR the AWS_LAMBDA_EXEC_WRAPPER env var
// flips HasOTelDistro to true. The OR semantics give operators on
// either detection family credit without forcing them to adopt both.
const OTelExecWrapperEnv = "AWS_LAMBDA_EXEC_WRAPPER"

// lambdaServerlessSurface is the Surface discriminator string for AWS
// Lambda snapshots. The proposer's recommendation-kind prefix routing
// switches on "lambda" → AWS, "cloudrun" / "cloudfunc" → GCP,
// "azfunc" → Azure, "ocifunc" → OCI.
const lambdaServerlessSurface = "lambda"

// scanRegionLambdaServerless walks the region's Lambda functions and
// appends mapped serverless snapshots to result.Serverless. Slice 1
// of the serverless-tier arc (v0.89.90, #721 Stream 119). Unlike the
// existing scanRegionLambda which fills result.Functions with the
// FunctionRuntimeSnapshot shape (compute-tier-adjacent, single-axis
// HasOTelLayer rule), this method fills result.Serverless with the
// new ServerlessInstanceSnapshot shape (two-axis HasTraceAxis +
// HasOTelDistro rule). The two methods coexist so backward-compat
// callers reading result.Functions stay green during the chunk-1
// rollout window; chunk-5 will deprecate the Functions wire shape
// once the per-provider Inventory tabs are migrated to the new tab.
//
// IAM permissions: lambda:ListFunctions already in the slice 1 trust
// policy. The ListFunctions response carries Layers, Environment, and
// TracingConfig inline, so no per-function GetFunctionConfiguration
// fan-out is needed for slice 1's detection axes. (The design doc
// names lambda:GetFunctionConfiguration as available for "additional
// detail" — slice 2 will pull it in when per-function span-quality
// probes ship.)
//
// Detection per docs/proposals/serverless-tier-slice1.md §3.1:
//
//   - HasTraceAxis  ← tracing_config.mode == "Active"
//   - HasOTelDistro ← (any layer ARN contains the ADOT account
//     segment) OR (environment.variables contains
//     AWS_LAMBDA_EXEC_WRAPPER)
//
// The OR semantics on HasOTelDistro mirror the design doc's "either
// detection family gives credit" framing. The proposer's
// lambda-otel-layer vs. lambda-otel-wrapper recommendation kinds
// differentiate which lever the operator should pull next; the
// scanner-side detection is the union.
//
// Returns the error verbatim so the caller's recordPartialFailure
// path can emit "lambda" on result.FailedServices. Pagination
// follows out.NextMarker; an empty function list returns nil without
// appending anything.
func (s *Scanner) scanRegionLambdaServerless(ctx context.Context, factory ClientFactory, region string, result *scanner.Result) error {
	client, err := factory.Lambda(ctx, region)
	if err != nil {
		return err
	}
	var marker *string
	for {
		input := &lambda.ListFunctionsInput{}
		if marker != nil {
			input.Marker = marker
		}
		var out *lambda.ListFunctionsOutput
		callErr := retryWithBackoff(ctx, func() error {
			var e error
			out, e = client.ListFunctions(ctx, input)
			return e
		})
		if callErr != nil {
			return callErr
		}
		for _, fn := range out.Functions {
			result.Serverless = append(result.Serverless,
				mapLambdaServerless(fn, result.AccountID, region))
		}
		if out.NextMarker == nil || *out.NextMarker == "" {
			break
		}
		marker = out.NextMarker
	}
	return nil
}

// mapLambdaServerless turns a single Lambda FunctionConfiguration
// into a ServerlessInstanceSnapshot. Extracted as a standalone
// helper so the per-axis detection logic is independently testable:
// the slice 1 acceptance tests 1-3 + 10 hit this helper directly
// with fixture FunctionConfiguration values, asserting the
// HasTraceAxis / HasOTelDistro outcome without spinning up a full
// scanner.
//
// Axis 1 (HasTraceAxis): tracing_config.mode == "Active". A nil
// TracingConfig (the SDK returns a pointer) is treated as "not
// active", which is the documented Lambda default. The
// TracingConfigResponse.Mode field is the v2 SDK shape — typed as
// types.TracingMode, an enum with values "Active" and "PassThrough".
//
// Axis 2 (HasOTelDistro): either of two sub-rules must hold.
//
//   - 2a: at least one entry in fn.Layers has an ARN containing the
//     ADOTLayerAccountSegment substring. The match is case-sensitive
//     because Lambda ARNs and AWS account IDs are case-sensitive in
//     the canonical form AWS publishes. The first match short-
//     circuits the layer loop.
//   - 2b: fn.Environment is non-nil and fn.Environment.Variables
//     contains the OTelExecWrapperEnv key. Presence (regardless of
//     value) flips the flag — operators sometimes set the wrapper to
//     the empty string to disable it; we treat that as a false
//     negative the operator owns, matching AWS's own posture (the
//     env var being present means the runtime will look for the
//     wrapper, even if the value is misconfigured).
//
// Surface-specific detail bag populates {x_ray_mode, layer_count}
// for the per-cloud Inventory tab's per-row drilldown. Empty when
// neither axis has anything to report (rare in practice — every
// Lambda has a layer count, even if zero).
func mapLambdaServerless(fn lambdatypes.FunctionConfiguration, accountID, region string) scanner.ServerlessInstanceSnapshot {
	snap := scanner.ServerlessInstanceSnapshot{
		Provider:  string(credstore.ProviderAWS),
		Surface:   lambdaServerlessSurface,
		AccountID: accountID,
		Region:    region,
	}
	if fn.FunctionName != nil {
		snap.ResourceName = *fn.FunctionName
	}
	if fn.FunctionArn != nil {
		snap.ResourceARN = *fn.FunctionArn
	}
	if fn.Runtime != "" {
		snap.Runtime = string(fn.Runtime)
	}

	// Axis 1: X-Ray active tracing.
	xrayMode := ""
	if fn.TracingConfig != nil {
		xrayMode = string(fn.TracingConfig.Mode)
		if fn.TracingConfig.Mode == lambdatypes.TracingModeActive {
			snap.HasTraceAxis = true
		}
	}

	// Axis 2a: ADOT layer attached.
	for _, layer := range fn.Layers {
		if layer.Arn == nil {
			continue
		}
		if strings.Contains(*layer.Arn, ADOTLayerAccountSegment) {
			snap.HasOTelDistro = true
			break
		}
	}

	// Axis 2b: AWS_LAMBDA_EXEC_WRAPPER env var. Independent of axis
	// 2a — a function with both gets credit once (HasOTelDistro is
	// already true from 2a; we still inspect 2b so the Detail bag
	// could later expose which sub-rule fired, useful for the
	// chunk-5 dashboard's per-axis breakdown).
	if fn.Environment != nil {
		if _, ok := fn.Environment.Variables[OTelExecWrapperEnv]; ok {
			snap.HasOTelDistro = true
		}
	}

	snap.Detail = map[string]any{
		"x_ray_mode":  xrayMode,
		"layer_count": len(fn.Layers),
	}
	return snap
}

// Ensure the SDK aws helper is reachable — keeps the import live
// even when the slice 1 detection logic happens to not reach for
// awssdk.ToString directly. The orchestrator path uses awssdk
// elsewhere; chunk 2's GCP scanner will reach for it here.
var _ = awssdk.String