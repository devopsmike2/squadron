// DiscoveryOCI — the v0.89.58 #684 Stream 82 (slice-1 chunk 4) OCI
// discovery page. Parallels DiscoveryAzure.tsx in tab structure
// (Wizard / Inventory / Recommendations) and operator UX, but the
// step bodies differ because the OCI credential model is "tenancy
// OCID + user OCID + API Signing Key (RSA keypair) + fingerprint +
// region" rather than Azure's "tenant + subscription + Service
// Principal client_id + client_secret paste" — see
// docs/proposals/oci-discovery-slice1.md §8 for the verbatim 5-step
// list this file implements.
//
// Slice-1 honesty (chunk 4):
//   - The Wizard tab is the primary surface. The 5-step state
//     machine lives in this file (no factoring into a shared shell
//     yet — the Azure page deferred the shared-shell extraction as a
//     slice-2 candidate so a single chunk doesn't carry that
//     refactor on top of the new page).
//   - The Inventory tab renders the last successful scan response
//     for the selected connection. Scans are NOT persisted (matches
//     AWS + GCP + Azure slice-1 posture); a refresh clears the
//     panel.
//   - The Recommendations tab is a stub. The proposer extension
//     (Provider="oci" path, compute-otel-tag kind, prompt extension)
//     ships in chunk 5 of this arc — until then the tab surfaces a
//     "ships in chunk 5" message so an operator who clicks through
//     during the chunk-4 → chunk-5 gap isn't stranded on an empty
//     panel.
//
// Token discipline: the pasted API Signing Key private key (PEM)
// lives in component state ONLY. It is base64-encoded into the
// createOCIConnection request body and then dropped from state on
// success. There is no reveal-toggle, no localStorage, no
// sessionStorage; an operator who refreshes the page loses the
// in-progress paste and has to re-paste from the openssl / oci CLI
// output. Same posture as the Azure SP client_secret in
// DiscoveryAzure.tsx and the GCP SA JSON in DiscoveryGCP.tsx — and
// the RSA private key is the strongest credential type Squadron
// handles, so this posture is non-negotiable per design doc §12.

import { Command } from "cmdk";
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  Cloud,
  Copy,
  ExternalLink,
  Loader2,
  Sparkles,
} from "lucide-react";
import { useCallback, useRef, useState } from "react";
import useSWR from "swr";

import { RecommendationsTab as AWSRecommendationsTab } from "./DiscoveryAWS";

import type { GenerateRecommendationsResponse } from "@/api/discovery";
import {
  createOCIConnection,
  encodePrivateKeyForWire,
  generateOCIRecommendations,
  generateOCITerraformImport,
  enableOCIDemoConnection,
  listOCIConnections,
  scanOCIConnection,
  validateOCIConnection,
  type ClusterSnapshot,
  type ComputeInstanceSnapshot,
  type DatabaseInstanceSnapshot,
  type OCIConnection,
  type OCIValidateErrorKind,
  type ScanOCIResponse,
  type ServerlessRow,
  type OrchestrationRow,
  type EventSourceRow,
  type ValidateOCIResponse,
} from "@/api/discoveryOCI";
import { TerraformAdoptCard } from "@/components/discovery/TerraformAdoptCard";
import { WizardShell } from "@/components/discovery/WizardShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import {
  OCI_API_KEYS_DOC_LINK,
  OCI_DOC_LINK,
  OCI_FINGERPRINT_REGEX,
  OCI_IAM_DOC_LINK,
  OCI_OPENSSL_GENERATE_CMD,
  OCI_REGIONS,
  OCI_SETUP_KEYS_CMD,
  OCI_STEP_CREDENTIALS,
  OCI_STEP_GENERATE_KEY,
  OCI_STEP_IDS,
  OCI_STEP_TENANCY,
  OCI_STEP_TITLES,
  OCI_STEP_UPLOAD_KEY,
  OCI_STEP_VALIDATE_SCAN,
  OCI_TENANCY_OCID_REGEX,
  OCI_USER_OCID_REGEX,
  validateErrorRemediation,
} from "@/data/ociDiscoveryWizard";
import { relativeTime } from "@/lib/relativeTime";

// Tab values — stable string literals double as both the Radix Tabs
// `value` and the test selector key. Mirrors the Azure page's
// constants one-for-one.
const WIZARD_TAB = "wizard";
const INVENTORY_TAB = "inventory";
const RECS_TAB = "recommendations";

// Inventory sub-tab values — database tier slice 2 (v0.89.66, #695
// Stream 93) splits Inventory into Compute + Databases sub-tabs.
// The Databases sub-tab surfaces the OCI DB Systems + Autonomous
// Database inventory from the chunk 4 scanner extension with the
// Database Management enrollment axis rendered per row.
//
// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100) adds a third
// Kubernetes sub-tab surfacing the OKE cluster inventory from the
// v0.89.70 chunk 4 OKE scanner extension with the Operations
// Insights enrollment axis rendered per row.
const INVENTORY_SUBTAB_COMPUTE = "compute";
const INVENTORY_SUBTAB_DATABASES = "databases";
const INVENTORY_SUBTAB_KUBERNETES = "kubernetes";
// Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) —
// fourth Inventory sub-tab carrying OCI Functions rows from the
// chunk 4 scanner extension.
const INVENTORY_SUBTAB_SERVERLESS = "serverless";
// Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) —
// fifth Inventory sub-tab. OCI orchestration coverage is deferred to
// slice 2; the slice 1 contract is "OCI scan responses always return
// orchestrations: []" and the sub-tab MUST be hidden when the rows
// array is empty (design doc §7 + §11 acceptance test 17).
const INVENTORY_SUBTAB_ORCHESTRATION = "orchestration";
// Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136) —
// sixth Inventory sub-tab. Unlike Orchestration (which is hidden when
// empty for OCI), Event sources renders for OCI because OCI Streaming
// ships as a real surface in slice 1 (design doc §3.4 + §7).
const INVENTORY_SUBTAB_EVENT_SOURCES = "event_sources";

// SWR_KEY_CONNECTIONS is the shared cache key the page reads and the
// wizard's onSave mutate() targets.
const SWR_KEY_CONNECTIONS = "/discovery/oci/connections";

export default function DiscoveryOCIPage() {
  // activeTab is controlled at the page level so the wizard's
  // "View Inventory" CTA after a successful scan can hop the
  // operator straight into the Inventory tab. Same controlled-tab
  // pattern as DiscoveryAzure / DiscoveryGCP / DiscoveryAWS.
  const [activeTab, setActiveTab] = useState<string>(WIZARD_TAB);

  // Selected connection drives the Inventory tab + the post-scan
  // result lookup. Empty when no connection is selected — the page
  // defaults to the wizard surface in that case.
  const [selectedConnectionID, setSelectedConnectionID] = useState<string>("");

  // scanResultByConn keeps the most-recent scan response per
  // connection ID in a single mutable map. Slice-1: in-memory only,
  // cleared on page refresh, matching the AWS / GCP / Azure posture.
  // The map keyed by connection ID (not just "the latest scan") so
  // switching the selector between two connections doesn't blow away
  // the other's result.
  const [scanResultByConn, setScanResultByConn] = useState<
    Record<string, ScanOCIResponse>
  >({});

  const { data: connections, mutate: mutateConnections } = useSWR(
    SWR_KEY_CONNECTIONS,
    () => listOCIConnections(),
  );

  const conns = connections ?? [];
  const hasConnections = conns.length > 0;

  const handleConnectionPicked = useCallback((id: string) => {
    setSelectedConnectionID(id);
    setActiveTab(INVENTORY_TAB);
  }, []);

  const handleWizardSuccess = useCallback(
    (conn: OCIConnection, scan: ScanOCIResponse) => {
      // Persist the scan result locally so the Inventory tab picks
      // it up on the tab swap below.
      setScanResultByConn((prev) => ({ ...prev, [conn.id]: scan }));
      setSelectedConnectionID(conn.id);
      // Refresh the SWR cache so the connection selector picks up
      // the new row.
      void mutateConnections();
      // Auto-switch to Inventory so the operator sees the result of
      // the work they just did. Matches the Azure scan-then-show-
      // inventory hop.
      setActiveTab(INVENTORY_TAB);
    },
    [mutateConnections],
  );

  // Demo mode (v0.89.245): provision the credential-free demo tenancy and
  // refresh the list so its row appears and can be scanned.
  const [demoBusy, setDemoBusy] = useState(false);
  const [demoError, setDemoError] = useState<string | null>(null);
  const handleTryDemo = useCallback(async () => {
    setDemoBusy(true);
    setDemoError(null);
    try {
      const conn = await enableOCIDemoConnection();
      await mutateConnections();
      setSelectedConnectionID(conn.id);
    } catch (e) {
      setDemoError(
        e instanceof Error ? e.message : "Could not start the demo.",
      );
    } finally {
      setDemoBusy(false);
    }
  }, [mutateConnections]);

  const selectedScan = scanResultByConn[selectedConnectionID];

  return (
    <div className="space-y-4 p-6">
      <header>
        <div className="flex items-center gap-2">
          <Cloud className="h-5 w-5 text-red-500" />
          <h1 className="text-2xl font-semibold">OCI Discovery</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Connect Oracle Cloud tenancies and discover what&apos;s
          uninstrumented.
        </p>
      </header>

      <ConnectionSelectorBar
        connections={conns}
        selectedID={selectedConnectionID}
        onSelect={handleConnectionPicked}
        onTryDemo={handleTryDemo}
        demoBusy={demoBusy}
        demoError={demoError}
      />

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList>
          <TabsTrigger value={WIZARD_TAB}>Wizard</TabsTrigger>
          <TabsTrigger value={INVENTORY_TAB}>Inventory</TabsTrigger>
          <TabsTrigger value={RECS_TAB}>Recommendations</TabsTrigger>
        </TabsList>

        <TabsContent value={WIZARD_TAB} forceMount className="mt-4">
          <OCIWizard onComplete={handleWizardSuccess} />
        </TabsContent>
        <TabsContent value={INVENTORY_TAB} className="mt-4">
          <InventoryTab
            hasConnections={hasConnections}
            scan={selectedScan}
            onJumpToWizard={() => setActiveTab(WIZARD_TAB)}
          />
        </TabsContent>
        <TabsContent value={RECS_TAB} className="mt-4">
          <RecommendationsTab
            scan={selectedScan}
            connectionID={selectedConnectionID}
          />
        </TabsContent>
      </Tabs>
    </div>
  );
}

