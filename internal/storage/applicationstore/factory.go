package applicationstore

import (
	"fmt"
	"io"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/config"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/postgres"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

const (
	sqliteStorageType   = "sqlite"
	memoryStorageType   = "memory"
	postgresStorageType = "postgres"
)

// AllStorageTypes defines all available application storage backends.
//
// Both sqlite (the zero-dependency default) and postgres (the opt-in HA /
// org-scale backend, ADR 0033) are OSS-selectable. The open-core boundary
// (ADR 0033 / ADR 0001) keeps the backend + DSN seam in OSS; only the
// HA/multi-replica operability layer is reserved for Enterprise.
var AllStorageTypes = []string{
	sqliteStorageType,
	memoryStorageType,
	postgresStorageType,
}

// Factory implements ApplicationStoreFactory interface as a meta-factory for application storage components.
// It provides a clean abstraction layer over concrete storage implementations, allowing easy switching
// between different storage backends (SQLite, Memory, PostgreSQL, etc.) without changing the main application code.
type Factory struct {
	Config    FactoryConfig
	logger    *zap.Logger
	factories map[string]ApplicationStoreFactory
}

// NewFactory creates the meta-factory.
// It automatically creates and registers the factory for the configured storage type.
// Example usage:
//
//	config := applicationstore.ConfigFrom(appConfig)
//	factory, err := applicationstore.NewFactory(config)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer factory.Close()
func NewFactory(config FactoryConfig) (*Factory, error) {
	f := &Factory{Config: config}
	f.factories = make(map[string]ApplicationStoreFactory)

	// Validate storage type
	if !IsStorageTypeSupported(config.Type) {
		return nil, fmt.Errorf("unsupported storage type %s. Supported types: %v", config.Type, AllStorageTypes)
	}

	// Initialize the factory for the configured storage type
	factory, err := f.getFactoryOfType(config.Type)
	if err != nil {
		return nil, fmt.Errorf("failed to create factory for storage type %s: %w", config.Type, err)
	}
	f.factories[config.Type] = factory

	return f, nil
}

// getFactoryOfType creates a factory instance for the given storage type
// To add a new storage type:
// 1. Add a constant for the storage type (e.g., postgresStorageType = "postgres")
// 2. Add it to AllStorageTypes slice
// 3. Add a case in this switch statement
// 4. Implement the ApplicationStoreFactory interface for your storage type
func (f *Factory) getFactoryOfType(factoryType string) (ApplicationStoreFactory, error) {
	switch factoryType {
	case sqliteStorageType:
		return sqlite.NewFactory(f.Config.Path), nil
	case memoryStorageType:
		return memory.NewFactory(), nil
	case postgresStorageType:
		// Postgres requires a DSN. Fail fast with a specific error rather
		// than falling back to another backend — a deployment that asked
		// for Postgres must get Postgres or a clear error, never a silent
		// SQLite store on a different (or empty) dataset.
		if f.Config.DSN == "" {
			return nil, fmt.Errorf("storage type %q requires storage.app.dsn to be set (the Postgres connection string, e.g. postgres://user:pass@host:5432/squadron?sslmode=require)", postgresStorageType)
		}
		return postgres.NewFactory(f.Config.DSN), nil
	default:
		return nil, fmt.Errorf("unknown application storage type %s. Valid types are %v", factoryType, AllStorageTypes)
	}
}

// Initialize initializes the meta factory and all underlying factories
func (f *Factory) Initialize(logger *zap.Logger) error {
	f.logger = logger

	// Initialize all registered factories
	for storageType, factory := range f.factories {
		if err := factory.Initialize(logger); err != nil {
			return fmt.Errorf("failed to initialize %s factory: %w", storageType, err)
		}
	}

	return nil
}

// CreateApplicationStore creates an application store using the configured storage type
func (f *Factory) CreateApplicationStore() (types.ApplicationStore, error) {
	factory, ok := f.factories[f.Config.Type]
	if !ok {
		return nil, fmt.Errorf("no %s backend registered for application store", f.Config.Type)
	}
	return factory.CreateApplicationStore()
}

// Close closes all underlying factories
func (f *Factory) Close() error {
	var errs []error
	for storageType, factory := range f.factories {
		if closer, ok := factory.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("failed to close %s factory: %w", storageType, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing factories: %v", errs)
	}

	return nil
}

// GetStorageType returns the configured storage type
func (f *Factory) GetStorageType() string {
	return f.Config.Type
}

// IsStorageTypeSupported checks if a storage type is supported
func IsStorageTypeSupported(storageType string) bool {
	for _, supportedType := range AllStorageTypes {
		if supportedType == storageType {
			return true
		}
	}
	return false
}

// NewFactoryFromAppConfig creates a new factory directly from app configuration
// This is a convenience function that combines ConfigFrom and NewFactory
func NewFactoryFromAppConfig(appConfig *config.Config) (*Factory, error) {
	config := ConfigFrom(appConfig)
	return NewFactory(config)
}
