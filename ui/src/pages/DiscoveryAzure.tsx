// DiscoveryAzure — the v0.89.53 #677 Stream 75 (slice-1 chunk 4) Azure
// discovery page. Parallels DiscoveryGCP.tsx in tab structure
// (Wizard / Inventory / Recommendations) and operator UX, but the
// step bodies differ because the Azure credential model is "tenant +
// subscription + Service Principal client_id + client_secret paste"
// rather than GCP's "SA JSON paste + base64 encode" — see
// docs/proposals/azure-discovery-slice1.md §8 for the verbatim 5-step
// list this file implements.
//
// Slice-1 honesty (chunk 4):
//   - The Wizard tab is the primary surface. The 5-step state
//     machine lives in this file (no factoring into a shared shell
//     yet — the GCP page deferred the shared-shell extraction as a
//     slice-2 candidate so a single chunk doesn't carry that
//     refactor on top of the new page).
//   - The Inventory tab renders the last successful scan response
//     for the selected connection. Scans are NOT persisted (matches
//     AWS + GCP slice-1 posture); a refresh clears the panel.
//   - The Recommendations tab is a stub. The proposer extension
//     (Provider="azure" path, vm-otel-tag kind, prompt extension)
//     ships in chunk 5 of this arc — until then the tab surfaces a
//     "ships in chunk 5" message so an operator who clicks through
//     during the chunk-4 → chunk-5 gap isn't stranded on an empty
//     panel.
//
// Token discipline: the pasted Service Principal client_secret lives
// in component state ONLY. It is base64-encoded into the
// createAzureConnection request body and then dropped from state on
// success. There is no reveal-toggle, no localStorage, no
// sessionStorage; an operator who refreshes the page loses the
// in-progress paste and has to re-paste from the `az ad sp
// create-for-rbac` output. Same posture as the GCP SA JSON paste
// and the GitHub PAT in IaCGitHubWizard.tsx.

import {
  AlertTriangle,
  CheckCircle2,
  ChevronLeft,
  Cloud,
  Copy,
  ExternalLink,
  Loader2,
} from "lucide-react";
import { useCallback, useState } from "react";
import useSWR from "swr";

import {
  createAzureConnection,
  encodeClientSecretForWire,
  listAzureConnections,
  scanAzureConnection,
  validateAzureConnection,
  type AzureConnection,
  type AzureValidateErrorKind,
  type ClusterSnapshot,
  type ComputeInstanceSnapshot,
  type DatabaseInstanceSnapshot,
  type ScanAzureResponse,
  type ServerlessRow,
  type OrchestrationRow,
  type EventSourceRow,
  type ValidateAzureResponse,
} from "@/api/discoveryAzure";
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
  AZURE_DOC_LINK,
  AZURE_LOCATION_REGEX,
  AZURE_RBAC_DOC_LINK,
  AZURE_SP_CREATE_CMD_TEMPLATE,
  AZURE_STEP_CREDENTIALS,
  AZURE_STEP_IDS,
  AZURE_STEP_SCAN,
  AZURE_STEP_SERVICE_PRINCIPAL,
  AZURE_STEP_SUBSCRIPTION,
  AZURE_STEP_TITLES,
  AZURE_STEP_VALIDATE,
  AZURE_UUID_REGEX,
  substituteSubscription,
  validateErrorRemediation,
} from "@/data/azureDiscoveryWizard";
import { relativeTime } from "@/lib/relativeTime";

// Tab values — stable string literals double as both the Radix Tabs
// `value` and the test selector key. Mirrors the GCP page's
// constants one-for-one.
const WIZARD_TAB = "wizard";
const INVENTORY_TAB = "inventory";
const RECS_TAB = "recommendations";

// Inventory sub-tab values — database tier slice 2 (v0.89.66, #695
// Stream 93) splits Inventory into Compute + Databases sub-tabs.
// The Databases sub-tab surfaces the Azure SQL inventory from the
// chunk 3 scanner extension with the SQLInsights diagnostic-setting
// instrumentation axis rendered per row.
//
// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100) adds a third
// Kubernetes sub-tab surfacing the AKS cluster inventory from the
// v0.89.70 chunk 3 AKS scanner extension with the Azure Monitor
// instrumentation axis rendered per row.
const INVENTORY_SUBTAB_COMPUTE = "compute";
const INVENTORY_SUBTAB_DATABASES = "databases";
const INVENTORY_SUBTAB_KUBERNETES = "kubernetes";
// Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) —
// fourth Inventory sub-tab carrying Azure Functions rows from the
// chunk 3 scanner extension. Same opt-in posture as the others.
const INVENTORY_SUBTAB_SERVERLESS = "serverless";
// Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) —
// fifth Inventory sub-tab carrying Azure Logic Apps rows from the
// chunk 3 Azure Logic Apps scanner extension.
const INVENTORY_SUBTAB_ORCHESTRATION = "orchestration";
// Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136) — all
// 4 providers render this sub-tab.
const INVENTORY_SUBTAB_EVENT_SOURCES = "event_sources";

