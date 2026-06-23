// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// adotLayerARN is a representative ADOT layer ARN matching the
// ADOTLayerAccountSegment constant. AWS publishes ADOT layers under
// account 901920570463 across every region; this fixture uses
// us-east-1 + the python 3.11 ADOT layer name AWS documents.
const adotLayerARN = "arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-python-amd64-ver-1-19-0:1"

// customLayerARN is a non-ADOT layer ARN — same account ID format,
// different (operator-owned) account, no aws-otel- prefix. Used to
// verify the substring match doesn't accidentally fire on look-alike
// layer ARNs.
const customLayerARN = "arn:aws:lambda:us-east-1:123456789012:layer:my-custom-lib:1"

// makeLambdaFn — fixture builder. Wires the three optional axes
// (tracing mode, layers, env vars) through to the SDK's
// FunctionConfiguration shape so each test can construct exactly the
// minimal Lambda function it needs to exercise one axis.
func makeLambdaFn(name string, traceMode lambdatypes.TracingMode, layers []string, envVars map[string]string) lambdatypes.FunctionConfiguration {
	fn := lambdatypes.FunctionConfiguration{
		FunctionName: awssdk.String(name),
		FunctionArn:  awssdk.String("arn:aws:lambda:us-east-1:123456789012:function:" + name),
		Runtime:      lambdatypes.RuntimePython311,
	}
	if traceMode != "" {
		fn.TracingConfig = &lambdatypes.TracingConfigResponse{Mode: traceMode}
	}
	for _, arn := range layers {
		fn.Layers = append(fn.Layers, lambdatypes.Layer{
			Arn: awssdk.String(arn),
		})
	}
	if envVars != nil {
		fn.Environment = &lambdatypes.EnvironmentResponse{
			Variables: envVars,
		}
	}
	return fn
}

// TestLambdaScanner_FunctionWithXRayActiveAndADOTLayer_BothAxesTrue
// — slice 1 acceptance test 1 (docs/proposals/serverless-tier-
// slice1.md §11). A Lambda with tracing_config.mode=="Active" AND a
// layer ARN starting with the ADOT prefix flips both detection axes
// to true.
func TestLambdaScanner_FunctionWithXRayActiveAndADOTLayer_BothAxesTrue(t *testing.T) {
	fn := makeLambdaFn("checkout", lambdatypes.TracingModeActive,
		[]string{adotLayerARN}, nil)

	snap := mapLambdaServerless(fn, "123456789012", "us-east-1")

	if !snap.HasTraceAxis {
		t.Errorf("HasTraceAxis = false, want true (tracing mode = Active)")
	}
	if !snap.HasOTelDistro {
		t.Errorf("HasOTelDistro = false, want true (ADOT layer attached)")
	}
	if snap.Provider != "aws" {
		t.Errorf("Provider = %q, want aws", snap.Provider)
	}
	if snap.Surface != "lambda" {
		t.Errorf("Surface = %q, want lambda", snap.Surface)
	}
	if snap.ResourceName != "checkout" {
		t.Errorf("ResourceName = %q, want checkout", snap.ResourceName)
	}
	if snap.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", snap.Region)
	}
	if snap.AccountID != "123456789012" {
		t.Errorf("AccountID = %q, want 123456789012", snap.AccountID)
	}
	if !snap.IsInstrumented() {
		t.Errorf("IsInstrumented() = false, want true (both axes on)")
	}
	if got, _ := snap.Detail["x_ray_mode"].(string); got != "Active" {
		t.Errorf("Detail[x_ray_mode] = %q, want Active", got)
	}
	if got, _ := snap.Detail["layer_count"].(int); got != 1 {
		t.Errorf("Detail[layer_count] = %d, want 1", got)
	}
}

// TestLambdaScanner_FunctionWithXRayActiveOnly_OnlyTraceAxis — slice
// 1 acceptance test 2. A Lambda with X-Ray Active but no ADOT layer
// and no exec wrapper sets HasTraceAxis=true and HasOTelDistro=false.
func TestLambdaScanner_FunctionWithXRayActiveOnly_OnlyTraceAxis(t *testing.T) {
	fn := makeLambdaFn("orders", lambdatypes.TracingModeActive,
		[]string{customLayerARN}, nil)

	snap := mapLambdaServerless(fn, "123456789012", "us-east-1")

	if !snap.HasTraceAxis {
		t.Errorf("HasTraceAxis = false, want true (X-Ray Active)")
	}
	if snap.HasOTelDistro {
		t.Errorf("HasOTelDistro = true, want false (custom layer is not ADOT)")
	}
	if !snap.IsInstrumented() {
		t.Errorf("IsInstrumented() = false, want true (HasTraceAxis carries it)")
	}
}

