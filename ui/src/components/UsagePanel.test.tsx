import { render, screen } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { UsagePanel } from "./UsagePanel";

import { listTenants } from "@/api/tenants";
import { getOwnUsage } from "@/api/usage";
import { useUsageCapabilities } from "@/hooks/useUsageCapabilities";

if (!Element.prototype.hasPointerCapture)
  Element.prototype.hasPointerCapture = () => false;

vi.mock("@/hooks/useUsageCapabilities", () => ({
  useUsageCapabilities: vi.fn(),
}));
vi.mock("@/api/usage", () => ({
  getOwnUsage: vi.fn(),
  getTenantUsage: vi.fn(),
}));
vi.mock("@/api/tenants", () => ({
  listTenants: vi.fn().mockResolvedValue([{ tenant_id: "beta", name: "Beta" }]),
}));

const mockedCaps = vi.mocked(useUsageCapabilities);

const renderPanel = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <UsagePanel />
    </SWRConfig>,
  );

describe("UsagePanel — enterprise per-tenant usage (ADR 0023)", () => {
  beforeEach(() => vi.clearAllMocks());

  it("OSS (capabilities 404): shows the enterprise gate, no controls", () => {
    mockedCaps.mockReturnValue({
      capabilities: undefined,
      isEnterprise: false,
      isLoading: false,
    });
    renderPanel();
    expect(screen.getByTestId("usage-enterprise-gate")).toBeInTheDocument();
    expect(screen.queryByTestId("usage-controls")).toBeNull();
  });

  it("enterprise: loads own-tenant usage + shows the cross-tenant picker", async () => {
    mockedCaps.mockReturnValue({
      capabilities: { cross_tenant: true, metrics: ["agents", "rollouts"] },
      isEnterprise: true,
      isLoading: false,
    });
    vi.mocked(getOwnUsage).mockResolvedValue({
      tenant: "acme",
      agents: 42,
      rollouts: 3,
      cross_tenant: false,
    });

    renderPanel();

    expect(screen.getByTestId("usage-tenant-picker")).toBeInTheDocument();
    expect(listTenants).toHaveBeenCalled();
    // own usage auto-loads and renders the counts.
    expect(await screen.findByTestId("usage-summary")).toBeInTheDocument();
    expect(screen.getByText("42")).toBeInTheDocument();
    expect(screen.getByText("tenant: acme")).toBeInTheDocument();
  });

  it("enterprise WITHOUT cross_tenant: no tenant picker", async () => {
    mockedCaps.mockReturnValue({
      capabilities: { cross_tenant: false, metrics: ["agents", "rollouts"] },
      isEnterprise: true,
      isLoading: false,
    });
    vi.mocked(getOwnUsage).mockResolvedValue({
      tenant: "acme",
      agents: 1,
      rollouts: 0,
      cross_tenant: false,
    });
    renderPanel();
    expect(screen.queryByTestId("usage-tenant-picker")).toBeNull();
    expect(await screen.findByTestId("usage-summary")).toBeInTheDocument();
  });
});
