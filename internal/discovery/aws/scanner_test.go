// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
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

// fakeS3 is the slice 3a (v0.88.0) S3 double. It services the four
// API methods the S3Client interface requires. ListBuckets returns
// the configured page; GetBucketLocation returns the configured
// region (per bucket name); GetBucketLogging + GetBucketTagging
// return per-bucket configured responses. Any of the four can be
// configured with an error to exercise the failure path.
type fakeS3 struct {
	listBucketsOutput *s3.ListBucketsOutput
	listBucketsErr    error
	// locations maps bucket name to the LocationConstraint string
	// the fake returns. An entry of "" returns empty constraint
	// (us-east-1 legacy quirk).
	locations map[string]string
	// loggingByBucket maps bucket name to a non-nil LoggingEnabled
	// pointer. Buckets absent from the map return an empty
	// LoggingEnabled, signaling logging disabled.
	loggingByBucket map[string]*s3types.LoggingEnabled
	// taggingByBucket maps bucket name to its tag list. Buckets
	// absent return an empty TagSet, modeling the NoSuchTagSet
	// path via taggingErr below when needed.
	taggingByBucket map[string][]s3types.Tag
	// taggingErr is returned from GetBucketTagging for ANY bucket
	// the test wants to fail. Tests that need per-bucket selective
	// failures should add a per-bucket map; current tests only
	// need the all-or-nothing shape.
	taggingErr error
	// loggingErr is returned from GetBucketLogging unconditionally
	// when set. Used by the FailedServices test.
	loggingErr error
}

func (f *fakeS3) ListBuckets(_ context.Context, _ *s3.ListBucketsInput, _ ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	if f.listBucketsErr != nil {
		return nil, f.listBucketsErr
	}
	if f.listBucketsOutput == nil {
		return &s3.ListBucketsOutput{}, nil
	}
	return f.listBucketsOutput, nil
}

func (f *fakeS3) GetBucketLocation(_ context.Context, in *s3.GetBucketLocationInput, _ ...func(*s3.Options)) (*s3.GetBucketLocationOutput, error) {
	out := &s3.GetBucketLocationOutput{}
	if in == nil || in.Bucket == nil {
		return out, nil
	}
	if loc, ok := f.locations[*in.Bucket]; ok {
		out.LocationConstraint = s3types.BucketLocationConstraint(loc)
	}
	return out, nil
}

func (f *fakeS3) GetBucketLogging(_ context.Context, in *s3.GetBucketLoggingInput, _ ...func(*s3.Options)) (*s3.GetBucketLoggingOutput, error) {
	if f.loggingErr != nil {
		return nil, f.loggingErr
	}
	out := &s3.GetBucketLoggingOutput{}
	if in == nil || in.Bucket == nil {
		return out, nil
	}
	if le, ok := f.loggingByBucket[*in.Bucket]; ok {
		out.LoggingEnabled = le
	}
	return out, nil
}

func (f *fakeS3) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	if f.taggingErr != nil {
		return nil, f.taggingErr
	}
	out := &s3.GetBucketTaggingOutput{}
	if in == nil || in.Bucket == nil {
		return out, nil
	}
	if tags, ok := f.taggingByBucket[*in.Bucket]; ok {
		out.TagSet = tags
	}
	return out, nil
}

// fakeELBv2 is the slice 3a ALB / NLB / GWLB double. Services the
// three methods the ELBv2Client interface requires. Pagination
// follows the same shape as fakeRDS — pages slice + callIdx
// advances on each DescribeLoadBalancers call.
type fakeELBv2 struct {
	pages          []*elasticloadbalancingv2.DescribeLoadBalancersOutput
	callIdx        int
	describeLBErr  error
	attrsByARN     map[string]*elasticloadbalancingv2.DescribeLoadBalancerAttributesOutput
	attrErr        error
	tagsByARN      map[string][]elbv2types.Tag
	describeTagErr error
}

func (f *fakeELBv2) DescribeLoadBalancers(_ context.Context, _ *elasticloadbalancingv2.DescribeLoadBalancersInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error) {
	if f.describeLBErr != nil {
		return nil, f.describeLBErr
	}
	if f.callIdx >= len(f.pages) {
		return &elasticloadbalancingv2.DescribeLoadBalancersOutput{}, nil
	}
	out := f.pages[f.callIdx]
	f.callIdx++
	return out, nil
}

func (f *fakeELBv2) DescribeLoadBalancerAttributes(_ context.Context, in *elasticloadbalancingv2.DescribeLoadBalancerAttributesInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancerAttributesOutput, error) {
	if f.attrErr != nil {
		return nil, f.attrErr
	}
	out := &elasticloadbalancingv2.DescribeLoadBalancerAttributesOutput{}
	if in == nil || in.LoadBalancerArn == nil {
		return out, nil
	}
	if a, ok := f.attrsByARN[*in.LoadBalancerArn]; ok && a != nil {
		return a, nil
	}
	return out, nil
}

func (f *fakeELBv2) DescribeTags(_ context.Context, in *elasticloadbalancingv2.DescribeTagsInput, _ ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTagsOutput, error) {
	if f.describeTagErr != nil {
		return nil, f.describeTagErr
	}
	out := &elasticloadbalancingv2.DescribeTagsOutput{}
	if in == nil {
		return out, nil
	}
	for _, arn := range in.ResourceArns {
		arn := arn
		desc := elbv2types.TagDescription{ResourceArn: awssdk.String(arn)}
		if tags, ok := f.tagsByARN[arn]; ok {
			desc.Tags = tags
		}
		out.TagDescriptions = append(out.TagDescriptions, desc)
	}
	return out, nil
}

// fakeEKS is the slice 3b (v0.89.0) EKS double. Services the six
// methods the EKSClient interface requires. Pagination follows the
// same NextToken shape as the SDK. Per-cluster expansion responses
// are keyed by cluster name so each test scenario can wire its
// own per-cluster behavior.
type fakeEKS struct {
	listClustersPages    []*eks.ListClustersOutput
	listClustersIdx      int
	listClustersErr      error
	describeByName       map[string]*eks.DescribeClusterOutput
	describeErr          error
	listAddonsByCluster  map[string]*eks.ListAddonsOutput
	listAddonsErr        error
	describeAddonByKey   map[string]*eks.DescribeAddonOutput // key = cluster + "/" + addon
	describeAddonErr     error
	listNodegroupsByName map[string]*eks.ListNodegroupsOutput
	listNodegroupsErr    error
	listFargateByName    map[string]*eks.ListFargateProfilesOutput
	listFargateErr       error
}

func (f *fakeEKS) ListClusters(_ context.Context, _ *eks.ListClustersInput, _ ...func(*eks.Options)) (*eks.ListClustersOutput, error) {
	if f.listClustersErr != nil {
		return nil, f.listClustersErr
	}
	if f.listClustersIdx >= len(f.listClustersPages) {
		return &eks.ListClustersOutput{}, nil
	}
	out := f.listClustersPages[f.listClustersIdx]
	f.listClustersIdx++
	return out, nil
}

func (f *fakeEKS) DescribeCluster(_ context.Context, in *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if in == nil || in.Name == nil {
		return &eks.DescribeClusterOutput{}, nil
	}
	if out, ok := f.describeByName[*in.Name]; ok && out != nil {
		return out, nil
	}
	return &eks.DescribeClusterOutput{}, nil
}

