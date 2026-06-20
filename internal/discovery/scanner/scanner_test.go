// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// TestScannerInterfaceCompiles confirms the Scanner contract is
// well-formed: a no-op implementation satisfies the interface and the
// public types compose into a complete Result / ValidationResult. The
// real per-field semantics are covered by each provider's scanner
// tests (see internal/discovery/aws); this file's only job is to
// catch breakage in the interface or the struct shapes.
func TestScannerInterfaceCompiles(t *testing.T) {
	var _ Scanner = (*noopScanner)(nil)

	// Result composes the category-typed snapshots and the
	// instrumented-count denormalization.
	r := &Result{
		ScanID:          "scan-1",
		ScanStartedAt:   time.Unix(1700000000, 0).UTC(),
		ScanCompletedAt: time.Unix(1700000060, 0).UTC(),
		Provider:        credstore.ProviderAWS,
		AccountID:       "123456789012",
		Regions:         []string{"us-east-1"},
		Compute: []ComputeInstanceSnapshot{{
			ResourceID:   "i-deadbeef",
			InstanceType: "m5.large",
			Tags:         map[string]string{"otel-agent": "true"},
			HasOTel:      true,
			OSFamily:     "linux",
			Region:       "us-east-1",
		}},
		Functions: []FunctionRuntimeSnapshot{{
			ResourceID:   "arn:aws:lambda:us-east-1:123456789012:function:fn",
			Name:         "fn",
			Runtime:      "nodejs20",
			HasOTelLayer: true,
			Region:       "us-east-1",
		}},
		Databases: []DatabaseInstanceSnapshot{{
			ResourceID:                 "arn:aws:rds:us-east-1:123456789012:db:db-prod-1",
			Engine:                     "postgres",
			EngineVersion:              "15.4",
			InstanceClass:              "db.r6g.large",
			PerformanceInsightsEnabled: true,
			EnhancedMonitoringEnabled:  true,
			Region:                     "us-east-1",
			Tags:                       map[string]string{"Env": "prod"},
		}},
		InstrumentedCount:   3,
		UninstrumentedCount: 0,
		Partial:             false,
		PartialReason:       "",
	}
	if r.Provider != credstore.ProviderAWS {
		t.Fatalf("Result.Provider round-trip lost: got %q", r.Provider)
	}
	if len(r.Compute) != 1 || !r.Compute[0].HasOTel {
		t.Fatalf("Compute snapshot did not preserve HasOTel")
	}
	if len(r.Functions) != 1 || !r.Functions[0].HasOTelLayer {
		t.Fatalf("Function snapshot did not preserve HasOTelLayer")
	}
	if len(r.Databases) != 1 {
		t.Fatalf("Database snapshot did not round-trip: %+v", r.Databases)
	}
	db := r.Databases[0]
	if db.Engine != "postgres" || db.EngineVersion != "15.4" {
		t.Fatalf("Database engine/version round-trip lost: %+v", db)
	}
	if !db.PerformanceInsightsEnabled || !db.EnhancedMonitoringEnabled {
		t.Fatalf("Database PI/EM flags lost: %+v", db)
	}

	// ValidationResult composes the humanized error + per-service
	// preflight rows.
	vr := &ValidationResult{
		AssumeRoleOK: false,
		AssumeRoleErr: &HumanizedError{
			Code:          "AccessDenied",
			Message:       "the role's trust policy doesn't authorize Squadron",
			SuggestedStep: "trust-policy",
			DocLink:       "",
		},
		Preflight: []PreflightCheck{{
			Service:     "ec2",
			OK:          true,
			SampleCount: 3,
			Err:         nil,
		}},
	}
	if vr.AssumeRoleErr == nil || vr.AssumeRoleErr.SuggestedStep != "trust-policy" {
		t.Fatalf("HumanizedError SuggestedStep lost: %+v", vr.AssumeRoleErr)
	}
	if len(vr.Preflight) != 1 || vr.Preflight[0].Service != "ec2" {
		t.Fatalf("Preflight row lost service tag: %+v", vr.Preflight)
	}
}

// TestResult_PartialFieldsJSONRoundTrip pins the wire shape for the
// two partial-scan fields the v0.87.3 audit-shape hotfix surfaces in
// the discovery.aws.scan_completed audit payload (and which existing
// HTTP consumers already see via the json tags on Result):
//   - partial_reason (omitempty, human-readable)
//   - failed_services (omitempty, structured list of service IDs)
//
// Audit consumers (SIEM forwarders, Timeline UI, squadronctl, the
// proposer's future scan-history learning loop) pattern-match against
// failed_services rather than parsing partial_reason; a JSON-shape
// regression here would silently break them.
func TestResult_PartialFieldsJSONRoundTrip(t *testing.T) {
	original := &Result{
		ScanID:         "scan-partial-1",
		Provider:       credstore.ProviderAWS,
		AccountID:      "123456789012",
		Regions:        []string{"us-east-1"},
		Partial:        true,
		PartialReason:  "rds scan failed in us-east-1: AccessDenied",
		FailedServices: []string{"rds"},
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Spot-check the snake_case keys land in the byte stream. The
	// audit payload, the HTTP response, and any future Result
	// serializer all share this shape.
	s := string(raw)
	for _, want := range []string{
		`"partial":true`,
		`"partial_reason":"rds scan failed in us-east-1: AccessDenied"`,
		`"failed_services":["rds"]`,
	} {
		if !contains(s, want) {
			t.Errorf("marshal output missing %q; got: %s", want, s)
		}
	}

	var round Result
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !round.Partial {
		t.Errorf("Partial round-trip lost: %v", round.Partial)
	}
	if round.PartialReason != original.PartialReason {
		t.Errorf("PartialReason round-trip = %q, want %q", round.PartialReason, original.PartialReason)
	}
	if len(round.FailedServices) != 1 || round.FailedServices[0] != "rds" {
		t.Errorf("FailedServices round-trip = %v, want [\"rds\"]", round.FailedServices)
	}

	// Empty FailedServices stays out of the wire shape entirely
	// (omitempty). Operators filtering on partial:false should not
	// see a stray failed_services:[] key.
	clean := &Result{ScanID: "scan-ok", Partial: false}
	cleanRaw, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("marshal clean: %v", err)
	}
	if contains(string(cleanRaw), "failed_services") {
		t.Errorf("clean Result should omit failed_services; got: %s", string(cleanRaw))
	}
	if contains(string(cleanRaw), "partial_reason") {
		t.Errorf("clean Result should omit partial_reason; got: %s", string(cleanRaw))
	}
}

// contains is a tiny helper so this package-level test stays
// dependency-free — strings is otherwise unused in scanner_test.go.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// noopScanner satisfies the Scanner interface without doing anything
// real — it exists so the compile-time conformance check above has a
// concrete type to bind.
type noopScanner struct{}

func (*noopScanner) Provider() credstore.Provider { return credstore.ProviderAWS }

func (*noopScanner) Scan(_ context.Context, _ *credstore.CloudConnection, _ []string) (*Result, error) {
	return &Result{}, nil
}

func (*noopScanner) Validate(_ context.Context, _ *credstore.CloudConnection) (*ValidationResult, error) {
	return &ValidationResult{}, nil
}
