// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package hclpatch

import (
	"strings"
	"testing"
)

func TestStripComments_RemovesLineAndBlockComments(t *testing.T) {
	src := `# top-level line comment
resource "aws_ssm_association" "adot" { // trailing line comment
  name = "AWS-ConfigureAWSPackage" # inline hash comment
  /* block
     comment */
  parameters = {
    action = "Install"
  }
}
`
	out := string(StripComments([]byte(src)))
	for _, bad := range []string{"top-level line comment", "trailing line comment", "inline hash comment", "block", "ConfigureAWSPackage // "} {
		if strings.Contains(out, bad) {
			t.Errorf("expected comment %q to be stripped, got:\n%s", bad, out)
		}
	}
	// Code must survive.
	for _, want := range []string{`resource "aws_ssm_association" "adot"`, `name = "AWS-ConfigureAWSPackage"`, `action = "Install"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected code %q to survive, got:\n%s", want, out)
		}
	}
}

func TestStripComments_PreservesHashInsideStringLiteral(t *testing.T) {
	// The classic regex trap: '#' and '//' inside string values must NOT
	// be treated as comments.
	src := `resource "aws_ssm_parameter" "p" {
  value = "https://example.com/path#fragment"
  tags  = { note = "value with # hash and // slashes" }
}
`
	out := string(StripComments([]byte(src)))
	if !strings.Contains(out, "https://example.com/path#fragment") {
		t.Errorf("URL with # was corrupted:\n%s", out)
	}
	if !strings.Contains(out, "value with # hash and // slashes") {
		t.Errorf("string literal with #/ was corrupted:\n%s", out)
	}
}

func TestStripComments_MalformedReturnsInputUnchanged(t *testing.T) {
	src := []byte(`this is not { valid hcl ===`)
	out := StripComments(src)
	if string(out) != string(src) {
		t.Errorf("malformed input should be returned unchanged, got:\n%s", out)
	}
}

func TestStripComments_NoCommentsIsStable(t *testing.T) {
	src := `resource "aws_s3_bucket_logging" "this" {
  bucket = aws_s3_bucket.logs.id
}
`
	out := string(StripComments([]byte(src)))
	if !strings.Contains(out, `resource "aws_s3_bucket_logging" "this"`) {
		t.Errorf("clean code mangled:\n%s", out)
	}
}
