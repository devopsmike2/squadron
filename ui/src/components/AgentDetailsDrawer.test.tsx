import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { AgentDetailsDrawer } from "./AgentDetailsDrawer";

import { decommissionAgent, dismissDuplicate } from "@/api/agents";
import { getAgentTopology } from "@/api/topology";
import type { AgentTopologyResponse } from "@/api/topology";
import type { Agent } from "@/types/agent";

// Stub the data + action APIs.
vi.mock("@/api/topology", () => ({ getAgentTopology: vi.fn() }));
vi.mock("@/api/agents", () => ({
  getAgentTopology: vi.fn(),
  decommissionAgent: vi.fn(),
  dismissDuplicate: vi.fn(),
  restartAgent: vi.fn(),
}));

// The drawer embeds several data-heavy child panels that each fetch on their
// own; stub them to keep this test focused on the duplicate banner + actions.
vi.mock("@/components/insights/VolumePanel", () => ({
  VolumePanel: () => null,
}));
vi.mock("@/components/pipeline-health/PipelineHealthPanel", () => ({
  PipelineHealthAgentPanel: () => null,
}));
vi.mock("@/components/recommendations/RecommendationsPanel", () => ({
  RecommendationsPanel: () => null,
}));
vi.mock("@/components/agent-details/AgentOverview", () => ({
  AgentOverview: () => null,
}));
vi.mock("@/components/agent-details/AgentConfigPipeline", () => ({
  AgentConfigPipeline: () => null,
}));
vi.mock("@/components/agent-details/AgentMetrics", () => ({
  AgentMetrics: () => null,
}));
vi.mock("@/components/agent-details/AgentLogs", () => ({
  AgentLogs: () => null,
}));

const mockedTopology = vi.mocked(getAgentTopology);
const mockedDismiss = vi.mocked(dismissDuplicate);
const mockedDecommission = vi.mocked(decommissionAgent);

function topologyFor(agent: Agent): AgentTopologyResponse {
  return {
    agent,
    metrics: {
      metric_count: 0,
      log_count: 0,
      trace_count: 0,
      throughput_rps: 0,
    },
    pipeline: {},
  };
}

const duplicateAgent: Agent = {
  id: "agent-b",
  name: "web-01",
  status: "online",
  last_seen: new Date().toISOString(),
  version: "",
  labels: { "host.name": "web-01" },
  discovery_source: "otlp",
  suspected_duplicate_of: {
    of_agent_id: "agent-a",
    of_agent_name: "collector-1",
    reason:
      'telemetry-only agent, same host.name "web-01" as an OpAMP-managed agent',
  },
};

const renderDrawer = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <AgentDetailsDrawer agentId="agent-b" open onOpenChange={() => {}} />
    </SWRConfig>,
  );

describe("AgentDetailsDrawer — duplicate-identity banner (backlog #5)", () => {
  beforeEach(() => vi.clearAllMocks());

  it("shows the banner naming the managed agent, with Dismiss and Decommission", async () => {
    mockedTopology.mockResolvedValue(topologyFor(duplicateAgent));
    renderDrawer();

    expect(
      await screen.findByText(/Likely duplicate of collector-1/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Dismiss/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Decommission/i }),
    ).toBeInTheDocument();
  });

  it("does not show the banner when the agent is not a suspected duplicate", async () => {
    const clean: Agent = {
      ...duplicateAgent,
      suspected_duplicate_of: undefined,
    };
    mockedTopology.mockResolvedValue(topologyFor(clean));
    renderDrawer();

    // Wait for the header to reflect the loaded agent, then assert no banner.
    await screen.findAllByText("web-01");
    expect(screen.queryByText(/Likely duplicate of/)).not.toBeInTheDocument();
  });

  it("Dismiss calls the dismiss-duplicate endpoint", async () => {
    mockedTopology.mockResolvedValue(topologyFor(duplicateAgent));
    mockedDismiss.mockResolvedValue(duplicateAgent);
    renderDrawer();

    const btn = await screen.findByRole("button", { name: /Dismiss/i });
    fireEvent.click(btn);
    await waitFor(() => expect(mockedDismiss).toHaveBeenCalledWith("agent-b"));
  });

  it("Decommission uses the existing delete flow", async () => {
    mockedTopology.mockResolvedValue(topologyFor(duplicateAgent));
    mockedDecommission.mockResolvedValue({ ok: true, agent_id: "agent-b" });
    vi.spyOn(window, "confirm").mockReturnValue(true);
    renderDrawer();

    const btn = await screen.findByRole("button", { name: /Decommission/i });
    fireEvent.click(btn);
    await waitFor(() =>
      expect(mockedDecommission).toHaveBeenCalledWith("agent-b"),
    );
  });
});
