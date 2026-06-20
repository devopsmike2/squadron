// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
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
