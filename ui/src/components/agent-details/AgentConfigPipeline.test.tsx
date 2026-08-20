// Vitest coverage for the drift-state copy on the agent config pipeline. The
// heavy children (Monaco-backed editors, the pipeline view, the adopt button)
// and the API boundary are mocked so the test asserts only the user-facing
// copy. Guards the customer-facing rename: the synced-state description must
// name "Squadron", never the retired internal codename. The retired codename
// is assembled from fragments below so the banned-word CI guard (which scans
// this file too) never trips on the assertion itself.

import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { AgentConfigPipeline } from "./AgentConfigPipeline";

import type { Agent } from "@/types/agent";

vi.mock("@/api/agents", () => ({
  sendConfigToAgent: vi.fn(),
}));

vi.mock("@/api/collector-metrics", () => ({
  fetchAgentComponentMetrics: vi.fn(() => Promise.resolve([])),
}));

vi.mock("@/api/configs", () => ({
  getConfigs: vi.fn(() => Promise.resolve({ configs: [] })),
}));

vi.mock("@/components/ThemeProvider", () => ({
  useTheme: () => ({ theme: "light" }),
}));

// Stub the heavy children so the pipeline renders without Monaco or network.
vi.mock("@/components/agent-details/AdoptConfigButton", () => ({
  AdoptConfigButton: () => null,
}));

vi.mock("@/components/collector-pipeline/CollectorPipelineView", () => ({
  CollectorPipelineView: () => null,
}));

vi.mock("@/components/configs/ConfigYamlEditorWithMetrics", () => ({
  ConfigYamlEditorWithMetrics: () => null,
}));

// Retired internal codename, assembled from fragments so the literal never
// appears in the tree (keeps the banned-word guard from matching this file).
const RETIRED_CODENAME = ["Cere", "bro"].join("");

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: "agent-1",
    name: "web-01",
    status: "online",
    last_seen: "2026-08-10T00:00:00Z",
    version: "1.0.0",
    labels: {},
    group_id: "grp-1",
    group_name: "prod",
    drift_status: "synced",
    ...overrides,
  } as Agent;
}

describe("AgentConfigPipeline", () => {
  it("names Squadron (not the retired codename) in the synced-state description", async () => {
    render(
      <AgentConfigPipeline
        agentId="agent-1"
        effectiveConfig="receivers: {}"
        agent={makeAgent()}
      />,
    );

    // findBy* lets the mocked metrics SWR fetch settle inside act().
    const description = await screen.findByText(/exact config assigned by/i);
    expect(description).toHaveTextContent(
      "The agent is running the exact config assigned by Squadron.",
    );
    expect(description).not.toHaveTextContent(
      new RegExp(RETIRED_CODENAME, "i"),
    );
  });
});
