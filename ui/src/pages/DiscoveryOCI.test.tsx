// Vitest coverage for the v0.89.58 #684 Stream 82 (slice-1 chunk 4)
// DiscoveryOCI page. Mirrors the DiscoveryAzure.test.tsx posture:
//   - SWR cache is wiped per-test via a fresh SWRConfig provider.
//   - Network mocked at the @/api/discoveryOCI module boundary.
//   - jsdom polyfills for Radix Select / Tabs pointer-capture.
//
// Test naming follows the chunk-4 brief's TestDiscoveryOCI_<area>_<behavior>
// convention so a future arc-spanning audit can grep for the OCI suite
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

import DiscoveryOCIPage from "./DiscoveryOCI";

import {
  createOCIConnection,
  enableOCIDemoConnection,
  listOCIConnections,
  scanOCIConnection,
  validateOCIConnection,
  type OCIConnection,
  type ScanOCIResponse,
} from "@/api/discoveryOCI";

// jsdom polyfills for Radix Select / Tabs pointer-capture lookups.
// Same posture as the Azure test file — without these the components
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

vi.mock("@/api/discoveryOCI", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/discoveryOCI")>(
      "@/api/discoveryOCI",
    );
  return {
    ...actual,
    listOCIConnections: vi.fn(),
    enableOCIDemoConnection: vi.fn(),
    createOCIConnection: vi.fn(),
    validateOCIConnection: vi.fn(),
    scanOCIConnection: vi.fn(),
  };
});

const mockedListOCIConnections = vi.mocked(listOCIConnections);
const mockedEnableOCIDemo = vi.mocked(enableOCIDemoConnection);
const mockedCreateOCIConnection = vi.mocked(createOCIConnection);
const mockedValidateOCIConnection = vi.mocked(validateOCIConnection);
const mockedScanOCIConnection = vi.mocked(scanOCIConnection);

// renderPage wraps the page in a fresh SWRConfig so each test starts
// with an empty cache. Mirrors the Azure page's renderPage helper.
function renderPage(initialEntries: string[] = ["/discovery/oci"]) {
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
  return render(<DiscoveryOCIPage />, { wrapper: Wrapper });
}

// Canonical OCID + fingerprint values used throughout the tests.
// Real OCI shapes — the regex validators only care about the shape,
// not the content, so any valid OCID works.
const TENANCY_OCID =
  "ocid1.tenancy.oc1..aaaaaaaaexampletenancyocid1234567890abcdef";
