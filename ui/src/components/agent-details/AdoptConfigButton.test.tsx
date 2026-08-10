// Vitest coverage for the "Convert to template" (adopt effective config)
// button on the agent Config view.
//
// The agents API module is mocked at the boundary so the button can be
// exercised without a Squadron server. Rendered inside a MemoryRouter
// because the success state renders a <Link> to the new config.

import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AdoptConfigButton } from "./AdoptConfigButton";

import { adoptAgentConfig } from "@/api/agents";

vi.mock("@/api/agents", () => ({
  adoptAgentConfig: vi.fn(),
}));

const mockAdopt = vi.mocked(adoptAgentConfig);

const renderButton = (props: { agentId: string; effectiveConfig?: string }) =>
  render(
    <MemoryRouter>
      <AdoptConfigButton {...props} />
    </MemoryRouter>,
  );

describe("AdoptConfigButton", () => {
  beforeEach(() => {
    mockAdopt.mockReset();
  });

  it("is disabled with an explanatory tooltip when there is no effective config", () => {
    renderButton({ agentId: "agent-1", effectiveConfig: undefined });

    const button = screen.getByRole("button", { name: /convert to template/i });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute(
      "title",
      expect.stringContaining("has not reported an effective config"),
    );
    expect(mockAdopt).not.toHaveBeenCalled();
  });

  it("is disabled when the effective config is only whitespace", () => {
    renderButton({ agentId: "agent-1", effectiveConfig: "   \n  " });
    expect(
      screen.getByRole("button", { name: /convert to template/i }),
    ).toBeDisabled();
  });

  it("calls the adopt endpoint and links to the new config on success", async () => {
    mockAdopt.mockResolvedValueOnce({
      config: {
        id: "cfg-123",
        name: "web-collector-adopted",
        config_hash: "abc",
        content: "receivers: {}",
        version: 1,
        created_at: "2026-08-10T00:00:00Z",
      },
      redacted: false,
    });

    renderButton({
      agentId: "agent-1",
      effectiveConfig: "receivers:\n  otlp:\n",
    });

    const button = screen.getByRole("button", { name: /convert to template/i });
    expect(button).toBeEnabled();
    await userEvent.click(button);

    expect(mockAdopt).toHaveBeenCalledWith("agent-1");

    await waitFor(() => {
      expect(screen.getByText(/template created/i)).toBeInTheDocument();
    });
    expect(screen.getByText("web-collector-adopted")).toBeInTheDocument();

    const link = screen.getByRole("link", { name: /view config/i });
    expect(link).toHaveAttribute("href", "/configs/cfg-123/edit");
  });

  it("surfaces the redaction note when the reported config looks redacted", async () => {
    mockAdopt.mockResolvedValueOnce({
      config: {
        id: "cfg-9",
        name: "brownfield-adopted",
        config_hash: "def",
        content: "exporters:\n  otlp:\n    headers:\n      auth: <redacted>\n",
        version: 1,
        created_at: "2026-08-10T00:00:00Z",
      },
      redacted: true,
      note: "The reported effective config appears to contain redacted secret values.",
    });

    renderButton({ agentId: "agent-2", effectiveConfig: "exporters: {}" });

    await userEvent.click(
      screen.getByRole("button", { name: /convert to template/i }),
    );

    await waitFor(() => {
      expect(
        screen.getByText(/contain redacted secret values/i),
      ).toBeInTheDocument();
    });
  });

  it("shows an error alert when the endpoint fails", async () => {
    mockAdopt.mockRejectedValueOnce(new Error("boom"));

    renderButton({ agentId: "agent-3", effectiveConfig: "receivers: {}" });

    await userEvent.click(
      screen.getByRole("button", { name: /convert to template/i }),
    );

    await waitFor(() => {
      expect(screen.getByText("boom")).toBeInTheDocument();
    });
  });
});
