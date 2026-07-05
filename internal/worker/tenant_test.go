package worker

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/extension/identity"
)

// TestTenantOrDefault covers the ADR 0012 §1 mapping the worker applies to a
// WorkItem's configured ingest tenant: an explicit tenant passes through, and
// an empty tenant resolves to identity.DefaultTenant (the inert OSS default).
func TestTenantOrDefault(t *testing.T) {
	t.Run("explicit tenant passes through", func(t *testing.T) {
		if got := tenantOrDefault("acme"); got != "acme" {
			t.Fatalf("tenantOrDefault(%q) = %q, want %q", "acme", got, "acme")
		}
	})

	t.Run("empty resolves to DefaultTenant", func(t *testing.T) {
		if got := tenantOrDefault(""); got != identity.DefaultTenant {
			t.Fatalf("tenantOrDefault(\"\") = %q, want %q", got, identity.DefaultTenant)
		}
	})
}

// TestWorkItemTenantStamping asserts that the exact context-stamping
// processItem performs — identity.WithTenant(ctx, tenantOrDefault(item.Tenant))
// — puts the configured tenant on the processing context, and that an empty
// tenant yields identity.DefaultTenant. processItem itself needs a fully-wired
// Pool (parser/enricher/writer), so we exercise the stamping expression it uses
// directly; keeping the two in lockstep is a one-line invariant.
func TestWorkItemTenantStamping(t *testing.T) {
	cases := []struct {
		name       string
		item       WorkItem
		wantTenant string
	}{
		{name: "configured tenant is stamped", item: WorkItem{Tenant: "acme"}, wantTenant: "acme"},
		{name: "empty tenant stamps DefaultTenant", item: WorkItem{Tenant: ""}, wantTenant: identity.DefaultTenant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := identity.WithTenant(context.Background(), tenantOrDefault(tc.item.Tenant))
			if got := identity.TenantFromContext(ctx); got != tc.wantTenant {
				t.Fatalf("stamped tenant = %q, want %q", got, tc.wantTenant)
			}
		})
	}
}
