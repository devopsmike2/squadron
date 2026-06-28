// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package tfimport turns a discovery-scan inventory into Terraform
// import{} blocks so an operator can adopt un-managed cloud resources
// into IaC. Squadron emits the part it can produce precisely — the
// resource type, a sane Terraform address, and the provider-specific
// import ID — and the operator runs:
//
//	terraform plan -generate-config-out=generated.tf
//
// so the provider writes the accurate, full configuration. Squadron
// deliberately does NOT hand-roll per-attribute HCL (that's the
// provider's job and is brittle to reproduce).
//
// Correctness rule: a category/provider pair is supported only when its
// import-ID format is known. Anything else is reported as Skipped with a
// reason — Squadron never emits a guessed import ID that would fail at
// `terraform import` time.
package tfimport

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Resource is the minimal, provider-agnostic shape the generator needs.
// Callers (the discovery handlers) map scan rows onto this.
type Resource struct {
	Provider   string // "aws" (slice 1); "gcp"/"azure"/"oci" in slice 3
	Category   string // "compute","object_store","function","database","load_balancer"
	ResourceID string // cloud identifier as the scan reported it
	Name       string // friendly name when distinct from ResourceID (e.g. Lambda)
	Region     string
}

// ImportBlock is one rendered Terraform import target.
type ImportBlock struct {
	TFType    string // e.g. "aws_instance"
	TFAddress string // e.g. "aws_instance.imported_i_0abc"
	ImportID  string // provider-specific import ID
	Region    string
}

// Skipped records a resource the generator could not safely map.
type Skipped struct {
	Provider   string `json:"provider"`
	Category   string `json:"category"`
	ResourceID string `json:"resource_id"`
	Reason     string `json:"reason"`
}

// mapper derives the tf type + import ID for one resource. Returns ok=false
// when the resource can't be safely mapped (caller records a Skipped).
type mapper func(r Resource) (tfType, importID string, ok bool)

// awsMappers is the per-category AWS registry. Each entry encodes the
// import-ID format for that resource type (see the design doc table).
var awsMappers = map[string]mapper{
	"compute": func(r Resource) (string, string, bool) {
		// aws_instance import ID = instance ID (i-...). Verified against
		// a live scan.
		if r.ResourceID == "" {
			return "", "", false
		}
		return "aws_instance", r.ResourceID, true
	},
	"object_store": func(r Resource) (string, string, bool) {
		// aws_s3_bucket import ID = bucket name. S3 buckets are
		// identified by name; the scan's resource_id carries it.
		if r.ResourceID == "" {
			return "", "", false
		}
		return "aws_s3_bucket", r.ResourceID, true
	},
	"function": func(r Resource) (string, string, bool) {
		// aws_lambda_function import ID = function name. Prefer Name;
		// fall back to the last ARN segment if only an ARN is present.
		name := r.Name
		if name == "" {
			name = lastARNSegment(r.ResourceID)
		}
		if name == "" {
			return "", "", false
		}
		return "aws_lambda_function", name, true
	},
	"database": func(r Resource) (string, string, bool) {
		// aws_db_instance import ID = the DB instance identifier. The
		// scan may report an ARN; extract the identifier after
		// ":db:" when present, else use resource_id verbatim.
		id := r.ResourceID
		if strings.Contains(id, ":db:") {
			id = id[strings.LastIndex(id, ":db:")+len(":db:"):]
		}
		if id == "" {
			return "", "", false
		}
		return "aws_db_instance", id, true
	},
	"load_balancer": func(r Resource) (string, string, bool) {
		// aws_lb import ID = the load balancer ARN.
		if !strings.HasPrefix(r.ResourceID, "arn:") {
			return "", "", false
		}
		return "aws_lb", r.ResourceID, true
	},
}

func lastARNSegment(arn string) string {
	if arn == "" {
		return ""
	}
	if i := strings.LastIndex(arn, ":"); i >= 0 && i+1 < len(arn) {
		return arn[i+1:]
	}
	return arn
}

// Generate maps resources to import blocks, returning the blocks plus a
// list of resources it could not safely map. Deterministic: blocks are
// sorted by address, and Terraform addresses are de-duplicated with a
// numeric suffix so two resources never collide.
func Generate(resources []Resource) ([]ImportBlock, []Skipped) {
	var blocks []ImportBlock
	var skipped []Skipped
	usedAddr := map[string]int{}

	for _, r := range resources {
		provider := strings.ToLower(strings.TrimSpace(r.Provider))
		if provider == "" {
			provider = "aws"
		}
		if provider != "aws" {
			skipped = append(skipped, Skipped{provider, r.Category, r.ResourceID, "provider not yet supported (slice 1 = AWS)"})
			continue
		}
		m, ok := awsMappers[strings.ToLower(strings.TrimSpace(r.Category))]
		if !ok {
			skipped = append(skipped, Skipped{provider, r.Category, r.ResourceID, "category has no import mapper yet"})
			continue
		}
		tfType, importID, ok := m(r)
		if !ok {
			skipped = append(skipped, Skipped{provider, r.Category, r.ResourceID, "could not derive a safe import ID from the scan fields"})
			continue
		}
		label := sanitizeLabel(r.Name)
		if label == "" {
			label = sanitizeLabel(importID)
		}
		addr := tfType + ".imported_" + label
		if n := usedAddr[addr]; n > 0 {
			usedAddr[addr] = n + 1
			addr = fmt.Sprintf("%s_%d", addr, n+1)
		} else {
			usedAddr[addr] = 1
		}
		blocks = append(blocks, ImportBlock{TFType: tfType, TFAddress: addr, ImportID: importID, Region: r.Region})
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].TFAddress < blocks[j].TFAddress })
	return blocks, skipped
}

var labelUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

// sanitizeLabel produces a valid Terraform identifier fragment.
func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = labelUnsafe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return ""
	}
	// A TF identifier can't start with a digit.
	if s[0] >= '0' && s[0] <= '9' {
		s = "r_" + s
	}
	return s
}

// Render emits the import blocks as a complete, paste-able .tf file with
// a header that explains the -generate-config-out workflow. Returns ""
// when there are no blocks (caller decides what to tell the operator).
func Render(blocks []ImportBlock, skipped []Skipped) string {
	if len(blocks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Squadron — Terraform import blocks for un-managed resources.\n")
	b.WriteString("#\n")
	b.WriteString("# These bring existing cloud resources under Terraform management.\n")
	b.WriteString("# Squadron emits the import targets; let the provider write the config:\n")
	b.WriteString("#\n")
	b.WriteString("#   terraform plan -generate-config-out=generated.tf\n")
	b.WriteString("#   terraform apply   # adopts the resources (no changes if config matches)\n")
	b.WriteString("#\n")
	b.WriteString("# Requires Terraform >= 1.5. Review generated.tf before applying.\n")
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "# Note: %d scanned resource(s) were skipped (no safe import mapping yet).\n", len(skipped))
	}
	b.WriteString("\n")
	for _, blk := range blocks {
		if blk.Region != "" {
			fmt.Fprintf(&b, "# region: %s\n", blk.Region)
		}
		b.WriteString("import {\n")
		fmt.Fprintf(&b, "  to = %s\n", blk.TFAddress)
		fmt.Fprintf(&b, "  id = %q\n", blk.ImportID)
		b.WriteString("}\n\n")
	}
	return b.String()
}
