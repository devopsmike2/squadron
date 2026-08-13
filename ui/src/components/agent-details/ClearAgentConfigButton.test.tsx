// Vitest coverage for the "Clear config (fall back to group)" action on the
// agent Overview. The agents API module is mocked at the boundary, and
// window.confirm is stubbed so the confirm gate can be exercised deterministically.

import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterAll, beforeEach, describe, expect, it, vi } from "vitest";

import { ClearAgentConfigButton } from "./ClearAgentConfigButton";

import { clearAgentConfig } from "@/api/agents";
import type { Agent } from "@/types/agent";

vi.mock("@/api/agents", () => ({
  clearAgentConfig: vi.fn(),
}));

const mockClear = vi.mocked(clearAgentConfig);

// Build a minimal Agent with the fields the button reads.
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
    config_intent: {
      source: "agent",
      config_id: "cfg-1",
      version: 1,
      hash: "abc",
      updated_at: "2026-08-10T00:00:00Z",
    },
    ...overrides,
  };
}

describe("ClearAgentConfigButton", () => {
  // Spy once with an inferred type; annotating with ReturnType<typeof vi.spyOn>
  // trips TS2322 under the pinned toolchain (the generic MockInstance is not
  // assignable from the concrete confirm-signature spy). Reset per test.
  const confirmSpy = vi.spyOn(window, "confirm");

  beforeEach(() => {
    mockClear.mockReset();
    confirmSpy.mockReset();
    confirmSpy.mockReturnValue(true);
  });

  afterAll(() => {
    confirmSpy.mockRestore();
  });

  it("is disabled with an explanation when there is no agent-scoped config", () => {
    render(
      <ClearAgentConfigButton
        agent={makeAgent({
          config_intent: {
            source: "group",
            config_id: "cfg-g",
            version: 2,
            hash: "def",
            updated_at: "2026-08-10T00:00:00Z",
          },
        })}
      />,
    );

    const button = screen.getByRole("button", {
      name: /clear config/i,
    });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute(
      "title",
      expect.stringContaining("no agent-scoped config"),
    );
    expect(mockClear).not.toHaveBeenCalled();
  });

  it("is disabled with an explanation when the agent is in no group", () => {
    render(
      <ClearAgentConfigButton
        agent={makeAgent({ group_id: undefined, group_name: undefined })}
      />,
    );

    const button = screen.getByRole("button", { name: /clear config/i });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute(
      "title",
      expect.stringContaining("no group"),
    );
  });

  it("clears the config and refetches on success", async () => {
    mockClear.mockResolvedValueOnce({
      cleared: true,
      fell_back_to: "group",
      group_id: "grp-1",
      pushed: true,
      message: "Cleared the agent config. The group config is now in effect.",
    });
    const onCleared = vi.fn();

    render(
      <ClearAgentConfigButton agent={makeAgent()} onCleared={onCleared} />,
    );

    const button = screen.getByRole("button", { name: /clear config/i });
    expect(button).toBeEnabled();
    await userEvent.click(button);

    expect(confirmSpy).toHaveBeenCalledOnce();
    expect(mockClear).toHaveBeenCalledWith("agent-1");
    await waitFor(() => expect(onCleared).toHaveBeenCalledOnce());
    await waitFor(() =>
      expect(
        screen.getByText(/group config is now in effect/i),
      ).toBeInTheDocument(),
    );
  });

  it("does nothing when the confirm is dismissed", async () => {
    confirmSpy.mockReturnValue(false);
    const onCleared = vi.fn();

    render(
      <ClearAgentConfigButton agent={makeAgent()} onCleared={onCleared} />,
    );

    await userEvent.click(
      screen.getByRole("button", { name: /clear config/i }),
    );

    expect(confirmSpy).toHaveBeenCalledOnce();
    expect(mockClear).not.toHaveBeenCalled();
    expect(onCleared).not.toHaveBeenCalled();
  });

  it("shows an error alert when the endpoint fails", async () => {
    mockClear.mockRejectedValueOnce(new Error("boom"));

    render(<ClearAgentConfigButton agent={makeAgent()} />);

    await userEvent.click(
      screen.getByRole("button", { name: /clear config/i }),
    );

    await waitFor(() => expect(screen.getByText("boom")).toBeInTheDocument());
  });
});
