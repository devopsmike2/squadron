// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScanAll_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/discovery/aws/scan-all", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"scan_all_id": "sa-001",
			"total_accounts": 2,
			"succeeded_accounts": [
				{"account_id":"111111111111","scan_id":"sc-a","resource_count":10,"instrumented_count":4,"uninstrumented_count":6},
				{"account_id":"222222222222","scan_id":"sc-b","resource_count":20,"instrumented_count":15,"uninstrumented_count":5}
			],
			"failed_accounts": [],
			"total_resources": 30,
			"total_instrumented": 19,
			"total_uninstrumented": 11,
			"partial": false,
			"concurrency": 3
		}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "sa-001")
	assert.Contains(t, out, "111111111111")
	assert.Contains(t, out, "222222222222")
}

func TestScanAll_AggregateRendering(t *testing.T) {
	// Two succeeded accounts with known counts; assert the aggregate
	// rolls up correctly in the human output. The server reports
	// these totals — we just verify the renderer surfaces them.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"scan_all_id": "sa-002",
			"total_accounts": 2,
			"succeeded_accounts": [
				{"account_id":"a1","scan_id":"sc-a","resource_count":3,"instrumented_count":1,"uninstrumented_count":2},
				{"account_id":"a2","scan_id":"sc-b","resource_count":7,"instrumented_count":4,"uninstrumented_count":3}
			],
			"failed_accounts": [],
			"total_resources": 10,
			"total_instrumented": 5,
			"total_uninstrumented": 5,
			"partial": false,
			"concurrency": 3
		}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	out := buf.String()
	// Per-account rows are present.
	assert.Contains(t, out, "a1")
	assert.Contains(t, out, "a2")
	// Aggregate totals match per-account sums (3+7=10 etc).
	assert.Contains(t, out, "total_resources:      10")
	assert.Contains(t, out, "total_instrumented:   5")
	assert.Contains(t, out, "total_uninstrumented: 5")
}

func TestScanAll_PartialFlagSurfacedInHumanOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"scan_all_id": "sa-003",
			"total_accounts": 2,
			"succeeded_accounts": [
				{"account_id":"a1","scan_id":"sc-a","resource_count":5,"instrumented_count":2,"uninstrumented_count":3}
			],
			"failed_accounts": [
				{"account_id":"a2","error_code":"AuthFailed","humanized_message":"role assumption denied"}
			],
			"total_resources": 5,
			"total_instrumented": 2,
			"total_uninstrumented": 3,
			"partial": true,
			"concurrency": 3
		}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute(),
		"partial true with at least one success must NOT cause a non-zero exit")
	out := buf.String()
	assert.Contains(t, out, "partial:        yes")
	assert.Contains(t, out, "AuthFailed")
	assert.Contains(t, out, "role assumption denied")
}

func TestScanAll_EveryAccountFailedExitsNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"scan_all_id": "sa-004",
			"total_accounts": 1,
			"succeeded_accounts": [],
			"failed_accounts": [
				{"account_id":"a1","error_code":"AuthFailed","humanized_message":"role assumption denied"}
			],
			"total_resources": 0,
			"total_instrumented": 0,
			"total_uninstrumented": 0,
			"partial": true,
			"concurrency": 3
		}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err, "every-account-failed must surface as a non-zero exit")
	assert.Contains(t, err.Error(), "every account failed")
}

func TestScanAll_OutputFormatJSON(t *testing.T) {
	body := `{
		"scan_all_id": "sa-005",
		"total_accounts": 1,
		"succeeded_accounts": [
			{"account_id":"a1","scan_id":"sc-a","resource_count":1,"instrumented_count":1,"uninstrumented_count":0}
		],
		"failed_accounts": [],
		"total_resources": 1,
		"total_instrumented": 1,
		"total_uninstrumented": 0,
		"partial": false,
		"concurrency": 3
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withServer(t, srv)
	prevOutput := flags.Output
	flags.Output = "json"
	t.Cleanup(func() { flags.Output = prevOutput })

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, "sa-005", got["scan_all_id"])
}

func TestScanAll_4xxRendersHumanizedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"NoConnections","message":"no AWS connections registered","suggested_step":"squadronctl iac connect (then connect an AWS account first)","doc_link":"https://docs/aws"}}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no AWS connections registered")
	assert.Contains(t, err.Error(), "connect an AWS account")
}

func TestScanAll_5xxRendersGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<html>panic</html>`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "internal server error")
}

func TestScanAll_RegionsAndConcurrencyFlagsForwarded(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scan_all_id":"sa-006","total_accounts":0,"succeeded_accounts":[],"failed_accounts":[],"total_resources":0,"total_instrumented":0,"total_uninstrumented":0,"partial":false,"concurrency":2}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newDiscoveryAWSScanAllCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--regions", "us-east-1,us-west-2,", "--concurrency", "2"})
	cmd.SetContext(context.Background())
	// total_accounts is 0, no non-zero-exit hazard.
	_ = cmd.Execute()
	assert.Contains(t, gotQuery, "regions=us-east-1%2Cus-west-2")
	assert.Contains(t, gotQuery, "concurrency=2")
}

func TestNormalizeRegionsCSV(t *testing.T) {
	assert.Equal(t, "", normalizeRegionsCSV(""))
	assert.Equal(t, "", normalizeRegionsCSV(", ,"))
	assert.Equal(t, "us-east-1", normalizeRegionsCSV("us-east-1"))
	assert.Equal(t, "us-east-1,us-west-2", normalizeRegionsCSV(" us-east-1 , us-west-2 ,"))
}
