import { render, screen } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { LicenseBanner } from "./LicenseBanner";

import { getLicenseStatus, type LicenseStatus } from "@/api/license";

vi.mock("@/api/license", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/license")>("@/api/license");
  return { ...actual, getLicenseStatus: vi.fn() };
});

const mockGet = vi.mocked(getLicenseStatus);

const renderBanner = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <LicenseBanner />
    </SWRConfig>,
  );

const tick = () => new Promise((r) => setTimeout(r, 0));

describe("LicenseBanner (ADR 0032 S1)", () => {
  beforeEach(() => vi.clearAllMocks());

  it("is silent for the OSS edition", async () => {
    mockGet.mockResolvedValue({
      edition: "oss",
      state: "n/a",
    } as LicenseStatus);
    renderBanner();
    await tick();
    expect(screen.queryByTestId("license-banner")).toBeNull();
  });

  it("is silent for a healthy enterprise license", async () => {
    mockGet.mockResolvedValue({
      edition: "enterprise",
      state: "valid",
      days_remaining: 300,
    } as LicenseStatus);
    renderBanner();
    await tick();
    expect(screen.queryByTestId("license-banner")).toBeNull();
  });

  it("warns when a trial is expiring soon", async () => {
    mockGet.mockResolvedValue({
      edition: "enterprise",
      state: "valid",
      trial: true,
      days_remaining: 5,
    } as LicenseStatus);
    renderBanner();
    await screen.findByTestId("license-banner");
    expect(screen.getByText(/expires in 5 days/i)).toBeTruthy();
    expect(screen.getByRole("link", { name: /manage license/i })).toBeTruthy();
  });

  it("shows an error with a renew link when expired", async () => {
    mockGet.mockResolvedValue({
      edition: "enterprise",
      state: "expired",
    } as LicenseStatus);
    renderBanner();
    await screen.findByTestId("license-banner");
    expect(screen.getByText(/has expired/i)).toBeTruthy();
    expect(screen.getByRole("link", { name: /renew license/i })).toBeTruthy();
  });
});
