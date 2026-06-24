// Vitest coverage for the v0.89.53 #677 Stream 75 (slice-1 chunk 4)
// DiscoveryAzure page. Mirrors the DiscoveryGCP.test.tsx posture:
//   - SWR cache is wiped per-test via a fresh SWRConfig provider.
//   - Network mocked at the @/api/discoveryAzure module boundary.
//   - jsdom polyfills for Radix Select / Tabs pointer-capture.
//
// Test naming follows the chunk-4 brief's TestDiscoveryAzure_<area>_<behavior>
// convention so a future arc-spanning audit can grep for the Azure suite
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

import DiscoveryAzurePage from "./DiscoveryAzure";

import {
  createAzureConnection,
  listAzureConnections,
  scanAzureConnection,
  validateAzureConnection,
  type AzureConnection,
  type ScanAzureResponse,
} from "@/api/discoveryAzure";

// jsdom polyfills for Radix Select / Tabs pointer-capture lookups.
// Same posture as the GCP test file — without these the components
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

vi.mock("@/api/discoveryAzure", async () => {
  const actual = await vi.importActual<typeof import("@/api/discoveryAzure")>(
    "@/api/discoveryAzure",
  );
  return {
    ...actual,
    listAzureConnections: vi.fn(),
    createAzureConnection: vi.fn(),
    validateAzureConnection: vi.fn(),
    scanAzureConnection: vi.fn(),
  };
});

const mockedListAzureConnections = vi.mocked(listAzureConnections);
const mockedCreateAzureConnection = vi.mocked(createAzureConnection);
const mockedValidateAzureConnection = vi.mocked(validateAzureConnection);
const mockedScanAzureConnection = vi.mocked(scanAzureConnection);

// renderPage wraps the page in a fresh SWRConfig so each test starts
// with an empty cache. Mirrors the GCP page's renderPage helper.
function renderPage(initialEntries: string[] = ["/discovery/azure"]) {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter initialEntries={initialEntries}>{children}</MemoryRouter>
      </SWRConfig>
    );
  }
  return render(<DiscoveryAzurePage />, { wrapper: Wrapper });
}

// Canonical UUIDs used throughout the tests. Real Azure UUIDs in this
// shape — the regex validator only cares about the shape, not the
// content, so any valid UUID works.
const TENANT_ID = "11111111-1111-1111-1111-111111111111";
const SUBSCRIPTION_ID = "22222222-2222-2222-2222-222222222222";
const CLIENT_ID = "33333333-3333-3333-3333-333333333333";

