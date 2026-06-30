// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package scannerfactory provides the production implementations of
// the per-cloud discovery ScannerFactory interfaces consumed by the
// API server's GCP / Azure / OCI discovery trampolines.
//
// Background: the discovery handlers (internal/api/handlers/
// discovery_{gcp,azure,oci}.go) depend on a provider-agnostic
// scanner.Scanner interface so the handler chunk and the scanner chunk
// could ship in parallel worktrees. Two final composition steps were
// left undone:
//
//  1. main.go was meant to "compose the concrete factory once both
//     chunks land" (see the Set{GCP,Azure,OCI}DiscoveryScannerFactory
//     godoc in internal/api/server.go) — it never did, so the
//     trampolines 503'd with "<cloud> discovery is not configured".
//  2. The chunk-2 *Scanner types expose Scan(ctx) (not the
//     provider-agnostic Scan(ctx, conn, regions)) and carry no
//     Validate method — the handler godoc references a "wrapper that
//     pulls the connection triple out of its constructor closure" that
//     was likewise never written.
//
// This package supplies both: tiny per-cloud factories plus the
// adapter that bridges each chunk-2 *Scanner onto scanner.Scanner.
// The adapters embed the concrete *Scanner, so Provider() and
// ScanEventSources() promote unchanged (the handler's
// EventSourceDiscoveryScanner type-assertion keeps working); only Scan
// needs a signature bridge and Validate needs adding. Every cloud's
// *Scanner acquires its own credentials/token at Scan() time from the
// fields mapped here, so the factory's whole job is to copy the
// persisted (and, for the secret, already-unsealed) connection fields
// into the Scanner struct. The factories hold no state and are safe
// for concurrent use.
//
// Shipped: v0.89.219.
package scannerfactory

