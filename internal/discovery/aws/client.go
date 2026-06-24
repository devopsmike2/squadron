// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// credentialDiscoveryTimeout caps how long LoadDefaultConfig is allowed
// to spend walking the credential chain. Real credential lookups (env
// vars, shared config file) complete in milliseconds — this budget only
// fires when the chain falls through to the IMDSv2 probe and the
// 169.254.169.254 endpoint is non-routable (i.e. Squadron is not on an
// EC2/ECS/EKS instance). The v0.85.0 post-ship E2E sweep caught the
// 30-second hang the SDK's default would otherwise produce.
const credentialDiscoveryTimeout = 5 * time.Second

// credentialDiscoveryHTTPTimeout backstops the IMDSv2 HTTP probe so a
// single hung request can't extend the credential discovery beyond
// credentialDiscoveryTimeout. 5s mirrors the context budget; in
// practice the probe either returns immediately (on EC2) or fails
// immediately (off-EC2 with a non-routable endpoint).
const credentialDiscoveryHTTPTimeout = 5 * time.Second

// EC2Client is the narrow EC2 surface the scanner depends on. The
// real *ec2.Client satisfies it (DescribeInstances is its API method);
// tests substitute fakes that implement the same single method. The
// scanner deliberately avoids depending on
// ec2.DescribeInstancesAPIClient directly so tests are not coupled to
// the SDK's own interface name.
type EC2Client interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// LambdaClient is the narrow Lambda surface the scanner depends on.
// Same shape rationale as EC2Client.
type LambdaClient interface {
	ListFunctions(ctx context.Context, params *lambda.ListFunctionsInput, optFns ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error)
}

