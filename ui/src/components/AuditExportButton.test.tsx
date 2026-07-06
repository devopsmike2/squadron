import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { AuditExportButton } from "./AuditExportButton";

import { downloadAuditExport, streamAuditExport } from "@/api/audit";
import { listTenants } from "@/api/tenants";
import { useAuditExportCapabilities } from "@/hooks/useAuditExportCapabilities";

vi.mock("@/hooks/useAuditExportCapabilities", () => ({
  useAuditExportCapabilities: vi.fn(),
}));
vi.mock("@/api/audit", () => ({
  downloadAuditExport: vi.fn().mockResolvedValue(undefined),
  streamAuditExport: vi.fn().mockResolvedValue({ rows: 0, bytes: 0 }),
}));
vi.mock("@/api/tenants", () => ({
  listTenants: vi.fn().mockResolvedValue([{ tenant_id: "beta", name: "Beta" }]),
}));

const mockedCaps = vi.mocked(useAuditExportCapabilities);

const renderButton = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <AuditExportButton filter={{ actor: "operator:x" }} />
    </SWRConfig>,
  );

describe("AuditExportButton — enterprise feature-detect (ADR 0020 6d-part-2)", () => {
  beforeEach(() => vi.clearAllMocks());

  it("OSS (probe 404 → not enterprise): no tenant picker, uses OSS download", async () => {
    mockedCaps.mockReturnValue({
      capabilities: undefined,
      isEnterprise: false,
      isLoading: false,
    });
    renderButton();

    expect(screen.queryByTestId("audit-export-tenant")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /export/i }));
    await waitFor(() => expect(downloadAuditExport).toHaveBeenCalled());
    expect(streamAuditExport).not.toHaveBeenCalled();
    // limit is raised to the store cap.
    expect(downloadAuditExport).toHaveBeenCalledWith(
      expect.objectContaining({ actor: "operator:x", limit: 1000 }),
      "csv",
    );
  });

  it("enterprise + cross_tenant: renders the tenant picker and streams", async () => {
    mockedCaps.mockReturnValue({
      capabilities: { formats: ["csv", "ndjson"], cross_tenant: true },
      isEnterprise: true,
      isLoading: false,
    });
    renderButton();

    expect(screen.getByTestId("audit-export-tenant")).toBeInTheDocument();
    expect(listTenants).toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /export/i }));
    await waitFor(() => expect(streamAuditExport).toHaveBeenCalled());
    expect(downloadAuditExport).not.toHaveBeenCalled();
    // default tenant selection is self → no tenant param.
    expect(streamAuditExport).toHaveBeenCalledWith(
      expect.objectContaining({ actor: "operator:x", limit: 1000 }),
      "csv",
      expect.objectContaining({ tenant: undefined }),
    );
  });

  it("enterprise WITHOUT cross_tenant: no tenant picker", () => {
    mockedCaps.mockReturnValue({
      capabilities: { formats: ["csv", "ndjson"], cross_tenant: false },
      isEnterprise: true,
      isLoading: false,
    });
    renderButton();
    expect(screen.queryByTestId("audit-export-tenant")).toBeNull();
  });
});