import (
	"context"

	"github.com/devopsmike2/squadron/internal/discovery/azure"
	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/gcp"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/oci"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// probeValidate maps a probe Scan's outcome onto a ValidationResult.
// The per-cloud validate endpoints already use Scan as their
// confidence probe (they call scn.Scan, never scn.Validate), so the
// interface's Validate honors the same contract: a clean Scan means
// the principal authenticated; an error means it did not. The chunk-2
// scanners guarantee their error strings name failure shapes without
// echoing the credential, so surfacing err.Error() here is safe.
func probeValidate(_ *scanner.Result, err error) (*scanner.ValidationResult, error) {
	if err != nil {
		return &scanner.ValidationResult{
			AssumeRoleOK:  false,
			AssumeRoleErr: &scanner.HumanizedError{Message: err.Error(), SuggestedStep: "validate"},
		}, nil
	}
	return &scanner.ValidationResult{AssumeRoleOK: true}, nil
}

// --- Azure ----------------------------------------------------------

// azureScanner adapts *azure.Scanner onto scanner.Scanner. Embedding
// promotes Provider() and ScanEventSources(); Scan is signature-
// bridged and Validate is added.
type azureScanner struct{ *azure.Scanner }

func (a azureScanner) Scan(ctx context.Context, _ *credstore.CloudConnection, _ []string) (*scanner.Result, error) {
	res, err := a.Scanner.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (a azureScanner) Validate(ctx context.Context, _ *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	return probeValidate(a.Scan(ctx, nil, nil))
}

// AzureFactory is the production handlers.AzureScannerFactory.
type AzureFactory struct {
	// CommercialDetectors activates the App Insights-backed cold-start +
	// error-rate detectors on each built scanner (#153 productization).
	// Wired from config.CommercialDetectors.Enabled in main.go; default
	// false keeps the detectors dormant (OSS posture).
	CommercialDetectors bool
}

// Build maps a persisted AzureConnection + unsealed client_secret into
// a live scanner. The Scanner performs the OAuth2 client-credentials
// exchange internally at Scan() time.
func (f AzureFactory) Build(conn azureconnstore.AzureConnection, clientSecret []byte) (scanner.Scanner, error) {
	sc := &azure.Scanner{
		TenantID:       conn.TenantID,
		SubscriptionID: conn.SubscriptionID,
		ClientID:       conn.ClientID,
		ClientSecret:   clientSecret,
		Location:       conn.Location,
	}
	if f.CommercialDetectors {
		sc = sc.WithCommercialDetectors(true)
	}
	return azureScanner{sc}, nil
}

// --- GCP ------------------------------------------------------------

// gcpScanner adapts *gcp.Scanner onto scanner.Scanner.
type gcpScanner struct{ *gcp.Scanner }

func (g gcpScanner) Scan(ctx context.Context, _ *credstore.CloudConnection, _ []string) (*scanner.Result, error) {
	res, err := g.Scanner.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (g gcpScanner) Validate(ctx context.Context, _ *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	return probeValidate(g.Scan(ctx, nil, nil))
}

// GCPFactory is the production handlers.GCPScannerFactory.
type GCPFactory struct{}

// Build maps a persisted GCPConnection + unsealed Service Account JSON
// into a live scanner. The Scanner builds an oauth2-backed client from
// the SA JSON at Scan() time.
func (GCPFactory) Build(conn *gcpconnstore.GCPConnection, saJSON []byte) (scanner.Scanner, error) {
	return gcpScanner{&gcp.Scanner{
		ProjectID: conn.ProjectID,
		SAJSON:    saJSON,
		Region:    conn.Region,
	}}, nil
}

// --- OCI ------------------------------------------------------------

// ociScanner adapts *oci.Scanner onto scanner.Scanner.
type ociScanner struct{ *oci.Scanner }

func (o ociScanner) Scan(ctx context.Context, _ *credstore.CloudConnection, _ []string) (*scanner.Result, error) {
	res, err := o.Scanner.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (o ociScanner) Validate(ctx context.Context, _ *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	return probeValidate(o.Scan(ctx, nil, nil))
}

// OCIObservationStore is the write-capable cold-start + error-rate
// observation store the native-metric serverless detectors persist to.
// The production *sqlite.Storage (appStore) satisfies it.
type OCIObservationStore interface {
	oci.ColdStartStore
	oci.ErrorRateStore
}

// OCIFactory is the production handlers.OCIScannerFactory.
type OCIFactory struct {
	// MetricDetection activates the OCI Monitoring-backed serverless
	// cold-start + error-rate detectors on each built scanner
	// (config.ServerlessMetricDetection.Enabled; option 2, #300). Wired
	// from main.go. Default false: the monitoring client stays nil and
	// the serverless detection block in Scan no-ops, so a stock scan
	// issues zero billed metric reads — the OSS posture.
	MetricDetection bool

	// ObsStore is the write-capable observation store the detectors
	// persist to. Required (alongside MetricDetection) to activate.
	ObsStore OCIObservationStore
}

// Build maps a persisted OCIConnection + unsealed RSA private key into
// a live scanner. The Scanner signs OCI REST calls with the key at
// Scan() time. OCI Region is required (regional endpoints) — the
// wizard + handler enforce a non-empty value before persisting.
//
// When MetricDetection is enabled with a store, the scanner is wired
// with the (already-implemented) signed OCI Monitoring client + the
// cold-start / error-rate observation stores + the connection id that
// scopes persisted observations, so a scan walks Functions and runs
// the native-metric detectors.
func (f OCIFactory) Build(conn ociconnstore.OCIConnection, privateKey []byte) (scanner.Scanner, error) {
	sc := &oci.Scanner{
		TenancyOCID: conn.TenancyOCID,
		UserOCID:    conn.UserOCID,
		Fingerprint: conn.Fingerprint,
		PrivateKey:  privateKey,
		Region:      conn.Region,
	}
	if f.MetricDetection && f.ObsStore != nil {
		sc = sc.WithMonitoringClient(oci.NewSignedMonitoringClient(sc)).
			WithColdStartStore(f.ObsStore).
			WithErrorRateStore(f.ObsStore).
			WithConnectionID(conn.ID)
	}
	return ociScanner{sc}, nil
}
