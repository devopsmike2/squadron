// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package quickstart is the v0.27.1 onboarding registry. Its job
// is to turn "I'm new" or "I have an existing fleet" into a
// copy-pasteable collector config + an OpAMP extension snippet
// that points at this Squadron instance.
//
// The package is deliberately data-heavy and logic-light: a
// catalog of backends + a small template engine that substitutes
// the OpAMP server URL. Each template is a complete, valid OTel
// Collector config (not a fragment) so the operator can copy it
// straight into /etc/otelcol/config.yaml and start.
//
// Design notes:
//
//   - Backends are an enum, not free-form, so the UI's backend
//     picker stays predictable and the catalog stays auditable.
//   - The OpAMP server URL is a per-request parameter, not a
//     baked-in constant. Operators run Squadron behind reverse
//     proxies, on different ports in prod vs dev, on different
//     hostnames per environment. We don't try to detect; we let
//     the UI prompt and pass the value.
//   - No external state. Everything is pure functions over the
//     in-memory catalog.
package quickstart

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Backend identifies one of the popular OTel destinations
// Squadron ships starter configs for. Keep this list focused —
// each new backend is a real curation commitment.
type Backend string

const (
	BackendDatadog     Backend = "datadog"
	BackendHoneycomb   Backend = "honeycomb"
	BackendNewRelic    Backend = "newrelic"
	BackendSigNoz      Backend = "signoz"
	BackendGrafana     Backend = "grafana"
	BackendGenericOTLP Backend = "otlp" // any OTel-compatible backend (Tempo, Mimir, self-hosted)
)

// AllBackends is the ordered catalog the UI renders as the
// backend picker. Order is "most likely first" for an SMB shop.
var AllBackends = []Backend{
	BackendDatadog,
	BackendHoneycomb,
	BackendNewRelic,
	BackendSigNoz,
	BackendGrafana,
	BackendGenericOTLP,
}

