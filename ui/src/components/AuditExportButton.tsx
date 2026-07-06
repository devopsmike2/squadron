import { Download } from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import {
  downloadAuditExport,
  streamAuditExport,
  type AuditExportFormat,
  type OSSAuditExportFormat,
  type StreamProgress,
} from "@/api/audit";
import { listTenants } from "@/api/tenants";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useAuditExportCapabilities } from "@/hooks/useAuditExportCapabilities";
import type { AuditEventFilter } from "@/types/audit";

// SELF_TENANT is the tenant-picker sentinel for "my own tenant" (no ?tenant=
// param). Radix Select requires non-empty item values, so we can't use "".
const SELF_TENANT = "__self__";

// AuditExportButton downloads the audit log as an evidence file (ADR 0020). It
// reuses the caller's current filter but raises the limit to the store's page
// cap so an export captures the full available window, not just the rows shown.
//
// It progressively enhances against the enterprise export: it probes
// /audit-export/capabilities and, when present, offers NDJSON, a cross-tenant
// tenant picker, and live streaming progress (via streamAuditExport). In OSS
// (probe 404s) it falls back to the single-tenant csv/json download exactly as
// before. The export is itself recorded in the audit log (audit.exported), so
// exports stay on the record for compliance evidence.
export function AuditExportButton({ filter }: { filter?: AuditEventFilter }) {
  const { isEnterprise, capabilities } = useAuditExportCapabilities();
  const crossTenant = isEnterprise && capabilities?.cross_tenant === true;

  const [format, setFormat] = useState<AuditExportFormat>("csv");
  const [tenant, setTenant] = useState<string>(SELF_TENANT);
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState<StreamProgress | null>(null);
  const [err, setErr] = useState<string | null>(null);

  // Tenant list only when a cross-tenant enterprise export is available.
  const { data: tenants } = useSWR(
    crossTenant ? "audit-export-tenants" : null,
    listTenants,
    { shouldRetryOnError: false },
  );

  const onExport = async () => {
    setBusy(true);
    setErr(null);
    setProgress(null);
    try {
      const limit = filter?.limit && filter.limit > 1000 ? filter.limit : 1000;
      if (isEnterprise) {
        await streamAuditExport({ ...filter, limit }, format, {
          tenant: tenant === SELF_TENANT ? undefined : tenant,
          onProgress: setProgress,
        });
      } else {
        // OSS single-tenant route: csv|json only (NDJSON isn't offered here).
        const ossFormat: OSSAuditExportFormat =
          format === "ndjson" ? "csv" : format;
        await downloadAuditExport({ ...filter, limit }, ossFormat);
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : "export failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col items-end gap-1">
      <div className="flex items-center gap-2">
        {crossTenant && (
          <div
            className="flex items-center gap-1"
            data-testid="audit-export-tenant"
          >
            <Label htmlFor="audit-export-tenant-select" className="sr-only">
              Tenant
            </Label>
            <Select value={tenant} onValueChange={setTenant}>
              <SelectTrigger
                id="audit-export-tenant-select"
                className="h-9 w-44"
              >
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
        <Select
          value={format}
          onValueChange={(v) => setFormat(v as AuditExportFormat)}
        >
          <SelectTrigger className="h-9 w-24">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="csv">CSV</SelectItem>
            <SelectItem value="json">JSON</SelectItem>
            {isEnterprise && <SelectItem value="ndjson">NDJSON</SelectItem>}
          </SelectContent>
        </Select>
        <Button onClick={onExport} disabled={busy} className="gap-1">
          <Download className="h-4 w-4" />
          {busy ? "Exporting…" : "Export"}
        </Button>
      </div>
      {busy && progress && (
        <span
          className="text-xs text-muted-foreground"
          data-testid="audit-export-progress"
        >
          {progress.rows.toLocaleString()} rows…
        </span>
      )}
      {err && <span className="text-xs text-red-600">{err}</span>}
    </div>
  );
}
