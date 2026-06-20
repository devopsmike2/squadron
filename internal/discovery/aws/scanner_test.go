// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// --- Test doubles -----------------------------------------------------
//
// fakeFactory + fakeEC2 + fakeLambda + fakeSTS satisfy the narrow
// interfaces the scanner depends on (EC2Client, LambdaClient,
// STSClient, ClientFactory). They expose call counts and let each
// test pre-populate the next response. The doubles are intentionally
// dumb — no behavior beyond "return what the test queued" — so the
// scanner's behavior under test is the only thing exercised.

type fakeEC2 struct {
	pages   []*ec2.DescribeInstancesOutput
	callIdx int
	lastIn  *ec2.DescribeInstancesInput
	callErr error
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.lastIn = in
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.callIdx >= len(f.pages) {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	out := f.pages[f.callIdx]
	f.callIdx++
	return out, nil
}

type fakeLambda struct {
	pages   []*lambda.ListFunctionsOutput
	callIdx int
	lastIn  *lambda.ListFunctionsInput
	callErr error
}

func (f *fakeLambda) ListFunctions(_ context.Context, in *lambda.ListFunctionsInput, _ ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error) {
	f.lastIn = in
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.callIdx >= len(f.pages) {
		return &lambda.ListFunctionsOutput{}, nil
	}
	out := f.pages[f.callIdx]
	f.callIdx++
	return out, nil
}

type fakeRDS struct {
	pages   []*rds.DescribeDBInstancesOutput
	callIdx int
	lastIn  *rds.DescribeDBInstancesInput
	callErr error
}

func (f *fakeRDS) DescribeDBInstances(_ context.Context, in *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	f.lastIn = in
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.callIdx >= len(f.pages) {
		return &rds.DescribeDBInstancesOutput{}, nil
	}
	out := f.pages[f.callIdx]
	f.callIdx++
	return out, nil
}

type fakeSTS struct {
	resp    *sts.GetCallerIdentityOutput
	callErr error
}

func (f *fakeSTS) GetCallerIdentity(_ context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.resp == nil {
		return &sts.GetCallerIdentityOutput{}, nil
	}
	return f.resp, nil
}

type fakeFactory struct {
	ec2    EC2Client
	lambda LambdaClient
	rds    RDSClient
	sts    STSClient
}

func (f *fakeFactory) STS(_ context.Context, _ string) (STSClient, error)       { return f.sts, nil }
func (f *fakeFactory) EC2(_ context.Context, _ string) (EC2Client, error)       { return f.ec2, nil }
func (f *fakeFactory) Lambda(_ context.Context, _ string) (LambdaClient, error) { return f.lambda, nil }

// RDS returns the configured fake RDS client. Tests that don't set
// the rds field get a zero-output fake so RDS calls return an empty
// inventory rather than nil-panicking — slice 2's preflight + scan
// always call RDS even when the test only cares about EC2/Lambda.
func (f *fakeFactory) RDS(_ context.Context, _ string) (RDSClient, error) {
	if f.rds == nil {
		return &fakeRDS{}, nil
	}
	return f.rds, nil
}

// newTestScanner builds a Scanner wired against the supplied fake
// factory. Skips the real assume-role path entirely — the
// factoryBuilder closes over the fake.
func newTestScanner(t *testing.T, factory ClientFactory) *Scanner {
	t.Helper()
	s := NewScannerForValidation(credstore.AWSCredentials{
		RoleARN:    "arn:aws:iam::123456789012:role/SquadronDiscovery",
		ExternalID: "test-external-id",
	}, "123456789012")
	s.factoryBuilder = func(_ context.Context, _ credstore.AWSCredentials, _ string) (ClientFactory, error) {
		return factory, nil
	}
	return s
}

// --- Tests ------------------------------------------------------------

func TestScanner_ProviderIsAWS(t *testing.T) {
	s := NewScannerForValidation(credstore.AWSCredentials{RoleARN: "x", ExternalID: "y"}, "1")
	if got := s.Provider(); got != credstore.ProviderAWS {
		t.Fatalf("Provider() = %q, want %q", got, credstore.ProviderAWS)
	}
}

func TestScanner_ScanMapsEC2Result(t *testing.T) {
	ec2Fake := &fakeEC2{
		pages: []*ec2.DescribeInstancesOutput{{
			Reservations: []ec2types.Reservation{{
				Instances: []ec2types.Instance{{
					InstanceId:      awssdk.String("i-1234567890abcdef0"),
					InstanceType:    ec2types.InstanceTypeM5Large,
					PlatformDetails: awssdk.String("Linux/UNIX"),
					Tags: []ec2types.Tag{
						{Key: awssdk.String("Name"), Value: awssdk.String("web-1")},
						{Key: awssdk.String("Env"), Value: awssdk.String("prod")},
					},
				}},
			}},
		}},
	}
	lambdaFake := &fakeLambda{}
	s := newTestScanner(t, &fakeFactory{ec2: ec2Fake, lambda: lambdaFake, sts: &fakeSTS{}})

	conn := &credstore.CloudConnection{
		AccountID: "123456789012",
		Provider:  credstore.ProviderAWS,
		Regions:   []string{"us-east-1"},
	}
	result, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.Compute) != 1 {
		t.Fatalf("Compute snapshots = %d, want 1", len(result.Compute))
	}
	snap := result.Compute[0]
	if snap.ResourceID != "i-1234567890abcdef0" {
		t.Errorf("ResourceID = %q", snap.ResourceID)
	}
	if snap.InstanceType != "m5.large" {
		t.Errorf("InstanceType = %q", snap.InstanceType)
	}
	if snap.Region != "us-east-1" {
		t.Errorf("Region = %q", snap.Region)
	}
	if snap.OSFamily != "linux" {
		t.Errorf("OSFamily = %q, want linux", snap.OSFamily)
	}
	if snap.HasOTel {
		t.Errorf("HasOTel should be false — no otel-* tag present")
	}
	if got := snap.Tags["Env"]; got != "prod" {
		t.Errorf("Tags[Env] = %q, want prod", got)
	}
	if result.AccountID != "123456789012" {
		t.Errorf("AccountID = %q, want 123456789012", result.AccountID)
	}
	if result.UninstrumentedCount != 1 {
		t.Errorf("UninstrumentedCount = %d, want 1", result.UninstrumentedCount)
	}
}

