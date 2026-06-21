// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Local type aliases that keep the AWS SDK type names from leaking
// into mapping function signatures. They make the mapper code
// readable without taking on a hard dependency on the SDK's package
// path in every signature.
//
// The aliases also serve the tests: a test can construct an
// ec2Instance / lambdaFunction / rdsDBInstance directly without
// importing the SDK types separately. The aliases are package-private
// so external code keeps using the SDK's canonical names.
type ec2Instance = ec2types.Instance

type lambdaFunction = lambdatypes.FunctionConfiguration

type rdsDBInstance = rdstypes.DBInstance

// s3Bucket / elbv2LoadBalancer are the slice 3a (v0.88.0) additions.
// Same alias rationale: keep mapping function signatures readable
// without leaking the SDK's nested package paths.
type s3Bucket = s3types.Bucket

type elbv2LoadBalancer = elbv2types.LoadBalancer

// eksCluster + eksAddon are the slice 3b (v0.89.0) additions. Same
// alias rationale: keep mapper signatures readable without leaking
// the SDK's nested package paths.
type eksCluster = ekstypes.Cluster

type eksAddon = ekstypes.Addon

// dynamoDBTable is the slice 4 (v0.89.6) addition. Same alias
// rationale: keep mapper signatures readable without leaking the
// SDK's nested package paths. AWS exposes the table description
// behind TableDescription rather than a top-level Table (the
// DescribeTable response carries a *TableDescription field), so
// the alias mirrors what the scanner mapper actually receives.
type dynamoDBTable = dynamodbtypes.TableDescription

// ecsCluster + ecsClusterFailure are the slice 5 (v0.89.10)
// additions. Same alias rationale: keep mapper signatures readable
// without leaking the SDK's nested package paths. AWS exposes the
// per-cluster describe response behind types.Cluster (the
// DescribeClusters response carries a []Cluster slice) and the
// race-against-deletion failure shape behind types.Failure (the
// DescribeClusters response carries a []Failure slice — entries
// here flag clusters that disappeared between ListClusters and
// DescribeClusters).
type ecsCluster = ecstypes.Cluster

type ecsClusterFailure = ecstypes.Failure
