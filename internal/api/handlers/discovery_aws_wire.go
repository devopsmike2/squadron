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
	return commercialAWSScannerFactory(key, false, false, nil)
}

// CommercialObservationStore is the write-capable observation store the
// commercial-tier detectors persist to. The production *sqlite.Storage
// (appStore) satisfies it; it is the union of the AWS scanner's
// cold-start + error-rate store contracts.
type CommercialObservationStore interface {
	awsscanner.ColdStartStore
	awsscanner.ErrorRateStore
}

// commercialAWSScannerFactory builds the production AWS scanner factory and
// activates serverless regression detectors on each constructed scanner per
// two independent opt-in gates, both wired to the same write-capable
// observation store:
//
//   - commercialEnabled (config.CommercialDetectors.Enabled): activates the
//     add-on-dependent cold-start + error-rate detectors (Lambda Insights).
//   - serverlessMetricEnabled (config.ServerlessMetricDetection.Enabled):
//     activates the native-metric error-rate detector (AWS/Lambda Errors +
//     Invocations) WITHOUT requiring the commercial add-on. Cold-start is not
//     covered here — it needs Lambda Insights and rides the commercial gate.
//
// Default (both false / store=nil) is the OSS dormant path: no detectors run.
// The two gates compose; if both are on, EnableCommercialDetectors already
// covers error-rate so the second call is a harmless no-op on the same store.
func commercialAWSScannerFactory(key *credstore.Key, commercialEnabled, serverlessMetricEnabled bool, obs CommercialObservationStore) AWSScannerFactory {
	return func(conn *credstore.CloudConnection) (DiscoveryScanner, error) {
		sc, err := awsscanner.NewScannerFromConnection(conn, key)
		if err != nil {
			return nil, err
		}
		if commercialEnabled && obs != nil {
			sc.EnableCommercialDetectors(obs, obs)
		}
		if serverlessMetricEnabled && obs != nil {
			sc.EnableServerlessMetricDetection(obs)
		}
		return sc, nil
	}
}