func TestScanner_ScanMapsLambdaResult(t *testing.T) {
	lambdaFake := &fakeLambda{
		pages: []*lambda.ListFunctionsOutput{{
			Functions: []lambdatypes.FunctionConfiguration{{
				FunctionArn:  awssdk.String("arn:aws:lambda:us-east-1:123456789012:function:hello"),
				FunctionName: awssdk.String("hello"),
				Runtime:      lambdatypes.RuntimeNodejs20x,
				Layers: []lambdatypes.Layer{{
					Arn: awssdk.String("arn:aws:lambda:us-east-1:123456789012:layer:custom-lib:1"),
				}},
			}},
		}},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: lambdaFake, sts: &fakeSTS{}})

	conn := &credstore.CloudConnection{AccountID: "123456789012", Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}}
	result, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.Functions) != 1 {
		t.Fatalf("Functions = %d, want 1", len(result.Functions))
	}
	fn := result.Functions[0]
	if fn.Name != "hello" {
		t.Errorf("Name = %q", fn.Name)
	}
	if fn.Runtime != "nodejs20.x" {
		t.Errorf("Runtime = %q", fn.Runtime)
	}
	if fn.Region != "us-east-1" {
		t.Errorf("Region = %q", fn.Region)
	}
	if fn.HasOTelLayer {
		t.Errorf("HasOTelLayer should be false — custom-lib is not OTel")
	}
}

