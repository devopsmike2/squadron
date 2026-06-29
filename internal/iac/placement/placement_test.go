// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package placement_test

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/iac"
	"github.com/devopsmike2/squadron/internal/iac/hclsummary"
	"github.com/devopsmike2/squadron/internal/iac/placement"
)

// TestEveryDispositionKindIsMapped is the drift guard: every kind that
// can open a PR (has a disposition) must have a resource-type mapping,
// or Suggest would silently fall back to the generic new-file path.
func TestEveryDispositionKindIsMapped(t *testing.T) {
	for kind := range iac.KindDispositions {
		if got := placement.KindToResourceTypes(kind); len(got) == 0 {
			t.Errorf("kind %q has a disposition but no KindToResourceTypes mapping", kind)
		}
	}
}

func TestKindToResourceTypes_Spot(t *testing.T) {
	cases := map[string]string{
		"gcs-logging-enable":        "google_storage_bucket",
		"azlb-diag-enable":          "azurerm_lb",
		"ocibucket-logging-enable":  "oci_objectstorage_bucket",
		"s3-access-logging":         "aws_s3_bucket",
		"sqs-redrive-policy-enable": "aws_sqs_queue",
	}
	for kind, wantPrimary := range cases {
		got := placement.KindToResourceTypes(kind)
		if len(got) == 0 || got[0] != wantPrimary {
			t.Errorf("%s: primary type = %v, want %s", kind, got, wantPrimary)
		}
	}
	if placement.KindToResourceTypes("nonexistent-kind") != nil {
		t.Error("unknown kind should map to nil")
	}
}

func TestSuggest_DeclaresTypeBeatsFilename(t *testing.T) {
	files := []hclsummary.FileSummary{
		{Path: "modules/data/storage.tf", Parsed: true}, // filename hint only
		{Path: "infra/buckets_real.tf", Parsed: true, Resources: []string{"google_storage_bucket.assets"}},
	}
	got := placement.Suggest("gcs-logging-enable", files)
	if len(got) == 0 {
		t.Fatal("expected suggestions")
	}
	if got[0].Path != "infra/buckets_real.tf" {
		t.Errorf("top suggestion = %q (%s), want the file that declares the bucket", got[0].Path, got[0].Reason)
	}
	if got[0].Score <= got[1].Score {
		t.Errorf("declaring file score %d should beat filename-hint score %d", got[0].Score, got[1].Score)
	}
}

func TestSuggest_FilenameHintWhenNoDeclaration(t *testing.T) {
	files := []hclsummary.FileSummary{
		{Path: "network.tf", Parsed: true, Resources: []string{"aws_vpc.main"}},
		{Path: "storage.tf", Parsed: true, Resources: []string{"random_id.x"}},
	}
	got := placement.Suggest("gcs-logging-enable", files)
	if got[0].Path != "storage.tf" {
		t.Errorf("top = %q, want storage.tf via filename hint", got[0].Path)
	}
}

func TestSuggest_ConventionalFallback(t *testing.T) {
	files := []hclsummary.FileSummary{
		{Path: "main.tf", Parsed: true, Resources: []string{"aws_vpc.main"}},
		{Path: "outputs.tf", Parsed: true},
	}
	got := placement.Suggest("gce-otel-label", files)
	if got[0].Path != "main.tf" {
		t.Errorf("top = %q, want main.tf as conventional fallback", got[0].Path)
	}
}

func TestSuggest_NewFileWhenRepoHasNoMatch(t *testing.T) {
	files := []hclsummary.FileSummary{
		{Path: "outputs.tf", Parsed: true},
		{Path: "variables.tf", Parsed: true, Resources: []string{}},
	}
	got := placement.Suggest("gcs-logging-enable", files)
	if len(got) != 1 || !got[0].NewFile {
		t.Fatalf("expected a single new-file suggestion, got %+v", got)
	}
	if got[0].Path != "storage.tf" {
		t.Errorf("new-file name = %q, want storage.tf", got[0].Path)
	}
}

func TestSuggest_DedupAndCap(t *testing.T) {
	var files []hclsummary.FileSummary
	for i := 0; i < 12; i++ {
		files = append(files, hclsummary.FileSummary{
			Path: "f" + string(rune('a'+i)) + ".tf", Parsed: true,
			Resources: []string{"aws_lb.x"},
		})
	}
	// same path twice — must dedupe
	files = append(files, files[0])
	got := placement.Suggest("alb-access-logs", files)
	if len(got) > 5 {
		t.Errorf("expected <=5 suggestions, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, s := range got {
		if seen[s.Path] {
			t.Errorf("duplicate path %q in suggestions", s.Path)
		}
		seen[s.Path] = true
	}
}

func TestSuggest_ShallowerPathWinsOnTie(t *testing.T) {
	files := []hclsummary.FileSummary{
		{Path: "a/b/c/lb.tf", Parsed: true, Resources: []string{"aws_lb.deep"}},
		{Path: "lb.tf", Parsed: true, Resources: []string{"aws_lb.shallow"}},
	}
	got := placement.Suggest("alb-access-logs", files)
	if got[0].Path != "lb.tf" {
		t.Errorf("top = %q, want the shallower lb.tf on score tie", got[0].Path)
	}
}
