package telemetrystore

import (
	"github.com/devopsmike2/squadron/internal/config"
)

// Config represents the configuration for the telemetry store meta factory
type Config struct {
	Type string `yaml:"type"`
	Path string `yaml:"path"`
	// MemoryLimit is an optional DuckDB memory_limit (e.g. "4GB"); empty
	// leaves DuckDB's default (~80% of RAM) in place.
	MemoryLimit string `yaml:"memory_limit,omitempty"`
}

// ConfigFrom creates a Config from the app storage config
func ConfigFrom(appConfig *config.Config) Config {
	return Config{
		Type:        appConfig.Storage.Telemetry.Type,
		Path:        appConfig.Storage.Telemetry.Path,
		MemoryLimit: appConfig.Storage.Telemetry.MemoryLimit,
	}
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		Type: "duckdb",
		Path: "./data/telemetry.db",
	}
}