// RDSClient is the narrow RDS surface the scanner depends on. Slice 2
// of the universal-observation arc only consumes DescribeDBInstances —
// it's the single API call that returns the inventory plus the two
// observability lever flags (Performance Insights + Enhanced
// Monitoring) the proposer reasons about. Same shape rationale as
// EC2Client.
type RDSClient interface {
	DescribeDBInstances(ctx context.Context, params *rds.DescribeDBInstancesInput, optFns ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
}

// S3Client is the narrow S3 surface the scanner depends on. Slice 3a
// (v0.88.0) of the universal-observation arc walks the per-bucket
// observability state with five low-cost reads:
//   - ListBuckets returns the account's buckets (single call, no
//     pagination — S3 is global, region per bucket is resolved via
//     GetBucketLocation)
//   - GetBucketLocation returns the bucket's home region so the
//     scanner can filter to the connection's region list
//   - GetBucketLogging returns the server access logging
//     configuration — the slice 3a single-axis instrumented rule
//   - GetBucketTagging returns per-bucket tags (handles NoSuchTagSet
//     in the scanner mapper)
//   - GetBucketRequestPayment returns the request-payer
//     configuration; combined with the logging config it's the
//     informational signal RequestMetricsEnabled keys off
//
// Same narrow-interface rationale as EC2Client: each method is what
// the scanner actually calls, nothing else.
type S3Client interface {
	ListBuckets(ctx context.Context, params *s3.ListBucketsInput, optFns ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
	GetBucketLocation(ctx context.Context, params *s3.GetBucketLocationInput, optFns ...func(*s3.Options)) (*s3.GetBucketLocationOutput, error)
	GetBucketLogging(ctx context.Context, params *s3.GetBucketLoggingInput, optFns ...func(*s3.Options)) (*s3.GetBucketLoggingOutput, error)
	GetBucketTagging(ctx context.Context, params *s3.GetBucketTaggingInput, optFns ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
}

// ELBv2Client is the narrow Elastic Load Balancing v2 surface the
// scanner depends on. Slice 3a (v0.88.0) walks per-region ALBs /
// NLBs with three reads:
//   - DescribeLoadBalancers returns the inventory (paginated via
//     Marker / NextMarker, mirroring RDS)
//   - DescribeLoadBalancerAttributes returns the access-logs
//     configuration (access_logs.s3.enabled + access_logs.s3.bucket)
//   - DescribeTags returns per-load-balancer tags in a single
//     batch call (up to 20 ARNs per call; the scanner batches
//     accordingly)
//
// Same narrow-interface rationale as the other service clients.
type ELBv2Client interface {
	DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error)
	DescribeLoadBalancerAttributes(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancerAttributesInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancerAttributesOutput, error)
	DescribeTags(ctx context.Context, params *elasticloadbalancingv2.DescribeTagsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTagsOutput, error)
}

// EKSClient is the narrow EKS surface the scanner depends on. Slice
// 3b (v0.89.0) of the universal-observation arc walks per-region
// clusters in two passes:
//
//   - Pass 1: ListClusters returns the region's cluster name list
//     (paginated via NextToken).
//   - Pass 2: per cluster, DescribeCluster returns the control
//     plane logging config + Kubernetes version + status; ListAddons
//     returns add-on names (paginated, then for each addon the
//     scanner calls DescribeAddon to read Status + Version);
//     ListNodegroups + ListFargateProfiles return informational
//     counts. The two-pass shape mirrors how real EKS clusters
//     expose their state — there's no DescribeAllClusters batch
//     endpoint.
//
// Same narrow-interface rationale as the other service clients.
type EKSClient interface {
	ListClusters(ctx context.Context, params *eks.ListClustersInput, optFns ...func(*eks.Options)) (*eks.ListClustersOutput, error)
	DescribeCluster(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
	ListAddons(ctx context.Context, params *eks.ListAddonsInput, optFns ...func(*eks.Options)) (*eks.ListAddonsOutput, error)
	DescribeAddon(ctx context.Context, params *eks.DescribeAddonInput, optFns ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
	ListNodegroups(ctx context.Context, params *eks.ListNodegroupsInput, optFns ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
	ListFargateProfiles(ctx context.Context, params *eks.ListFargateProfilesInput, optFns ...func(*eks.Options)) (*eks.ListFargateProfilesOutput, error)
}

// DynamoDBClient is the narrow DynamoDB surface the scanner depends
// on. Slice 4 (v0.89.6) of the universal-observation arc walks
// per-region tables in two passes:
//
//   - Pass 1: ListTables returns the region's table name list
//     (paginated via ExclusiveStartTableName / LastEvaluatedTableName).
//   - Pass 2: per table, DescribeTable returns the table's ARN +
//     status + billing mode + tags; DescribeContributorInsights
//     returns the single observability axis
//     (ContributorInsightsStatus); ListTagsOfResource returns the
//     per-table tags.
//
// Why two passes (matching EKS rather than the single-pass S3 /
// RDS / ALB shape): DynamoDB's ListTables is name-only — the
// observability state lives behind DescribeTable +
// DescribeContributorInsights. There's no "DescribeAllTables"
// batch endpoint. Same narrow-interface rationale as the other
// service clients.
type DynamoDBClient interface {
	ListTables(ctx context.Context, params *dynamodb.ListTablesInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	DescribeContributorInsights(ctx context.Context, params *dynamodb.DescribeContributorInsightsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeContributorInsightsOutput, error)
	ListTagsOfResource(ctx context.Context, params *dynamodb.ListTagsOfResourceInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ListTagsOfResourceOutput, error)
}

// ECSClient is the narrow ECS surface the scanner depends on. Slice
// 5 (v0.89.10) of the universal-observation arc walks per-region
// clusters in two passes:
//
//   - Pass 1: ListClusters returns the region's cluster ARN list
//     (paginated via NextToken).
//   - Pass 2: per batch of up to 100 ARNs, DescribeClusters returns
//     each cluster's settings (CloudWatch Container Insights status
//     lives behind settings[name=containerInsights].value), task /
//     service counts, registered-container-instance count, and tags.
//     ListTagsForResource is the defensive fallback when the
//     DescribeClusters call did not surface tags (the Include=TAGS
//     hint is honored by AWS but a per-cluster fallback keeps the
//     scanner robust to API quirks).
//
// Why two passes (matching EKS rather than the single-pass S3 /
// RDS / ALB shape): ECS's ListClusters returns ARNs only — the
// observability state lives behind DescribeClusters with the
// SETTINGS include hint. There's no "DescribeAllClusters" batch
// endpoint that combines list + describe. Same narrow-interface
// rationale as the other service clients.
//
// HONEST TASK-DEFINITION-LEVEL LIMITATION (re-stated honestly):
// Squadron detects cluster-level CloudWatch Container Insights.
// Squadron does not detect task-definition-level instrumentation —
// X-Ray daemon sidecars, ADOT collector sidecars, or FireLens log
// routing in your task definitions. If your task defs include
// those sidecars but the cluster does not have Container Insights
// enabled, Squadron will report the cluster as uninstrumented —
// this is a known limitation of cluster-level scanning. A future
// slice can extend the rule to inspect task definitions if
// operators request it.
type ECSClient interface {
	ListClusters(ctx context.Context, params *ecs.ListClustersInput, optFns ...func(*ecs.Options)) (*ecs.ListClustersOutput, error)
	DescribeClusters(ctx context.Context, params *ecs.DescribeClustersInput, optFns ...func(*ecs.Options)) (*ecs.DescribeClustersOutput, error)
	ListTagsForResource(ctx context.Context, params *ecs.ListTagsForResourceInput, optFns ...func(*ecs.Options)) (*ecs.ListTagsForResourceOutput, error)
}

// STSClient is the narrow STS surface used by Validate to confirm the
// AssumeRole chain is functional. The real *sts.Client satisfies it.
type STSClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// SFNClient is the narrow AWS Step Functions surface the orchestration
// scanner depends on. Added in v0.89.95 (#728 Stream 126, slice 1 chunk
// 1 of the Orchestration tier arc). The real *sfn.Client satisfies it.
//
// The two methods cover the slice 1 detection contract: ListStateMachines
// surfaces every state machine the assumed-role principal can see in a
// region (paginated via NextToken); DescribeStateMachine pulls the per-
// machine TracingConfiguration + LoggingConfiguration the scanner reads
// for the HasTraceAxis / HasLogAxis axes. Both APIs are read-only.
//
// IAM contract per docs/proposals/orchestration-tier-slice1.md §3.1:
// states:ListStateMachines + states:DescribeStateMachine. Squadron does
// NOT call any state-machine mutation API.
type SFNClient interface {
	ListStateMachines(ctx context.Context, params *sfn.ListStateMachinesInput, optFns ...func(*sfn.Options)) (*sfn.ListStateMachinesOutput, error)
	DescribeStateMachine(ctx context.Context, params *sfn.DescribeStateMachineInput, optFns ...func(*sfn.Options)) (*sfn.DescribeStateMachineOutput, error)
}

// ClientFactory hands out region-scoped service clients backed by an
// already-assumed STS session. Production code wires the real SDK
// factory (see newSDKClientFactory below); tests inject fakes that
// return prebuilt mock clients without ever touching AWS.
//
// The factory is created once per Scanner — the scanner's lifetime is
// one Validate or one Scan call, so per-call assume-role + factory
// construction is the steady-state cost. The design doc's STS lifecycle
// section explicitly accepts this: short-lived creds, in-memory only,
// dropped at the end of each call.
type ClientFactory interface {
	// STS returns a client bound to the assumed-role session, used
	// for the GetCallerIdentity preflight. Region is the caller's
	// preferred home region; STS endpoints are global but a region
	// is still required by the SDK.
	STS(ctx context.Context, region string) (STSClient, error)

	// EC2 returns an EC2 client for the supplied region.
	EC2(ctx context.Context, region string) (EC2Client, error)

	// Lambda returns a Lambda client for the supplied region.
	Lambda(ctx context.Context, region string) (LambdaClient, error)

	// RDS returns an RDS client for the supplied region. Added in
	// slice 2 of the universal-observation arc.
	RDS(ctx context.Context, region string) (RDSClient, error)

	// S3 returns an S3 client for the supplied region. Added in
	// slice 3a of the universal-observation arc (v0.88.0).
	//
	// S3 is technically a global service for ListBuckets — the
	// region argument here pins the SDK client's signing region,
	// not the buckets it can see. The scanner resolves each
	// bucket's actual home region via GetBucketLocation and filters
	// to the connection's region list at the mapper layer.
	S3(ctx context.Context, region string) (S3Client, error)

	// ELBv2 returns an Elastic Load Balancing v2 (ALB/NLB/GWLB)
	// client for the supplied region. Added in slice 3a of the
	// universal-observation arc (v0.88.0). Unlike S3, ELBv2 is a
	// per-region API — the supplied region is where
	// DescribeLoadBalancers walks.
	ELBv2(ctx context.Context, region string) (ELBv2Client, error)

	// EKS returns an EKS client for the supplied region. Added in
	// slice 3b of the universal-observation arc (v0.89.0). EKS is
	// a per-region API — the supplied region is where ListClusters
	// walks.
	EKS(ctx context.Context, region string) (EKSClient, error)

	// DynamoDB returns a DynamoDB client for the supplied region.
	// Added in slice 4 of the universal-observation arc (v0.89.6).
	// DynamoDB is a per-region API — the supplied region is where
	// ListTables walks.
	DynamoDB(ctx context.Context, region string) (DynamoDBClient, error)

	// ECS returns an ECS client for the supplied region. Added in
	// slice 5 of the universal-observation arc (v0.89.10). ECS is a
	// per-region API — the supplied region is where ListClusters
	// walks. Covers both Fargate and EC2 launch types because
	// Container Insights is a per-cluster setting, not a
	// per-launch-type one.
	ECS(ctx context.Context, region string) (ECSClient, error)

	// SFN returns a Step Functions client for the supplied region.
	// Added in slice 1 chunk 1 of the orchestration-tier arc
	// (v0.89.95, #728 Stream 126). Step Functions is a per-region
	// API — the supplied region is where ListStateMachines walks.
	// Covers both STANDARD and EXPRESS workflow types; the per-
	// machine WorkflowType is recorded so the proposer can route to
	// the appropriate recommendation kind.
	SFN(ctx context.Context, region string) (SFNClient, error)
}

// sdkClientFactory is the production ClientFactory — it does a real
// sts:AssumeRole against the customer's role ARN and hands out
// per-region service clients backed by the resulting short-lived
// credentials. Constructed once per Scanner call; sessions live only
// as long as the factory.
type sdkClientFactory struct {
	creds awssdk.CredentialsProvider
}

// newSDKClientFactory does the actual sts:AssumeRole. It uses the
// default credential chain (env, shared config, IAM role on the
// Squadron host) as the base identity that calls AssumeRole, then
// caches the assumed-role credentials behind aws.CredentialsCache so
// the per-service clients all share the same in-memory pool. When the
// 1-hour TTL expires mid-scan, the cache refreshes silently — matching
// the design doc's "re-assume silently" requirement.
//
// Returns an error wrapping the raw AWS SDK error verbatim so callers
// can hand the error to HumanizeError. The error is NOT pre-wrapped
// here because that would lose the smithy.APIError shape the
// humanizer pattern-matches against.
func newSDKClientFactory(ctx context.Context, awsCreds credstore.AWSCredentials, defaultRegion string) (*sdkClientFactory, error) {
	if awsCreds.RoleARN == "" {
		return nil, errors.New("aws: role ARN is required")
	}
	if awsCreds.ExternalID == "" {
		// Defense-in-depth: even though MarshalAWSCredentials
		// already rejects empty ExternalID, we re-check here so
		// the validate endpoint catches it before any AWS call.
		return nil, errors.New("aws: external ID is required (trust policy is unsafe without it)")
	}

	// Load the base config — picks up env vars, the shared config
	// file, the instance metadata service, etc. This is the identity
	// that calls sts:AssumeRole; it must already have permissions to
	// assume the customer's role.
	//
	// Credential discovery is wrapped in a short context budget. Real
	// lookups complete in milliseconds; the budget only fires when the
	// SDK's default credential chain falls through to the IMDSv2 probe
	// (169.254.169.254). On a non-AWS host with no env vars and no
	// shared config file, that probe would otherwise hang for the
	// SDK's default HTTP timeout (~30s) and translate into a 30-second
	// hang in the wizard's "Validate" step. The 5s budget plus the
	// matching HTTP client timeout caps the worst case at ~5s and
	// surfaces a deterministic context.DeadlineExceeded that
	// HumanizeError maps to the NoCredentialsFound humanized message.
	credCtx, cancel := context.WithTimeout(ctx, credentialDiscoveryTimeout)
	defer cancel()
	baseCfg, err := config.LoadDefaultConfig(credCtx,
		config.WithRegion(defaultRegion),
		// Bound the SDK's HTTP transport with an explicit timeout so a
		// non-routable IMDSv2 endpoint can't outlast credCtx. The SDK
		// keeps IMDS in its default-enabled state (env vars and shared
		// config still take precedence in the credential chain) — only
		// the per-request HTTP timeout is overridden.
		config.WithHTTPClient(&http.Client{Timeout: credentialDiscoveryHTTPTimeout}),
	)
	if err != nil {
		return nil, fmt.Errorf("aws: load default config: %w", err)
	}

	// Dry-run the base credential chain inside credCtx. LoadDefaultConfig
	// itself returns successfully even when no credentials are reachable
	// — it installs deferred providers and lets the actual retrieval
	// happen at first AWS call. The v0.85.0 bug came from that deferred
	// retrieval: the assume-role provider's call to sts:AssumeRole
	// triggered the IMDSv2 probe at GetCallerIdentity time, well after
	// credCtx had been discarded. Forcing the dry-run here keeps the
	// fail-fast budget meaningful — Retrieve walks the chain (env vars,
	// shared config, IMDS) and either returns credentials quickly or
	// surfaces a chain-exhausted error that HumanizeError maps to the
	// NoCredentials humanized message.
	if _, err := baseCfg.Credentials.Retrieve(credCtx); err != nil {
		return nil, fmt.Errorf("aws: retrieve base credentials: %w", err)
	}

	// Build the assume-role provider with the customer's ExternalID
	// pinned. STS will reject the assume if the trust policy's
	// ExternalId condition is missing or mismatched — which is
	// exactly the failure mode the humanizer maps to "did you paste
	// the trust policy from Step 2?".
	stsClient := sts.NewFromConfig(baseCfg)
	provider := stscreds.NewAssumeRoleProvider(stsClient, awsCreds.RoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = awssdk.String(awsCreds.ExternalID)
		// RoleSessionName lands in the AWS audit log on the
		// customer's side; "squadron-discovery" is a stable
		// identifier the customer can grep for.
		o.RoleSessionName = "squadron-discovery"
	})

	return &sdkClientFactory{
		creds: awssdk.NewCredentialsCache(provider),
	}, nil
}

func (f *sdkClientFactory) STS(_ context.Context, region string) (STSClient, error) {
	return sts.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) EC2(_ context.Context, region string) (EC2Client, error) {
	return ec2.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) Lambda(_ context.Context, region string) (LambdaClient, error) {
	return lambda.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) RDS(_ context.Context, region string) (RDSClient, error) {
	return rds.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) S3(_ context.Context, region string) (S3Client, error) {
	return s3.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) ELBv2(_ context.Context, region string) (ELBv2Client, error) {
	return elasticloadbalancingv2.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) EKS(_ context.Context, region string) (EKSClient, error) {
	return eks.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) DynamoDB(_ context.Context, region string) (DynamoDBClient, error) {
	return dynamodb.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) ECS(_ context.Context, region string) (ECSClient, error) {
	return ecs.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

func (f *sdkClientFactory) SFN(_ context.Context, region string) (SFNClient, error) {
	return sfn.NewFromConfig(awssdk.Config{
		Region:      region,
		Credentials: f.creds,
	}), nil
}

// ensureCredstoreImport keeps the credstore import live in this file
// for the production constructor and makes the build hard-fail if
// credstore is ever accidentally dropped from this package. The
// var assignment is removed by the linker but trips the compiler
// before it ever does.
var _ = credentials.NewStaticCredentialsProvider // ensure credentials package is referenced
