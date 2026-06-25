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
  fetchSpanQuality,
  type ProviderSpanQuality,
  type SpanQualityResponse,
} from "@/api/discoverySpanQuality";
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
import {
  fetchWorkloadHealth,
  type ProviderWorkloadHealth,
  type WorkloadHealthResponse,
} from "@/api/discoveryWorkloadHealth";

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

vi.mock("@/api/discoverySpanQuality", async () => {
  const actual = await vi.importActual<
    typeof import("@/api/discoverySpanQuality")
  >("@/api/discoverySpanQuality");
  return {
    ...actual,
    fetchSpanQuality: vi.fn(),
  };
});

vi.mock("@/api/discoveryWorkloadHealth", async () => {
  const actual = await vi.importActual<
    typeof import("@/api/discoveryWorkloadHealth")
  >("@/api/discoveryWorkloadHealth");
  return {
    ...actual,
    fetchWorkloadHealth: vi.fn(),
  };
});

const mockedGetDiscoverySummary = vi.mocked(getDiscoverySummary);
const mockedGetTraceCoverage = vi.mocked(getTraceCoverage);
const mockedFetchSpanQuality = vi.mocked(fetchSpanQuality);
const mockedFetchWorkloadHealth = vi.mocked(fetchWorkloadHealth);

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
    // v0.89.92 (#725 Stream 123, Serverless tier slice 1 chunk 5) —
    // default to zero so SERVERLESS chip stays hidden.
    serverless_pct: 0,
    // v0.89.97 (#731 Stream 129, Orchestration tier slice 1 chunk 4)
    // — default to zero so ORCH chip stays hidden in tests that
    // don't explicitly opt in.
    orchestration_pct: 0,
    // v0.89.102 (#738 Stream 136, Event source tier slice 1 chunk 5)
    // — default to zero so EVT chip stays hidden in tests that don't
    // explicitly opt in.
    event_source_pct: 0,
    // v0.89.107 (#745 Stream 143, Event source tier slice 2 chunk 5)
    // — default to zero so the EVT chip propagation suffix stays
    // hidden in tests that don't explicitly opt in.
    propagation_pct: 0,
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
    // v0.89.92 (#725 Stream 123, Serverless tier slice 1 chunk 5).
    serverless_count: 0,
    // v0.89.97 (#731 Stream 129, Orchestration tier slice 1 chunk 4).
    orchestration_count: 0,
    // v0.89.102 (#738 Stream 136, Event source tier slice 1 chunk 5).
    event_source_count: 0,
    ...over,
  };
}

// makeSummary builds a fully populated DiscoverySummary with all four
// providers enabled by default. Tests override per-provider state via
// the `providers` partial.
function makeSummary(
  over: Partial<DiscoverySummary> = {},
  providersOver: Partial<
    Record<keyof DiscoverySummary["providers"], Partial<ProviderSummary>>
  > = {},
): DiscoverySummary {
  const base: DiscoverySummary = {
    providers: {
      aws: makeProvider({
        connection_count: 3,
        instance_count: 142,
        instrumented_count: 89,
        uninstrumented_count: 53,
        recommendation_count: 53,
      }),
      gcp: makeProvider({
        connection_count: 1,
        instance_count: 24,
        instrumented_count: 18,
        uninstrumented_count: 6,
        recommendation_count: 6,
      }),
      azure: makeProvider({
        connection_count: 1,
        instance_count: 16,
        instrumented_count: 12,
        uninstrumented_count: 4,
        recommendation_count: 4,
      }),
      oci: makeProvider({
        connection_count: 1,
        instance_count: 16,
        instrumented_count: 13,
        uninstrumented_count: 3,
        recommendation_count: 3,
      }),
    },
    totals: {
      connection_count: 6,
      instance_count: 198,
      instrumented_count: 132,
      uninstrumented_count: 66,
      recommendation_count: 66,
      serverless_count: 0,
      orchestration_count: 0,
      event_source_count: 0,
      coverage_pct: 66.7,
    },
    recent_recommendations: [],
  };
  for (const k of Object.keys(providersOver) as Array<
    keyof DiscoverySummary["providers"]
  >) {
    base.providers[k] = { ...base.providers[k], ...(providersOver[k] ?? {}) };
  }
  return { ...base, ...over };
}