// --- Connection selector bar ----------------------------------------

function ConnectionSelectorBar({
  connections,
  selectedID,
  onSelect,
  onTryDemo,
  demoBusy,
  demoError,
}: {
  connections: OCIConnection[];
  selectedID: string;
  onSelect: (id: string) => void;
  onTryDemo: () => void;
  demoBusy: boolean;
  demoError: string | null;
}) {
  if (connections.length === 0) {
    return (
      <div className="flex flex-col gap-3 rounded-md border border-dashed bg-muted/30 p-4 text-sm text-muted-foreground">
        <span>
          No OCI tenancies connected yet. Use the Wizard tab to connect one — or
          explore a sample inventory and recommendations with no cloud account:
        </span>
        <div className="flex items-center gap-3">
          <Button onClick={onTryDemo} disabled={demoBusy} size="sm">
            <Sparkles className="mr-2 h-4 w-4" aria-hidden />
            {demoBusy ? "Starting demo…" : "Try the demo"}
          </Button>
          {demoError && (
            <span className="text-xs text-destructive">{demoError}</span>
          )}
        </div>
      </div>
    );
  }
  return (
    <div className="flex items-center gap-3">
      <Label
        htmlFor="oci-connection-select"
        className="text-xs uppercase tracking-wider text-muted-foreground"
      >
        Tenancy
      </Label>
      <div className="w-72">
        <Select value={selectedID} onValueChange={onSelect}>
          <SelectTrigger
            id="oci-connection-select"
            aria-label="OCI connection selector"
          >
            <SelectValue placeholder="Select a tenancy" />
          </SelectTrigger>
          <SelectContent>
            {connections.map((c) => (
              <SelectItem key={c.id} value={c.id}>
                {c.display_name} — {c.region}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </div>
  );
}

// --- Wizard ---------------------------------------------------------

interface OCIWizardProps {
  // onComplete fires after step 5 (scan) succeeds. The page uses it
  // to seed the Inventory tab and hop the operator over.
  onComplete: (conn: OCIConnection, scan: ScanOCIResponse) => void;
}

function OCIWizard({ onComplete }: OCIWizardProps) {
  const [stepIndex, setStepIndex] = useState(0);

  // Step-1 form state.
  const [displayName, setDisplayName] = useState("");
  const [tenancyOCID, setTenancyOCID] = useState("");
  const [userOCID, setUserOCID] = useState("");
  const [region, setRegion] = useState("");
  const [showWhyExplainer, setShowWhyExplainer] = useState(false);

  // Step-4 form state — pasted fingerprint + private key PEM + ack
  // checkbox. The PEM text lives in state for the duration of the
  // wizard; on a successful step-5 it gets dropped from state.
  // NEVER persisted to localStorage / cookies.
  const [fingerprint, setFingerprint] = useState("");
  const [privateKey, setPrivateKey] = useState("");
  const [keyAcknowledged, setKeyAcknowledged] = useState(false);

  // Step-5 in-flight + result state. Step 5 fuses validate + scan —
  // the operator hits "Validate" first; on success the same step
  // surfaces a "Run scan" button.
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [createdConnection, setCreatedConnection] =
    useState<OCIConnection | null>(null);
  // Signature of the create payload used to seal the current
  // createdConnection. Lets handleValidate re-create when the operator
  // edits any credential field, even if validateResult was cleared.
  const createdConnectionSig = useRef<string | null>(null);
  const [validateResult, setValidateResult] =
    useState<ValidateOCIResponse | null>(null);
  const [scanResult, setScanResult] = useState<ScanOCIResponse | null>(null);

  const stepCount = OCI_STEP_IDS.length;
  const currentStepID = OCI_STEP_IDS[stepIndex];

  // Step-1 field validation.
  const displayNameValid = displayName.trim() !== "";
  const tenancyOCIDValid =
    tenancyOCID !== "" && OCI_TENANCY_OCID_REGEX.test(tenancyOCID.trim());
  const userOCIDValid =
    userOCID !== "" && OCI_USER_OCID_REGEX.test(userOCID.trim());
  const regionValid = region.trim() !== "";

  // Step-4 field validation.
  const fingerprintValid =
    fingerprint !== "" &&
    OCI_FINGERPRINT_REGEX.test(fingerprint.trim().toLowerCase());
  // Best-effort PEM shape check — the operator pasted something
  // that contains the BEGIN PRIVATE KEY marker. The server does the
  // real RSA parse; this is a "did the operator paste anything that
  // looks like a key" guard so the ack checkbox doesn't enable on a
  // blank textarea.
  const privateKeyValid =
    privateKey.trim() !== "" &&
    /-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(privateKey) &&
    /-----END [A-Z ]*PRIVATE KEY-----/.test(privateKey);

  // Next-enablement matrix per step. Mirrors the Azure / GCP /
  // IaCGitHubWizard pattern: a switch on currentStepID populates a
  // single boolean the global Next button reads.
  let nextEnabled = false;
  switch (currentStepID) {
    case OCI_STEP_TENANCY:
      nextEnabled =
        displayNameValid && tenancyOCIDValid && userOCIDValid && regionValid;
      break;
    case OCI_STEP_GENERATE_KEY:
      // The key-generate step is read-only instructions — Next is
      // always advanceable.
      nextEnabled = true;
      break;
    case OCI_STEP_UPLOAD_KEY:
      // The upload-key step is read-only instructions — Next is
      // always advanceable.
      nextEnabled = true;
      break;
    case OCI_STEP_CREDENTIALS:
      nextEnabled = fingerprintValid && privateKeyValid && keyAcknowledged;
      break;
    case OCI_STEP_VALIDATE_SCAN:
      // Validate+scan step's primary actions are the Validate and
      // Scan buttons in the step body — the global Next is unused on
      // the last step (the shell suppresses Next there).
      nextEnabled = false;
      break;
  }

  const handleBack = useCallback(() => {
    setStepIndex((i) => Math.max(0, i - 1));
    // Back from validate / scan invalidates the validate result so
    // the operator must re-run after any edit.
    setValidateResult(null);
    setSubmitError(null);
  }, []);

  const handleNext = useCallback(() => {
    if (!nextEnabled) return;
    setStepIndex((i) => Math.min(stepCount - 1, i + 1));
  }, [nextEnabled, stepCount]);

  const handleCopy = useCallback((value: string) => {
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(value);
    }
  }, []);

  // handleValidate — Step 5 first action.
  //
  // Two-stage: createOCIConnection persists the row (encoded
  // private_key gets credstore-sealed server-side under the
  // squadron.oci_signing_key.v1 AAD), then validateOCIConnection
  // dry-runs a ListInstances call against the configured tenancy +
  // region. On a permission_denied / tenancy_not_found /
  // fingerprint_mismatch / private_key_invalid / network failure the
  // connection still exists but is unhealthy; the operator gets a
  // remediation-specific banner and can re-validate after fixing the
  // upstream state. (We do NOT delete the connection on validation
  // failure — partial setup beats no setup, mirroring the Azure /
  // GCP / IaCGitHubWizard two-stage submit.)
  const handleValidate = useCallback(async () => {
    if (submitting) return;
    if (!fingerprintValid || !privateKeyValid) return;
    setSubmitting(true);
    setSubmitError(null);
    setValidateResult(null);
    try {
      // Reuse the connection created on a prior validate ONLY if that
      // validate succeeded. If it failed, the operator is fixing the
      // credentials (e.g. a rotated or mistyped secret), so re-create
      // with the corrected values — otherwise the re-validate silently
      // re-tests the stale connection and can never recover (the
      // credentials_invalid remediation loop is a dead end). Failed rows
      // are left in place, matching the existing don't-delete-on-failure
      // posture.
      const createReq = {
        display_name: displayName.trim(),
        tenancy_ocid: tenancyOCID.trim(),
        user_ocid: userOCID.trim(),
        fingerprint: fingerprint.trim().toLowerCase(),
        sealed_private_key: encodePrivateKeyForWire(privateKey),
        region: region.trim(),
      };
      // Re-create the connection when there is none yet, the prior
      // validate failed, OR the credential fields changed since the
      // connection was created. The signature comparison is the robust
      // guard: validateResult?.ok === false alone is fragile because it
      // can be cleared (e.g. by step navigation) while createdConnection
      // persists, silently re-testing a stale connection and trapping
      // the operator in a credentials_invalid dead end even after they
      // paste a corrected key. (Failed rows are left in place, matching
      // the don't-delete-on-failure posture.)
      const createSig = JSON.stringify(createReq);
      let conn = createdConnection;
      if (
        !conn ||
        validateResult?.ok === false ||
        createdConnectionSig.current !== createSig
      ) {
        conn = await createOCIConnection(createReq);
        setCreatedConnection(conn);
        createdConnectionSig.current = createSig;
      }
      const v = await validateOCIConnection(conn.id);
      setValidateResult(v);
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }, [
    submitting,
    fingerprintValid,
    privateKeyValid,
    createdConnection,
    validateResult,
    displayName,
    tenancyOCID,
    userOCID,
    fingerprint,
    privateKey,
    region,
  ]);

  // handleScan — Step 5 second action.
  const handleScan = useCallback(async () => {
    if (submitting) return;
    if (!createdConnection) return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      const s = await scanOCIConnection(createdConnection.id);
      setScanResult(s);
      // Hand off to the page so the Inventory tab loads with the
      // result. Drop the pasted private key from state on success
      // — the wizard is done with it.
      onComplete(createdConnection, s);
      setPrivateKey("");
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }, [submitting, createdConnection, onComplete]);

  return (
    <WizardShell
      stepIndex={stepIndex}
      stepCount={stepCount}
      stepTitle={OCI_STEP_TITLES[currentStepID]}
      canAdvance={nextEnabled}
      submitting={submitting}
      onBack={handleBack}
      onNext={handleNext}
    >
      {currentStepID === OCI_STEP_TENANCY && (
        <TenancyStep
          displayName={displayName}
          onDisplayNameChange={setDisplayName}
          tenancyOCID={tenancyOCID}
          onTenancyOCIDChange={setTenancyOCID}
          userOCID={userOCID}
          onUserOCIDChange={setUserOCID}
          region={region}
          onRegionChange={setRegion}
          tenancyOCIDValid={tenancyOCIDValid}
          userOCIDValid={userOCIDValid}
          regionValid={regionValid}
          showWhyExplainer={showWhyExplainer}
          onToggleWhyExplainer={() => setShowWhyExplainer((v) => !v)}
        />
      )}

      {currentStepID === OCI_STEP_GENERATE_KEY && (
        <GenerateKeyStep onCopy={handleCopy} />
      )}

      {currentStepID === OCI_STEP_UPLOAD_KEY && <UploadKeyStep />}

      {currentStepID === OCI_STEP_CREDENTIALS && (
        <CredentialsStep
          fingerprint={fingerprint}
          onFingerprintChange={setFingerprint}
          privateKey={privateKey}
          onPrivateKeyChange={setPrivateKey}
          fingerprintValid={fingerprintValid}
          privateKeyValid={privateKeyValid}
          acknowledged={keyAcknowledged}
          onAcknowledgeChange={setKeyAcknowledged}
        />
      )}

      {currentStepID === OCI_STEP_VALIDATE_SCAN && (
        <ValidateScanStep
          submitting={submitting}
          submitError={submitError}
          validateResult={validateResult}
          scanResult={scanResult}
          connectionRegion={region}
          connectionTenancyOCID={tenancyOCID}
          onValidate={handleValidate}
          onScan={handleScan}
        />
      )}
    </WizardShell>
  );
}

// OCIRegionCombobox is a searchable, scrollable region picker. OCI has ~28
// regions — a plain dropdown overflows the viewport and isn't filterable, so
// this uses cmdk for type-to-filter + keyboard nav, a height-capped scrollable
// list, and a click-outside backdrop to dismiss.
function OCIRegionCombobox({
  value,
  onChange,
  invalid,
}: {
  value: string;
  onChange: (v: string) => void;
  invalid: boolean;
}) {
  const [open, setOpen] = useState(false);
  const selected = OCI_REGIONS.find((r) => r.id === value);
  return (
    <div className="relative">
      <button
        type="button"
        id="oci-region"
        aria-label="OCI region"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-invalid={invalid}
        onClick={() => setOpen((o) => !o)}
        className="flex h-10 w-full items-center justify-between rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 aria-[invalid=true]:border-destructive"
      >
        <span className={selected ? "" : "text-muted-foreground"}>
          {selected
            ? `${selected.id} — ${selected.label}`
            : "Select an OCI region"}
        </span>
        <ChevronDown className="h-4 w-4 opacity-50" />
      </button>
      {open && (
        <>
          <div
            className="fixed inset-0 z-40"
            aria-hidden="true"
            onClick={() => setOpen(false)}
          />
          <div className="absolute z-50 mt-1 w-full rounded-md border bg-popover text-popover-foreground shadow-md">
            <Command label="OCI region" className="flex flex-col">
              <Command.Input
                autoFocus
                placeholder="Search regions (e.g. ashburn, us-, tokyo)…"
                className="w-full border-b border-border bg-transparent px-3 py-2 text-sm outline-none placeholder:text-muted-foreground"
              />
              <Command.List className="max-h-60 overflow-y-auto p-1">
                <Command.Empty className="px-3 py-6 text-center text-sm text-muted-foreground">
                  No matching region.
                </Command.Empty>
                {OCI_REGIONS.map((r) => (
                  <Command.Item
                    key={r.id}
                    value={`${r.id} ${r.label}`}
                    onSelect={() => {
                      onChange(r.id);
                      setOpen(false);
                    }}
                    className="cursor-pointer rounded px-2 py-1.5 text-sm data-[selected=true]:bg-accent data-[selected=true]:text-accent-foreground"
                  >
                    {r.id} — {r.label}
                  </Command.Item>
                ))}
              </Command.List>
            </Command>
          </div>
        </>
      )}
    </div>
  );
}

// --- Step 1: Connect an OCI tenancy ---------------------------------

function TenancyStep({
  displayName,
  onDisplayNameChange,
  tenancyOCID,
  onTenancyOCIDChange,
  userOCID,
  onUserOCIDChange,
  region,
  onRegionChange,
  tenancyOCIDValid,
  userOCIDValid,
  regionValid,
  showWhyExplainer,
  onToggleWhyExplainer,
}: {
  displayName: string;
  onDisplayNameChange: (v: string) => void;
  tenancyOCID: string;
  onTenancyOCIDChange: (v: string) => void;
  userOCID: string;
  onUserOCIDChange: (v: string) => void;
  region: string;
  onRegionChange: (v: string) => void;
  tenancyOCIDValid: boolean;
  userOCIDValid: boolean;
  regionValid: boolean;
  showWhyExplainer: boolean;
  onToggleWhyExplainer: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Tell Squadron which Oracle Cloud tenancy to scan. We&apos;ll never write
        to it — read-only IAM permissions on compute instances are the most the
        API Signing Key will be asked to carry.
      </p>

      <div className="space-y-2">
        <Label htmlFor="oci-display-name">Display name</Label>
        <Input
          id="oci-display-name"
          value={displayName}
          onChange={(e) => onDisplayNameChange(e.target.value)}
          placeholder="Production OCI"
        />
        <p className="text-xs text-muted-foreground">
          A human-friendly label shown in the connection selector. Editable
          later.
        </p>
      </div>

      <div className="space-y-2">
        <Label htmlFor="oci-tenancy-ocid">Tenancy OCID</Label>
        <Input
          id="oci-tenancy-ocid"
          value={tenancyOCID}
          onChange={(e) => onTenancyOCIDChange(e.target.value)}
          placeholder="ocid1.tenancy.oc1..aaaaaaaa..."
          aria-invalid={tenancyOCID !== "" && !tenancyOCIDValid}
          autoComplete="off"
          spellCheck={false}
        />
        {tenancyOCID !== "" && !tenancyOCIDValid && (
          <p className="text-xs text-destructive">
            Tenancy OCIDs must start with <code>ocid1.tenancy.oc1.</code> Find
            the value in the OCI Console under Profile → Tenancy → OCID.
          </p>
        )}
        {tenancyOCID === "" && (
          <p className="text-xs text-muted-foreground">
            The OCI tenancy the scan targets. Visible in the OCI Console at
            Profile → Tenancy → OCID.
          </p>
        )}
      </div>

      <div className="space-y-2">
        <Label htmlFor="oci-user-ocid">User OCID</Label>
        <Input
          id="oci-user-ocid"
          value={userOCID}
          onChange={(e) => onUserOCIDChange(e.target.value)}
          placeholder="ocid1.user.oc1..aaaaaaaa..."
          aria-invalid={userOCID !== "" && !userOCIDValid}
          autoComplete="off"
          spellCheck={false}
        />
        {userOCID !== "" && !userOCIDValid && (
          <p className="text-xs text-destructive">
            User OCIDs must start with <code>ocid1.user.oc1.</code> Find the
            value in the OCI Console under Identity → Users → select user →
            OCID.
          </p>
        )}
        {userOCID === "" && (
          <p className="text-xs text-muted-foreground">
            The OCI user whose API Signing Key will sign Squadron&apos;s API
            calls. Visible in the OCI Console at Identity → Users.
          </p>
        )}
      </div>

      <div className="space-y-2">
        <Label htmlFor="oci-region">Region</Label>
        <OCIRegionCombobox
          value={region}
          onChange={onRegionChange}
          invalid={region !== "" && !regionValid}
        />
        <p className="text-xs text-muted-foreground">
          Unlike AWS / GCP / Azure, OCI requires a region — OCI&apos;s API
          endpoints are regional, so the scanner has to know where to query.
          Slice 1 ships single-region per connection.
        </p>
      </div>

      <button
        type="button"
        className="text-xs text-primary underline"
        onClick={onToggleWhyExplainer}
      >
        Why am I doing this?
      </button>
      {showWhyExplainer && (
        <div className="rounded-md border bg-muted/30 p-3 text-xs text-muted-foreground">
          <p>
            Squadron walks your OCI Compute Instances inventory and flags
            instances that lack the OpenTelemetry tag heuristic the proposer
            reads. The connection here is the credential + scope tuple Squadron
            uses to call the OCI ListInstances API — nothing else. You can
            disconnect at any time; the sealed API Signing Key private key is
            removed from the credstore on delete.
          </p>
          <p className="mt-2">
            <a
              href={OCI_DOC_LINK}
              className="text-primary underline"
              target="_blank"
              rel="noreferrer"
            >
              Read the operator runbook
            </a>
          </p>
        </div>
      )}
    </div>
  );
}

// --- Step 2: Generate the API signing key ---------------------------

function GenerateKeyStep({ onCopy }: { onCopy: (v: string) => void }) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Generate a 2048-bit RSA keypair locally. The private half stays on your
        machine + gets pasted into Squadron in Step 4; the public half gets
        uploaded to OCI Console in Step 3.
      </p>

      <CommandBlock
        label="Option 1: OCI CLI helper"
        cmd={OCI_SETUP_KEYS_CMD}
        onCopy={() => onCopy(OCI_SETUP_KEYS_CMD)}
      />

      <CommandBlock
        label="Option 2: openssl"
        cmd={OCI_OPENSSL_GENERATE_CMD}
        onCopy={() => onCopy(OCI_OPENSSL_GENERATE_CMD)}
      />

      <div className="rounded-md border bg-muted/30 p-3 text-xs text-muted-foreground">
        <p>
          The last command outputs the key fingerprint — note it for Step 4. It
          looks like a colon-separated 16-hex-pair string, e.g.
          <code> aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99</code>.
        </p>
      </div>

      <p className="text-xs text-muted-foreground">
        <a
          href={OCI_API_KEYS_DOC_LINK}
          target="_blank"
          rel="noreferrer"
          className="text-primary underline inline-flex items-center gap-1"
        >
          Learn more about OCI API Signing Keys
          <ExternalLink className="h-3 w-3" aria-hidden />
        </a>
      </p>
    </div>
  );
}

// CommandBlock renders a monospace pre + Copy button for one CLI
// command. Factored out for parallelism with the Azure wizard's
// CommandBlock and the GCP wizard's three uses.
function CommandBlock({
  label,
  cmd,
  onCopy,
}: {
  label: string;
  cmd: string;
  onCopy: () => void;
}) {
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
          {label}
        </span>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={onCopy}
          aria-label={`Copy command: ${label}`}
        >
          <Copy className="mr-1 h-3 w-3" aria-hidden />
          Copy
        </Button>
      </div>
      <pre className="rounded-md border bg-muted/40 p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-all">
        {cmd}
      </pre>
    </div>
  );
}

