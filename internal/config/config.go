package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the application configuration
type Config struct {
	Server          ServerConfig          `yaml:"server"`
	OTLP            OTLPConfig            `yaml:"otlp"`
	Storage         StorageConfig         `yaml:"storage"`
	Retention       RetentionConfig       `yaml:"retention"`
	Rollups         RollupsConfig         `yaml:"rollups"`
	Logging         LoggingConfig         `yaml:"logging"`
	Worker          WorkerConfig          `yaml:"worker"`
	Auth            AuthConfig            `yaml:"auth"`
	Telemetry       TelemetryConfig       `yaml:"telemetry"`
	AI              AIConfig              `yaml:"ai"`
	Pricing         PricingConfig         `yaml:"pricing"`
	SilentAgents    SilentAgentsConfig    `yaml:"silent_agents,omitempty"`
	Deploy          DeployConfig          `yaml:"deploy,omitempty"`
	Billing         BillingConfig         `yaml:"billing,omitempty"`
	CostCorrelation CostCorrelationConfig `yaml:"cost_correlation,omitempty"`

	CommercialDetectors CommercialDetectorsConfig `yaml:"commercial_detectors,omitempty"`

	ServerlessMetricDetection ServerlessMetricDetectionConfig `yaml:"serverless_metric_detection,omitempty"`

	AuditRetention AuditRetentionConfig `yaml:"audit_retention,omitempty"`

	UsageReporting UsageReportingConfig `yaml:"usage_reporting,omitempty"`

	TraceIndex TraceIndexConfig `yaml:"trace_index,omitempty"`

	Ingest IngestConfig `yaml:"ingest,omitempty"`
}

// IngestConfig groups the operator-facing tuning for the ingest paths.
// Today it carries only the OTLP receiver's tenant binding (ADR 0012 §1),
// but it is a natural home for future per-listener ingest knobs.
type IngestConfig struct {
	OTLP OTLPIngestConfig `yaml:"otlp,omitempty"`
}

// OTLPIngestConfig binds this instance's OTLP ingest to a single tenant
// (ADR 0012 §1, "single tenant per instance"). The OTLP receiver
// (gRPC/HTTP) is unauthenticated and its worker pool mints a fresh
// context decoupled from the connection, so there is no per-request
// identity to derive a tenant from — the operator pins one here and it
// is threaded onto every WorkItem the receiver submits.
//
// Empty (the default) is treated as identity.DefaultTenant, so OSS —
// where multi-tenancy is inert and everything resolves to "default" —
// is unchanged. In the enterprise edition an empty value is the signal
// that OTLP ingest would silently land in the default tenant; 3d-5's
// strict-mode gate reads TenantID to fail fast on that misconfiguration.
type OTLPIngestConfig struct {
	// TenantID is the tenant every OTLP-ingested item is stamped with.
	// Empty => identity.DefaultTenant (inert in OSS).
	TenantID string `yaml:"tenant_id,omitempty"`
}

// AuditRetentionConfig is the operator-facing switch for pruning the
// audit_events log. OFF by default — audit_events is the append-only
// compliance/evidence log, so it grows unbounded unless an operator whose
// regime permits (or requires) a bounded window explicitly opts in. Unlike
// the other retention sweeps (which prune operator/discovery activity on a
// fixed 90-day cutoff), there is no safe universal default here: retention
// regimes vary widely (e.g. PCI-DSS ~1yr, HIPAA ~6yr, SOX ~7yr, plus GDPR
// erasure obligations), so the operator sets the window. Leaving the block
// out — or Enabled false — keeps the compliance-safe unbounded default.
type AuditRetentionConfig struct {
	// Enabled turns audit-log pruning ON. Default false: the audit log is
	// never pruned and grows unbounded.
	Enabled bool `yaml:"enabled"`
	// RetentionDays is the window kept when Enabled: events whose timestamp
	// is older than now-RetentionDays are pruned on a daily sweep. Must be
	// > 0 to take effect — Enabled with a non-positive value is treated as
	// INACTIVE so a misconfiguration can never silently wipe the whole log.
	RetentionDays int `yaml:"retention_days"`
}

