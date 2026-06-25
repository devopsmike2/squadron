// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package configlint inspects OpenTelemetry collector YAML configurations
// for common mistakes and anti-patterns that the type system can't catch.
//
// It's structural: rules walk the parsed YAML tree and flag findings with a
// severity, an explanation, and a line number from the source. The goal is
// to fail fast on the kinds of mistakes that otherwise turn into 3am pages
// (typo in a pipeline component name, an unbatched exporter that crushes
// throughput, a memory_limiter declared but not placed first in the
// processor chain).
package configlint

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Severity classifies a finding. The UI maps these to badge colors.
type Severity string

const (
	SeverityError   Severity = "error"   // build-breaking — config will not run as intended
	SeverityWarning Severity = "warning" // strongly suggests fixing — silent footgun
	SeverityInfo    Severity = "info"    // best practice recommendation
)

// Finding is one lint result.
type Finding struct {
	Severity Severity `json:"severity"`
	Rule     string   `json:"rule"`           // stable identifier so callers can ignore or auto-fix
	Message  string   `json:"message"`        // human-readable explanation
	Line     int      `json:"line,omitempty"` // 1-indexed; 0 if unknown
	Path     string   `json:"path,omitempty"` // dotted path through the YAML tree e.g. "service.pipelines.traces"
}

// Lint parses YAML and returns every finding. A YAML parse error becomes a
// single SeverityError finding with line 0 so the UI can still render it
// rather than blowing up.
func Lint(yamlSrc string) []Finding {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(yamlSrc), &root); err != nil {
		return []Finding{{
			Severity: SeverityError,
			Rule:     "yaml-parse",
			Message:  fmt.Sprintf("YAML parse error: %v", err),
		}}
	}

	// Empty config is valid YAML but useless — flag it as info so we don't
	// surface a scary error on a blank editor.
	if root.Kind == 0 {
		return []Finding{{
			Severity: SeverityInfo,
			Rule:     "empty-config",
			Message:  "Configuration is empty. Pick a template to get started.",
		}}
	}

	doc := root.Content[0] // unwrap the document node
	if doc == nil || doc.Kind != yaml.MappingNode {
		return []Finding{{
			Severity: SeverityError,
			Rule:     "structural",
			Message:  "Top-level config must be a YAML mapping (key/value pairs)",
		}}
	}

	var findings []Finding
	for _, rule := range rules {
		findings = append(findings, rule(doc)...)
	}
	return findings
}

// rule is the contract every lint rule implements: walk the document, return
// any findings.
type rule func(doc *yaml.Node) []Finding

// rules is the ordered set of every lint rule the engine runs. Order is
// significant only for stable output — every rule sees the same input doc.
var rules = []rule{
	ruleServiceMissing,
	rulePipelineComponentExists,
	rulePipelineNeedsExporter,
	ruleMemoryLimiterFirst,
	ruleBatchBeforeExporter,
	ruleLocalhostInContainerizedExporter,
}

// ruleServiceMissing flags configs without a service section — without it,
// no pipelines run, nothing happens. Easy to forget when refactoring.
func ruleServiceMissing(doc *yaml.Node) []Finding {
	if _, ok := mapChild(doc, "service"); !ok {
		return []Finding{{
			Severity: SeverityError,
			Rule:     "service-missing",
			Message:  "Top-level `service` section is required — collector won't start any pipelines without it",
			Line:     doc.Line,
		}}
	}
	return nil
}