func (f *fakeEKS) ListAddons(_ context.Context, in *eks.ListAddonsInput, _ ...func(*eks.Options)) (*eks.ListAddonsOutput, error) {
	if f.listAddonsErr != nil {
		return nil, f.listAddonsErr
	}
	if in == nil || in.ClusterName == nil {
		return &eks.ListAddonsOutput{}, nil
	}
	if out, ok := f.listAddonsByCluster[*in.ClusterName]; ok && out != nil {
		return out, nil
	}
	return &eks.ListAddonsOutput{}, nil
}

func (f *fakeEKS) DescribeAddon(_ context.Context, in *eks.DescribeAddonInput, _ ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	if f.describeAddonErr != nil {
		return nil, f.describeAddonErr
	}
	if in == nil || in.ClusterName == nil || in.AddonName == nil {
		return &eks.DescribeAddonOutput{}, nil
	}
	key := *in.ClusterName + "/" + *in.AddonName
	if out, ok := f.describeAddonByKey[key]; ok && out != nil {
		return out, nil
	}
	return &eks.DescribeAddonOutput{}, nil
}

func (f *fakeEKS) ListNodegroups(_ context.Context, in *eks.ListNodegroupsInput, _ ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	if f.listNodegroupsErr != nil {
		return nil, f.listNodegroupsErr
	}
	if in == nil || in.ClusterName == nil {
		return &eks.ListNodegroupsOutput{}, nil
	}
	if out, ok := f.listNodegroupsByName[*in.ClusterName]; ok && out != nil {
		return out, nil
	}
	return &eks.ListNodegroupsOutput{}, nil
}

func (f *fakeEKS) ListFargateProfiles(_ context.Context, in *eks.ListFargateProfilesInput, _ ...func(*eks.Options)) (*eks.ListFargateProfilesOutput, error) {
	if f.listFargateErr != nil {
		return nil, f.listFargateErr
	}
	if in == nil || in.ClusterName == nil {
		return &eks.ListFargateProfilesOutput{}, nil
	}
	if out, ok := f.listFargateByName[*in.ClusterName]; ok && out != nil {
		return out, nil
	}
	return &eks.ListFargateProfilesOutput{}, nil
}

// fakeDynamoDB is the slice 4 (v0.89.6) DynamoDB double. Services
// the four methods the DynamoDBClient interface requires. Pagination
// follows the same ExclusiveStartTableName / LastEvaluatedTableName
// shape the SDK uses. Per-table expansion responses are keyed by
// table name so each test scenario can wire its own per-table
// behavior — mirrors fakeEKS's describeByName / describeAddonByKey
// keying pattern.
type fakeDynamoDB struct {
	listTablesPages []*dynamodb.ListTablesOutput
	listTablesIdx   int
	listTablesErr   error
	// describeByName keys on the table name supplied to
	// DescribeTable. Tables not in the map return an empty
	// DescribeTableOutput; tests that want a per-table not-found
	// shape add a row with describeErrByName below.
	describeByName    map[string]*dynamodb.DescribeTableOutput
	describeErr       error
	describeErrByName map[string]error
	// ciByName keys on table name. Absent entries return an empty
	// DescribeContributorInsightsOutput (status="" -> uninstrumented).
	ciByName    map[string]*dynamodb.DescribeContributorInsightsOutput
	ciErr       error
	ciErrByName map[string]error
	// tagsByARN keys on the table ARN supplied to
	// ListTagsOfResource. Absent entries return an empty tag list.
	tagsByARN map[string][]dynamodbtypes.Tag
	tagsErr   error
}

func (f *fakeDynamoDB) ListTables(_ context.Context, _ *dynamodb.ListTablesInput, _ ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error) {
	if f.listTablesErr != nil {
		return nil, f.listTablesErr
	}
	if f.listTablesIdx >= len(f.listTablesPages) {
		return &dynamodb.ListTablesOutput{}, nil
	}
	out := f.listTablesPages[f.listTablesIdx]
	f.listTablesIdx++
	return out, nil
}

func (f *fakeDynamoDB) DescribeTable(_ context.Context, in *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if in == nil || in.TableName == nil {
		return &dynamodb.DescribeTableOutput{}, nil
	}
	if err, ok := f.describeErrByName[*in.TableName]; ok && err != nil {
		return nil, err
	}
	if out, ok := f.describeByName[*in.TableName]; ok && out != nil {
		return out, nil
	}
	return &dynamodb.DescribeTableOutput{}, nil
}

func (f *fakeDynamoDB) DescribeContributorInsights(_ context.Context, in *dynamodb.DescribeContributorInsightsInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeContributorInsightsOutput, error) {
	if f.ciErr != nil {
		return nil, f.ciErr
	}
	if in == nil || in.TableName == nil {
		return &dynamodb.DescribeContributorInsightsOutput{}, nil
	}
	if err, ok := f.ciErrByName[*in.TableName]; ok && err != nil {
		return nil, err
	}
	if out, ok := f.ciByName[*in.TableName]; ok && out != nil {
		return out, nil
	}
	return &dynamodb.DescribeContributorInsightsOutput{}, nil
}

func (f *fakeDynamoDB) ListTagsOfResource(_ context.Context, in *dynamodb.ListTagsOfResourceInput, _ ...func(*dynamodb.Options)) (*dynamodb.ListTagsOfResourceOutput, error) {
	if f.tagsErr != nil {
		return nil, f.tagsErr
	}
	out := &dynamodb.ListTagsOfResourceOutput{}
	if in == nil || in.ResourceArn == nil {
		return out, nil
	}
	if tags, ok := f.tagsByARN[*in.ResourceArn]; ok {
		out.Tags = tags
	}
	return out, nil
}