// --- Step 3: Upload public key to OCI Console -----------------------

function UploadKeyStep() {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Upload the public half of the keypair (
        <code>oci_api_key_public.pem</code>) to OCI Console so OCI can verify
        Squadron&apos;s signed requests.
      </p>

      <ol className="ml-4 list-decimal space-y-2 text-sm">
        <li>
          Navigate to the{" "}
          <a
            href="https://cloud.oracle.com/identity/users"
            target="_blank"
            rel="noreferrer"
            className="text-primary underline inline-flex items-center gap-1"
          >
            OCI Console → Identity → Users
            <ExternalLink className="h-3 w-3" aria-hidden />
          </a>
          .
        </li>
        <li>Select the user whose OCID you entered in Step 1.</li>
        <li>
          Under the Resources sidebar, click <strong>API Keys</strong>.
        </li>
        <li>
          Click <strong>Add API Key</strong>.
        </li>
        <li>
          Choose <strong>Paste a public key</strong> and paste the contents of{" "}
          <code>oci_api_key_public.pem</code>.
        </li>
        <li>
          Confirm the fingerprint OCI Console displays matches the fingerprint
          your openssl command output in Step 2. Note the fingerprint for Step
          4.
        </li>
      </ol>

      <div className="rounded-md border bg-muted/30 p-3 text-xs text-muted-foreground">
        <p>
          OCI Console also displays a config file snippet after the upload — you
          can ignore that snippet for Squadron; Squadron only needs the tenancy
          OCID, user OCID, region, fingerprint, and private key (entered
          separately in this wizard).
        </p>
      </div>

      <p className="text-xs text-muted-foreground">
        <a
          href={OCI_IAM_DOC_LINK}
          target="_blank"
          rel="noreferrer"
          className="text-primary underline inline-flex items-center gap-1"
        >
          Learn more about OCI IAM policies
          <ExternalLink className="h-3 w-3" aria-hidden />
        </a>
      </p>
    </div>
  );
}

