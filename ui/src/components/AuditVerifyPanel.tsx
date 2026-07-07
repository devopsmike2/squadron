import { Download, ShieldAlert, ShieldCheck } from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import {
  downloadAttestation,
  getFleetVerify,
  getTenantVerify,
  type TenantVerification,
} from "@/api/auditVerify";
import { listTenants } from "@/api/tenants";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useAuditVerifyCapabilities } from "@/hooks/useAuditVerifyCapabilities";

const SELF_TENANT = "__self__";

// VerifyOutcome normalizes a fleet rollup and a single-tenant verify into the
// same shape so the results view renders one row per tenant either way.
interface VerifyOutcome {
  verified_at?: string;
  ok: boolean;
  tenants: TenantVerification[];
}

// AuditVerifyPanel is the enterprise tamper-evidence surface, a sibling of the
// AuditReviewPanel, mounted as the "Integrity" tab on /audit. It feature-detects
// via the capabilities probe: in OSS (404) it shows an enterprise-feature notice;
// in enterprise it re-verifies the audit hash chain — the whole fleet in one pass
// (primary action) or a single named tenant (cross-tenant picker) — and, when the
// backend seals attestations, downloads a signed attestation for a named tenant.
// The backend 403 is the real scope enforcement; the UI can't see the operator's
// scopes.
export function AuditVerifyPanel() {
  const { isEnterprise, capabilities, isLoading } =
    useAuditVerifyCapabilities();

  const [tenant, setTenant] = useState<string>(SELF_TENANT);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [outcome, setOutcome] = useState<VerifyOutcome | null>(null);
  // verifiedTenant is the specific named tenant last verified — gates the
  // attestation download (a fleet pass or "my tenant" has no single target).
  const [verifiedTenant, setVerifiedTenant] = useState<string | null>(null);
  const [dlBusy, setDlBusy] = useState(false);
  const [dlErr, setDlErr] = useState<string | null>(null);

  const crossTenantCapable =
    isEnterprise && capabilities?.cross_tenant === true;
  const canAttest = isEnterprise && capabilities?.sealed_attestation === true;

  // Tenant list backs the cross-tenant picker. Enterprise-only; 404s in OSS.
  const { data: tenants } = useSWR(
    crossTenantCapable ? "audit-verify-tenants" : null,
    listTenants,
    { shouldRetryOnError: false },
  );

  if (isLoading) {
    return (
      <p
        className="text-muted-foreground text-sm"
        data-testid="audit-verify-loading"
      >
        Checking integrity-verification availability…
      </p>
    );
  }

  if (!isEnterprise) {
    return (
      <div
        className="text-muted-foreground rounded-md border border-dashed p-6 text-sm"
        data-testid="audit-verify-enterprise-gate"
      >
        <p className="text-foreground font-medium">
          Audit integrity verification is an enterprise feature.
        </p>
        <p className="mt-1">
          Hash-chain re-verification (per-tenant and fleet-wide) and sealed
          tamper-evidence attestations are available in the enterprise edition.
          The single-tenant audit log and CSV/JSON export on the “Recent
          activity” tab remain available here.
        </p>
      </div>
    );
  }

  const verifyFleet = async () => {
    setBusy(true);
    setErr(null);
    setOutcome(null);
    setVerifiedTenant(null);
    setDlErr(null);
    try {
      const data = await getFleetVerify();
      setOutcome({
        verified_at: data.verified_at,
        ok: data.ok,
        tenants: data.tenants,
      });
    } catch (e) {
      setErr(e instanceof Error ? e.message : "verification failed");
    } finally {
      setBusy(false);
    }
  };

  const verifyTenant = async () => {
    setBusy(true);
    setErr(null);
    setOutcome(null);
    setVerifiedTenant(null);
    setDlErr(null);
    try {
      // "My tenant" has no client-side id, so fall back to the fleet pass
      // (which covers the caller's own tenant). A named tenant targets one.
      if (tenant === SELF_TENANT) {
        const data = await getFleetVerify();
        setOutcome({
          verified_at: data.verified_at,
          ok: data.ok,
          tenants: data.tenants,
        });
      } else {
        const row = await getTenantVerify(tenant);
        setOutcome({
          verified_at: row.verified_at,
          ok: row.ok,
          tenants: [row],
        });
        setVerifiedTenant(tenant);
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : "verification failed");
    } finally {
      setBusy(false);
    }
  };

  const download = async () => {
    if (!verifiedTenant) return;
    setDlBusy(true);
    setDlErr(null);
    try {
      await downloadAttestation(verifiedTenant);
    } catch (e) {
      setDlErr(e instanceof Error ? e.message : "attestation failed");
    } finally {
      setDlBusy(false);
    }
  };

  return (
    <div className="space-y-4" data-testid="audit-verify-controls">
      <div className="flex flex-wrap items-end gap-2">
        {capabilities?.fleet && (
          <Button
            onClick={verifyFleet}
            disabled={busy}
            className="h-9 gap-1"
            data-testid="audit-verify-fleet"
          >
            <ShieldCheck className="h-4 w-4" />
            {busy ? "Verifying…" : "Verify fleet"}
          </Button>
        )}

        {/* Cross-tenant picker for a targeted single-tenant verify (own tenant
            by default; named tenants require audit:cross_tenant server-side). */}
        {crossTenantCapable && (
          <>
            <div data-testid="audit-verify-tenant">
              <Label htmlFor="verify-tenant" className="sr-only">
                Tenant
              </Label>
              <Select value={tenant} onValueChange={setTenant}>
                <SelectTrigger id="verify-tenant" className="h-9 w-56">
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
            <Button
              onClick={verifyTenant}
              disabled={busy}
              variant="outline"
              className="h-9 gap-1"
              data-testid="audit-verify-run"
            >
              <ShieldCheck className="h-4 w-4" />
              {busy ? "Verifying…" : "Verify"}
            </Button>
          </>
        )}

        {canAttest && verifiedTenant && (
          <Button
            onClick={download}
            disabled={dlBusy}
            variant="outline"
            className="h-9 gap-1"
            data-testid="audit-verify-download"
          >
            <Download className="h-4 w-4" />
            {dlBusy ? "Downloading…" : "Download attestation"}
          </Button>
        )}
      </div>

      {err && (
        <p className="text-xs text-red-600" data-testid="audit-verify-error">
          {err}
        </p>
      )}
      {dlErr && (
        <p
          className="text-xs text-red-600"
          data-testid="audit-verify-download-error"
        >
          {dlErr}
        </p>
      )}

      {outcome && (
        <div data-testid="audit-verify-results" className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <span className="text-sm font-medium">
              {outcome.ok ? "Chain intact" : "Tampering detected"}
            </span>
            {outcome.verified_at && (
              <span className="text-muted-foreground text-xs">
                verified {outcome.verified_at}
              </span>
            )}
          </div>
          <div className="overflow-hidden rounded-md border">
            {outcome.tenants.map((t) => (
              <TenantRow key={t.tenant} row={t} />
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function TenantRow({ row }: { row: TenantVerification }) {
  return (
    <div
      className="flex flex-wrap items-center gap-3 border-b px-3 py-2 text-sm last:border-b-0"
      data-testid={`audit-verify-row-${row.tenant}`}
    >
      {row.ok ? (
        <ShieldCheck className="h-4 w-4 text-green-600" />
      ) : (
        <ShieldAlert className="h-4 w-4 text-red-600" />
      )}
      <span className="font-medium">{row.tenant}</span>
      <Badge
        variant="outline"
        className={
          row.ok
            ? "border-green-600 text-green-700"
            : "border-red-600 text-red-700"
        }
      >
        {row.ok ? "✓ intact" : "✗ broken"}
      </Badge>
      <span className="text-muted-foreground text-xs">
        {row.rows_verified} rows · head #{row.head_seq}
      </span>
      {!row.ok && row.first_break_seq != null && (
        <span className="text-xs text-red-600">
          first break at seq {row.first_break_seq}
        </span>
      )}
      {row.detail && (
        <span className="text-muted-foreground text-xs">{row.detail}</span>
      )}
    </div>
  );
}
