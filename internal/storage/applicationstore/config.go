package applicationstore

import (
	"github.com/devopsmike2/squadron/internal/config"
)

// FactoryConfig represents the configuration for the application store meta factory.
// Path is used by the sqlite backend; DSN is the connection string used by the
// postgres backend (decisions/0033). Only one applies, per Type.
type FactoryConfig struct {
	Type string `yaml:"type"`
	Path string `yaml:"path"`
	DSN  string `yaml:"dsn"`
}

// ConfigFrom creates a FactoryConfig from the app storage config
func ConfigFrom(appConfig *config.Config) FactoryConfig {
	return FactoryConfig{
		Type: appConfig.Storage.App.Type,
		Path: appConfig.Storage.App.Path,
		DSN:  appConfig.Storage.App.DSN,
	}
}

// DefaultConfig returns a default configuration
func DefaultConfig() FactoryConfig {
	return FactoryConfig{
		Type: "sqlite",
		Path: "./data/app.db",
	}
}
