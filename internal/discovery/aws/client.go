// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

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

// STSClient is the narrow STS surface used by Validate to confirm the
// AssumeRole chain is functional. The real *sts.Client satisfies it.
type STSClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
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
	baseCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(defaultRegion))
	if err != nil {
		return nil, fmt.Errorf("aws: load default config: %w", err)
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

// ensureCredstoreImport keeps the credstore import live in this file
// for the production constructor and makes the build hard-fail if
// credstore is ever accidentally dropped from this package. The
// var assignment is removed by the linker but trips the compiler
// before it ever does.
var _ = credentials.NewStaticCredentialsProvider // ensure credentials package is referenced
