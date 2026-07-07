import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { BudgetsPanel } from "./BudgetsPanel";

import { listBudgets, putTenantBudget } from "@/api/budgets";
import { useBudgetCapabilities } from "@/hooks/useBudgetCapabilities";

if (!Element.prototype.hasPointerCapture)
  Element.prototype.hasPointerCapture = () => false;
if (!Element.prototype.scrollIntoView)
  Element.prototype.scrollIntoView = () => {};

vi.mock("@/hooks/useBudgetCapabilities", () => ({
  useBudgetCapabilities: vi.fn(),
}));
vi.mock("@/api/budgets", () => ({
  listBudgets: vi.fn(),
  putTenantBudget: vi.fn(),
  deleteTenantBudget: vi.fn(),
}));

const mockedCaps = vi.mocked(useBudgetCapabilities);

const renderPanel = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <BudgetsPanel />
    </SWRConfig>,
  );

describe("BudgetsPanel — enterprise per-tenant budget admin", () => {
  beforeEach(() => vi.clearAllMocks());

  it("OSS (capabilities 404): shows the enterprise gate, no controls", () => {
    mockedCaps.mockReturnValue({
      capabilities: undefined,
      isEnterprise: false,
      isLoading: false,
    });
    renderPanel();
    expect(screen.getByTestId("budgets-enterprise-gate")).toBeInTheDocument();
    expect(screen.queryByTestId("budgets-controls")).toBeNull();
  });

  it("enterprise own-tenant: renders the caller's budget row", async () => {
    mockedCaps.mockReturnValue({
      capabilities: { cross_tenant: false, scopes: ["budgets:read"] },
      isEnterprise: true,
      isLoading: false,
    });
    vi.mocked(listBudgets).mockResolvedValue([
      { tenant: "default", max_rows: 0, updated_at: "" },
    ]);

    renderPanel();

    expect(screen.getByTestId("budgets-controls")).toBeInTheDocument();
    expect(
      await screen.findByTestId("budgets-maxrows-default"),
    ).toBeInTheDocument();
    expect(screen.getByText("tenant: default")).toBeInTheDocument();
  });

  it("enterprise cross-tenant write: saves an edited budget", async () => {
    mockedCaps.mockReturnValue({
      capabilities: {
        cross_tenant: true,
        scopes: ["budgets:read", "budgets:write", "budgets:cross_tenant"],
      },
      isEnterprise: true,
      isLoading: false,
    });
    vi.mocked(listBudgets).mockResolvedValue([
      { tenant: "acme", max_rows: 1000, updated_at: "" },
      { tenant: "beta", max_rows: 0, updated_at: "" },
    ]);
    vi.mocked(putTenantBudget).mockResolvedValue({
      tenant: "acme",
      max_rows: 5000,
      updated_at: "",
    });

    renderPanel();

    const input = await screen.findByTestId("budgets-maxrows-acme");
    const user = userEvent.setup();
    await user.clear(input);
    await user.type(input, "5000");
    await user.click(screen.getByTestId("budgets-save-acme"));

    expect(putTenantBudget).toHaveBeenCalledWith("acme", 5000);
  });

  it("enterprise cross-tenant write: surfaces the inline server error", async () => {
    mockedCaps.mockReturnValue({
      capabilities: {
        cross_tenant: true,
        scopes: ["budgets:read", "budgets:write", "budgets:cross_tenant"],
      },
      isEnterprise: true,
      isLoading: false,
    });
    vi.mocked(listBudgets).mockResolvedValue([
      { tenant: "acme", max_rows: 1000, updated_at: "" },
    ]);
    vi.mocked(putTenantBudget).mockRejectedValue(
      new Error("max_rows must be positive"),
    );

    renderPanel();

    const input = await screen.findByTestId("budgets-maxrows-acme");
    const user = userEvent.setup();
    await user.clear(input);
    await user.type(input, "5000");
    await user.click(screen.getByTestId("budgets-save-acme"));

    expect(await screen.findByTestId("budgets-error-acme")).toHaveTextContent(
      "max_rows must be positive",
    );
  });
});
