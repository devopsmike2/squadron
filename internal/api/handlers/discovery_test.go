// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// mockValidator records the connection it was handed and returns a
// pre-canned ValidationResult. Lets the handler tests verify that the
// request body was correctly transformed into a CloudConnection
// without ever touching the AWS SDK.
type mockValidator struct {
	called   bool
	gotCreds credstore.AWSCredentials
	gotConn  *credstore.CloudConnection
	result   *scanner.ValidationResult
	err      error
}

func (m *mockValidator) Validate(_ context.Context, conn *credstore.CloudConnection) (*scanner.ValidationResult, error) {
	m.called = true
	m.gotConn = conn
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &scanner.ValidationResult{AssumeRoleOK: true}, nil
}

// newTestHandlers builds DiscoveryHandlers wired against the supplied
// mock validator. The credstore is nil (the validate endpoint should
// never need it); the logger is a no-op.
func newTestHandlers(_ *testing.T, mv *mockValidator) *DiscoveryHandlers {
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	h.WithAWSValidatorFactory(func(creds credstore.AWSCredentials, _ string) DiscoveryValidator {
		mv.gotCreds = creds
		return mv
	})
	return h
}

// doRequest is the shared POST harness for these tests. Returns the
// recorder so each test inspects status + body.
func doRequest(h *DiscoveryHandlers, body string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/api/v1/discovery/aws/validate", h.HandleAWSValidate)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/discovery/aws/validate", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleAWSValidate_BadRequest(t *testing.T) {
	h := newTestHandlers(t, &mockValidator{})
	// Completely malformed JSON.
	w := doRequest(h, `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "could not be parsed") {
		t.Errorf("body should explain the parse failure: %s", w.Body.String())
	}
}

func TestHandleAWSValidate_MissingRoleARN(t *testing.T) {
	mv := &mockValidator{}
	h := newTestHandlers(t, mv)
	body := `{"external_id":"abc","regions":["us-east-1"]}`
	w := doRequest(h, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if mv.called {
		t.Fatalf("validator should not have been called when role_arn is missing")
	}
	if !strings.Contains(w.Body.String(), "Role ARN is required") {
		t.Errorf("missing-role-ARN message not surfaced: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"suggested_step":"role-arn"`) && !strings.Contains(w.Body.String(), `"SuggestedStep":"role-arn"`) {
		// HumanizedError has no json tags, so fields land
		// capitalised. The wizard reads both cases; the test
		// accepts either to avoid being a JSON-shape test.
		t.Errorf("suggested_step pointer not surfaced: %s", w.Body.String())
	}
}

func TestHandleAWSValidate_MissingExternalID(t *testing.T) {
	mv := &mockValidator{}
	h := newTestHandlers(t, mv)
	body := `{"role_arn":"arn:aws:iam::123:role/x","regions":["us-east-1"]}`
	w := doRequest(h, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "External ID is required") {
		t.Errorf("missing-external-id message not surfaced: %s", w.Body.String())
	}
}

func TestHandleAWSValidate_ScannerCalled(t *testing.T) {
	mv := &mockValidator{
		result: &scanner.ValidationResult{
			AssumeRoleOK: true,
			Preflight: []scanner.PreflightCheck{
				{Service: "ec2", OK: true, SampleCount: 3},
				{Service: "lambda", OK: true, SampleCount: 1},
			},
		},
	}
	h := newTestHandlers(t, mv)
	body := `{"role_arn":"arn:aws:iam::123456789012:role/SquadronDiscovery","external_id":"xid","regions":["us-east-1"],"account_id":"123456789012"}`
	w := doRequest(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !mv.called {
		t.Fatalf("validator was not called")
	}
	if mv.gotCreds.RoleARN != "arn:aws:iam::123456789012:role/SquadronDiscovery" {
		t.Errorf("RoleARN not forwarded: %q", mv.gotCreds.RoleARN)
	}
	if mv.gotCreds.ExternalID != "xid" {
		t.Errorf("ExternalID not forwarded: %q", mv.gotCreds.ExternalID)
	}
	if mv.gotConn == nil {
		t.Fatalf("transient connection not forwarded")
	}
	if mv.gotConn.Provider != credstore.ProviderAWS {
		t.Errorf("Provider on conn = %q, want %q", mv.gotConn.Provider, credstore.ProviderAWS)
	}
	if mv.gotConn.AccountID != "123456789012" {
		t.Errorf("AccountID on conn = %q", mv.gotConn.AccountID)
	}
	if len(mv.gotConn.Regions) != 1 || mv.gotConn.Regions[0] != "us-east-1" {
		t.Errorf("Regions on conn = %+v, want [us-east-1]", mv.gotConn.Regions)
	}
	// Response shape sanity — assume_role_ok must round-trip and
	// the preflight rows must be present.
	var resp struct {
		AssumeRoleOK bool `json:"assume_role_ok"`
		Preflight    []struct {
			Service     string `json:"service"`
			OK          bool   `json:"ok"`
			SampleCount int    `json:"sample_count"`
		} `json:"preflight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body did not decode: %v body=%s", err, w.Body.String())
	}
	if !resp.AssumeRoleOK {
		t.Errorf("assume_role_ok = false, want true")
	}
	if len(resp.Preflight) != 2 {
		t.Errorf("preflight rows = %d, want 2", len(resp.Preflight))
	}
}

func TestHandleAWSValidate_ZeroPreflightOnAssumeFailure(t *testing.T) {
	// When assume-role fails, the handler should still 200 — the
	// validation result's typed AssumeRoleErr is the wizard's signal
	// to highlight the failing step, not an HTTP error code.
	mv := &mockValidator{
		result: &scanner.ValidationResult{
			AssumeRoleOK: false,
			AssumeRoleErr: &scanner.HumanizedError{
				Code:          "AccessDenied",
				Message:       "trust policy",
				SuggestedStep: "trust-policy",
			},
		},
	}
	h := newTestHandlers(t, mv)
	body := `{"role_arn":"arn:aws:iam::1:role/x","external_id":"y","regions":["us-east-1"]}`
	w := doRequest(h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on assume-role failure; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("assume-role err not surfaced: %s", w.Body.String())
	}
}
