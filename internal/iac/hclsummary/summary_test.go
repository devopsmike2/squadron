// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package hclsummary

import (
	"strings"
	"testing"
)

const sampleTF = `
provider "aws" {
  region = var.region
}

variable "name_prefix" { default = "squadron-test" }
variable "region"      { default = "us-east-1" }

data "aws_ami" "al2023" {
  most_recent = true
}

resource "aws_security_group" "compute" {
  name = "${var.name_prefix}-sg"
}

resource "aws_instance" "this" {
  ami           = data.aws_ami.al2023.id
  instance_type = "t3.micro"
}

module "logs" {
  source = "../logs"
}
`

func TestSummarizeFile_ExtractsAddresses(t *testing.T) {
	s := SummarizeFile("modules/compute/main.tf", []byte(sampleTF))
	if !s.Parsed {
		t.Fatal("expected Parsed=true")
	}
	wantRes := []string{"aws_instance.this", "aws_security_group.compute"}
	if strings.Join(s.Resources, ",") != strings.Join(wantRes, ",") {
		t.Errorf("resources = %v, want %v (sorted)", s.Resources, wantRes)
	}
	if strings.Join(s.DataSources, ",") != "data.aws_ami.al2023" {
		t.Errorf("data sources = %v", s.DataSources)
	}
	if strings.Join(s.Variables, ",") != "name_prefix,region" {
		t.Errorf("variables = %v", s.Variables)
	}
	if strings.Join(s.Providers, ",") != "aws" {
		t.Errorf("providers = %v", s.Providers)
	}
	if strings.Join(s.Modules, ",") != "logs" {
		t.Errorf("modules = %v", s.Modules)
	}
}

func TestSummarizeFile_MalformedIsNotParsedNotError(t *testing.T) {
	s := SummarizeFile("bad.tf", []byte(`this is not { valid hcl ===`))
	if s.Parsed {
		t.Error("expected Parsed=false for malformed input")
	}
	if len(s.Resources) != 0 {
		t.Errorf("expected no resources from malformed input, got %v", s.Resources)
	}
}

func TestRenderForPrompt_EmptyWhenNothingUseful(t *testing.T) {
	// An unparsed file and an empty-but-parsed file both contribute nothing.
	out := RenderForPrompt([]FileSummary{
		{Path: "x.tf", Parsed: false},
		{Path: "y.tf", Parsed: true},
	}, 6000)
	if out != "" {
		t.Errorf("expected empty render (cold-start parity), got:\n%s", out)
	}
}

func TestRenderForPrompt_RendersRealAddresses(t *testing.T) {
	s := SummarizeFile("modules/compute/main.tf", []byte(sampleTF))
	out := RenderForPrompt([]FileSummary{s}, 6000)
	for _, want := range []string{
		"EXISTING TERRAFORM CONTEXT",
		"modules/compute/main.tf",
		"aws_instance.this",
		"data.aws_ami.al2023",
		"name_prefix",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q, got:\n%s", want, out)
		}
	}
}

func TestRenderForPrompt_RespectsByteBudget(t *testing.T) {
	s := SummarizeFile("modules/compute/main.tf", []byte(sampleTF))
	many := []FileSummary{s, s, s, s, s}
	// Tiny budget: only the first file fits, rest are summarised as omitted.
	out := RenderForPrompt(many, 200)
	if !strings.Contains(out, "more file(s) omitted") {
		t.Errorf("expected truncation note under tight budget, got:\n%s", out)
	}
}

func TestOverlappingResources(t *testing.T) {
	existing := []byte(`resource "aws_instance" "this" {}
resource "aws_ssm_association" "adot_i_123" { name = "x" }
`)
	// Snippet re-declares the SAME ssm association → overlap.
	dupSnippet := []byte(`resource "aws_ssm_association" "adot_i_123" { name = "y" }`)
	got := OverlappingResources(dupSnippet, existing)
	if strings.Join(got, ",") != "aws_ssm_association.adot_i_123" {
		t.Errorf("expected overlap on the ssm association, got %v", got)
	}
	// A fresh resource → no overlap.
	newSnippet := []byte(`resource "aws_ssm_association" "adot_i_999" { name = "z" }`)
	if got := OverlappingResources(newSnippet, existing); len(got) != 0 {
		t.Errorf("expected no overlap for a new resource, got %v", got)
	}
	// Malformed snippet → no false-positive overlap.
	if got := OverlappingResources([]byte("not { hcl ==="), existing); len(got) != 0 {
		t.Errorf("malformed snippet should yield no overlap, got %v", got)
	}
}