const sampleConnection: AzureConnection = {
  id: "conn-uuid-1",
  display_name: "Production Azure",
  tenant_id: TENANT_ID,
  subscription_id: SUBSCRIPTION_ID,
  client_id: CLIENT_ID,
  location: "eastus",
  learn_from_accepted_recommendations: true,
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

const sampleScan: ScanAzureResponse = {
  connection_id: "conn-uuid-1",
  subscription_id: SUBSCRIPTION_ID,
  location: "eastus",
  scan_id: "scan-uuid-1",
  compute: [
    {
      resource_id: "web-1",
      instance_type: "Standard_D4s_v3",
      tags: { otel: "true" },
      has_otel: true,
      os_family: "linux",
      region: "eastus",
    },
    {
      resource_id: "web-2",
      instance_type: "Standard_D4s_v3",
      tags: { env: "prod" },
      has_otel: false,
      os_family: "linux",
      region: "eastus",
    },
    {
      resource_id: "web-3",
      instance_type: "Standard_B2s",
      tags: {},
      has_otel: false,
      os_family: "windows",
      region: "eastus",
    },
    {
      resource_id: "web-4",
      instance_type: "Standard_D4s_v3",
      tags: {},
      has_otel: false,
      os_family: "linux",
      region: "eastus",
    },
    {
      resource_id: "web-5",
      instance_type: "Standard_D4s_v3",
      tags: {},
      has_otel: false,
      os_family: "linux",
      region: "eastus",
    },
  ],
  instrumented_count: 1,
  uninstrumented_count: 4,
  partial: false,
};

describe("DiscoveryAzure", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockedListAzureConnections.mockResolvedValue([]);
  });

  it("TestDiscoveryAzure_WizardStep1_TenantIDValidation", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // Fill display name + subscription ID so Next-enablement isn't
    // gated on the unrelated fields.
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production Azure" },
    });
    fireEvent.change(screen.getByLabelText(/Subscription ID/i), {
      target: { value: SUBSCRIPTION_ID },
    });

    const tenantInput = screen.getByLabelText(/Tenant ID/i);

    // Invalid tenant ID (non-UUID) — Next stays disabled and the
    // inline error message renders.
    fireEvent.change(tenantInput, { target: { value: "not-a-uuid" } });
    expect(
      screen.getByText(/Tenant IDs must be a UUID/i),
    ).toBeInTheDocument();
    const nextBtn = screen.getByRole("button", { name: /^Next$/i });
    expect(nextBtn).toBeDisabled();

    // Valid tenant ID — Next enables.
    fireEvent.change(tenantInput, { target: { value: TENANT_ID } });
    await waitFor(() => {
      expect(
        screen.queryByText(/Tenant IDs must be a UUID/i),
      ).not.toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();
  });

  it("TestDiscoveryAzure_WizardStep1_SubscriptionIDValidation", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // Fill display name + tenant so Next-enablement isn't gated on
    // unrelated fields.
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production Azure" },
    });
    fireEvent.change(screen.getByLabelText(/Tenant ID/i), {
      target: { value: TENANT_ID },
    });

    const subInput = screen.getByLabelText(/Subscription ID/i);

    // Invalid (non-UUID) — Next stays disabled.
    fireEvent.change(subInput, { target: { value: "12345" } });
    expect(
      screen.getByText(/Subscription IDs must be a UUID/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();

    // Valid UUID — Next enables.
    fireEvent.change(subInput, { target: { value: SUBSCRIPTION_ID } });
    await waitFor(() => {
      expect(
        screen.queryByText(/Subscription IDs must be a UUID/i),
      ).not.toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();
  });

  it("TestDiscoveryAzure_WizardStep3_RequiresClientIDAndSecret", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToCredentialsStep();

    // Acknowledgment checkbox is disabled until client_id + secret
    // both validate.
    const ack = screen.getByRole("checkbox", {
      name: /I have stored this secret securely/i,
    });
    expect(ack).toBeDisabled();

    // Paste a malformed client_id — inline error renders.
    const clientIDInput = screen.getByLabelText(/Client ID/i);
    fireEvent.change(clientIDInput, { target: { value: "not-a-uuid" } });
    expect(
      screen.getByText(/Client IDs must be a UUID/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();

    // Fix the client_id but leave the secret empty — ack still
    // disabled, Next still disabled.
    fireEvent.change(clientIDInput, { target: { value: CLIENT_ID } });
    expect(ack).toBeDisabled();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();

    // Paste a secret — ack enables but is still unchecked so Next
    // stays disabled.
    fireEvent.change(screen.getByLabelText(/Client Secret/i), {
      target: { value: "supersecret" },
    });
    await waitFor(() => {
      expect(
        screen.getByRole("checkbox", {
          name: /I have stored this secret securely/i,
        }),
      ).not.toBeDisabled();
    });
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryAzure_WizardStep4_ValidateSuccess_AdvancesToScan", async () => {
    const user = userEvent.setup();
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({
      ok: true,
      instance_count: 3,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateStep(user);

    const validateBtn = screen.getByRole("button", {
      name: /Validate connection/i,
    });
    await user.click(validateBtn);

    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 3 virtual machines visible/i),
      ).toBeInTheDocument();
    });

    const nextBtn = screen.getByRole("button", { name: /^Next$/i });
    expect(nextBtn).toBeEnabled();
    await user.click(nextBtn);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });

    expect(mockedCreateAzureConnection).toHaveBeenCalledTimes(1);
    expect(mockedValidateAzureConnection).toHaveBeenCalledWith(
      sampleConnection.id,
    );
  });

  it("TestDiscoveryAzure_WizardStep4_PermissionDenied_ShowsRemediation", async () => {
    const user = userEvent.setup();
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({
      ok: false,
      error_kind: "permission_denied",
      message: "AuthorizationFailed: SP lacks Reader on the subscription.",
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
        screen.getByText(/Verify the Service Principal has the Reader role/i),
      ).toBeInTheDocument();
    });
    // Server's message renders too.
    expect(
      screen.getByText(/AuthorizationFailed: SP lacks Reader/i),
    ).toBeInTheDocument();
    // Next stays disabled — operator must fix the upstream state.
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryAzure_WizardStep4_CredentialsInvalid_ShowsRemediation", async () => {
    const user = userEvent.setup();
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({
      ok: false,
      error_kind: "credentials_invalid",
      message: "AADSTS7000215: Invalid client secret provided.",
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
        screen.getByText(/Re-check the Client ID and Client Secret/i),
      ).toBeInTheDocument();
    });
    expect(
      screen.getByText(/AADSTS7000215: Invalid client secret/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryAzure_WizardStep5_ScanSuccess_TransitionsToInventory", async () => {
    const user = userEvent.setup();
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanAzureConnection.mockResolvedValue(sampleScan);
    // After the wizard succeeds the page re-lists connections; return
    // the new row so the selector shows it.
    mockedListAzureConnections.mockResolvedValueOnce([]);
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);

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
        screen.getByText(/Connected — 5 virtual machines visible/i),
      ).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    // Page auto-switches to the Inventory tab.
    await waitFor(() => {
      const inventoryTab = screen.getByRole("tab", { name: /Inventory/i });
      expect(inventoryTab).toHaveAttribute("data-state", "active");
    });

    // The table renders with the seeded 5 rows.
    await waitFor(() => {
      expect(screen.getByText("web-1")).toBeInTheDocument();
    });
    expect(screen.getByText("web-5")).toBeInTheDocument();
    expect(mockedScanAzureConnection).toHaveBeenCalledWith(sampleConnection.id);
  });

  it("TestDiscoveryAzure_InventoryTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanAzureConnection.mockResolvedValue(sampleScan);

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
        screen.getByText(/Connected — 5 virtual machines visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Run scan/i }),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    // Inventory tab should have the 5-row table + summary.
    await waitFor(() => {
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Instrumented: 1/)).toBeInTheDocument();
    expect(screen.getByText(/Uninstrumented: 4/)).toBeInTheDocument();

    // Each compute row's resource_id is present.
    for (const row of sampleScan.compute) {
      expect(screen.getByText(row.resource_id)).toBeInTheDocument();
    }
  });

  it("TestDiscoveryAzure_RecommendationsTab_ChunkStubMessage", async () => {
    const user = userEvent.setup();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await user.click(
      screen.getByRole("tab", { name: /Recommendations/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/ships in chunk 5 of this arc/i),
      ).toBeInTheDocument();
    });
  });

  // Database tier slice 2 chunk 5 (v0.89.66, #695 Stream 93) —
  // Inventory tab gains a Databases sub-tab rendering the Azure SQL
  // inventory from the chunk 3 scanner extension. The
  // instrumentation column reads from sql_insights_diag_enabled.
  it("TestDiscoveryAzure_InventoryTab_DatabasesSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue({
      ...sampleScan,
      databases: [
        {
          resource_id: "/subscriptions/s/databases/db-covered",
          engine: "sqlserver",
          engine_version: "12.0",
          instance_class: "GP_S_Gen5_2",
          region: "eastus",
          provider: "azure",
          sql_insights_diag_enabled: true,
          tags: { env: "prod" },
        },
        {
          resource_id: "/subscriptions/s/databases/db-uncovered",
          engine: "sqlserver",
          engine_version: "12.0",
          instance_class: "GP_S_Gen5_1",
          region: "eastus",
          provider: "azure",
          sql_insights_diag_enabled: false,
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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const databasesTab = screen.getByRole("tab", { name: /^Databases$/i });
    await user.click(databasesTab);
    expect(databasesTab).toHaveAttribute("data-state", "active");
    expect(
      screen.getByText("/subscriptions/s/databases/db-covered"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("/subscriptions/s/databases/db-uncovered"),
    ).toBeInTheDocument();
    expect(screen.getByText(/SQLInsights routed\?/i)).toBeInTheDocument();
  });

  // Database tier slice 2 chunk 5 — empty-state UX for Azure.
  it("TestDiscoveryAzure_InventoryTab_DatabasesSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue(sampleScan);

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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const databasesTab = screen.getByRole("tab", { name: /^Databases$/i });
    await user.click(databasesTab);
    expect(
      screen.getByText(/No databases discovered\. Run a scan to refresh\./i),
    ).toBeInTheDocument();
  });

  // Kubernetes tier slice 2 chunk 5 (v0.89.71, #702 Stream 100) —
  // Inventory tab gains a Kubernetes sub-tab rendering the AKS
  // cluster inventory from the v0.89.70 chunk 3 scanner extension.
  // The instrumentation column reads from azure_monitor_enabled.
  it("TestDiscoveryAzure_InventoryTab_KubernetesSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue({
      ...sampleScan,
      clusters: [
        {
          resource_id: "/subscriptions/s/managedClusters/aks-covered",
          name: "aks-covered",
          kubernetes_version: "1.29",
          status: "Succeeded",
          region: "eastus",
          provider: "azure",
          azure_monitor_enabled: true,
          tags: { env: "prod" },
        },
        {
          resource_id: "/subscriptions/s/managedClusters/aks-uncovered",
          name: "aks-uncovered",
          kubernetes_version: "1.29",
          status: "Succeeded",
          region: "eastus",
          provider: "azure",
          azure_monitor_enabled: false,
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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const kubernetesTab = screen.getByRole("tab", { name: /^Kubernetes$/i });
    await user.click(kubernetesTab);
    expect(kubernetesTab).toHaveAttribute("data-state", "active");
    expect(
      screen.getByText("/subscriptions/s/managedClusters/aks-covered"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("/subscriptions/s/managedClusters/aks-uncovered"),
    ).toBeInTheDocument();
    expect(screen.getByText(/Azure Monitor\?/i)).toBeInTheDocument();
  });

  // Kubernetes tier slice 2 chunk 5 — empty-state UX for Azure.
  it("TestDiscoveryAzure_InventoryTab_KubernetesSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue(sampleScan);

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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const kubernetesTab = screen.getByRole("tab", { name: /^Kubernetes$/i });
    await user.click(kubernetesTab);
    expect(
      screen.getByText(/No Kubernetes clusters discovered\. Run a scan to refresh\./i),
    ).toBeInTheDocument();
  });

  // Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) —
  // Inventory tab gains a Serverless sub-tab rendering the Azure
  // Functions inventory from the chunk 3 scanner extension.
  it("TestDiscoveryAzure_InventoryTab_ServerlessSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "azure",
          surface: "azfunc",
          account_id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
          region: "eastus",
          resource_name: "queue-consumer",
          resource_arn:
            "/subscriptions/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/resourceGroups/rg/providers/Microsoft.Web/sites/queue-consumer",
          runtime: "dotnet8",
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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(serverlessTab).toHaveAttribute("data-state", "active");
    expect(screen.getByText("queue-consumer")).toBeInTheDocument();
    expect(screen.getByText("azfunc")).toBeInTheDocument();
  });

  it("TestDiscoveryAzure_InventoryTab_ServerlessSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue(sampleScan);

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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(
      screen.getByText(/No serverless functions discovered\. Run a scan to refresh\./i),
    ).toBeInTheDocument();
  });

  // --- Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream 157) ---

  // TestDiscoveryAzure_Serverless_ColdStartColumnRenders — slice 2 §11
  // acceptance test 13 extension. The Azure Serverless table's
  // "Cold-start P95 (24h)" column renders between OTel distro and
  // Last seen. Mirrors the AWS column from slice 1 chunk 3.
  it("TestDiscoveryAzure_Serverless_ColdStartColumnRenders", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "azure",
          surface: "azfunc",
          account_id: "sub1",
          region: "eastus",
          resource_name: "payments-fn",
          resource_arn:
            "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Web/sites/payments-fn",
          runtime: "dotnet6",
          has_trace_axis: true,
          has_otel_distro: true,
          cold_start_p95_ms: 3200,
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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(screen.getByText(/Cold-start P95 \(24h\)/i)).toBeInTheDocument();
  });

  // TestDiscoveryAzure_Serverless_ColdStartCell_AmberWhenExceedsThreshold —
  // when the server's cold_start_exceeds_threshold flag is true, the
  // cell renders in amber. Mirrors AWS slice 1 test.
  it("TestDiscoveryAzure_Serverless_ColdStartCell_AmberWhenExceedsThreshold", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({ ok: true, instance_count: 5 });
    mockedScanAzureConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "azure",
          surface: "azfunc",
          account_id: "sub1",
          region: "eastus",
          resource_name: "hot-azfunc",
          resource_arn:
            "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Web/sites/hot-azfunc",
          runtime: "dotnet6",
          has_trace_axis: true,
          has_otel_distro: false,
          cold_start_p95_ms: 3200,
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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/VMs: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);

    const cells = screen.getAllByTestId("cold-start-cell");
    expect(cells.length).toBeGreaterThan(0);
    const cell = cells[0];
    expect(cell).toHaveAttribute("data-value", "amber");
    expect(cell.className).toMatch(/text-amber-600/);
    expect(cell.textContent).toMatch(/3200ms/);
  });

  // --- helpers ---

  // advanceToServicePrincipalStep walks the wizard from step 1
  // (subscription) to step 2 (Service Principal create instructions).
  async function advanceToServicePrincipalStep() {
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production Azure" },
    });
    fireEvent.change(screen.getByLabelText(/Tenant ID/i), {
      target: { value: TENANT_ID },
    });
    fireEvent.change(screen.getByLabelText(/Subscription ID/i), {
      target: { value: SUBSCRIPTION_ID },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    // Step 2's body has the unique "az ad sp create-for-rbac" command
    // text. Step title and the command-block label both contain
    // "Create the Service Principal", so we anchor on the command
    // body text rather than the heading copy.
    await waitFor(() => {
      expect(
        screen.getByText(/az ad sp create-for-rbac/i),
      ).toBeInTheDocument();
    });
  }

  // advanceToCredentialsStep walks from step 1 to step 3.
  async function advanceToCredentialsStep() {
    await advanceToServicePrincipalStep();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(screen.getByLabelText(/Client ID/i)).toBeInTheDocument();
    });
  }

  // advanceToValidateStep walks from step 1 to step 4.
  // userEvent is used for the checkbox click because Radix Checkbox
  // branches on pointer events that fireEvent.click doesn't fire.
  async function advanceToValidateStep(
    user: ReturnType<typeof userEvent.setup>,
  ) {
    await advanceToCredentialsStep();
    fireEvent.change(screen.getByLabelText(/Client ID/i), {
      target: { value: CLIENT_ID },
    });
    fireEvent.change(screen.getByLabelText(/Client Secret/i), {
      target: { value: "supersecret-from-az-cli" },
    });
    await waitFor(() => {
      const ack = screen.getByRole("checkbox", {
        name: /I have stored this secret securely/i,
      });
      expect(ack).not.toBeDisabled();
    });
    await user.click(
      screen.getByRole("checkbox", {
        name: /I have stored this secret securely/i,
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
    const twoHoursAgo = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanAzureConnection.mockResolvedValue({
      ...sampleScan,
      compute: [
        { ...sampleScan.compute[0], last_seen_at: twoHoursAgo },
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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getByText(/2h ago/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Last seen/i)).toBeInTheDocument();
  });

  it("TestInventoryTab_ComputeSubTab_LastSeenColumn_NeverValue", async () => {
    const user = userEvent.setup();
    mockedListAzureConnections.mockResolvedValue([sampleConnection]);
    mockedCreateAzureConnection.mockResolvedValue(sampleConnection);
    mockedValidateAzureConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanAzureConnection.mockResolvedValue({
      ...sampleScan,
      compute: [
        { ...sampleScan.compute[0], last_seen_at: undefined },
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
        screen.getByText(/Connected — 5 virtual machines visible/i),
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
      expect(screen.getAllByTestId("last-seen-never").length).toBeGreaterThan(0);
    });
  });
});

// within is imported above for potential per-row scoping; silence the
// unused warning by referencing it once. (Some tests may scope by
// table row in a follow-up; keeping the import keeps the diff narrow
// when that lands.)
void within;
