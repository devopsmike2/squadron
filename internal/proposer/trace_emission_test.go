// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// discoverySystemPromptForTest pulls the discovery proposer system
// prompt out via the ai package's test-only export so the proposer
// package's cross-cutting trace-emission tests can assert on it
// without re-declaring the const.
func discoverySystemPromptForTest() string {
	return ai.DiscoverySystemPromptForTest()
}

// fakeExclusionStore is a deterministic in-memory ApplicationStore slice
// for the trace-emission detection tests. Returns either the seeded
// rows or an error from ErrFromList.
type fakeExclusionStore struct {
	rows        []applicationstore.ExcludedRecommendation
	errFromList error
}

func (f *fakeExclusionStore) ListExcludedRecommendations(
	_ context.Context,
	_, _, _ string,
	_ int,
) ([]applicationstore.ExcludedRecommendation, error) {
	if f.errFromList != nil {
		return nil, f.errFromList
	}
	return f.rows, nil
}

// fixedNow returns a deterministic Now() the tests pin against. The
// staleness check uses now - 24h; tests choose timestamps relative to
// this anchor.
func fixedNow() time.Time {
	return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
}

// TestTraceEmissionDetection_EC2WithOTelTagAndStaleEmission_EmitsRecommendation
// — §10 acceptance test 1. EC2 inventory row with HasOTel=true and
// LastSeenAt 25h ago. Detection branch emits a trace-emission-aws-compute
// recommendation with the §4.1 Terraform pattern.
func TestTraceEmissionDetection_EC2WithOTelTagAndStaleEmission_EmitsRecommendation(t *testing.T) {
	now := fixedNow()
	lastSeen := now.Add(-25 * time.Hour)
	row := TraceEmissionInventoryRow{
		RecommendationID: "rec-aws-ec2-i-0abc",
		Provider:         "aws",
		Tier:             "compute",
		PrimitiveEnabled: true,
		LastSeenAt:       &lastSeen,
		ResourceID:       "i-0abc",
		ResourceTFName:   "web_01",
		Region:           "us-east-1",
	}
	scope := TraceEmissionScope{ConnectionID: "conn-1", ScopeID: "123456789012", Region: "us-east-1"}

	draft, err := CheckTraceEmissionGap(
		context.Background(), row, scope, &fakeExclusionStore{}, nil, now,
	)
	if err != nil {
		t.Fatalf("CheckTraceEmissionGap: %v", err)
	}
	if draft == nil {
		t.Fatal("expected a draft for stale EC2 row; got nil")
	}
	if draft.Kind != "trace-emission-aws-compute" {
		t.Errorf("Kind = %q, want trace-emission-aws-compute", draft.Kind)
	}
	if !strings.Contains(draft.Terraform, "aws_ssm_association") {
		t.Errorf("Terraform missing aws_ssm_association; got:\n%s", draft.Terraform)
	}
	if !strings.Contains(draft.Terraform, "AWS-ConfigureAWSPackage") {
		t.Errorf("Terraform missing AWS-ConfigureAWSPackage")
	}
	if !strings.Contains(draft.Reasoning, "Three failure modes are possible") {
		t.Errorf("Reasoning missing failure-mode template")
	}
	if !strings.Contains(draft.Reasoning, "i-0abc") {
		t.Errorf("Reasoning missing resource id; got: %s", draft.Reasoning)
	}
	if draft.ScopeID != "123456789012" {
		t.Errorf("ScopeID = %q", draft.ScopeID)
	}
}

// TestTraceEmissionDetection_EC2WithRecentEmission_NoRecommendation
// — §10 acceptance test 2. Same row as test 1 but LastSeenAt 1h ago.
// Detection branch returns nil (no recommendation).
func TestTraceEmissionDetection_EC2WithRecentEmission_NoRecommendation(t *testing.T) {
	now := fixedNow()
	recent := now.Add(-1 * time.Hour)
	row := TraceEmissionInventoryRow{
		RecommendationID: "rec-aws-ec2-i-0abc",
		Provider:         "aws",
		Tier:             "compute",
		PrimitiveEnabled: true,
		LastSeenAt:       &recent,
		ResourceID:       "i-0abc",
	}
	scope := TraceEmissionScope{ConnectionID: "conn-1", ScopeID: "123456789012", Region: "us-east-1"}
	draft, err := CheckTraceEmissionGap(context.Background(), row, scope, &fakeExclusionStore{}, nil, now)
	if err != nil {
		t.Fatalf("CheckTraceEmissionGap: %v", err)
	}
	if draft != nil {
		t.Errorf("expected no draft for fresh emission; got Kind=%q", draft.Kind)
	}
}