// TestLambdaScanner_FunctionWithNeither_BothFalse — slice 1
// acceptance test 3. A Lambda with no X-Ray, no ADOT layer, no exec
// wrapper, no env vars: both axes false.
func TestLambdaScanner_FunctionWithNeither_BothFalse(t *testing.T) {
	fn := makeLambdaFn("legacy", lambdatypes.TracingModePassThrough,
		nil, nil)

	snap := mapLambdaServerless(fn, "123456789012", "us-east-1")

	if snap.HasTraceAxis {
		t.Errorf("HasTraceAxis = true, want false (mode = PassThrough)")
	}
	if snap.HasOTelDistro {
		t.Errorf("HasOTelDistro = true, want false (no layers, no env)")
	}
	if snap.IsInstrumented() {
		t.Errorf("IsInstrumented() = true, want false (both axes off)")
	}
	if got, _ := snap.Detail["layer_count"].(int); got != 0 {
		t.Errorf("Detail[layer_count] = %d, want 0", got)
	}
}

// TestLambdaScanner_FunctionWithOTelExecWrapperEnv_HasOTelDistro —
// the second sub-rule on axis 2: AWS_LAMBDA_EXEC_WRAPPER env var
// presence flips HasOTelDistro even without an ADOT layer.
func TestLambdaScanner_FunctionWithOTelExecWrapperEnv_HasOTelDistro(t *testing.T) {
	fn := makeLambdaFn("workers", lambdatypes.TracingModePassThrough,
		nil, map[string]string{
			OTelExecWrapperEnv: "/opt/otel-handler",
		})

	snap := mapLambdaServerless(fn, "123456789012", "us-east-1")

	if snap.HasTraceAxis {
		t.Errorf("HasTraceAxis = true, want false (no X-Ray Active)")
	}
	if !snap.HasOTelDistro {
		t.Errorf("HasOTelDistro = false, want true (exec wrapper env set)")
	}
}

// TestLambdaScanner_FunctionWithBothADOTLayerAndExecWrapper_HasOTelDistro
// — both sub-rules on axis 2 firing produces the same outcome as
// either alone (HasOTelDistro=true). Defense-in-depth: the loop
// shouldn't double-count or fall through.
func TestLambdaScanner_FunctionWithBothADOTLayerAndExecWrapper_HasOTelDistro(t *testing.T) {
	fn := makeLambdaFn("api", lambdatypes.TracingModeActive,
		[]string{adotLayerARN}, map[string]string{
			OTelExecWrapperEnv: "/opt/otel-handler",
		})

	snap := mapLambdaServerless(fn, "123456789012", "us-east-1")

	if !snap.HasOTelDistro {
		t.Errorf("HasOTelDistro = false, want true (both sub-rules fire)")
	}
	if !snap.HasTraceAxis {
		t.Errorf("HasTraceAxis = false, want true (X-Ray Active)")
	}
}