type fakeFactory struct {
	ec2      EC2Client
	lambda   LambdaClient
	rds      RDSClient
	s3       S3Client
	elbv2    ELBv2Client
	eks      EKSClient
	dynamodb DynamoDBClient
	sts      STSClient
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

// S3 returns the configured fake S3 client. Tests that don't set
// the s3 field get a zero-output fake so S3 calls return an empty
// inventory rather than nil-panicking — same posture as RDS. Slice
// 3a (v0.88.0).
func (f *fakeFactory) S3(_ context.Context, _ string) (S3Client, error) {
	if f.s3 == nil {
		return &fakeS3{}, nil
	}
	return f.s3, nil
}

// ELBv2 returns the configured fake ELBv2 client. Same zero-output
// fallback as the other services. Slice 3a (v0.88.0).
func (f *fakeFactory) ELBv2(_ context.Context, _ string) (ELBv2Client, error) {
	if f.elbv2 == nil {
		return &fakeELBv2{}, nil
	}
	return f.elbv2, nil
}

// EKS returns the configured fake EKS client. Same zero-output
// fallback as the other services. Slice 3b (v0.89.0).
func (f *fakeFactory) EKS(_ context.Context, _ string) (EKSClient, error) {
	if f.eks == nil {
		return &fakeEKS{}, nil
	}
	return f.eks, nil
}

// DynamoDB returns the configured fake DynamoDB client. Same
// zero-output fallback as the other services. Slice 4 (v0.89.6).
func (f *fakeFactory) DynamoDB(_ context.Context, _ string) (DynamoDBClient, error) {
	if f.dynamodb == nil {
		return &fakeDynamoDB{}, nil
	}
	return f.dynamodb, nil
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
	// Slice 3a (v0.88.0) added s3 + alb as the 4th and 5th preflight
	// rows; slice 3b (v0.89.0) added eks as the 6th; slice 4
	// (v0.89.6) adds dynamodb as the 7th — assert all seven services
	// land in the validation panel. Slice 1 shipped ec2+lambda;
	// slice 2 (v0.87) added rds; slice 3a (v0.88.0) added s3+alb;
	// slice 3b (v0.89.0) added eks; slice 4 (v0.89.6) added dynamodb.
	if len(vr.Preflight) != 7 {
		t.Fatalf("Preflight rows = %d, want 7 (ec2 + lambda + rds + s3 + alb + eks + dynamodb)", len(vr.Preflight))
	}
	services := map[string]bool{}
	for _, p := range vr.Preflight {
		services[p.Service] = true
		if !p.OK {
			t.Errorf("Preflight %q OK=false, err=%+v", p.Service, p.Err)
		}
	}
	for _, want := range []string{"ec2", "lambda", "rds", "s3", "alb", "eks", "dynamodb"} {
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

// TestScanner_ScanRDSFailureSetsPartialAndFailedServices pins the
// v0.87.3 audit-shape contract on the scanner side: when the rds
// per-region walk fails (the live reproducer from task #584 is
// rds:DescribeDBInstances revoked from the SquadronDiscoveryReadOnly
// inline policy), Result.Partial flips to true, Result.PartialReason
// carries the human-readable explanation, AND Result.FailedServices
// carries the structured ["rds"] entry the discovery handler's
// scan_completed audit event now surfaces.
//
// Mirrors TestScanner_RDSPreflightAccessDenied's posture but exercises
// the Scan path (not Validate) — same single-service-failure shape the
// audit payload is widened around.
func TestScanner_ScanRDSFailureSetsPartialAndFailedServices(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		rds:    &fakeRDS{callErr: &apiErr{code: "AccessDenied", msg: "rds:DescribeDBInstances denied"}},
		sts:    &fakeSTS{},
	})
	result, err := s.Scan(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}}, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v (Scanner contract says Partial=true, not a Go error)", err)
	}
	if !result.Partial {
		t.Fatalf("Result.Partial = false, want true when the rds walk fails")
	}
	if result.PartialReason == "" {
		t.Errorf("Result.PartialReason is empty; want the rds-walk failure explanation")
	}
	// Pin the structured failed-services list so audit consumers and
	// the proposer's future "learn from past scans" loop can pattern-
	// match against "rds" without parsing the formatted PartialReason
	// string. Single-service-failure case: exactly one entry.
	if len(result.FailedServices) != 1 || result.FailedServices[0] != "rds" {
		t.Errorf("Result.FailedServices = %v, want [\"rds\"]", result.FailedServices)
	}
}

// TestScanner_MultiServiceFailureAccumulatesPartialReason pins the
// v0.88.3 fix for #586: when multiple services fail in the same scan,
// PartialReason accumulates joined by "; " instead of overwriting
// (only the last failure surviving). FailedServices was always
// accumulating correctly — that part of the contract pre-dates v0.88.3.
// This test seeds two simultaneous failures (rds + s3) and asserts:
//
//  1. Result.Partial = true (same as single-failure case)
//  2. Result.PartialReason contains BOTH service names (not just the
//     last-failed one — that was #586's symptom)
//  3. Result.FailedServices contains BOTH entries in their walk order
//
// Multi-region scans are out of scope here; that path can compound
// failures further but the accumulator logic is the same per region.
func TestScanner_MultiServiceFailureAccumulatesPartialReason(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		rds:    &fakeRDS{callErr: &apiErr{code: "AccessDenied", msg: "rds:DescribeDBInstances denied"}},
		s3:     &fakeS3{listBucketsErr: &apiErr{code: "AccessDenied", msg: "s3:ListAllMyBuckets denied"}},
		elbv2:  &fakeELBv2{},
		sts:    &fakeSTS{},
	})
	result, err := s.Scan(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}}, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v (Scanner contract says Partial=true, not a Go error)", err)
	}
	if !result.Partial {
		t.Fatalf("Result.Partial = false, want true when multiple services fail")
	}
	// PartialReason must contain BOTH service identifiers. Pre-v0.88.3
	// the rds message would be CLOBBERED by the s3 message (s3 walks
	// after rds in the per-region loop). The accumulator joins them
	// with "; " so audit consumers and operators see every failure.
	if !strings.Contains(result.PartialReason, "rds scan failed") {
		t.Errorf("PartialReason missing rds message — got %q; #586 regression", result.PartialReason)
	}
	if !strings.Contains(result.PartialReason, "s3 scan failed") {
		t.Errorf("PartialReason missing s3 message — got %q", result.PartialReason)
	}
	if !strings.Contains(result.PartialReason, "; ") {
		t.Errorf("PartialReason missing ';' separator — got %q; accumulator format broken", result.PartialReason)
	}
	// FailedServices must contain BOTH in the walk order (rds then s3).
	if len(result.FailedServices) != 2 {
		t.Fatalf("Result.FailedServices length = %d, want 2; got %v", len(result.FailedServices), result.FailedServices)
	}
	if result.FailedServices[0] != "rds" || result.FailedServices[1] != "s3" {
		t.Errorf("Result.FailedServices = %v, want [\"rds\", \"s3\"] in walk order", result.FailedServices)
	}
}

// --- S3 tests (slice 3a, v0.88.0) -----------------------------------

// TestScanner_ScanMapsS3Result drives the per-region S3 walk through
// ListBuckets + GetBucketLocation + GetBucketLogging +
// GetBucketTagging and verifies the mapping —
// server_access_logging_enabled, tags, region — round-trips into
// the scanner.Result.
//
// The bucket is in us-east-1 (empty LocationConstraint is the legacy
// AWS quirk the scanner normalizes); the connection's region list
// matches. ServerAccessLoggingEnabled is true because
// GetBucketLogging returns a non-empty LoggingEnabled.TargetBucket.
func TestScanner_ScanMapsS3Result(t *testing.T) {
	s3Fake := &fakeS3{
		listBucketsOutput: &s3.ListBucketsOutput{
			Buckets: []s3types.Bucket{{
				Name: awssdk.String("squadron-logs-prod"),
			}},
		},
		locations: map[string]string{
			// Empty constraint means us-east-1; the scanner
			// normalizes.
			"squadron-logs-prod": "",
		},
		loggingByBucket: map[string]*s3types.LoggingEnabled{
			"squadron-logs-prod": {
				TargetBucket: awssdk.String("squadron-logs-archive"),
				TargetPrefix: awssdk.String("prod-logs/"),
			},
		},
		taggingByBucket: map[string][]s3types.Tag{
			"squadron-logs-prod": {
				{Key: awssdk.String("Env"), Value: awssdk.String("prod")},
			},
		},
	}
	s := newTestScanner(t, &fakeFactory{
		ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: &fakeRDS{},
		s3: s3Fake, sts: &fakeSTS{},
	})

	conn := &credstore.CloudConnection{
		AccountID: "123456789012",
		Provider:  credstore.ProviderAWS,
		Regions:   []string{"us-east-1"},
	}
	result, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.ObjectStores) != 1 {
		t.Fatalf("ObjectStores = %d, want 1", len(result.ObjectStores))
	}
	o := result.ObjectStores[0]
	if o.ResourceID != "squadron-logs-prod" {
		t.Errorf("ResourceID = %q", o.ResourceID)
	}
	if o.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1 (empty LocationConstraint normalizes)", o.Region)
	}
	if !o.ServerAccessLoggingEnabled {
		t.Errorf("ServerAccessLoggingEnabled = false, want true (LoggingEnabled.TargetBucket present)")
	}
	if o.RequestMetricsEnabled {
		t.Errorf("RequestMetricsEnabled = true; slice 3a leaves this informational/false until the scanner adds the lookup")
	}
	if got := o.Tags["Env"]; got != "prod" {
		t.Errorf("Tags[Env] = %q, want prod", got)
	}
	// One logging-enabled bucket, zero EC2 / Lambda / RDS / ALB →
	// 1 instrumented, 0 uninstrumented under the slice 3a
	// single-axis rule.
	if result.InstrumentedCount != 1 || result.UninstrumentedCount != 0 {
		t.Errorf("counts = (instrumented=%d, uninstrumented=%d), want (1, 0)",
			result.InstrumentedCount, result.UninstrumentedCount)
	}
}