// BackendInfo is what the UI renders for each backend on the
// picker screen. Kept lightweight: name, description, the
// environment variables the operator will need to set on the
// collector host, and a docs link.
type BackendInfo struct {
	ID          Backend `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	// EnvVars is the list of env vars the rendered config
	// references (typically one for the API key). The UI renders
	// these as a "you'll need to export these on the collector
	// host" reminder. We deliberately don't ask the operator to
	// paste the key — the config references the env var by name.
	EnvVars []EnvVar `json:"env_vars,omitempty"`
	DocsURL string   `json:"docs_url,omitempty"`
}

// EnvVar describes one environment variable the generated config
// references. The operator sets these on the collector host
// before starting the collector; Squadron never sees them.
type EnvVar struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	// Required is false for env vars that have sensible defaults
	// hard-coded in the template (e.g. region defaults).
	Required bool `json:"required"`
}

// Catalog returns the public-facing list of backends for the UI.
func Catalog() []BackendInfo {
	out := make([]BackendInfo, 0, len(AllBackends))
	for _, b := range AllBackends {
		out = append(out, backendInfoFor(b))
	}
	return out
}

// StarterConfig builds a complete, valid OTel Collector config
// for the chosen backend, wired to talk to this Squadron at
// opampServerURL (e.g. "ws://squadron:4320/v1/opamp" inside a
// docker network, or "ws://squadron.example.com:4320/v1/opamp"
// for a remote agent). Returns an error for unknown backends.
//
// The output is ready to drop into /etc/otelcol/config.yaml or
// equivalent. The collector + supervisor binary references are
// not included; the install-command UI tells the operator how to
// fetch them.
func StarterConfig(b Backend, opampServerURL string) (string, error) {
	if opampServerURL == "" {
		return "", fmt.Errorf("opamp_server_url is required")
	}
	body, ok := starterTemplates[b]
	if !ok {
		return "", fmt.Errorf("unknown backend %q", b)
	}
	full := strings.ReplaceAll(body, "{{OPAMP_SERVER_URL}}", opampServerURL)
	return full, nil
}

// OpAMPExtensionSnippet returns just the opamp extension block
// the operator pastes into an existing collector config to opt
// in. This is the v0.27.1 "I already have collectors" branch's
// killer feature — operators don't have to swap their whole
// config to adopt Squadron management.
//
// The returned YAML is two top-level keys (extensions + service)
// the operator merges into their existing config. The Comment
// header explains the merge.
func OpAMPExtensionSnippet(opampServerURL string) (string, error) {
	return AdoptionSnippet(opampServerURL, "", nil)
}

// AdoptionSnippet builds a per-host adoption snippet — the OpAMP
// extension config plus an optional agent_description block that
// identifies this agent to Squadron with the supplied labels.
//
// Use this when bringing an EXISTING collector under Squadron
// management. The snippet adds the OpAMP extension and registers
// the agent identity; it doesn't touch receivers, processors,
// exporters, or pipelines, so the operator's custom config
// (different log file paths per host, custom processors, etc.)
// is preserved.
//
// Inputs:
//
//   - opampServerURL: ws://host:port/v1/opamp — required.
//   - hostname:       optional. When set, registered as a
//     non-identifying attribute "host.name". The OpAMP supervisor
//     reports the OS hostname by default, but having Squadron know
//     the canonical CMDB hostname (which sometimes differs) makes
//     the inventory reconciliation reliable.
//   - labels:         optional kv map. Each becomes a non-identifying
//     attribute on the agent. Use for environment / region / etc.
//
// Added in v0.45.0. The original OpAMPExtensionSnippet is preserved
// as a one-line caller of this with empty hostname + labels.
func AdoptionSnippet(opampServerURL string, hostname string, labels map[string]string) (string, error) {
	if opampServerURL == "" {
		return "", fmt.Errorf("opamp_server_url is required")
	}

	// Build the agent_description block conditionally. The OpAMP
	// extension in otelcol-contrib supports
	// `agent_description.non_identifying_attributes` as an arbitrary
	// kv map; Squadron renders these as labels on the agent card.
	var agentDesc string
	if hostname != "" || len(labels) > 0 {
		agentDesc = "    agent_description:\n"
		agentDesc += "      non_identifying_attributes:\n"
		if hostname != "" {
			agentDesc += fmt.Sprintf("        host.name: %q\n", hostname)
		}
		// Stable iteration order so the snippet is byte-deterministic
		// for the same inputs — important for diff-based tests and
		// for operators reviewing what changed.
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			agentDesc += fmt.Sprintf("        %s: %q\n", k, labels[k])
		}
	}

	header := `# Squadron adoption snippet — paste this into your existing
# collector config. Adds two things: the extension definition
# under extensions:, and a reference to it under
# service.extensions: so the collector actually starts it.
#
# Your existing receivers / processors / exporters / pipelines
# are NOT touched. Squadron registers this agent and starts
# managing it; your custom config (log paths, sampling, etc.)
# stays exactly as you have it.
#
# Restart the collector after merging. It will show up in
# Squadron's fleet view within a few seconds.`

	body := fmt.Sprintf(`
extensions:
  opamp:
    server:
      ws:
        endpoint: %s
    # capabilities the agent reports to Squadron. Conservative
    # defaults; Squadron honors all of them.
    capabilities:
      reports_effective_config: true
      reports_own_metrics: true
      reports_health: true
      accepts_remote_config: true
      accepts_packages: false
%s
service:
  # Append "opamp" to your existing service.extensions list if
  # you already have one (don't replace the list).
  extensions: [opamp]
`, opampServerURL, agentDesc)

	return strings.TrimSpace(header + "\n" + body), nil
}

