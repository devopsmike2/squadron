// Vitest coverage for the v0.89.62 #689 Stream 87 (slice-1 chunk 2)
// Discovery dashboard page. Mirrors the DiscoveryOCI / DiscoveryAzure
// posture:
//   - SWR cache is wiped per-test via a fresh SWRConfig provider.
//   - Network mocked at the @/api/discoverySummary module boundary.
//   - Test naming follows the design doc §7 contract item 9 list so
//     a future arc-spanning audit can grep TestDiscoveryDashboard_*.

import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { SWRConfig } from "swr";
import { beforeEach, describe, expect, it, vi } from "vitest";

import DiscoveryPage from "./Discovery";

import {
  getDiscoverySummary,
  type DiscoverySummary,
  type ProviderSummary,
} from "@/api/discoverySummary";
import {
  getTraceCoverage,
  type ProviderTraceCoverage,
  type TraceCoverage,
} from "@/api/discoveryTraceCoverage";

vi.mock("@/api/discoverySummary", async () => {
  const actual = await vi.importActual<typeof import("@/api/discoverySummary")>(
    "@/api/discoverySummary",
  );
  return {
    ...actual,
    getDiscoverySummary: vi.fn(),
  };
});

vi.mock("@/api/discoveryTraceCoverage", async () => {
  const actual = await vi.importActual<
    typeof import("@/api/discoveryTraceCoverage")
  >("@/api/discoveryTraceCoverage");
  return {
    ...actual,
    getTraceCoverage: vi.fn(),
  };
});

const mockedGetDiscoverySummary = vi.mocked(getDiscoverySummary);
const mockedGetTraceCoverage = vi.mocked(getTraceCoverage);

// makeProviderTrace builds a ProviderTraceCoverage with sensible
// defaults the per-test partials override.
function makeProviderTrace(
  over: Partial<ProviderTraceCoverage> = {},
): ProviderTraceCoverage {
  return {
    inventory_count: 10,
    emitting_count: 5,
    coverage_pct: 50,
    strong_match_pct: 100,
    weak_match_pct: 0,
    // v0.89.82 (#713 Stream 111, Trace integration slice 2 chunk 3) —
    // default to zero so existing trace-coverage tests still pass and
    // the sub-indicator stays hidden unless a test explicitly opts in.
    pending_trace_emission_count: 0,
    ...over,
  };
}

// makeTraceCoverage builds a fully populated TraceCoverage payload.
// Tests override per-provider state via the `providers` partial.
function makeTraceCoverage(
  over: Partial<TraceCoverage> = {},
  providersOver: Partial<
    Record<keyof TraceCoverage["providers"], Partial<ProviderTraceCoverage>>
  > = {},
): TraceCoverage {
  const base: TraceCoverage = {
    providers: {
      aws: makeProviderTrace({ coverage_pct: 67 }),
      gcp: makeProviderTrace({ coverage_pct: 40 }),
      azure: makeProviderTrace({ coverage_pct: 55 }),
      oci: makeProviderTrace({ coverage_pct: 30 }),
    },
    totals: {
      inventory_count: 40,
      emitting_count: 20,
      coverage_pct: 50,
    },
  };
  for (const k of Object.keys(providersOver) as Array<
    keyof TraceCoverage["providers"]
  >) {
    base.providers[k] = { ...base.providers[k], ...(providersOver[k] ?? {}) };
  }
  return { ...base, ...over };
}

function renderPage(initialEntries: string[] = ["/discovery"]) {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter initialEntries={initialEntries}>{children}</MemoryRouter>
      </SWRConfig>
    );
  }
  return render(<DiscoveryPage />, { wrapper: Wrapper });
}

// makeProvider builds a ProviderSummary with sensible defaults the
// individual tests override per-field.
function makeProvider(over: Partial<ProviderSummary> = {}): ProviderSummary {
  return {
    enabled: true,
    connection_count: 0,
    instance_count: 0,
    instrumented_count: 0,
    uninstrumented_count: 0,
    recommendation_count: 0,
    ...over,
  };
}