// --- Step 4: Paste credentials --------------------------------------

function CredentialsStep({
  fingerprint,
  onFingerprintChange,
  privateKey,
  onPrivateKeyChange,
  fingerprintValid,
  privateKeyValid,
  acknowledged,
  onAcknowledgeChange,
}: {
  fingerprint: string;
  onFingerprintChange: (v: string) => void;
  privateKey: string;
  onPrivateKeyChange: (v: string) => void;
  fingerprintValid: boolean;
  privateKeyValid: boolean;
  acknowledged: boolean;
  onAcknowledgeChange: (v: boolean) => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Paste the fingerprint OCI Console displayed in Step 3 and the contents
        of <code>oci_api_key.pem</code> from Step 2.
      </p>

      <div className="space-y-2">
        <Label htmlFor="oci-fingerprint">Fingerprint</Label>
        <Input
          id="oci-fingerprint"
          value={fingerprint}
          onChange={(e) => onFingerprintChange(e.target.value)}
          placeholder="aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
          aria-invalid={fingerprint !== "" && !fingerprintValid}
          autoComplete="off"
          spellCheck={false}
          className="font-mono text-xs"
        />
        {fingerprint !== "" && !fingerprintValid && (
          <p className="text-xs text-destructive">
            Fingerprints must be 16 colon-separated hex pairs (e.g.{" "}
            <code>aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99</code>).
            Re-verify with{" "}
            <code>
              openssl rsa -pubout -outform DER -in oci_api_key.pem | openssl md5
              -c
            </code>
            .
          </p>
        )}
      </div>

      <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-amber-900 dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-200">
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 h-4 w-4 flex-shrink-0" aria-hidden />
          <p className="text-xs">
            The API Signing Key private key is full asymmetric authentication
            material — the strongest credential type Squadron handles. Squadron
            seals it at rest with AES-GCM; the bytes never appear in audit
            payloads or logs. Never paste this key into Slack, email, or any
            other transient surface.
          </p>
        </div>
      </div>

      <div className="space-y-2">
        <Label htmlFor="oci-private-key">Private key (PEM)</Label>
        <Textarea
          id="oci-private-key"
          value={privateKey}
          onChange={(e) => onPrivateKeyChange(e.target.value)}
          placeholder={
            "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIB...\n-----END PRIVATE KEY-----"
          }
          rows={8}
          className="font-mono text-xs"
          aria-invalid={privateKey !== "" && !privateKeyValid}
          autoComplete="off"
          spellCheck={false}
          data-1p-ignore
        />
        {privateKey !== "" && !privateKeyValid && (
          <p className="text-xs text-destructive">
            The paste should include the{" "}
            <code>-----BEGIN PRIVATE KEY-----</code> and{" "}
            <code>-----END PRIVATE KEY-----</code> markers (or the matching
            RSA-PRIVATE-KEY variant). Re-copy the file contents — most terminals
            truncate when copying via select-all.
          </p>
        )}
        <p className="text-xs text-muted-foreground">
          The key stays in browser memory until the wizard completes — it is
          base64-encoded over the wire and sealed at rest by Squadron under the{" "}
          <code>squadron.oci_signing_key.v1</code> AAD.
        </p>
      </div>

      <div className="flex items-start gap-2">
        <Checkbox
          id="oci-key-ack"
          checked={acknowledged}
          onCheckedChange={(v) => onAcknowledgeChange(v === true)}
          disabled={!fingerprintValid || !privateKeyValid}
        />
        <Label
          htmlFor="oci-key-ack"
          className="text-xs font-normal leading-tight text-muted-foreground"
        >
          I have stored this private key securely. Squadron seals it at rest,
          but the bytes are visible during paste.
        </Label>
      </div>
    </div>
  );
}