// TestTraceEmissionDetection_PrimitiveOff_NoRecommendation — §3 detection
// rule: rows without the primitive enabled don't fire trace-emission
// (they get handled by the existing per-tier primitive-enablement kinds).
func TestTraceEmissionDetection_PrimitiveOff_NoRecommendation(t *testing.T) {
	now := fixedNow()
	row := TraceEmissionInventoryRow{
		Provider:         "aws",
		Tier:             "compute",
		PrimitiveEnabled: false,
	}
	scope := TraceEmissionScope{ConnectionID: "conn-1", ScopeID: "123", Region: "us-east-1"}
	draft, _ := CheckTraceEmissionGap(context.Background(), row, scope, &fakeExclusionStore{}, nil, now)
	if draft != nil {
		t.Errorf("expected no draft when primitive is off; got Kind=%q", draft.Kind)
	}
}

// TestTraceEmissionDetection_GKEWithManagedPrometheusAndNoEmission_EmitsRecommendation
// — §10 acceptance test 3. GKE cluster with ManagedPrometheusEnabled
// (provider=gcp, tier=k8s, PrimitiveEnabled=true) + nil LastSeenAt
// (never observed) → trace-emission-gcp-k8s emitted.
func TestTraceEmissionDetection_GKEWithManagedPrometheusAndNoEmission_EmitsRecommendation(t *testing.T) {
	now := fixedNow()
	row := TraceEmissionInventoryRow{
		RecommendationID: "rec-gcp-k8s-prod",
		Provider:         "gcp",
		Tier:             "k8s",
		PrimitiveEnabled: true,
		LastSeenAt:       nil, // never observed
		ResourceID:       "projects/p/locations/us-central1/clusters/prod",
		Region:           "us-central1",
	}
	scope := TraceEmissionScope{ConnectionID: "conn-2", ScopeID: "my-project", Region: "us-central1"}
	draft, err := CheckTraceEmissionGap(context.Background(), row, scope, &fakeExclusionStore{}, nil, now)
	if err != nil {
		t.Fatalf("CheckTraceEmissionGap: %v", err)
	}
	if draft == nil {
		t.Fatal("expected a draft for GKE with no emission; got nil")
	}
	if draft.Kind != "trace-emission-gcp-k8s" {
		t.Errorf("Kind = %q, want trace-emission-gcp-k8s", draft.Kind)
	}
	if !strings.Contains(draft.Terraform, "google_gke_hub_feature") {
		t.Errorf("Terraform missing google_gke_hub_feature; got:\n%s", draft.Terraform)
	}
}

// TestTraceEmissionDetection_RowExcluded_NoRecommendation — §10 acceptance
// test 6. The operator clicked "Don't propose this again" on a previous
// trace-emission recommendation for this row. The exclusion store row
// suppresses the new draft.
func TestTraceEmissionDetection_RowExcluded_NoRecommendation(t *testing.T) {
	now := fixedNow()
	lastSeen := now.Add(-30 * time.Hour)
	row := TraceEmissionInventoryRow{
		RecommendationID: "rec-aws-ec2-i-0abc",
		Provider:         "aws",
		Tier:             "compute",
		PrimitiveEnabled: true,
		LastSeenAt:       &lastSeen,
		ResourceID:       "i-0abc",
	}
	scope := TraceEmissionScope{ConnectionID: "conn-1", ScopeID: "123456789012", Region: "us-east-1"}
	exclusions := &fakeExclusionStore{
		rows: []applicationstore.ExcludedRecommendation{
			{
				RecommendationID:   "rec-aws-ec2-i-0abc",
				ConnectionID:       "conn-1",
				AccountID:          "123456789012",
				Region:             "us-east-1",
				RecommendationKind: "trace-emission-aws-compute",
				ExcludedAt:         now.Add(-24 * time.Hour),
				ExcludedBy:         "alice",
			},
		},
	}
	draft, err := CheckTraceEmissionGap(context.Background(), row, scope, exclusions, nil, now)
	if err != nil {
		t.Fatalf("CheckTraceEmissionGap: %v", err)
	}
	if draft != nil {
		t.Errorf("expected exclusion to suppress draft; got Kind=%q", draft.Kind)
	}
}

