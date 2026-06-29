// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package otelconfig

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

const baseConfig = `receivers:
  otlp:
    protocols:
      grpc:
      http:
processors:
  batch:
exporters:
  debug:
    verbosity: detailed
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
`

func TestInject_AddsExporterAndWiresPipelines(t *testing.T) {
	r, err := InjectOTLPExporter([]byte(baseConfig), "squadron.example.com:4317", Options{Insecure: true})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !r.Changed {
		t.Fatalf("expected Changed=true")
	}
	out := string(r.Bytes)
	for _, want := range []string{
		"otlp/squadron:",
		"endpoint: squadron.example.com:4317",
		"insecure: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// existing exporter + receivers preserved (minimal diff).
	for _, keep := range []string{"debug:", "verbosity: detailed", "receivers:", "batch:"} {
		if !strings.Contains(out, keep) {
			t.Errorf("output dropped existing %q:\n%s", keep, out)
		}
	}
	// wired into both pipelines that existed.
	if strings.Count(out, "otlp/squadron") < 3 { // 1 exporter def + 2 pipeline refs
		t.Errorf("exporter not wired into both pipelines:\n%s", out)
	}
}

func TestInject_Idempotent(t *testing.T) {
	first, err := InjectOTLPExporter([]byte(baseConfig), "sq:4317", Options{Insecure: true})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := InjectOTLPExporter(first.Bytes, "sq:4317", Options{Insecure: true})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Changed {
		t.Errorf("second injection should be a no-op, got Changed=true:\n%s", second.Bytes)
	}
}

func TestInject_UpdatesEndpointWhenChanged(t *testing.T) {
	first, _ := InjectOTLPExporter([]byte(baseConfig), "old:4317", Options{})
	second, err := InjectOTLPExporter(first.Bytes, "new:4317", Options{})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Changed {
		t.Fatalf("endpoint change should flip Changed=true")
	}
	out := string(second.Bytes)
	if !strings.Contains(out, "endpoint: new:4317") || strings.Contains(out, "endpoint: old:4317") {
		t.Errorf("endpoint not updated:\n%s", out)
	}
}

func TestInject_CreatesPipelineWhenAbsent(t *testing.T) {
	// logs pipeline absent in baseConfig; default signals include it.
	r, err := InjectOTLPExporter([]byte(baseConfig), "sq:4317", Options{})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	out := string(r.Bytes)
	if !strings.Contains(out, "logs:") {
		t.Errorf("logs pipeline not scaffolded:\n%s", out)
	}
}

func TestInject_NoCreatePipelines_OnlyWiresExisting(t *testing.T) {
	r, err := InjectOTLPExporter([]byte(baseConfig), "sq:4317", Options{NoCreatePipelines: true})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	out := string(r.Bytes)
	// logs pipeline did not exist -> must NOT be created.
	if strings.Contains(out, "logs:") {
		t.Errorf("logs pipeline should not be scaffolded with NoCreatePipelines:\n%s", out)
	}
}

func TestInject_HTTPProtocolUsesOtlphttp(t *testing.T) {
	r, err := InjectOTLPExporter([]byte(baseConfig), "sq:4318", Options{Protocol: "http"})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !strings.Contains(string(r.Bytes), "otlphttp/squadron:") {
		t.Errorf("expected otlphttp/squadron exporter:\n%s", r.Bytes)
	}
}

func TestInject_EmptyEndpointErrors(t *testing.T) {
	if _, err := InjectOTLPExporter([]byte(baseConfig), "", Options{}); err == nil {
		t.Fatalf("expected error for empty endpoint")
	}
}

func TestInject_PlaceholderConfigGetsConnected(t *testing.T) {
	// Mirrors the inject-target VM: a standalone collector with a
	// placeholder endpoint and no real exporter wired.
	placeholder := `receivers:
  otlp:
    protocols:
      grpc:
exporters:
  otlp:
    endpoint: REPLACE_WITH_SQUADRON_OTLP
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp]
`
	r, err := InjectOTLPExporter([]byte(placeholder), "squadron:4317", Options{Insecure: true})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	out := string(r.Bytes)
	// our dedicated exporter added; operator's placeholder otlp untouched.
	if !strings.Contains(out, "otlp/squadron:") || !strings.Contains(out, "endpoint: squadron:4317") {
		t.Errorf("squadron exporter not injected:\n%s", out)
	}
	if !strings.Contains(out, "REPLACE_WITH_SQUADRON_OTLP") {
		t.Errorf("operator's own otlp exporter was clobbered:\n%s", out)
	}
}

// TestInject_ScaffoldedPipelineHasReceiver pins the fix for the live-e2e
// bug: a newly-scaffolded pipeline (e.g. logs, absent in baseConfig) must
// carry a receiver, otherwise otelcol rejects the config at startup and
// the collector never runs. The otlp receiver (supports all signals) is
// seeded from the config's receivers section.
func TestInject_ScaffoldedPipelineHasReceiver(t *testing.T) {
	r, err := InjectOTLPExporter([]byte(baseConfig), "sq:4317", Options{})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	out := string(r.Bytes)
	// Re-parse and assert the logs pipeline has >=1 receiver.
	var doc map[string]any
	if err := yaml.Unmarshal(r.Bytes, &doc); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	svc, _ := doc["service"].(map[string]any)
	pipes, _ := svc["pipelines"].(map[string]any)
	logs, ok := pipes["logs"].(map[string]any)
	if !ok {
		t.Fatalf("logs pipeline not scaffolded:\n%s", out)
	}
	recv, ok := logs["receivers"].([]any)
	if !ok || len(recv) == 0 {
		t.Fatalf("scaffolded logs pipeline has no receiver (invalid otelcol config):\n%s", out)
	}
	if recv[0] != "otlp" {
		t.Errorf("logs receiver = %v, want otlp", recv[0])
	}
}

// TestInject_SkipsScaffoldWhenNoOTLPReceiver: if there's no otlp receiver
// to seed a new pipeline with, the injector must not emit an invalid
// receiver-less pipeline — it skips that signal instead.
func TestInject_SkipsScaffoldWhenNoOTLPReceiver(t *testing.T) {
	cfg := "receivers:\n  hostmetrics:\n    scrapers:\n      cpu:\n" +
		"exporters:\n  debug:\n" +
		"service:\n  pipelines:\n    metrics:\n      receivers: [hostmetrics]\n      exporters: [debug]\n"
	r, err := InjectOTLPExporter([]byte(cfg), "sq:4317", Options{Signals: []string{"logs"}})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if strings.Contains(string(r.Bytes), "logs:") {
		t.Errorf("logs pipeline should be skipped (no otlp receiver to seed it):\n%s", r.Bytes)
	}
}
