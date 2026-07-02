// One-click demo data — seeds/removes sample data across every feature area so a
// first-time user can explore Squadron's flagship loops (fleet, configs, a cost
// spike -> AI proposal -> rollout, discovery inventory + recommendations)
// without configuring anything real. Backed by POST/DELETE /api/v1/demo.

import { apiDelete, apiPost } from "./base";

export interface EnableDemoDataResponse {
  status: string;
  discovery_enabled: boolean;
  seeded: {
    group_id: string;
    config_id: string;
    agent_id: string;
    spike_id: string;
  };
}

/** Seed the full demo scenario. Idempotent server-side. */
export function enableDemoData(): Promise<EnableDemoDataResponse> {
  return apiPost<EnableDemoDataResponse>("/demo/enable", {});
}

/** Remove the demo-scoped rows. Idempotent server-side. */
export function removeDemoData(): Promise<{ status: string }> {
  return apiDelete<{ status: string }>("/demo");
}