// makeSummary builds a fully populated DiscoverySummary with all four
// providers enabled by default. Tests override per-provider state via
// the `providers` partial.
function makeSummary(
  over: Partial<DiscoverySummary> = {},
  providersOver: Partial<Record<keyof DiscoverySummary["providers"], Partial<ProviderSummary>>> = {},
): DiscoverySummary {
  const base: DiscoverySummary = {
    providers: {
      aws: makeProvider({ connection_count: 3, instance_count: 142, instrumented_count: 89, uninstrumented_count: 53, recommendation_count: 53 }),
      gcp: makeProvider({ connection_count: 1, instance_count: 24, instrumented_count: 18, uninstrumented_count: 6, recommendation_count: 6 }),
      azure: makeProvider({ connection_count: 1, instance_count: 16, instrumented_count: 12, uninstrumented_count: 4, recommendation_count: 4 }),
      oci: makeProvider({ connection_count: 1, instance_count: 16, instrumented_count: 13, uninstrumented_count: 3, recommendation_count: 3 }),
    },
    totals: {
      connection_count: 6,
      instance_count: 198,
      instrumented_count: 132,
      uninstrumented_count: 66,
      recommendation_count: 66,
      coverage_pct: 66.7,
    },
    recent_recommendations: [],
  };
  for (const k of Object.keys(providersOver) as Array<keyof DiscoverySummary["providers"]>) {
    base.providers[k] = { ...base.providers[k], ...(providersOver[k] ?? {}) };
  }
  return { ...base, ...over };
}

