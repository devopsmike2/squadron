import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { GroupAgents } from "./GroupAgents";

import { getAgents, updateAgentGroup } from "@/api/agents";
import { getGroupAgents } from "@/api/groups";
import type { Agent } from "@/types/agent";

// Radix Select needs these in jsdom when a Select is interacted with.
if (!Element.prototype.hasPointerCapture)
  Element.prototype.hasPointerCapture = () => false;
if (!Element.prototype.scrollIntoView)
  Element.prototype.scrollIntoView = () => {};

vi.mock("@/api/agents", () => ({
  getAgents: vi.fn(),
  updateAgentGroup: vi.fn(),
}));
vi.mock("@/api/groups", () => ({ getGroupAgents: vi.fn() }));

const mockedGetGroupAgents = vi.mocked(getGroupAgents);
const mockedGetAgents = vi.mocked(getAgents);
const mockedUpdate = vi.mocked(updateAgentGroup);

const member: Agent = {
  id: "agent-member",
  name: "in-group-01",
  status: "online",
  last_seen: new Date().toISOString(),
  version: "1.0.0",
  labels: {},
  discovery_source: "opamp",
  group_id: "grp-1",
  group_name: "prod",
};

const candidate: Agent = {
  id: "agent-free",
  name: "ungrouped-02",
  status: "online",
  last_seen: new Date().toISOString(),
  version: "1.0.0",
  labels: {},
  discovery_source: "opamp",
};

function fleetResponse(items: Agent[]) {
  return {
    items,
    total: items.length,
    offset: 0,
    limit: 100,
    agents: {},
    totalCount: items.length,
    activeCount: items.length,
    inactiveCount: 0,
  };
}

const renderTab = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <GroupAgents groupId="grp-1" />
    </SWRConfig>,
  );

describe("GroupAgents — membership editing", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockedGetGroupAgents.mockResolvedValue({ agents: [member], count: 1 });
    mockedGetAgents.mockResolvedValue(fleetResponse([member, candidate]));
    mockedUpdate.mockResolvedValue(undefined);
  });

  it("adds an agent to the group and refreshes the member list", async () => {
    renderTab();

    // Wait for the picker to become enabled (candidates loaded).
    await waitFor(() =>
      expect(screen.getByLabelText("Add agents")).not.toBeDisabled(),
    );
    fireEvent.click(screen.getByLabelText("Add agents"));
    fireEvent.click(await screen.findByText("ungrouped-02"));

    await waitFor(() =>
      expect(mockedUpdate).toHaveBeenCalledWith("agent-free", "grp-1"),
    );
    // mutate() revalidates the member-list key.
    await waitFor(() =>
      expect(mockedGetGroupAgents.mock.calls.length).toBeGreaterThan(1),
    );
  });

  it("removes an agent from the group and refreshes", async () => {
    renderTab();

    const removeBtn = await screen.findByRole("button", {
      name: /Remove in-group-01 from group/,
    });
    fireEvent.click(removeBtn);

    await waitFor(() =>
      expect(mockedUpdate).toHaveBeenCalledWith("agent-member", null),
    );
    await waitFor(() =>
      expect(mockedGetGroupAgents.mock.calls.length).toBeGreaterThan(1),
    );
  });

  it("surfaces an error when the assignment fails", async () => {
    mockedUpdate.mockRejectedValue(new Error("assign boom"));
    renderTab();

    await waitFor(() =>
      expect(screen.getByLabelText("Add agents")).not.toBeDisabled(),
    );
    fireEvent.click(screen.getByLabelText("Add agents"));
    fireEvent.click(await screen.findByText("ungrouped-02"));

    expect(await screen.findByRole("alert")).toHaveTextContent(/assign boom/);
  });
});
