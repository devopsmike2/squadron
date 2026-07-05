// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/driftnotify"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
)

// tenant_scan_writepath_d6b_test.go — ADR 0013 §D6-b write-path
// isolation. The rescan scheduler lists connections under a system ctx
// but each per-connection discovery_scans row must land in the OWNING
// connection's tenant, not `default`. This proves the stamp applied by
// stampOwnerTenant (the exact function the scheduler's ScanAccount
// closures call) reaches the effective write tenant.

// effectiveWriteTenant resolves the tenant a SaveDiscoveryScan write
// would PERSIST for the given ctx, mirroring the SQLite store's
// tenantScope: a system ctx resolves to DefaultTenant; a WithTenant ctx
// resolves to that tenant. (Mirrors D6-a's tenant_anchor helper.)
func effectiveWriteTenant(ctx context.Context) string {
	if identity.IsSystemContext(ctx) {
		return identity.DefaultTenant
	}
	return identity.TenantFromContext(ctx)
}

// TestScheduler_D6b_GCP_ScanLandsInOwningTenant: a GCP connection owned
// by tenant acme ⇒ stampOwnerTenant (with the scheduler's real GCP
// ownerOf lookup) produces a ctx whose effective write tenant is acme,
// not default.
func TestScheduler_D6b_GCP_ScanLandsInOwningTenant(t *testing.T) {
	store := gcpconnstore.NewMemoryStore()
	conn := &gcpconnstore.GCPConnection{
		DisplayName: "acme project",
		ProjectID:   "acme-proj",
		SealedSA:    []byte("opaque"),
		TenantID:    "acme",
	}
	require.NoError(t, store.Create(context.Background(), conn))

	// The scheduler runs under a system ctx.
	sysCtx := identity.WithSystemContext(context.Background())
	require.Equal(t, identity.DefaultTenant, effectiveWriteTenant(sysCtx),
		"sanity: an un-stamped system ctx would write default")

	stamped := stampOwnerTenant(sysCtx, conn.ID, func(ctx context.Context, id string) string {
		if c, err := store.Get(ctx, id); err == nil && c != nil {
			return c.TenantID
		}
		return ""
	})
	require.Equal(t, "acme", effectiveWriteTenant(stamped),
		"discovery_scans write landed in acme not default — anchored on the connection's owner tenant")
}

// TestScheduler_D6b_Azure_ScanLandsInOwningTenant: uses SquadronTenantID
// (the Squadron owner tenant, distinct from the Azure-AD TenantID).
func TestScheduler_D6b_Azure_ScanLandsInOwningTenant(t *testing.T) {
	store := azureconnstore.NewMemoryStore()
	conn := &azureconnstore.AzureConnection{
		DisplayName:      "acme sub",
		TenantID:         "00000000-0000-0000-0000-000000000001", // Azure-AD tenant
		SubscriptionID:   "sub-acme",
		ClientID:         "00000000-0000-0000-0000-000000000002",
		SealedSecret:     []byte("opaque"),
		SquadronTenantID: "acme",
	}
	require.NoError(t, store.Create(context.Background(), conn))

	sysCtx := identity.WithSystemContext(context.Background())
	stamped := stampOwnerTenant(sysCtx, conn.ID, func(ctx context.Context, id string) string {
		if c, err := store.Get(ctx, id); err == nil && c != nil {
			return c.SquadronTenantID
		}
		return ""
	})
	require.Equal(t, "acme", effectiveWriteTenant(stamped),
		"discovery_scans write landed in acme — anchored on SquadronTenantID, not the Azure-AD tenant")
}

