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
  listExcludedRecommendations,
  runAWSScan,
  setRecommendationExclusion,
  type CloudConnection,
  type ExcludedRecommendation,
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
    // v0.89.7b (#619 Stream 23) — scanAllAWS is mocked via vi.spyOn
    // inside the scan-all tests; declaring it as vi.fn() here lets
    // vi.mocked(...) typing work without breaking the tests that
    // don't touch the scan-all path (they get a stub that throws if
    // accidentally called).
    scanAllAWS: vi.fn(),
    // v0.89.38 (#658 Stream 56, #531 slice 2 chunk 5) — exclude
    // affordance POSTs through this helper. Default mock resolves
    // with the request body echoed back; per-test overrides via
    // mockResolvedValueOnce / mockRejectedValueOnce drive the
    // happy + rollback paths.
    setRecommendationExclusion: vi.fn(),
    // v0.89.40 (#660 Stream 58, #531 slice 2 chunk 5 follow-on) —
    // hydration GET fires from RecommendationsTab on mount. Default
    // mock resolves with an empty array so the existing chunk 5
    // tests (which don't seed any exclusions) keep their pre-
    // hydration UI shape; tests that exercise the hydration path
    // override via mockResolvedValueOnce / mockRejectedValueOnce.
    listExcludedRecommendations: vi.fn().mockResolvedValue([]),
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
const mockedSetRecommendationExclusion = vi.mocked(setRecommendationExclusion);
const mockedListExcludedRecommendations = vi.mocked(listExcludedRecommendations);

// renderPage wraps the page in a fresh SWRConfig so each test starts
// with an empty cache. Without this, the second test in the file would
// see the prior test's mocked connection list.
//
// v0.89.7b (#619 Stream 23) — initialEntries threads through to
// MemoryRouter so a test can land in aggregate (?account=all, default)
// or single-account (?account=<id>) view. Pre-Stream-23 tests that
// drive the Inventory tab default to a single-account entry because
// aggregate mode renders the summary card instead of the per-account
// dropdown those tests assert against.
function renderPage(initialEntries: string[] = ["/discovery/aws?account=all"]) {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter initialEntries={initialEntries}>{children}</MemoryRouter>
      </SWRConfig>
    );
  }
  return render(<DiscoveryAWSPage />, { wrapper: Wrapper });
}