func TestScanner_OTelDetectionEC2(t *testing.T) {
	cases := []struct {
		name string
		tags []ec2types.Tag
		want bool
	}{
		{
			name: "otel-agent tag flips HasOTel",
			tags: []ec2types.Tag{{Key: awssdk.String("otel-agent"), Value: awssdk.String("true")}},
			want: true,
		},
		{
			name: "uppercase OTEL prefix also flips HasOTel (case-insensitive)",
			tags: []ec2types.Tag{{Key: awssdk.String("OTEL_VERSION"), Value: awssdk.String("0.85")}},
			want: true,
		},
		{
			name: "unrelated tag leaves HasOTel false",
			tags: []ec2types.Tag{{Key: awssdk.String("CostCenter"), Value: awssdk.String("eng")}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ec2Fake := &fakeEC2{
				pages: []*ec2.DescribeInstancesOutput{{
					Reservations: []ec2types.Reservation{{
						Instances: []ec2types.Instance{{
							InstanceId:   awssdk.String("i-deadbeef"),
							InstanceType: ec2types.InstanceTypeT3Micro,
							Tags:         tc.tags,
						}},
					}},
				}},
			}
			s := newTestScanner(t, &fakeFactory{ec2: ec2Fake, lambda: &fakeLambda{}, sts: &fakeSTS{}})
			result, err := s.Scan(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}}, []string{"us-east-1"})
			if err != nil {
				t.Fatalf("Scan returned error: %v", err)
			}
			if got := result.Compute[0].HasOTel; got != tc.want {
				t.Errorf("HasOTel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScanner_OTelDetectionLambda(t *testing.T) {
	cases := []struct {
		name     string
		layerARN string
		want     bool
	}{
		{
			name:     "OpenTelemetry layer ARN flips HasOTelLayer",
			layerARN: "arn:aws:lambda:us-east-1:184161586896:layer:opentelemetry-collector-amd64-0_3_0:1",
			want:     true,
		},
		{
			name:     "otel-prefixed layer also matches (case-insensitive substring)",
			layerARN: "arn:aws:lambda:us-east-1:123:layer:OTEL-extension:7",
			want:     true,
		},
		{
			name:     "unrelated layer leaves HasOTelLayer false",
			layerARN: "arn:aws:lambda:us-east-1:123:layer:datadog-extension:42",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lambdaFake := &fakeLambda{
				pages: []*lambda.ListFunctionsOutput{{
					Functions: []lambdatypes.FunctionConfiguration{{
						FunctionArn:  awssdk.String("arn:aws:lambda:us-east-1:123456789012:function:hello"),
						FunctionName: awssdk.String("hello"),
						Runtime:      lambdatypes.RuntimePython311,
						Layers: []lambdatypes.Layer{{
							Arn: awssdk.String(tc.layerARN),
						}},
					}},
				}},
			}
			s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: lambdaFake, sts: &fakeSTS{}})
			result, err := s.Scan(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}}, []string{"us-east-1"})
			if err != nil {
				t.Fatalf("Scan returned error: %v", err)
			}
			if got := result.Functions[0].HasOTelLayer; got != tc.want {
				t.Errorf("HasOTelLayer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScanner_ValidateHappyPath(t *testing.T) {
	ec2Fake := &fakeEC2{
		pages: []*ec2.DescribeInstancesOutput{{
			Reservations: []ec2types.Reservation{{
				Instances: []ec2types.Instance{
					{InstanceId: awssdk.String("i-1")},
					{InstanceId: awssdk.String("i-2")},
				},
			}},
		}},
	}
	lambdaFake := &fakeLambda{
		pages: []*lambda.ListFunctionsOutput{{
			Functions: []lambdatypes.FunctionConfiguration{
				{FunctionArn: awssdk.String("arn1")},
			},
		}},
	}
	s := newTestScanner(t, &fakeFactory{
		ec2:    ec2Fake,
		lambda: lambdaFake,
		sts:    &fakeSTS{resp: &sts.GetCallerIdentityOutput{Account: awssdk.String("123456789012")}},
	})
	vr, err := s.Validate(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !vr.AssumeRoleOK {
		t.Fatalf("AssumeRoleOK should be true on the happy path; AssumeRoleErr=%+v", vr.AssumeRoleErr)
	}
	// Slice 2 (v0.87) added rds as the third preflight row — assert all
	// three services land in the validation panel.
	if len(vr.Preflight) != 3 {
		t.Fatalf("Preflight rows = %d, want 3 (ec2 + lambda + rds)", len(vr.Preflight))
	}
	services := map[string]bool{}
	for _, p := range vr.Preflight {
		services[p.Service] = true
		if !p.OK {
			t.Errorf("Preflight %q OK=false, err=%+v", p.Service, p.Err)
		}
	}
	for _, want := range []string{"ec2", "lambda", "rds"} {
		if !services[want] {
			t.Errorf("Validate did not produce a %q preflight row", want)
		}
	}
}

func TestScanner_ValidateAssumeRoleFailure(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		sts:    &fakeSTS{callErr: &apiErr{code: "AccessDenied", msg: "trust policy missing"}},
	})
	vr, err := s.Validate(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if vr.AssumeRoleOK {
		t.Fatalf("AssumeRoleOK should be false when GetCallerIdentity fails")
	}
	if vr.AssumeRoleErr == nil {
		t.Fatalf("AssumeRoleErr should be populated")
	}
	if vr.AssumeRoleErr.SuggestedStep != "trust-policy" {
		t.Errorf("SuggestedStep = %q, want trust-policy", vr.AssumeRoleErr.SuggestedStep)
	}
}

// --- RDS tests (slice 2, v0.87) --------------------------------------

// TestScanner_ScanMapsRDSResult drives the per-region RDS walk through
// a single page of DescribeDBInstances and verifies the mapping —
// engine, version, instance class, both observability lever flags,
// tags, and region — round-trips into the scanner.Result.
func TestScanner_ScanMapsRDSResult(t *testing.T) {
	rdsFake := &fakeRDS{
		pages: []*rds.DescribeDBInstancesOutput{{
			DBInstances: []rdstypes.DBInstance{{
				DBInstanceArn:              awssdk.String("arn:aws:rds:us-east-1:123456789012:db:db-prod-1"),
				Engine:                     awssdk.String("postgres"),
				EngineVersion:              awssdk.String("15.4"),
				DBInstanceClass:            awssdk.String("db.r6g.large"),
				PerformanceInsightsEnabled: awssdk.Bool(true),
				MonitoringInterval:         awssdk.Int32(60),
				TagList: []rdstypes.Tag{
					{Key: awssdk.String("Env"), Value: awssdk.String("prod")},
					{Key: awssdk.String("Owner"), Value: awssdk.String("platform")},
				},
			}},
		}},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: rdsFake, sts: &fakeSTS{}})

	conn := &credstore.CloudConnection{
		AccountID: "123456789012",
		Provider:  credstore.ProviderAWS,
		Regions:   []string{"us-east-1"},
	}
	result, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.Databases) != 1 {
		t.Fatalf("Databases = %d, want 1", len(result.Databases))
	}
	db := result.Databases[0]
	if db.ResourceID != "arn:aws:rds:us-east-1:123456789012:db:db-prod-1" {
		t.Errorf("ResourceID = %q", db.ResourceID)
	}
	if db.Engine != "postgres" {
		t.Errorf("Engine = %q, want postgres", db.Engine)
	}
	if db.EngineVersion != "15.4" {
		t.Errorf("EngineVersion = %q, want 15.4", db.EngineVersion)
	}
	if db.InstanceClass != "db.r6g.large" {
		t.Errorf("InstanceClass = %q", db.InstanceClass)
	}
	if !db.PerformanceInsightsEnabled {
		t.Errorf("PerformanceInsightsEnabled = false, want true")
	}
	if !db.EnhancedMonitoringEnabled {
		t.Errorf("EnhancedMonitoringEnabled = false, want true (MonitoringInterval=60 should flip it)")
	}
	if db.Region != "us-east-1" {
		t.Errorf("Region = %q", db.Region)
	}
	if got := db.Tags["Env"]; got != "prod" {
		t.Errorf("Tags[Env] = %q, want prod", got)
	}
	// One PI+EM-covered RDS instance, zero EC2, zero Lambda → 1
	// instrumented, 0 uninstrumented under the slice 2 two-part rule.
	if result.InstrumentedCount != 1 || result.UninstrumentedCount != 0 {
		t.Errorf("counts = (instrumented=%d, uninstrumented=%d), want (1, 0)",
			result.InstrumentedCount, result.UninstrumentedCount)
	}
}

// TestScanner_RDSTwoPartInstrumentedRule pins the two-part rule the
// scanner package documents on DatabaseInstanceSnapshot: an RDS row
// counts as instrumented ONLY when BOTH Performance Insights AND
// Enhanced Monitoring are enabled. Any single-lever combination
// counts as uninstrumented.
func TestScanner_RDSTwoPartInstrumentedRule(t *testing.T) {
	cases := []struct {
		name             string
		pi               bool
		monitorInterval  int32
		wantInstrumented bool
	}{
		{name: "both on -> instrumented", pi: true, monitorInterval: 60, wantInstrumented: true},
		{name: "PI only -> uninstrumented", pi: true, monitorInterval: 0, wantInstrumented: false},
		{name: "EM only -> uninstrumented", pi: false, monitorInterval: 60, wantInstrumented: false},
		{name: "neither -> uninstrumented", pi: false, monitorInterval: 0, wantInstrumented: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rdsFake := &fakeRDS{
				pages: []*rds.DescribeDBInstancesOutput{{
					DBInstances: []rdstypes.DBInstance{{
						DBInstanceArn:              awssdk.String("arn:aws:rds:us-east-1:123:db:x"),
						Engine:                     awssdk.String("mysql"),
						EngineVersion:              awssdk.String("8.0"),
						DBInstanceClass:            awssdk.String("db.t3.medium"),
						PerformanceInsightsEnabled: awssdk.Bool(tc.pi),
						MonitoringInterval:         awssdk.Int32(tc.monitorInterval),
					}},
				}},
			}
			s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: rdsFake, sts: &fakeSTS{}})
			result, err := s.Scan(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}}, []string{"us-east-1"})
			if err != nil {
				t.Fatalf("Scan returned error: %v", err)
			}
			if len(result.Databases) != 1 {
				t.Fatalf("Databases = %d", len(result.Databases))
			}
			if tc.wantInstrumented {
				if result.InstrumentedCount != 1 || result.UninstrumentedCount != 0 {
					t.Errorf("counts = (%d, %d), want (1, 0)", result.InstrumentedCount, result.UninstrumentedCount)
				}
			} else {
				if result.InstrumentedCount != 0 || result.UninstrumentedCount != 1 {
					t.Errorf("counts = (%d, %d), want (0, 1)", result.InstrumentedCount, result.UninstrumentedCount)
				}
			}
		})
	}
}