// TestTraceEmissionDetection_KindOnlyExclusion_SuppressesAcrossScope —
// when the operator excludes a whole kind (RecommendationID="" in the
// projection), the detection branch suppresses ALL rows of that kind in
// the scope.
func TestTraceEmissionDetection_KindOnlyExclusion_SuppressesAcrossScope(t *testing.T) {
	now := fixedNow()
	lastSeen := now.Add(-30 * time.Hour)
	row := TraceEmissionInventoryRow{
		RecommendationID: "rec-aws-ec2-i-0abc",
		Provider:         "aws",
		Tier:             "compute",
		PrimitiveEnabled: true,
		LastSeenAt:       &lastSeen,
	}
	scope := TraceEmissionScope{ConnectionID: "conn-1", ScopeID: "123456789012", Region: "us-east-1"}
	exclusions := &fakeExclusionStore{
		rows: []applicationstore.ExcludedRecommendation{
			{RecommendationID: "", RecommendationKind: "trace-emission-aws-compute", ExcludedBy: "alice"},
		},
	}
	draft, _ := CheckTraceEmissionGap(context.Background(), row, scope, exclusions, nil, now)
	if draft != nil {
		t.Errorf("expected kind-level exclusion to suppress draft; got Kind=%q", draft.Kind)
	}
}

// TestTraceEmissionDetection_AllTwelveKinds_KindLookupCorrect — pin the
// 12 (provider, tier) → kind mappings the slice 2 design doc defines.
func TestTraceEmissionDetection_AllTwelveKinds_KindLookupCorrect(t *testing.T) {
	cases := []struct {
		provider, tier, want string
	}{
		{"aws", "compute", "trace-emission-aws-compute"},
		{"aws", "db", "trace-emission-aws-db"},
		{"aws", "k8s", "trace-emission-aws-k8s"},
		{"gcp", "compute", "trace-emission-gcp-compute"},
		{"gcp", "db", "trace-emission-gcp-db"},
		{"gcp", "k8s", "trace-emission-gcp-k8s"},
		{"azure", "compute", "trace-emission-azure-compute"},
		{"azure", "db", "trace-emission-azure-db"},
		{"azure", "k8s", "trace-emission-azure-k8s"},
		{"oci", "compute", "trace-emission-oci-compute"},
		{"oci", "db", "trace-emission-oci-db"},
		{"oci", "k8s", "trace-emission-oci-k8s"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"-"+tc.tier, func(t *testing.T) {
			if got := traceEmissionKindFor(tc.provider, tc.tier); got != tc.want {
				t.Errorf("traceEmissionKindFor(%q, %q) = %q, want %q",
					tc.provider, tc.tier, got, tc.want)
			}
		})
	}
}

// TestTraceEmissionDetection_UnrecognizedProvider_NoRecommendation —
// kindLookup returns "" for unrecognized pairs, so the detection
// branch returns nil.
func TestTraceEmissionDetection_UnrecognizedProvider_NoRecommendation(t *testing.T) {
	now := fixedNow()
	row := TraceEmissionInventoryRow{
		Provider:         "ibm",
		Tier:             "compute",
		PrimitiveEnabled: true,
		LastSeenAt:       nil,
	}
	scope := TraceEmissionScope{ConnectionID: "c", ScopeID: "x", Region: "us-east-1"}
	draft, _ := CheckTraceEmissionGap(context.Background(), row, scope, &fakeExclusionStore{}, nil, now)
	if draft != nil {
		t.Errorf("expected nil draft for unrecognized provider; got Kind=%q", draft.Kind)
	}
}

// TestTraceEmissionDetection_ExclusionStoreError_BubbledUp — a transient
// store failure on the exclusion lookup must NOT silently drop the
// recommendation. The detection branch returns the error so the caller
// can decide (log + skip OR log + emit).
func TestTraceEmissionDetection_ExclusionStoreError_BubbledUp(t *testing.T) {
	now := fixedNow()
	lastSeen := now.Add(-25 * time.Hour)
	row := TraceEmissionInventoryRow{
		RecommendationID: "rec-1",
		Provider:         "aws",
		Tier:             "compute",
		PrimitiveEnabled: true,
		LastSeenAt:       &lastSeen,
	}
	scope := TraceEmissionScope{ConnectionID: "c", ScopeID: "x", Region: "us-east-1"}
	exclusions := &fakeExclusionStore{errFromList: errors.New("db is down")}
	_, err := CheckTraceEmissionGap(context.Background(), row, scope, exclusions, nil, now)
	if err == nil {
		t.Fatal("expected error from CheckTraceEmissionGap")
	}
	if !strings.Contains(err.Error(), "db is down") {
		t.Errorf("error did not wrap the store error: %v", err)
	}
}

