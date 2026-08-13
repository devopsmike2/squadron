import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { AgentOverview } from "./AgentOverview";

import { updateAgentGroup } from "@/api/agents";
import { getGroups } from "@/api/groups";
import type { Agent } from "@/types/agent";

// Radix Select needs these in jsdom when a Select is interacted with.
if (!Element.prototype.hasPointerCapture)
  Element.prototype.hasPointerCapture = () => false;
if (!Element.prototype.scrollIntoView)
  Element.prototype.scrollIntoView = () => {};

vi.mock("@/api/agents", () => ({ updateAgentGroup: vi.fn() }));
vi.mock("@/api/groups", () => ({ getGroups: vi.fn() }));
// AuditTimeline fetches audit rows on mount; stub it so this test stays
// focused on the group selector.
vi.mock("@/components/AuditTimeline", () => ({ AuditTimeline: () => null }));

const mockedUpdate = vi.mocked(updateAgentGroup);
const mockedGetGroups = vi.mocked(getGroups);

const groupsResponse = {
  groups: [
    {
      id: "grp-1",
      name: "prod",
      labels: {},
      agent_count: 1,
      created_at: "",
      updated_at: "",
    },
    {
      id: "grp-2",
      name: "staging",
      labels: {},
      agent_count: 0,
      created_at: "",
      updated_at: "",
    },
  ],
  count: 2,
};

const agentInProd: Agent = {
  id: "agent-1",
  name: "web-01",
  status: "online",
  last_seen: new Date().toISOString(),
  version: "1.2.3",
  labels: {},
  discovery_source: "opamp",
  group_id: "grp-1",
  group_name: "prod",
};

const renderOverview = (
  agent: Agent,
  onAgentChanged?: () => void | Promise<unknown>,
) =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <AgentOverview agent={agent} onAgentChanged={onAgentChanged} />
    </SWRConfig>,
  );

describe("AgentOverview — group selector", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockedGetGroups.mockResolvedValue(groupsResponse);
  });

  it("renders the agent's current group as the selected value", async () => {
    renderOverview(agentInProd);
    await waitFor(() =>
      expect(screen.getByLabelText("Group")).not.toBeDisabled(),
    );
    expect(screen.getByLabelText("Group")).toHaveTextContent("prod");
  });

  it("changing the group calls updateAgentGroup with the new id and refetches", async () => {
    const onAgentChanged = vi.fn().mockResolvedValue(undefined);
    mockedUpdate.mockResolvedValue(undefined);
    renderOverview(agentInProd, onAgentChanged);

    await waitFor(() =>
      expect(screen.getByLabelText("Group")).not.toBeDisabled(),
    );
    fireEvent.click(screen.getByLabelText("Group"));
    fireEvent.click(await screen.findByText("staging"));

    await waitFor(() =>
      expect(mockedUpdate).toHaveBeenCalledWith("agent-1", "grp-2"),
    );
    await waitFor(() => expect(onAgentChanged).toHaveBeenCalled());
    expect(await screen.findByText(/Moved to staging/)).toBeInTheDocument();
  });

  it("clearing the group calls updateAgentGroup with null", async () => {
    const onAgentChanged = vi.fn().mockResolvedValue(undefined);
    mockedUpdate.mockResolvedValue(undefined);
    renderOverview(agentInProd, onAgentChanged);

    await waitFor(() =>
      expect(screen.getByLabelText("Group")).not.toBeDisabled(),
    );
    fireEvent.click(screen.getByLabelText("Group"));
    fireEvent.click(await screen.findByText("No group"));

    await waitFor(() =>
      expect(mockedUpdate).toHaveBeenCalledWith("agent-1", null),
    );
    expect(await screen.findByText(/Removed from group/)).toBeInTheDocument();
  });

  it("surfaces an error when the update fails", async () => {
    mockedUpdate.mockRejectedValue(new Error("patch boom"));
    renderOverview(agentInProd);

    await waitFor(() =>
      expect(screen.getByLabelText("Group")).not.toBeDisabled(),
    );
    fireEvent.click(screen.getByLabelText("Group"));
    fireEvent.click(await screen.findByText("staging"));

    expect(await screen.findByRole("alert")).toHaveTextContent(/patch boom/);
  });
});
