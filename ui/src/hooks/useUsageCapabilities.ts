import useSWR from "swr";

import { getUsageCapabilities, type UsageCapabilities } from "@/api/usage";

// useUsageCapabilities feature-detects the enterprise per-tenant usage surface
// by probing GET /usage/capabilities (ADR 0023). Same idiom as the audit
// capability hooks: shouldRetryOnError:false so a 404 (OSS — the usage seam is
// unmounted) is a single clean "not enterprise" signal. Distinct SWR key.
export interface UsageCapabilitiesState {
  capabilities?: UsageCapabilities;
  isEnterprise: boolean;
  isLoading: boolean;
}

export function useUsageCapabilities(): UsageCapabilitiesState {
  const { data, error, isLoading } = useSWR<UsageCapabilities>(
    "usage-capabilities",
    getUsageCapabilities,
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
