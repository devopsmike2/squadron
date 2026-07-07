import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { AuditVerifyPanel } from "./AuditVerifyPanel";

import {
  getFleetVerify,
  getTenantVerify,
  downloadAttestation,
} from "@/api/auditVerify";
import { useAuditVerifyCapabilities } from "@/hooks/useAuditVerifyCapabilities";

// Radix Select needs these in jsdom if a Select is interacted with.
if (!Element.prototype.hasPointerCapture)
  Element.prototype.hasPointerCapture = () => false;
if (!Element.prototype.scrollIntoView)
  Element.prototype.scrollIntoView = () => {};

vi.mock("@/hooks/useAuditVerifyCapabilities", () => ({
  useAuditVerifyCapabilities: vi.fn(),
}));
vi.mock("@/api/auditVerify", () => ({
  getFleetVerify: vi.fn(),
  getTenantVerify: vi.fn(),
  downloadAttestation: vi.fn(),
}));
vi.mock("@/api/tenants", () => ({
  listTenants: vi.fn().mockResolvedValue([{ tenant_id: "beta", name: "Beta" }]),
}));

const mockedCaps = vi.mocked(useAuditVerifyCapabilities);

const renderPanel = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <AuditVerifyPanel />
    </SWRConfig>,
  );

const enterprise = {
  capabilities: {
    cross_tenant: true,
    fleet: true,
    sealed_attestation: true,
    patterns: ["tenant", "fleet", "attest"],
  },
  isEnterprise: true,
  isLoading: false,
};

describe("AuditVerifyPanel — enterprise tamper-evidence UI", () => {
  beforeEach(() => vi.clearAllMocks());

  it("OSS (capabilities 404 → not enterprise): shows the enterprise gate, no controls", () => {
    mockedCaps.mockReturnValue({
      capabilities: undefined,
      isEnterprise: false,
      isLoading: false,
    });
    renderPanel();
    expect(
      screen.getByTestId("audit-verify-enterprise-gate"),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("audit-verify-controls")).toBeNull();
  });

  it("enterprise fleet verify: renders a row per tenant with the broken one's first break seq", async () => {
    mockedCaps.mockReturnValue(enterprise);
    vi.mocked(getFleetVerify).mockResolvedValue({
      verified_at: "2026-07-07T00:00:00Z",
      ok: false,
      tenants: [
        {
          tenant: "alpha",
          ok: true,
          rows_verified: 120,
          head_seq: 120,
          head_row_hash: "abc",
        },
        {
          tenant: "beta",
          ok: false,
          rows_verified: 44,
          head_seq: 80,
          head_row_hash: "def",
          first_break_seq: 45,
        },
      ],
    });

    renderPanel();
    fireEvent.click(screen.getByTestId("audit-verify-fleet"));

    await waitFor(() => expect(getFleetVerify).toHaveBeenCalled());
    expect(
      await screen.findByTestId("audit-verify-results"),
    ).toBeInTheDocument();
    expect(screen.getByTestId("audit-verify-row-alpha")).toBeInTheDocument();
    expect(screen.getByTestId("audit-verify-row-beta")).toBeInTheDocument();
    // The broken tenant surfaces its first_break_seq.
    expect(screen.getByTestId("audit-verify-row-beta")).toHaveTextContent(
      /first break at seq 45/i,
    );
  });

  it("targeted verify + download: verifies a named tenant then downloads its attestation", async () => {
    mockedCaps.mockReturnValue(enterprise);
    vi.mocked(getTenantVerify).mockResolvedValue({
      tenant: "beta",
      ok: true,
      rows_verified: 44,
      head_seq: 80,
      head_row_hash: "def",
    });
    vi.mocked(downloadAttestation).mockResolvedValue(undefined);

    renderPanel();
    // Pick the named tenant from the cross-tenant picker.
    fireEvent.click(screen.getByRole("combobox"));
    fireEvent.click(await screen.findByText("Beta (beta)"));
    // Run the targeted verify.
    fireEvent.click(screen.getByTestId("audit-verify-run"));

    await waitFor(() => expect(getTenantVerify).toHaveBeenCalledWith("beta"));
    // Download button appears once a specific tenant is verified.
    fireEvent.click(await screen.findByTestId("audit-verify-download"));
    await waitFor(() =>
      expect(downloadAttestation).toHaveBeenCalledWith("beta"),
    );
  });

  it("surfaces an error when fleet verification fails", async () => {
    mockedCaps.mockReturnValue(enterprise);
    vi.mocked(getFleetVerify).mockRejectedValue(new Error("verify boom"));

    renderPanel();
    fireEvent.click(screen.getByTestId("audit-verify-fleet"));

    expect(await screen.findByTestId("audit-verify-error")).toHaveTextContent(
      /verify boom/i,
    );
  });
});
