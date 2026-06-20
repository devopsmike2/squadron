// Vitest coverage for the v0.85 Stream 2E /discovery/aws page.
//
// Scope: the tab structure, the Account empty + populated states, the
// wizard dialog open path, and the Inventory tab's Run-scan -> result-
// panel happy path. These tests do NOT exercise the underlying network
// — the discovery module is mocked at the module boundary so the page
// can be rendered in isolation without an Squadron server.
//
// SWR's cache is per-Provider; each test wraps the page in a fresh
// SWRConfig with provider: () => new Map() so cached connection
// lookups from a prior test don't leak into the next one.
//
// Radix UI shim: Radix Select / Tabs branch on PointerEvent capture
// methods that jsdom does not implement. Polyfill them at import time
// so userEvent.click on these components doesn't throw before reaching
// the user-visible behavior. Setup applies once per test module.

import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { SWRConfig } from "swr";
import { beforeEach, describe, expect, it, vi } from "vitest";

import DiscoveryAWSPage from "./DiscoveryAWS";

import {
  generateAWSRecommendations,
  listAWSConnections,
  runAWSScan,
  type CloudConnection,
  type GenerateRecommendationsResponse,
  type ScanResult,
} from "@/api/discovery";
import type { Recommendation } from "@/api/recommendations";

// Mock the discovery API module. The page imports the four named
// exports we care about — listAWSConnections, runAWSScan,
// saveAWSConnection, validateAWSConnection — plus the type defs the
// test fixtures use. The mock replaces all of them with vi.fn so each
// test can configure per-call behavior via mockResolvedValueOnce.
// vi.mock hoists above the imports at transform time, so the imported
// references end up bound to the mocked module.
// jsdom polyfill for Radix Select / Tooltip pointer-capture lookups.
// Without these the components throw `target.hasPointerCapture is not a
// function` as soon as the user clicks the trigger. Patched at module
// scope so every test in this file picks up the shim.
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false;
}
if (!Element.prototype.releasePointerCapture) {
  Element.prototype.releasePointerCapture = () => {};
}
if (!Element.prototype.setPointerCapture) {
  Element.prototype.setPointerCapture = () => {};
}
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

vi.mock("@/api/discovery", async () => {
  const actual = await vi.importActual<typeof import("@/api/discovery")>(
    "@/api/discovery",
  );
  return {
    ...actual,
    listAWSConnections: vi.fn(),
    runAWSScan: vi.fn(),
    saveAWSConnection: vi.fn(),
    validateAWSConnection: vi.fn(),
    generateAWSRecommendations: vi.fn(),
  };
});

const mockedListAWSConnections = vi.mocked(listAWSConnections);
const mockedRunAWSScan = vi.mocked(runAWSScan);
const mockedGenerateAWSRecommendations = vi.mocked(generateAWSRecommendations);

// renderPage wraps the page in a fresh SWRConfig so each test starts
// with an empty cache. Without this, the second test in the file would
// see the prior test's mocked connection list.
function renderPage() {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        {children}
      </SWRConfig>
    );
  }
  return render(<DiscoveryAWSPage />, { wrapper: Wrapper });
}

const sampleConnections: CloudConnection[] = [
  {
    account_id: "123456789012",
    display_name: "Prod AWS",
    regions: ["us-east-1"],
    created_at: new Date().toISOString(),
  },
  {
    account_id: "987654321098",
    display_name: "Staging AWS",
    regions: ["us-west-2"],
    created_at: new Date().toISOString(),
  },
];

const sampleScan: ScanResult = {
  scan_id: "scan-uuid",
  scan_started_at: new Date().toISOString(),
  scan_completed_at: new Date().toISOString(),
  account_id: "123456789012",
  provider: "aws",
  regions: ["us-east-1"],
  compute: [
    {
      resource_id: "i-aaa",
      instance_type: "t3.micro",
      tags: { Name: "web-1" },
      has_otel: true,
      os_family: "linux",
      region: "us-east-1",
    },
  ],
  functions: [
    {
      resource_id: "arn:aws:lambda:us-east-1:123:function:hello",
      name: "hello",
      runtime: "python3.11",
      has_otel_layer: false,
      region: "us-east-1",
    },
  ],
  // Slice 2 (v0.87) — one fully-covered RDS row plus one PI-only row so
  // the test verifies both lever-badge states render.
  databases: [
    {
      resource_id: "arn:aws:rds:us-east-1:123:db:db-covered",
      engine: "postgres",
      engine_version: "15.4",
      instance_class: "db.r6g.large",
      performance_insights_enabled: true,
      enhanced_monitoring_enabled: true,
      region: "us-east-1",
      tags: {},
    },
    {
      resource_id: "arn:aws:rds:us-east-1:123:db:db-pi-only",
      engine: "mysql",
      engine_version: "8.0",
      instance_class: "db.t3.medium",
      performance_insights_enabled: true,
      enhanced_monitoring_enabled: false,
      region: "us-east-1",
      tags: {},
    },
  ],
  instrumented_count: 2,
  uninstrumented_count: 2,
  partial: false,
};