// RetentionWindow returns the audit-log retention window and whether pruning
// is active. Active only when Enabled AND RetentionDays > 0, so an
// enabled-but-zero-day config safely no-ops instead of deleting everything.
func (c AuditRetentionConfig) RetentionWindow() (time.Duration, bool) {
	if !c.Enabled || c.RetentionDays <= 0 {
		return 0, false
	}
	return time.Duration(c.RetentionDays) * 24 * time.Hour, true
}

// ServerlessMetricDetectionConfig is the operator-facing switch for the
// natively-available serverless metric detectors — cold-start latency
// regression (24h vs 168h P95) and error-rate spike (24h vs 168h ratio) —
// on the surfaces where the signal lives in a NATIVE cloud metric and needs
// no paid add-on: AWS Lambda error-rate (AWS/Lambda Errors + Invocations),
// GCP Cloud Run / Functions cold-start + error-rate (Cloud Monitoring), and
// OCI Functions cold-start + error-rate (OCI Monitoring).
//
// OFF by default. Activating it constructs a per-cloud metric client and, on
// every scan, issues per-serverless-resource metric API reads — on AWS those
// are CloudWatch GetMetricStatistics calls, which are billed per request.
// Cloud Monitoring / OCI Monitoring have free tiers then bill. This is the
// single switch that flips these detectors from plumbed-but-dormant (the
// metric client is never constructed in the stock factories) to live, and it
// keeps that per-scan cost in the operator's hands — mirroring
// CommercialDetectorsConfig and CostCorrelationConfig.
//
// SCOPE — what this does NOT cover: the add-on-dependent detectors stay under
// commercial_detectors, because they need a paid telemetry add-on rather than
// a native metric: AWS Lambda COLD-START (Lambda Insights init_duration) and
// ALL Azure Functions detection (Application Insights). Those remain gated on
// CommercialDetectors.Enabled. This flag is strictly the native-metric subset.
type ServerlessMetricDetectionConfig struct {
	// Enabled constructs the per-cloud serverless metric client and runs the
	// native-metric cold-start + error-rate detectors. Default false (OSS):
	// the detectors stay dormant and the metric client is never built.
	Enabled bool `yaml:"enabled"`
}

// CommercialDetectorsConfig is the operator-facing switch for the
// add-on-dependent regression detectors that are part of the future
// commercial tier (#152 AWS Lambda cold-start via Lambda Insights;
// #153 Azure Functions cold-start + error via Application Insights).
// OFF by default: in OSS these detectors stay dormant and Squadron
// instead surfaces the gap by recommending the operator enable the
// paid add-on (lambda-insights-enable / azfunc-appinsights-enable).
//
// Setting Enabled=true is the explicit decision to run the regression
// detectors against the add-on telemetry. It does NOT enable the
// add-ons (those are paid features the operator turns on in their
// cloud account) and it does NOT change OSS metric availability — it
// only re-points the existing detector queries at the namespaces
// (Lambda Insights / Application Insights) where the cold-start +
// error signals actually live, and wires the observation stores the
// detection branch persists to. When the add-ons are absent the
// detectors read empty datapoints and stay silent, exactly as in OSS.
//
// This is the single switch that flips these detectors from
// plumbed-but-dormant to live, mirroring CostCorrelationConfig.
type CommercialDetectorsConfig struct {
	// Enabled turns the commercial-tier regression detectors on.
	// Default false (OSS) — the detectors stay dormant and OSS
	// recommends enabling the add-on instead.
	Enabled bool `yaml:"enabled"`
}

// BillingConfig wires the v0.42 billing connectors (Splunk for now;
// Datadog / Honeycomb / New Relic slot in here later). All sections
// are optional — omit any block and the connector silently disables.
type BillingConfig struct {
	Splunk SplunkBillingConfig `yaml:"splunk,omitempty"`
}