// --- Span quality factories (v0.89.87 #718 Stream 116) -------------

// makeProviderSpanQuality builds a ProviderSpanQuality with sensible
// defaults the per-test partials override. Defaults are all-zero so
// the SPAN QUALITY panel stays hidden in tests that don't opt in
// (matching the §10 acceptance test 12 contract).
function makeProviderSpanQuality(
  over: Partial<ProviderSpanQuality> = {},
): ProviderSpanQuality {
  return {
    resource_count: 0,
    resources_with_issues: 0,
    orphan_pct: 0,
    missing_attr_pct: 0,
    attr_mismatch_pct: 0,
    // Slice 2 (v0.89.110) — the panel hides when ALL FIVE percentages
    // are zero; the makeSpanQuality default keeps the panel hidden so
    // tests opt in explicitly.
    malformed_traceparent_pct: 0,
    missing_traceparent_on_child_pct: 0,
    // Sampling rate slice 1 chunk 3 (v0.89.124, #764 Stream 162) —
    // sixth percentage. Default zero so the panel stays hidden in
    // tests that don't explicitly opt in.
    sampling_too_aggressive_pct: 0,
    ...over,
  };
}

// makeSpanQuality builds a fully populated SpanQualityResponse. Tests
// override per-provider state via the `providers` partial.
function makeSpanQuality(
  over: Partial<SpanQualityResponse> = {},
  providersOver: Partial<
    Record<keyof SpanQualityResponse["providers"], Partial<ProviderSpanQuality>>
  > = {},
): SpanQualityResponse {
  const base: SpanQualityResponse = {
    providers: {
      aws: makeProviderSpanQuality(),
      gcp: makeProviderSpanQuality(),
      azure: makeProviderSpanQuality(),
      oci: makeProviderSpanQuality(),
    },
    totals: {
      resource_count: 0,
      resources_with_issues: 0,
      orphan_pct: 0,
      missing_attr_pct: 0,
      attr_mismatch_pct: 0,
      malformed_traceparent_pct: 0,
      missing_traceparent_on_child_pct: 0,
      sampling_too_aggressive_pct: 0,
    },
  };
  for (const k of Object.keys(providersOver) as Array<
    keyof SpanQualityResponse["providers"]
  >) {
    base.providers[k] = {
      ...base.providers[k],
      ...(providersOver[k] ?? {}),
    };
  }
  return { ...base, ...over };
}

// --- Workload health factories (v0.89.132 #772 Stream 170) ---------

// makeProviderWorkloadHealth builds a ProviderWorkloadHealth with
// sensible defaults the per-test partials override. Defaults are
// all-zero so the WORKLOAD HEALTH panel stays hidden in tests that
// don't opt in (matching the §8 acceptance tests 8 + 9 hide
// conditions).
function makeProviderWorkloadHealth(
  over: Partial<ProviderWorkloadHealth> = {},
): ProviderWorkloadHealth {
  return {
    serverless_resource_count: 0,
    cold_start_exceeded_count: 0,
    cold_start_exceeded_pct: 0,
    sampling_too_aggressive_count: 0,
    sampling_too_aggressive_pct: 0,
    error_rate_spike_count: 0,
    error_rate_spike_pct: 0,
    any_issue_count: 0,
    any_issue_pct: 0,
    ...over,
  };
}