const USER_OCID = "ocid1.user.oc1..aaaaaaaaexampleuserocid1234567890abcdef1234";
const REGION = "us-phoenix-1";
const FINGERPRINT = "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99";
const PRIVATE_KEY = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBexample
-----END PRIVATE KEY-----`;

const sampleConnection: OCIConnection = {
  id: "conn-uuid-1",
  display_name: "Production OCI",
  tenancy_ocid: TENANCY_OCID,
  user_ocid: USER_OCID,
  fingerprint: FINGERPRINT,
  region: REGION,
  learn_from_accepted_recommendations: true,
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

const sampleScan: ScanOCIResponse = {
  connection_id: "conn-uuid-1",
  tenancy_ocid: TENANCY_OCID,
  region: REGION,
  scan_id: "scan-uuid-1",
  instance_count: 5,
  computes: [
    {
      resource_id: "web-1",
      instance_type: "VM.Standard.E4.Flex",
      tags: { otel: "true" },
      has_otel: true,
      os_family: "unknown",
      region: REGION,
    },
    {
      resource_id: "web-2",
      instance_type: "VM.Standard.E4.Flex",
      tags: { env: "prod" },
      has_otel: false,
      os_family: "unknown",
      region: REGION,
    },
    {
      resource_id: "web-3",
      instance_type: "VM.Standard.A1.Flex",
      tags: {},
      has_otel: false,
      os_family: "unknown",
      region: REGION,
    },
    {
      resource_id: "web-4",
      instance_type: "VM.Standard.E4.Flex",
      tags: {},
      has_otel: false,
      os_family: "unknown",
      region: REGION,
    },
    {
      resource_id: "web-5",
      instance_type: "VM.Standard.E4.Flex",
      tags: {},
      has_otel: false,
      os_family: "unknown",
      region: REGION,
    },
  ],
  instrumented_count: 1,
  uninstrumented_count: 4,
  partial: false,
};

describe("DiscoveryOCI", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockedListOCIConnections.mockResolvedValue([]);
  });

  it("Try the demo provisions the demo tenancy and refreshes the list", async () => {
    mockedListOCIConnections.mockResolvedValue([]);
    mockedEnableOCIDemo.mockResolvedValue({
      id: "demo-oci",
      display_name: "Demo Tenancy (sample data)",
      tenancy_ocid: "ocid1.tenancy.oc1..demo",
      user_ocid: "ocid1.user.oc1..demo",
      fingerprint: "00:00",
      region: "us-ashburn-1",
      learn_from_accepted_recommendations: true,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
    } as OCIConnection);
    const user = userEvent.setup();
    renderPage();
    const demoBtn = await screen.findByRole("button", {
      name: /Try the demo/i,
    });
    await user.click(demoBtn);
    await waitFor(() => expect(mockedEnableOCIDemo).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(mockedListOCIConnections.mock.calls.length).toBeGreaterThan(1),
    );
  });

  it("TestDiscoveryOCI_WizardState_SurvivesTabSwitch", async () => {
    // Regression (v0.89.257): the wizard's TabsContent unmounted when the
    // operator switched to Inventory/Recommendations, wiping the in-progress
    // form. forceMount keeps it mounted (hidden) so state survives.
    const user = userEvent.setup();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Persisted Tenancy" },
    });
    // Switch away to Inventory, then back to the Wizard tab.
    await user.click(screen.getByRole("tab", { name: /Inventory/i }));
    await user.click(screen.getByRole("tab", { name: /Wizard/i }));
    // The in-progress value must still be there (would be "" pre-fix).
    expect(screen.getByLabelText(/Display name/i)).toHaveValue(
      "Persisted Tenancy",
    );
  });

  it("TestDiscoveryOCI_WizardStep1_TenancyOCIDValidation", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // Fill display name + user OCID so Next-enablement isn't gated
    // on the unrelated fields. Region remains empty so Next stays
    // disabled regardless of tenancy validity — but the inline error
    // for tenancy still surfaces, which is what this test asserts.
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production OCI" },
    });
    fireEvent.change(screen.getByLabelText(/User OCID/i), {
      target: { value: USER_OCID },
    });

    const tenancyInput = screen.getByLabelText(/Tenancy OCID/i);

    // Invalid tenancy OCID (non-OCID format) — Next stays disabled
    // and the inline error message renders.
    fireEvent.change(tenancyInput, { target: { value: "not-an-ocid" } });
    expect(
      screen.getByText(/Tenancy OCIDs must start with/i),
    ).toBeInTheDocument();
    const nextBtn = screen.getByRole("button", { name: /^Next$/i });
    expect(nextBtn).toBeDisabled();

    // Valid tenancy OCID — inline error clears.
    fireEvent.change(tenancyInput, { target: { value: TENANCY_OCID } });
    await waitFor(() => {
      expect(
        screen.queryByText(/Tenancy OCIDs must start with/i),
      ).not.toBeInTheDocument();
    });
    // Next stays disabled because region is still empty (verified by
    // the dedicated RegionRequired test); the per-field test only
    // proves the format check itself.
  });

  it("TestDiscoveryOCI_WizardStep1_UserOCIDValidation", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production OCI" },
    });
    fireEvent.change(screen.getByLabelText(/Tenancy OCID/i), {
      target: { value: TENANCY_OCID },
    });

    const userInput = screen.getByLabelText(/User OCID/i);

    // Invalid user OCID — Next stays disabled, inline error renders.
    fireEvent.change(userInput, { target: { value: "not-an-ocid" } });
    expect(screen.getByText(/User OCIDs must start with/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();

    // Valid user OCID — inline error clears.
    fireEvent.change(userInput, { target: { value: USER_OCID } });
    await waitFor(() => {
      expect(
        screen.queryByText(/User OCIDs must start with/i),
      ).not.toBeInTheDocument();
    });
  });

  it("TestDiscoveryOCI_WizardStep1_RegionRequired", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // All other fields valid, region intentionally empty — Next
    // stays disabled. Per design doc §5, OCI requires region always.
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production OCI" },
    });
    fireEvent.change(screen.getByLabelText(/Tenancy OCID/i), {
      target: { value: TENANCY_OCID },
    });
    fireEvent.change(screen.getByLabelText(/User OCID/i), {
      target: { value: USER_OCID },
    });

    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryOCI_WizardStep4_FingerprintValidation", async () => {
    const user = userEvent.setup();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToCredentialsStep(user);

    const fingerprintInput = screen.getByLabelText(/Fingerprint/i);

    // Malformed fingerprint — inline error renders, ack stays
    // disabled, Next stays disabled.
    fireEvent.change(fingerprintInput, {
      target: { value: "not-a-fingerprint" },
    });
    expect(
      screen.getByText(/Fingerprints must be 16 colon-separated hex pairs/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();

    // Valid fingerprint — inline error clears.
    fireEvent.change(fingerprintInput, { target: { value: FINGERPRINT } });
    await waitFor(() => {
      expect(
        screen.queryByText(
          /Fingerprints must be 16 colon-separated hex pairs/i,
        ),
      ).not.toBeInTheDocument();
    });
  });

  it("TestDiscoveryOCI_WizardStep4_RequiresAllFields", async () => {
    const user = userEvent.setup();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToCredentialsStep(user);

    // Acknowledgment checkbox is disabled until fingerprint +
    // private key both validate.
    const ack = screen.getByRole("checkbox", {
      name: /I have stored this private key securely/i,
    });
    expect(ack).toBeDisabled();

    // Fingerprint alone — ack still disabled because private key
    // empty.
    fireEvent.change(screen.getByLabelText(/Fingerprint/i), {
      target: { value: FINGERPRINT },
    });
    expect(ack).toBeDisabled();
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();

    // Add private key — ack enables but unchecked, Next stays
    // disabled.
    fireEvent.change(screen.getByLabelText(/Private key \(PEM\)/i), {
      target: { value: PRIVATE_KEY },
    });
    await waitFor(() => {
      expect(
        screen.getByRole("checkbox", {
          name: /I have stored this private key securely/i,
        }),
      ).not.toBeDisabled();
    });
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();
  });

  it("TestDiscoveryOCI_WizardStep5_ValidateSuccess_AdvancesToScan", async () => {
    const user = userEvent.setup();
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);

    const validateBtn = screen.getByRole("button", {
      name: /Validate connection/i,
    });
    await user.click(validateBtn);

    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });

    // The scan button enables after a successful validate.
    const scanBtn = screen.getByRole("button", { name: /Run scan/i });
    expect(scanBtn).toBeEnabled();

    expect(mockedCreateOCIConnection).toHaveBeenCalledTimes(1);
    expect(mockedValidateOCIConnection).toHaveBeenCalledWith(
      sampleConnection.id,
    );
  });

  it("TestDiscoveryOCI_WizardStep5_EditCredentialAfterSuccess_Recreates", async () => {
    // Regression guard for the credentials_invalid dead end found
    // during the OCI live-validate: editing any credential field must
    // re-create (re-seal) the connection on the next validate, even
    // when the prior validate SUCCEEDED. The old guard only re-created
    // when validateResult?.ok === false, so an edit after success (or
    // after the result was cleared by step navigation) silently
    // re-tested the stale connection — trapping the operator forever.
    const user = userEvent.setup();
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    expect(mockedCreateOCIConnection).toHaveBeenCalledTimes(1);

    // Go back to the credentials step and change the fingerprint.
    fireEvent.click(screen.getByRole("button", { name: /^Back$/i }));
    await waitFor(() => {
      expect(screen.getByLabelText(/Fingerprint/i)).toBeInTheDocument();
    });
    const editedFingerprint = "11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00";
    fireEvent.change(screen.getByLabelText(/Fingerprint/i), {
      target: { value: editedFingerprint },
    });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Validate connection/i }),
      ).toBeInTheDocument();
    });

    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );

    await waitFor(() => {
      expect(mockedCreateOCIConnection).toHaveBeenCalledTimes(2);
    });
    expect(mockedCreateOCIConnection).toHaveBeenLastCalledWith(
      expect.objectContaining({ fingerprint: editedFingerprint }),
    );
  });

  it("TestDiscoveryOCI_WizardStep5_PermissionDenied_ShowsRemediation", async () => {
    const user = userEvent.setup();
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: false,
      error_kind: "permission_denied",
      message:
        "NotAuthorizedOrNotFound: user lacks compute.instances:read on tenancy.",
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );

    await waitFor(() => {
      expect(
        screen.getByText(
          /Verify the user has compute.instances:read permission/i,
        ),
      ).toBeInTheDocument();
    });
    // Server's message renders too.
    expect(
      screen.getByText(/NotAuthorizedOrNotFound: user lacks compute/i),
    ).toBeInTheDocument();
    // Scan button stays disabled — operator must fix the upstream
    // state and re-validate.
    expect(screen.getByRole("button", { name: /Run scan/i })).toBeDisabled();
  });

  it("TestDiscoveryOCI_WizardStep5_FingerprintMismatch_ShowsRemediation", async () => {
    const user = userEvent.setup();
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: false,
      error_kind: "fingerprint_mismatch",
      message: "InvalidSignature: fingerprint does not match uploaded key.",
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );

    await waitFor(() => {
      expect(
        screen.getByText(
          /The fingerprint doesn't match the public key uploaded/i,
        ),
      ).toBeInTheDocument();
    });
    expect(
      screen.getByText(/InvalidSignature: fingerprint does not match/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Run scan/i })).toBeDisabled();
  });

  it("TestDiscoveryOCI_WizardStep5_PrivateKeyInvalid_ShowsRemediation", async () => {
    const user = userEvent.setup();
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: false,
      error_kind: "private_key_invalid",
      message: "Squadron could not decrypt the stored API Signing Key.",
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );

    await waitFor(() => {
      expect(
        screen.getByText(/The pasted PEM is malformed or not an RSA key/i),
      ).toBeInTheDocument();
    });
    expect(
      screen.getByText(
        /Squadron could not decrypt the stored API Signing Key/i,
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Run scan/i })).toBeDisabled();
  });

  it("TestDiscoveryOCI_ScanSuccess_TransitionsToInventory", async () => {
    const user = userEvent.setup();
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue(sampleScan);
    // After the wizard succeeds the page re-lists connections; return
    // the new row so the selector shows it.
    mockedListOCIConnections.mockResolvedValueOnce([]);
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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
    expect(mockedScanOCIConnection).toHaveBeenCalledWith(sampleConnection.id);
  });

  it("TestDiscoveryOCI_InventoryTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    // Walk the wizard so the scan result lands on the page state.
    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    // Inventory tab should have the 5-row table + summary.
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Instrumented: 1/)).toBeInTheDocument();
    expect(screen.getByText(/Uninstrumented: 4/)).toBeInTheDocument();

    // Each compute row's resource_id is present.
    for (const row of sampleScan.computes) {
      expect(screen.getByText(row.resource_id)).toBeInTheDocument();
    }
  });

  it("TestDiscoveryOCI_RecommendationsTab_EmptyStateWithoutScan", async () => {
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
  // Inventory tab gains a Databases sub-tab rendering the OCI DB
  // Systems + Autonomous Database inventory from the chunk 4
  // scanner extension. The instrumentation column reads from
  // database_management_enabled (Operations Insights enrollment).
  it("TestDiscoveryOCI_InventoryTab_DatabasesSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      databases: [
        {
          resource_id: "ocid1.dbsystem.oc1.phx.covered",
          engine: "oracle",
          engine_version: "19c",
          instance_class: "VM.Standard.E4.Flex",
          region: "us-phoenix-1",
          provider: "oci",
          database_management_enabled: true,
          tags: { env: "prod" },
        },
        {
          resource_id: "ocid1.autonomousdatabase.oc1.phx.uncovered",
          engine: "oracle",
          engine_version: "19c",
          instance_class: "OCPU=2",
          region: "us-phoenix-1",
          provider: "oci",
          database_management_enabled: false,
          tags: {},
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const databasesTab = screen.getByRole("tab", { name: /^Databases$/i });
    await user.click(databasesTab);
    expect(databasesTab).toHaveAttribute("data-state", "active");
    expect(
      screen.getByText("ocid1.dbsystem.oc1.phx.covered"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("ocid1.autonomousdatabase.oc1.phx.uncovered"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Database Management enabled\?/i),
    ).toBeInTheDocument();
  });

  // Database tier slice 2 chunk 5 — empty-state UX for OCI.
  it("TestDiscoveryOCI_InventoryTab_DatabasesSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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
  // Inventory tab gains a Kubernetes sub-tab rendering the OKE
  // cluster inventory from the v0.89.70 chunk 4 scanner extension.
  // The instrumentation column reads from
  // operations_insights_enabled (Operations Insights enrollment).
  it("TestDiscoveryOCI_InventoryTab_KubernetesSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      clusters: [
        {
          resource_id: "ocid1.cluster.oc1.phx.covered",
          name: "oke-covered",
          kubernetes_version: "1.29",
          status: "ACTIVE",
          region: "us-phoenix-1",
          provider: "oci",
          operations_insights_enabled: true,
          tags: { env: "prod" },
        },
        {
          resource_id: "ocid1.cluster.oc1.phx.uncovered",
          name: "oke-uncovered",
          kubernetes_version: "1.29",
          status: "ACTIVE",
          region: "us-phoenix-1",
          provider: "oci",
          operations_insights_enabled: false,
          tags: {},
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const kubernetesTab = screen.getByRole("tab", { name: /^Kubernetes$/i });
    await user.click(kubernetesTab);
    expect(kubernetesTab).toHaveAttribute("data-state", "active");
    expect(
      screen.getByText("ocid1.cluster.oc1.phx.covered"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("ocid1.cluster.oc1.phx.uncovered"),
    ).toBeInTheDocument();
    expect(screen.getByText(/Operations Insights\?/i)).toBeInTheDocument();
  });

  // Kubernetes tier slice 2 chunk 5 — empty-state UX for OCI.
  it("TestDiscoveryOCI_InventoryTab_KubernetesSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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
  // Inventory tab gains a Serverless sub-tab rendering the OCI
  // Functions inventory from the chunk 4 scanner extension.
  it("TestDiscoveryOCI_InventoryTab_ServerlessSubTab_RendersTable", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "oci",
          surface: "ocifunc",
          account_id: "ocid1.tenancy.oc1..aaaaaaaa",
          region: "us-phoenix-1",
          resource_name: "img-resize",
          resource_arn: "ocid1.fnfunc.oc1.phx.aaaaaaaa",
          runtime: "node20",
          has_trace_axis: true,
          has_otel_distro: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const serverlessTab = screen.getByRole("tab", { name: /^Serverless$/i });
    await user.click(serverlessTab);
    expect(serverlessTab).toHaveAttribute("data-state", "active");
    expect(screen.getByText("img-resize")).toBeInTheDocument();
    expect(screen.getByText("ocifunc")).toBeInTheDocument();
  });

  it("TestDiscoveryOCI_InventoryTab_ServerlessSubTab_EmptyState", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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

  // TestDiscoveryOCI_Serverless_ColdStartColumnRenders — slice 2 §11
  // acceptance test 13 extension. The OCI Serverless table's
  // "Cold-start P95 (24h)" column renders between OTel distro and
  // Last seen. Mirrors the AWS column from slice 1 chunk 3.
  it("TestDiscoveryOCI_Serverless_ColdStartColumnRenders", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "oci",
          surface: "ocifunc",
          account_id: "ocid1.tenancy.oc1..aaaa",
          region: "us-ashburn-1",
          resource_name: "ingest-worker",
          resource_arn: "ocid1.fnfunc.oc1.iad.aaaa",
          runtime: "java17",
          has_trace_axis: true,
          has_otel_distro: true,
          cold_start_p95_ms: 2100,
          cold_start_exceeds_threshold: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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

  // TestDiscoveryOCI_Serverless_ColdStartCell_AmberWhenExceedsThreshold —
  // when the server's cold_start_exceeds_threshold flag is true, the
  // cell renders in amber. Mirrors the AWS slice 1 test.
  it("TestDiscoveryOCI_Serverless_ColdStartCell_AmberWhenExceedsThreshold", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "oci",
          surface: "ocifunc",
          account_id: "ocid1.tenancy.oc1..aaaa",
          region: "us-ashburn-1",
          resource_name: "hot-ocifunc",
          resource_arn: "ocid1.fnfunc.oc1.iad.bbbb",
          runtime: "java17",
          has_trace_axis: true,
          has_otel_distro: false,
          cold_start_p95_ms: 2100,
          cold_start_exceeds_threshold: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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
    expect(cell.textContent).toMatch(/2100ms/);
  });

  // --- Sampling rate slice 1 chunk 3 (v0.89.124, #764 Stream 162) ---

  it("TestDiscoveryOCI_Serverless_SamplingRateColumnRenders", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "oci",
          surface: "ocifunc",
          account_id: "ocid1.tenancy.oc1..aaaa",
          region: "us-ashburn-1",
          resource_name: "sampling-fn",
          resource_arn: "ocid1.fnfunc.oc1.iad.bbbb",
          runtime: "java17",
          has_trace_axis: true,
          has_otel_distro: false,
          sampling_ratio: 0.041,
          sampling_exceeds_floor: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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

  it("TestDiscoveryOCI_Serverless_SamplingRateCell_AmberWhenBelowFloor", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "oci",
          surface: "ocifunc",
          account_id: "ocid1.tenancy.oc1..aaaa",
          region: "us-ashburn-1",
          resource_name: "below-floor-fn",
          resource_arn: "ocid1.fnfunc.oc1.iad.cccc",
          runtime: "java17",
          has_trace_axis: true,
          has_otel_distro: false,
          sampling_ratio: 0.02,
          sampling_exceeds_floor: true,
        },
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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
    expect(cell.textContent).toMatch(/2.0%/);
  });

  // --- Error rate slice 1 chunk 3 (v0.89.129, #769 Stream 167) ---

  it("TestDiscoveryOCI_Serverless_ErrorRateColumnRenders", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "oci",
          surface: "ocifunc",
          account_id: "ocid1.tenancy.oc1..aaaa",
          region: "us-ashburn-1",
          resource_name: "er-render-fn",
          resource_arn: "ocid1.fnfunc.oc1.iad.render",
          runtime: "java17",
          has_trace_axis: true,
          has_otel_distro: false,
        },
      ],
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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

  it("TestDiscoveryOCI_Serverless_ErrorRateCell_AmberWhenExceedsThreshold", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      serverless: [
        {
          provider: "oci",
          surface: "ocifunc",
          account_id: "ocid1.tenancy.oc1..aaaa",
          region: "us-ashburn-1",
          resource_name: "er-amber-fn",
          resource_arn: "ocid1.fnfunc.oc1.iad.amber",
          runtime: "java17",
          has_trace_axis: true,
          has_otel_distro: false,
          current_error_rate: 0.0187,
          error_rate_exceeds_threshold: true,
        },
      ],
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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
    expect(cell.textContent).toMatch(/1.87%/);
  });

  // Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) —
  // slice 1 contract: the Orchestration sub-tab MUST be hidden when
  // the orchestrations[] array is empty. OCI orchestration coverage
  // is deferred to slice 2; until then the sub-tab is invisible on
  // the OCI page even though the type substrate carries the row
  // shape. The cold-start scan response (orchestrations omitted /
  // empty) drives this assertion.
  it("TestDiscoveryOCI_OrchestrationSubTab_HiddenWhenEmpty", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue(sampleScan);

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });

    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    // The Orchestration sub-tab must not be in the DOM at all when
    // orchestrations is empty. Other sub-tabs (Compute / Databases /
    // Kubernetes / Serverless) remain visible.
    expect(
      screen.queryByRole("tab", { name: /^Orchestration$/i }),
    ).not.toBeInTheDocument();
  });

  // Forward-compatible slice 2 test: when the OCI substrate one day
  // populates orchestrations[], the sub-tab SHOULD appear and render
  // the rows. This test pins the conditional render branch so the
  // slice 2 path doesn't accidentally regress.
  it("TestDiscoveryOCI_OrchestrationSubTab_ShownWhenPopulated", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      // Slice 2 hypothetical row — the OrchestrationRow shape is the
      // shared cross-cloud one but with provider restricted to
      // aws|gcp|azure. The forward-compat test cheats by widening the
      // provider field via `as any` so the assertion can pin the
      // visible sub-tab without committing to a future OCI surface
      // name.
      orchestrations: [
        {
          provider: "aws",
          surface: "stepfunc",
          account_id: "ocid1.tenancy.oc1..aaaaaaaa",
          region: "us-phoenix-1",
          resource_name: "future-oci-workflow",
          workflow_type: "STANDARD",
          has_trace_axis: true,
          has_log_axis: true,
        } as never,
      ],
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));
    await waitFor(() => {
      expect(screen.getByText(/Instances: 5/)).toBeInTheDocument();
    });

    const orchestrationTab = screen.getByRole("tab", {
      name: /^Orchestration$/i,
    });
    await user.click(orchestrationTab);
    expect(orchestrationTab).toHaveAttribute("data-state", "active");
    expect(screen.getByText("future-oci-workflow")).toBeInTheDocument();
  });

  // --- helpers ---

  // selectRegion picks the canonical test region from the Radix
  // Select on step 1. The Region trigger renders as a button with
  // accessible name "OCI region" (from the SelectTrigger's
  // aria-label); Radix opens the listbox on click; the option text
  // includes the region id + label so we filter on the id substring.
  async function selectRegion(user: ReturnType<typeof userEvent.setup>) {
    // The region picker is a searchable cmdk combobox: a button trigger that
    // opens a filterable list of role="option" items.
    const regionTrigger = screen.getByRole("button", { name: /OCI region/i });
    await user.click(regionTrigger);
    const option = await screen.findByRole("option", {
      name: new RegExp(REGION, "i"),
    });
    await user.click(option);
  }

  // advanceToGenerateKeyStep walks the wizard from step 1 (tenancy)
  // to step 2 (generate key instructions).
  async function advanceToGenerateKeyStep(
    user: ReturnType<typeof userEvent.setup>,
  ) {
    fireEvent.change(screen.getByLabelText(/Display name/i), {
      target: { value: "Production OCI" },
    });
    fireEvent.change(screen.getByLabelText(/Tenancy OCID/i), {
      target: { value: TENANCY_OCID },
    });
    fireEvent.change(screen.getByLabelText(/User OCID/i), {
      target: { value: USER_OCID },
    });
    await selectRegion(user);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    // Step 2's body has the unique "oci setup keys" command text.
    await waitFor(() => {
      expect(screen.getByText(/oci setup keys/i)).toBeInTheDocument();
    });
  }

  // advanceToUploadKeyStep walks the wizard from step 1 to step 3.
  async function advanceToUploadKeyStep(
    user: ReturnType<typeof userEvent.setup>,
  ) {
    await advanceToGenerateKeyStep(user);
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/OCI Console → Identity → Users/i),
      ).toBeInTheDocument();
    });
  }

  // advanceToCredentialsStep walks from step 1 to step 4.
  async function advanceToCredentialsStep(
    user: ReturnType<typeof userEvent.setup>,
  ) {
    await advanceToUploadKeyStep(user);
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    await waitFor(() => {
      expect(screen.getByLabelText(/Fingerprint/i)).toBeInTheDocument();
    });
  }

  // advanceToValidateScanStep walks from step 1 to step 5.
  // userEvent is used for the checkbox click because Radix Checkbox
  // branches on pointer events that fireEvent.click doesn't fire.
  async function advanceToValidateScanStep(
    user: ReturnType<typeof userEvent.setup>,
  ) {
    await advanceToCredentialsStep(user);
    fireEvent.change(screen.getByLabelText(/Fingerprint/i), {
      target: { value: FINGERPRINT },
    });
    fireEvent.change(screen.getByLabelText(/Private key \(PEM\)/i), {
      target: { value: PRIVATE_KEY },
    });
    await waitFor(() => {
      const ack = screen.getByRole("checkbox", {
        name: /I have stored this private key securely/i,
      });
      expect(ack).not.toBeDisabled();
    });
    await user.click(
      screen.getByRole("checkbox", {
        name: /I have stored this private key securely/i,
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
    const threeDaysAgo = new Date(
      Date.now() - 3 * 24 * 60 * 60 * 1000,
    ).toISOString();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      computes: [{ ...sampleScan.computes[0], last_seen_at: threeDaysAgo }],
      instrumented_count: 1,
      uninstrumented_count: 0,
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
      ).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: /Run scan/i }));

    await waitFor(() => {
      expect(screen.getByText(/3d ago/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Last seen/i)).toBeInTheDocument();
  });

  it("TestInventoryTab_ComputeSubTab_LastSeenColumn_NeverValue", async () => {
    const user = userEvent.setup();
    mockedListOCIConnections.mockResolvedValue([sampleConnection]);
    mockedCreateOCIConnection.mockResolvedValue(sampleConnection);
    mockedValidateOCIConnection.mockResolvedValue({
      ok: true,
      instance_count: 5,
    });
    mockedScanOCIConnection.mockResolvedValue({
      ...sampleScan,
      computes: [{ ...sampleScan.computes[0], last_seen_at: undefined }],
      instrumented_count: 1,
      uninstrumented_count: 0,
    });

    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: /Wizard/i })).toBeInTheDocument();
    });
    await advanceToValidateScanStep(user);
    await user.click(
      screen.getByRole("button", { name: /Validate connection/i }),
    );
    await waitFor(() => {
      expect(
        screen.getByText(/Connected — 5 compute instances visible/i),
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
