import useSWR from "swr";

import {
  getAuditReviewCapabilities,
  type AuditReviewCapabilities,
} from "@/api/auditReview";

// useAuditReviewCapabilities feature-detects the enterprise access-review surface
// by probing GET /audit-review/capabilities (ADR 0020 6c / ADR 0022). Same idiom
// as useAuditExportCapabilities: shouldRetryOnError:false so a 404 (OSS — the
// review seam is unmounted) is a single clean "not enterprise" signal. A distinct
// SWR key from the export hook so their caches don't collide.
export interface AuditReviewCapabilitiesState {
  capabilities?: AuditReviewCapabilities;
  isEnterprise: boolean;
  isLoading: boolean;
}

export function useAuditReviewCapabilities(): AuditReviewCapabilitiesState {
  const { data, error, isLoading } = useSWR<AuditReviewCapabilities>(
    "audit-review-capabilities",
    getAuditReviewCapabilities,
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