// TestScanner_S3FiltersOutOfRegionBuckets pins the region-filter
// contract: a bucket whose home region isn't in the connection's
// region list does NOT appear in result.ObjectStores. Operators who
// want broader visibility add more regions to the connection.
func TestScanner_S3FiltersOutOfRegionBuckets(t *testing.T) {
	s3Fake := &fakeS3{
		listBucketsOutput: &s3.ListBucketsOutput{
			Buckets: []s3types.Bucket{
				{Name: awssdk.String("in-region")},
				{Name: awssdk.String("out-of-region")},
			},
		},
		locations: map[string]string{
			"in-region":     "us-east-1",
			"out-of-region": "eu-west-1",
		},
	}
	s := newTestScanner(t, &fakeFactory{
		ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: &fakeRDS{},
		s3: s3Fake, sts: &fakeSTS{},
	})
	result, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.ObjectStores) != 1 {
		t.Fatalf("ObjectStores = %d, want 1 (out-of-region bucket should be filtered)", len(result.ObjectStores))
	}
	if result.ObjectStores[0].ResourceID != "in-region" {
		t.Errorf("ResourceID = %q, want in-region", result.ObjectStores[0].ResourceID)
	}
}

// TestScanner_S3IgnoresNoSuchTagSet verifies the
// collectBucketDetails handler treats the NoSuchTagSet API error as
// "this bucket has no tags" rather than a walk-breaking failure.
// The mapped snapshot lands with nil Tags.
func TestScanner_S3IgnoresNoSuchTagSet(t *testing.T) {
	s3Fake := &fakeS3{
		listBucketsOutput: &s3.ListBucketsOutput{
			Buckets: []s3types.Bucket{{Name: awssdk.String("untagged-bucket")}},
		},
		locations:       map[string]string{"untagged-bucket": "us-east-1"},
		loggingByBucket: map[string]*s3types.LoggingEnabled{},
		taggingErr:      &apiErr{code: "NoSuchTagSet", msg: "The TagSet does not exist"},
	}
	s := newTestScanner(t, &fakeFactory{
		ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: &fakeRDS{},
		s3: s3Fake, sts: &fakeSTS{},
	})
	result, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v (NoSuchTagSet should not break the walk)", err)
	}
	if len(result.ObjectStores) != 1 {
		t.Fatalf("ObjectStores = %d, want 1", len(result.ObjectStores))
	}
	if result.ObjectStores[0].Tags != nil {
		t.Errorf("Tags = %v, want nil for NoSuchTagSet path", result.ObjectStores[0].Tags)
	}
}

// TestScanner_S3PreflightAccessDenied exercises the error path —
// s3:ListAllMyBuckets is missing from the permissions policy. The
// preflight row carries the trust-policy step pointer and the other
// preflight rows stay green.
func TestScanner_S3PreflightAccessDenied(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		rds:    &fakeRDS{},
		s3:     &fakeS3{listBucketsErr: &apiErr{code: "AccessDenied", msg: "s3:ListAllMyBuckets denied"}},
		sts:    &fakeSTS{resp: &sts.GetCallerIdentityOutput{Account: awssdk.String("123456789012")}},
	})
	vr, err := s.Validate(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !vr.AssumeRoleOK {
		t.Fatalf("AssumeRoleOK should be true even when S3 preflight fails")
	}
	var s3Row *scanner.PreflightCheck
	for i := range vr.Preflight {
		if vr.Preflight[i].Service == "s3" {
			s3Row = &vr.Preflight[i]
		}
	}
	if s3Row == nil {
		t.Fatalf("no s3 preflight row in result: %+v", vr.Preflight)
	}
	if s3Row.OK {
		t.Fatalf("s3 preflight OK should be false on AccessDenied")
	}
	if s3Row.Err == nil {
		t.Fatalf("s3 preflight Err should be populated")
	}
	if s3Row.Err.SuggestedStep != "trust-policy" {
		t.Errorf("SuggestedStep = %q, want trust-policy", s3Row.Err.SuggestedStep)
	}
}

// TestScanner_ScanS3FailureSetsPartialAndFailedServices pins the
// v0.87.3 audit-shape contract for the slice 3a S3 service: when
// the per-region S3 walk fails (s3:ListAllMyBuckets revoked),
// Result.Partial flips to true, PartialReason carries the
// human-readable explanation, AND FailedServices includes "s3".
// Mirrors TestScanner_ScanRDSFailureSetsPartialAndFailedServices.
func TestScanner_ScanS3FailureSetsPartialAndFailedServices(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		rds:    &fakeRDS{},
		s3:     &fakeS3{listBucketsErr: &apiErr{code: "AccessDenied", msg: "s3:ListAllMyBuckets denied"}},
		sts:    &fakeSTS{},
	})
	result, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v (Scanner contract says Partial=true, not a Go error)", err)
	}
	if !result.Partial {
		t.Fatalf("Result.Partial = false, want true when the s3 walk fails")
	}
	if result.PartialReason == "" {
		t.Errorf("Result.PartialReason is empty; want the s3-walk failure explanation")
	}
	hasS3 := false
	for _, fs := range result.FailedServices {
		if fs == "s3" {
			hasS3 = true
		}
	}
	if !hasS3 {
		t.Errorf("Result.FailedServices = %v, want to include \"s3\"", result.FailedServices)
	}
}

// --- ALB tests (slice 3a, v0.88.0) ----------------------------------