// SplunkBillingConfig is the per-deployment Splunk billing wiring.
// Maps onto billing.SplunkConfig in main.go after env var expansion.
type SplunkBillingConfig struct {
	Enabled            bool   `yaml:"enabled"`
	SearchHead         string `yaml:"search_head"`
	Token              string `yaml:"token"`
	WindowDays         int    `yaml:"window_days,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify,omitempty"`
}

// CostCorrelationConfig is the operator-facing opt-in for the
// cost-correlation substrate (cost-correlation slice 6). It is OFF by
// default: omitting the block, or leaving Enabled false, means
// Squadron makes ZERO cost-reporting API calls and incurs ZERO
// spend on the connected account. Setting Enabled=true is the
// explicit decision to let Squadron read cost data — which on AWS
// Cost Explorer costs ~$0.01 per request (GCP/Azure/OCI cost reads
// are free). The per-account spend is bounded by the budget governor
// at MonthlyBudgetUSD (default $1.00 / 30-day window).
//
// This is the single switch that flips the cost path from
// plumbed-but-dormant to live. The scan orchestrator calls the
// per-cloud Scanner's EnableCostCorrelation only when Enabled is
// true.
type CostCorrelationConfig struct {
	// Enabled turns cost correlation on. Default false (off) — the
	// safe default: no cost API calls, no spend.
	Enabled bool `yaml:"enabled"`

	// MonthlyBudgetUSD caps per-account spend on charged cost APIs
	// over a rolling 30-day window. Defaults to $1.00 when unset
	// (see EffectiveMonthlyBudgetUSD). The budget governor rejects
	// any cost call that would exceed it.
	MonthlyBudgetUSD float64 `yaml:"monthly_budget_usd,omitempty"`
}

// EffectiveMonthlyBudgetUSD returns the configured per-account
// monthly cost budget in USD, defaulting to $1.00 when the operator
// left it unset (or non-positive). The default is deliberately
// conservative.
func (c CostCorrelationConfig) EffectiveMonthlyBudgetUSD() float64 {
	if c.MonthlyBudgetUSD <= 0 {
		return 1.0
	}
	return c.MonthlyBudgetUSD
}

// DeployConfig is the v0.35+ deploy integration tuning. The deploy
// feature itself is gated by SQUADRON_DEPLOY_KEY at the env-var
// level (so it can't accidentally be enabled by a yaml typo without
// the operator also setting up the encryption key). This block is
// for the optional knobs.
type DeployConfig struct {
	// CompletionWebhookURL fires a JSON event on every deploy
	// terminal-state transition (success / failure / cancelled).
	// Reuses the v0.33 silent-agent webhook receiver shape — same
	// receiver handles both, keyed on the `kind` field.
	CompletionWebhookURL string `yaml:"completion_webhook_url,omitempty"`
}

// SilentAgentsConfig controls the v0.33 silent-agent watcher. The
// watcher polls the agents table on a fixed interval and fires
// webhook notifications when an agent's last_seen falls outside the
// silence threshold (and again when it recovers).
//
// Defaults: enabled=false, silence_threshold=10m, poll_interval=60s.
// Disabled by default so existing deployments don't suddenly spam
// a webhook URL the operator hasn't tested.
type SilentAgentsConfig struct {
	Enabled          bool          `yaml:"enabled"`
	SilenceThreshold time.Duration `yaml:"silence_threshold,omitempty"`
	PollInterval     time.Duration `yaml:"poll_interval,omitempty"`
	WebhookURL       string        `yaml:"webhook_url,omitempty"`
	// v0.43 — vendor-aware webhook formatting. Empty / "generic"
	// preserves the legacy plain-JSON shape so existing receivers
	// don't break on upgrade. Supported: "slack", "teams",
	// "pagerduty", "opsgenie", "discord" (v0.62), "generic".
	DestinationType string `yaml:"destination_type,omitempty"`
	// DestinationExtra is vendor-specific config. For pagerduty,
	// set routing_key. For opsgenie, set api_key (and optionally
	// region: "us" or "eu"). Slack/Teams/Discord need only WebhookURL.
	DestinationExtra map[string]string `yaml:"destination_extra,omitempty"`
	// PublicBaseURL is the externally-reachable Squadron base URL.
	// Used to build "Open in Squadron" deep links inside vendor-
	// formatted messages. Leave empty in dev — the link is just
	// dropped from the body.
	PublicBaseURL string `yaml:"public_base_url,omitempty"`
}

