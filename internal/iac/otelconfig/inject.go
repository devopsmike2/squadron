// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package otelconfig injects a Squadron OTLP exporter into an existing
// OpenTelemetry Collector configuration so an installed-but-unconnected
// agent starts shipping telemetry to Squadron. The injection is
// deterministic, idempotent, and minimal-diff: it edits the YAML node
// tree in place so untouched keys, ordering, and comments survive in
// the rendered output (the change is delivered as a review-friendly PR).
//
// Design: docs/proposals/otel-agent-deploy-and-inject.md (slice 1).
package otelconfig

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// DefaultSignals are the service pipelines the Squadron exporter is
// wired into when Options.Signals is empty.
var DefaultSignals = []string{"traces", "metrics", "logs"}

// Options tunes the injection. The zero value is valid and yields a
// gRPC "otlp/squadron" exporter wired into traces+metrics+logs, with
// pipeline scaffolding created when absent.
type Options struct {
	// ExporterName is the collector component name for the Squadron
	// exporter. Defaults to "otlp/squadron" (grpc) or
	// "otlphttp/squadron" (http). A distinct named instance means we
	// never overwrite the operator's own otlp/otlphttp exporter.
	ExporterName string
	// Protocol selects the exporter component type: "grpc" -> otlp,
	// "http" -> otlphttp. Defaults to "grpc".
	Protocol string
	// Insecure adds `tls: {insecure: true}` to the exporter (dev /
	// self-signed Squadron endpoints).
	Insecure bool
	// Signals is the subset of service pipelines to wire the exporter
	// into. Empty means DefaultSignals.
	Signals []string
	// NoCreatePipelines, when true, only wires the exporter into
	// pipelines that already exist (won't scaffold service.pipelines).
	NoCreatePipelines bool
}

func (o Options) exporterType() string {
	if o.Protocol == "http" {
		return "otlphttp"
	}
	return "otlp"
}

func (o Options) exporterName() string {
	if o.ExporterName != "" {
		return o.ExporterName
	}
	return o.exporterType() + "/squadron"
}

func (o Options) signals() []string {
	if len(o.Signals) == 0 {
		return DefaultSignals
	}
	return o.Signals
}

// Result is the outcome of an injection.
type Result struct {
	// Bytes is the rendered config. Equal to the input (re-rendered)
	// when Changed is false.
	Bytes []byte
	// Changed is false when the config already exported to the endpoint
	// and was wired into every requested pipeline (idempotent no-op).
	Changed bool
	// Summary is a short human-readable description of what changed.
	Summary string
}

// InjectOTLPExporter ensures the collector config in src exports to
// endpoint via a dedicated Squadron OTLP exporter wired into the
// requested service pipelines. It returns the rendered config and
// whether anything changed.
func InjectOTLPExporter(src []byte, endpoint string, opts Options) (Result, error) {
	if endpoint == "" {
		return Result{}, errors.New("otelconfig: endpoint is required")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return Result{}, fmt.Errorf("otelconfig: parse collector config: %w", err)
	}

	// Resolve (or create) the root mapping node.
	root := rootMapping(&doc)
	if root == nil {
		return Result{}, errors.New("otelconfig: collector config root is not a mapping")
	}

	name := opts.exporterName()
	changed := false

	// 1. exporters.<name>.endpoint (+ tls.insecure).
	exporters := mapChildMapping(root, "exporters", true)
	exp := mapChildMapping(exporters, name, true)
	if setScalar(exp, "endpoint", endpoint) {
		changed = true
	}
	if opts.Insecure {
		tls := mapChildMapping(exp, "tls", true)
		if setScalar(tls, "insecure", "true") {
			// store as bool, not string
			if v, _ := mapValue(tls, "insecure"); v != nil {
				v.Tag = "!!bool"
			}
			changed = true
		}
	}

	// 2. service.pipelines.<signal>.exporters: ensure name present.
	service := mapChildMapping(root, "service", true)
	pipelines := mapChildMapping(service, "pipelines", true)
	for _, sig := range opts.signals() {
		pipe, _ := mapValue(pipelines, sig)
		if pipe == nil {
			if opts.NoCreatePipelines {
				continue
			}
			pipe = newMapping()
			mapAppend(pipelines, sig, pipe)
			changed = true
		}
		if pipe.Kind != yaml.MappingNode {
			continue
		}
		seq, _ := mapValue(pipe, "exporters")
		if seq == nil {
			seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			mapAppend(pipe, "exporters", seq)
		}
		if ensureInSequence(seq, name) {
			changed = true
		}
	}

	out, err := render(&doc)
	if err != nil {
		return Result{}, err
	}
	summary := fmt.Sprintf("no change: %s already exports to %s", name, endpoint)
	if changed {
		summary = fmt.Sprintf("injected exporter %q -> %s and wired into pipelines %v", name, endpoint, opts.signals())
	}
	return Result{Bytes: out, Changed: changed, Summary: summary}, nil
}

// --- yaml.Node helpers ---------------------------------------------

func rootMapping(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			m := newMapping()
			doc.Content = []*yaml.Node{m}
			return m
		}
		if doc.Content[0].Kind == yaml.MappingNode {
			return doc.Content[0]
		}
		return nil
	}
	if doc.Kind == yaml.MappingNode {
		return doc
	}
	return nil
}

func newMapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

// mapValue returns the value node for key in mapping m (nil if absent).
func mapValue(m *yaml.Node, key string) (*yaml.Node, int) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], i + 1
		}
	}
	return nil, -1
}

// mapChildMapping returns the mapping value for key, creating it when
// create is true and it is absent.
func mapChildMapping(m *yaml.Node, key string, create bool) *yaml.Node {
	v, _ := mapValue(m, key)
	if v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	if v != nil || !create {
		return v
	}
	child := newMapping()
	mapAppend(m, key, child)
	return child
}

func mapAppend(m *yaml.Node, key string, val *yaml.Node) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val,
	)
}

// setScalar sets key=val under mapping m; returns true if it created or
// changed the value.
func setScalar(m *yaml.Node, key, val string) bool {
	v, _ := mapValue(m, key)
	if v != nil {
		if v.Value == val {
			return false
		}
		v.Value = val
		v.Tag = "!!str"
		return true
	}
	mapAppend(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
	return true
}

// ensureInSequence appends val to a scalar sequence when absent.
func ensureInSequence(seq *yaml.Node, val string) bool {
	if seq.Kind != yaml.SequenceNode {
		return false
	}
	for _, c := range seq.Content {
		if c.Value == val {
			return false
		}
	}
	seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
	return true
}

func render(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("otelconfig: render: %w", err)
	}
	if cerr := enc.Close(); cerr != nil {
		return nil, fmt.Errorf("otelconfig: render close: %w", cerr)
	}
	return buf.Bytes(), nil
}