// renderPageSingleAccount is the existing-test entry point. Pre-Stream
// 23 the Inventory tab always rendered with a dropdown; in Stream 23
// that's only the case in single-account view. This helper drops the
// existing tests on ?account=<id> so the per-account dropdown is
// visible exactly as before.
const SINGLE_ACCOUNT_PATH = "/discovery/aws?account=123456789012";
function renderPageInSingleAccountView() {
  return renderPage([SINGLE_ACCOUNT_PATH]);
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
    // v0.89.40 (#660 Stream 58, #531 slice 2 chunk 5 follow-on):
    // RecommendationsTab hydrates its excludedSet from this GET on
    // mount. Default to an empty list so the existing chunk-5
    // toggle tests keep their pre-hydration UI shape; the new
    // hydration tests below override per-test.
    mockedListExcludedRecommendations.mockResolvedValue([]);
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

  // --- #622 connections-list resume entry point ----------------
  //
  // The "I've connected this account before — Resume an existing
  // ExternalId" entry point lives on the connections list page so
  // an operator recovering from a previous Squadron deployment
  // (Docker→local swap, reinstall) can paste their old UUID before
  // the wizard generates a fresh one. Fixes the third failure from
  // the #621 walkthrough.

  it("DiscoveryAWS_RendersResumeEntryPointOnConnectionsList (empty state)", async () => {
    mockedListAWSConnections.mockResolvedValue({ connections: [] });
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByText(/No accounts connected yet/i),
      ).toBeInTheDocument();
    });
    // Empty-state copy variant — the affordance leads with the
    // operator's own context ("Already have an ExternalId?").
    expect(
      screen.getByRole("button", {
        name: /Already have an ExternalId\? Resume an existing connection/i,
      }),
    ).toBeInTheDocument();
  });

  it("DiscoveryAWS_RendersResumeEntryPointOnConnectionsList (populated)", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    // Populated-state copy variant — terser since the operator has
    // already seen the empty-state context.
    expect(
      screen.getByRole("button", { name: /^Resume an existing connection$/i }),
    ).toBeInTheDocument();
  });

  it("DiscoveryAWS_ResumeEntryPointOpensWizardWithExistingExternalIdField", async () => {
    mockedListAWSConnections.mockResolvedValue({ connections: [] });
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByText(/No accounts connected yet/i),
      ).toBeInTheDocument();
    });
    // Click the resume entry point — opens the wizard in resumeMode
    // so step 1 renders the "Existing ExternalId (optional)" field
    // above the account-id input.
    fireEvent.click(
      screen.getByRole("button", {
        name: /Already have an ExternalId\? Resume an existing connection/i,
      }),
    );
    await waitFor(() => {
      expect(
        screen.getByLabelText(/Existing ExternalId/i),
      ).toBeInTheDocument();
    });
    // The standard account-id input is still there — the resume
    // field is additive, not replacement.
    expect(
      screen.getByPlaceholderText("123456789012"),
    ).toBeInTheDocument();
  });

  it("Connect new account button (without resume) does NOT show the Existing ExternalId field", async () => {
    mockedListAWSConnections.mockResolvedValue({ connections: [] });
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByText(/No accounts connected yet/i),
      ).toBeInTheDocument();
    });
    fireEvent.click(
      screen.getByRole("button", { name: /^Connect new account$/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Enter your AWS account ID/i),
      ).toBeInTheDocument();
    });
    expect(
      screen.queryByLabelText(/Existing ExternalId/i),
    ).not.toBeInTheDocument();
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
    renderPageInSingleAccountView();

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
    const option = await screen.findByRole("option", {
      name: /Prod AWS \(123456789012\)/,
    });
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
    renderPageInSingleAccountView();

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
    const option = await screen.findByRole("option", {
      name: /Prod AWS \(123456789012\)/,
    });
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
    renderPageInSingleAccountView();

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
    const option = await screen.findByRole("option", {
      name: /Prod AWS \(123456789012\)/,
    });
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
    // v0.89.4 (#611) — the discovery proposer emits per-step
    // affected_resources; the Open-PR call forwards this verbatim
    // so the backend's PR title's "for <N> resources" count and
    // the PR body's "Affected resources" bullets are accurate.
    affected_resources: [
      "arn:aws:lambda:us-east-1:123:function:hello",
      "arn:aws:lambda:us-east-1:123:function:goodbye",
    ],
    // v0.89.11 (#626 Stream 27) — slice-1.5 disposition. lambda is
    // a patch_existing kind; the Recommendations card renders a
    // "Needs manual merge" badge next to Open PR for these.
    disposition: "patch_existing",
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
    renderPageInSingleAccountView();
    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    const select = screen.getByRole("combobox", {
      name: /Connected account/i,
    });
    await user.click(select);
    const option = await screen.findByRole("option", {
      name: /Prod AWS \(123456789012\)/,
    });
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
    // v0.89.4 (#610) — State-B link target deep-links the wizard
    // via ?connection_id=...&step=placement&kind=<resource_kind>
    // so the operator lands on the right placement row in one
    // click. Bare /discovery/iac/github is the Phase-4 stopgap and
    // is no longer the expected href.
    const link = screen.getByRole("link", { name: /Configure placement/i });
    expect(link).toHaveAttribute(
      "href",
      "/discovery/iac/github?connection_id=conn-1&step=placement&kind=lambda-otel-layer",
    );
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
    // v0.89.4 (#611) — affected_resources from the recommendation
    // rides through to the Open-PR call payload verbatim. The
    // backend's PR title's "for <N> resources" count and the body's
    // bullets read off this array. A regression that hard-coded
    // [] (the Phase 4 stopgap) would silently revert the title to
    // "for 0 resources" in production.
    expect(callBody.affected_resources).toEqual([
      "arn:aws:lambda:us-east-1:123:function:hello",
      "arn:aws:lambda:us-east-1:123:function:goodbye",
    ]);

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
    // v0.89.4 (#610) — NoPlacementMapping recovery deep-links the
    // wizard via the same shape as State B so the operator lands
    // on the missing row in one click instead of scanning the
    // connections list. Bare /discovery/iac/github was the
    // Phase-4 stopgap.
    expect(link).toHaveAttribute(
      "href",
      "/discovery/iac/github?connection_id=conn-1&step=placement&kind=lambda-otel-layer",
    );
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

  // ---------------------------------------------------------------
  // v0.89.11 (#626 Stream 27) — slice-1.5 hybrid PR disposition.
  // For patch_existing kinds (lambda-otel-layer is canonical), the
  // recommendation card renders a "Needs manual merge" badge next
  // to the Open PR button so the operator sees the slice-1
  // append-only friction BEFORE clicking. For new_file kinds the
  // badge is absent — the implicit clean experience.
  // ---------------------------------------------------------------

  it("RecommendationCard_renders_NeedsManualMerge_badge_on_patch_existing", async () => {
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

    // Badge is rendered next to the Open PR button. The Open PR
    // button still renders — the badge is informational, not
    // disabling.
    expect(
      screen.getByRole("button", { name: /^Open PR for/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Needs manual merge/i),
    ).toBeInTheDocument();
  });

  // v0.89.12 (#628 Stream 29) — slice 2 — when the recommendation
  // carries an `hcl_patch` field, the backend's HCL merger will
  // produce a clean drop-in PR. The amber "Needs manual merge"
  // badge is replaced by a green "HCL-merged" checkmark so the
  // operator sees the clean experience BEFORE clicking Open PR.
  it("RecommendationCard_renders_HCLMerged_checkmark_on_patch_existing_with_hcl_patch", async () => {
    const lambdaRecWithPatch: Recommendation = {
      ...lambdaRec,
      // Opaque shape from the UI's perspective — the backend
      // schema-validates and applies.
      hcl_patch: {
        kind: "lambda-otel-layer",
        disposition: "patch_existing",
        target_resource_address: "aws_lambda_function.hello",
        patches: [
          {
            attribute_path: ["layers"],
            op: "list_append_dedupe",
            value: ["arn:aws:lambda:us-east-1:999:layer:otel:1"],
          },
        ],
      },
    };
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
    await driveToRecsTab(user, makeRecsResp([lambdaRecWithPatch]));

    // Open PR button still rendered; the slice-2 badge is the
    // green checkmark, NOT the amber "Needs manual merge".
    expect(
      screen.getByRole("button", { name: /^Open PR for/i }),
    ).toBeInTheDocument();
    expect(screen.getByText(/HCL-merged/i)).toBeInTheDocument();
    expect(
      screen.queryByText(/Needs manual merge/i),
    ).not.toBeInTheDocument();
  });

  // v0.89.12 — fallback case: patch_existing kind with NO
  // hcl_patch (slice-1.5-era recommendation, or a proposer prompt
  // that didn't emit one). The amber "Needs manual merge" badge
  // is preserved — backend will fall back to slice-1.5 append.
  it("RecommendationCard_renders_NeedsManualMerge_badge_when_patch_existing_has_no_hcl_patch", async () => {
    // lambdaRec already has disposition=patch_existing and NO
    // hcl_patch. Reuse it directly.
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

    expect(
      screen.getByRole("button", { name: /^Open PR for/i }),
    ).toBeInTheDocument();
    expect(screen.getByText(/Needs manual merge/i)).toBeInTheDocument();
    expect(screen.queryByText(/HCL-merged/i)).not.toBeInTheDocument();
  });

  it("RecommendationCard_does_NOT_render_NeedsManualMerge_badge_on_new_file", async () => {
    // s3-access-logging is a new_file disposition. Card renders
    // Open PR cleanly without the manual-merge badge.
    const s3Rec: Recommendation = {
      ...lambdaRec,
      id: "discovery-scan-uuid-1",
      title: "AI plan step 1: enable S3 access logging",
      resource_kind: "s3-access-logging",
      disposition: "new_file",
      iac: {
        format: "terraform",
        source:
          'resource "aws_s3_bucket_logging" "example" {\n  bucket = aws_s3_bucket.example.id\n}',
      },
    };
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
          resource_kind: "s3-access-logging",
          file_path: "modules/s3/main.tf",
        },
      ],
      created_at: new Date().toISOString(),
    };
    mockedListIaCConnections.mockResolvedValue({ connections: [conn] });
    const user = userEvent.setup();
    await driveToRecsTab(user, makeRecsResp([s3Rec]));

    // Open PR present, badge absent.
    expect(
      screen.getByRole("button", { name: /^Open PR for/i }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/Needs manual merge/i),
    ).not.toBeInTheDocument();
  });

  // ---------------------------------------------------------------
  // v0.89.7b #619 Stream 23 — multi-account scan UI surfaces.
  // These tests cover the account selector dropdown's URL state,
  // the aggregate vs single-account view branching, and the scan-
  // all in-flight grid + success/failed split rendering.
  // ---------------------------------------------------------------

  it("DiscoveryAWS_DefaultsToAllAccountsWhenNoQueryParam", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    const user = userEvent.setup();
    // No ?account= param — page should default to aggregate view.
    renderPage(["/discovery/aws"]);
    await waitFor(() => {
      expect(
        screen.getByRole("combobox", { name: /Account selector/i }),
      ).toBeInTheDocument();
    });
    // Aggregate view shows the Scan-all CTA at the top of the page.
    expect(
      screen.getByRole("button", { name: /Scan all accounts/i }),
    ).toBeInTheDocument();
    // Inventory tab in aggregate view renders the summary CTA, not
    // the per-account dropdown. Radix Tabs branch on pointer events
    // so userEvent.click is required (fireEvent.click won't flip the
    // tab state in jsdom). The JSX uses &quot; for the quoted string
    // which can render as discrete text nodes; use a substring match
    // that skips the inner quotes.
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/to populate the aggregate summary/i),
      ).toBeInTheDocument();
    });
    // Per-account Run-scan UI is NOT rendered in aggregate Inventory.
    expect(
      screen.queryByText(/Run an inventory scan/i),
    ).not.toBeInTheDocument();
  });

  it("DiscoveryAWS_RespectsAccountQueryParam", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    const user = userEvent.setup();
    // ?account=<id> drops the operator on the single-account view.
    renderPage(["/discovery/aws?account=123456789012"]);
    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    // Scan-all CTA is hidden in single-account view; the page-level
    // selector still renders.
    expect(
      screen.queryByRole("button", { name: /Scan all accounts/i }),
    ).not.toBeInTheDocument();
    // Inventory tab renders the per-account dropdown.
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(screen.getByText(/Run an inventory scan/i)).toBeInTheDocument();
    });
  });

  it("DiscoveryAWS_AccountSelectorChangeUpdatesURL", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    const user = userEvent.setup();
    renderPage(["/discovery/aws"]);
    await waitFor(() => {
      expect(
        screen.getByRole("combobox", { name: /Account selector/i }),
      ).toBeInTheDocument();
    });
    // Pick a single account — Scan-all CTA should disappear and the
    // single-account Inventory tab should be available. The top-level
    // selector renders display_name + last-4 to differentiate from
    // the InventoryTab's per-account picker label shape.
    await user.click(
      screen.getByRole("combobox", { name: /Account selector/i }),
    );
    const option = await screen.findByRole("option", {
      name: /Prod AWS — …9012/,
    });
    await user.click(option);
    await waitFor(() => {
      expect(
        screen.queryByRole("button", { name: /Scan all accounts/i }),
      ).not.toBeInTheDocument();
    });
    // The page is now in single-account view. Inventory tab renders
    // its per-account dropdown.
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(screen.getByText(/Run an inventory scan/i)).toBeInTheDocument();
    });
  });

  it("DiscoveryAWS_AggregateView_ShowsScanAllCTA", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    renderPage(["/discovery/aws?account=all"]);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Scan all accounts/i }),
      ).toBeInTheDocument();
    });
  });

  it("DiscoveryAWS_SingleAccountView_HidesScanAllCTA_ShowsPerAccountScanCTA", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    const user = userEvent.setup();
    renderPage(["/discovery/aws?account=123456789012"]);
    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    // Top-of-page Scan-all CTA is absent in single-account view.
    expect(
      screen.queryByRole("button", { name: /Scan all accounts/i }),
    ).not.toBeInTheDocument();
    // Per-account Run-scan CTA is present in the Inventory tab. The
    // button label matches the existing /Run scan/i pattern (the
    // button text reads "Run scan" when not in-flight).
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
  });

  it("DiscoveryAWS_ScanAllSuccess_RendersSucceededFailedSplit", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    // Configure a mixed-result scan-all so the success/failed split
    // is visible on the rendered grid.
    const scanAllResp = {
      scan_all_id: "scan-all-uuid",
      total_accounts: 2,
      succeeded_accounts: [
        {
          account_id: "123456789012",
          scan_id: "scan-1",
          resource_count: 10,
          instrumented_count: 6,
          uninstrumented_count: 4,
        },
      ],
      failed_accounts: [
        {
          account_id: "987654321098",
          error_code: "AccessDenied",
          humanized_message:
            "Squadron's IAM role lacks ec2:DescribeInstances. Update the trust policy.",
        },
      ],
      total_resources: 10,
      total_instrumented: 6,
      total_uninstrumented: 4,
      partial: true,
      concurrency: 3,
    };
    // Mock the scan-all API on the discovery module.
    const discoveryModule = await import("@/api/discovery");
    const scanAllSpy = vi
      .spyOn(discoveryModule, "scanAllAWS")
      .mockResolvedValue(scanAllResp);

    const user = userEvent.setup();
    renderPage(["/discovery/aws?account=all"]);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Scan all accounts/i }),
      ).toBeInTheDocument();
    });
    await user.click(
      screen.getByRole("button", { name: /Scan all accounts/i }),
    );

    // Succeeded card: shows resource counts. Failed card: shows the
    // humanized error message + the error_code.
    await waitFor(() => {
      expect(screen.getByText(/Scan succeeded/i)).toBeInTheDocument();
    });
    expect(
      screen.getByText(
        /10 resources · 6 instrumented · 4 uninstrumented/,
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("AccessDenied")).toBeInTheDocument();
    expect(
      screen.getByText(/Squadron's IAM role lacks ec2:DescribeInstances/i),
    ).toBeInTheDocument();

    expect(scanAllSpy).toHaveBeenCalledTimes(1);
    scanAllSpy.mockRestore();
  });

  it("DiscoveryAWS_ScanAllPartialFailure_RendersPartialBanner", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    const scanAllResp = {
      scan_all_id: "scan-all-uuid",
      total_accounts: 2,
      succeeded_accounts: [
        {
          account_id: "123456789012",
          scan_id: "scan-1",
          resource_count: 10,
          instrumented_count: 6,
          uninstrumented_count: 4,
        },
      ],
      failed_accounts: [
        {
          account_id: "987654321098",
          error_code: "AccessDenied",
          humanized_message: "lacks permissions",
        },
      ],
      total_resources: 10,
      total_instrumented: 6,
      total_uninstrumented: 4,
      partial: true,
      concurrency: 3,
    };
    const discoveryModule = await import("@/api/discovery");
    const scanAllSpy = vi
      .spyOn(discoveryModule, "scanAllAWS")
      .mockResolvedValue(scanAllResp);

    const user = userEvent.setup();
    renderPage(["/discovery/aws?account=all"]);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Scan all accounts/i }),
      ).toBeInTheDocument();
    });
    await user.click(
      screen.getByRole("button", { name: /Scan all accounts/i }),
    );

    // Partial banner renders above the per-account grid, naming the
    // failure count.
    await waitFor(() => {
      expect(screen.getByText(/Partial scan-all/i)).toBeInTheDocument();
    });
    expect(
      screen.getByText(/1 of 2 accounts failed/i),
    ).toBeInTheDocument();
    scanAllSpy.mockRestore();
  });

  it("DiscoveryAWS_RecommendationCard_ShowsAccountBadgeOnlyInAggregateView", async () => {
    // Component-level render: prove the badge gates on inAggregateView
    // without driving the full page flow. The page's aggregate
    // Recommendations tab punts on cards (renders a notice instead),
    // so the badge code path is tested via direct component render.
    const { DiscoveryRecommendationCard } = await import("./DiscoveryAWS");
    const rec: Recommendation = {
      id: "rec-1",
      category: "empty_signal",
      severity: "warn",
      title: "Instrument 1 Lambda",
      detail: "lambda lacks otel",
      est_savings_bytes: 0,
      generated_at: new Date().toISOString(),
      source: { kind: "discovery_scan", ref_id: "scan-x" },
      action: { kind: "plan", payload: {} },
      iac: { format: "terraform", source: "resource ..." },
      resource_kind: "lambda-otel-layer",
    };

    // Aggregate view: badge present, showing the trailing four
    // digits of the account id.
    const { unmount } = render(
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter>
          <DiscoveryRecommendationCard
            rec={rec}
            stepIdx={0}
            scanID="scan-x"
            accountID="123456789012"
            proposerReasoning=""
            iacConnections={[]}
            inAggregateView={true}
            onPickAccount={() => {}}
          />
        </MemoryRouter>
      </SWRConfig>,
    );
    expect(screen.getByText(/from 9012/)).toBeInTheDocument();
    unmount();

    // Single-account view: badge suppressed.
    render(
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter>
          <DiscoveryRecommendationCard
            rec={rec}
            stepIdx={0}
            scanID="scan-x"
            accountID="123456789012"
            proposerReasoning=""
            iacConnections={[]}
            inAggregateView={false}
          />
        </MemoryRouter>
      </SWRConfig>,
    );
    expect(screen.queryByText(/from 9012/)).not.toBeInTheDocument();
  });

  it("DiscoveryAWS_BadgeClickFiltersToSingleAccount", async () => {
    const { DiscoveryRecommendationCard } = await import("./DiscoveryAWS");
    const rec: Recommendation = {
      id: "rec-1",
      category: "empty_signal",
      severity: "warn",
      title: "Instrument 1 Lambda",
      detail: "lambda lacks otel",
      est_savings_bytes: 0,
      generated_at: new Date().toISOString(),
      source: { kind: "discovery_scan", ref_id: "scan-x" },
      action: { kind: "plan", payload: {} },
      iac: { format: "terraform", source: "resource ..." },
      resource_kind: "lambda-otel-layer",
    };
    const onPick = vi.fn();
    const user = userEvent.setup();
    render(
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter>
          <DiscoveryRecommendationCard
            rec={rec}
            stepIdx={0}
            scanID="scan-x"
            accountID="123456789012"
            proposerReasoning=""
            iacConnections={[]}
            inAggregateView={true}
            onPickAccount={onPick}
          />
        </MemoryRouter>
      </SWRConfig>,
    );
    await user.click(
      screen.getByRole("button", { name: /Filter to account 123456789012/i }),
    );
    expect(onPick).toHaveBeenCalledWith("123456789012");
  });

  // ----- v0.89.38 #658 Stream 56 (#531 slice 2 chunk 5) ---------
  //
  // Exclude affordance on the discovery Recommendations tab. Three
  // tests cover the slice-2-chunk-5 surface:
  //   - happy click-to-exclude posts the right body + flips the UI.
  //   - restore (un-exclude) inverts the state + posts excluded=false.
  //   - error rollback: failed POST surfaces a toast and clears the
  //     optimistic Set.

  function buildRecsResponse(
    ...overrides: Partial<Recommendation>[]
  ): GenerateRecommendationsResponse {
    const base = (i: number): Recommendation => ({
      id: `rec-${i}`,
      category: "empty_signal",
      severity: "warn",
      title: `Instrument resource ${i}`,
      detail: "lambda lacks otel",
      est_savings_bytes: 0,
      generated_at: new Date().toISOString(),
      source: { kind: "discovery_scan", ref_id: "scan-x" },
      action: { kind: "plan", payload: {} },
      iac: { format: "terraform", source: "resource ..." },
      resource_kind: "lambda-otel-layer",
      affected_resources: [`arn:aws:lambda:us-east-1:123:function:fn-${i}`],
    });
    const items = overrides.length === 0
      ? [base(1), base(2)]
      : overrides.map((o, i) => ({ ...base(i + 1), ...o }));
    return {
      declined: false,
      reasoning: "",
      recommendations: items,
    };
  }

  async function renderRecommendationsTabWith(
    recs: GenerateRecommendationsResponse,
    region = "us-east-1",
  ) {
    const { RecommendationsTab } = await import("./DiscoveryAWS");
    return render(
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter>
          <RecommendationsTab
            recs={recs}
            accountID="123456789012"
            scanID="scan-x"
            region={region}
          />
        </MemoryRouter>
      </SWRConfig>,
    );
  }

  it("RecommendationsTab_ExcludeButton_ClickPostsExclusionAndUpdatesUI", async () => {
    const recs = buildRecsResponse();
    mockedSetRecommendationExclusion.mockResolvedValueOnce({
      recommendation_id: "rec-1",
      excluded: true,
      excluded_at: new Date().toISOString(),
      excluded_by: "operator",
    });
    await renderRecommendationsTabWith(recs);

    const buttons = await screen.findAllByRole("button", {
      name: /Don't propose .* again/i,
    });
    expect(buttons).toHaveLength(2);
    const user = userEvent.setup();
    await user.click(buttons[0]);

    await waitFor(() => {
      expect(mockedSetRecommendationExclusion).toHaveBeenCalledTimes(1);
    });
    expect(mockedSetRecommendationExclusion).toHaveBeenCalledWith({
      recommendation_id: "rec-1",
      connection_id: "123456789012",
      account_id: "123456789012",
      region: "us-east-1",
      recommendation_kind: "lambda-otel-layer",
      resource_id: "arn:aws:lambda:us-east-1:123:function:fn-1",
      excluded: true,
    });
    // Excluded badge appears in the first card; the second card is
    // unchanged.
    await waitFor(() => {
      expect(
        screen.getByLabelText(/Excluded from future recommendations/i),
      ).toBeInTheDocument();
    });
    // Success toast names the persistence behavior.
    expect(
      screen.getByTestId("exclusion-toast"),
    ).toHaveTextContent(/Excluded — Squadron won't propose this/i);
    // The first card switches its button label to Restore; the
    // second still reads "Don't propose this again".
    expect(
      screen.getByRole("button", { name: /Restore .* as a recommendation/i }),
    ).toBeInTheDocument();
    expect(
      screen.getAllByRole("button", { name: /Don't propose .* again/i }),
    ).toHaveLength(1);
  });

  it("RecommendationsTab_RestoreButton_ClickPostsRestoreAndClearsExclusion", async () => {
    const recs = buildRecsResponse();
    mockedSetRecommendationExclusion
      .mockResolvedValueOnce({
        recommendation_id: "rec-1",
        excluded: true,
        excluded_at: new Date().toISOString(),
        excluded_by: "operator",
      })
      .mockResolvedValueOnce({
        recommendation_id: "rec-1",
        excluded: false,
      });
    await renderRecommendationsTabWith(recs);

    const user = userEvent.setup();
    // First click excludes.
    const excludeButtons = await screen.findAllByRole("button", {
      name: /Don't propose .* again/i,
    });
    await user.click(excludeButtons[0]);
    await waitFor(() => {
      expect(
        screen.getByRole("button", {
          name: /Restore .* as a recommendation/i,
        }),
      ).toBeInTheDocument();
    });

    // Second click restores.
    await user.click(
      screen.getByRole("button", {
        name: /Restore .* as a recommendation/i,
      }),
    );
    await waitFor(() => {
      expect(mockedSetRecommendationExclusion).toHaveBeenCalledTimes(2);
    });
    expect(mockedSetRecommendationExclusion).toHaveBeenNthCalledWith(2, {
      recommendation_id: "rec-1",
      connection_id: "123456789012",
      account_id: "123456789012",
      region: "us-east-1",
      recommendation_kind: "lambda-otel-layer",
      resource_id: "arn:aws:lambda:us-east-1:123:function:fn-1",
      excluded: false,
    });
    // Excluded badge clears.
    await waitFor(() => {
      expect(
        screen.queryByLabelText(/Excluded from future recommendations/i),
      ).not.toBeInTheDocument();
    });
    // Success toast on restore names the inverse behavior.
    expect(screen.getByTestId("exclusion-toast")).toHaveTextContent(
      /Restored — Squadron will propose this again/i,
    );
  });

  it("RecommendationsTab_OptimisticRollbackOnError", async () => {
    const recs = buildRecsResponse();
    mockedSetRecommendationExclusion.mockRejectedValueOnce(
      new Error("storage offline"),
    );
    await renderRecommendationsTabWith(recs);

    const user = userEvent.setup();
    const excludeButtons = await screen.findAllByRole("button", {
      name: /Don't propose .* again/i,
    });
    await user.click(excludeButtons[0]);

    // The POST fires; the badge briefly appears via optimistic
    // update, then disappears once the error rollback runs. The
    // error toast surfaces the err message verbatim.
    await waitFor(() => {
      expect(mockedSetRecommendationExclusion).toHaveBeenCalledTimes(1);
    });
    await waitFor(() => {
      expect(screen.getByTestId("exclusion-toast")).toHaveTextContent(
        /storage offline/,
      );
    });
    // Rolled back: no Excluded badge on either card.
    expect(
      screen.queryByLabelText(/Excluded from future recommendations/i),
    ).not.toBeInTheDocument();
    // Both rows still read "Don't propose this again" — the
    // optimistic update was undone.
    expect(
      screen.getAllByRole("button", { name: /Don't propose .* again/i }),
    ).toHaveLength(2);
  });

  // ----- v0.89.40 #660 Stream 58 (#531 slice 2 chunk 5 follow-on) ---
  //
  // Hydration on mount. Closes the chunk 5 TODO that the Excluded
  // badges were lost on page refresh. Two tests:
  //   - happy hydration: mocked list returns 2 exclusions; both
  //     corresponding rows render Excluded immediately on first
  //     paint. The third (non-excluded) row stays in the normal
  //     "Don't propose this again" posture.
  //   - error path: list throws; no badges, no error toast, all
  //     rows show the regular button. Console error is logged
  //     (acceptable graceful degradation).

  function buildHydrationRecsResponse(): GenerateRecommendationsResponse {
    // Three recs so the test can assert "2 excluded, 1 not".
    const mk = (i: number): Recommendation => ({
      id: `rec-${i}`,
      category: "empty_signal",
      severity: "warn",
      title: `Instrument resource ${i}`,
      detail: "lambda lacks otel",
      est_savings_bytes: 0,
      generated_at: new Date().toISOString(),
      source: { kind: "discovery_scan", ref_id: "scan-x" },
      action: { kind: "plan", payload: {} },
      iac: { format: "terraform", source: "resource ..." },
      resource_kind: "lambda-otel-layer",
      affected_resources: [`arn:aws:lambda:us-east-1:123:function:fn-${i}`],
    });
    return {
      declined: false,
      reasoning: "",
      recommendations: [mk(1), mk(2), mk(3)],
    };
  }

  it("RecommendationsTab_HydratesExcludedSetOnMount", async () => {
    const recs = buildHydrationRecsResponse();
    const hydrated: ExcludedRecommendation[] = [
      {
        recommendation_id: "rec-1",
        recommendation_kind: "lambda-otel-layer",
        resource_id: "arn:aws:lambda:us-east-1:123:function:fn-1",
        excluded_at: new Date().toISOString(),
        excluded_by: "alice",
      },
      {
        recommendation_id: "rec-2",
        recommendation_kind: "lambda-otel-layer",
        resource_id: "arn:aws:lambda:us-east-1:123:function:fn-2",
        excluded_at: new Date().toISOString(),
        excluded_by: "alice",
      },
    ];
    mockedListExcludedRecommendations.mockResolvedValueOnce(hydrated);

    await renderRecommendationsTabWith(recs);

    // GET fired with the scope tuple the tab was rendered against.
    await waitFor(() => {
      expect(mockedListExcludedRecommendations).toHaveBeenCalledWith({
        connection_id: "123456789012",
        account_id: "123456789012",
        region: "us-east-1",
      });
    });
    // The two seeded recommendations render with Excluded badges.
    await waitFor(() => {
      expect(
        screen.getAllByLabelText(/Excluded from future recommendations/i),
      ).toHaveLength(2);
    });
    // The third (non-seeded) row keeps the normal "Don't propose
    // this again" button.
    expect(
      screen.getAllByRole("button", { name: /Don't propose .* again/i }),
    ).toHaveLength(1);
    // And the two excluded rows surface Restore buttons.
    expect(
      screen.getAllByRole("button", { name: /Restore .* as a recommendation/i }),
    ).toHaveLength(2);
    // No toast on hydration — that's a side-effect of a user
    // action, not a passive load.
    expect(screen.queryByTestId("exclusion-toast")).not.toBeInTheDocument();
  });

  it("RecommendationsTab_HydrationErrorGraceful", async () => {
    const recs = buildHydrationRecsResponse();
    mockedListExcludedRecommendations.mockRejectedValueOnce(
      new Error("hydration backend offline"),
    );
    // Silence the console.error the graceful-degradation path emits
    // so the test output stays clean — the assertion is "no toast,
    // all rows show the regular button", not "no console output".
    const consoleErrorSpy = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});

    await renderRecommendationsTabWith(recs);

    // Wait for the hydration GET to have been attempted.
    await waitFor(() => {
      expect(mockedListExcludedRecommendations).toHaveBeenCalledTimes(1);
    });

    // No Excluded badges anywhere — the set stayed empty.
    expect(
      screen.queryByLabelText(/Excluded from future recommendations/i),
    ).not.toBeInTheDocument();
    // No toast — the failure is logged, not surfaced to the
    // operator as a banner.
    expect(screen.queryByTestId("exclusion-toast")).not.toBeInTheDocument();
    // All three rows render the regular "Don't propose this again"
    // button — the tab is fully usable despite the hydration miss.
    expect(
      screen.getAllByRole("button", { name: /Don't propose .* again/i }),
    ).toHaveLength(3);
    // Console.error was called with the failure (so an SRE looking
    // at the browser console can diagnose) — but no other side
    // effect surfaces to the UI.
    expect(consoleErrorSpy).toHaveBeenCalled();

    consoleErrorSpy.mockRestore();
  });

  // --- v0.89.77 trace integration slice 1 chunk 4 — last_seen_at ---

  it("TestInventoryTab_ComputeSubTab_LastSeenColumn_RendersRelativeTime", async () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    mockedRunAWSScan.mockResolvedValue({
      ...sampleScan,
      compute: [
        { ...sampleScan.compute[0], last_seen_at: fiveMinAgo },
      ],
    });
    const user = userEvent.setup();
    renderPageInSingleAccountView();

    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(screen.getByText(/Run an inventory scan/i)).toBeInTheDocument();
    });
    const select = screen.getByRole("combobox", {
      name: /Connected account/i,
    });
    await user.click(select);
    const option = await screen.findByRole("option", {
      name: /Prod AWS \(123456789012\)/,
    });
    await user.click(option);
    const runBtn = await screen.findByRole("button", { name: /Run scan/i });
    await user.click(runBtn);

    await waitFor(() => {
      expect(screen.getByText(/Last seen: 5m ago/)).toBeInTheDocument();
    });
  });

  it("TestInventoryTab_ComputeSubTab_LastSeenColumn_NeverValue", async () => {
    mockedListAWSConnections.mockResolvedValue({
      connections: sampleConnections,
    });
    mockedRunAWSScan.mockResolvedValue({
      ...sampleScan,
      compute: [
        { ...sampleScan.compute[0], last_seen_at: undefined },
      ],
    });
    const user = userEvent.setup();
    renderPageInSingleAccountView();

    await waitFor(() => {
      expect(screen.getByText("Prod AWS")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await waitFor(() => {
      expect(screen.getByText(/Run an inventory scan/i)).toBeInTheDocument();
    });
    const select = screen.getByRole("combobox", {
      name: /Connected account/i,
    });
    await user.click(select);
    const option = await screen.findByRole("option", {
      name: /Prod AWS \(123456789012\)/,
    });
    await user.click(option);
    const runBtn = await screen.findByRole("button", { name: /Run scan/i });
    await user.click(runBtn);

    await waitFor(() => {
      // At least one row renders with "never" + warning indicator.
      expect(screen.getAllByTestId("last-seen-never").length).toBeGreaterThan(0);
    });
  });

  // ----- v0.89.82 (#713 Stream 111, Trace integration slice 2 chunk 3) ---
  //
  // Filter chip above the recommendations list narrows to the slice-2
  // trace-emission-* drafts. Rendered directly via the exported
  // RecommendationsTab so the test stays focused on the filter
  // behavior, not the full page state machine. Recommendation kind is
  // recognised via the resource_kind prefix per recommendations.ts.

  it("TestDiscoveryAWS_RecommendationsTab_TraceEmissionFilter_FiltersList", async () => {
    const mk = (i: number, kind: string): Recommendation => ({
      id: `rec-${i}`,
      category: "empty_signal",
      severity: "warn",
      title: `Step ${i}`,
      detail: "draft",
      est_savings_bytes: 0,
      generated_at: new Date().toISOString(),
      source: { kind: "discovery_scan", ref_id: "scan-x" },
      iac: { format: "terraform", source: "resource ..." },
      resource_kind: kind,
      affected_resources: [`arn:aws:test::${i}`],
    });
    const recs: GenerateRecommendationsResponse = {
      declined: false,
      reasoning: "",
      recommendations: [
        mk(1, "trace-emission-ec2"),
        mk(2, "trace-emission-lambda"),
        mk(3, "trace-emission-rds"),
        mk(4, "ec2-otel-layer"),
        mk(5, "lambda-otel-layer"),
      ],
    };
    // Hydration GET defaults to empty so the Excluded badges don't
    // distract from the filter behavior.
    mockedListExcludedRecommendations.mockResolvedValueOnce([]);

    const { RecommendationsTab } = await import("./DiscoveryAWS");
    render(
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter>
          <RecommendationsTab
            recs={recs}
            accountID="123456789012"
            scanID="scan-x"
            region="us-east-1"
          />
        </MemoryRouter>
      </SWRConfig>,
    );

    // All 5 cards visible initially.
    expect(
      screen.getAllByTestId("discovery-recommendation-card"),
    ).toHaveLength(5);

    // Click the filter chip; only the 3 trace-emission-* cards remain.
    const chip = screen.getByTestId("trace-emission-filter-chip");
    expect(chip).toHaveAttribute("data-active", "false");
    const user = userEvent.setup();
    await user.click(chip);

    await waitFor(() => {
      expect(
        screen.getAllByTestId("discovery-recommendation-card"),
      ).toHaveLength(3);
    });
    expect(
      screen.getByTestId("trace-emission-filter-chip"),
    ).toHaveAttribute("data-active", "true");

    // Click again — filter clears, all 5 return.
    await user.click(screen.getByTestId("trace-emission-filter-chip"));
    await waitFor(() => {
      expect(
        screen.getAllByTestId("discovery-recommendation-card"),
      ).toHaveLength(5);
    });
  });

  // --- v0.89.87 #718 Stream 116 — Span quality slice 1 chunk 3 ----
  //
  // Per-Inventory-row Quality dot. The dot maps row.span_quality
  // (an optional RowSpanQuality with the three pathology
  // percentages) to a 4-state color: green=0 issues, yellow=1
  // issue, red=2+ issues, gray=undefined (no observations). The
  // tooltip surfaces the three percentages so the operator can
  // read the per-pathology contribution without leaving the
  // Inventory tab. The dot is exported from DiscoveryAWS.tsx so
  // we can render it in isolation rather than driving the whole
  // page state machine.

  it("TestDiscoveryAWS_Inventory_QualityDot_GreenWhenNoIssues", async () => {
    const { QualityDot } = await import("./DiscoveryAWS");
    render(
      <QualityDot
        quality={{
          orphan_pct: 0,
          missing_attr_pct: 0,
          attr_mismatch_pct: 0,
        }}
      />,
    );
    const dot = screen.getByTestId("quality-dot");
    expect(dot).toHaveAttribute("data-color", "green");
    // Tooltip surfaces the zero percentages — operator can confirm
    // the row was inspected (vs the gray "no observations" state).
    expect(dot).toHaveAttribute(
      "title",
      expect.stringContaining("Orphan 0.0%"),
    );
  });

  it("TestDiscoveryAWS_Inventory_QualityDot_YellowWhenOneIssue", async () => {
    const { QualityDot } = await import("./DiscoveryAWS");
    render(
      <QualityDot
        quality={{
          orphan_pct: 0,
          missing_attr_pct: 11.5,
          attr_mismatch_pct: 0,
        }}
      />,
    );
    const dot = screen.getByTestId("quality-dot");
    expect(dot).toHaveAttribute("data-color", "yellow");
  });

  it("TestDiscoveryAWS_Inventory_QualityDot_RedWhenMultipleIssues", async () => {
    const { QualityDot } = await import("./DiscoveryAWS");
    render(
      <QualityDot
        quality={{
          orphan_pct: 12.0,
          missing_attr_pct: 8.5,
          attr_mismatch_pct: 4.2,
        }}
      />,
    );
    expect(screen.getByTestId("quality-dot")).toHaveAttribute(
      "data-color",
      "red",
    );
  });

  it("TestDiscoveryAWS_Inventory_QualityDot_GrayWhenNoObservations", async () => {
    const { QualityDot } = await import("./DiscoveryAWS");
    // null is the per-resource fetcher's "404 mapped" signal —
    // same gray rendering as undefined so the dashboard handles
    // both identically.
    render(<QualityDot quality={undefined} />);
    const dot = screen.getByTestId("quality-dot");
    expect(dot).toHaveAttribute("data-color", "gray");
    expect(dot).toHaveAttribute(
      "title",
      expect.stringContaining("No spans observed"),
    );
  });

  it("TestDiscoveryAWS_Inventory_QualityDot_HoverTooltipShowsPercentages", async () => {
    const { QualityDot } = await import("./DiscoveryAWS");
    render(
      <QualityDot
        quality={{
          orphan_pct: 3.2,
          missing_attr_pct: 8.1,
          attr_mismatch_pct: 1.7,
        }}
      />,
    );
    const dot = screen.getByTestId("quality-dot");
    const tip = dot.getAttribute("title") ?? "";
    expect(tip).toContain("Orphan 3.2%");
    expect(tip).toContain("Missing attrs 8.1%");
    expect(tip).toContain("Mismatch 1.7%");
  });
});