// PricingConfig is the v0.27 dollar-projection layer. Disabled by
// default so existing v0.24/v0.25 deployments don't suddenly show
// $0 hero numbers (which look broken) — operators opt in once
// they've tuned the rules against their own invoice.
//
// When Enabled is true and Rules is empty, Squadron uses
// pricing.DefaultConfig — a conservative starter rule set with
// per-destination rates for Datadog, Honeycomb, New Relic, SigNoz,
// Grafana Cloud, Splunk, plus a $0.30/GB catch-all.
//
// See docs/savings.md for the full pricing rule shape, the default
// rates' rationale, and how to tune them.
type PricingConfig struct {
	Enabled  bool                `yaml:"enabled"`
	Currency string              `yaml:"currency,omitempty"`
	Rules    []PricingRuleConfig `yaml:"rules,omitempty"`
}

// PricingRuleConfig mirrors pricing.Rule but lives in the config
// package so callers don't have to import internal/pricing just to
// load the yaml. main.go converts between the two types.
type PricingRuleConfig struct {
	Match      string  `yaml:"match"`
	Label      string  `yaml:"label,omitempty"`
	PricePerGB float64 `yaml:"price_per_gb"`
	Traces     float64 `yaml:"traces,omitempty"`
	Metrics    float64 `yaml:"metrics,omitempty"`
	Logs       float64 `yaml:"logs,omitempty"`
}

// AIConfig controls v0.26+ AI-assist features (Anthropic Messages
// API). Disabled by default. APIKey resolves in this order:
//  1. AIConfig.APIKey from the yaml file (if set)
//  2. The env var named by APIKeyEnv (default: ANTHROPIC_API_KEY)
//  3. Empty — the service runs but every call returns ErrDisabled
//     and the UI hides AI affordances.
//
// Model names default to constants in the ai package so a model
// migration is one file. BaseURL is overridable for self-hosted
// gateways or mock servers in CI; defaults to api.anthropic.com.
type AIConfig struct {
	Enabled bool `yaml:"enabled"`
	// Provider selects the LLM backend. Empty or "anthropic" (default)
	// keeps the historical Anthropic Messages path; "openai" uses the
	// OpenAI-compatible Chat Completions path (also covers Azure
	// OpenAI, Gemini's OpenAI endpoint, Mistral, and local
	// Ollama/vLLM/LM Studio via BaseURL).
	Provider     string `yaml:"provider"`
	APIKey       string `yaml:"api_key"`
	APIKeyEnv    string `yaml:"api_key_env"`
	BaseURL      string `yaml:"base_url"`
	ExplainModel string `yaml:"explain_model"`
	MergeModel   string `yaml:"merge_model"`
	MaxTokens    int    `yaml:"max_tokens"`
	// Models is an optional per-capability model override map, passed
	// through to the ai package. Optional; nil is fine.
	Models map[string]string `yaml:"models,omitempty"`
}

// TelemetryConfig controls Squadron's self-monitoring: when enabled,
// Squadron emits its own audit events as OpenTelemetry traces to a
// configurable OTLP endpoint. Default: disabled. Configure to point at
// your existing telemetry stack (the same one you're using Squadron to
// manage the collectors for, if you like).
//
// This is intentionally separate from the OTLP receiver config — the
// receiver accepts telemetry FROM agents, this section emits telemetry
// TO somewhere else.
type TelemetryConfig struct {
	Enabled        bool             `yaml:"enabled"`      // master switch
	ServiceName    string           `yaml:"service_name"` // resource attr; default "squadron"
	OTLP           OTLPExportConfig `yaml:"otlp"`
	MetricInterval time.Duration    `yaml:"metric_interval"` // bridge scrape cadence; default 30s. Matches the Prom-scrape cadence operators typically already run.
}

