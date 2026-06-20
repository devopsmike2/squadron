// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanner

import (
	"context"
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
