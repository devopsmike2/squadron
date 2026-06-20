// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	awsscanner "github.com/devopsmike2/squadron/internal/discovery/aws"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// defaultAWSValidatorFactory is the production AWSValidatorFactory.
// Lives in its own file so the test file's mock factory doesn't have
// to fight with the SDK import path — anyone reading discovery.go can
// follow the contract without scrolling past SDK plumbing.
func defaultAWSValidatorFactory(creds credstore.AWSCredentials, accountID string) DiscoveryValidator {
	return awsscanner.NewScannerForValidation(creds, accountID)
}
