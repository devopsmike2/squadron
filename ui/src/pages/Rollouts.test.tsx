import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";

import { RolloutCard } from "./Rollouts";

import { applyScope, selectorValueFor } from "@/lib/rolloutSelectors";
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

// Slice 3 — the facet quick-pick (Environment/Cluster) upserts into a
// label-mode stage's selector rows via the pure applyScope/selectorValueFor
// helpers. AND semantics: env and cluster coexist as separate rows.
describe("label-mode facet quick-pick (applyScope / selectorValueFor)", () => {
  const ENV = "deployment.environment";
  const CLUSTER = "k8s.cluster.name";

  it("replaces the blank placeholder with the picked env row", () => {
    const rows = applyScope([{ key: "", value: "" }], ENV, "prod");
    expect(rows).toEqual([{ key: ENV, value: "prod" }]);
  });

  it("keeps env and cluster as distinct AND'd rows", () => {
    let rows = applyScope([{ key: "", value: "" }], ENV, "prod");
    rows = applyScope(rows, CLUSTER, "us-east-1");
    expect(rows).toEqual([
      { key: ENV, value: "prod" },
      { key: CLUSTER, value: "us-east-1" },
    ]);
    expect(selectorValueFor(rows, ENV)).toBe("prod");
    expect(selectorValueFor(rows, CLUSTER)).toBe("us-east-1");
  });

  it("re-picking a key overwrites its value rather than duplicating", () => {
    let rows = applyScope([{ key: "", value: "" }], ENV, "prod");
    rows = applyScope(rows, ENV, "staging");
    expect(rows).toEqual([{ key: ENV, value: "staging" }]);
  });

  it("clearing a key removes its row but preserves hand-typed rows", () => {
    let rows: { key: string; value: string }[] = [
      { key: ENV, value: "prod" },
      { key: "host.name", value: "canary-1" },
    ];
    rows = applyScope(rows, ENV, "");
    expect(rows).toEqual([{ key: "host.name", value: "canary-1" }]);
  });

  it("clearing the last remaining row leaves one blank placeholder", () => {
    const rows = applyScope([{ key: ENV, value: "prod" }], ENV, "");
    expect(rows).toEqual([{ key: "", value: "" }]);
  });

  it("selectorValueFor returns '' for an unset key", () => {
    expect(selectorValueFor([{ key: "", value: "" }], ENV)).toBe("");
  });
});