// TestTraceEmissionDetection_IacContentReader_AKSExtendsExisting — when
// the iacContentReader returns an HCL snippet containing an existing
// oms_agent block, the picker extends that block and the draft's
// Terraform field carries the oms_agent pattern.
func TestTraceEmissionDetection_IacContentReader_AKSExtendsExisting(t *testing.T) {
	now := fixedNow()
	row := TraceEmissionInventoryRow{
		RecommendationID: "rec-aks-prod",
		Provider:         "azure",
		Tier:             "k8s",
		PrimitiveEnabled: true,
		LastSeenAt:       nil,
		ResourceTFName:   "prod",
	}
	reader := func(_ TraceEmissionInventoryRow) string {
		return `resource "azurerm_kubernetes_cluster" "prod" {
  oms_agent {
    log_analytics_workspace_id = azurerm_log_analytics_workspace.aks.id
  }
}
`
	}
	scope := TraceEmissionScope{ConnectionID: "c", ScopeID: "sub-1", Region: "eastus"}
	draft, err := CheckTraceEmissionGap(context.Background(), row, scope, &fakeExclusionStore{}, reader, now)
	if err != nil {
		t.Fatalf("CheckTraceEmissionGap: %v", err)
	}
	if draft == nil {
		t.Fatal("expected draft")
	}
	if !strings.Contains(draft.Terraform, "oms_agent") {
		t.Errorf("expected oms_agent in Terraform; got:\n%s", draft.Terraform)
	}
	if !strings.Contains(draft.Reasoning, "extending existing oms_agent") {
		t.Errorf("expected reasoning to mention extending oms_agent; got:\n%s", draft.Reasoning)
	}
}

// TestTraceEmissionDetection_Batch_AggregatesAndSkipsCleanly — the
// CheckTraceEmissionGapBatch helper accumulates non-nil drafts and
// keeps going when one row is below threshold.
func TestTraceEmissionDetection_Batch_AggregatesAndSkipsCleanly(t *testing.T) {
	now := fixedNow()
	stale := now.Add(-25 * time.Hour)
	fresh := now.Add(-1 * time.Hour)
	rows := []TraceEmissionInventoryRow{
		{RecommendationID: "r1", Provider: "aws", Tier: "compute", PrimitiveEnabled: true, LastSeenAt: &stale, ResourceID: "i-1"},
		{RecommendationID: "r2", Provider: "aws", Tier: "compute", PrimitiveEnabled: true, LastSeenAt: &fresh, ResourceID: "i-2"}, // fresh → skip
		{RecommendationID: "r3", Provider: "gcp", Tier: "k8s", PrimitiveEnabled: true, LastSeenAt: nil, ResourceID: "c-1"},
		{RecommendationID: "r4", Provider: "ibm", Tier: "compute", PrimitiveEnabled: true, LastSeenAt: nil, ResourceID: "ibm-1"}, // unknown → skip
	}
	scope := TraceEmissionScope{ConnectionID: "c", ScopeID: "x", Region: "us-east-1"}
	drafts, errs := CheckTraceEmissionGapBatch(context.Background(), rows, scope, &fakeExclusionStore{}, nil, now)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(drafts) != 2 {
		t.Fatalf("expected 2 drafts (stale + nil); got %d", len(drafts))
	}
	if drafts[0].Kind != "trace-emission-aws-compute" {
		t.Errorf("drafts[0].Kind = %q", drafts[0].Kind)
	}
	if drafts[1].Kind != "trace-emission-gcp-k8s" {
		t.Errorf("drafts[1].Kind = %q", drafts[1].Kind)
	}
}