// makeWorkloadHealth builds a fully populated WorkloadHealthResponse
// with every provider at the all-zero baseline. Tests override
// per-provider state via the `providers` partial. The totals row is
// independently overridable since the panel hide check inspects only
// totals — tests that pin specific behavior want to set totals
// directly without re-deriving from per-provider counts.
function makeWorkloadHealth(
  over: Partial<WorkloadHealthResponse> = {},
  providersOver: Partial<
    Record<
      keyof WorkloadHealthResponse["providers"],
      Partial<ProviderWorkloadHealth>
    >
  > = {},
): WorkloadHealthResponse {
  const base: WorkloadHealthResponse = {
    providers: {
      aws: makeProviderWorkloadHealth(),
      gcp: makeProviderWorkloadHealth(),
      azure: makeProviderWorkloadHealth(),
      oci: makeProviderWorkloadHealth(),
    },
    totals: makeProviderWorkloadHealth(),
  };
  for (const k of Object.keys(providersOver) as Array<
    keyof WorkloadHealthResponse["providers"]
  >) {
    base.providers[k] = {
      ...base.providers[k],
      ...(providersOver[k] ?? {}),
    };
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
    // Default span quality to all-zero so the SPAN QUALITY panel
    // stays hidden in tests that don't opt in (mirrors the trace-
    // coverage benign default).
    mockedFetchSpanQuality.mockResolvedValue(makeSpanQuality());
    // Default workload health to all-zero so the WORKLOAD HEALTH
    // panel stays hidden in tests that don't opt in (matches the
    // §8 acceptance tests 8 + 9 hide conditions). Per-test
    // overrides land in the workload-health-focused tests below.
    mockedFetchWorkloadHealth.mockResolvedValue(makeWorkloadHealth());
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
            serverless_count: 0,
            orchestration_count: 0,
            event_source_count: 0,
            coverage_pct: 0,
          },
          recent_recommendations: [],
        },
        {
          aws: {
            enabled: false,
            connection_count: 0,
            instance_count: 0,
            instrumented_count: 0,
            uninstrumented_count: 0,
            recommendation_count: 0,
          },
          gcp: {
            enabled: false,
            connection_count: 0,
            instance_count: 0,
            instrumented_count: 0,
            uninstrumented_count: 0,
            recommendation_count: 0,
          },
          azure: {
            enabled: false,
            connection_count: 0,
            instance_count: 0,
            instrumented_count: 0,
            uninstrumented_count: 0,
            recommendation_count: 0,
          },
          oci: {
            enabled: false,
            connection_count: 0,
            instance_count: 0,
            instrumented_count: 0,
            uninstrumented_count: 0,
            recommendation_count: 0,
          },
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
          serverless_count: 0,
          orchestration_count: 0,
          event_source_count: 0,
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
          serverless_count: 0,
          orchestration_count: 0,
          event_source_count: 0,
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
          serverless_count: 0,
          orchestration_count: 0,
          event_source_count: 0,
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
      makeSummary(
        {},
        {
          oci: {
            enabled: false,
            connection_count: 0,
            instance_count: 0,
            instrumented_count: 0,
            uninstrumented_count: 0,
            recommendation_count: 0,
          },
        },
      ),
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
    expect(screen.getByText(/No recommendations yet/i)).toBeInTheDocument();
  });

  // --- Trace coverage panel (v0.89.76 #707 Stream 105) --------------

  it("TestDiscoveryDashboard_RendersTraceCoveragePanel", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage({
        totals: {
          inventory_count: 198,
          emitting_count: 122,
          coverage_pct: 61.6,
        },
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
      expect(screen.getByTestId("trace-coverage-chip-gcp")).toBeInTheDocument();
    });
    // GCP shows the caveat icon (weak_match_pct=25 > threshold 20).
    expect(screen.getByTestId("trace-coverage-caveat-gcp")).toBeInTheDocument();
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

  // --- Tier coverage chip line (v0.89.97 #731 Stream 129) -----------
  //
  // Orchestration tier slice 1 chunk 4 adds a per-tier chip line
  // beneath the per-provider chip row. Slice 1 lands the ORCH column
  // only. The chip line stays hidden when no tier reports a non-zero
  // pct (design doc §7 acceptance test).

  it("TestDiscoveryDashboard_TraceCoveragePanel_ORCHColumnRendersWhenNonZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {},
        {
          aws: makeProviderTrace({
            coverage_pct: 67,
            emitting_count: 10,
            orchestration_pct: 12,
          }),
        },
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByTestId("trace-coverage-tier-chip-orch"),
      ).toBeInTheDocument();
    });
    expect(
      screen.getByTestId("trace-coverage-tier-chip-orch"),
    ).toHaveTextContent(/ORCH/);
    expect(
      screen.getByTestId("trace-coverage-tier-chip-orch"),
    ).toHaveTextContent(/12%/);
  });

  it("TestDiscoveryDashboard_TraceCoveragePanel_ORCHColumnHiddenWhenZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Default makeTraceCoverage() carries orchestration_pct = 0 on
    // every provider, so the ORCH chip must stay hidden.
    mockedGetTraceCoverage.mockResolvedValue(makeTraceCoverage());
    renderPage();
    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-panel")).toBeInTheDocument();
    });
    expect(
      screen.queryByTestId("trace-coverage-tier-chip-orch"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("trace-coverage-tier-chip-row"),
    ).not.toBeInTheDocument();
  });

  // --- EVT chip column (v0.89.102 #738 Stream 136) ------------------
  //
  // Event source tier slice 1 chunk 5 extends the per-tier chip row
  // with an EVT column. Same hide-when-zero pattern as ORCH; the
  // chip line stays hidden when no tier reports a non-zero pct.

  it("TestDiscoveryDashboard_TraceCoveragePanel_EVTColumnRendersWhenNonZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {},
        {
          aws: makeProviderTrace({
            coverage_pct: 67,
            emitting_count: 10,
            event_source_pct: 8,
          }),
        },
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByTestId("trace-coverage-tier-chip-evt"),
      ).toBeInTheDocument();
    });
    expect(
      screen.getByTestId("trace-coverage-tier-chip-evt"),
    ).toHaveTextContent(/EVT/);
    expect(
      screen.getByTestId("trace-coverage-tier-chip-evt"),
    ).toHaveTextContent(/8%/);
  });

  it("TestDiscoveryDashboard_TraceCoveragePanel_EVTColumnHiddenWhenZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Default makeTraceCoverage() carries event_source_pct = 0 on every
    // provider, so the EVT chip must stay hidden.
    mockedGetTraceCoverage.mockResolvedValue(makeTraceCoverage());
    renderPage();
    await waitFor(() => {
      expect(screen.getByTestId("trace-coverage-panel")).toBeInTheDocument();
    });
    expect(
      screen.queryByTestId("trace-coverage-tier-chip-evt"),
    ).not.toBeInTheDocument();
  });

  // --- EVT chip propagation suffix (v0.89.107 #745 Stream 143) ------
  //
  // Event source tier slice 2 chunk 5 extends the EVT chip with a
  // "(prop N%)" suffix sourced from the per-provider propagation_pct
  // weighted average. The suffix hides when the fleet-wide
  // event_source_count is zero — per the spec §7 hide-when-no-events
  // rule.

  it("TestDiscoveryDashboard_EVTColumnGainsPropagationSuffix", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(
      makeSummary(
        {
          totals: {
            connection_count: 6,
            instance_count: 198,
            instrumented_count: 132,
            uninstrumented_count: 66,
            recommendation_count: 66,
            serverless_count: 0,
            orchestration_count: 0,
            // Non-zero fleet-wide event_source_count drives the
            // propagation suffix on the EVT chip.
            event_source_count: 4,
            coverage_pct: 66.7,
          },
        },
        {
          aws: { event_source_count: 4 },
        },
      ),
    );
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {},
        {
          aws: makeProviderTrace({
            coverage_pct: 67,
            emitting_count: 10,
            event_source_pct: 8,
            propagation_pct: 23,
          }),
        },
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByTestId("trace-coverage-tier-chip-evt"),
      ).toBeInTheDocument();
    });
    const suffix = screen.getByTestId("trace-coverage-tier-chip-evt-suffix");
    expect(suffix).toHaveTextContent(/prop/);
    expect(suffix).toHaveTextContent(/23%/);
  });

  it("TestDiscoveryDashboard_PropagationSuffixHiddenWhenEventSourceCountZero", async () => {
    // event_source_count = 0 fleet-wide → the propagation suffix on
    // the EVT chip stays hidden even when at least one provider has a
    // non-zero propagation_pct (defensive — backend shouldn't emit
    // non-zero propagation_pct without underlying event sources, but
    // the UI gates on the inventory count for the operator-facing
    // "(prop N%)" surface).
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedGetTraceCoverage.mockResolvedValue(
      makeTraceCoverage(
        {},
        {
          aws: makeProviderTrace({
            coverage_pct: 67,
            emitting_count: 10,
            event_source_pct: 8,
            propagation_pct: 23,
          }),
        },
      ),
    );
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByTestId("trace-coverage-tier-chip-evt"),
      ).toBeInTheDocument();
    });
    // The EVT chip itself renders (event_source_pct = 8 keeps it
    // visible), but the propagation suffix must NOT render since the
    // fleet-wide event_source_count is zero.
    expect(
      screen.queryByTestId("trace-coverage-tier-chip-evt-suffix"),
    ).not.toBeInTheDocument();
  });

  // --- SPAN QUALITY panel (v0.89.87 #718 Stream 116) -----------------
  //
  // Span quality slice 1 chunk 3 surfaces a 3-column health grid below
  // the TRACE COVERAGE panel. The panel renders when ANY of the three
  // totals percentages is non-zero (test 11) and hides entirely when
  // all three are zero (test 12, design doc §10 acceptance contract).
  // Each column is a Link that deeplinks to /discovery/aws#recommendations
  // with the matching kind in the hash — the slice-2-chunk-3 trace
  // emission filter chip pattern carries over.

  it("TestDiscoveryDashboard_SpanQualityPanel_RendersWhenNonZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality(
        {
          totals: {
            resource_count: 142,
            resources_with_issues: 38,
            orphan_pct: 4.1,
            missing_attr_pct: 6.3,
            attr_mismatch_pct: 2.0,
            malformed_traceparent_pct: 0,
            missing_traceparent_on_child_pct: 0,
            sampling_too_aggressive_pct: 0,
          },
        },
        {
          aws: {
            resource_count: 47,
            resources_with_issues: 12,
            orphan_pct: 3.2,
            missing_attr_pct: 8.1,
            attr_mismatch_pct: 1.7,
          },
        },
      ),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("span-quality-panel")).toBeInTheDocument();
    });
    expect(screen.getByTestId("span-quality-pct-orphan")).toHaveTextContent(
      "4.1%",
    );
    expect(
      screen.getByTestId("span-quality-pct-missing-attrs"),
    ).toHaveTextContent("6.3%");
    expect(screen.getByTestId("span-quality-pct-mismatch")).toHaveTextContent(
      "2.0%",
    );

    // The three columns are present and deep-link to the matching
    // recommendation kind via a hash fragment.
    const orphanCol = screen.getByTestId("span-quality-column-orphan");
    expect(orphanCol).toHaveAttribute("data-kind", "span-quality-orphan-trace");
    expect(orphanCol).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-orphan-trace",
    );
  });

  it("TestDiscoveryDashboard_SpanQualityPanel_HiddenWhenAllZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Default makeSpanQuality() carries all-zero totals — the panel
    // must stay out of the DOM entirely (§10 acceptance test 12).
    mockedFetchSpanQuality.mockResolvedValue(makeSpanQuality());
    renderPage();

    // Wait for the dashboard body to land so we're not asserting
    // against a transient pre-fetch state.
    await waitFor(() => {
      expect(screen.getByTestId("provider-grid")).toBeInTheDocument();
    });
    expect(screen.queryByTestId("span-quality-panel")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("span-quality-pct-orphan"),
    ).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_SpanQualityPanel_ColumnClickDeepLinksToFilteredRecommendations", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality({
        totals: {
          resource_count: 50,
          resources_with_issues: 20,
          orphan_pct: 7.0,
          missing_attr_pct: 12.0,
          attr_mismatch_pct: 3.0,
          malformed_traceparent_pct: 0,
          missing_traceparent_on_child_pct: 0,
          sampling_too_aggressive_pct: 0,
        },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("span-quality-panel")).toBeInTheDocument();
    });

    // Each column is a Link whose href carries the kind hash. The
    // chunk-4 filter chip reads the hash on mount.
    const missing = screen.getByTestId("span-quality-column-missing-attrs");
    expect(missing.tagName).toBe("A");
    expect(missing).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-missing-resource-attrs",
    );
    expect(screen.getByTestId("span-quality-column-mismatch")).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-attribute-mismatch",
    );
  });

  // --- Slice 2 (v0.89.110) — 5-column grid + traceparent columns --

  it("TestDiscoveryDashboard_SpanQualityPanel_FiveColumnsRenderWhenNonZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality({
        totals: {
          resource_count: 142,
          resources_with_issues: 50,
          orphan_pct: 3.2,
          missing_attr_pct: 6.3,
          attr_mismatch_pct: 2.0,
          malformed_traceparent_pct: 0.8,
          missing_traceparent_on_child_pct: 4.1,
          sampling_too_aggressive_pct: 0,
        },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("span-quality-panel")).toBeInTheDocument();
    });
    // All five percentage tiles render.
    expect(screen.getByTestId("span-quality-pct-orphan")).toHaveTextContent(
      "3.2%",
    );
    expect(
      screen.getByTestId("span-quality-pct-missing-attrs"),
    ).toHaveTextContent("6.3%");
    expect(screen.getByTestId("span-quality-pct-mismatch")).toHaveTextContent(
      "2.0%",
    );
    expect(
      screen.getByTestId("span-quality-pct-malformed-traceparent"),
    ).toHaveTextContent("0.8%");
    expect(
      screen.getByTestId("span-quality-pct-missing-traceparent"),
    ).toHaveTextContent("4.1%");
  });

  it("TestDiscoveryDashboard_SpanQualityPanel_HiddenWhenAllFiveZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Default makeSpanQuality() carries all-zero on every slice-1 +
    // slice-2 percentage; the panel must stay out of the DOM.
    mockedFetchSpanQuality.mockResolvedValue(makeSpanQuality());
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("provider-grid")).toBeInTheDocument();
    });
    expect(screen.queryByTestId("span-quality-panel")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("span-quality-pct-malformed-traceparent"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("span-quality-pct-missing-traceparent"),
    ).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_SpanQualityPanel_MalformedTraceparentColumnDeepLinks", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Only the new malformed-traceparent percentage is non-zero — the
    // panel must still appear (slice 2 extended hide-check) and the
    // malformed column must deep-link to the matching kind.
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality({
        totals: {
          resource_count: 12,
          resources_with_issues: 3,
          orphan_pct: 0,
          missing_attr_pct: 0,
          attr_mismatch_pct: 0,
          malformed_traceparent_pct: 1.5,
          missing_traceparent_on_child_pct: 0,
          sampling_too_aggressive_pct: 0,
        },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("span-quality-panel")).toBeInTheDocument();
    });
    const col = screen.getByTestId("span-quality-column-malformed-traceparent");
    expect(col.tagName).toBe("A");
    expect(col).toHaveAttribute(
      "data-kind",
      "span-quality-traceparent-malformed",
    );
    expect(col).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-traceparent-malformed",
    );
  });

  it("TestDiscoveryDashboard_SpanQualityPanel_MissingOnChildColumnDeepLinks", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality({
        totals: {
          resource_count: 12,
          resources_with_issues: 3,
          orphan_pct: 0,
          missing_attr_pct: 0,
          attr_mismatch_pct: 0,
          malformed_traceparent_pct: 0,
          missing_traceparent_on_child_pct: 5.7,
          sampling_too_aggressive_pct: 0,
        },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("span-quality-panel")).toBeInTheDocument();
    });
    const col = screen.getByTestId("span-quality-column-missing-traceparent");
    expect(col.tagName).toBe("A");
    expect(col).toHaveAttribute(
      "data-kind",
      "span-quality-traceparent-missing",
    );
    expect(col).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-traceparent-missing",
    );
  });

  // --- Sampling rate slice 1 chunk 3 (v0.89.124, #764 Stream 162) ---
  //
  // Acceptance tests 14 + 15 — SPAN QUALITY panel grows from 5 to 6
  // columns. The new sampling-too-aggressive column renders when the
  // backend reports a non-zero sampling_too_aggressive_pct, and the
  // panel hides entirely when all six percentages are zero.

  it("TestDiscoveryDashboard_SpanQualityPanel_SixColumnsRenderWhenNonZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality({
        totals: {
          resource_count: 142,
          resources_with_issues: 50,
          orphan_pct: 3.2,
          missing_attr_pct: 6.3,
          attr_mismatch_pct: 2.0,
          malformed_traceparent_pct: 0.8,
          missing_traceparent_on_child_pct: 4.1,
          sampling_too_aggressive_pct: 12.5,
        },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("span-quality-panel")).toBeInTheDocument();
    });
    // All six percentage tiles render.
    expect(screen.getByTestId("span-quality-pct-orphan")).toHaveTextContent(
      "3.2%",
    );
    expect(
      screen.getByTestId("span-quality-pct-missing-attrs"),
    ).toHaveTextContent("6.3%");
    expect(screen.getByTestId("span-quality-pct-mismatch")).toHaveTextContent(
      "2.0%",
    );
    expect(
      screen.getByTestId("span-quality-pct-malformed-traceparent"),
    ).toHaveTextContent("0.8%");
    expect(
      screen.getByTestId("span-quality-pct-missing-traceparent"),
    ).toHaveTextContent("4.1%");
    expect(
      screen.getByTestId("span-quality-pct-sampling-too-aggressive"),
    ).toHaveTextContent("12.5%");
  });

  it("TestDiscoveryDashboard_SpanQualityPanel_HiddenWhenAllSixZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Default makeSpanQuality() carries all-zero on every slice-1 +
    // slice-2 + sampling-rate-slice-1 percentage; the panel must stay
    // out of the DOM.
    mockedFetchSpanQuality.mockResolvedValue(makeSpanQuality());
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("provider-grid")).toBeInTheDocument();
    });
    expect(screen.queryByTestId("span-quality-panel")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("span-quality-pct-sampling-too-aggressive"),
    ).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_SpanQualityPanel_SamplingColumnDeepLinks", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Only the new sampling-too-aggressive percentage is non-zero —
    // the panel must still appear (extended hide-check) and the
    // sampling column must deep-link to the matching kind.
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality({
        totals: {
          resource_count: 24,
          resources_with_issues: 4,
          orphan_pct: 0,
          missing_attr_pct: 0,
          attr_mismatch_pct: 0,
          malformed_traceparent_pct: 0,
          missing_traceparent_on_child_pct: 0,
          sampling_too_aggressive_pct: 12.5,
        },
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("span-quality-panel")).toBeInTheDocument();
    });
    const col = screen.getByTestId(
      "span-quality-column-sampling-too-aggressive",
    );
    expect(col.tagName).toBe("A");
    expect(col).toHaveAttribute(
      "data-kind",
      "span-quality-sampling-too-aggressive",
    );
    expect(col).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-sampling-too-aggressive",
    );
  });

  // --- Workload Health panel (v0.89.132 #772 Stream 170, chunk 1) ---

  it("TestDiscoveryDashboard_WorkloadHealthPanel_RendersWhenAnyPercentageNonZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Inventory present, cold-start fires, the other two zero — the
    // §8 acceptance test 10 contract says the panel still appears.
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 142,
          cold_start_exceeded_count: 12,
          cold_start_exceeded_pct: 8.5,
          any_issue_count: 12,
          any_issue_pct: 8.5,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("workload-health-panel")).toBeInTheDocument();
    });
    expect(
      screen.getByTestId("workload-health-pct-cold-start"),
    ).toHaveTextContent("8.5%");
    expect(
      screen.getByTestId("workload-health-pct-sampling"),
    ).toHaveTextContent("0.0%");
    expect(
      screen.getByTestId("workload-health-pct-error-rate"),
    ).toHaveTextContent("0.0%");
  });

  it("TestDiscoveryDashboard_WorkloadHealthPanel_HiddenWhenServerlessResourceCountZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // §8 acceptance test 8 — no serverless inventory means the panel
    // hides entirely even if the per-pct fields would somehow be set
    // (an out-of-sync server shape). We model the clean case where
    // serverless_resource_count is zero.
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 0,
          cold_start_exceeded_pct: 0,
          sampling_too_aggressive_pct: 0,
          error_rate_spike_pct: 0,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("provider-grid")).toBeInTheDocument();
    });
    expect(
      screen.queryByTestId("workload-health-panel"),
    ).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_WorkloadHealthPanel_HiddenWhenAllThreePctZero", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // §8 acceptance test 9 — inventory exists but every detection
    // declined to fire. The panel hides regardless of the
    // serverless_resource_count value because the operator has
    // nothing actionable to see.
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 142,
          cold_start_exceeded_pct: 0,
          sampling_too_aggressive_pct: 0,
          error_rate_spike_pct: 0,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("provider-grid")).toBeInTheDocument();
    });
    expect(
      screen.queryByTestId("workload-health-panel"),
    ).not.toBeInTheDocument();
  });

  it("TestDiscoveryDashboard_WorkloadHealthPanel_ColdStartButtonDeepLinks", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 142,
          cold_start_exceeded_count: 12,
          cold_start_exceeded_pct: 8.5,
          any_issue_count: 12,
          any_issue_pct: 8.5,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("workload-health-panel")).toBeInTheDocument();
    });
    const col = screen.getByTestId("workload-health-cold-start");
    expect(col).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:cold-start",
    );
    expect(col).toHaveAttribute("data-kind", "cold-start");
  });

  it("TestDiscoveryDashboard_WorkloadHealthPanel_SamplingButtonDeepLinks", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 142,
          sampling_too_aggressive_count: 8,
          sampling_too_aggressive_pct: 5.6,
          any_issue_count: 8,
          any_issue_pct: 5.6,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("workload-health-panel")).toBeInTheDocument();
    });
    const col = screen.getByTestId("workload-health-sampling");
    expect(col).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-sampling-too-aggressive",
    );
  });

  it("TestDiscoveryDashboard_WorkloadHealthPanel_ErrorRateButtonDeepLinks", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 142,
          error_rate_spike_count: 5,
          error_rate_spike_pct: 3.5,
          any_issue_count: 5,
          any_issue_pct: 3.5,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("workload-health-panel")).toBeInTheDocument();
    });
    const col = screen.getByTestId("workload-health-error-rate");
    expect(col).toHaveAttribute(
      "href",
      "/discovery/aws#recommendations:span-quality-error-rate-spike",
    );
  });

  it("TestDiscoveryDashboard_WorkloadHealthPanel_FooterShowsAnyIssueCount", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // §8 acceptance test 14 — footer count must equal the
    // any_issue_count from the endpoint (UNION rule), not the sum of
    // the per-diagnostic counts.
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 142,
          cold_start_exceeded_count: 12,
          cold_start_exceeded_pct: 8.5,
          sampling_too_aggressive_count: 8,
          sampling_too_aggressive_pct: 5.6,
          error_rate_spike_count: 5,
          error_rate_spike_pct: 3.5,
          any_issue_count: 22,
          any_issue_pct: 15.5,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("workload-health-panel")).toBeInTheDocument();
    });
    expect(
      screen.getByTestId("workload-health-any-issue-count"),
    ).toHaveTextContent("22");
    expect(screen.getByTestId("workload-health-footer")).toHaveTextContent(
      "22 / 142 (15.5%)",
    );
  });

  it("TestDiscoveryDashboard_WorkloadHealthPanel_PlacedBetweenTraceCoverageAndSpanQuality", async () => {
    mockedGetDiscoverySummary.mockResolvedValue(makeSummary());
    // Pin EVERY panel as visible so the DOM order assertion is
    // meaningful.
    mockedGetTraceCoverage.mockResolvedValue(makeTraceCoverage());
    mockedFetchSpanQuality.mockResolvedValue(
      makeSpanQuality({
        totals: {
          resource_count: 142,
          resources_with_issues: 12,
          orphan_pct: 4.0,
          missing_attr_pct: 0,
          attr_mismatch_pct: 0,
          malformed_traceparent_pct: 0,
          missing_traceparent_on_child_pct: 0,
          sampling_too_aggressive_pct: 0,
        },
      }),
    );
    mockedFetchWorkloadHealth.mockResolvedValue(
      makeWorkloadHealth({
        totals: makeProviderWorkloadHealth({
          serverless_resource_count: 142,
          cold_start_exceeded_count: 12,
          cold_start_exceeded_pct: 8.5,
          any_issue_count: 12,
          any_issue_pct: 8.5,
        }),
      }),
    );
    renderPage();

    await waitFor(() => {
      expect(screen.getByTestId("workload-health-panel")).toBeInTheDocument();
    });

    const trace = screen.getByTestId("trace-coverage-panel");
    const workload = screen.getByTestId("workload-health-panel");
    const span = screen.getByTestId("span-quality-panel");

    // Use the DOM bitmask to assert TRACE COVERAGE precedes WORKLOAD
    // HEALTH precedes SPAN QUALITY. compareDocumentPosition returns
    // Node.DOCUMENT_POSITION_FOLLOWING (4) when the argument is a
    // later sibling.
    expect(
      trace.compareDocumentPosition(workload) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      workload.compareDocumentPosition(span) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});
