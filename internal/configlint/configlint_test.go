// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package configlint

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// hasFinding reports whether the slice contains a finding with the given rule ID.
func hasFinding(findings []Finding, rule string) bool {
	for _, f := range findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func TestLint_CleanConfig(t *testing.T) {
	clean := `
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  memory_limiter:
    limit_mib: 512
  batch:

exporters:
  otlp:
    endpoint: backend.observability.svc.cluster.local:4317

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp]
`
	findings := Lint(clean)
	assert.Empty(t, findings, "expected no findings on a clean config; got %+v", findings)
}

func TestLint_EmptyYAML(t *testing.T) {
	findings := Lint("")
	assert.Len(t, findings, 1)
	assert.Equal(t, "empty-config", findings[0].Rule)
}

func TestLint_ParseError(t *testing.T) {
	findings := Lint("this is: :: not yaml")
	assert.Len(t, findings, 1)
	assert.Equal(t, "yaml-parse", findings[0].Rule)
	assert.Equal(t, SeverityError, findings[0].Severity)
}

func TestLint_MissingService(t *testing.T) {
	src := `
receivers:
  otlp:
exporters:
  otlp:
    endpoint: foo:4317
`
	findings := Lint(src)
	assert.True(t, hasFinding(findings, "service-missing"))
}

func TestLint_UndefinedReceiverInPipeline(t *testing.T) {
	// Typo: pipeline references "otpl" but receiver is defined as "otlp".
	src := `
receivers:
  otlp:
exporters:
  otlp:
    endpoint: foo:4317
service:
  pipelines:
    traces:
      receivers: [otpl]
      exporters: [otlp]
`
	findings := Lint(src)
	assert.True(t, hasFinding(findings, "undefined-component"),
		"expected undefined-component finding on typo; got %+v", findings)
}

func TestLint_PipelineWithNoExporters(t *testing.T) {
	src := `
receivers:
  otlp:
exporters:
  otlp:
    endpoint: foo:4317
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: []
`
	findings := Lint(src)
	assert.True(t, hasFinding(findings, "pipeline-no-exporters"))
}

func TestLint_MemoryLimiterNotFirst(t *testing.T) {
	src := `
receivers:
  otlp:
processors:
  batch:
  memory_limiter:
    limit_mib: 512
exporters:
  otlp:
    endpoint: foo:4317
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch, memory_limiter]
      exporters: [otlp]
`
	findings := Lint(src)
	assert.True(t, hasFinding(findings, "memory-limiter-position"))
}

func TestLint_MemoryLimiterFirstIsAccepted(t *testing.T) {
	src := `
receivers:
  otlp:
processors:
  memory_limiter:
    limit_mib: 512
  batch:
exporters:
  otlp:
    endpoint: foo:4317
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp]
`
	findings := Lint(src)
	assert.False(t, hasFinding(findings, "memory-limiter-position"),
		"should not flag memory_limiter when it's first")
}

func TestLint_MissingBatchProcessor(t *testing.T) {
	src := `
receivers:
  otlp:
exporters:
  otlp:
    endpoint: foo:4317
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp]
`
	findings := Lint(src)
	assert.True(t, hasFinding(findings, "missing-batch-processor"))
}

func TestLint_LocalhostInOTLPExporter(t *testing.T) {
	src := `
receivers:
  otlp:
processors:
  batch:
exporters:
  otlp:
    endpoint: localhost:4317
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
`
	findings := Lint(src)
	assert.True(t, hasFinding(findings, "localhost-exporter"))
}

func TestLint_LineNumbersAreReported(t *testing.T) {
	// Build a config where the typo is on a known line, verify the finding
	// surfaces that line number for the editor to jump to.
	src := `receivers:
  otlp:
exporters:
  otlp:
    endpoint: foo:4317
service:
  pipelines:
    traces:
      receivers: [missing_receiver]
      exporters: [otlp]
`
	findings := Lint(src)
	var typoFinding *Finding
	for i := range findings {
		if findings[i].Rule == "undefined-component" {
			typoFinding = &findings[i]
		}
	}
	if assert.NotNil(t, typoFinding) {
		assert.Greater(t, typoFinding.Line, 0, "expected a non-zero line number on the finding")
	}
}