// TestScanner_RDSPaginates verifies the scan walks past the first page
// of DescribeDBInstances when the SDK returns a non-empty Marker.
// Mirrors the existing EC2 / Lambda pagination posture so a future
// change that breaks RDS pagination surfaces here.
func TestScanner_RDSPaginates(t *testing.T) {
	rdsFake := &fakeRDS{
		pages: []*rds.DescribeDBInstancesOutput{
			{
				DBInstances: []rdstypes.DBInstance{{
					DBInstanceArn:   awssdk.String("arn:aws:rds:us-east-1:123:db:page1"),
					Engine:          awssdk.String("postgres"),
					DBInstanceClass: awssdk.String("db.t3.medium"),
				}},
				Marker: awssdk.String("next"),
			},
			{
				DBInstances: []rdstypes.DBInstance{{
					DBInstanceArn:   awssdk.String("arn:aws:rds:us-east-1:123:db:page2"),
					Engine:          awssdk.String("mysql"),
					DBInstanceClass: awssdk.String("db.t3.medium"),
				}},
			},
		},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: rdsFake, sts: &fakeSTS{}})
	result, err := s.Scan(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}}, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.Databases) != 2 {
		t.Fatalf("Databases = %d, want 2 (both pages should land)", len(result.Databases))
	}
}

