// Vitest coverage for the v0.89.48 #670 Stream 68 (slice-1 chunk 4)
// DiscoveryGCP page. Mirrors the DiscoveryAWS.test.tsx posture:
//   - SWR cache is wiped per-test via a fresh SWRConfig provider.
//   - Network mocked at the @/api/discoveryGCP module boundary.
//   - jsdom polyfills for Radix Select / Tabs pointer-capture.
//
// Test naming follows the chunk-4 brief's TestDiscoveryGCP_<area>_<behavior>
// convention so a future arc-spanning audit can grep for the GCP suite
// in one pass.

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

import DiscoveryGCPPage from "./DiscoveryGCP";

import {
  createGCPConnection,
  listGCPConnections,
  scanGCPConnection,
  validateGCPConnection,
  type GCPConnection,
  type ScanGCPResponse,
} from "@/api/discoveryGCP";

// jsdom polyfills for Radix Select / Tabs pointer-capture lookups.
// Same posture as the AWS test file — without these the components
// throw `target.hasPointerCapture is not a function` as soon as the
// user clicks a trigger.
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

vi.mock("@/api/discoveryGCP", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/discoveryGCP")>(
      "@/api/discoveryGCP",
    );
  return {
    ...actual,
    listGCPConnections: vi.fn(),
    createGCPConnection: vi.fn(),
    validateGCPConnection: vi.fn(),
    scanGCPConnection: vi.fn(),
  };
});

const mockedListGCPConnections = vi.mocked(listGCPConnections);
const mockedCreateGCPConnection = vi.mocked(createGCPConnection);
const mockedValidateGCPConnection = vi.mocked(validateGCPConnection);
const mockedScanGCPConnection = vi.mocked(scanGCPConnection);

// renderPage wraps the page in a fresh SWRConfig so each test starts
// with an empty cache. Mirrors the AWS page's renderPage helper.
function renderPage(initialEntries: string[] = ["/discovery/gcp"]) {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig
        value={{
          provider: () => new Map(),
          dedupingInterval: 0,
          revalidateOnFocus: false,
          revalidateOnReconnect: false,
        }}
      >
        <MemoryRouter initialEntries={initialEntries}>{children}</MemoryRouter>
      </SWRConfig>
    );
  }
  return render(<DiscoveryGCPPage />, { wrapper: Wrapper });
}

// validSAJSON is a minimal-but-complete service-account JSON blob the
// wizard's parser accepts: type + project_id + private_key_id +
// private_key + client_email + client_id. Only the three fields the
// wizard reads (client_email, private_key, project_id) are required;
// the others are just for realism so a future reader sees the typical
// shape.
function buildSAJSON(overrides: Partial<Record<string, string>> = {}): string {
  const base: Record<string, string> = {
    type: "service_account",
    project_id: "my-prod-project",
    private_key_id: "abcd1234",
    private_key:
      "-----BEGIN PRIVATE KEY-----\nfake-key-bytes\n-----END PRIVATE KEY-----\n",
    client_email: "squadron-discovery@my-prod-project.iam.gserviceaccount.com",
    client_id: "1234567890",
  };
  return JSON.stringify({ ...base, ...overrides });
}