// OTLPExportConfig points at the OTLP endpoint Squadron exports to.
//
// Endpoint must be set when Telemetry.Enabled is true. Protocol picks
// between OTLP gRPC (the standard) and OTLP HTTP. Headers is forwarded
// verbatim — typically a bearer token if your destination requires
// auth (e.g. SigNoz Cloud, Honeycomb, Datadog OTLP).
type OTLPExportConfig struct {
	Endpoint string            `yaml:"endpoint"` // e.g. "localhost:4317" or "https://api.honeycomb.io"
	Protocol string            `yaml:"protocol"` // "grpc" (default) or "http"
	Headers  map[string]string `yaml:"headers"`  // forwarded on every export request
	Insecure bool              `yaml:"insecure"` // skip TLS for grpc (local dev only)
}

// AuthConfig controls API authentication. When enabled, every
// /api/v1/* request must carry a valid Bearer token. /metrics and
// /health stay public regardless. Defaults to disabled — turn it on
// before exposing Squadron beyond a trusted network.
type AuthConfig struct {
	Enabled bool `yaml:"enabled"`
}

// ServerConfig contains server configuration
type ServerConfig struct {
	HTTPPort  int `yaml:"http_port"`
	OpAMPPort int `yaml:"opamp_port"`
}

// OTLPConfig contains OTLP receiver configuration
type OTLPConfig struct {
	GRPCEndpoint      string `yaml:"grpc_endpoint"`
	HTTPEndpoint      string `yaml:"http_endpoint"`
	AgentGRPCEndpoint string `yaml:"agent_grpc_endpoint"` // Endpoint to offer to agents (if different from grpc_endpoint)
	AgentHTTPEndpoint string `yaml:"agent_http_endpoint"` // Endpoint to offer to agents (if different from http_endpoint)
}

// StorageConfig contains storage configuration
type StorageConfig struct {
	App       AppStorageConfig       `yaml:"app"`
	Telemetry TelemetryStorageConfig `yaml:"telemetry"`
}

// AppStorageConfig contains app storage configuration
type AppStorageConfig struct {
	Type string `yaml:"type"`
	Path string `yaml:"path"`
}

// TelemetryStorageConfig contains telemetry storage configuration
type TelemetryStorageConfig struct {
	Type string `yaml:"type"`
	Path string `yaml:"path"`
	// MemoryLimit is an optional DuckDB memory_limit (e.g. "4GB" or "75%").
	// Empty leaves DuckDB's default in place (~80% of host RAM). Operators
	// should cap this to bound RSS on shared or memory-constrained hosts.
	MemoryLimit string `yaml:"memory_limit,omitempty"`
}

// RetentionConfig contains data retention configuration
type RetentionConfig struct {
	RawMetrics string `yaml:"raw_metrics"`
	RawLogs    string `yaml:"raw_logs"`
	Rollups1m  string `yaml:"rollups_1m"`
	Rollups5m  string `yaml:"rollups_5m"`
}

// RollupsConfig contains rollup configuration
type RollupsConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Interval1m string `yaml:"interval_1m"`
	Interval5m string `yaml:"interval_5m"`
}

// LoggingConfig contains logging configuration
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// WorkerConfig contains worker pool configuration
type WorkerConfig struct {
	QueueSize int `yaml:"queue_size"`
	// MaxQueueBytes bounds the volatile ack'd-but-unwritten backlog in DATA
	// (Σ raw OTLP payload bytes queued), the real memory ceiling — queue_size
	// only caps request COUNT, whose per-request size varies wildly, so a
	// burst of large batches could hold ~500k items volatile. Item counts
	// aren't known until the worker parses, so bytes are the only cheap signal
	// at ingest. 0/unset => 256 MiB default (applied in cmd/all-in-one);
	// negative => unbounded (request-count cap only).
	MaxQueueBytes int    `yaml:"max_queue_bytes,omitempty"`
	Workers       int    `yaml:"workers"`
	Timeout       string `yaml:"timeout"` // Duration string like "5s", "1m"
}

