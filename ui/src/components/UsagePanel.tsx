import { useState } from "react";
import useSWR from "swr";

import { listTenants } from "@/api/tenants";
import { getOwnUsage, getTenantUsage, type UsageSummary } from "@/api/usage";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useUsageCapabilities } from "@/hooks/useUsageCapabilities";

const SELF_TENANT = "__self__";

// UsagePanel is the enterprise per-tenant usage/billing dashboard (ADR 0023),
// mounted as the "Usage" tab on SettingsIdentity. It feature-detects the
// enterprise /api/v1/usage surface: in OSS (404) it shows an enterprise-feature
// notice; in enterprise it shows the caller's own-tenant usage (agents +
// rollouts), and — when cross_tenant is available — a tenant picker to view
// another tenant's usage (a chargeback/showback view). The backend 403 is the
// real cross-tenant enforcement; the picker is only offered when capabilities
// report cross_tenant.
export function UsagePanel() {
  const { isEnterprise, capabilities, isLoading } = useUsageCapabilities();
  const [tenant, setTenant] = useState<string>(SELF_TENANT);

  const crossTenantCapable =
    isEnterprise && capabilities?.cross_tenant === true;

  const { data: tenants } = useSWR(
    crossTenantCapable ? "usage-tenants" : null,
    listTenants,
    { shouldRetryOnError: false },
  );

  const viewingSelf = !crossTenantCapable || tenant === SELF_TENANT;
  const {
    data: summary,
    error: summaryError,
    isLoading: summaryLoading,
  } = useSWR<UsageSummary>(
    isEnterprise ? ["usage-summary", viewingSelf ? SELF_TENANT : tenant] : null,
    () => (viewingSelf ? getOwnUsage() : getTenantUsage(tenant)),
    { shouldRetryOnError: false },
  );

  if (isLoading) {
    return (
      <p className="text-muted-foreground text-sm">
        Checking usage availability…
      </p>
    );
  }

  if (!isEnterprise) {
    return (
      <div
        className="text-muted-foreground rounded-md border border-dashed p-6 text-sm"
        data-testid="usage-enterprise-gate"
      >
        <p className="text-foreground font-medium">
          Per-tenant usage is an enterprise feature
        </p>
        <p className="mt-1">
          Per-tenant usage summaries (agents, rollouts) — a chargeback/showback
          view across your tenants — are available in the enterprise edition.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-4" data-testid="usage-controls">
      {crossTenantCapable && (
        <div className="max-w-sm" data-testid="usage-tenant-picker">
          <Label htmlFor="usage-tenant">Tenant</Label>
          <Select value={tenant} onValueChange={setTenant}>
            <SelectTrigger id="usage-tenant">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={SELF_TENANT}>My tenant</SelectItem>
              {(tenants ?? []).map((t) => (
                <SelectItem key={t.tenant_id} value={t.tenant_id}>
                  {`${t.name} (${t.tenant_id})`}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}

      {summaryError && (
        <p className="text-xs text-red-600" data-testid="usage-error">
          {summaryError instanceof Error
            ? summaryError.message
            : "failed to load usage"}
        </p>
      )}

      {summaryLoading && !summary && (
        <p className="text-muted-foreground text-sm">Loading usage…</p>
      )}

      {summary && (
        <div className="grid gap-3 sm:grid-cols-2" data-testid="usage-summary">
          <UsageStat label="Agents" value={summary.agents} />
          <UsageStat label="Rollouts" value={summary.rollouts} />
          <div className="sm:col-span-2 flex items-center gap-2 text-xs">
            <Badge variant="outline">tenant: {summary.tenant}</Badge>
            {summary.cross_tenant && (
              <Badge variant="secondary">cross-tenant</Badge>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function UsageStat({ label, value }: { label: string; value: number }) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-muted-foreground text-xs font-medium">
          {label}
        </CardTitle>
      </CardHeader>
      <CardContent>
        <span className="text-2xl font-semibold">{value.toLocaleString()}</span>
      </CardContent>
    </Card>
  );
}
