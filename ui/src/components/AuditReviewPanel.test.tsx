import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { AuditReviewPanel } from "./AuditReviewPanel";

import {
  getActorTimeline,
  getResourceAccess,
  getAdminActions,
} from "@/api/auditReview";
import { listTenants } from "@/api/tenants";
import { useAuditReviewCapabilities } from "@/hooks/useAuditReviewCapabilities";

// Radix Select needs these in jsdom if a Select is interacted with.
if (!Element.prototype.hasPointerCapture)
  Element.prototype.hasPointerCapture = () => false;
if (!Element.prototype.scrollIntoView)
  Element.prototype.scrollIntoView = () => {};

vi.mock("@/hooks/useAuditReviewCapabilities", () => ({
  useAuditReviewCapabilities: vi.fn(),
}));
vi.mock("@/api/auditReview", () => ({
  getActorTimeline: vi.fn(),
  getResourceAccess: vi.fn(),
  getAdminActions: vi.fn(),
}));
vi.mock("@/api/tenants", () => ({
  listTenants: vi.fn().mockResolvedValue([{ tenant_id: "beta", name: "Beta" }]),
}));

const mockedCaps = vi.mocked(useAuditReviewCapabilities);

const renderPanel = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <AuditReviewPanel />
    </SWRConfig>,
  );

const enterprise = {
  capabilities: {
    cross_tenant: true,
    rollups: ["by_actor", "by_event_type"],
    patterns: ["query", "actor-timeline", "resource-access", "admin-actions"],
  },
  isEnterprise: true,
  isLoading: false,
};

describe("AuditReviewPanel — enterprise access-review UI (ADR 0022)", () => {
  beforeEach(() => vi.clearAllMocks());

  it("OSS (capabilities 404 → not enterprise): shows the enterprise gate, no controls", () => {
    mockedCaps.mockReturnValue({
      capabilities: undefined,
      isEnterprise: false,
      isLoading: false,
    });
    renderPanel();
    expect(
      screen.getByTestId("audit-review-enterprise-gate"),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("audit-review-controls")).toBeNull();
  });

  it("enterprise: renders controls + cross-tenant picker", () => {
    mockedCaps.mockReturnValue(enterprise);
    renderPanel();
    expect(screen.getByTestId("audit-review-controls")).toBeInTheDocument();
    // cross-tenant picker present for the default (actor) mode.
    expect(screen.getByTestId("audit-review-tenant")).toBeInTheDocument();
    expect(listTenants).toHaveBeenCalled();
  });

  it("enterprise WITHOUT cross_tenant: no cross-tenant picker", () => {
    mockedCaps.mockReturnValue({
      ...enterprise,
      capabilities: { ...enterprise.capabilities, cross_tenant: false },
    });
    renderPanel();
    expect(screen.queryByTestId("audit-review-tenant")).toBeNull();
  });

  it("runs a per-actor timeline and renders the results", async () => {
    mockedCaps.mockReturnValue(enterprise);
    vi.mocked(getActorTimeline).mockResolvedValue({
      events: [
        {
          id: "e1",
          timestamp: "2026-07-06T12:00:00Z",
          actor: "operator:alice",
          event_type: "config.applied",
          target_type: "agent",
          target_id: "a1",
          action: "applied",
        },
      ],
      count: 1,
      truncated: false,
      by_actor: [{ key: "operator:alice", count: 1 }],
      by_event_type: [{ key: "config.applied", count: 1 }],
      cross_tenant: false,
    });

    renderPanel();
    fireEvent.change(screen.getByLabelText("actor"), {
      target: { value: "operator:alice" },
    });
    fireEvent.click(screen.getByTestId("audit-review-run"));

    await waitFor(() =>
      expect(getActorTimeline).toHaveBeenCalledWith(
        "operator:alice",
        expect.objectContaining({ tenant: undefined }),
      ),
    );
    expect(
      await screen.findByTestId("audit-review-results"),
    ).toBeInTheDocument();
    // event_type shows in both the rollup badge and the table; assert the
    // table row via its unique target cell.
    expect(screen.getByText("agent/a1")).toBeInTheDocument();
    expect(getResourceAccess).not.toHaveBeenCalled();
    expect(getAdminActions).not.toHaveBeenCalled();
  });

  it("surfaces a validation error when the actor is empty", async () => {
    mockedCaps.mockReturnValue(enterprise);
    renderPanel();
    fireEvent.click(screen.getByTestId("audit-review-run"));
    expect(await screen.findByTestId("audit-review-error")).toHaveTextContent(
      /actor is required/i,
    );
    expect(getActorTimeline).not.toHaveBeenCalled();
  });
});