// sortStrings is sort.Strings without pulling in the sort package
// for a single call site. Keeps the import footprint of this
// package small.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// HostAdoption is one entry in the bulk adoption batch — a hostname
// to target plus the labels to bake into that host's snippet.
type HostAdoption struct {
	Hostname string            `json:"hostname"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// BulkAdoptionPayload returns a single JSON document containing one
// snippet per host. The intended consumer is a customer-side GitHub
// Actions (or Ansible Tower / Azure DevOps) pipeline triggered by
// Squadron via workflow_dispatch with this payload as a single
// input. The pipeline parses, iterates, and applies each entry to
// the matching host without re-calling Squadron.
//
// JSON because the workflow_dispatch input system serializes well
// to JSON and most pipelines have native parsing. Operators who
// prefer YAML can run a yq one-liner; we'd rather not maintain two
// emitters.
//
// Shape:
//
//	{
//	  "opamp_server_url": "ws://squadron.example/v1/opamp",
//	  "hosts": [
//	    {"hostname": "host01.prod", "snippet": "<yaml>"},
//	    {"hostname": "host02.prod", "snippet": "<yaml>"}
//	  ]
//	}
//
// Each `snippet` is a complete merge-into-existing-config block —
// the receiving pipeline can `yq merge` it into the host's current
// config without further transformation.
//
// Added in v0.46.0 (bulk adoption via deploy pipeline).
func BulkAdoptionPayload(opampServerURL string, hosts []HostAdoption) (string, error) {
	if opampServerURL == "" {
		return "", fmt.Errorf("opamp_server_url is required")
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("at least one host is required")
	}

	type entry struct {
		Hostname string `json:"hostname"`
		Snippet  string `json:"snippet"`
	}
	out := struct {
		OpAMPServerURL string  `json:"opamp_server_url"`
		Hosts          []entry `json:"hosts"`
	}{OpAMPServerURL: opampServerURL}

	for _, h := range hosts {
		if h.Hostname == "" {
			// Skip rows without a hostname rather than erroring —
			// in practice an inventory row with no hostname is a
			// data bug we don't want to fail the whole batch on.
			continue
		}
		snippet, err := AdoptionSnippet(opampServerURL, h.Hostname, h.Labels)
		if err != nil {
			return "", fmt.Errorf("host %s: %w", h.Hostname, err)
		}
		out.Hosts = append(out.Hosts, entry{
			Hostname: h.Hostname,
			Snippet:  snippet,
		})
	}
	if len(out.Hosts) == 0 {
		return "", fmt.Errorf("no usable hosts in batch")
	}

	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// ----------------------------------------------------------------
// Starter templates
// ----------------------------------------------------------------

// starterTemplates are full, ready-to-use OTel Collector configs.
// Each contains:
//   - opamp extension pointed at {{OPAMP_SERVER_URL}}
//   - OTLP receivers (the standard ingest path)
//   - batch processor (the one processor every config should have)
//   - backend-specific exporter
//   - service.pipelines wiring traces/metrics/logs
//
// Keep them small. Production tuning (memory_limiter, sampling,
// resource detection) is the operator's job; the starter is a
// minimum-viable starting point.
var starterTemplates = map[Backend]string{
	BackendDatadog: `# Squadron starter config — Datadog
# Set DD_API_KEY in your environment before starting the collector.
extensions:
  opamp:
    server:
      ws:
        endpoint: {{OPAMP_SERVER_URL}}

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 10s
    send_batch_size: 1024

exporters:
  datadog:
    api:
      key: ${env:DD_API_KEY}
      site: datadoghq.com   # change to datadoghq.eu for EU, ap1.datadoghq.com for APAC

service:
  extensions: [opamp]
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [datadog]
    metrics:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [datadog]
    logs:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [datadog]
`,
	BackendHoneycomb: `# Squadron starter config — Honeycomb
# Set HONEYCOMB_API_KEY in your environment before starting.
extensions:
  opamp:
    server:
      ws:
        endpoint: {{OPAMP_SERVER_URL}}

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 10s
    send_batch_size: 1024

exporters:
  otlp/honeycomb:
    endpoint: api.honeycomb.io:443
    headers:
      x-honeycomb-team: ${env:HONEYCOMB_API_KEY}

service:
  extensions: [opamp]
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/honeycomb]
    metrics:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/honeycomb]
    logs:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/honeycomb]
`,
	BackendNewRelic: `# Squadron starter config — New Relic
# Set NEW_RELIC_LICENSE_KEY in your environment before starting.
extensions:
  opamp:
    server:
      ws:
        endpoint: {{OPAMP_SERVER_URL}}

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 10s
    send_batch_size: 1024

exporters:
  otlphttp/newrelic:
    endpoint: https://otlp.nr-data.net
    headers:
      api-key: ${env:NEW_RELIC_LICENSE_KEY}

service:
  extensions: [opamp]
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlphttp/newrelic]
    metrics:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlphttp/newrelic]
    logs:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlphttp/newrelic]
`,
	BackendSigNoz: `# Squadron starter config — SigNoz Cloud
# Set SIGNOZ_INGESTION_KEY in your environment before starting.
# For self-hosted SigNoz, swap the endpoint for your installation.
extensions:
  opamp:
    server:
      ws:
        endpoint: {{OPAMP_SERVER_URL}}

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 10s
    send_batch_size: 1024

exporters:
  otlp/signoz:
    endpoint: ingest.us.signoz.cloud:443   # change region as needed
    headers:
      signoz-access-token: ${env:SIGNOZ_INGESTION_KEY}
    tls:
      insecure: false

service:
  extensions: [opamp]
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/signoz]
    metrics:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/signoz]
    logs:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/signoz]
`,
	BackendGrafana: `# Squadron starter config — Grafana Cloud
# Set GRAFANA_OTLP_TOKEN in your environment before starting.
# Find the endpoint in your Grafana Cloud "Send data" page.
extensions:
  opamp:
    server:
      ws:
        endpoint: {{OPAMP_SERVER_URL}}

  basicauth/grafana:
    client_auth:
      username: ${env:GRAFANA_INSTANCE_ID}   # your numeric Grafana Cloud stack ID
      password: ${env:GRAFANA_OTLP_TOKEN}

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 10s
    send_batch_size: 1024