// --- Step 5: Validate + Scan ----------------------------------------

function ValidateScanStep({
  submitting,
  submitError,
  validateResult,
  scanResult,
  connectionRegion,
  connectionTenancyOCID,
  onValidate,
  onScan,
}: {
  submitting: boolean;
  submitError: string | null;
  validateResult: ValidateOCIResponse | null;
  scanResult: ScanOCIResponse | null;
  connectionRegion: string;
  connectionTenancyOCID: string;
  onValidate: () => void;
  onScan: () => void;
}) {
  const validatedOK = validateResult?.ok === true;
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Squadron will dry-run a ListInstances call against your tenancy to
        confirm the API Signing Key works. On success, the second button runs a
        full scan and lands you on the Inventory tab.
      </p>
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          onClick={onValidate}
          disabled={submitting || validatedOK}
        >
          {submitting && !validatedOK ? (
            <>
              <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
              Validating...
            </>
          ) : (
            "Validate connection"
          )}
        </Button>
        <Button
          type="button"
          onClick={onScan}
          disabled={submitting || !validatedOK}
          variant={validatedOK ? "default" : "secondary"}
        >
          {submitting && validatedOK ? (
            <>
              <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
              Scanning...
            </>
          ) : (
            "Run scan"
          )}
        </Button>
      </div>

      {submitError && (
        <div
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {submitError}
        </div>
      )}

      {validateResult?.ok === true && (
        <div
          role="status"
          className="rounded-md border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-900 dark:border-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-200"
        >
          <div className="flex items-center gap-2">
            <CheckCircle2 className="h-4 w-4" aria-hidden />
            <span className="font-medium">
              Connected — {validateResult.instance_count ?? 0} compute instances
              visible.
            </span>
          </div>
          <p className="mt-1 text-xs">
            Click <strong>Run scan</strong> to inventory the tenancy.
          </p>
        </div>
      )}

      {validateResult?.ok === false && (
        <div
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          <div className="flex items-center gap-2 font-medium">
            <AlertTriangle className="h-4 w-4" aria-hidden />
            <span>Validation failed</span>
          </div>
          {validateResult.message && (
            <p className="mt-1 text-xs">{validateResult.message}</p>
          )}
          <p className="mt-2 text-xs">
            {validateErrorRemediation(
              validateResult.error_kind as OCIValidateErrorKind,
              {
                connectionRegion,
                connectionTenancyOCID,
              },
            )}
          </p>
        </div>
      )}

      {scanResult && (
        <div
          role="status"
          className="rounded-md border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-900 dark:border-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-200"
        >
          <div className="flex items-center gap-2">
            <CheckCircle2 className="h-4 w-4" aria-hidden />
            <span className="font-medium">
              Scan complete: {scanResult.instance_count} compute instances (
              {scanResult.instrumented_count} instrumented,{" "}
              {scanResult.uninstrumented_count} uninstrumented). View Inventory
              →
            </span>
          </div>
          <p className="mt-1 text-xs">
            View the Inventory tab for per-instance detail.
          </p>
        </div>
      )}
    </div>
  );
}

// --- Inventory tab --------------------------------------------------