// LoadConfig loads configuration from a YAML file
func LoadConfig(path string) (*Config, error) {
	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Parse YAML
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// v0.26 AI config — resolve API key from env if the yaml didn't
	// set one. The env-var path is the recommended one (keeps the
	// key out of squadron.yaml + version control); the yaml field
	// exists for dev convenience and forward-compat with a future
	// settings page that writes the file.
	applyAIEnv(&config.AI)
	applyLoggingEnv(&config.Logging)
	applyUsageEnv(&config.UsageReporting)

	return &config, nil
}

// applyLoggingEnv lets an operator override the log level/format at container
// start without editing squadron.yaml — the knob a self-hoster reaches for first
// when debugging. SQUADRON_LOG_LEVEL / SQUADRON_LOG_FORMAT win; the unprefixed
// LOG_LEVEL / LOG_FORMAT are also honored because the shipped docker-compose sets
// those (previously they were silently ignored). Empty/unset leaves the yaml (or
// default) value intact. Pure function over LoggingConfig for testability.
func applyLoggingEnv(c *LoggingConfig) {
	if v := firstNonEmptyEnv("SQUADRON_LOG_LEVEL", "LOG_LEVEL"); v != "" {
		c.Level = v
	}
	if v := firstNonEmptyEnv("SQUADRON_LOG_FORMAT", "LOG_FORMAT"); v != "" {
		c.Format = v
	}
}

// firstNonEmptyEnv returns the value of the first env var in names that is set
// and non-empty, or "" if none are.
func firstNonEmptyEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// applyAIEnv fills config.AI.APIKey from the env var when the yaml
// didn't set one. Also enforces the default env var name when the
// operator didn't override it. Pure function over the AIConfig, so
// it's safe to call from tests with arbitrary inputs.
func applyAIEnv(c *AIConfig) {
	// Provider + endpoint env overrides. Unset leaves the yaml value
	// intact, so the ANTHROPIC-only default path (no SQUADRON_AI_* vars,
	// just ANTHROPIC_API_KEY) is untouched.
	if v := firstNonEmptyEnv("SQUADRON_AI_PROVIDER"); v != "" {
		c.Provider = v
	}
	if v := firstNonEmptyEnv("SQUADRON_AI_BASE_URL"); v != "" {
		c.BaseURL = v
	}

	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "openai":
		// OpenAI-family key resolution + model defaults. Kept entirely
		// separate from the Anthropic default so it can never disturb it.
		if c.APIKeyEnv == "" {
			c.APIKeyEnv = "OPENAI_API_KEY"
		}
		if c.APIKey == "" {
			c.APIKey = firstNonEmptyEnv("SQUADRON_AI_API_KEY", c.APIKeyEnv, "OPENAI_API_KEY")
		}
		if c.ExplainModel == "" {
			c.ExplainModel = "gpt-4o-mini"
		}
		if c.MergeModel == "" {
			c.MergeModel = "gpt-4o"
		}
	default: // "" or "anthropic" — historical behavior, byte-for-byte.
		if c.APIKeyEnv == "" {
			c.APIKeyEnv = "ANTHROPIC_API_KEY"
		}
		if c.APIKey == "" {
			c.APIKey = os.Getenv(c.APIKeyEnv)
		}
	}

	// SQUADRON_AI_MODEL is a convenience single-model override (handy for
	// local single-model servers). Explicit opt-in, so it wins when set;
	// unset leaves the resolved/default models alone.
	if v := firstNonEmptyEnv("SQUADRON_AI_MODEL"); v != "" {
		c.ExplainModel = v
		c.MergeModel = v
	}
}