describe("DiscoveryAWSPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders all three tabs", async () => {
    mockedListAWSConnections.mockResolvedValue({ connections: [] });
    renderPage();
    // Tabs render as buttons with role tab.
    expect(screen.getByRole("tab", { name: /Account/i })).toBeInTheDocument();
    expect(
      screen.getByRole("tab", { name: /Inventory/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("tab", { name: /Recommendations/i }),
    ).toBeInTheDocument();
    // Wait for the SWR fetch to settle so the test's teardown doesn't
    // race with an in-flight state update. Silences the "update to
    // AccountTab inside a test was not wrapped in act" warning.
    await waitFor(() => {
      expect(
        screen.getByText(/No accounts connected yet/i),
      ).toBeInTheDocument();
    });
  });

  it("Account tab shows empty state when no connections", async () => {
    mockedListAWSConnections.mockResolvedValue({ connections: [] });
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByText(/No accounts connected yet/i),
      ).toBeInTheDocument();
    });
    // Brand-line reinforcement at the empty state — Squadron's
    // read-only posture is the first thing a new operator reads.
    expect(
      screen.getByText(/never holds your AWS write credentials/i),
    ).toBeInTheDocument();
  });

  it("Account tab shows connection cards when populated", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    expect(screen.getByText("Staging AWS")).toBeInTheDocument();
  });

  it("Connect new account button opens the wizard dialog", async () => {
    mockedListAWSConnections.mockResolvedValue({ connections: [] });
    renderPage();
    // Wait for the page to settle on the empty state so the dialog
    // trigger button has rendered.
    await waitFor(() => {
      expect(
        screen.getByText(/No accounts connected yet/i),
      ).toBeInTheDocument();
    });
    fireEvent.click(
      screen.getByRole("button", { name: /Connect new account/i }),
    );
    // The wizard's first step title from awsWizard.ts — verifies the
    // dialog mounted the ConnectorWizard component, not just an empty
    // shell.
    await waitFor(() => {
      expect(
        screen.getByText(/Enter your AWS account ID/i),
      ).toBeInTheDocument();
    });
  });

  it("Inventory tab Run scan triggers scanner and renders result", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    mockedRunAWSScan.mockResolvedValue(sampleScan);
    // userEvent simulates real pointer events — Radix Tabs + Select
    // both branch on pointerdown/pointerup, which fireEvent.click
    // doesn't dispatch in jsdom.
    const user = userEvent.setup();
    renderPage();

    // Wait for the Account tab's connection load to settle so the
    // Inventory tab's Select can read the cached SWR result without
    // re-fetching.
    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });

    // Switch to the Inventory tab.
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(screen.getByText(/Run an inventory scan/i)).toBeInTheDocument();
    });

    // Pick the first connection from the Select. Radix Select renders
    // a combobox button; opening it surfaces the option list inside a
    // portal.
    const select = screen.getByRole("combobox", {
      name: /Connected account/i,
    });
    await user.click(select);
    const option = await screen.findByText(/Prod AWS \(123456789012\)/);
    await user.click(option);

    // The Run scan button is enabled once a connection is selected.
    const runBtn = await screen.findByRole("button", { name: /Run scan/i });
    expect(runBtn).not.toBeDisabled();
    await user.click(runBtn);

    // Result panel renders with the compute + functions + databases
    // rows and OTel badges visible.
    await waitFor(() => {
      expect(screen.getByText(/Scan result for account/i)).toBeInTheDocument();
    });
    expect(screen.getByText("i-aaa")).toBeInTheDocument();
    expect(screen.getByText("hello")).toBeInTheDocument();
    // Slice 2 (v0.87) — the Databases section renders the RDS rows.
    expect(
      screen.getByText("arn:aws:rds:us-east-1:123:db:db-covered"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("arn:aws:rds:us-east-1:123:db:db-pi-only"),
    ).toBeInTheDocument();
    // OTel detection badges — the compute row has has_otel=true, the
    // function has has_otel_layer=false; both shapes must render.
    expect(screen.getByText(/OTel detected/i)).toBeInTheDocument();
    // "No OTel" appears at least once (the function row).
    expect(
      within(document.body).getAllByText(/No OTel/i).length,
    ).toBeGreaterThan(0);
    // RDS lever badges — Performance Insights renders on both rows
    // (covered + PI-only), Enhanced Monitoring renders on both with
    // different states. Confirm at least one of each label is present.
    expect(
      within(document.body).getAllByText(/Performance Insights/i).length,
    ).toBeGreaterThan(0);
    expect(
      within(document.body).getAllByText(/Enhanced Monitoring/i).length,
    ).toBeGreaterThan(0);

    // Scanner was called exactly once with the chosen account ID.
    expect(mockedRunAWSScan).toHaveBeenCalledTimes(1);
    expect(mockedRunAWSScan).toHaveBeenCalledWith("123456789012");
  });

  // --- Stream 2F: Generate recommendations flow --------------------

  it("Inventory: Generate recommendations button appears after scan", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    mockedRunAWSScan.mockResolvedValue(sampleScan);
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(screen.getByText(/Run an inventory scan/i)).toBeInTheDocument();
    });

    // Pre-scan: no Generate-recommendations button rendered.
    expect(
      screen.queryByRole("button", { name: /Generate recommendations/i }),
    ).not.toBeInTheDocument();

    // Trigger a scan.
    const select = screen.getByRole("combobox", {
      name: /Connected account/i,
    });
    await user.click(select);
    const option = await screen.findByText(/Prod AWS \(123456789012\)/);
    await user.click(option);
    const runBtn = await screen.findByRole("button", { name: /Run scan/i });
    await user.click(runBtn);

    // Post-scan: the Generate-recommendations button is rendered in the
    // scan result panel and is enabled.
    await waitFor(() => {
      expect(screen.getByText(/Scan result for account/i)).toBeInTheDocument();
    });
    const genBtn = await screen.findByRole("button", {
      name: /Generate recommendations/i,
    });
    expect(genBtn).toBeInTheDocument();
    expect(genBtn).not.toBeDisabled();
  });

  it("Inventory: clicking Generate recommendations calls the API and switches tabs", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    mockedRunAWSScan.mockResolvedValue(sampleScan);

    // Two-step plan with real-looking Terraform per step. The test
    // verifies the Terraform body renders in the Recommendations tab
    // after the auto-switch.
    const sampleRecs: Recommendation[] = [
      {
        id: "discovery-scan-uuid-0",
        category: "empty_signal",
        severity: "warn",
        title: "AI plan step 0: instrument 2 Lambdas",
        detail: "Two Lambdas plus one EC2 instance lack OTel.",
        est_savings_bytes: 0,
        generated_at: new Date().toISOString(),
        source: { kind: "discovery_scan", ref_id: "scan-uuid" },
        action: { kind: "plan", payload: {} },
        iac: {
          format: "terraform",
          source: 'resource "aws_lambda_function" "hello" {\n  layers = [...]\n}',
        },
      },
      {
        id: "discovery-scan-uuid-1",
        category: "empty_signal",
        severity: "warn",
        title: "AI plan step 1: instrument 1 EC2 instance",
        detail: "Stage EC2 after Lambda so you can observe between batches.",
        est_savings_bytes: 0,
        generated_at: new Date().toISOString(),
        source: { kind: "discovery_scan", ref_id: "scan-uuid" },
        action: { kind: "plan", payload: {} },
        iac: {
          format: "terraform",
          source: 'resource "aws_ssm_association" "adot" {\n  name = "..."\n}',
        },
      },
    ];
    const sampleResp: GenerateRecommendationsResponse = {
      declined: false,
      reasoning:
        "Two Lambdas plus one EC2 instance lack OTel. Stage Lambda first.",
      recommendations: sampleRecs,
    };
    mockedGenerateAWSRecommendations.mockResolvedValue(sampleResp);

    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(screen.getByText(/Run an inventory scan/i)).toBeInTheDocument();
    });

    // Scan first.
    const select = screen.getByRole("combobox", {
      name: /Connected account/i,
    });
    await user.click(select);
    const option = await screen.findByText(/Prod AWS \(123456789012\)/);
    await user.click(option);
    const runBtn = await screen.findByRole("button", { name: /Run scan/i });
    await user.click(runBtn);
    await waitFor(() => {
      expect(screen.getByText(/Scan result for account/i)).toBeInTheDocument();
    });

    // Generate.
    const genBtn = await screen.findByRole("button", {
      name: /Generate recommendations/i,
    });
    await user.click(genBtn);

    // API was called with the scan's account ID and the scan result.
    await waitFor(() => {
      expect(mockedGenerateAWSRecommendations).toHaveBeenCalledTimes(1);
    });
    expect(mockedGenerateAWSRecommendations).toHaveBeenCalledWith(
      "123456789012",
      sampleScan,
    );

    // Auto-switched to the Recommendations tab — the proposer reasoning
    // and both step titles render.
    await waitFor(() => {
      expect(
        screen.getByText(/Proposer reasoning/i),
      ).toBeInTheDocument();
    });
    expect(
      screen.getByText(/AI plan step 0: instrument 2 Lambdas/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/AI plan step 1: instrument 1 EC2 instance/i),
    ).toBeInTheDocument();
    // The Terraform bodies render verbatim — operator copies them into
    // their IaC pipeline.
    expect(
      within(document.body).getAllByText(/aws_lambda_function/i).length,
    ).toBeGreaterThan(0);
    expect(
      within(document.body).getAllByText(/aws_ssm_association/i).length,
    ).toBeGreaterThan(0);
  });
});
