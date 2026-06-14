package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the application configuration
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	OTLP      OTLPConfig      `yaml:"otlp"`
	Storage   StorageConfig   `yaml:"storage"`
	Retention RetentionConfig `yaml:"retention"`
	Rollups   RollupsConfig   `yaml:"rollups"`
	Logging   LoggingConfig   `yaml:"logging"`
	Worker    WorkerConfig    `yaml:"worker"`
	Auth      AuthConfig      `yaml:"auth"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
	AI        AIConfig        `yaml:"ai"`
	Pricing       PricingConfig       `yaml:"pricing"`
	SilentAgents  SilentAgentsConfig  `yaml:"silent_agents,omitempty"`
	Deploy        DeployConfig        `yaml:"deploy,omitempty"`
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
	Enabled  bool                 `yaml:"enabled"`
	Currency string               `yaml:"currency,omitempty"`
	Rules    []PricingRuleConfig  `yaml:"rules,omitempty"`
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
	Enabled      bool   `yaml:"enabled"`
	APIKey       string `yaml:"api_key"`
	APIKeyEnv    string `yaml:"api_key_env"`
	BaseURL      string `yaml:"base_url"`
	ExplainModel string `yaml:"explain_model"`
	MergeModel   string `yaml:"merge_model"`
	MaxTokens    int    `yaml:"max_tokens"`
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
	Enabled        bool             `yaml:"enabled"`         // master switch
	ServiceName    string           `yaml:"service_name"`    // resource attr; default "squadron"
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
	Endpoint string            `yaml:"endpoint"`  // e.g. "localhost:4317" or "https://api.honeycomb.io"
	Protocol string            `yaml:"protocol"`  // "grpc" (default) or "http"
	Headers  map[string]string `yaml:"headers"`   // forwarded on every export request
	Insecure bool              `yaml:"insecure"`  // skip TLS for grpc (local dev only)
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
	QueueSize int    `yaml:"queue_size"`
	Workers   int    `yaml:"workers"`
	Timeout   string `yaml:"timeout"` // Duration string like "5s", "1m"
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

	return &config, nil
}

// applyAIEnv fills config.AI.APIKey from the env var when the yaml
// didn't set one. Also enforces the default env var name when the
// operator didn't override it. Pure function over the AIConfig, so
// it's safe to call from tests with arbitrary inputs.
func applyAIEnv(c *AIConfig) {
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = "ANTHROPIC_API_KEY"
	}
	if c.APIKey == "" {
		c.APIKey = os.Getenv(c.APIKeyEnv)
	}
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