function InventoryTab({
  hasConnections,
  scan,
  onJumpToWizard,
}: {
  hasConnections: boolean;
  scan: ScanOCIResponse | undefined;
  onJumpToWizard: () => void;
}) {
  if (!hasConnections) {
    return (
      <div className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
        Connect an OCI tenancy from the Wizard tab to populate inventory.
      </div>
    );
  }
  if (!scan) {
    return (
      <div className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
        No inventory loaded. Run a scan from the{" "}
        <button
          type="button"
          onClick={onJumpToWizard}
          className="text-primary underline"
        >
          Wizard tab
        </button>
        .
      </div>
    );
  }
  // Database tier slice 2 (v0.89.66, #695 Stream 93) — nested
  // Compute / Databases sub-tabs. Default sub-tab is Compute so
  // the slice-1 UX is preserved; the Databases sub-tab surfaces
  // the OCI DB Systems + Autonomous Database inventory from the
  // chunk 4 scanner extension.
  //
  // Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) —
  // the Orchestration sub-tab is conditionally hidden when the
  // orchestrations[] array is empty (slice 1 contract per
  // docs/proposals/orchestration-tier-slice1.md §7). The OCI scanner
  // returns no orchestration rows in slice 1; once slice 2 ships the
  // OCI orchestration substrate the sub-tab will appear automatically.
  const showOrchestration = (scan.orchestrations?.length ?? 0) > 0;
  return (
    <div className="space-y-3">
      <InventorySummary scan={scan} />
      <TerraformAdoptCard
        onGenerate={() => generateOCITerraformImport(scan.connection_id, scan)}
      />
      <Tabs defaultValue={INVENTORY_SUBTAB_COMPUTE}>
        <TabsList>
          <TabsTrigger value={INVENTORY_SUBTAB_COMPUTE}>Compute</TabsTrigger>
          <TabsTrigger value={INVENTORY_SUBTAB_DATABASES}>
            Databases
          </TabsTrigger>
          <TabsTrigger value={INVENTORY_SUBTAB_KUBERNETES}>
            Kubernetes
          </TabsTrigger>
          <TabsTrigger value={INVENTORY_SUBTAB_SERVERLESS}>
            Serverless
          </TabsTrigger>
          {showOrchestration && (
            <TabsTrigger value={INVENTORY_SUBTAB_ORCHESTRATION}>
              Orchestration
            </TabsTrigger>
          )}
          {/* Event sources sub-tab renders for OCI in slice 1, unlike
              Orchestration which is hidden — see design doc §7. */}
          <TabsTrigger value={INVENTORY_SUBTAB_EVENT_SOURCES}>
            Event sources
          </TabsTrigger>
        </TabsList>
        <TabsContent value={INVENTORY_SUBTAB_COMPUTE} className="mt-3">
          <InventoryTable rows={scan.computes ?? []} />
        </TabsContent>
        <TabsContent value={INVENTORY_SUBTAB_DATABASES} className="mt-3">
          <DatabaseInventoryTable rows={scan.databases ?? []} />
        </TabsContent>
        <TabsContent value={INVENTORY_SUBTAB_KUBERNETES} className="mt-3">
          <ClusterInventoryTable rows={scan.clusters ?? []} />
        </TabsContent>
        <TabsContent value={INVENTORY_SUBTAB_SERVERLESS} className="mt-3">
          <ServerlessInventoryTable rows={scan.serverless ?? []} />
        </TabsContent>
        {showOrchestration && (
          <TabsContent value={INVENTORY_SUBTAB_ORCHESTRATION} className="mt-3">
            <OrchestrationInventoryTable rows={scan.orchestrations ?? []} />
          </TabsContent>
        )}
        <TabsContent value={INVENTORY_SUBTAB_EVENT_SOURCES} className="mt-3">
          <EventSourcesInventoryTable rows={scan.event_sources ?? []} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function InventorySummary({ scan }: { scan: ScanOCIResponse }) {
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-md border p-3 text-sm">
      <span>
        Tenancy: <code className="text-xs">{scan.tenancy_ocid}</code>
      </span>
      <span>Region: {scan.region}</span>
      <span>Instances: {scan.instance_count}</span>
      <span>Instrumented: {scan.instrumented_count}</span>
      <span>Uninstrumented: {scan.uninstrumented_count}</span>
      {scan.partial && (
        <Badge variant="outline" className="text-amber-600">
          Partial: {scan.partial_reason || "unknown"}
        </Badge>
      )}
    </div>
  );
}

function InventoryTable({ rows }: { rows: ComputeInstanceSnapshot[] }) {
  if (rows.length === 0) {
    return (
      <div className="rounded-md border p-6 text-center text-sm text-muted-foreground">
        Scan completed but no compute instances were returned. Either the
        tenancy is empty in this region or the user OCID lacks read access on
        the compartments it spans.
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">Instance Name</th>
            <th className="px-3 py-2 font-medium">Shape</th>
            <th className="px-3 py-2 font-medium">OS</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">OTel?</th>
            <th className="px-3 py-2 font-medium">Last seen</th>
            <th className="px-3 py-2 font-medium">Tags</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.resource_id} className="border-t">
              <td className="px-3 py-2 font-mono text-xs">{row.resource_id}</td>
              <td className="px-3 py-2 text-xs">{row.instance_type}</td>
              <td className="px-3 py-2 text-xs">
                {row.os_family || "unknown"}
              </td>
              <td className="px-3 py-2 text-xs">{row.region}</td>
              <td className="px-3 py-2 text-xs">
                {row.has_otel ? (
                  <Badge variant="outline" className="text-emerald-600">
                    Yes
                  </Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground">
                    No
                  </Badge>
                )}
              </td>
              <td className="px-3 py-2 text-xs">
                <LastSeenCell value={row.last_seen_at} />
              </td>
              <td className="px-3 py-2 font-mono text-xs">
                {Object.keys(row.tags ?? {}).length === 0
                  ? "-"
                  : Object.entries(row.tags)
                      .map(([k, v]) => `${k}=${v}`)
                      .join(", ")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// DatabaseInventoryTable — database tier slice 2 (v0.89.66, #695
// Stream 93). Renders the OCI DB Systems + Autonomous Database
// inventory from the chunk 4 scanner extension. The instrumentation
// column reads from database_management_enabled (the OCI single-
// axis observability lever — Operations Insights / Database
// Management enrollment); rows where the field is undefined render
// "No" because absence is the uncovered signal per design doc §3.3.
function DatabaseInventoryTable({
  rows,
}: {
  rows: DatabaseInstanceSnapshot[];
}) {
  if (rows.length === 0) {
    return (
      <div className="rounded-md border p-6 text-center text-sm text-muted-foreground">
        No databases discovered. Run a scan to refresh.
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">Resource ID</th>
            <th className="px-3 py-2 font-medium">Engine</th>
            <th className="px-3 py-2 font-medium">Engine Version</th>
            <th className="px-3 py-2 font-medium">Shape</th>
            <th className="px-3 py-2 font-medium">
              Database Management enabled?
            </th>
            <th className="px-3 py-2 font-medium">Last seen</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">Tags</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.resource_id} className="border-t">
              <td className="px-3 py-2 font-mono text-xs">{row.resource_id}</td>
              <td className="px-3 py-2 text-xs">{row.engine || "-"}</td>
              <td className="px-3 py-2 text-xs">{row.engine_version || "-"}</td>
              <td className="px-3 py-2 text-xs">{row.instance_class || "-"}</td>
              <td className="px-3 py-2 text-xs">
                {row.database_management_enabled ? (
                  <Badge variant="outline" className="text-emerald-600">
                    Yes
                  </Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground">
                    No
                  </Badge>
                )}
              </td>
              <td className="px-3 py-2 text-xs">
                <LastSeenCell value={row.last_seen_at} />
              </td>
              <td className="px-3 py-2 text-xs">{row.region}</td>
              <td className="px-3 py-2 font-mono text-xs">
                {Object.keys(row.tags ?? {}).length === 0
                  ? "-"
                  : Object.entries(row.tags ?? {})
                      .map(([k, v]) => `${k}=${v}`)
                      .join(", ")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ClusterInventoryTable — Kubernetes tier slice 2 (v0.89.71, #702
// Stream 100). Renders the OKE cluster inventory from the v0.89.70
// chunk 4 scanner extension. The instrumentation column reads from
// operations_insights_enabled (the OCI single-axis observability
// lever — Operations Insights enrollment via the
// operations-insights-enabled=true freeform tag convention); rows
// where the field is undefined render "No" because absence is the
// uncovered signal per design doc §3.3.
function ClusterInventoryTable({ rows }: { rows: ClusterSnapshot[] }) {
  if (rows.length === 0) {
    return (
      <div className="rounded-md border p-6 text-center text-sm text-muted-foreground">
        No Kubernetes clusters discovered. Run a scan to refresh.
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">Resource ID</th>
            <th className="px-3 py-2 font-medium">Cluster Name</th>
            <th className="px-3 py-2 font-medium">Kubernetes Version</th>
            <th className="px-3 py-2 font-medium">Status</th>
            <th className="px-3 py-2 font-medium">Operations Insights?</th>
            <th className="px-3 py-2 font-medium">Last seen</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">Tags</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.resource_id} className="border-t">
              <td className="px-3 py-2 font-mono text-xs">{row.resource_id}</td>
              <td className="px-3 py-2 text-xs">{row.name || "-"}</td>
              <td className="px-3 py-2 text-xs">
                {row.kubernetes_version || "-"}
              </td>
              <td className="px-3 py-2 text-xs">{row.status || "-"}</td>
              <td className="px-3 py-2 text-xs">
                {row.operations_insights_enabled ? (
                  <Badge variant="outline" className="text-emerald-600">
                    Yes
                  </Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground">
                    No
                  </Badge>
                )}
              </td>
              <td className="px-3 py-2 text-xs">
                <LastSeenCell value={row.last_seen_at} />
              </td>
              <td className="px-3 py-2 text-xs">{row.region}</td>
              <td className="px-3 py-2 font-mono text-xs">
                {Object.keys(row.tags ?? {}).length === 0
                  ? "-"
                  : Object.entries(row.tags ?? {})
                      .map(([k, v]) => `${k}=${v}`)
                      .join(", ")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ServerlessInventoryTable — serverless tier slice 1 chunk 5
// (v0.89.92, #725 Stream 123). Renders the per-row OCI Functions
// inventory the chunk 4 OCI scanner extension produced. Same column
// shape as the GCP / Azure Serverless sub-tabs: Resource Name,
// Surface (always "ocifunc" on OCI), Runtime, Region, Trace axis
// (config[OCI_APM_ENABLED] true?), OTel distro (config[OTEL_DISTRO]
// set?), Last seen.
function ServerlessInventoryTable({ rows }: { rows: ServerlessRow[] }) {
  if (rows.length === 0) {
    return (
      <div className="rounded-md border p-6 text-center text-sm text-muted-foreground">
        No serverless functions discovered. Run a scan to refresh.
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">Resource Name</th>
            <th className="px-3 py-2 font-medium">Surface</th>
            <th className="px-3 py-2 font-medium">Runtime</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">Trace axis</th>
            <th className="px-3 py-2 font-medium">OTel distro</th>
            {/* Cold-start latency analysis slice 2 chunk 4 (v0.89.119,
                #759 Stream 157) — populated for OCI Functions when
                the per-cloud MetricQuerier substrate (slice 2
                chunk 3) has persisted an observation. Renders the
                canonical "—" otherwise. The OCI substrate skips
                detection when cold_start_count == 0 in the current
                window; those rows leave the cell at "—" until cold
                starts occur. */}
            <th className="px-3 py-2 font-medium">Cold-start P95 (24h)</th>
            {/* Sampling rate analysis slice 1 chunk 3 (v0.89.124,
                #764 Stream 162) — new "Sampling rate (24h)" column
                between Cold-start P95 and Last seen. OCI Functions
                participate per slice 1. */}
            <th className="px-3 py-2 font-medium">Sampling rate (24h)</th>
            {/* Error rate correlation slice 1 chunk 3 (v0.89.129,
                #769 Stream 167) — new "Error rate (24h)" column
                between Sampling rate and Last seen. OCI Functions
                participates. */}
            <th className="px-3 py-2 font-medium">Error rate (24h)</th>
            <th className="px-3 py-2 font-medium">Last seen</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr
              key={row.resource_arn || row.resource_name}
              className="border-t"
            >
              <td className="px-3 py-2 font-mono text-xs">
                {row.resource_name}
              </td>
              <td className="px-3 py-2 text-xs">{row.surface}</td>
              <td className="px-3 py-2 text-xs">{row.runtime || "-"}</td>
              <td className="px-3 py-2 text-xs">{row.region}</td>
              <td className="px-3 py-2 text-xs">
                {row.has_trace_axis ? (
                  <Badge variant="outline" className="text-emerald-600">
                    Yes
                  </Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground">
                    No
                  </Badge>
                )}
              </td>
              <td className="px-3 py-2 text-xs">
                {row.has_otel_distro ? (
                  <Badge variant="outline" className="text-emerald-600">
                    Yes
                  </Badge>
                ) : (
                  <Badge variant="outline" className="text-muted-foreground">
                    No
                  </Badge>
                )}
              </td>
              <td className="px-3 py-2 text-xs">
                <ColdStartCell row={row} />
              </td>
              <td className="px-3 py-2 text-xs">
                <SamplingRateCell row={row} />
              </td>
              <td className="px-3 py-2 text-xs">
                <ErrorRateCell row={row} />
              </td>
              <td className="px-3 py-2 text-xs">
                <LastSeenCell value={row.last_seen_at} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// OrchestrationInventoryTable — orchestration tier slice 1 chunk 4
// (v0.89.97, #731 Stream 129). Slice 1 contract: OCI orchestration
// substrate ships no rows, so this table is only rendered by the
// conditional sub-tab path when slice 2 lands. Column layout matches
// the GCP / Azure tables exactly so the forward-compatible UX is
// already in place.
function OrchestrationInventoryTable({ rows }: { rows: OrchestrationRow[] }) {
  if (rows.length === 0) {
    return (
      <div className="rounded-md border p-6 text-center text-sm text-muted-foreground">
        No orchestration workflows discovered. Run a scan to refresh.
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">Resource Name</th>
            <th className="px-3 py-2 font-medium">Surface</th>
            <th className="px-3 py-2 font-medium">Type</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">Trace axis</th>
            <th className="px-3 py-2 font-medium">Log axis</th>
            <th className="px-3 py-2 font-medium">Last seen</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr
              key={row.resource_arn || row.resource_name}
              className="border-t"
            >
              <td className="px-3 py-2 font-mono text-xs">
                {row.resource_name}
              </td>
              <td className="px-3 py-2 text-xs">{row.surface}</td>
              <td className="px-3 py-2 text-xs">{row.workflow_type || "—"}</td>
              <td className="px-3 py-2 text-xs">{row.region}</td>
              <td className="px-3 py-2 text-xs">
                <OrchestrationAxisCheck ok={row.has_trace_axis} />
              </td>
              <td className="px-3 py-2 text-xs">
                <OrchestrationAxisCheck ok={row.has_log_axis} />
              </td>
              <td className="px-3 py-2 text-xs">
                <LastSeenCell value={row.last_seen_at} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// EventSourcesInventoryTable — event source tier slice 1 chunk 5
// (v0.89.102, #738 Stream 136). Renders the per-row OCI Streaming
// inventory the chunk 4 scanner produced. Columns follow §7 of the
// design doc: Resource Name, Surface, Type, Region, Trace axis, Log
// axis, Last seen. The Quality column is AWS-only per the slice 1
// constraint. OCI populates this sub-tab unlike Orchestration which
// is hidden when empty — OCI Streaming ships as a real surface in
// slice 1 (design doc §3.4).
function EventSourcesInventoryTable({ rows }: { rows: EventSourceRow[] }) {
  // propagationDialog — event source tier slice 2 chunk 5 (v0.89.107,
  // #745 Stream 143). See DiscoveryAWS.tsx::PropagationNotesDialog.
  const [propagationDialog, setPropagationDialog] = useState<{
    row: EventSourceRow;
    notes: string[];
  } | null>(null);
  if (rows.length === 0) {
    return (
      <div
        className="rounded-md border p-6 text-center text-sm text-muted-foreground"
        data-testid="event-sources-empty"
      >
        No event sources discovered. Run a scan to refresh.
      </div>
    );
  }
  return (
    <>
      <div
        className="overflow-x-auto rounded-md border"
        data-testid="event-sources-table"
      >
        <table className="w-full text-sm">
          <thead className="bg-muted/40">
            <tr className="text-left">
              <th className="px-3 py-2 font-medium">Resource Name</th>
              <th className="px-3 py-2 font-medium">Surface</th>
              <th className="px-3 py-2 font-medium">Type</th>
              <th className="px-3 py-2 font-medium">Region</th>
              <th className="px-3 py-2 font-medium">Trace axis</th>
              <th className="px-3 py-2 font-medium">Log axis</th>
              <th className="px-3 py-2 font-medium">Propagation</th>
              <th className="px-3 py-2 font-medium">Last seen</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr
                key={row.resource_arn || row.resource_name}
                className="border-t"
                data-testid="event-sources-row"
              >
                <td className="px-3 py-2 font-mono text-xs">
                  {row.resource_name}
                </td>
                <td className="px-3 py-2 text-xs">{row.surface}</td>
                <td className="px-3 py-2 text-xs">{row.source_type || "—"}</td>
                <td className="px-3 py-2 text-xs">{row.region}</td>
                <td className="px-3 py-2 text-xs">
                  <OrchestrationAxisCheck ok={row.has_trace_axis} />
                </td>
                <td className="px-3 py-2 text-xs">
                  <OrchestrationAxisCheck ok={row.has_log_axis} />
                </td>
                <td className="px-3 py-2 text-xs">
                  <PropagationCell
                    row={row}
                    onOpen={(r, notes) =>
                      setPropagationDialog({ row: r, notes })
                    }
                  />
                </td>
                <td className="px-3 py-2 text-xs">
                  <LastSeenCell value={row.last_seen_at} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <PropagationNotesDialog
        state={propagationDialog}
        onClose={() => setPropagationDialog(null)}
      />
    </>
  );
}

// PropagationCell — event source tier slice 2 chunk 5 (v0.89.107,
// #745 Stream 143). Mirrors the AWS variant; amber on ✗ matches the
// slice 1 palette.
function PropagationCell({
  row,
  onOpen,
}: {
  row: EventSourceRow;
  onOpen: (row: EventSourceRow, notes: string[]) => void;
}) {
  if (row.has_propagation_config === undefined) {
    return (
      <span
        aria-label="not evaluated"
        title="Propagation not evaluated for this row"
        data-testid="propagation-cell"
        data-value="unknown"
      >
        —
      </span>
    );
  }
  if (row.has_propagation_config) {
    return (
      <span
        className="text-emerald-600"
        aria-label="propagation preserved"
        title="Config preserves trace context end-to-end"
        data-testid="propagation-cell"
        data-value="yes"
      >
        ✓
      </span>
    );
  }
  const notes = row.propagation_notes ?? [];
  return (
    <button
      type="button"
      onClick={() => onOpen(row, notes)}
      className="text-amber-500 hover:text-amber-600"
      aria-label="propagation broken — click for details"
      title={notes[0] ?? "Propagation broken"}
      data-testid="propagation-cell"
      data-value="no"
    >
      ✗
    </button>
  );
}

// PropagationNotesDialog — event source tier slice 2 chunk 5
// (v0.89.107, #745 Stream 143). Side panel listing all
// propagation_notes for a row.
function PropagationNotesDialog({
  state,
  onClose,
}: {
  state: { row: EventSourceRow; notes: string[] } | null;
  onClose: () => void;
}) {
  const isOpen = state !== null;
  return (
    <Dialog
      open={isOpen}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent
        className="max-w-lg"
        data-testid="propagation-notes-dialog"
      >
        <DialogHeader>
          <DialogTitle>Propagation notes</DialogTitle>
          <DialogDescription>
            {state ? `${state.row.surface} · ${state.row.resource_name}` : ""}
          </DialogDescription>
        </DialogHeader>
        {state && state.notes.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            Propagation broken; no specific notes recorded for this row.
          </p>
        ) : null}
        {state && state.notes.length > 0 ? (
          <ul
            className="space-y-2 text-sm"
            data-testid="propagation-notes-list"
          >
            {state.notes.map((note, i) => (
              <li
                key={i}
                className="rounded-md border border-amber-500/40 bg-amber-50/40 px-3 py-2 text-amber-900 dark:bg-amber-950/20 dark:text-amber-200"
              >
                {note}
              </li>
            ))}
          </ul>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function OrchestrationAxisCheck({ ok }: { ok: boolean }) {
  if (ok) {
    return (
      <span
        className="text-emerald-600"
        title="enabled"
        aria-label="enabled"
        data-testid="orchestration-axis-check"
        data-value="yes"
      >
        ✓
      </span>
    );
  }
  return (
    <span
      className="text-muted-foreground"
      title="disabled"
      aria-label="disabled"
      data-testid="orchestration-axis-check"
      data-value="no"
    >
      ✗
    </span>
  );
}

// --- Recommendations tab --------------------------------------------

function RecommendationsTab({
  scan,
  connectionID,
}: {
  scan?: ScanOCIResponse;
  connectionID: string;
}) {
  const [recs, setRecs] = useState<GenerateRecommendationsResponse | null>(
    null,
  );
  const [generating, setGenerating] = useState(false);
  const [genError, setGenError] = useState<string | null>(null);

  const onGenerate = useCallback(async () => {
    if (!scan || generating) return;
    setGenerating(true);
    setGenError(null);
    try {
      const r = await generateOCIRecommendations(connectionID, scan);
      setRecs(r);
    } catch (e) {
      setGenError(e instanceof Error ? e.message : String(e));
    } finally {
      setGenerating(false);
    }
  }, [scan, connectionID, generating]);

  if (!scan) {
    return (
      <div className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
        Run a scan from the Inventory tab first — recommendations are drafted
        from the latest OCI scan result.
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          Draft an instrumentation plan from the latest scan of this tenancy.
        </p>
        <Button onClick={onGenerate} disabled={generating}>
          {generating ? "Generating…" : "Generate recommendations"}
        </Button>
      </div>
      {genError && (
        <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
          {genError}
        </div>
      )}
      <AWSRecommendationsTab
        recs={recs}
        accountID={scan.tenancy_ocid}
        scanID={scan.scan_id}
        region={scan.region}
      />
    </div>
  );
}

// LastSeenCell — v0.89.77 trace integration slice 1 chunk 4. See
// DiscoveryGCP.tsx::LastSeenCell for the shared rendering rule.
function LastSeenCell({ value }: { value?: string }) {
  const rel = relativeTime(value);
  if (rel.isNever) {
    return (
      <span
        className="inline-flex items-center gap-1 text-amber-600"
        title="No spans observed for this resource"
        data-testid="last-seen-never"
      >
        <AlertTriangle className="h-3 w-3" aria-hidden />
        {rel.text}
      </span>
    );
  }
  return (
    <span className="text-muted-foreground" title={value}>
      {rel.text}
    </span>
  );
}

// ColdStartCell — Cold-start latency analysis slice 2 chunk 4
// (v0.89.119, #759 Stream 157). Renders the per-row 24h P95
// cold-start observation surfaced on ServerlessRow. Mirrors the AWS
// DiscoveryAWS ColdStartCell logic so the per-cloud Discovery tabs
// keep a single rendering vocabulary across the 4 providers.
function ColdStartCell({ row }: { row: ServerlessRow }) {
  if (row.cold_start_p95_ms === undefined || row.cold_start_p95_ms === null) {
    return (
      <span
        className="text-muted-foreground"
        title="No cold-start observation yet"
        data-testid="cold-start-cell"
        data-value="none"
      >
        —
      </span>
    );
  }
  const isAmber = row.cold_start_exceeds_threshold === true;
  const ms = Math.round(row.cold_start_p95_ms);
  return (
    <span
      className={isAmber ? "text-amber-600" : "text-foreground"}
      title={
        isAmber
          ? `Cold-start P95 ${ms}ms exceeds baseline threshold (>= 1.5x baseline AND >= 500ms)`
          : `Cold-start P95 ${ms}ms`
      }
      data-testid="cold-start-cell"
      data-value={isAmber ? "amber" : "ok"}
    >
      {ms}ms
    </span>
  );
}

// ErrorRateCell — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Mirrors the AWS DiscoveryAWS
// ErrorRateCell exactly. See AWS ErrorRateCell godoc.
function ErrorRateCell({ row }: { row: ServerlessRow }) {
  if (row.current_error_rate === undefined || row.current_error_rate === null) {
    return (
      <span
        className="text-muted-foreground"
        title="No error-rate observation yet"
        data-testid="error-rate-cell"
        data-value="none"
      >
        —
      </span>
    );
  }
  const pct = row.current_error_rate * 100;
  const isAmber = row.error_rate_exceeds_threshold === true;
  return (
    <span
      className={isAmber ? "text-amber-600" : "text-foreground"}
      title={
        isAmber
          ? `Error rate ${pct.toFixed(2)}% — exceeds 2x baseline + minimums`
          : `Error rate ${pct.toFixed(2)}%`
      }
      data-testid="error-rate-cell"
      data-value={isAmber ? "amber" : "ok"}
    >
      {pct.toFixed(2)}%
    </span>
  );
}

// SamplingRateCell — Sampling rate analysis slice 1 chunk 3
// (v0.89.124, #764 Stream 162). Mirrors the AWS DiscoveryAWS
// SamplingRateCell exactly. See AWS SamplingRateCell godoc.
function SamplingRateCell({ row }: { row: ServerlessRow }) {
  if (row.sampling_ratio === undefined || row.sampling_ratio === null) {
    return (
      <span
        className="text-muted-foreground"
        title="No sampling observation yet"
        data-testid="sampling-rate-cell"
        data-value="none"
      >
        —
      </span>
    );
  }
  const pct = row.sampling_ratio * 100;
  const isAmber = row.sampling_exceeds_floor === true;
  return (
    <span
      className={isAmber ? "text-amber-600" : "text-foreground"}
      title={
        isAmber
          ? `Sampling ratio ${pct.toFixed(1)}% — below 5% floor with >= 1000 invocations`
          : `Sampling ratio ${pct.toFixed(1)}%`
      }
      data-testid="sampling-rate-cell"
      data-value={isAmber ? "amber" : "ok"}
    >
      {pct.toFixed(1)}%
    </span>
  );
}
