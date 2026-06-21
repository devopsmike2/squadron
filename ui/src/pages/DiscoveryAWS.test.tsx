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
import { MemoryRouter } from "react-router-dom";
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
import {
  IaCGitHubOpenPRError,
  listIaCGitHubConnections,
  openIaCGitHubPullRequest,
  type IaCGitHubConnection,
} from "@/api/iacGithub";
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

// v0.89.3 #603 Stream 19 Phase 4: the Recommendations tab now reads
// IaC-connection state to decide whether to render Open PR, the
// configure-placement notice, or the connect-a-repo notice. The
// mock replaces the live fetcher with a vi.fn each test configures.
vi.mock("@/api/iacGithub", async () => {
  const actual = await vi.importActual<typeof import("@/api/iacGithub")>(
    "@/api/iacGithub",
  );
  return {
    ...actual,
    listIaCGitHubConnections: vi.fn(),
    openIaCGitHubPullRequest: vi.fn(),
  };
});

const mockedListAWSConnections = vi.mocked(listAWSConnections);
const mockedRunAWSScan = vi.mocked(runAWSScan);
const mockedGenerateAWSRecommendations = vi.mocked(generateAWSRecommendations);
const mockedListIaCConnections = vi.mocked(listIaCGitHubConnections);
const mockedOpenPullRequest = vi.mocked(openIaCGitHubPullRequest);

// renderPage wraps the page in a fresh SWRConfig so each test starts
// with an empty cache. Without this, the second test in the file would
// see the prior test's mocked connection list.
function renderPage() {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter>{children}</MemoryRouter>
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
  // Slice 3a (v0.88.0) — two buckets (one with logging, one without)
  // and one ALB pointing access logs at the logging-enabled bucket so
  // the test verifies the cross-reference rendering.
  object_stores: [
    {
      resource_id: "prod-logs",
      region: "us-east-1",
      server_access_logging_enabled: true,
      request_metrics_enabled: false,
      tags: {},
    },
    {
      resource_id: "user-uploads",
      region: "us-east-1",
      server_access_logging_enabled: false,
      request_metrics_enabled: false,
      tags: {},
    },
  ],
  load_balancers: [
    {
      resource_id: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/api-prod/aaaa",
      name: "api-prod",
      type: "application",
      scheme: "internet-facing",
      access_logs_enabled: true,
      access_logs_s3_bucket: "prod-logs",
      region: "us-east-1",
      tags: {},
    },
  ],
  // Slice 3b (v0.89.0) — two EKS clusters exercising the composite
  // instrumented rule: one with logging on (api + audit) AND an
  // ACTIVE adot addon → COVERED; one with logging off and a
  // non-observability addon (aws-ebs-csi-driver) → UNCOVERED. The
  // assertions check both axes render and that ADOT highlights.
  clusters: [
    {
      resource_id: "arn:aws:eks:us-east-1:123:cluster/prod-cluster",
      name: "prod-cluster",
      kubernetes_version: "1.29",
      status: "ACTIVE",
      control_plane_logging: ["api", "audit"],
      addons: [
        { name: "adot", version: "v0.92.0-eksbuild.1", status: "ACTIVE" },
      ],
      nodegroup_count: 2,
      fargate_profile_count: 0,
      region: "us-east-1",
      tags: {},
    },
    {
      resource_id: "arn:aws:eks:us-east-1:123:cluster/staging-cluster",
      name: "staging-cluster",
      kubernetes_version: "1.29",
      status: "ACTIVE",
      control_plane_logging: [],
      addons: [
        { name: "aws-ebs-csi-driver", version: "v1.0.0", status: "ACTIVE" },
      ],
      nodegroup_count: 1,
      fargate_profile_count: 0,
      region: "us-east-1",
      tags: {},
    },
  ],
  instrumented_count: 3,
  uninstrumented_count: 3,
  partial: false,
};