describe("DiscoveryDashboard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Default the trace coverage fetch to a benign payload so the
    // existing summary-only tests don't trip on the new fetcher
    // surfacing an unmocked-call error. Per-test overrides land in
    // the trace-coverage-focused tests below.
    mockedGetTraceCoverage.mockResolvedValue(makeTraceCoverage());
  });

  it("TestDiscoveryDashboard_RendersFourProviderCards", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("provider-grid")).toBeInTheDocument();
    });

    // All four provider cards present and enabled.
    for (const p of ["aws", "gcp", "azure", "oci"] as const) {
      const card = screen.getByTestId(`provider-card-${p}`);
      expect(card).toBeInTheDocument();
      expect(card).toHaveAttribute("data-enabled", "true");
    }

    // Per-provider counts wired through.
    expect(screen.getByTestId("provider-aws-connections")).toHaveTextContent(
      "3",
    );
    expect(screen.getByTestId("provider-aws-instances")).toHaveTextContent(
      "142",
    );
    expect(screen.getByTestId("provider-gcp-connections")).toHaveTextContent(
      "1",
    );
    expect(screen.getByTestId("provider-gcp-instances")).toHaveTextContent(
      "24",
    );
    expect(screen.getByTestId("provider-azure-instances")).toHaveTextContent(
      "16",
    );
    expect(screen.getByTestId("provider-oci-instances")).toHaveTextContent(
      "16",
    );

    // Header subtitle reflects totals.
    expect(
      screen.getByText(/Squadron sees 198 resources across 6 connections/i),
    ).toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_EmptyStateWhenNoConnections", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(
      makeSummary(
        {
          totals: {
            connection_count: 0,
            instance_count: 0,
            instrumented_count: 0,
            uninstrumented_count: 0,
            recommendation_count: 0,
            coverage_pct: 0,
          },
          recent_recommendations: [],
        },
        {
          aws: { enabled: false, connection_count: 0, instance_count: 0, instrumented_count: 0, uninstrumented_count: 0, recommendation_count: 0 },
          gcp: { enabled: false, connection_count: 0, instance_count: 0, instrumented_count: 0, uninstrumented_count: 0, recommendation_count: 0 },
          azure: { enabled: false, connection_count: 0, instance_count: 0, instrumented_count: 0, uninstrumented_count: 0, recommendation_count: 0 },
          oci: { enabled: false, connection_count: 0, instance_count: 0, instrumented_count: 0, uninstrumented_count: 0, recommendation_count: 0 },
        },
      ),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("discovery-welcome")).toBeInTheDocument();
    });

    expect(
      screen.getByText(/Welcome to Squadron Discovery/i),
    ).toBeInTheDocument();

    // Four Connect <Provider> buttons present and pointing at the per-
    // provider routes.
    for (const label of ["AWS", "GCP", "AZURE", "OCI"]) {
      const btn = screen.getByRole("link", {
        name: new RegExp(`Connect ${label}`, "i"),
      });
      expect(btn).toBeInTheDocument();
    }

    // Coverage panel + recommendations table NOT rendered when in the
    // welcome state — the dashboard short-circuits to the welcome
    // experience.
    expect(screen.queryByTestId("coverage-ring")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("recommendations-table"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("recommendations-empty"),
    ).not.toBeInTheDocument();
    expect(screen.queryByTestId("provider-grid")).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_CoverageRingColorByThreshold_Green", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(
      makeSummary({
        totals: {
          connection_count: 1,
          instance_count: 100,
          instrumented_count: 90,
          uninstrumented_count: 10,
          recommendation_count: 10,
          coverage_pct: 90,
        },
      }),
    );
    renderPage();
    await waitFor(() => {
      const ring = screen.getByTestId("coverage-ring");
      expect(ring).toHaveAttribute("data-color", "#16a34a");
    });
    expect(screen.getByTestId("coverage-pct")).toHaveTextContent("90%");
  });

  it("TestDiscoveryDashboard_CoverageRingColorByThreshold_Yellow", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(
      makeSummary({
        totals: {
          connection_count: 1,
          instance_count: 100,
          instrumented_count: 65,
          uninstrumented_count: 35,
          recommendation_count: 35,
          coverage_pct: 65,
        },
      }),
    );
    renderPage();
    await waitFor(() => {
      const ring = screen.getByTestId("coverage-ring");
      expect(ring).toHaveAttribute("data-color", "#ca8a04");
    });
    expect(screen.getByTestId("coverage-pct")).toHaveTextContent("65%");
  });

  it("TestDiscoveryDashboard_CoverageRingColorByThreshold_Red", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(
      makeSummary({
        totals: {
          connection_count: 1,
          instance_count: 100,
          instrumented_count: 30,
          uninstrumented_count: 70,
          recommendation_count: 70,
          coverage_pct: 30,
        },
      }),
    );
    renderPage();
    await waitFor(() => {
      const ring = screen.getByTestId("coverage-ring");
      expect(ring).toHaveAttribute("data-color", "#dc2626");
    });
    expect(screen.getByTestId("coverage-pct")).toHaveTextContent("30%");
  });

  it("TestDiscoveryDashboard_RecentRecommendationsTable", async () => {
    const summary = makeSummary({
      recent_recommendations: [
        {
          provider: "aws",
          kind: "ec2-otel-tag",
          resource_id: "i-0abc",
          scope_id: "123456789012",
          region: "us-east-1",
          generated_at: new Date(Date.now() - 60_000).toISOString(),
        },
        {
          provider: "gcp",
          kind: "gce-otel-tag",
          resource_id: "projects/foo/instances/web-1",
          scope_id: "foo-project",
          region: "us-central1",
          generated_at: new Date(Date.now() - 120_000).toISOString(),
        },
        {
          provider: "azure",
          kind: "vm-otel-tag",
          resource_id: "/subscriptions/s/x/web-1",
          scope_id: "sub-uuid",
          region: "eastus",
          generated_at: new Date(Date.now() - 180_000).toISOString(),
        },
        {
          provider: "oci",
          kind: "compute-otel-tag",
          resource_id: "ocid1.instance.oc1..aaaaaa",
          scope_id: "ocid1.tenancy.oc1..aaaaaa",
          region: "us-phoenix-1",
          generated_at: new Date(Date.now() - 240_000).toISOString(),
        },
        {
          provider: "aws",
          kind: "lambda-otel-layer",
          resource_id: "i-0def",
          scope_id: "098765432109",
          region: "us-west-2",
          generated_at: new Date(Date.now() - 300_000).toISOString(),
        },
      ],
    });
    mockedGetDiscoverySummary.mockResolvedValue(summary);
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("recommendations-table")).toBeInTheDocument();
    });

    const table = screen.getByTestId("recommendations-table");
    const rows = within(table).getAllByRole("row");
    // Header row + 5 data rows.
    expect(rows).toHaveLength(6);

    // Per-row content + per-provider badge presence.
    expect(within(table).getByText("i-0abc")).toBeInTheDocument();
    expect(within(table).getByText("ec2-otel-tag")).toBeInTheDocument();
    expect(within(table).getByText("123456789012")).toBeInTheDocument();
    expect(within(table).getByText("us-east-1")).toBeInTheDocument();

    // The 5 rows include 2 AWS badges, 1 each of GCP / AZURE / OCI.
    expect(within(table).getAllByTestId("rec-badge-aws")).toHaveLength(2);
    expect(within(table).getAllByTestId("rec-badge-gcp")).toHaveLength(1);
    expect(within(table).getAllByTestId("rec-badge-azure")).toHaveLength(1);
    expect(within(table).getAllByTestId("rec-badge-oci")).toHaveLength(1);

    // Per-cloud badge text is the uppercase provider label.
    const awsBadges = within(table).getAllByTestId("rec-badge-aws");
    expect(awsBadges[0]).toHaveTextContent("AWS");
    expect(within(table).getByTestId("rec-badge-gcp")).toHaveTextContent("GCP");
    expect(within(table).getByTestId("rec-badge-azure")).toHaveTextContent(
      "AZURE",
    );
    expect(within(table).getByTestId("rec-badge-oci")).toHaveTextContent("OCI");
  });

  it("TestDiscoveryDashboard_RefreshButton_RefetchesSummary", async () => {
    const user = userEvent.setup();
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    renderPage();

    // Initial fetch.
    await waitFor(() => {
      expect(mockedGetDiscoverySummary).toHaveBeenCalledTimes(1);
    });

    const refreshBtn = screen.getByRole("button", {
      name: /Refresh dashboard/i,
    });
    await user.click(refreshBtn);

    await waitFor(() => {
      expect(mockedGetDiscoverySummary).toHaveBeenCalledTimes(2);
    });
  });

  it("TestDiscoveryDashboard_DisabledProvider_RendersConnectCTA", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(
      makeSummary({}, {
        oci: { enabled: false, connection_count: 0, instance_count: 0, instrumented_count: 0, uninstrumented_count: 0, recommendation_count: 0 },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("provider-card-oci")).toBeInTheDocument();
    });

    const ociCard = screen.getByTestId("provider-card-oci");
    expect(ociCard).toHaveAttribute("data-enabled", "false");

    // Muted-style subtext + Connect button visible.
    expect(
      within(ociCard).getByText(/Connect OCI to add to your fleet view/i),
    ).toBeInTheDocument();

    const connectLink = within(ociCard).getByRole("link", {
      name: /Connect OCI/i,
    });
    expect(connectLink).toHaveAttribute("href", "/discovery/oci");

    // The other three providers remain enabled.
    expect(screen.getByTestId("provider-card-aws")).toHaveAttribute(
      "data-enabled",
      "true",
    );
    expect(screen.getByTestId("provider-card-gcp")).toHaveAttribute(
      "data-enabled",
      "true",
    );
    expect(screen.getByTestId("provider-card-azure")).toHaveAttribute(
      "data-enabled",
      "true",
    );
  });

  it("TestDiscoveryDashboard_RecentRecommendationsEmpty_ShowsHint", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("recommendations-empty")).toBeInTheDocument();
    });
    expect(
      screen.getByText(/No recommendations yet/i),
    ).toBeInTheDocument();
  });

  // --- Trace coverage panel (v0.89.76 #707 Stream 105) --------------

  it("TestDiscoveryDashboard_RendersTraceCoveragePanel", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage({
        totals: { inventory_count: 198, emitting_count: 122, coverage_pct: 61.6 },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-panel")).toBeInTheDocument();
    });
    expect(screen.getByTestId("trace-coverage-pct")).toHaveTextContent("61.6%");
    expect(
      screen.getByText(
        /122 of 198 inventoried resources have emitted spans in the last 24h/i,
      ),
    ).toBeInTheDocument();
    // All four provider chips present.
    for (const p of ["aws", "gcp", "azure", "oci"] as const) {
      expect(
        screen.getByTestId(`trace-coverage-chip-${p}`),
      ).toBeInTheDocument();
    }
  });

  it("TestDiscoveryDashboard_TraceCoverageEmptyState", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {
          totals: { inventory_count: 0, emitting_count: 0, coverage_pct: 0 },
        },
        {
          aws: makeProviderTrace({
            inventory_count: 0,
            emitting_count: 0,
            coverage_pct: 0,
          }),
          gcp: makeProviderTrace({
            inventory_count: 0,
            emitting_count: 0,
            coverage_pct: 0,
          }),
          azure: makeProviderTrace({
            inventory_count: 0,
            emitting_count: 0,
            coverage_pct: 0,
          }),
          oci: makeProviderTrace({
            inventory_count: 0,
            emitting_count: 0,
            coverage_pct: 0,
          }),
        },
      ),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-empty")).toBeInTheDocument();
    });
    expect(
      screen.getByText(
        /Run a discovery scan to populate the trace coverage view/i,
      ),
    ).toBeInTheDocument();
    // Headline pct + chip row should NOT render in empty state.
    expect(screen.queryByTestId("trace-coverage-pct")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("trace-coverage-chip-row"),
    ).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_TraceCoverageWeakMatchCaveat", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {},
        {
          gcp: makeProviderTrace({
            coverage_pct: 40,
            strong_match_pct: 75,
            weak_match_pct: 25,
          }),
        },
      ),
    );
    renderPage();

    await waitFor(() => {
      expect(
        screen.getByTestId("trace-coverage-chip-gcp"),
      ).toBeInTheDocument();
    });
    // GCP shows the caveat icon (weak_match_pct=25 > threshold 20).
    expect(
      screen.getByTestId("trace-coverage-caveat-gcp"),
    ).toBeInTheDocument();
    // Other providers (weak_match_pct=0) do NOT show the icon.
    expect(
      screen.queryByTestId("trace-coverage-caveat-aws"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("trace-coverage-caveat-azure"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("trace-coverage-caveat-oci"),
    ).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_TraceCoverageColorByThreshold_Green", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {
          totals: {
            inventory_count: 100,
            emitting_count: 90,
            coverage_pct: 90,
          },
        },
        {
          aws: makeProviderTrace({ coverage_pct: 90 }),
        },
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-panel")).toBeInTheDocument();
    });
    // The trace coverage panel's own ring picks up the green threshold;
    // the per-provider AWS chip color-codes the same way.
    expect(screen.getByTestId("trace-coverage-chip-aws")).toHaveAttribute(
      "data-color",
      "#16a34a",
    );
  });

  it("TestDiscoveryDashboard_TraceCoverageColorByThreshold_Yellow", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {
          totals: {
            inventory_count: 100,
            emitting_count: 65,
            coverage_pct: 65,
          },
        },
        {
          aws: makeProviderTrace({ coverage_pct: 65 }),
        },
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-panel")).toBeInTheDocument();
    });
    expect(screen.getByTestId("trace-coverage-chip-aws")).toHaveAttribute(
      "data-color",
      "#ca8a04",
    );
  });

  it("TestDiscoveryDashboard_TraceCoverageColorByThreshold_Red", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {
          totals: {
            inventory_count: 100,
            emitting_count: 30,
            coverage_pct: 30,
          },
        },
        {
          aws: makeProviderTrace({ coverage_pct: 30 }),
        },
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-panel")).toBeInTheDocument();
    });
    expect(screen.getByTestId("trace-coverage-chip-aws")).toHaveAttribute(
      "data-color",
      "#dc2626",
    );
  });

  // --- Trace coverage pending sub-indicator (v0.89.82 #713 Stream 111) ---
  //
  // Slice-2 chunk 3 surfaces a fleet-wide "instrumented but not
  // emitting" count below the chip row. Renders when the cross-provider
  // sum is non-zero; hides when zero (design doc §10 acceptance test
  // 10).

  it("TestDiscoveryDashboard_TraceCoverageSubIndicator_RendersWhenNonZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {},
        {
          aws: makeProviderTrace({ pending_trace_emission_count: 2 }),
          gcp: makeProviderTrace({ pending_trace_emission_count: 1 }),
          azure: makeProviderTrace({ pending_trace_emission_count: 1 }),
          oci: makeProviderTrace({ pending_trace_emission_count: 1 }),
        },
      ),
    );
    renderPage();

    await waitFor(() => {
      expect(
        screen.getByTestId("trace-coverage-pending-indicator"),
      ).toBeInTheDocument();
    });
    expect(
      screen.getByTestId("trace-coverage-pending-indicator"),
    ).toHaveTextContent(/5 resources/);
    expect(
      screen.getByText(
        /5 resources have the primitive enabled but no recent emission/i,
      ),
    ).toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_TraceCoverageSubIndicator_HiddenWhenZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Default makeTraceCoverage() carries pending_trace_emission_count
    // = 0 on every provider, so the indicator must stay hidden.
    mockedGetTraceCoverage.mockResolvedValue(makeTraceCoverage());
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-panel")).toBeInTheDocument();
    });
    expect(
      screen.queryByTestId("trace-coverage-pending-indicator"),
    ).not.toBeInTheDocument();
  });
});