// rulePipelineComponentExists catches the single most common typo class:
// a pipeline references `receivers: [otlp]` but the `receivers:` section
// declares `otpl` (transposed). Without this rule, the collector silently
// drops the data and the operator gets paged for "no telemetry."
func rulePipelineComponentExists(doc *yaml.Node) []Finding {
	defined := map[string]map[string]bool{
		"receivers":  collectKeys(doc, "receivers"),
		"processors": collectKeys(doc, "processors"),
		"exporters":  collectKeys(doc, "exporters"),
		"extensions": collectKeys(doc, "extensions"),
	}

	service, ok := mapChild(doc, "service")
	if !ok || service.Kind != yaml.MappingNode {
		return nil
	}

	var findings []Finding

	// Extensions referenced under service.extensions
	if extList, ok := mapChild(service, "extensions"); ok && extList.Kind == yaml.SequenceNode {
		for _, name := range extList.Content {
			if !defined["extensions"][name.Value] {
				findings = append(findings, Finding{
					Severity: SeverityError,
					Rule:     "undefined-component",
					Message:  fmt.Sprintf("Extension %q referenced under `service.extensions` but not defined in top-level `extensions:` section", name.Value),
					Line:     name.Line,
					Path:     "service.extensions",
				})
			}
		}
	}

	pipelines, ok := mapChild(service, "pipelines")
	if !ok || pipelines.Kind != yaml.MappingNode {
		return findings
	}

	for i := 0; i < len(pipelines.Content); i += 2 {
		pipeName := pipelines.Content[i].Value
		pipeNode := pipelines.Content[i+1]
		if pipeNode.Kind != yaml.MappingNode {
			continue
		}

		for _, kind := range []string{"receivers", "processors", "exporters"} {
			listNode, ok := mapChild(pipeNode, kind)
			if !ok || listNode.Kind != yaml.SequenceNode {
				continue
			}
			for _, comp := range listNode.Content {
				if !defined[kind][comp.Value] {
					findings = append(findings, Finding{
						Severity: SeverityError,
						Rule:     "undefined-component",
						Message:  fmt.Sprintf("Pipeline %q references %s %q which isn't defined in top-level `%s:` section", pipeName, singular(kind), comp.Value, kind),
						Line:     comp.Line,
						Path:     fmt.Sprintf("service.pipelines.%s.%s", pipeName, kind),
					})
				}
			}
		}
	}
	return findings
}

// rulePipelineNeedsExporter catches a pipeline with no exporters declared —
// the data is parsed and processed but goes nowhere.
func rulePipelineNeedsExporter(doc *yaml.Node) []Finding {
	service, ok := mapChild(doc, "service")
	if !ok || service.Kind != yaml.MappingNode {
		return nil
	}
	pipelines, ok := mapChild(service, "pipelines")
	if !ok || pipelines.Kind != yaml.MappingNode {
		return nil
	}

	var findings []Finding
	for i := 0; i < len(pipelines.Content); i += 2 {
		pipeName := pipelines.Content[i].Value
		pipeNode := pipelines.Content[i+1]
		if pipeNode.Kind != yaml.MappingNode {
			continue
		}
		exporters, ok := mapChild(pipeNode, "exporters")
		if !ok || exporters.Kind != yaml.SequenceNode || len(exporters.Content) == 0 {
			findings = append(findings, Finding{
				Severity: SeverityError,
				Rule:     "pipeline-no-exporters",
				Message:  fmt.Sprintf("Pipeline %q has no exporters — data has nowhere to go", pipeName),
				Line:     pipeNode.Line,
				Path:     fmt.Sprintf("service.pipelines.%s", pipeName),
			})
		}
	}
	return findings
}

// ruleMemoryLimiterFirst enforces the OTel best practice that
// memory_limiter MUST be the first processor in any pipeline that uses it,
// otherwise it can't drop incoming data before downstream processors buffer
// it and OOM the collector.
func ruleMemoryLimiterFirst(doc *yaml.Node) []Finding {
	service, ok := mapChild(doc, "service")
	if !ok || service.Kind != yaml.MappingNode {
		return nil
	}
	pipelines, ok := mapChild(service, "pipelines")
	if !ok || pipelines.Kind != yaml.MappingNode {
		return nil
	}

	var findings []Finding
	for i := 0; i < len(pipelines.Content); i += 2 {
		pipeName := pipelines.Content[i].Value
		pipeNode := pipelines.Content[i+1]
		if pipeNode.Kind != yaml.MappingNode {
			continue
		}
		procs, ok := mapChild(pipeNode, "processors")
		if !ok || procs.Kind != yaml.SequenceNode {
			continue
		}
		for idx, p := range procs.Content {
			name := p.Value
			if strings.HasPrefix(name, "memory_limiter") && idx != 0 {
				findings = append(findings, Finding{
					Severity: SeverityWarning,
					Rule:     "memory-limiter-position",
					Message:  fmt.Sprintf("memory_limiter should be the first processor in pipeline %q so it can drop data before downstream buffers OOM the collector", pipeName),
					Line:     p.Line,
					Path:     fmt.Sprintf("service.pipelines.%s.processors", pipeName),
				})
			}
		}
	}
	return findings
}

