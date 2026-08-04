import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";

import { EnterpriseUpgradePrompt } from "./EnterpriseUpgradePrompt";

describe("EnterpriseUpgradePrompt (ADR 0032 S1c)", () => {
  it("renders title, body, and an upgrade CTA link", () => {
    render(
      <EnterpriseUpgradePrompt testId="x-gate" title="Feature X is enterprise">
        Body copy.
      </EnterpriseUpgradePrompt>,
    );
    expect(screen.getByTestId("x-gate")).toBeTruthy();
    expect(screen.getByText("Feature X is enterprise")).toBeTruthy();
    expect(screen.getByText("Body copy.")).toBeTruthy();
    const cta = screen.getByTestId(
      "enterprise-upgrade-cta",
    ) as HTMLAnchorElement;
    expect(cta.getAttribute("href")).toBeTruthy();
    expect(cta.textContent).toMatch(/upgrade to enterprise/i);
  });
});