// TestScheduler_D6b_OCI_ScanLandsInOwningTenant: uses OwnerTenantID (the
// Squadron owner tenant, distinct from the OCI TenancyOCID).
func TestScheduler_D6b_OCI_ScanLandsInOwningTenant(t *testing.T) {
	store := ociconnstore.NewMemoryStore()
	conn := &ociconnstore.OCIConnection{
		DisplayName:      "acme tenancy",
		TenancyOCID:      "ocid1.tenancy.oc1..acme",
		UserOCID:         "ocid1.user.oc1..acme",
		Fingerprint:      "aa:bb:cc",
		SealedPrivateKey: []byte("opaque"),
		Region:           "us-phoenix-1",
		OwnerTenantID:    "acme",
	}
	require.NoError(t, store.Create(context.Background(), conn))

	sysCtx := identity.WithSystemContext(context.Background())
	stamped := stampOwnerTenant(sysCtx, conn.ID, func(ctx context.Context, id string) string {
		if c, err := store.Get(ctx, id); err == nil && c != nil {
			return c.OwnerTenantID
		}
		return ""
	})
	require.Equal(t, "acme", effectiveWriteTenant(stamped),
		"discovery_scans write landed in acme — anchored on OwnerTenantID, not the OCI tenancy OCID")
}

// TestScheduler_D6b_InertInOSS: a connection with no owner tenant (OSS
// reality) ⇒ stampOwnerTenant leaves the system ctx unchanged, so the
// write stays default. The stamp is a no-op.
func TestScheduler_D6b_InertInOSS(t *testing.T) {
	store := gcpconnstore.NewMemoryStore()
	conn := &gcpconnstore.GCPConnection{
		DisplayName: "default project",
		ProjectID:   "default-proj",
		SealedSA:    []byte("opaque"),
		// No TenantID set on the struct — but the store default-guard
		// stamps "default" on Create, which is the OSS reality.
	}
	require.NoError(t, store.Create(context.Background(), conn))
	require.Equal(t, "default", conn.TenantID)

	sysCtx := identity.WithSystemContext(context.Background())
	stamped := stampOwnerTenant(sysCtx, conn.ID, func(ctx context.Context, id string) string {
		if c, err := store.Get(ctx, id); err == nil && c != nil {
			return c.TenantID
		}
		return ""
	})
	// "default" is a real (non-empty) owner tenant, so the ctx is stamped
	// to default. The effective write tenant is still default — byte-inert
	// relative to the pre-D6-b behavior (system ctx → DefaultTenant).
	require.Equal(t, "default", effectiveWriteTenant(stamped),
		"OSS connection writes default — the stamp is inert")
}

// TestScheduler_D6b_MissingConnLeavesCtxUnchanged: when the connection
// lookup fails (deleted between list and scan), stampOwnerTenant returns
// the ctx unchanged so the system ctx's DefaultTenant fallback holds.
func TestScheduler_D6b_MissingConnLeavesCtxUnchanged(t *testing.T) {
	sysCtx := identity.WithSystemContext(context.Background())
	stamped := stampOwnerTenant(sysCtx, "does-not-exist", func(ctx context.Context, id string) string {
		return "" // lookup miss
	})
	require.True(t, identity.IsSystemContext(stamped),
		"a lookup miss must leave the system ctx intact (fleet-wide fallback)")
	require.Equal(t, identity.DefaultTenant, effectiveWriteTenant(stamped))
}

// TestScanAccountWithDrift_D6b_PropagatesStampedCtxToScanAndDrift proves
// the stamped ctx that enters scanAccountWithDrift is the SAME ctx the
// inner scan fn (which performs the SaveDiscoveryScan write) receives —
// and, because EmitIfChanged is called with that same ctx variable, the
// drift emit inherits the tenant too. With a nil appStore EmitIfChanged
// no-ops, so this isolates the ctx-propagation guarantee.
func TestScanAccountWithDrift_D6b_PropagatesStampedCtxToScanAndDrift(t *testing.T) {
	s := &Server{} // nil appStore + nil auditService → EmitIfChanged no-ops
	emitter := driftnotify.NewEmitter(0)

	var scanSawTenant string
	wrapped := s.scanAccountWithDrift(emitter, "gcp", func(ctx context.Context, id string) error {
		scanSawTenant = effectiveWriteTenant(ctx)
		return nil
	})

	stamped := identity.WithTenant(identity.WithSystemContext(context.Background()), "acme")
	require.NoError(t, wrapped(stamped, "conn-1"))
	require.Equal(t, "acme", scanSawTenant,
		"the stamped tenant reaches the inner scan fn (which performs the SaveDiscoveryScan write); "+
			"EmitIfChanged is invoked with the same ctx so the drift emit inherits it too")
}