// ruleBatchBeforeExporter warns when a pipeline has no `batch` processor
// before its exporters. Per-record exports are usually fine in dev but
// disastrous in production for high-volume signals.
func ruleBatchBeforeExporter(doc *yaml.Node) []Finding {
	service, ok := mapChild(doc, "service")
	if !ok || service.Kind != yaml.MappingNode {
		return nil
	}
	pipelines, ok := mapChild(service, "pipelines")
	if !ok || pipelines.Kind != yaml.MappingNode {
		return nil
	}

	var findings []Finding
	for i := 0; i < len(pipelines.Content); i += 2 {
		pipeName := pipelines.Content[i].Value
		pipeNode := pipelines.Content[i+1]
		if pipeNode.Kind != yaml.MappingNode {
			continue
		}
		procs, ok := mapChild(pipeNode, "processors")
		hasBatch := false
		if ok && procs.Kind == yaml.SequenceNode {
			for _, p := range procs.Content {
				if strings.HasPrefix(p.Value, "batch") {
					hasBatch = true
					break
				}
			}
		}
		if !hasBatch {
			findings = append(findings, Finding{
				Severity: SeverityWarning,
				Rule:     "missing-batch-processor",
				Message:  fmt.Sprintf("Pipeline %q has no `batch` processor — per-record exports will be inefficient at production scale", pipeName),
				Line:     pipeNode.Line,
				Path:     fmt.Sprintf("service.pipelines.%s.processors", pipeName),
			})
		}
	}
	return findings
}

// ruleLocalhostInContainerizedExporter flags otlp exporters pointing at
// `localhost` or `127.0.0.1` — almost always a mistake in a containerized
// deployment where localhost is the collector container itself.
func ruleLocalhostInContainerizedExporter(doc *yaml.Node) []Finding {
	exporters, ok := mapChild(doc, "exporters")
	if !ok || exporters.Kind != yaml.MappingNode {
		return nil
	}

	var findings []Finding
	for i := 0; i < len(exporters.Content); i += 2 {
		name := exporters.Content[i].Value
		body := exporters.Content[i+1]
		if body.Kind != yaml.MappingNode {
			continue
		}
		// Only flag otlp/otlphttp exporters. Other exporters use endpoint
		// fields too but rarely have the same "you meant the host machine"
		// footgun.
		if !strings.HasPrefix(name, "otlp") {
			continue
		}
		endpoint, ok := mapChild(body, "endpoint")
		if !ok {
			continue
		}
		v := strings.ToLower(endpoint.Value)
		if strings.Contains(v, "localhost") || strings.HasPrefix(v, "127.") {
			findings = append(findings, Finding{
				Severity: SeverityWarning,
				Rule:     "localhost-exporter",
				Message:  fmt.Sprintf("Exporter %q points at %q — in a containerized deployment this is usually wrong; use the destination service's DNS name or IP", name, endpoint.Value),
				Line:     endpoint.Line,
				Path:     fmt.Sprintf("exporters.%s.endpoint", name),
			})
		}
	}
	return findings
}

// mapChild looks up `key` under a YAML mapping node. Returns the value node
// and ok=true if found.
func mapChild(parent *yaml.Node, key string) (*yaml.Node, bool) {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i+1], true
		}
	}
	return nil, false
}

// collectKeys returns the set of keys defined under a top-level mapping
// (e.g. every defined receiver name under `receivers:`).
func collectKeys(doc *yaml.Node, section string) map[string]bool {
	out := make(map[string]bool)
	node, ok := mapChild(doc, section)
	if !ok || node.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i < len(node.Content); i += 2 {
		out[node.Content[i].Value] = true
	}
	return out
}

// singular returns the user-facing singular of a section name.
func singular(s string) string {
	switch s {
	case "receivers":
		return "receiver"
	case "processors":
		return "processor"
	case "exporters":
		return "exporter"
	case "extensions":
		return "extension"
	}
	return s
}