// UsageReportingConfig is the operator-facing switch for ANONYMOUS, aggregate
// usage reporting (a periodic "phone-home"). OFF by default — Squadron reports
// nothing unless an operator opts in. When enabled it sends only anonymized
// aggregate COUNTS (Squadron version + edition; tallies such as the number of
// agents and rollouts) to Endpoint — no tenant/host/account identifiers, no
// config or resource content. Distinct from the `telemetry:` block
// (TelemetryConfig / internal/selftel), which exports THIS instance's own
// operational metrics to the operator's OWN OTLP backend.
type UsageReportingConfig struct {
	// Enabled turns anonymous usage reporting ON. Default false.
	Enabled bool `yaml:"enabled"`
	// Endpoint is the HTTPS URL the aggregate report is POSTed to. Empty (the
	// default) disables reporting even if Enabled — a no-op, never a silent send
	// to an unintended target.
	Endpoint string `yaml:"endpoint,omitempty"`
	// IntervalHours is the reporting cadence; <= 0 defaults to 24h.
	IntervalHours int `yaml:"interval_hours,omitempty"`
}

// Target returns the endpoint + cadence and whether usage reporting is active.
// Active only when Enabled AND Endpoint is set, so an enabled-but-endpointless
// config safely no-ops instead of reporting nowhere.
func (c UsageReportingConfig) Target() (string, time.Duration, bool) {
	endpoint := strings.TrimSpace(c.Endpoint)
	if !c.Enabled || endpoint == "" {
		return "", 0, false
	}
	interval := time.Duration(c.IntervalHours) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return endpoint, interval, true
}

// applyUsageEnv lets an operator enable + point anonymous usage reporting via env
// (SQUADRON_USAGE_ENABLED / SQUADRON_USAGE_ENDPOINT) without editing the yaml.
// Empty/unset leaves the yaml value intact. Pure function over the config.
func applyUsageEnv(c *UsageReportingConfig) {
	if v := firstNonEmptyEnv("SQUADRON_USAGE_ENABLED"); v != "" {
		c.Enabled = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	if v := firstNonEmptyEnv("SQUADRON_USAGE_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
}

// TraceIndexConfig carries per-tenant trace-index (trace_resource_seen) LRU
// budgets (ADR 0024). PerTenantMaxRows maps tenant id → max index rows for that
// tenant; tenants absent from the map use the global SQUADRON_TRACEINDEX_MAX_ROWS
// cap. Inert in OSS — only the enterprise build wires a provider that reads it;
// the OSS build ignores it and keeps the global cap for every tenant.
type TraceIndexConfig struct {
	PerTenantMaxRows map[string]int `yaml:"per_tenant_max_rows,omitempty"`
}

// DefaultConfig returns default configuration
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			HTTPPort:  8080,
			OpAMPPort: 4320,
		},
		OTLP: OTLPConfig{
			GRPCEndpoint: "0.0.0.0:4317",
			HTTPEndpoint: "0.0.0.0:4318",
		},
		Storage: StorageConfig{
			App: AppStorageConfig{
				Type: "sqlite",
				Path: "./data/app.db",
			},
			Telemetry: TelemetryStorageConfig{
				Type: "duckdb",
				Path: "./data/telemetry.db",
			},
		},
		Retention: RetentionConfig{
			RawMetrics: "24h",
			RawLogs:    "24h",
			Rollups1m:  "7d",
			Rollups5m:  "30d",
		},
		Rollups: RollupsConfig{
			Enabled:    true,
			Interval1m: "*/1 * * * *",
			Interval5m: "*/5 * * * *",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		Worker: WorkerConfig{
			QueueSize: 10000,
			Workers:   3,
			Timeout:   "5s",
		},
	}
}

// ParseDuration parses a duration string like "24h", "7d", "30d"
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration format: %s", s)
	}

	unit := s[len(s)-1:]
	value := s[:len(s)-1]

	var duration time.Duration
	switch unit {
	case "h":
		d, err := time.ParseDuration(value + "h")
		if err != nil {
			return 0, err
		}
		duration = d
	case "d":
		// Parse days as integer
		var days int
		if _, err := fmt.Sscanf(value, "%d", &days); err != nil {
			return 0, fmt.Errorf("invalid day value: %s", value)
		}
		duration = time.Duration(days*24) * time.Hour
	default:
		return time.ParseDuration(s)
	}

	return duration, nil
}
