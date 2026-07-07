import useSWR from "swr";

import { getBudgetCapabilities, type BudgetCapabilities } from "@/api/budgets";

// useBudgetCapabilities feature-detects the enterprise per-tenant budget-admin
// surface by probing GET /budgets/capabilities. Same idiom as the usage
// capability hook: shouldRetryOnError:false so a 404 (OSS — the budgets seam is
// unmounted) is a single clean "not enterprise" signal. Distinct SWR key.
export interface BudgetCapabilitiesState {
  capabilities?: BudgetCapabilities;
  isEnterprise: boolean;
  isLoading: boolean;
}

export function useBudgetCapabilities(): BudgetCapabilitiesState {
  const { data, error, isLoading } = useSWR<BudgetCapabilities>(
    "budgets-capabilities",
    getBudgetCapabilities,
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