// TestDiscoveryProposer_ColdStart_PromptUnchanged_PostTraceSlice2 —
// trace integration slice 2 chunk 1 (v0.89.80, #711 Stream 109)
// cold-start parity invariant: across all four providers, the
// compute-only user message produced by buildDiscoveryUserMessage
// stays byte-identical when the scan context carries no inventory
// rows that trigger trace-emission kinds. The 12 new kind strings live
// ONLY in the system prompt; the user-message renderer is unchanged.
//
// Per the §10 acceptance test 11 invariant ("Cold-start parity
// preserved — all 4 providers cold-start prompts byte-identical to
// v0.89.78") this test pins the user-message body for each provider.
func TestDiscoveryProposer_ColdStart_PromptUnchanged_PostTraceSlice2(t *testing.T) {
	cases := []struct {
		name string
		in   ai.DiscoveryScanContext
		must []string
		// none of the 12 trace-emission kind strings may appear in the
		// user message for a cold-start scan.
	}{
		{
			name: "aws",
			in: ai.DiscoveryScanContext{
				ScanID: "scan-aws-cold", AccountID: "123456789012", Regions: []string{"us-east-1"},
			},
			must: []string{
				"AWS discovery scan completed on a Squadron-connected account.",
				"account_id: 123456789012",
				"group_id on every step MUST equal the account_id above",
			},
		},
		{
			name: "gcp",
			in: ai.DiscoveryScanContext{
				ScanID: "scan-gcp-cold", Provider: "gcp", ProjectID: "my-project", Regions: []string{"us-central1"},
			},
			must: []string{
				"GCP discovery scan completed on a Squadron-connected project.",
				"project_id: my-project",
				"group_id on every step MUST equal the project_id above",
			},
		},
		{
			name: "azure",
			in: ai.DiscoveryScanContext{
				ScanID: "scan-azure-cold", Provider: "azure",
				TenantID: "11111111-2222-3333-4444-555555555555", SubscriptionID: "aaaa-bbbb",
				Regions: []string{"eastus"},
			},
			must: []string{
				"Azure discovery scan completed on a Squadron-connected subscription.",
				"subscription_id: aaaa-bbbb",
				"group_id on every step MUST equal the subscription_id above",
			},
		},
		{
			name: "oci",
			in: ai.DiscoveryScanContext{
				ScanID: "scan-oci-cold", Provider: "oci", TenancyOCID: "ocid1.tenancy.oc1..aaaa",
				Regions: []string{"us-phoenix-1"},
			},
			must: []string{
				"OCI discovery scan completed on a Squadron-connected tenancy.",
				"tenancy_ocid: ocid1.tenancy.oc1..aaaa",
				"group_id on every step MUST equal the tenancy_ocid above",
			},
		},
	}
	traceKinds := []string{
		"trace-emission-aws-compute", "trace-emission-aws-db", "trace-emission-aws-k8s",
		"trace-emission-gcp-compute", "trace-emission-gcp-db", "trace-emission-gcp-k8s",
		"trace-emission-azure-compute", "trace-emission-azure-db", "trace-emission-azure-k8s",
		"trace-emission-oci-compute", "trace-emission-oci-db", "trace-emission-oci-k8s",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ai.BuildDiscoveryUserMessageForTest(tc.in)
			for _, want := range tc.must {
				if !strings.Contains(msg, want) {
					t.Errorf("user message missing %q for provider %s", want, tc.name)
				}
			}
			for _, kind := range traceKinds {
				if strings.Contains(msg, kind) {
					t.Errorf("cold-start user message must NOT contain trace-emission kind %q (provider=%s)", kind, tc.name)
				}
			}
		})
	}
}

// TestDiscoveryProposer_TraceEmissionKindsInSystemPrompt — cold-start
// parity invariant: the 12 new kind strings appear in the system prompt
// (NOT the user message). Pinned here in the proposer package because
// the system prompt is consumed via the ai package's exported const.
func TestDiscoveryProposer_TraceEmissionKindsInSystemPrompt(t *testing.T) {
	want := []string{
		"trace-emission-aws-compute",
		"trace-emission-aws-db",
		"trace-emission-aws-k8s",
		"trace-emission-gcp-compute",
		"trace-emission-gcp-db",
		"trace-emission-gcp-k8s",
		"trace-emission-azure-compute",
		"trace-emission-azure-db",
		"trace-emission-azure-k8s",
		"trace-emission-oci-compute",
		"trace-emission-oci-db",
		"trace-emission-oci-k8s",
	}
	sp := discoverySystemPromptForTest()
	for _, w := range want {
		if !strings.Contains(sp, w) {
			t.Errorf("system prompt missing %q", w)
		}
	}
	// Also verify the three-failure-mode reasoning template is in the
	// system prompt so the model has the language pinned regardless of
	// the per-row reasoning field.
	if !strings.Contains(sp, "Three failure modes are possible") {
		t.Errorf("system prompt missing the three-failure-mode template")
	}
}
