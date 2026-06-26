// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scannerfactory

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// eventSourcer mirrors handlers.EventSourceDiscoveryScanner. The
// discovery scan handler type-asserts the built scanner to this
// interface to fold in event sources; if the adapter fails to promote
// ScanEventSources, event-source discovery silently vanishes. These
// tests assert the promotion holds — the regression that would
// otherwise ship a connected-but-blind scanner.
type eventSourcer interface {
	ScanEventSources(context.Context, scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error)
}

// The factories must (a) satisfy scanner.Scanner (compile-time, via
// the return type), (b) map every persisted field onto the embedded
// concrete scanner, (c) thread the unsealed secret through, (d) report
// the correct Provider, and (e) still satisfy EventSourceDiscoveryScanner.

func TestAzureFactory_BuildMapsFieldsAndPromotesEventSources(t *testing.T) {
	conn := azureconnstore.AzureConnection{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		Location:       "eastus",
	}
	scn, err := AzureFactory{}.Build(conn, []byte("sp-secret"))
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	a, ok := scn.(azureScanner)
	if !ok {
		t.Fatalf("expected azureScanner adapter, got %T", scn)
	}
	if a.TenantID != conn.TenantID || a.SubscriptionID != conn.SubscriptionID ||
		a.ClientID != conn.ClientID || a.Location != conn.Location {
		t.Errorf("field mapping mismatch: %+v vs conn %+v", a.Scanner, conn)
	}
	if string(a.ClientSecret) != "sp-secret" {
		t.Errorf("client secret not threaded through")
	}
	if got := scn.Provider(); got != credstore.ProviderAzure {
		t.Errorf("Provider() = %q, want %q", got, credstore.ProviderAzure)
	}
	if _, ok := scn.(eventSourcer); !ok {
		t.Errorf("adapter must promote ScanEventSources (EventSourceDiscoveryScanner)")
	}
}

func TestGCPFactory_BuildMapsFieldsAndPromotesEventSources(t *testing.T) {
	conn := &gcpconnstore.GCPConnection{ProjectID: "my-project-123", Region: "us-central1"}
	scn, err := GCPFactory{}.Build(conn, []byte(`{"type":"service_account"}`))
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	g, ok := scn.(gcpScanner)
	if !ok {
		t.Fatalf("expected gcpScanner adapter, got %T", scn)
	}
	if g.ProjectID != conn.ProjectID || g.Region != conn.Region {
		t.Errorf("field mapping mismatch: %+v vs conn %+v", g.Scanner, conn)
	}
	if string(g.SAJSON) != `{"type":"service_account"}` {
		t.Errorf("SA JSON not threaded through")
	}
	if got := scn.Provider(); got != credstore.ProviderGCP {
		t.Errorf("Provider() = %q, want %q", got, credstore.ProviderGCP)
	}
	if _, ok := scn.(eventSourcer); !ok {
		t.Errorf("adapter must promote ScanEventSources (EventSourceDiscoveryScanner)")
	}
}

func TestOCIFactory_BuildMapsFieldsAndPromotesEventSources(t *testing.T) {
	conn := ociconnstore.OCIConnection{
		TenancyOCID: "ocid1.tenancy.oc1..aaaa",
		UserOCID:    "ocid1.user.oc1..bbbb",
		Fingerprint: "aa:bb:cc:dd",
		Region:      "us-phoenix-1",
	}
	scn, err := OCIFactory{}.Build(conn, []byte("-----BEGIN RSA PRIVATE KEY-----"))
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	o, ok := scn.(ociScanner)
	if !ok {
		t.Fatalf("expected ociScanner adapter, got %T", scn)
	}
	if o.TenancyOCID != conn.TenancyOCID || o.UserOCID != conn.UserOCID ||
		o.Fingerprint != conn.Fingerprint || o.Region != conn.Region {
		t.Errorf("field mapping mismatch: %+v vs conn %+v", o.Scanner, conn)
	}
	if string(o.PrivateKey) != "-----BEGIN RSA PRIVATE KEY-----" {
		t.Errorf("private key not threaded through")
	}
	if got := scn.Provider(); got != credstore.ProviderOCI {
		t.Errorf("Provider() = %q, want %q", got, credstore.ProviderOCI)
	}
	if _, ok := scn.(eventSourcer); !ok {
		t.Errorf("adapter must promote ScanEventSources (EventSourceDiscoveryScanner)")
	}
}
