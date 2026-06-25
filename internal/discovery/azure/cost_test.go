// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Cost-correlation substrate slice 6 chunk 4 (v0.89.186, #828 Stream
// 225) — acceptance tests for the Azure Cost Management QueryCost body
// + Service Bus cost enrichment.

// costFakeServer is an httptest stub for the Cost Management /query
// endpoint. Captures the last request body so tests can assert on the
// filter, and returns the canned columns/rows.
type costFakeServer struct {
	srv          *httptest.Server
	lastBody     []byte
	status       int
	responseBody interface{}
}

func newCostFakeServer(t *testing.T, columns []map[string]string, rows [][]interface{}) *costFakeServer {
	t.Helper()
	cf := &costFakeServer{status: http.StatusOK}
	cf.responseBody = map[string]interface{}{
		"properties": map[string]interface{}{
			"columns": columns,
			"rows":    rows,
		},
	}
	cf.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cf.lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cf.status)
		_ = json.NewEncoder(w).Encode(cf.responseBody)
	}))
	t.Cleanup(cf.srv.Close)
	return cf
}

func costScanner(t *testing.T, cf *costFakeServer, gov *scanner.CostBudgetGovernor) *Scanner {
	t.Helper()
	s := &Scanner{
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		httpClient:     cf.srv.Client(),
		armEndpoint:    cf.srv.URL,
		accessToken:    "fake-token",
	}
	return s.WithCostBudgetGovernor(gov)
}

var costColumns = []map[string]string{
	{"name": "Cost", "type": "Number"},
	{"name": "Currency", "type": "String"},
}

// --- per-call cost is free --------------------------------------

func TestAzureCostPerCallIsFree(t *testing.T) {
	assert.Equal(t, scanner.MicroUSD(0), AzureCostManagementPerCallMicroUSD)
}

// --- gate: token + governor required ----------------------------

func TestAzureQueryCost_NilTokenReturnsNotImplemented(t *testing.T) {
	cf := newCostFakeServer(t, costColumns, [][]interface{}{{12.5, "USD"}})
	s := costScanner(t, cf, scanner.NewCostBudgetGovernor(0, 0))
	s.accessToken = "" // clear
	_, err := s.QueryCost(context.Background(), "ns", AzureServiceBusCostServiceName, 30*24*time.Hour)
	assert.ErrorIs(t, err, scanner.ErrCostNotImplemented)
}

func TestAzureQueryCost_NilGovernorReturnsNotImplemented(t *testing.T) {
	cf := newCostFakeServer(t, costColumns, [][]interface{}{{12.5, "USD"}})
	s := &Scanner{
		SubscriptionID: "sub", httpClient: cf.srv.Client(),
		armEndpoint: cf.srv.URL, accessToken: "fake-token",
	} // no governor
	_, err := s.QueryCost(context.Background(), "ns", AzureServiceBusCostServiceName, 30*24*time.Hour)
	assert.ErrorIs(t, err, scanner.ErrCostNotImplemented)
}

// --- parse columns/rows → micro-USD + ServiceName filter --------

func TestAzureQueryCost_ParsesCostAndCurrency(t *testing.T) {
	cf := newCostFakeServer(t, costColumns, [][]interface{}{{123.456789, "USD"}})
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := costScanner(t, cf, gov)

	res, err := s.QueryCost(context.Background(), "ns", AzureServiceBusCostServiceName, 30*24*time.Hour)
	assert.NoError(t, err)
	assert.True(t, res.Covered)
	assert.Equal(t, scanner.MicroUSD(123_456_789), res.AmountMicroUSD, "123.456789 → micro-USD (truncated at 6dp)")
	assert.Equal(t, "USD", res.Currency)
	assert.Equal(t, scanner.CostGranularityMonthly, res.Granularity)

	// Request body filters on the ServiceName dimension.
	var body map[string]interface{}
	assert.NoError(t, json.Unmarshal(cf.lastBody, &body))
	ds := body["dataset"].(map[string]interface{})
	flt := ds["filter"].(map[string]interface{})["dimensions"].(map[string]interface{})
	assert.Equal(t, "ServiceName", flt["name"])
	assert.Equal(t, []interface{}{AzureServiceBusCostServiceName}, flt["values"])
}

// --- column-order independence (Currency first) -----------------

func TestAzureQueryCost_FindsColumnsByName(t *testing.T) {
	// Currency column BEFORE Cost — the substrate must find by name.
	cols := []map[string]string{
		{"name": "Currency", "type": "String"},
		{"name": "Cost", "type": "Number"},
	}
	cf := newCostFakeServer(t, cols, [][]interface{}{{"EUR", 50.0}})
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := costScanner(t, cf, gov)

	res, err := s.QueryCost(context.Background(), "ns", AzureServiceBusCostServiceName, 30*24*time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, scanner.MicroUSD(50_000_000), res.AmountMicroUSD)
	assert.Equal(t, "EUR", res.Currency, "non-USD source currency preserved honestly")
}

// --- empty rows → not covered -----------------------------------

func TestAzureQueryCost_EmptyRowsNotCovered(t *testing.T) {
	cf := newCostFakeServer(t, costColumns, [][]interface{}{})
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := costScanner(t, cf, gov)

	res, err := s.QueryCost(context.Background(), "ns", AzureServiceBusCostServiceName, 30*24*time.Hour)
	assert.NoError(t, err)
	assert.False(t, res.Covered)
	assert.Equal(t, scanner.MicroUSD(0), res.AmountMicroUSD)
}

// --- enrichment attaches to SB namespaces / no-op unwired -------

func sbNsSnap(id string) scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		ResourceARN: id,
		Detail:      map[string]any{},
	}
}

func TestEnrichServiceBusCost_AttachesToNamespaces(t *testing.T) {
	cf := newCostFakeServer(t, costColumns, [][]interface{}{{200.0, "USD"}})
	gov := scanner.NewCostBudgetGovernor(scanner.MicroUSDPerDollar, time.Hour)
	s := costScanner(t, cf, gov)

	snaps := []scanner.EventSourceInstanceSnapshot{sbNsSnap("/subscriptions/x/.../namespaces/a")}
	s.enrichServiceBusCost(context.Background(), snaps, "fake-token")

	assert.Equal(t, scanner.MicroUSD(200_000_000), snaps[0].Detail["service_cost_monthly_micro_usd"])
	assert.Equal(t, "USD", snaps[0].Detail["service_cost_currency"])
	assert.Equal(t, "service", snaps[0].Detail["service_cost_scope"])
}

func TestEnrichServiceBusCost_NoOpWhenGovernorMissing(t *testing.T) {
	cf := newCostFakeServer(t, costColumns, [][]interface{}{{200.0, "USD"}})
	s := &Scanner{
		SubscriptionID: "sub", httpClient: cf.srv.Client(),
		armEndpoint: cf.srv.URL, accessToken: "fake-token",
	} // no governor
	snaps := []scanner.EventSourceInstanceSnapshot{sbNsSnap("/subscriptions/x/.../namespaces/a")}
	s.enrichServiceBusCost(context.Background(), snaps, "fake-token")
	_, has := snaps[0].Detail["service_cost_monthly_micro_usd"]
	assert.False(t, has, "no governor → cost correlation off (no keys)")
}