exporters:
  otlphttp/grafana:
    endpoint: ${env:GRAFANA_OTLP_ENDPOINT}   # e.g. https://otlp-gateway-prod-us-central-0.grafana.net/otlp
    auth:
      authenticator: basicauth/grafana

service:
  extensions: [opamp, basicauth/grafana]
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlphttp/grafana]
    metrics:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlphttp/grafana]
    logs:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlphttp/grafana]
`,
	BackendGenericOTLP: `# Squadron starter config — Generic OTLP
# Works with any OTel-compatible backend (Tempo, Mimir, Loki,
# self-hosted SigNoz, Jaeger via OTLP, custom collectors, etc.).
# Set OTLP_ENDPOINT (and any required auth headers) before starting.
extensions:
  opamp:
    server:
      ws:
        endpoint: {{OPAMP_SERVER_URL}}

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 10s
    send_batch_size: 1024

exporters:
  otlp/backend:
    endpoint: ${env:OTLP_ENDPOINT}   # e.g. backend.example.com:4317
    tls:
      insecure: false               # set true for plaintext local backends only
    # headers:
    #   authorization: "Bearer ${env:OTLP_TOKEN}"

service:
  extensions: [opamp]
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/backend]
    metrics:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/backend]
    logs:
      receivers:  [otlp]
      processors: [batch]
      exporters:  [otlp/backend]
`,
}

// backendInfoFor returns the public catalog entry for a backend.
// Hand-curated copy: short enough for a card, accurate enough for
// an operator to recognize their tool.
func backendInfoFor(b Backend) BackendInfo {
	switch b {
	case BackendDatadog:
		return BackendInfo{
			ID:          b,
			Name:        "Datadog",
			Description: "Sends traces, metrics, and logs to Datadog via the native datadog exporter.",
			DocsURL:     "https://docs.datadoghq.com/opentelemetry/",
			EnvVars: []EnvVar{
				{Name: "DD_API_KEY", Purpose: "Datadog API key", Required: true},
			},
		}
	case BackendHoneycomb:
		return BackendInfo{
			ID:          b,
			Name:        "Honeycomb",
			Description: "Sends all signals to Honeycomb via OTLP-over-gRPC.",
			DocsURL:     "https://docs.honeycomb.io/send-data/opentelemetry/",
			EnvVars: []EnvVar{
				{Name: "HONEYCOMB_API_KEY", Purpose: "Honeycomb ingest key", Required: true},
			},
		}
	case BackendNewRelic:
		return BackendInfo{
			ID:          b,
			Name:        "New Relic",
			Description: "Sends all signals to New Relic via OTLP-over-HTTP at otlp.nr-data.net.",
			DocsURL:     "https://docs.newrelic.com/docs/opentelemetry/",
			EnvVars: []EnvVar{
				{Name: "NEW_RELIC_LICENSE_KEY", Purpose: "New Relic license key", Required: true},
			},
		}
	case BackendSigNoz:
		return BackendInfo{
			ID:          b,
			Name:        "SigNoz Cloud",
			Description: "Sends all signals to SigNoz Cloud via OTLP-over-gRPC. Swap the endpoint for self-hosted.",
			DocsURL:     "https://signoz.io/docs/instrumentation/",
			EnvVars: []EnvVar{
				{Name: "SIGNOZ_INGESTION_KEY", Purpose: "SigNoz ingestion token", Required: true},
			},
		}
	case BackendGrafana:
		return BackendInfo{
			ID:          b,
			Name:        "Grafana Cloud",
			Description: "Sends all signals to Grafana Cloud via the OTLP gateway. Routes to Tempo / Mimir / Loki automatically.",
			DocsURL:     "https://grafana.com/docs/opentelemetry/",
			EnvVars: []EnvVar{
				{Name: "GRAFANA_INSTANCE_ID", Purpose: "Numeric Grafana Cloud stack ID", Required: true},
				{Name: "GRAFANA_OTLP_TOKEN", Purpose: "Grafana Cloud OTLP write token", Required: true},
				{Name: "GRAFANA_OTLP_ENDPOINT", Purpose: "From the 'Send data' page", Required: true},
			},
		}
	case BackendGenericOTLP:
		return BackendInfo{
			ID:          b,
			Name:        "Generic OTLP",
			Description: "Any OTel-compatible backend — Tempo, Mimir, self-hosted SigNoz, custom collectors, etc.",
			EnvVars: []EnvVar{
				{Name: "OTLP_ENDPOINT", Purpose: "Backend's OTLP endpoint (e.g. backend:4317)", Required: true},
			},
		}
	}
	return BackendInfo{ID: b, Name: string(b)}
}