// TestScanner_ScanMapsALBResult drives the per-region ALB walk
// through DescribeLoadBalancers + DescribeLoadBalancerAttributes +
// DescribeTags and verifies the mapping — name, type, scheme,
// access_logs_enabled, access_logs_s3_bucket, region, tags —
// round-trips.
func TestScanner_ScanMapsALBResult(t *testing.T) {
	albARN := "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/prod-alb/abcdef1234567890"
	elbv2Fake := &fakeELBv2{
		pages: []*elasticloadbalancingv2.DescribeLoadBalancersOutput{{
			LoadBalancers: []elbv2types.LoadBalancer{{
				LoadBalancerArn:  awssdk.String(albARN),
				LoadBalancerName: awssdk.String("prod-alb"),
				Type:             elbv2types.LoadBalancerTypeEnumApplication,
				Scheme:           elbv2types.LoadBalancerSchemeEnumInternetFacing,
			}},
		}},
		attrsByARN: map[string]*elasticloadbalancingv2.DescribeLoadBalancerAttributesOutput{
			albARN: {
				Attributes: []elbv2types.LoadBalancerAttribute{
					{Key: awssdk.String("access_logs.s3.enabled"), Value: awssdk.String("true")},
					{Key: awssdk.String("access_logs.s3.bucket"), Value: awssdk.String("squadron-logs-prod")},
				},
			},
		},
		tagsByARN: map[string][]elbv2types.Tag{
			albARN: {{Key: awssdk.String("Env"), Value: awssdk.String("prod")}},
		},
	}
	s := newTestScanner(t, &fakeFactory{
		ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: &fakeRDS{},
		elbv2: elbv2Fake, sts: &fakeSTS{},
	})

	result, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.LoadBalancers) != 1 {
		t.Fatalf("LoadBalancers = %d, want 1", len(result.LoadBalancers))
	}
	lb := result.LoadBalancers[0]
	if lb.ResourceID != albARN {
		t.Errorf("ResourceID = %q", lb.ResourceID)
	}
	if lb.Name != "prod-alb" {
		t.Errorf("Name = %q, want prod-alb", lb.Name)
	}
	if lb.Type != "application" {
		t.Errorf("Type = %q, want application", lb.Type)
	}
	if lb.Scheme != "internet-facing" {
		t.Errorf("Scheme = %q, want internet-facing", lb.Scheme)
	}
	if !lb.AccessLogsEnabled {
		t.Errorf("AccessLogsEnabled = false, want true (access_logs.s3.enabled=true attribute)")
	}
	if lb.AccessLogsS3Bucket != "squadron-logs-prod" {
		t.Errorf("AccessLogsS3Bucket = %q, want squadron-logs-prod", lb.AccessLogsS3Bucket)
	}
	if lb.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", lb.Region)
	}
	if got := lb.Tags["Env"]; got != "prod" {
		t.Errorf("Tags[Env] = %q, want prod", got)
	}
	// One access-logs-enabled ALB → 1 instrumented, 0 uninstrumented
	// under the slice 3a single-axis rule.
	if result.InstrumentedCount != 1 || result.UninstrumentedCount != 0 {
		t.Errorf("counts = (instrumented=%d, uninstrumented=%d), want (1, 0)",
			result.InstrumentedCount, result.UninstrumentedCount)
	}
}

// TestScanner_ALBPaginates verifies the scan walks past the first
// page of DescribeLoadBalancers when the SDK returns a non-empty
// NextMarker. Mirrors the existing RDS pagination posture.
func TestScanner_ALBPaginates(t *testing.T) {
	elbv2Fake := &fakeELBv2{
		pages: []*elasticloadbalancingv2.DescribeLoadBalancersOutput{
			{
				LoadBalancers: []elbv2types.LoadBalancer{{
					LoadBalancerArn:  awssdk.String("arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/page1/x"),
					LoadBalancerName: awssdk.String("page1"),
					Type:             elbv2types.LoadBalancerTypeEnumApplication,
					Scheme:           elbv2types.LoadBalancerSchemeEnumInternal,
				}},
				NextMarker: awssdk.String("next"),
			},
			{
				LoadBalancers: []elbv2types.LoadBalancer{{
					LoadBalancerArn:  awssdk.String("arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/page2/y"),
					LoadBalancerName: awssdk.String("page2"),
					Type:             elbv2types.LoadBalancerTypeEnumApplication,
					Scheme:           elbv2types.LoadBalancerSchemeEnumInternal,
				}},
			},
		},
	}
	s := newTestScanner(t, &fakeFactory{
		ec2: &fakeEC2{}, lambda: &fakeLambda{}, rds: &fakeRDS{},
		elbv2: elbv2Fake, sts: &fakeSTS{},
	})
	result, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.LoadBalancers) != 2 {
		t.Fatalf("LoadBalancers = %d, want 2 (both pages)", len(result.LoadBalancers))
	}
}

// TestScanner_ALBPreflightAccessDenied exercises the error path —
// elasticloadbalancing:DescribeLoadBalancers missing from the
// permissions policy. Same shape as the rds + s3 preflight tests.
func TestScanner_ALBPreflightAccessDenied(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		rds:    &fakeRDS{},
		elbv2:  &fakeELBv2{describeLBErr: &apiErr{code: "AccessDenied", msg: "elasticloadbalancing:DescribeLoadBalancers denied"}},
		sts:    &fakeSTS{resp: &sts.GetCallerIdentityOutput{Account: awssdk.String("123456789012")}},
	})
	vr, err := s.Validate(context.Background(), &credstore.CloudConnection{Regions: []string{"us-east-1"}})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !vr.AssumeRoleOK {
		t.Fatalf("AssumeRoleOK should be true even when ALB preflight fails")
	}
	var albRow *scanner.PreflightCheck
	for i := range vr.Preflight {
		if vr.Preflight[i].Service == "alb" {
			albRow = &vr.Preflight[i]
		}
	}
	if albRow == nil {
		t.Fatalf("no alb preflight row in result: %+v", vr.Preflight)
	}
	if albRow.OK {
		t.Fatalf("alb preflight OK should be false on AccessDenied")
	}
	if albRow.Err == nil {
		t.Fatalf("alb preflight Err should be populated")
	}
	if albRow.Err.SuggestedStep != "trust-policy" {
		t.Errorf("SuggestedStep = %q, want trust-policy", albRow.Err.SuggestedStep)
	}
}

// TestScanner_ScanALBFailureSetsPartialAndFailedServices pins the
// v0.87.3 audit-shape contract for the slice 3a ALB service: when
// the per-region ALB walk fails, Result.Partial flips, PartialReason
// is set, AND FailedServices includes "alb".
func TestScanner_ScanALBFailureSetsPartialAndFailedServices(t *testing.T) {
	s := newTestScanner(t, &fakeFactory{
		ec2:    &fakeEC2{},
		lambda: &fakeLambda{},
		rds:    &fakeRDS{},
		elbv2:  &fakeELBv2{describeLBErr: &apiErr{code: "AccessDenied", msg: "elasticloadbalancing:DescribeLoadBalancers denied"}},
		sts:    &fakeSTS{},
	})
	result, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if !result.Partial {
		t.Fatalf("Result.Partial = false, want true when the alb walk fails")
	}
	if result.PartialReason == "" {
		t.Errorf("Result.PartialReason is empty; want the alb-walk failure explanation")
	}
	hasALB := false
	for _, fs := range result.FailedServices {
		if fs == "alb" {
			hasALB = true
		}
	}
	if !hasALB {
		t.Errorf("Result.FailedServices = %v, want to include \"alb\"", result.FailedServices)
	}
}

