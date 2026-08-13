import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";

import { AgentCard } from "./AgentCard";

import type { Agent } from "@/types/agent";

const baseAgent: Agent = {
  id: "11111111-1111-1111-1111-111111111111",
  name: "web-01",
  status: "online",
  last_seen: new Date().toISOString(),
  version: "",
  labels: { "host.name": "web-01" },
  discovery_source: "otlp",
};

describe("AgentCard — suspected duplicate badge (backlog #5)", () => {
  it("shows the 'Likely duplicate' badge when the agent is a suspected duplicate", () => {
    const agent: Agent = {
      ...baseAgent,
      suspected_duplicate_of: {
        of_agent_id: "22222222-2222-2222-2222-222222222222",
        of_agent_name: "collector-1",
        reason:
          'telemetry-only agent, same host.name "web-01" as an OpAMP-managed agent',
      },
    };
    render(<AgentCard agent={agent} onClick={() => {}} />);
    expect(screen.getByText("Likely duplicate")).toBeInTheDocument();
  });

  it("shows no duplicate badge when the agent is not a suspected duplicate", () => {
    render(<AgentCard agent={baseAgent} onClick={() => {}} />);
    expect(screen.queryByText("Likely duplicate")).not.toBeInTheDocument();
  });

  it("hides Squadron-internal reserved labels from the label chips", () => {
    const agent: Agent = {
      ...baseAgent,
      labels: {
        "host.name": "web-01",
        "squadron.io/duplicate-dismissed": "true",
      },
    };
    render(<AgentCard agent={agent} onClick={() => {}} />);
    // The reserved dismiss marker must not surface as an operator-facing chip.
    expect(screen.queryByText(/duplicate-dismissed/)).not.toBeInTheDocument();
  });
});
