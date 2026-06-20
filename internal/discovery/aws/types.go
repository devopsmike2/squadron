// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
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