// TestScanner_ScanMapsEKSResult is the slice 3b (v0.89.0) mapping
// happy path: a single cluster returned by ListClusters expands
// through DescribeCluster + ListAddons + DescribeAddon + node /
// fargate listings into a populated ClusterSnapshot. Both axes of
// the composite instrumented rule are exercised: control plane
// logging includes api + audit AND an ACTIVE adot add-on is
// present, so the cluster lands in InstrumentedCount.
func TestScanner_ScanMapsEKSResult(t *testing.T) {
	enabled := true
	clusterName := "prod"
	clusterARN := "arn:aws:eks:us-east-1:123456789012:cluster/prod"
	version := "1.29"
	addonName := "adot"
	addonVersion := "v0.92.0-eksbuild.1"
	feks := &fakeEKS{
		listClustersPages: []*eks.ListClustersOutput{
			{Clusters: []string{clusterName}},
		},
		describeByName: map[string]*eks.DescribeClusterOutput{
			clusterName: {
				Cluster: &ekstypes.Cluster{
					Arn:     awssdk.String(clusterARN),
					Version: awssdk.String(version),
					Status:  ekstypes.ClusterStatusActive,
					Logging: &ekstypes.Logging{
						ClusterLogging: []ekstypes.LogSetup{
							{Enabled: &enabled, Types: []ekstypes.LogType{ekstypes.LogTypeApi, ekstypes.LogTypeAudit}},
						},
					},
					Tags: map[string]string{"env": "prod"},
				},
			},
		},
		listAddonsByCluster: map[string]*eks.ListAddonsOutput{
			clusterName: {Addons: []string{addonName}},
		},
		describeAddonByKey: map[string]*eks.DescribeAddonOutput{
			clusterName + "/" + addonName: {
				Addon: &ekstypes.Addon{
					AddonName:    awssdk.String(addonName),
					AddonVersion: awssdk.String(addonVersion),
					Status:       ekstypes.AddonStatusActive,
				},
			},
		},
		listNodegroupsByName: map[string]*eks.ListNodegroupsOutput{
			clusterName: {Nodegroups: []string{"ng-1", "ng-2"}},
		},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, eks: feks, sts: &fakeSTS{}})
	conn := &credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}}
	r, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Clusters) != 1 {
		t.Fatalf("Clusters len = %d, want 1", len(r.Clusters))
	}
	c := r.Clusters[0]
	if c.ResourceID != clusterARN {
		t.Errorf("ResourceID = %q, want %q", c.ResourceID, clusterARN)
	}
	if c.Name != clusterName {
		t.Errorf("Name = %q, want %q", c.Name, clusterName)
	}
	if c.KubernetesVersion != version {
		t.Errorf("KubernetesVersion = %q, want %q", c.KubernetesVersion, version)
	}
	if len(c.ControlPlaneLogging) != 2 {
		t.Fatalf("ControlPlaneLogging len = %d, want 2", len(c.ControlPlaneLogging))
	}
	gotLogs := strings.Join(c.ControlPlaneLogging, ",")
	if !strings.Contains(gotLogs, "api") || !strings.Contains(gotLogs, "audit") {
		t.Errorf("ControlPlaneLogging = %v, want api + audit", c.ControlPlaneLogging)
	}
	if len(c.Addons) != 1 || c.Addons[0].Name != addonName || c.Addons[0].Status != "ACTIVE" {
		t.Errorf("Addons = %+v, want one ACTIVE adot", c.Addons)
	}
	if c.NodegroupCount != 2 {
		t.Errorf("NodegroupCount = %d, want 2", c.NodegroupCount)
	}
	if c.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", c.Region)
	}
	if r.InstrumentedCount != 1 {
		t.Errorf("InstrumentedCount = %d, want 1 (cluster is covered)", r.InstrumentedCount)
	}
}

// TestScanner_EKSCompositeInstrumentedRule exercises the four
// corners of the composite rule: covered / logs-only / addon-only
// / uncovered. The InstrumentedCount tally should only catch the
// COVERED cluster; the other three are UninstrumentedCount.
func TestScanner_EKSCompositeInstrumentedRule(t *testing.T) {
	enabled := true
	clusters := []struct {
		name   string
		logs   []ekstypes.LogType
		addons []ekstypes.AddonStatus
	}{
		{name: "covered", logs: []ekstypes.LogType{ekstypes.LogTypeApi, ekstypes.LogTypeAudit}, addons: []ekstypes.AddonStatus{ekstypes.AddonStatusActive}},
		{name: "logs-only", logs: []ekstypes.LogType{ekstypes.LogTypeApi, ekstypes.LogTypeAudit}, addons: nil},
		{name: "addon-only", logs: nil, addons: []ekstypes.AddonStatus{ekstypes.AddonStatusActive}},
		{name: "uncovered", logs: nil, addons: nil},
	}
	feks := &fakeEKS{
		describeByName:       map[string]*eks.DescribeClusterOutput{},
		listAddonsByCluster:  map[string]*eks.ListAddonsOutput{},
		describeAddonByKey:   map[string]*eks.DescribeAddonOutput{},
		listNodegroupsByName: map[string]*eks.ListNodegroupsOutput{},
	}
	names := make([]string, 0, len(clusters))
	for _, c := range clusters {
		names = append(names, c.name)
		desc := &ekstypes.Cluster{
			Arn:     awssdk.String("arn:aws:eks:us-east-1:123:cluster/" + c.name),
			Version: awssdk.String("1.29"),
			Status:  ekstypes.ClusterStatusActive,
		}
		if len(c.logs) > 0 {
			desc.Logging = &ekstypes.Logging{
				ClusterLogging: []ekstypes.LogSetup{{Enabled: &enabled, Types: c.logs}},
			}
		}
		feks.describeByName[c.name] = &eks.DescribeClusterOutput{Cluster: desc}
		if len(c.addons) > 0 {
			feks.listAddonsByCluster[c.name] = &eks.ListAddonsOutput{Addons: []string{"adot"}}
			feks.describeAddonByKey[c.name+"/adot"] = &eks.DescribeAddonOutput{
				Addon: &ekstypes.Addon{
					AddonName: awssdk.String("adot"),
					Status:    c.addons[0],
				},
			}
		}
	}
	feks.listClustersPages = []*eks.ListClustersOutput{{Clusters: names}}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, eks: feks, sts: &fakeSTS{}})
	conn := &credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}}
	r, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Clusters) != 4 {
		t.Fatalf("Clusters len = %d, want 4", len(r.Clusters))
	}
	if r.InstrumentedCount != 1 {
		t.Errorf("InstrumentedCount = %d, want 1 (only the covered cluster)", r.InstrumentedCount)
	}
	if r.UninstrumentedCount != 3 {
		t.Errorf("UninstrumentedCount = %d, want 3 (logs-only + addon-only + uncovered)", r.UninstrumentedCount)
	}
}

// TestScanner_ScanEKSFailureSetsPartialAndFailedServices pins the
// slice 3b failure path: when the EKS walk fails, FailedServices
// carries "eks" and PartialReason carries the formatted
// explanation. Routes through recordPartialFailure per v0.88.3's
// accumulator helper.
func TestScanner_ScanEKSFailureSetsPartialAndFailedServices(t *testing.T) {
	feks := &fakeEKS{listClustersErr: errors.New("AccessDenied: eks:ListClusters")}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, eks: feks, sts: &fakeSTS{}})
	conn := &credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}}
	r, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !r.Partial {
		t.Error("Partial = false, want true")
	}
	if !containsString(r.FailedServices, "eks") {
		t.Errorf("FailedServices = %v, want it to contain \"eks\"", r.FailedServices)
	}
	if !strings.Contains(r.PartialReason, "eks scan failed") {
		t.Errorf("PartialReason = %q, want it to mention eks scan failed", r.PartialReason)
	}
}

