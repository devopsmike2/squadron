// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package hclsummary extracts a compact, deterministic summary of a
// Terraform/HCL file — the real resource addresses, data sources,
// variables, providers, and modules an operator already declared — so
// the discovery proposer can generate snippets that reference the
// operator's actual config instead of inventing resource addresses.
//
// The whole point is to do the heavy lifting WITHOUT spending AI
// tokens: parsing is local and free, and only the small rendered
// summary (not the raw files) is fed into the prompt. RenderForPrompt
// enforces a byte budget so the token cost stays bounded and
// predictable regardless of repo size.
package hclsummary

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// FileSummary is the deterministic extract of one .tf file.
type FileSummary struct {
	Path        string
	Resources   []string // "aws_instance.web"
	DataSources []string // "data.aws_ami.al2023"
	Variables   []string // "region"
	Providers   []string // "aws"
	Modules     []string // "compute"
	// Parsed is false when the file did not parse as HCL; callers
	// can choose to skip it. The other slices are empty in that case.
	Parsed bool
}

// SummarizeFile parses src as HCL and returns its structural summary.
// A parse failure is NOT an error: it returns a FileSummary with
// Parsed=false and empty slices, because "this file is noise to the
// model" is a normal outcome (a .tf.json, a templated fragment, etc.)
// and must never break recommendation generation.
func SummarizeFile(path string, src []byte) FileSummary {
	fs := FileSummary{Path: path}
	f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() || f == nil {
		return fs
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return fs
	}
	fs.Parsed = true
	for _, blk := range body.Blocks {
		switch blk.Type {
		case "resource":
			if len(blk.Labels) >= 2 {
				fs.Resources = append(fs.Resources, blk.Labels[0]+"."+blk.Labels[1])
			}
		case "data":
			if len(blk.Labels) >= 2 {
				fs.DataSources = append(fs.DataSources, "data."+blk.Labels[0]+"."+blk.Labels[1])
			}
		case "variable":
			if len(blk.Labels) >= 1 {
				fs.Variables = append(fs.Variables, blk.Labels[0])
			}
		case "provider":
			if len(blk.Labels) >= 1 {
				fs.Providers = append(fs.Providers, blk.Labels[0])
			}
		case "module":
			if len(blk.Labels) >= 1 {
				fs.Modules = append(fs.Modules, blk.Labels[0])
			}
		}
	}
	sort.Strings(fs.Resources)
	sort.Strings(fs.DataSources)
	sort.Strings(fs.Variables)
	sort.Strings(fs.Providers)
	sort.Strings(fs.Modules)
	return fs
}

// RenderForPrompt renders one or more file summaries into a compact
// prompt block, capped at byteBudget bytes. Files are emitted in the
// order given; once the budget is exhausted the remaining files are
// summarised as a single "(N more files omitted for brevity)" line so
// the model knows the context was truncated rather than absent.
//
// Returns "" when there is nothing useful to say (no parsed files with
// any declarations) so callers can keep the cold-start prompt
// byte-identical by only appending a non-empty result.
func RenderForPrompt(summaries []FileSummary, byteBudget int) string {
	if byteBudget <= 0 {
		byteBudget = 6000
	}
	var useful []FileSummary
	for _, s := range summaries {
		if s.Parsed && (len(s.Resources)+len(s.DataSources)+len(s.Variables)+len(s.Providers)+len(s.Modules)) > 0 {
			useful = append(useful, s)
		}
	}
	if len(useful) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("EXISTING TERRAFORM CONTEXT (the operator's repo — generate snippets that reference these REAL addresses; do not invent resource names):\n")
	rendered := 0
	for i, s := range useful {
		line := renderOne(s)
		if b.Len()+len(line) > byteBudget && rendered > 0 {
			fmt.Fprintf(&b, "(%d more file(s) omitted for brevity)\n", len(useful)-i)
			break
		}
		b.WriteString(line)
		rendered++
	}
	return b.String()
}

func renderOne(s FileSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "File %s:\n", s.Path)
	if len(s.Resources) > 0 {
		fmt.Fprintf(&b, "  resources: %s\n", strings.Join(s.Resources, ", "))
	}
	if len(s.DataSources) > 0 {
		fmt.Fprintf(&b, "  data sources: %s\n", strings.Join(s.DataSources, ", "))
	}
	if len(s.Variables) > 0 {
		fmt.Fprintf(&b, "  variables: %s\n", strings.Join(s.Variables, ", "))
	}
	if len(s.Providers) > 0 {
		fmt.Fprintf(&b, "  providers: %s\n", strings.Join(s.Providers, ", "))
	}
	if len(s.Modules) > 0 {
		fmt.Fprintf(&b, "  modules: %s\n", strings.Join(s.Modules, ", "))
	}
	return b.String()
}

// OverlappingResources returns the resource addresses ("type.name")
// that appear in BOTH the snippet and the existing file. A non-empty
// result means committing the snippet would declare a resource that
// already exists — a duplicate that Terraform rejects at plan time.
// Callers use this to dedup: refuse (or skip) re-adding instrumentation
// the operator already has.
//
// Both inputs are parsed independently; a parse failure on either side
// yields no overlap (empty result) rather than a false positive — the
// safe default is "let it through" so a malformed snippet never blocks
// a legitimate PR on a spurious dedup hit.
func OverlappingResources(snippet, existing []byte) []string {
	s := SummarizeFile("snippet.tf", snippet)
	e := SummarizeFile("existing.tf", existing)
	if !s.Parsed || !e.Parsed {
		return nil
	}
	have := make(map[string]struct{}, len(e.Resources))
	for _, r := range e.Resources {
		have[r] = struct{}{}
	}
	var out []string
	for _, r := range s.Resources {
		if _, dup := have[r]; dup {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}