// TestLambdaScanner_PaginationFollowsNextMarker — multi-page
// ListFunctions response surfaces every function across pages.
//
// Note on fake wiring: the legacy scanRegionLambda walk and the new
// scanRegionLambdaServerless walk both consume ListFunctions pages
// from the same shared fake. The fixture duplicates the page
// sequence so both walks see the full set; the slice 1 chunk 1
// pagination assertion is on the serverless walk's output, which
// runs second. Chunk 5 deprecates the legacy walk and this fixture
// drops the duplication.
func TestLambdaScanner_PaginationFollowsNextMarker(t *testing.T) {
	makePage1 := func() *lambda.ListFunctionsOutput {
		return &lambda.ListFunctionsOutput{
			Functions: []lambdatypes.FunctionConfiguration{
				makeLambdaFn("a", lambdatypes.TracingModeActive, nil, nil),
				makeLambdaFn("b", lambdatypes.TracingModePassThrough, nil, nil),
			},
			NextMarker: awssdk.String("page2"),
		}
	}
	makePage2 := func() *lambda.ListFunctionsOutput {
		return &lambda.ListFunctionsOutput{
			Functions: []lambdatypes.FunctionConfiguration{
				makeLambdaFn("c", lambdatypes.TracingModePassThrough,
					[]string{adotLayerARN}, nil),
			},
		}
	}
	// Two consecutive walks (legacy Functions + new Serverless) each
	// consume 2 pages from the queue.
	lambdaFake := &fakeLambda{pages: []*lambda.ListFunctionsOutput{
		makePage1(), makePage2(), makePage1(), makePage2(),
	}}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: lambdaFake, sts: &fakeSTS{}})

	conn := &credstore.CloudConnection{
		AccountID: "123456789012",
		Provider:  credstore.ProviderAWS,
		Regions:   []string{"us-east-1"},
	}
	result, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.Serverless) != 3 {
		t.Fatalf("Serverless = %d, want 3 across pages", len(result.Serverless))
	}
	// Verify each function landed with the right axis values.
	byName := make(map[string]bool, 3)
	var bAxis, cAxis bool
	for _, sv := range result.Serverless {
		byName[sv.ResourceName] = true
		if sv.ResourceName == "b" && sv.HasTraceAxis {
			t.Errorf("function b should have HasTraceAxis=false")
		}
		if sv.ResourceName == "b" {
			bAxis = true
		}
		if sv.ResourceName == "c" && !sv.HasOTelDistro {
			t.Errorf("function c should have HasOTelDistro=true (ADOT layer)")
		}
		if sv.ResourceName == "c" {
			cAxis = true
		}
	}
	if !byName["a"] || !byName["b"] || !byName["c"] {
		t.Errorf("missing function: a=%v b=%v c=%v", byName["a"], byName["b"], byName["c"])
	}
	if !bAxis || !cAxis {
		t.Errorf("did not exercise both expected functions: bAxis=%v cAxis=%v", bAxis, cAxis)
	}
}

// TestLambdaScanner_EmptyFunctionList_ReturnsEmptySlice — an account
// with zero Lambda functions returns a nil/empty Serverless slice
// without erroring.
func TestLambdaScanner_EmptyFunctionList_ReturnsEmptySlice(t *testing.T) {
	lambdaFake := &fakeLambda{
		pages: []*lambda.ListFunctionsOutput{
			{Functions: nil},
		},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: lambdaFake, sts: &fakeSTS{}})

	conn := &credstore.CloudConnection{
		AccountID: "123456789012",
		Provider:  credstore.ProviderAWS,
		Regions:   []string{"us-east-1"},
	}
	result, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.Serverless) != 0 {
		t.Errorf("Serverless = %d, want 0 on empty function list", len(result.Serverless))
	}
}

// TestLambdaScanner_NilTracingConfig_DefaultsToFalse — defensive
// check: a nil TracingConfig (the SDK returns it as a pointer) must
// not flip HasTraceAxis or panic.
func TestLambdaScanner_NilTracingConfig_DefaultsToFalse(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionName: awssdk.String("nilcfg"),
		FunctionArn:  awssdk.String("arn:aws:lambda:us-east-1:123456789012:function:nilcfg"),
		Runtime:      lambdatypes.RuntimeNodejs20x,
	}
	snap := mapLambdaServerless(fn, "123456789012", "us-east-1")
	if snap.HasTraceAxis {
		t.Errorf("HasTraceAxis = true, want false on nil TracingConfig")
	}
	if got, _ := snap.Detail["x_ray_mode"].(string); got != "" {
		t.Errorf("Detail[x_ray_mode] = %q, want empty string", got)
	}
}

// TestLambdaScanner_ADOTLayerAccountSegmentConstant — pins the
// canonical ADOT account segment per §12 of the design doc's threat
// model. A test failure here flags an unintentional drift in the
// constant; the chunk-6 runbook documents how to refresh it when
// AWS publishes a new ADOT layer family. The constant should never
// be edited without a corresponding runbook entry.
func TestLambdaScanner_ADOTLayerAccountSegmentConstant(t *testing.T) {
	const expected = ":901920570463:layer:aws-otel-"
	if ADOTLayerAccountSegment != expected {
		t.Errorf("ADOTLayerAccountSegment drifted from canonical AWS-published value: got %q, want %q — see chunk-6 runbook before merging",
			ADOTLayerAccountSegment, expected)
	}
	if OTelExecWrapperEnv != "AWS_LAMBDA_EXEC_WRAPPER" {
		t.Errorf("OTelExecWrapperEnv drifted: got %q, want AWS_LAMBDA_EXEC_WRAPPER",
			OTelExecWrapperEnv)
	}
}