// SWR_KEY_CONNECTIONS is the shared cache key the page reads and the
// wizard's onSave mutate() targets.
const SWR_KEY_CONNECTIONS = "/discovery/azure/connections";

export default function DiscoveryAzurePage() {
  // activeTab is controlled at the page level so the wizard's
  // "View Inventory" CTA after a successful scan can hop the
  // operator straight into the Inventory tab. Same controlled-tab
  // pattern as DiscoveryGCP / DiscoveryAWS.
  const [activeTab, setActiveTab] = useState<string>(WIZARD_TAB);

  // Selected connection drives the Inventory tab + the post-scan
  // result lookup. Empty when no connection is selected — the page
  // defaults to the wizard surface in that case.
  const [selectedConnectionID, setSelectedConnectionID] = useState<string>("");

  // scanResultByConn keeps the most-recent scan response per
  // connection ID in a single mutable map. Slice-1: in-memory only,
  // cleared on page refresh, matching the AWS / GCP posture. The map
  // keyed by connection ID (not just "the latest scan") so switching
  // the selector between two connections doesn't blow away the
  // other's result.
  const [scanResultByConn, setScanResultByConn] = useState<
    Record<string, ScanAzureResponse>
  >({});

  const { data: connections, mutate: mutateConnections } = useSWR(
    SWR_KEY_CONNECTIONS,
    () => listAzureConnections(),
  );

  const conns = connections ?? [];
  const hasConnections = conns.length > 0;

  const handleConnectionPicked = useCallback((id: string) => {
    setSelectedConnectionID(id);
    setActiveTab(INVENTORY_TAB);
  }, []);

  const handleWizardSuccess = useCallback(
    (conn: AzureConnection, scan: ScanAzureResponse) => {
      // Persist the scan result locally so the Inventory tab picks
      // it up on the tab swap below.
      setScanResultByConn((prev) => ({ ...prev, [conn.id]: scan }));
      setSelectedConnectionID(conn.id);
      // Refresh the SWR cache so the connection selector picks up
      // the new row.
      void mutateConnections();
      // Auto-switch to Inventory so the operator sees the result of
      // the work they just did. Matches the GCP scan-then-show-inventory
      // hop.
      setActiveTab(INVENTORY_TAB);
    },
    [mutateConnections],
  );

  const selectedScan = scanResultByConn[selectedConnectionID];

  return (
    <div className="space-y-4 p-6">
      <header>
        <div className="flex items-center gap-2">
          <Cloud className="h-5 w-5 text-sky-500" />
          <h1 className="text-2xl font-semibold">Azure Discovery</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Connect Azure subscriptions and discover what&apos;s uninstrumented.
        </p>
      </header>

      <ConnectionSelectorBar
        connections={conns}
        selectedID={selectedConnectionID}
        onSelect={handleConnectionPicked}
      />

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList>
          <TabsTrigger value={WIZARD_TAB}>Wizard</TabsTrigger>
          <TabsTrigger value={INVENTORY_TAB}>Inventory</TabsTrigger>
          <TabsTrigger value={RECS_TAB}>Recommendations</TabsTrigger>
        </TabsList>

        <TabsContent value={WIZARD_TAB} className="mt-4">
          <AzureWizard onComplete={handleWizardSuccess} />
        </TabsContent>
        <TabsContent value={INVENTORY_TAB} className="mt-4">
          <InventoryTab
            hasConnections={hasConnections}
            scan={selectedScan}
            onJumpToWizard={() => setActiveTab(WIZARD_TAB)}
          />
        </TabsContent>
        <TabsContent value={RECS_TAB} className="mt-4">
          <RecommendationsTab />
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
}: {
  connections: AzureConnection[];
  selectedID: string;
  onSelect: (id: string) => void;
}) {
  if (connections.length === 0) {
    return (
      <div className="rounded-md border border-dashed bg-muted/30 p-4 text-sm text-muted-foreground">
        No Azure subscriptions connected yet. Use the Wizard tab to connect one.
      </div>
    );
  }
  return (
    <div className="flex items-center gap-3">
      <Label
        htmlFor="azure-connection-select"
        className="text-xs uppercase tracking-wider text-muted-foreground"
      >
        Subscription
      </Label>
      <div className="w-72">
        <Select value={selectedID} onValueChange={onSelect}>
          <SelectTrigger
            id="azure-connection-select"
            aria-label="Azure connection selector"
          >
            <SelectValue placeholder="Select a subscription" />
          </SelectTrigger>
          <SelectContent>
            {connections.map((c) => (
              <SelectItem key={c.id} value={c.id}>
                {c.display_name} — {c.subscription_id}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </div>
  );
}

// --- Wizard ---------------------------------------------------------

interface AzureWizardProps {
  // onComplete fires after step 5 (scan) succeeds. The page uses it
  // to seed the Inventory tab and hop the operator over.
  onComplete: (conn: AzureConnection, scan: ScanAzureResponse) => void;
}

function AzureWizard({ onComplete }: AzureWizardProps) {
  const [stepIndex, setStepIndex] = useState(0);

  // Step-1 form state.
  const [displayName, setDisplayName] = useState("");
  const [tenantID, setTenantID] = useState("");
  const [subscriptionID, setSubscriptionID] = useState("");
  const [location, setLocation] = useState("");
  const [showWhyExplainer, setShowWhyExplainer] = useState(false);

  // Step-3 form state — pasted client_id + client_secret + ack
  // checkbox. The secret text lives in state for the duration of the
  // wizard; on a successful step-5 it gets dropped from state.
  // NEVER persisted to localStorage / cookies.
  const [clientID, setClientID] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [secretAcknowledged, setSecretAcknowledged] = useState(false);

  // Step-4 / step-5 in-flight + result state.
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [createdConnection, setCreatedConnection] =
    useState<AzureConnection | null>(null);
  const [validateResult, setValidateResult] =
    useState<ValidateAzureResponse | null>(null);
  const [scanResult, setScanResult] = useState<ScanAzureResponse | null>(null);

  const stepCount = AZURE_STEP_IDS.length;
  const currentStepID = AZURE_STEP_IDS[stepIndex];
  const isLastStep = stepIndex === stepCount - 1;

  // Step-1 field validation.
  const displayNameValid = displayName.trim() !== "";
  const tenantIDValid =
    tenantID !== "" && AZURE_UUID_REGEX.test(tenantID.trim());
  const subscriptionIDValid =
    subscriptionID !== "" && AZURE_UUID_REGEX.test(subscriptionID.trim());
  const locationValid =
    location.trim() === "" || AZURE_LOCATION_REGEX.test(location.trim());

  // Step-3 field validation.
  const clientIDValid = clientID !== "" && AZURE_UUID_REGEX.test(clientID.trim());
  const clientSecretValid = clientSecret.trim() !== "";

  // Next-enablement matrix per step. Mirrors the GCP / IaCGitHubWizard
  // pattern: a switch on currentStepID populates a single boolean the
  // global Next button reads.
  let nextEnabled = false;
  switch (currentStepID) {
    case AZURE_STEP_SUBSCRIPTION:
      nextEnabled =
        displayNameValid && tenantIDValid && subscriptionIDValid && locationValid;
      break;
    case AZURE_STEP_SERVICE_PRINCIPAL:
      // The SP-create step is read-only instructions — Next is
      // always advanceable.
      nextEnabled = true;
      break;
    case AZURE_STEP_CREDENTIALS:
      nextEnabled = clientIDValid && clientSecretValid && secretAcknowledged;
      break;
    case AZURE_STEP_VALIDATE:
      // Validate step's primary action is the Validate button — the
      // global Next only enables after a successful validate run.
      nextEnabled = validateResult?.ok === true;
      break;
    case AZURE_STEP_SCAN:
      // Scan step's primary action is the Scan button.
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

  // handleValidate — Step 4 primary action.
  //
  // Two-stage: createAzureConnection persists the row (encoded
  // client_secret gets credstore-sealed server-side), then
  // validateAzureConnection dry-runs a VirtualMachinesClient.NewListAllPager
  // first-page against the configured subscription. On a
  // permission_denied / subscription_not_found / tenant_invalid /
  // credentials_invalid / network failure the connection still exists
  // but is unhealthy; the operator gets a remediation-specific banner
  // and can re-validate after fixing the upstream state. (We do NOT
  // delete the connection on validation failure — partial setup beats
  // no setup, mirroring the GCP / IaCGitHubWizard two-stage submit.)
  const handleValidate = useCallback(async () => {
    if (submitting) return;
    if (!clientIDValid || !clientSecretValid) return;
    setSubmitting(true);
    setSubmitError(null);
    setValidateResult(null);
    try {
      // If we already created the connection on a prior validate
      // attempt, reuse it — don't create a duplicate row.
      let conn = createdConnection;
      if (!conn) {
        conn = await createAzureConnection({
          display_name: displayName.trim(),
          tenant_id: tenantID.trim(),
          subscription_id: subscriptionID.trim(),
          client_id: clientID.trim(),
          sealed_secret: encodeClientSecretForWire(clientSecret),
          location: location.trim(),
        });
        setCreatedConnection(conn);
      }
      const v = await validateAzureConnection(conn.id);
      setValidateResult(v);
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }, [
    submitting,
    clientIDValid,
    clientSecretValid,
    createdConnection,
    displayName,
    tenantID,
    subscriptionID,
    clientID,
    clientSecret,
    location,
  ]);

  // handleScan — Step 5 primary action.
  const handleScan = useCallback(async () => {
    if (submitting) return;
    if (!createdConnection) return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      const s = await scanAzureConnection(createdConnection.id);
      setScanResult(s);
      // Hand off to the page so the Inventory tab loads with the
      // result. Drop the pasted client_secret from state on success
      // — the wizard is done with it.
      onComplete(createdConnection, s);
      setClientSecret("");
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }, [submitting, createdConnection, onComplete]);

  return (
    <div className="space-y-6">
      <WizardHeader stepIndex={stepIndex} stepCount={stepCount} />

      <div className="rounded-lg border bg-card p-6">
        <h3 className="text-base font-semibold">
          {AZURE_STEP_TITLES[currentStepID]}
        </h3>

        <div className="mt-4">
          {currentStepID === AZURE_STEP_SUBSCRIPTION && (
            <SubscriptionStep
              displayName={displayName}
              onDisplayNameChange={setDisplayName}
              tenantID={tenantID}
              onTenantIDChange={setTenantID}
              subscriptionID={subscriptionID}
              onSubscriptionIDChange={setSubscriptionID}
              location={location}
              onLocationChange={setLocation}
              tenantIDValid={tenantIDValid}
              subscriptionIDValid={subscriptionIDValid}
              locationValid={locationValid}
              showWhyExplainer={showWhyExplainer}
              onToggleWhyExplainer={() => setShowWhyExplainer((v) => !v)}
            />
          )}

          {currentStepID === AZURE_STEP_SERVICE_PRINCIPAL && (
            <ServicePrincipalStep
              subscriptionID={subscriptionID}
              onCopy={handleCopy}
            />
          )}

          {currentStepID === AZURE_STEP_CREDENTIALS && (
            <CredentialsStep
              clientID={clientID}
              onClientIDChange={setClientID}
              clientSecret={clientSecret}
              onClientSecretChange={setClientSecret}
              clientIDValid={clientIDValid}
              clientSecretValid={clientSecretValid}
              acknowledged={secretAcknowledged}
              onAcknowledgeChange={setSecretAcknowledged}
            />
          )}

          {currentStepID === AZURE_STEP_VALIDATE && (
            <ValidateStep
              submitting={submitting}
              submitError={submitError}
              result={validateResult}
              connectionTenantID={tenantID}
              connectionSubscriptionID={subscriptionID}
              onValidate={handleValidate}
            />
          )}

          {currentStepID === AZURE_STEP_SCAN && (
            <ScanStep
              submitting={submitting}
              submitError={submitError}
              result={scanResult}
              onScan={handleScan}
            />
          )}
        </div>
      </div>

      <div className="flex items-center justify-between">
        <Button
          type="button"
          variant="ghost"
          onClick={handleBack}
          disabled={stepIndex === 0 || submitting}
        >
          <ChevronLeft className="mr-1 h-4 w-4" aria-hidden />
          Back
        </Button>
        {!isLastStep && (
          <Button
            type="button"
            onClick={handleNext}
            disabled={!nextEnabled || submitting}
          >
            Next
          </Button>
        )}
      </div>
    </div>
  );
}

// --- Wizard header / progress ---------------------------------------

function WizardHeader({
  stepIndex,
  stepCount,
}: {
  stepIndex: number;
  stepCount: number;
}) {
  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <span className="text-xs uppercase tracking-wider text-muted-foreground">
          Step {stepIndex + 1} of {stepCount}
        </span>
        <span className="text-xs text-muted-foreground">
          {AZURE_STEP_TITLES[AZURE_STEP_IDS[stepIndex]]}
        </span>
      </div>
      <div className="h-1 w-full rounded bg-muted">
        <div
          className="h-1 rounded bg-primary transition-all"
          style={{ width: `${((stepIndex + 1) / stepCount) * 100}%` }}
        />
      </div>
    </div>
  );
}

// --- Step 1: Connect an Azure subscription --------------------------

function SubscriptionStep({
  displayName,
  onDisplayNameChange,
  tenantID,
  onTenantIDChange,
  subscriptionID,
  onSubscriptionIDChange,
  location,
  onLocationChange,
  tenantIDValid,
  subscriptionIDValid,
  locationValid,
  showWhyExplainer,
  onToggleWhyExplainer,
}: {
  displayName: string;
  onDisplayNameChange: (v: string) => void;
  tenantID: string;
  onTenantIDChange: (v: string) => void;
  subscriptionID: string;
  onSubscriptionIDChange: (v: string) => void;
  location: string;
  onLocationChange: (v: string) => void;
  tenantIDValid: boolean;
  subscriptionIDValid: boolean;
  locationValid: boolean;
  showWhyExplainer: boolean;
  onToggleWhyExplainer: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Tell Squadron which Azure subscription to scan. We&apos;ll never write
        to it — the Reader role is the only RBAC role we&apos;ll ask the
        Service Principal to carry.
      </p>

      <div className="space-y-2">
        <Label htmlFor="azure-display-name">Display name</Label>
        <Input
          id="azure-display-name"
          value={displayName}
          onChange={(e) => onDisplayNameChange(e.target.value)}
          placeholder="Production Azure"
        />
        <p className="text-xs text-muted-foreground">
          A human-friendly label shown in the connection selector. Editable
          later.
        </p>
      </div>

      <div className="space-y-2">
        <Label htmlFor="azure-tenant-id">Tenant ID</Label>
        <Input
          id="azure-tenant-id"
          value={tenantID}
          onChange={(e) => onTenantIDChange(e.target.value)}
          placeholder="00000000-0000-0000-0000-000000000000"
          aria-invalid={tenantID !== "" && !tenantIDValid}
        />
        {tenantID !== "" && !tenantIDValid && (
          <p className="text-xs text-destructive">
            Tenant IDs must be a UUID (8-4-4-4-12 hex with hyphens). Find the
            value in the Azure portal under Azure Active Directory → Overview.
          </p>
        )}
        {tenantID === "" && (
          <p className="text-xs text-muted-foreground">
            The Azure AD tenant the Service Principal lives in. Visible in the
            portal at Azure Active Directory → Overview → Tenant ID.
          </p>
        )}
      </div>

      <div className="space-y-2">
        <Label htmlFor="azure-subscription-id">Subscription ID</Label>
        <Input
          id="azure-subscription-id"
          value={subscriptionID}
          onChange={(e) => onSubscriptionIDChange(e.target.value)}
          placeholder="00000000-0000-0000-0000-000000000000"
          aria-invalid={subscriptionID !== "" && !subscriptionIDValid}
        />
        {subscriptionID !== "" && !subscriptionIDValid && (
          <p className="text-xs text-destructive">
            Subscription IDs must be a UUID (8-4-4-4-12 hex with hyphens).
            Find the value in the Azure portal under Subscriptions.
          </p>
        )}
        {subscriptionID === "" && (
          <p className="text-xs text-muted-foreground">
            The Azure subscription to scan. Visible in the portal at
            Subscriptions → &lt;your subscription&gt; → Subscription ID.
          </p>
        )}
      </div>

      <div className="space-y-2">
        <Label htmlFor="azure-location">Location (optional)</Label>
        <Input
          id="azure-location"
          value={location}
          onChange={(e) => onLocationChange(e.target.value)}
          placeholder="eastus (leave empty to scan all locations)"
          aria-invalid={location !== "" && !locationValid}
        />
        {location !== "" && !locationValid && (
          <p className="text-xs text-destructive">
            That doesn&apos;t look like an Azure location name. Examples:
            eastus, westeurope, centralindia, francecentral.
          </p>
        )}
        {location === "" && (
          <p className="text-xs text-muted-foreground">
            Empty means &quot;scan every location the Service Principal can
            see.&quot; Pick a single location if your subscription spans
            multiple regions and you want to scope this scan.
          </p>
        )}
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
            Squadron walks your Azure Virtual Machines inventory and flags
            instances that lack the OpenTelemetry tag heuristic the proposer
            reads. The connection here is the credential + scope tuple Squadron
            uses to call the Azure Resource Manager API — nothing else. You
            can disconnect at any time; the sealed Service Principal secret is
            removed from the credstore on delete.
          </p>
          <p className="mt-2">
            <a
              href={AZURE_DOC_LINK}
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

// --- Step 2: Create the Service Principal ---------------------------

function ServicePrincipalStep({
  subscriptionID,
  onCopy,
}: {
  subscriptionID: string;
  onCopy: (v: string) => void;
}) {
  const createCmd = substituteSubscription(
    AZURE_SP_CREATE_CMD_TEMPLATE,
    subscriptionID,
  );
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Run this <code>az</code> CLI command in a shell where you&apos;re
        authenticated as a subscription owner. It creates a Service Principal
        named &quot;Squadron Discovery&quot; with the Reader role scoped to
        your subscription. Squadron never asks for anything more permissive
        than read.
      </p>
      <CommandBlock
        label="Create the Service Principal"
        cmd={createCmd}
        onCopy={() => onCopy(createCmd)}
      />
      <div className="rounded-md border bg-muted/30 p-3 text-xs text-muted-foreground">
        <p>
          The command outputs JSON with three fields you&apos;ll need on the
          next step:
        </p>
        <ul className="ml-4 mt-2 list-disc space-y-1">
          <li>
            <code>appId</code> is the <strong>Client ID</strong>.
          </li>
          <li>
            <code>password</code> is the <strong>Client Secret</strong>.
          </li>
          <li>
            <code>tenant</code> is the <strong>Tenant ID</strong> (you should
            have already entered it in Step 1).
          </li>
        </ul>
      </div>
      <p className="text-xs text-muted-foreground">
        <a
          href={AZURE_RBAC_DOC_LINK}
          target="_blank"
          rel="noreferrer"
          className="text-primary underline inline-flex items-center gap-1"
        >
          Learn more about the Reader role
          <ExternalLink className="h-3 w-3" aria-hidden />
        </a>
      </p>
    </div>
  );
}

// CommandBlock renders a monospace pre + Copy button for one az
// command. Single instance in this wizard (step 2) but factored out
// for parallelism with the GCP wizard's three uses.
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

// --- Step 3: Paste credentials --------------------------------------

function CredentialsStep({
  clientID,
  onClientIDChange,
  clientSecret,
  onClientSecretChange,
  clientIDValid,
  clientSecretValid,
  acknowledged,
  onAcknowledgeChange,
}: {
  clientID: string;
  onClientIDChange: (v: string) => void;
  clientSecret: string;
  onClientSecretChange: (v: string) => void;
  clientIDValid: boolean;
  clientSecretValid: boolean;
  acknowledged: boolean;
  onAcknowledgeChange: (v: boolean) => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Paste the <code>appId</code> and <code>password</code> values from
        Step 2&apos;s <code>az ad sp create-for-rbac</code> output.
      </p>

      <div className="space-y-2">
        <Label htmlFor="azure-client-id">Client ID (appId)</Label>
        <Input
          id="azure-client-id"
          value={clientID}
          onChange={(e) => onClientIDChange(e.target.value)}
          placeholder="00000000-0000-0000-0000-000000000000"
          aria-invalid={clientID !== "" && !clientIDValid}
          autoComplete="off"
          spellCheck={false}
        />
        {clientID !== "" && !clientIDValid && (
          <p className="text-xs text-destructive">
            Client IDs must be a UUID (8-4-4-4-12 hex with hyphens). Paste the
            <code> appId</code> field from the <code>az ad sp create-for-rbac</code>
            output, not the display name.
          </p>
        )}
      </div>

      <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-amber-900 dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-200">
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 h-4 w-4 flex-shrink-0" aria-hidden />
          <p className="text-xs">
            The Client Secret grants read access to your Azure VM inventory.
            Squadron seals it at rest with AES-GCM; the bytes never appear in
            audit payloads or logs. Treat this as a credential.
          </p>
        </div>
      </div>

      <div className="space-y-2">
        <Label htmlFor="azure-client-secret">Client Secret (password)</Label>
        <Textarea
          id="azure-client-secret"
          value={clientSecret}
          onChange={(e) => onClientSecretChange(e.target.value)}
          placeholder="Paste the password field from az ad sp create-for-rbac"
          rows={3}
          className="font-mono text-xs"
          aria-invalid={clientSecret !== "" && !clientSecretValid}
          autoComplete="off"
          spellCheck={false}
          data-1p-ignore
        />
        <p className="text-xs text-muted-foreground">
          The secret stays in browser memory until the wizard completes — it
          is base64-encoded over the wire and sealed at rest by Squadron.
        </p>
      </div>

      <div className="flex items-start gap-2">
        <Checkbox
          id="azure-secret-ack"
          checked={acknowledged}
          onCheckedChange={(v) => onAcknowledgeChange(v === true)}
          disabled={!clientIDValid || !clientSecretValid}
        />
        <Label
          htmlFor="azure-secret-ack"
          className="text-xs font-normal leading-tight text-muted-foreground"
        >
          I have stored this secret securely. Squadron seals it at rest, but
          the bytes are visible during paste.
        </Label>
      </div>
    </div>
  );
}

// --- Step 4: Validate -----------------------------------------------

function ValidateStep({
  submitting,
  submitError,
  result,
  connectionTenantID,
  connectionSubscriptionID,
  onValidate,
}: {
  submitting: boolean;
  submitError: string | null;
  result: ValidateAzureResponse | null;
  connectionTenantID: string;
  connectionSubscriptionID: string;
  onValidate: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Squadron will dry-run a VirtualMachines list against your subscription
        to confirm the Service Principal has the right scope. No data is
        persisted from this call — it&apos;s a confidence check.
      </p>
      <Button type="button" onClick={onValidate} disabled={submitting}>
        {submitting ? (
          <>
            <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
            Validating...
          </>
        ) : (
          "Validate connection"
        )}
      </Button>

      {submitError && (
        <div
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {submitError}
        </div>
      )}

      {result?.ok === true && (
        <div
          role="status"
          className="rounded-md border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-900 dark:border-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-200"
        >
          <div className="flex items-center gap-2">
            <CheckCircle2 className="h-4 w-4" aria-hidden />
            <span className="font-medium">
              Connected — {result.instance_count ?? 0} virtual machines visible.
            </span>
          </div>
          <p className="mt-1 text-xs">
            Click Next to run a full scan.
          </p>
        </div>
      )}

      {result?.ok === false && (
        <div
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          <div className="flex items-center gap-2 font-medium">
            <AlertTriangle className="h-4 w-4" aria-hidden />
            <span>Validation failed</span>
          </div>
          {result.message && (
            <p className="mt-1 text-xs">{result.message}</p>
          )}
          <p className="mt-2 text-xs">
            {validateErrorRemediation(
              result.error_kind as AzureValidateErrorKind,
              {
                connectionTenantID,
                connectionSubscriptionID,
              },
            )}
          </p>
        </div>
      )}
    </div>
  );
}

// --- Step 5: Scan ---------------------------------------------------

function ScanStep({
  submitting,
  submitError,
  result,
  onScan,
}: {
  submitting: boolean;
  submitError: string | null;
  result: ScanAzureResponse | null;
  onScan: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Walk Virtual Machines across the configured location and inventory
        every VM. Single-location per slice-1 — this can take a minute or two
        on large subscriptions.
      </p>
      <Button type="button" onClick={onScan} disabled={submitting}>
        {submitting ? (
          <>
            <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
            Scanning...
          </>
        ) : (
          "Run scan"
        )}
      </Button>

      {submitError && (
        <div
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {submitError}
        </div>
      )}

      {result && (
        <div
          role="status"
          className="rounded-md border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-900 dark:border-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-200"
        >
          <div className="flex items-center gap-2">
            <CheckCircle2 className="h-4 w-4" aria-hidden />
            <span className="font-medium">
              Scan complete: {result.compute.length} virtual machines (
              {result.instrumented_count} instrumented,{" "}
              {result.uninstrumented_count} uninstrumented). View Inventory →
            </span>
          </div>
          <p className="mt-1 text-xs">
            View the Inventory tab for per-VM detail.
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
  scan: ScanAzureResponse | undefined;
  onJumpToWizard: () => void;
}) {
  if (!hasConnections) {
    return (
      <div className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
        Connect an Azure subscription from the Wizard tab to populate inventory.
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
  // the Azure SQL inventory from the chunk 3 scanner extension.
  return (
    <div className="space-y-3">
      <InventorySummary scan={scan} />
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
          <TabsTrigger value={INVENTORY_SUBTAB_ORCHESTRATION}>
            Orchestration
          </TabsTrigger>
          <TabsTrigger value={INVENTORY_SUBTAB_EVENT_SOURCES}>
            Event sources
          </TabsTrigger>
        </TabsList>
        <TabsContent value={INVENTORY_SUBTAB_COMPUTE} className="mt-3">
          <InventoryTable rows={scan.compute} />
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
        <TabsContent value={INVENTORY_SUBTAB_ORCHESTRATION} className="mt-3">
          <OrchestrationInventoryTable rows={scan.orchestrations ?? []} />
        </TabsContent>
        <TabsContent value={INVENTORY_SUBTAB_EVENT_SOURCES} className="mt-3">
          <EventSourcesInventoryTable rows={scan.event_sources ?? []} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function InventorySummary({ scan }: { scan: ScanAzureResponse }) {
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-md border p-3 text-sm">
      <span>
        Subscription: <code className="text-xs">{scan.subscription_id}</code>
      </span>
      <span>Location: {scan.location || "all"}</span>
      <span>VMs: {scan.compute.length}</span>
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
        Scan completed but no virtual machines were returned. Either the
        subscription is empty or the scan was scoped to a location with no
        VMs.
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">VM Name</th>
            <th className="px-3 py-2 font-medium">Size</th>
            <th className="px-3 py-2 font-medium">OS</th>
            <th className="px-3 py-2 font-medium">Location</th>
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
              <td className="px-3 py-2 text-xs">{row.os_family || "-"}</td>
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
// Stream 93). Renders the Azure SQL inventory from the chunk 3
// scanner extension. The instrumentation column reads from
// sql_insights_diag_enabled (the Azure single-axis observability
// lever); rows where the field is undefined render "No" because
// absence is the uncovered signal per design doc §3.2.
function DatabaseInventoryTable({ rows }: { rows: DatabaseInstanceSnapshot[] }) {
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
            <th className="px-3 py-2 font-medium">Size</th>
            <th className="px-3 py-2 font-medium">SQLInsights routed?</th>
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
                {row.sql_insights_diag_enabled ? (
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
// Stream 100). Renders the AKS cluster inventory from the v0.89.70
// chunk 3 scanner extension. The instrumentation column reads from
// azure_monitor_enabled (the Azure single-axis observability lever,
// itself the three-way disjunction across omsagent / metrics /
// containerInsights resolved scanner-side); rows where the field is
// undefined render "No" because absence is the uncovered signal per
// design doc §3.2.
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
            <th className="px-3 py-2 font-medium">Azure Monitor?</th>
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
              <td className="px-3 py-2 text-xs">{row.kubernetes_version || "-"}</td>
              <td className="px-3 py-2 text-xs">{row.status || "-"}</td>
              <td className="px-3 py-2 text-xs">
                {row.azure_monitor_enabled ? (
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
// (v0.89.92, #725 Stream 123). Renders the per-row Azure Functions
// inventory the chunk 3 Azure scanner extension produced. Same column
// shape as the GCP / OCI Serverless sub-tabs: Resource Name, Surface
// (always "azfunc" on Azure), Runtime, Region, Trace axis (App
// Insights connection string set?), OTel distro (OTEL_DOTNET_AUTO_HOME
// / OTEL_PYTHON_DISTRO set?), Last seen.
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
            {/* Cold-start latency analysis slice 1 chunk 3 (v0.89.115,
                #753 Stream 151) — mirrored column on Azure Serverless
                table; slice 1 covers AWS Lambda only, so every row
                renders "—" here. Slice 2 will populate Azure
                Functions cold-start observations. */}
            <th className="px-3 py-2 font-medium">Cold-start P95 (24h)</th>
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
                <span
                  className="text-muted-foreground"
                  title="Cold-start observation not available on this surface (AWS-only in slice 1)"
                  data-testid="cold-start-cell"
                  data-value="none"
                >
                  —
                </span>
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
// (v0.89.97, #731 Stream 129). Renders the per-row Azure Logic Apps
// inventory the chunk 3 scanner extension produced. Columns per §7:
// Resource Name, Surface, Type, Region, Trace axis, Log axis, Last
// seen. Empty state mirrors the Serverless sub-tab.
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
// (v0.89.102, #738 Stream 136). Renders the per-row Service Bus
// inventory the chunk 3 Azure scanner produced. Columns follow §7 of
// the design doc: Resource Name, Surface, Type, Region, Trace axis,
// Log axis, Last seen. The Quality column is AWS-only per the slice 1
// constraint mirroring v0.89.92 / v0.89.97.
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
            {state
              ? `${state.row.surface} · ${state.row.resource_name}`
              : ""}
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

function RecommendationsTab() {
  return (
    <div className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
      <p>
        Recommendations are pending — proposer integration ships in chunk 5 of
        this arc.
      </p>
      <p className="mt-2 text-xs">
        Chunk 5 extends the discovery proposer with the Provider=&quot;azure&quot;
        path and the vm-otel-tag recommendation kind, then wires this tab to
        the same generate-recommendations flow the AWS / GCP pages use.
      </p>
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
