// Vitest coverage for the shared WizardShell chrome (v0.89.190).
//
// Scope: the load-bearing footer contract every provider relies on —
// Back is disabled on the first step, Next is gated by canAdvance,
// the final step suppresses Next (its body owns the terminal action),
// and submitting disables navigation. The per-provider page tests
// (DiscoveryGCP / Azure / OCI) exercise the shell transitively through
// real flows; these assertions pin the contract directly so a future
// refactor of the shell can't silently break every wizard at once.

import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { WizardShell } from "./WizardShell";

function renderShell(
  overrides: Partial<React.ComponentProps<typeof WizardShell>> = {},
) {
  const onBack = vi.fn();
  const onNext = vi.fn();
  render(
    <WizardShell
      stepIndex={1}
      stepCount={4}
      stepTitle="Service account"
      canAdvance={true}
      onBack={onBack}
      onNext={onNext}
      {...overrides}
    >
      <p>step body</p>
    </WizardShell>,
  );
  return { onBack, onNext };
}

describe("WizardShell", () => {
  it("renders the step title, progress, and the provided body", () => {
    renderShell();
    expect(screen.getByText("Step 2 of 4")).toBeInTheDocument();
    expect(screen.getByText("step body")).toBeInTheDocument();
    // Title appears in both the progress header and the card heading.
    expect(screen.getAllByText("Service account").length).toBeGreaterThan(0);
  });

  it("disables Back on the first step", () => {
    renderShell({ stepIndex: 0 });
    expect(screen.getByRole("button", { name: /Back/i })).toBeDisabled();
  });

  it("gates Next on canAdvance", () => {
    const { onNext } = renderShell({ canAdvance: false });
    const next = screen.getByRole("button", { name: /Next/i });
    expect(next).toBeDisabled();
    fireEvent.click(next);
    expect(onNext).not.toHaveBeenCalled();
  });

  it("advances when Next is clicked and enabled", () => {
    const { onNext } = renderShell({ canAdvance: true });
    fireEvent.click(screen.getByRole("button", { name: /Next/i }));
    expect(onNext).toHaveBeenCalledTimes(1);
  });

  it("suppresses Next on the final step (body owns the terminal action)", () => {
    renderShell({ stepIndex: 3, stepCount: 4 });
    expect(screen.queryByRole("button", { name: /Next/i })).toBeNull();
  });

  it("disables navigation while submitting", () => {
    renderShell({ stepIndex: 1, submitting: true });
    expect(screen.getByRole("button", { name: /Back/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Next/i })).toBeDisabled();
  });
});
