import useSWR from "swr";

import {
  getAuditExportCapabilities,
  type AuditExportCapabilities,
} from "@/api/audit";

// useAuditExportCapabilities feature-detects the enterprise audit-export surface
// by probing GET /audit-export/capabilities (ADR 0020 6d-part-2). It reuses the
// SettingsIdentity idiom: shouldRetryOnError:false so a 404 (OSS — the export
// seam is unmounted) is a single clean "not enterprise" signal, not a retry
// storm (a non-404 / transient probe error likewise degrades to the OSS
// export, without retry, until the view remounts). isEnterprise gates the
// extra export controls (NDJSON, cross-tenant
// picker, streaming progress); everything falls back to the single-tenant OSS
// export when it's false.
export interface AuditExportCapabilitiesState {
  capabilities?: AuditExportCapabilities;
  isEnterprise: boolean;
  isLoading: boolean;
}

export function useAuditExportCapabilities(): AuditExportCapabilitiesState {
  const { data, error, isLoading } = useSWR<AuditExportCapabilities>(
    "audit-export-capabilities",
    getAuditExportCapabilities,
    { shouldRetryOnError: false, revalidateOnFocus: false },
  );

  const notEnterprise =
    error != null && (error as { status?: number }).status === 404;

  return {
    capabilities: data,
    isEnterprise: data != null && !notEnterprise,
    isLoading: isLoading && error == null && data == null,
  };
}