// TestScanner_EKSPreflightAccessDenied pins the preflight failure
// path: when ListClusters returns AccessDenied, the preflight check
// for "eks" lands on the ValidationResult with OK=false and the
// trust-policy step suggestion (per HumanizeError's AccessDenied
// branch which is shared across all services).
func TestScanner_EKSPreflightAccessDenied(t *testing.T) {
	feks := &fakeEKS{listClustersErr: &apiErr{code: "AccessDenied", msg: "User cannot perform eks:ListClusters"}}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, eks: feks, sts: &fakeSTS{}})
	conn := &credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}}
	vr, err := s.Validate(context.Background(), conn)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var got *scanner.PreflightCheck
	for i := range vr.Preflight {
		if vr.Preflight[i].Service == "eks" {
			got = &vr.Preflight[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("Preflight missing \"eks\" entry: %+v", vr.Preflight)
	}
	if got.OK {
		t.Errorf("eks preflight OK = true, want false")
	}
	if got.Err == nil || got.Err.SuggestedStep != "trust-policy" {
		t.Errorf("eks preflight Err = %+v, want SuggestedStep=trust-policy", got.Err)
	}
}

// containsString is a small helper used by the EKS failure test.
// The discovery test file already has many ad-hoc helpers; this
// one was kept local to the slice 3b additions so a future
// rename of the file's existing helpers won't ripple here.
func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// --- DynamoDB tests (slice 4, v0.89.6) -------------------------------

// TestScanRegionDynamoDB_HappyPath_TwoTables_OneInstrumented_OneNot
// drives the mapping happy path: two tables surface through
// ListTables; the per-table fan-out populates ARN, status, billing
// mode, and Contributor Insights status for each. The one with
// ContributorInsightsStatus == "ENABLED" counts as instrumented;
// the one with "DISABLED" counts as uninstrumented. The shared
// IsInstrumented predicate evaluates per the single-axis rule.
func TestScanRegionDynamoDB_HappyPath_TwoTables_OneInstrumented_OneNot(t *testing.T) {
	const (
		nameOn      = "orders"
		nameOff     = "events"
		arnOn       = "arn:aws:dynamodb:us-east-1:123456789012:table/orders"
		arnOff      = "arn:aws:dynamodb:us-east-1:123456789012:table/events"
		billingMode = "PAY_PER_REQUEST"
	)
	fdb := &fakeDynamoDB{
		listTablesPages: []*dynamodb.ListTablesOutput{
			{TableNames: []string{nameOn, nameOff}},
		},
		describeByName: map[string]*dynamodb.DescribeTableOutput{
			nameOn: {Table: &dynamodbtypes.TableDescription{
				TableArn:           awssdk.String(arnOn),
				TableName:          awssdk.String(nameOn),
				TableStatus:        dynamodbtypes.TableStatusActive,
				BillingModeSummary: &dynamodbtypes.BillingModeSummary{BillingMode: dynamodbtypes.BillingModePayPerRequest},
			}},
			nameOff: {Table: &dynamodbtypes.TableDescription{
				TableArn:           awssdk.String(arnOff),
				TableName:          awssdk.String(nameOff),
				TableStatus:        dynamodbtypes.TableStatusActive,
				BillingModeSummary: &dynamodbtypes.BillingModeSummary{BillingMode: dynamodbtypes.BillingModePayPerRequest},
			}},
		},
		ciByName: map[string]*dynamodb.DescribeContributorInsightsOutput{
			nameOn:  {ContributorInsightsStatus: dynamodbtypes.ContributorInsightsStatusEnabled},
			nameOff: {ContributorInsightsStatus: dynamodbtypes.ContributorInsightsStatusDisabled},
		},
		tagsByARN: map[string][]dynamodbtypes.Tag{
			arnOn: {{Key: awssdk.String("Env"), Value: awssdk.String("prod")}},
		},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, dynamodb: fdb, sts: &fakeSTS{}})
	conn := &credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}}
	r, err := s.Scan(context.Background(), conn, []string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.DynamoDBTables) != 2 {
		t.Fatalf("DynamoDBTables len = %d, want 2", len(r.DynamoDBTables))
	}
	byName := map[string]scanner.DynamoDBTableSnapshot{}
	for _, tb := range r.DynamoDBTables {
		byName[tb.Name] = tb
	}
	on, ok := byName[nameOn]
	if !ok {
		t.Fatalf("missing %q snapshot", nameOn)
	}
	if on.ResourceID != arnOn {
		t.Errorf("on.ResourceID = %q, want %q", on.ResourceID, arnOn)
	}
	if on.Status != "ACTIVE" {
		t.Errorf("on.Status = %q, want ACTIVE", on.Status)
	}
	if on.BillingMode != billingMode {
		t.Errorf("on.BillingMode = %q, want %q", on.BillingMode, billingMode)
	}
	if on.ContributorInsightsStatus != "ENABLED" {
		t.Errorf("on.ContributorInsightsStatus = %q, want ENABLED", on.ContributorInsightsStatus)
	}
	if !on.IsInstrumented() {
		t.Errorf("on.IsInstrumented() = false, want true (Contributor Insights ENABLED)")
	}
	if on.Tags["Env"] != "prod" {
		t.Errorf("on.Tags[Env] = %q, want prod", on.Tags["Env"])
	}
	off, ok := byName[nameOff]
	if !ok {
		t.Fatalf("missing %q snapshot", nameOff)
	}
	if off.ContributorInsightsStatus != "DISABLED" {
		t.Errorf("off.ContributorInsightsStatus = %q, want DISABLED", off.ContributorInsightsStatus)
	}
	if off.IsInstrumented() {
		t.Errorf("off.IsInstrumented() = true, want false (Contributor Insights DISABLED)")
	}
	// One ENABLED + one DISABLED -> 1 instrumented, 1 uninstrumented.
	if r.InstrumentedCount != 1 || r.UninstrumentedCount != 1 {
		t.Errorf("counts = (instrumented=%d, uninstrumented=%d), want (1, 1)",
			r.InstrumentedCount, r.UninstrumentedCount)
	}
}

// TestScanRegionDynamoDB_EmptyAccount_ReturnsEmptyList confirms an
// account with zero tables produces zero DynamoDBTables snapshots
// and no per-region failure — the empty-inventory shape every
// category exposes.
func TestScanRegionDynamoDB_EmptyAccount_ReturnsEmptyList(t *testing.T) {
	fdb := &fakeDynamoDB{
		listTablesPages: []*dynamodb.ListTablesOutput{{TableNames: nil}},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, dynamodb: fdb, sts: &fakeSTS{}})
	r, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.DynamoDBTables) != 0 {
		t.Errorf("DynamoDBTables = %d, want 0", len(r.DynamoDBTables))
	}
	if r.Partial {
		t.Errorf("Partial = true on empty account; should be false")
	}
}

