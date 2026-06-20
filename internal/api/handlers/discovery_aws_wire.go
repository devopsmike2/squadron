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

// defaultAWSScannerFactory is the production AWSScannerFactory. Closes
// over the credstore Key so the returned factory has everything it
// needs to decrypt the connection's stored AWSCredentials and run a
// scan. Wired by DiscoveryHandlers.WithCredstoreKey alongside the
// equivalent marshaller — both are installed in the same call so the
// Save/Scan paths cannot end up half-wired.
//
// Lives in this file (rather than discovery.go) so the AWS SDK import
// stays isolated to the production-only wire layer.
func defaultAWSScannerFactory(key *credstore.Key) AWSScannerFactory {
	return func(conn *credstore.CloudConnection) (DiscoveryScanner, error) {
		return awsscanner.NewScannerFromConnection(conn, key)
	}
}
