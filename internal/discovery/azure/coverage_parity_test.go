// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAzureParity mocks the ARM surface for the coverage-parity tiers
// (storage accounts + load balancers) plus the token endpoint. Every
// other list endpoint (VMs / SQL / AKS / Functions) returns an empty
// value so the full Scan() completes with only the parity rows.
type fakeAzureParity struct {
	storageJSON string
	lbJSON      string
	// diagEnabled controls whether diagnosticSettings probes report an
	// enabled log category (true) or 404 "no settings" (false).
	diagEnabled bool
}

func (f *fakeAzureParity) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/oauth2/v2.0/token"):
			_ = json.NewEncoder(w).Encode(armTokenResponse{AccessToken: "fake-bearer", TokenType: "Bearer", ExpiresIn: 3600})
		case strings.Contains(path, "diagnosticSettings"):
			if !f.diagEnabled {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"NotFound","message":"no settings"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"value":[{"properties":{"logs":[{"category":"StorageRead","enabled":true}]}}]}`))
		case strings.HasSuffix(path, "/providers/Microsoft.Storage/storageAccounts"):
			_, _ = w.Write([]byte(f.storageJSON))
		case strings.HasSuffix(path, "/providers/Microsoft.Network/loadBalancers"):
			_, _ = w.Write([]byte(f.lbJSON))
		default:
			// VMs / SQL / AKS / Functions etc. — empty list.
			_, _ = w.Write([]byte(`{"value":[]}`))
		}
	})
}

func newParityScanner(t *testing.T, fake *fakeAzureParity) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		tokenEndpoint:  srv.URL,
	}
}

func TestScan_AzureStorage_ObjectStores(t *testing.T) {
	fake := &fakeAzureParity{
		diagEnabled: true,
		storageJSON: `{"value":[{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/acct1","name":"acct1","location":"eastus","tags":{"env":"prod"}}]}`,
		lbJSON:      `{"value":[]}`,
	}
	result, err := newParityScanner(t, fake).Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, result.ObjectStores, 1)
	o := result.ObjectStores[0]
	assert.Equal(t, "acct1", o.ResourceID)
	assert.Equal(t, "eastus", o.Region)
	assert.True(t, o.ServerAccessLoggingEnabled, "blob diagnostic logging enabled => instrumented")
	assert.Equal(t, "prod", o.Tags["env"])
}

func TestScan_AzureStorage_NoLogging_NotInstrumented(t *testing.T) {
	fake := &fakeAzureParity{
		diagEnabled: false, // diagnosticSettings 404 => no logging
		storageJSON: `{"value":[{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/acct2","name":"acct2","location":"westus"}]}`,
		lbJSON:      `{"value":[]}`,
	}
	result, err := newParityScanner(t, fake).Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, result.ObjectStores, 1)
	assert.False(t, result.ObjectStores[0].ServerAccessLoggingEnabled)
}

func TestScan_AzureLoadBalancers(t *testing.T) {
	fake := &fakeAzureParity{
		diagEnabled: true,
		storageJSON: `{"value":[]}`,
		lbJSON:      `{"value":[{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1","name":"lb1","location":"eastus","sku":{"name":"Standard"},"properties":{"frontendIPConfigurations":[{"properties":{"publicIPAddress":{"id":"/pip/1"}}}]}}]}`,
	}
	result, err := newParityScanner(t, fake).Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, result.LoadBalancers, 1)
	lb := result.LoadBalancers[0]
	assert.Equal(t, "lb1", lb.Name)
	assert.Equal(t, "Standard", lb.Type)
	assert.Equal(t, "internet-facing", lb.Scheme, "public frontend => internet-facing")
	assert.True(t, lb.AccessLogsEnabled, "diagnostic logging enabled => instrumented")
	assert.Equal(t, "eastus", lb.Region)
}

func TestScan_AzureLoadBalancers_InternalScheme(t *testing.T) {
	fake := &fakeAzureParity{
		diagEnabled: false,
		storageJSON: `{"value":[]}`,
		lbJSON:      `{"value":[{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-int","name":"lb-int","location":"eastus","sku":{"name":"Standard"},"properties":{"frontendIPConfigurations":[{"properties":{}}]}}]}`,
	}
	result, err := newParityScanner(t, fake).Scan(context.Background())
	require.NoError(t, err)
	require.Len(t, result.LoadBalancers, 1)
	assert.Equal(t, "internal", result.LoadBalancers[0].Scheme, "no public frontend => internal")
	assert.False(t, result.LoadBalancers[0].AccessLogsEnabled)
}