const sampleConnection: GCPConnection = {
  id: "conn-uuid-1",
  display_name: "Production GCP",
  project_id: "my-prod-project",
  region: "us-central1",
  learn_from_accepted_recommendations: true,
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

const sampleScan: ScanGCPResponse = {
  connection_id: "conn-uuid-1",
  project_id: "my-prod-project",
  region: "us-central1",
  scan_id: "scan-uuid-1",
  compute: [
    {
      resource_id:
        "projects/my-prod-project/zones/us-central1-a/instances/web-1",
      instance_type: "e2-medium",
      tags: { otel: "true" },
      has_otel: true,
      os_family: "linux",
      region: "us-central1",
    },
    {
      resource_id:
        "projects/my-prod-project/zones/us-central1-a/instances/web-2",
      instance_type: "e2-medium",
      tags: { env: "prod" },
      has_otel: false,
      os_family: "linux",
      region: "us-central1",
    },
    {
      resource_id:
        "projects/my-prod-project/zones/us-central1-a/instances/web-3",
      instance_type: "n2-standard-2",
      tags: {},
      has_otel: false,
      os_family: "windows",
      region: "us-central1",
    },
    {
      resource_id:
        "projects/my-prod-project/zones/us-central1-a/instances/web-4",
      instance_type: "e2-medium",
      tags: {},
      has_otel: false,
      os_family: "linux",
      region: "us-central1",
    },
    {
      resource_id:
        "projects/my-prod-project/zones/us-central1-a/instances/web-5",
      instance_type: "e2-medium",
      tags: {},
      has_otel: false,
      os_family: "linux",
      region: "us-central1",
    },
  ],
  instrumented_count: 1,
  uninstrumented_count: 4,
  partial: false,
};

describe("DiscoveryGCP", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockedListGCPConnections.mockResolvedValue([]);
  });

  it("TestDiscoveryGCP_WizardStep1_ProjectIDValidation", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // Fill display name first so Next-enablement isn't gated on
    // the unrelated field.
    const displayName = screen.getByLabelText(/Display name/i);
    fireEvent.change(displayName, { target: { value: "Production GCP" } });

    const projectInput = screen.getByLabelText(/Project ID/i);

    // Invalid project ID (uppercase) — Next should stay disabled and
    // the inline error message renders.
    fireEvent.change(projectInput, { target: { value: "MyProject" } });
    expect(
      screen.getByText(/Project IDs must be 6 to 30 characters/i),
    ).toBeInTheDocument();
    const nextBtn = screen.getByRole("button", { name: /^Next$/i });
    expect(nextBtn).toBeDisabled();

    // Valid project ID — Next enables.
    fireEvent.change(projectInput, { target: { value: "my-prod-project" } });
    await waitFor(() => {
      expect(
        screen.queryByText(/Project IDs must be 6 to 30 characters/i),
      ).not.toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();
  });

  it("TestDiscoveryGCP_WizardStep3_RejectsInvalidSAJSON", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // Advance to step 3.
    await advanceToKeyPasteStep();

    const textarea = screen.getByLabelText(/Service-account key/i);
    fireEvent.change(textarea, { target: { value: "not json" } });

    // The error message renders inline under the textarea.
    expect(
      screen.getByText(/doesn't look like valid JSON/i),
    ).toBeInTheDocument();

    // The acknowledgment checkbox is disabled until parse succeeds.
    const ack = screen.getByRole("checkbox", {
      name: /I have read this warning/i,
    });
    expect(ack).toBeDisabled();

    // Next should be disabled.
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryGCP_WizardStep3_RejectsSAWithoutClientEmail", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToKeyPasteStep();

    // Parseable JSON, but missing client_email.
    const incomplete = JSON.stringify({
      type: "service_account",
      project_id: "my-prod-project",
      private_key:
        "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----",
      // client_email intentionally omitted.
    });
    const textarea = screen.getByLabelText(/Service-account key/i);
    fireEvent.change(textarea, { target: { value: incomplete } });

    expect(
      screen.getByText(/missing the "client_email" field/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryGCP_WizardStep4_ValidateSuccess_AdvancesToScan", async () => {
    const user = userEvent.setup();
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 3,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);

    // Hit the Validate button.
    const validateBtn = screen.getByRole("button", {
      name: /Validate connection/i,
    });
    await user.click(validateBtn);

    // Success banner with the instance count.
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 3 instances visible/i),
      ).toBeInTheDocument();
    });

    // Next is now enabled — clicking it should land on the scan step.
    const nextBtn = screen.getByRole("button", { name: /^Next$/i });
    expect(nextBtn).toBeEnabled();
    await user.click(nextBtn);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });

    // Create + validate were each called once.
    expect(mockedCreateGCPConnection).toHaveBeenCalledTimes(1);
    expect(mockedValidateGCPConnection).toHaveBeenCalledWith(
      sampleConnection.id,
    );
  });

  it("TestDiscoveryGCP_WizardStep4_ProjectMismatch_ShowsRemediation", async () => {
    const user = userEvent.setup();
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: false,
      error_kind: "project_mismatch",
      message: "SA JSON is for a different project.",
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);

    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );

    // Specific project_mismatch remediation copy — the SA we pasted
    // was for "my-prod-project" so it appears in the remediation as
    // "(unknown)" only if the SA project_id field was missing, which
    // it isn't in our fixture. Assert on the prose pattern.
    await waitFor(() => {
      expect(
        screen.getByText(/Either change the connection's project ID/i),
      ).toBeInTheDocument();
    });
    // The server's message renders too.
    expect(
      screen.getByText(/SA JSON is for a different project/i),
    ).toBeInTheDocument();
    // Next stays disabled — operator must fix the upstream state.
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryGCP_WizardStep5_ScanSuccess_TransitionsToInventory", async () => {
    const user = userEvent.setup();
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue(sampleScan);
    // After the wizard succeeds the page re-lists connections; return
    // the new row so the selector shows it.
    mockedListGCPConnections.mockResolvedValueOnce([]);
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    // Scan handler hands the result up; page auto-switches to the
    // Inventory tab.
    await waitFor(() => {
      const inventoryTab = screen.getByRole("tab", { name: /Inventory/i });
      expect(inventoryTab).toHaveAttribute("data-state", "active");
    });

    // The table renders with the seeded 5 rows.
    await waitFor(() => {
      expect(screen.getByText(/instances\/web-1/)).toBeInTheDocument();
    });
    expect(screen.getByText(/instances\/web-5/)).toBeInTheDocument();
    expect(mockedScanGCPConnection).toHaveBeenCalledWith(sampleConnection.id);
  });

  it("TestDiscoveryGCP_InventoryTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // Walk the wizard so the scan result lands on the page state.
    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    // Inventory tab should have the 5-row table + summary row.
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Instrumented: 1/)).toBeInTheDocument();
    expect(screen.getByText(/Uninstrumented: 4/)).toBeInTheDocument();

    // Each compute row's resource_id is present.
    for (const row of sampleScan.compute) {
      expect(screen.getByText(row.resource_id)).toBeInTheDocument();
    }
  });

  // v0.89.201 — the Recommendations tab is now wired (chunk-5 proposer
  // + the recs UI). With no scan selected it shows the run-a-scan-first
  // empty state; the generate flow + shared RecommendationsTab render
  // once a scan exists.
  it("TestDiscoveryGCP_RecommendationsTab_EmptyStateWithoutScan", async () => {
    const user = userEvent.setup();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await user.click(screen.getByRole("tab", { name: /Recommendations/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/Run a scan from the Inventory tab first/i),
      ).toBeInTheDocument();
    });
  });

  // Database tier slice 2 chunk 5 (v0.89.66, #695 Stream 93) —
  // Inventory tab gains a Databases sub-tab rendering the Cloud
  // SQL inventory from the chunk 2 scanner extension. Default
  // sub-tab is Compute so the slice-1 UX stays untouched; switching
  // to Databases renders the per-row table with the Query Insights
  // instrumentation column.
  it("TestDiscoveryGCP_InventoryTab_DatabasesSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      databases: [
        {
          resource_id: "projects/my-prod-project/instances/db-covered",
          engine: "postgres",
          engine_version: "15",
          instance_class: "db-custom-2-7680",
          region: "us-central1",
          provider: "gcp",
          query_insights_enabled: true,
          tags: { env: "prod" },
        },
        {
          resource_id: "projects/my-prod-project/instances/db-uncovered",
          engine: "mysql",
          engine_version: "8.0",
          instance_class: "db-n1-standard-1",
          region: "us-central1",
          provider: "gcp",
          query_insights_enabled: false,
          tags: {},
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    // Default sub-tab is Compute — switch to Databases to inspect
    // the new table.
    const databasesTab = screen.getByRole("tab", { name: /^Databases$/i });
    await user.click(databasesTab);
    expect(databasesTab).toHaveAttribute("data-state", "active");

    // Both database rows render. The covered row's instrumentation
    // column reads "Yes"; the uncovered row reads "No". The
    // instrumentation column header reads "Query Insights enabled?".
    expect(
      screen.getByText("projects/my-prod-project/instances/db-covered"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("projects/my-prod-project/instances/db-uncovered"),
    ).toBeInTheDocument();
    expect(screen.getByText(/Query Insights enabled\?/i)).toBeInTheDocument();
  });

  // Database tier slice 2 chunk 5 — empty-state UX: scans without
  // databases (or older scan rows from before the chunk 2 scanner
  // extension where the wire field is undefined) render the empty
  // message rather than an empty table or a crash.
  it("TestDiscoveryGCP_InventoryTab_DatabasesSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const databasesTab = screen.getByRole("tab", { name: /^Databases$/i });
    await user.click(databasesTab);
    expect(
      screen.getByText(/No databases discovered\. Run a scan to refresh\./i),
    ).toBeInTheDocument();
  });

  // Kubernetes tier slice 2 chunk 5 (v0.89.71, #702 Stream 100) —
  // Inventory tab gains a Kubernetes sub-tab rendering the GKE
  // cluster inventory from the v0.89.70 chunk 2 scanner extension.
  // The instrumentation column reads from managed_prometheus_enabled
  // (the GCP single-axis observability lever).
  it("TestDiscoveryGCP_InventoryTab_KubernetesSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      clusters: [
        {
          resource_id:
            "projects/my-prod-project/locations/us-central1/clusters/gke-covered",
          name: "gke-covered",
          kubernetes_version: "1.29",
          status: "RUNNING",
          region: "us-central1",
          provider: "gcp",
          managed_prometheus_enabled: true,
          tags: { env: "prod" },
        },
        {
          resource_id:
            "projects/my-prod-project/locations/us-central1/clusters/gke-uncovered",
          name: "gke-uncovered",
          kubernetes_version: "1.29",
          status: "RUNNING",
          region: "us-central1",
          provider: "gcp",
          managed_prometheus_enabled: false,
          tags: {},
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const kubernetesTab = screen.getByRole("tab", { name: /^Kubernetes$/i });
    await user.click(kubernetesTab);
    expect(kubernetesTab).toHaveAttribute("data-state", "active");

    // Both cluster rows render. The covered row's instrumentation
    // column reads "Yes"; the uncovered row reads "No". The
    // instrumentation column header reads "Managed Prometheus?".
    expect(
      screen.getByText(
        "projects/my-prod-project/locations/us-central1/clusters/gke-covered",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "projects/my-prod-project/locations/us-central1/clusters/gke-uncovered",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText(/Managed Prometheus\?/i)).toBeInTheDocument();
  });

  // Kubernetes tier slice 2 chunk 5 — empty-state UX: scans without
  // clusters (or older scan rows from before the v0.89.70 chunk 2
  // scanner extension where the wire field is undefined) render the
  // empty message rather than an empty table or a crash.
  it("TestDiscoveryGCP_InventoryTab_KubernetesSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const kubernetesTab = screen.getByRole("tab", { name: /^Kubernetes$/i });
    await user.click(kubernetesTab);
    expect(
      screen.getByText(
        /No Kubernetes clusters discovered\. Run a scan to refresh\./i,
      ),
    ).toBeInTheDocument();
  });

  // Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) —
  // Inventory tab gains a Serverless sub-tab rendering the Cloud Run
  // + Cloud Functions inventory from the chunk 2 GCP scanner
  // extension. The Surface column shows "cloudrun" / "cloudfunc" per
  // row. Empty state mirrors the Databases / Kubernetes sub-tabs.
  it("TestDiscoveryGCP_InventoryTab_ServerlessSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "gcp",
          surface: "cloudrun",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "api-svc",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/services/api-svc",
          runtime: "go1.21",
          has_trace_axis: true,
          has_otel_distro: true,
        },
        {
          provider: "gcp",
          surface: "cloudfunc",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "nightly-cron",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/functions/nightly-cron",
          runtime: "python3.11",
          has_trace_axis: false,
          has_otel_distro: false,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(serverlessTab).toHaveAttribute("data-state", "active");

    expect(screen.getByText("api-svc")).toBeInTheDocument();
    expect(screen.getByText("nightly-cron")).toBeInTheDocument();
    expect(screen.getByText("cloudrun")).toBeInTheDocument();
    expect(screen.getByText("cloudfunc")).toBeInTheDocument();
  });

  it("TestDiscoveryGCP_InventoryTab_ServerlessSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(
      screen.getByText(
        /No serverless functions discovered\. Run a scan to refresh\./i,
      ),
    ).toBeInTheDocument();
  });

  // --- Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream 157) ---

  // TestDiscoveryGCP_Serverless_ColdStartColumnRenders — slice 2 §11
  // acceptance test 13 extension. The GCP Serverless table's
  // "Cold-start P95 (24h)" column renders between OTel distro and
  // Last seen. Mirrors the AWS column from slice 1 chunk 3.
  it("TestDiscoveryGCP_Serverless_ColdStartColumnRenders", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "gcp",
          surface: "cloudrun",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "checkout-svc",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/services/checkout-svc",
          runtime: "go1.21",
          has_trace_axis: true,
          has_otel_distro: true,
          cold_start_p95_ms: 1800,
          cold_start_exceeds_threshold: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(screen.getByText(/Cold-start P95 \(24h\)/i)).toBeInTheDocument();
  });

  // TestDiscoveryGCP_Serverless_ColdStartCell_AmberWhenExceedsThreshold —
  // when the server's cold_start_exceeds_threshold flag is true, the
  // cell value renders in the amber-600 color class with the
  // data-value="amber" attribute. Mirrors the AWS test from slice 1.
  it("TestDiscoveryGCP_Serverless_ColdStartCell_AmberWhenExceedsThreshold", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "gcp",
          surface: "cloudrun",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "hot-cloudrun",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/services/hot-cloudrun",
          runtime: "go1.21",
          has_trace_axis: true,
          has_otel_distro: false,
          cold_start_p95_ms: 1800,
          cold_start_exceeds_threshold: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);

    const cells = screen.getAllByTestId("cold-start-cell");
    expect(cells.length).toBeGreaterThan(0);
    const cell = cells[0];
    expect(cell).toHaveAttribute("data-value", "amber");
    expect(cell.className).toMatch(/text-amber-600/);
    expect(cell.textContent).toMatch(/1800ms/);
  });

  // --- Sampling rate slice 1 chunk 3 (v0.89.124, #764 Stream 162) ---

  it("TestDiscoveryGCP_Serverless_SamplingRateColumnRenders", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "gcp",
          surface: "cloudrun",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "sampling-svc",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/services/sampling-svc",
          runtime: "go1.21",
          has_trace_axis: true,
          has_otel_distro: true,
          sampling_ratio: 0.041,
          sampling_exceeds_floor: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(screen.getByText(/Sampling rate \(24h\)/i)).toBeInTheDocument();
  });

  it("TestDiscoveryGCP_Serverless_SamplingRateCell_AmberWhenBelowFloor", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "gcp",
          surface: "cloudrun",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "below-floor-svc",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/services/below-floor-svc",
          runtime: "go1.21",
          has_trace_axis: true,
          has_otel_distro: false,
          sampling_ratio: 0.025,
          sampling_exceeds_floor: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);

    const cells = screen.getAllByTestId("sampling-rate-cell");
    expect(cells.length).toBeGreaterThan(0);
    const cell = cells[0];
    expect(cell).toHaveAttribute("data-value", "amber");
    expect(cell.className).toMatch(/text-amber-600/);
    expect(cell.textContent).toMatch(/2.5%/);
  });

  // --- Error rate slice 1 chunk 3 (v0.89.129, #769 Stream 167) ---

  it("TestDiscoveryGCP_Serverless_ErrorRateColumnRenders", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "gcp",
          surface: "cloudrun",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "er-render-svc",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/services/er-render-svc",
          runtime: "go1.21",
          has_trace_axis: true,
          has_otel_distro: false,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(screen.getByText(/Error rate \(24h\)/i)).toBeInTheDocument();
  });

  it("TestDiscoveryGCP_Serverless_ErrorRateCell_AmberWhenExceedsThreshold", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "gcp",
          surface: "cloudrun",
          account_id: "my-prod-project",
          region: "us-central1",
          resource_name: "er-amber-svc",
          resource_arn:
            "projects/my-prod-project/locations/us-central1/services/er-amber-svc",
          runtime: "go1.21",
          has_trace_axis: true,
          has_otel_distro: false,
          current_error_rate: 0.025,
          error_rate_exceeds_threshold: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);

    const cells = screen.getAllByTestId("error-rate-cell");
    expect(cells.length).toBeGreaterThan(0);
    const cell = cells[0];
    expect(cell).toHaveAttribute("data-value", "amber");
    expect(cell.className).toMatch(/text-amber-600/);
    expect(cell.textContent).toMatch(/2.50%/);
  });

  // --- helpers ---

  // advanceToKeyPasteStep walks the wizard from step 1 (project) to
  // step 3 (key paste). Used by the step-3 tests; the wizard's Next
  // button is gated on each step's validation, so the helper has to
  // fill the project step's required fields before clicking Next.
  async function advanceToKeyPasteStep() {
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production GCP" },
    });
    fireEvent.change(screen.getByLabelText(/Project ID/i), {
      target: { value: "my-prod-project" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    // Step 2 — no inputs, just instructions; Next advances directly.
    await waitFor(() => {
      expect(
        screen.getByText(/Create the service account/i),
      ).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(screen.getByLabelText(/Service-account key/i)).toBeInTheDocument();
    });
  }

  // advanceToValidateStep walks the wizard from step 1 to step 4.
  // userEvent is used (not fireEvent) for the checkbox click because
  // Radix Checkbox branches on pointer events, which fireEvent.click
  // doesn't fire.
  async function advanceToValidateStep(
    user: ReturnType<typeof userEvent.setup>,
  ) {
    await advanceToKeyPasteStep();
    fireEvent.change(screen.getByLabelText(/Service-account key/i), {
      target: { value: buildSAJSON() },
    });
    // Wait for the parse to settle so the checkbox enables.
    await waitFor(() => {
      const ack = screen.getByRole("checkbox", {
        name: /I have read this warning/i,
      });
      expect(ack).not.toBeDisabled();
    });
    await user.click(
      screen.getByRole("checkbox", {
        name: /I have read this warning/i,
      }),
    );
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Validate connection/i }),
      ).toBeInTheDocument();
    });
  }

  // --- v0.89.77 trace integration slice 1 chunk 4 — last_seen_at ---

  it("TestInventoryTab_ComputeSubTab_LastSeenColumn_RendersRelativeTime", async () => {
    const user = userEvent.setup();
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      compute: [
        {
          ...sampleScan.compute[0],
          last_seen_at: fiveMinAgo,
        },
      ],
      instrumented_count: 1,
      uninstrumented_count: 0,
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/5m ago/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Last seen/i)).toBeInTheDocument();
  });

  it("TestInventoryTab_ComputeSubTab_LastSeenColumn_NeverValue", async () => {
    const user = userEvent.setup();
    mockedListGCPConnections.mockResolvedValue([sampleConnection]);
    mockedCreateGCPConnection.mockResolvedValue(sampleConnection);
    mockedValidateGCPConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanGCPConnection.mockResolvedValue({
      ...sampleScan,
      compute: [
        {
          ...sampleScan.compute[0],
          last_seen_at: undefined,
        },
      ],
      instrumented_count: 1,
      uninstrumented_count: 0,
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    await advanceToValidateStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getAllByTestId("last-seen-never").length).toBeGreaterThan(
        0,
      );
    });
  });
});

// within is imported above for potential per-row scoping; silence the
// unused warning by referencing it once. (Some tests may scope by
// table row in a follow-up; keeping the import keeps the diff narrow
// when that lands.)
void within;
