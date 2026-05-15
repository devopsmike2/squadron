// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package configtemplates is the curated library of OpenTelemetry collector
// snippets that the UI exposes as a "Templates" dropdown in the config editor.
//
// Templates are intentionally complete (a user can copy one in and start a
// collector with it) but minimal — they show the shape of a use case, not
// every possible flag. Real-world configs get richer through the editor
// after the template is inserted.
package configtemplates

// Template is one snippet entry. ID is stable so the UI can deep-link to a
// template (?template=basic-otlp-relay) if we ever want preview-on-URL.
type Template struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"` // groups the picker UI
	YAML        string `json:"yaml"`
}

// All returns every template in display order.
func All() []Template {
	return []Template{
		{
			ID:          "basic-otlp-relay",
			Name:        "Basic OTLP relay",
			Description: "Receive OTLP, batch, forward to another OTLP endpoint. The starting point for most collectors.",
			Category:    "Common",
			YAML:        basicOTLPRelay,
		},
		{
			ID:          "memory-limited-otlp",
			Name:        "Memory-limited OTLP relay",
			Description: "Same as basic, with memory_limiter first so the collector drops data before OOMing under load.",
			Category:    "Common",
			YAML:        memoryLimitedRelay,
		},
		{
			ID:          "prometheus-scrape",
			Name:        "Prometheus scrape",
			Description: "Scrape a Prometheus endpoint and forward metrics over OTLP.",
			Category:    "Receivers",
			YAML:        prometheusScrape,
		},
		{
			ID:          "k8s-logs",
			Name:        "Kubernetes container logs",
			Description: "Tail container log files on a node, enrich with Kubernetes attributes, forward as OTLP logs.",
			Category:    "Receivers",
			YAML:        k8sLogs,
		},
		{
			ID:          "tail-sampling",
			Name:        "Tail-based trace sampling",
			Description: "Sample traces based on policies (error spans, slow spans, plus a baseline rate).",
			Category:    "Sampling",
			YAML:        tailSampling,
		},
		{
			ID:          "filter-pii",
			Name:        "PII redaction",
			Description: "Drop or hash attributes that commonly carry PII (email, ssn, phone) before they reach the backend.",
			Category:    "Processing",
			YAML:        filterPII,
		},
		{
			ID:          "honeycomb-export",
			Name:        "Export to Honeycomb",
			Description: "Forward traces, metrics, and logs to api.honeycomb.io via OTLP. Requires HONEYCOMB_API_KEY env var.",
			Category:    "Destinations",
			YAML:        honeycombExport,
		},
		{
			ID:          "grafana-cloud",
			Name:        "Export to Grafana Cloud",
			Description: "Forward telemetry to Grafana Cloud's OTLP endpoint using basic auth.",
			Category:    "Destinations",
			YAML:        grafanaCloudExport,
		},
	}
}

// Get returns a single template by ID, or nil if not found.
func Get(id string) *Template {
	for _, t := range All() {
		if t.ID == id {
			return &t
		}
	}
	return nil
}

const basicOTLPRelay = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:

exporters:
  otlp:
    endpoint: backend.observability.svc.cluster.local:4317
    tls:
      insecure: false

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
`

const memoryLimitedRelay = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
    spike_limit_mib: 128
  batch:
    send_batch_size: 1024
    timeout: 5s

exporters:
  otlp:
    endpoint: backend:4317
    sending_queue:
      enabled: true
      queue_size: 5000
    retry_on_failure:
      enabled: true
      initial_interval: 5s
      max_interval: 30s

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp]
    metrics:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp]
`

const prometheusScrape = `receivers:
  prometheus:
    config:
      scrape_configs:
        - job_name: 'my-service'
          scrape_interval: 30s
          static_configs:
            - targets: ['my-service:9090']

processors:
  batch:

exporters:
  otlp:
    endpoint: backend:4317

service:
  pipelines:
    metrics:
      receivers: [prometheus]
      processors: [batch]
      exporters: [otlp]
`

const k8sLogs = `receivers:
  filelog:
    include:
      - /var/log/pods/*/*/*.log
    start_at: end
    include_file_path: true
    operators:
      - type: container

processors:
  k8sattributes:
    auth_type: serviceAccount
    extract:
      metadata:
        - k8s.pod.name
        - k8s.namespace.name
        - k8s.deployment.name
        - k8s.node.name
  batch:

exporters:
  otlp:
    endpoint: backend:4317

service:
  pipelines:
    logs:
      receivers: [filelog]
      processors: [k8sattributes, batch]
      exporters: [otlp]
`

const tailSampling = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  tail_sampling:
    decision_wait: 10s
    num_traces: 100
    policies:
      - name: errors
        type: status_code
        status_code:
          status_codes: [ERROR]
      - name: slow
        type: latency
        latency:
          threshold_ms: 500
      - name: baseline
        type: probabilistic
        probabilistic:
          sampling_percentage: 10
  batch:

exporters:
  otlp:
    endpoint: backend:4317

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [tail_sampling, batch]
      exporters: [otlp]
`

const filterPII = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  attributes:
    actions:
      - key: user.email
        action: hash
      - key: user.ssn
        action: delete
      - key: user.phone
        action: delete
      - key: http.request.body
        action: delete
  batch:

exporters:
  otlp:
    endpoint: backend:4317

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [attributes, batch]
      exporters: [otlp]
    logs:
      receivers: [otlp]
      processors: [attributes, batch]
      exporters: [otlp]
`

const honeycombExport = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  batch:

exporters:
  otlp/honeycomb:
    endpoint: api.honeycomb.io:443
    headers:
      x-honeycomb-team: ${env:HONEYCOMB_API_KEY}

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/honeycomb]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/honeycomb]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/honeycomb]
`

const grafanaCloudExport = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  batch:

exporters:
  otlphttp/grafana:
    endpoint: ${env:GRAFANA_OTLP_ENDPOINT}
    auth:
      authenticator: basicauth/grafana

extensions:
  basicauth/grafana:
    client_auth:
      username: ${env:GRAFANA_INSTANCE_ID}
      password: ${env:GRAFANA_API_KEY}

service:
  extensions: [basicauth/grafana]
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/grafana]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/grafana]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/grafana]
`