describe("DiscoveryAWSPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Default: no IaC connections. Each Phase 4 test overrides this
    // when it needs a connection + placement row to exist.
    mockedListIaCConnections.mockResolvedValue({ connections: [] });
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

    // Slice 3a (v0.88.0) — the Object stores section renders both
    // buckets; the Load balancers section renders the ALB.
    expect(screen.getByText("prod-logs")).toBeInTheDocument();
    expect(screen.getByText("user-uploads")).toBeInTheDocument();
    expect(screen.getByText("api-prod")).toBeInTheDocument();
    // S3 lever badge label appears (one row covered, one
    // uncovered). Request Metrics label appears for both rows.
    expect(
      within(document.body).getAllByText(/Server Access Logging/i).length,
    ).toBeGreaterThan(0);
    expect(
      within(document.body).getAllByText(/Request Metrics/i).length,
    ).toBeGreaterThan(0);
    // ALB lever badge label appears.
    expect(
      within(document.body).getAllByText(/Access Logs/i).length,
    ).toBeGreaterThan(0);
    // ALB→S3 cross-reference: the configured target bucket renders
    // under the Access Logs badge for the covered row.
    expect(screen.getByText(/→ prod-logs/i)).toBeInTheDocument();

    // Slice 3b (v0.89.0) — the Clusters section renders both
    // clusters and surfaces the composite-rule axes as
    // independent badge groups.
    expect(screen.getByText("prod-cluster")).toBeInTheDocument();
    expect(screen.getByText("staging-cluster")).toBeInTheDocument();
    // Control plane logging badge for the covered cluster
    // renders "api" + "audit" (both required for axis 1).
    expect(
      within(document.body).getAllByText(/^api$/).length,
    ).toBeGreaterThan(0);
    expect(
      within(document.body).getAllByText(/^audit$/).length,
    ).toBeGreaterThan(0);
    // ADOT add-on label appears as the observability addon
    // highlight (axis 2 — covered for prod-cluster).
    expect(screen.getByText("adot")).toBeInTheDocument();
    // The non-observability add-on still renders (informationally)
    // — confirms the section walks all addons, not just the
    // observability ones.
    expect(screen.getByText("aws-ebs-csi-driver")).toBeInTheDocument();
    // Uncovered cluster (staging-cluster) shows "none" badge for
    // its empty control_plane_logging axis.
    expect(
      within(document.body).getAllByText(/^none$/i).length,
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

  // ---------------------------------------------------------------
  // v0.89.3 #603 Stream 19 Phase 4 — Recommendations-tab Open-PR
  // button. These tests cover the three card states (Open-PR
  // available, connection-but-no-placement, no-connections-at-all)
  // plus the success/error paths.
  //
  // The helper drives the page through scan → generate so each
  // test starts from a known Recommendations-tab state. Tests
  // configure the IaC-connection list before clicking Generate so
  // the card derives the right state on render.
  // ---------------------------------------------------------------

  const lambdaRec: Recommendation = {
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
    resource_kind: "lambda-otel-layer",
  };

  function makeRecsResp(
    recs: Recommendation[] = [lambdaRec],
  ): GenerateRecommendationsResponse {
    return {
      declined: false,
      reasoning: "Lambdas first, EC2 second.",
      recommendations: recs,
    };
  }

  async function driveToRecsTab(
    user: ReturnType<typeof userEvent.setup>,
    recsResp: GenerateRecommendationsResponse = makeRecsResp(),
  ) {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    mockedRunAWSScan.mockResolvedValue(sampleScan);
    mockedGenerateAWSRecommendations.mockResolvedValue(recsResp);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
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
    const genBtn = await screen.findByRole("button", {
      name: /Generate recommendations/i,
    });
    await user.click(genBtn);
    await waitFor(() => {
      expect(screen.getByText(/Proposer reasoning/i)).toBeInTheDocument();
    });
  }

  it("RecommendationCard_renders_Copy_only_when_no_iac_connections", async () => {
    mockedListIaCConnections.mockResolvedValue({ connections: [] });
    const user = userEvent.setup();
    await driveToRecsTab(user);

    // Copy button present, Open PR not present, the State-C notice
    // points the operator at the connect flow.
    expect(
      screen.getByRole("button", { name: /Copy Terraform snippet/i }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /^Open PR for/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/Want one-click PRs\?/i),
    ).toBeInTheDocument();
    const link = screen.getByRole("link", {
      name: /Connect a Terraform repo/i,
    });
    expect(link).toHaveAttribute("href", "/discovery/iac/github");
  });

  it("RecommendationCard_renders_OpenPR_button_when_connection_and_placement_exist", async () => {
    const conn: IaCGitHubConnection = {
      connection_id: "conn-1",
      provider: "github",
      auth_kind: "pat",
      repo_full_name: "octo/infra",
      default_branch: "main",
      repo_layout: "multi",
      placement_map: [
        {
          provider: "aws",
          resource_kind: "lambda-otel-layer",
          file_path: "modules/lambda/main.tf",
        },
      ],
      created_at: new Date().toISOString(),
    };
    mockedListIaCConnections.mockResolvedValue({ connections: [conn] });
    const user = userEvent.setup();
    await driveToRecsTab(user);

    // Open PR present, copy present, no State-C notice.
    expect(
      screen.getByRole("button", { name: /^Open PR for/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Copy Terraform snippet/i }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/Want one-click PRs\?/i),
    ).not.toBeInTheDocument();
  });

  it("RecommendationCard_renders_configure_placement_link_when_connection_but_no_placement", async () => {
    const conn: IaCGitHubConnection = {
      connection_id: "conn-1",
      provider: "github",
      auth_kind: "pat",
      repo_full_name: "octo/infra",
      default_branch: "main",
      repo_layout: "multi",
      // No row for lambda-otel-layer — connection is for a different
      // resource_kind entirely. Card must render the State-B notice.
      placement_map: [
        {
          provider: "aws",
          resource_kind: "eks-cluster-logging",
          file_path: "modules/eks/main.tf",
        },
      ],
      created_at: new Date().toISOString(),
    };
    mockedListIaCConnections.mockResolvedValue({ connections: [conn] });
    const user = userEvent.setup();
    await driveToRecsTab(user);

    // Copy only; State-B notice names the missing kind and links to
    // the connections page.
    expect(
      screen.getByRole("button", { name: /Copy Terraform snippet/i }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /^Open PR for/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/Open PR needs a Terraform file path for/i),
    ).toBeInTheDocument();
    // The resource_kind name is in the notice (rendered inside a
    // <code>); querying the surrounding paragraph is sufficient.
    expect(
      screen.getByText(/lambda-otel-layer/),
    ).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /Configure placement/i });
    expect(link).toHaveAttribute("href", "/discovery/iac/github");
  });

  it("RecommendationCard_OpenPR_success_renders_PR_link_and_disables_button", async () => {
    const conn: IaCGitHubConnection = {
      connection_id: "conn-1",
      provider: "github",
      auth_kind: "pat",
      repo_full_name: "octo/infra",
      default_branch: "main",
      repo_layout: "multi",
      placement_map: [
        {
          provider: "aws",
          resource_kind: "lambda-otel-layer",
          file_path: "modules/lambda/main.tf",
        },
      ],
      created_at: new Date().toISOString(),
    };
    mockedListIaCConnections.mockResolvedValue({ connections: [conn] });
    mockedOpenPullRequest.mockResolvedValue({
      pr_number: 42,
      pr_url: "https://github.com/octo/infra/pull/42",
      branch: "squadron/rec-scan-uu-0",
      commit_sha: "abc1234",
      file_path: "modules/lambda/main.tf",
      repo_full_name: "octo/infra",
    });
    const user = userEvent.setup();
    await driveToRecsTab(user);

    const openBtn = screen.getByRole("button", { name: /^Open PR for/i });
    await user.click(openBtn);

    // API was called with the full snippet, the resource_kind, the
    // scan_id, and the account_id from the inventory flow.
    await waitFor(() => {
      expect(mockedOpenPullRequest).toHaveBeenCalledTimes(1);
    });
    const [callConnID, callBody] = mockedOpenPullRequest.mock.calls[0];
    expect(callConnID).toBe("conn-1");
    expect(callBody.scan_id).toBe("scan-uuid");
    expect(callBody.step_idx).toBe(0);
    expect(callBody.resource_kind).toBe("lambda-otel-layer");
    expect(callBody.account_id).toBe("123456789012");
    expect(callBody.snippet).toContain("aws_lambda_function");

    // Success card rendered; Open PR button is gone (one PR per
    // click); PR link is target=_blank to the GitHub URL.
    await waitFor(() => {
      expect(screen.getByText(/PR #42 opened in/i)).toBeInTheDocument();
    });
    expect(
      screen.queryByRole("button", { name: /^Open PR for/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/Squadron will not push to this branch again/i),
    ).toBeInTheDocument();
    const viewLink = screen.getByRole("link", { name: /View PR/i });
    expect(viewLink).toHaveAttribute(
      "href",
      "https://github.com/octo/infra/pull/42",
    );
    expect(viewLink).toHaveAttribute("target", "_blank");
  });

  it("RecommendationCard_OpenPR_NoPlacementMapping_error_renders_configure_link", async () => {
    // Wizard state drifted between the page-mount fetch and the
    // Open-PR click — the backend returns NoPlacementMapping. Card
    // renders the humanized message + a configure link.
    const conn: IaCGitHubConnection = {
      connection_id: "conn-1",
      provider: "github",
      auth_kind: "pat",
      repo_full_name: "octo/infra",
      default_branch: "main",
      repo_layout: "multi",
      placement_map: [
        {
          provider: "aws",
          resource_kind: "lambda-otel-layer",
          file_path: "modules/lambda/main.tf",
        },
      ],
      created_at: new Date().toISOString(),
    };
    mockedListIaCConnections.mockResolvedValue({ connections: [conn] });
    mockedOpenPullRequest.mockRejectedValue(
      new IaCGitHubOpenPRError(
        422,
        {
          code: "NoPlacementMapping",
          message:
            'No placement-map row exists for resource_kind "lambda-otel-layer".',
          suggested_step: "placement-map",
        },
        "fallback",
      ),
    );
    const user = userEvent.setup();
    await driveToRecsTab(user);

    await user.click(screen.getByRole("button", { name: /^Open PR for/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/No placement-map row exists/i),
      ).toBeInTheDocument();
    });
    const link = screen.getByRole("link", {
      name: /Configure the missing placement row/i,
    });
    expect(link).toHaveAttribute("href", "/discovery/iac/github");
    // Open PR button is still rendered so the operator can retry
    // once the placement is configured.
    expect(
      screen.getByRole("button", { name: /^Open PR for/i }),
    ).toBeInTheDocument();
  });

  it("RecommendationCard_OpenPR_RepoNotFound_error_renders_reconnect_link", async () => {
    const conn: IaCGitHubConnection = {
      connection_id: "conn-1",
      provider: "github",
      auth_kind: "pat",
      repo_full_name: "octo/infra",
      default_branch: "main",
      repo_layout: "multi",
      placement_map: [
        {
          provider: "aws",
          resource_kind: "lambda-otel-layer",
          file_path: "modules/lambda/main.tf",
        },
      ],
      created_at: new Date().toISOString(),
    };
    mockedListIaCConnections.mockResolvedValue({ connections: [conn] });
    mockedOpenPullRequest.mockRejectedValue(
      new IaCGitHubOpenPRError(
        404,
        {
          code: "RepoNotFound",
          message: 'The repo "octo/infra" is no longer reachable.',
          suggested_step: "save",
        },
        "fallback",
      ),
    );
    const user = userEvent.setup();
    await driveToRecsTab(user);

    await user.click(screen.getByRole("button", { name: /^Open PR for/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/The repo "octo\/infra" is no longer reachable/i),
      ).toBeInTheDocument();
    });
    const link = screen.getByRole("link", {
      name: /Re-run the IaC connect wizard/i,
    });
    expect(link).toHaveAttribute("href", "/discovery/iac/github");
  });

  it("RecommendationCard_OpenPR_AuthFailed_error_renders_reconnect_link", async () => {
    const conn: IaCGitHubConnection = {
      connection_id: "conn-1",
      provider: "github",
      auth_kind: "pat",
      repo_full_name: "octo/infra",
      default_branch: "main",
      repo_layout: "multi",
      placement_map: [
        {
          provider: "aws",
          resource_kind: "lambda-otel-layer",
          file_path: "modules/lambda/main.tf",
        },
      ],
      created_at: new Date().toISOString(),
    };
    mockedListIaCConnections.mockResolvedValue({ connections: [conn] });
    mockedOpenPullRequest.mockRejectedValue(
      new IaCGitHubOpenPRError(
        401,
        {
          code: "AuthFailed",
          message:
            "GitHub rejected the stored token. Re-run the IaC connect wizard.",
          suggested_step: "save",
        },
        "fallback",
      ),
    );
    const user = userEvent.setup();
    await driveToRecsTab(user);

    await user.click(screen.getByRole("button", { name: /^Open PR for/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/GitHub rejected the stored token/i),
      ).toBeInTheDocument();
    });
    const link = screen.getByRole("link", {
      name: /Re-run the IaC connect wizard/i,
    });
    expect(link).toHaveAttribute("href", "/discovery/iac/github");
  });
});