// TestScanner_RDSPreflightAccessDenied exercises the error path: RDS
// returns AccessDenied when the role's policy is missing
// rds:DescribeDBInstances. The preflight row carries the humanized
// trust-policy step pointer so the wizard can deep-link the operator
// back to fix the trust policy. The other two preflights (ec2 +
// lambda) stay green — the partial failure must not poison them.
func TestScanner_RDSPreflightAccessDenied(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		rds:    &fakeRDS{callErr: &apiErr{code: "AccessDenied", msg: "rds:DescribeDBInstances denied"}},
		sts:    &fakeSTS{resp: &sts.GetCallerIdentityOutput{Account: awssdk.String("123456789012")}},
	})
	vr, err := s.Validate(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !vr.AssumeRoleOK {
		t.Fatalf("AssumeRoleOK should be true even when RDS preflight fails (assume-role itself succeeded)")
	}
	var rdsRow *scanner.PreflightCheck
	for i := range vr.Preflight {
		if vr.Preflight[i].Service == "rds" {
			rdsRow = &vr.Preflight[i]
		}
	}
	if rdsRow == nil {
		t.Fatalf("no rds preflight row in result: %+v", vr.Preflight)
	}
	if rdsRow.OK {
		t.Fatalf("rds preflight OK should be false on AccessDenied")
	}
	if rdsRow.Err == nil {
		t.Fatalf("rds preflight Err should be populated")
	}
	if rdsRow.Err.SuggestedStep != "trust-policy" {
		t.Errorf("SuggestedStep = %q, want trust-policy", rdsRow.Err.SuggestedStep)
	}
	if rdsRow.Err.Code != "AccessDenied" {
		t.Errorf("Code = %q, want AccessDenied", rdsRow.Err.Code)
	}
}
