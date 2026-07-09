import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";

import { RolloutCard } from "./Rollouts";

import type { Rollout } from "@/types/rollout";

// ADR 0029 — the rollout card must render N-of-M approval progress (k/N)
// for a pending_approval rollout that needs more than one distinct
// approver, and must leave the single-approver (v0.47) UX untouched.

const baseRollout = (overrides: Partial<Rollout>): Rollout => ({
  id: "ro-1",
  name: "test rollout",
  group_id: "g-1",
  target_config_id: "cfg-1",
  stages: [{ mode: "percent", percentage: 100, dwell_seconds: 60 }],
  abort_criteria: { max_drifted_agents: 0 },
  state: "pending_approval",
  current_stage: 0,
  requested_by: "alice@example.com",
  created_at: "2026-07-01T00:00:00Z",
  updated_at: "2026-07-01T00:00:00Z",
  ...overrides,
});

const noop = () => {};

const renderCard = (r: Rollout) =>
  render(
    <RolloutCard
      rollout={r}
      groupName="team-a"
      configLabel={() => "cfg v1"}
      onAbort={noop}
      onPauseResume={noop}
      onApprove={noop}
      onReject={noop}
      onRollBack={noop}
    />,
  );

describe("RolloutCard — N-of-M approval progress (ADR 0029)", () => {
  it("renders '1 of 3' progress for a pending_approval rollout needing 3 approvers", () => {
    renderCard(baseRollout({ required_approvals: 3, approver_count: 1 }));

    const progress = screen.getByTestId("approval-progress");
    expect(progress).toBeInTheDocument();
    expect(progress).toHaveTextContent("Approvals: 1 of 3");
    // 2 more distinct approvers are still needed.
    expect(progress).toHaveTextContent("2 more distinct approvers needed");

    const bar = screen.getByRole("progressbar");
    expect(bar).toHaveAttribute("aria-valuenow", "1");
    expect(bar).toHaveAttribute("aria-valuemax", "3");
  });

  it("treats a missing approver_count as 0 ('0 of 2')", () => {
    renderCard(baseRollout({ required_approvals: 2 }));

    const progress = screen.getByTestId("approval-progress");
    expect(progress).toHaveTextContent("Approvals: 0 of 2");
  });

  it("keeps the simple single-approver UX when required_approvals is 1 (no k/N clutter)", () => {
    renderCard(baseRollout({ required_approvals: 1 }));

    expect(screen.queryByTestId("approval-progress")).toBeNull();
    expect(
      screen.getByText(/Waiting on a second approver/i),
    ).toBeInTheDocument();
  });

  it("keeps the simple single-approver UX when required_approvals is unset", () => {
    renderCard(baseRollout({}));

    expect(screen.queryByTestId("approval-progress")).toBeNull();
    expect(
      screen.getByText(/Waiting on a second approver/i),
    ).toBeInTheDocument();
  });
});
