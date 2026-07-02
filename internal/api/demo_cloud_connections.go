package api

import (
	"context"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
)

// enableCloudDemoConnections registers a demo connection in each per-cloud
// discovery store (GCP / Azure / OCI) so the one-click demo populates all four
// Discovery pages, not just AWS. Each connection carries the reserved demo
// project/subscription/tenancy id, which makes the scan + recommendations
// handlers short-circuit to canned sample data — no real cloud call, no unseal.
// The sealed-credential fields carry placeholders: the demo scan path returns
// before any unseal, so they're never used. Best-effort + idempotent: a store
// that's unwired or an already-present demo connection is skipped, and a failure
// here never fails the demo enable (the fleet + AWS discovery are already up).
func (s *Server) enableCloudDemoConnections(ctx context.Context) {
	if s.discoveryGCPStore != nil && !gcpDemoConnExists(ctx, s.discoveryGCPStore) {
		if err := s.discoveryGCPStore.Create(ctx, &gcpconnstore.GCPConnection{
			DisplayName: "Demo GCP (sample data)",
			ProjectID:   demo.GCPProjectID,
			SealedSA:    []byte("demo-placeholder"),
		}); err != nil && s.logger != nil {
			s.logger.Warn("demo: register GCP demo connection failed", zap.Error(err))
		}
	}
	if s.discoveryAzureStore != nil && !azureDemoConnExists(ctx, s.discoveryAzureStore) {
		if err := s.discoveryAzureStore.Create(ctx, &azureconnstore.AzureConnection{
			DisplayName:    "Demo Azure (sample data)",
			TenantID:       "demo-tenant",
			SubscriptionID: demo.AzureSubscriptionID,
			ClientID:       "demo-client",
			SealedSecret:   []byte("demo-placeholder"),
		}); err != nil && s.logger != nil {
			s.logger.Warn("demo: register Azure demo connection failed", zap.Error(err))
		}
	}
	if s.discoveryOCIStore != nil && !ociDemoConnExists(ctx, s.discoveryOCIStore) {
		if err := s.discoveryOCIStore.Create(ctx, &ociconnstore.OCIConnection{
			DisplayName:      "Demo OCI (sample data)",
			TenancyOCID:      demo.OCITenancyOCID,
			UserOCID:         "ocid1.user.oc1..demo",
			Fingerprint:      "demo",
			SealedPrivateKey: []byte("demo-placeholder"),
			Region:           "us-ashburn-1",
		}); err != nil && s.logger != nil {
			s.logger.Warn("demo: register OCI demo connection failed", zap.Error(err))
		}
	}
}

// removeCloudDemoConnections deletes the demo connections created above,
// matching on the reserved demo project/subscription/tenancy id. Best-effort.
func (s *Server) removeCloudDemoConnections(ctx context.Context) {
	if s.discoveryGCPStore != nil {
		if conns, err := s.discoveryGCPStore.List(ctx); err == nil {
			for _, c := range conns {
				if c.ProjectID == demo.GCPProjectID {
					_ = s.discoveryGCPStore.Delete(ctx, c.ID)
				}
			}
		}
	}
	if s.discoveryAzureStore != nil {
		if conns, err := s.discoveryAzureStore.List(ctx); err == nil {
			for _, c := range conns {
				if c.SubscriptionID == demo.AzureSubscriptionID {
					_ = s.discoveryAzureStore.Delete(ctx, c.ID)
				}
			}
		}
	}
	if s.discoveryOCIStore != nil {
		if conns, err := s.discoveryOCIStore.List(ctx); err == nil {
			for _, c := range conns {
				if c.TenancyOCID == demo.OCITenancyOCID {
					_ = s.discoveryOCIStore.Delete(ctx, c.ID)
				}
			}
		}
	}
}

func gcpDemoConnExists(ctx context.Context, store gcpconnstore.Store) bool {
	conns, err := store.List(ctx)
	if err != nil {
		return false
	}
	for _, c := range conns {
		if c.ProjectID == demo.GCPProjectID {
			return true
		}
	}
	return false
}

func azureDemoConnExists(ctx context.Context, store azureconnstore.Store) bool {
	conns, err := store.List(ctx)
	if err != nil {
		return false
	}
	for _, c := range conns {
		if c.SubscriptionID == demo.AzureSubscriptionID {
			return true
		}
	}
	return false
}

func ociDemoConnExists(ctx context.Context, store ociconnstore.Store) bool {
	conns, err := store.List(ctx)
	if err != nil {
		return false
	}
	for _, c := range conns {
		if c.TenancyOCID == demo.OCITenancyOCID {
			return true
		}
	}
	return false
}