// TestScanRegionDynamoDB_PaginationHandled drives the scan past the
// first page of ListTables. Two pages of one table each must both
// surface in DynamoDBTables.
func TestScanRegionDynamoDB_PaginationHandled(t *testing.T) {
	page1Name := "p1"
	page2Name := "p2"
	fdb := &fakeDynamoDB{
		listTablesPages: []*dynamodb.ListTablesOutput{
			{TableNames: []string{page1Name}, LastEvaluatedTableName: awssdk.String(page1Name)},
			{TableNames: []string{page2Name}},
		},
		describeByName: map[string]*dynamodb.DescribeTableOutput{
			page1Name: {Table: &dynamodbtypes.TableDescription{
				TableArn:    awssdk.String("arn:aws:dynamodb:us-east-1:123:table/p1"),
				TableName:   awssdk.String(page1Name),
				TableStatus: dynamodbtypes.TableStatusActive,
			}},
			page2Name: {Table: &dynamodbtypes.TableDescription{
				TableArn:    awssdk.String("arn:aws:dynamodb:us-east-1:123:table/p2"),
				TableName:   awssdk.String(page2Name),
				TableStatus: dynamodbtypes.TableStatusActive,
			}},
		},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, dynamodb: fdb, sts: &fakeSTS{}})
	r, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.DynamoDBTables) != 2 {
		t.Fatalf("DynamoDBTables = %d, want 2 (both pages should land)", len(r.DynamoDBTables))
	}
}

// TestExpandDynamoDBTable_DescribeContributorInsightsAccessDenied_FallsBackToUnknownStatus
// pins the documented fallback path: when a customer's policy grants
// dynamodb:DescribeTable but not dynamodb:DescribeContributorInsights,
// the scan does NOT fail the whole walk. The table is still returned;
// its ContributorInsightsStatus is the sentinel "UNKNOWN" and
// IsInstrumented() returns false (Squadron cannot prove coverage).
func TestExpandDynamoDBTable_DescribeContributorInsightsAccessDenied_FallsBackToUnknownStatus(t *testing.T) {
	const name = "orders"
	const arn = "arn:aws:dynamodb:us-east-1:123:table/orders"
	fdb := &fakeDynamoDB{
		listTablesPages: []*dynamodb.ListTablesOutput{{TableNames: []string{name}}},
		describeByName: map[string]*dynamodb.DescribeTableOutput{
			name: {Table: &dynamodbtypes.TableDescription{
				TableArn:    awssdk.String(arn),
				TableName:   awssdk.String(name),
				TableStatus: dynamodbtypes.TableStatusActive,
			}},
		},
		ciErrByName: map[string]error{
			name: &apiErr{code: "AccessDeniedException", msg: "User cannot perform dynamodb:DescribeContributorInsights"},
		},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, dynamodb: fdb, sts: &fakeSTS{}})
	r, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v (AccessDenied on DescribeContributorInsights should not break the walk)", err)
	}
	if r.Partial {
		t.Errorf("Partial = true; AccessDenied on DescribeContributorInsights should be a per-table fallback, not a walk failure")
	}
	if len(r.DynamoDBTables) != 1 {
		t.Fatalf("DynamoDBTables = %d, want 1 (the table should still surface with UNKNOWN sentinel)", len(r.DynamoDBTables))
	}
	got := r.DynamoDBTables[0]
	if got.ContributorInsightsStatus != "UNKNOWN" {
		t.Errorf("ContributorInsightsStatus = %q, want UNKNOWN", got.ContributorInsightsStatus)
	}
	if got.IsInstrumented() {
		t.Errorf("IsInstrumented() = true, want false (UNKNOWN must not count as covered)")
	}
}

// TestExpandDynamoDBTable_DescribeTableNotFound_SkipsTable pins the
// race-against-deletion fallback: between ListTables and
// DescribeTable, AWS deletes the table. DescribeTable returns
// ResourceNotFoundException; the scan skips the table silently and
// continues to the next one. Result.Partial stays false because the
// race is a normal cloud-API condition, not a failed walk.
func TestExpandDynamoDBTable_DescribeTableNotFound_SkipsTable(t *testing.T) {
	const gone = "deleted-mid-scan"
	const survives = "still-here"
	fdb := &fakeDynamoDB{
		listTablesPages: []*dynamodb.ListTablesOutput{{TableNames: []string{gone, survives}}},
		describeByName: map[string]*dynamodb.DescribeTableOutput{
			survives: {Table: &dynamodbtypes.TableDescription{
				TableArn:    awssdk.String("arn:aws:dynamodb:us-east-1:123:table/still-here"),
				TableName:   awssdk.String(survives),
				TableStatus: dynamodbtypes.TableStatusActive,
			}},
		},
		describeErrByName: map[string]error{
			gone: &apiErr{code: "ResourceNotFoundException", msg: "Requested resource not found"},
		},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, dynamodb: fdb, sts: &fakeSTS{}})
	r, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v (ResourceNotFoundException should not break the walk)", err)
	}
	if r.Partial {
		t.Errorf("Partial = true; per-table ResourceNotFoundException is a race, not a walk failure")
	}
	if len(r.DynamoDBTables) != 1 {
		t.Fatalf("DynamoDBTables = %d, want 1 (the deleted table is skipped)", len(r.DynamoDBTables))
	}
	if r.DynamoDBTables[0].Name != survives {
		t.Errorf("survivor name = %q, want %q", r.DynamoDBTables[0].Name, survives)
	}
}

// TestScanRegionDynamoDB_ListTablesAccessDenied_SetsPartialAndFailedServices
// pins the failure path: when dynamodb:ListTables itself is missing
// from the policy, the per-region walk fails the way every other
// service does — Result.Partial=true, FailedServices contains
// "dynamodb", PartialReason carries the formatted explanation.
// Routes through recordPartialFailure per the v0.88.3 accumulator
// contract.
func TestScanRegionDynamoDB_ListTablesAccessDenied_SetsPartialAndFailedServices(t *testing.T) {
	fdb := &fakeDynamoDB{
		listTablesErr: &apiErr{code: "AccessDeniedException", msg: "User cannot perform dynamodb:ListTables"},
	}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, dynamodb: fdb, sts: &fakeSTS{}})
	r, err := s.Scan(context.Background(),
		&credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}},
		[]string{"us-east-1"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !r.Partial {
		t.Error("Partial = false, want true")
	}
	if !containsString(r.FailedServices, "dynamodb") {
		t.Errorf("FailedServices = %v, want it to contain \"dynamodb\"", r.FailedServices)
	}
	if !strings.Contains(r.PartialReason, "dynamodb scan failed") {
		t.Errorf("PartialReason = %q, want it to mention dynamodb scan failed", r.PartialReason)
	}
}

// TestScanner_DynamoDBPreflightAccessDenied mirrors the EKS / S3 / ALB
// preflight tests: when ListTables returns AccessDenied, the
// preflight row for "dynamodb" lands on the ValidationResult with
// OK=false and the trust-policy step suggestion.
func TestScanner_DynamoDBPreflightAccessDenied(t *testing.T) {
	fdb := &fakeDynamoDB{listTablesErr: &apiErr{code: "AccessDenied", msg: "User cannot perform dynamodb:ListTables"}}
	s := newTestScanner(t, &fakeFactory{ec2: &fakeEC2{}, lambda: &fakeLambda{}, dynamodb: fdb, sts: &fakeSTS{}})
	conn := &credstore.CloudConnection{Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}}
	vr, err := s.Validate(context.Background(), conn)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var got *scanner.PreflightCheck
	for i := range vr.Preflight {
		if vr.Preflight[i].Service == "dynamodb" {
			got = &vr.Preflight[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("Preflight missing \"dynamodb\" entry: %+v", vr.Preflight)
	}
	if got.OK {
		t.Errorf("dynamodb preflight OK = true, want false")
	}
	if got.Err == nil || got.Err.SuggestedStep != "trust-policy" {
		t.Errorf("dynamodb preflight Err = %+v, want SuggestedStep=trust-policy", got.Err)
	}
}
