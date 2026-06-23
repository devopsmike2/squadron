// DiscoveryGCP — the v0.89.48 #670 Stream 68 (slice-1 chunk 4) GCP
// discovery page. Parallels DiscoveryAWS.tsx in tab structure
// (Wizard / Inventory / Recommendations) and operator UX, but the
// step bodies differ because the GCP credential model is "SA JSON
// paste + base64 encode" rather than "AssumeRole trust policy" — see
// docs/proposals/gcp-discovery-slice1.md §7 for the verbatim 5-step
// list this file implements.
//
// Slice-1 honesty (chunk 4):
//   - The Wizard tab is the primary surface. The 5-step state
//     machine lives in this file (no factoring into a shared shell
//     yet — design doc §7 calls out shared-shell extraction as a
//     slice-2 candidate so a single chunk doesn't carry that
//     refactor on top of the new page).
//   - The Inventory tab renders the last successful scan response
//     for the selected connection. Scans are NOT persisted (matches
//     AWS slice-1 posture); a refresh clears the panel.
//   - The Recommendations tab is a stub. The proposer extension
//     (Provider field on DiscoveryScanContext, gce-otel-label kind,
//     prompt extension) ships in chunk 5 of this arc — until then
//     the tab surfaces a "ships in chunk 5" message so an operator
//     who clicks through during the chunk-4 → chunk-5 gap isn't
//     stranded on an empty panel.
//
// Token discipline: the pasted service-account JSON lives in
// component state ONLY. It is base64-encoded into the createGCPConnection
// request body and then dropped from state on success. There is no
// reveal-toggle, no localStorage, no sessionStorage; an operator who
// refreshes the page loses the in-progress paste and has to re-paste
// from key.json. Same posture as the GitHub PAT in IaCGitHubWizard.tsx.

import {
  AlertTriangle,
  CheckCircle2,
  ChevronLeft,
  Cloud,
  Copy,
  ExternalLink,
  Loader2,
} from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import useSWR from "swr";

import {
  createGCPConnection,
  encodeServiceAccountForWire,
  listGCPConnections,
  scanGCPConnection,
  validateGCPConnection,
  type ClusterSnapshot,
  type ComputeInstanceSnapshot,
  type DatabaseInstanceSnapshot,
  type GCPConnection,
  type GCPValidateErrorKind,
  type ScanGCPResponse,
  type ValidateGCPResponse,
} from "@/api/discoveryGCP";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
  GCP_DOC_LINK,
  GCP_IAM_DOC_LINK,
  GCP_PROJECT_ID_REGEX,
  GCP_REGION_REGEX,
  GCP_SA_CREATE_CMD_TEMPLATE,
  GCP_SA_KEY_CREATE_CMD_TEMPLATE,
  GCP_SA_ROLE_BIND_CMD_TEMPLATE,
  GCP_STEP_IDS,
  GCP_STEP_KEY_PASTE,
  GCP_STEP_PROJECT,
  GCP_STEP_SCAN,
  GCP_STEP_SERVICE_ACCOUNT,
  GCP_STEP_TITLES,
  GCP_STEP_VALIDATE,
  parseServiceAccount,
  substituteProject,
  validateErrorRemediation,
  type ParsedServiceAccount,
} from "@/data/gcpDiscoveryWizard";
import { relativeTime } from "@/lib/relativeTime";

// Tab values — stable string literals double as both the Radix Tabs
// `value` and the test selector key.
const WIZARD_TAB = "wizard";
const INVENTORY_TAB = "inventory";
const RECS_TAB = "recommendations";

// Inventory sub-tab values — database tier slice 2 (v0.89.66, #695
// Stream 93) splits Inventory into Compute + Databases sub-tabs.
// Default sub-tab is Compute so the slice-1 UX is preserved; the
// Databases sub-tab surfaces the Cloud SQL inventory from the chunk
// 2 scanner extension with the Query Insights instrumentation axis
// rendered per row.
//
// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100) adds a third
// Kubernetes sub-tab surfacing the GKE cluster inventory from the
// v0.89.70 chunk 2 GKE scanner extension with the Managed
// Prometheus instrumentation axis rendered per row.
const INVENTORY_SUBTAB_COMPUTE = "compute";
const INVENTORY_SUBTAB_DATABASES = "databases";
const INVENTORY_SUBTAB_KUBERNETES = "kubernetes";

// SWR_KEY_CONNECTIONS is the shared cache key the page reads and the
// wizard's onSave mutate() targets.
const SWR_KEY_CONNECTIONS = "/discovery/gcp/connections";

export default function DiscoveryGCPPage() {
  // activeTab is controlled at the page level so the wizard's
  // "View Inventory" CTA after a successful scan can hop the
  // operator straight into the Inventory tab. Same controlled-tab
  // pattern as DiscoveryAWS.tsx.
  const [activeTab, setActiveTab] = useState<string>(WIZARD_TAB);

  // Selected connection drives the Inventory tab + the post-scan
  // result lookup. Null when no connection is selected — the page
  // defaults to the wizard surface in that case.
  const [selectedConnectionID, setSelectedConnectionID] = useState<string>("");

  // scanResultByConn keeps the most-recent scan response per
  // connection ID in a single mutable map. Slice-1: in-memory only,
  // cleared on page refresh, matching the AWS posture. The map keyed
  // by connection ID (not just "the latest scan") so switching the
  // selector between two connections doesn't blow away the other's
  // result.
  const [scanResultByConn, setScanResultByConn] = useState<
    Record<string, ScanGCPResponse>
  >({});

  const { data: connections, mutate: mutateConnections } = useSWR(
    SWR_KEY_CONNECTIONS,
    () => listGCPConnections(),
  );

  // Default tab routing: when connections exist and one is selected,
  // land on Inventory; when no connections exist, land on Wizard.
  // The selector below seeds selectedConnectionID once the list loads.
  const conns = connections ?? [];
  const hasConnections = conns.length > 0;

  const handleConnectionPicked = useCallback((id: string) => {
    setSelectedConnectionID(id);
    setActiveTab(INVENTORY_TAB);
  }, []);

  const handleWizardSuccess = useCallback(
    (conn: GCPConnection, scan: ScanGCPResponse) => {
      // Persist the scan result locally so the Inventory tab picks
      // it up on the tab swap below.
      setScanResultByConn((prev) => ({ ...prev, [conn.id]: scan }));
      setSelectedConnectionID(conn.id);
      // Refresh the SWR cache so the connection selector picks up
      // the new row.
      void mutateConnections();
      // Auto-switch to Inventory so the operator sees the result of
      // the work they just did. Matches the AWS scan-then-show-recs
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
          <Cloud className="h-5 w-5 text-violet-500" />
          <h1 className="text-2xl font-semibold">GCP Discovery</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Connect GCP projects and discover what&apos;s uninstrumented.
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
          <GCPWizard onComplete={handleWizardSuccess} />
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
  connections: GCPConnection[];
  selectedID: string;
  onSelect: (id: string) => void;
}) {
  if (connections.length === 0) {
    return (
      <div className="rounded-md border border-dashed bg-muted/30 p-4 text-sm text-muted-foreground">
        No GCP projects connected yet. Use the Wizard tab to connect one.
      </div>
    );
  }
  return (
    <div className="flex items-center gap-3">
      <Label htmlFor="gcp-connection-select" className="text-xs uppercase tracking-wider text-muted-foreground">
        Project
      </Label>
      <div className="w-72">
        <Select value={selectedID} onValueChange={onSelect}>
          <SelectTrigger
            id="gcp-connection-select"
            aria-label="GCP connection selector"
          >
            <SelectValue placeholder="Select a project" />
          </SelectTrigger>
          <SelectContent>
            {connections.map((c) => (
              <SelectItem key={c.id} value={c.id}>
                {c.display_name} — {c.project_id}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </div>
  );
}

// --- Wizard ---------------------------------------------------------

interface GCPWizardProps {
  // onComplete fires after step 5 (scan) succeeds. The page uses it
  // to seed the Inventory tab and hop the operator over.
  onComplete: (conn: GCPConnection, scan: ScanGCPResponse) => void;
}

function GCPWizard({ onComplete }: GCPWizardProps) {
  const [stepIndex, setStepIndex] = useState(0);

  // Step-1 form state.
  const [displayName, setDisplayName] = useState("");
  const [projectID, setProjectID] = useState("");
  const [region, setRegion] = useState("");
  const [showWhyExplainer, setShowWhyExplainer] = useState(false);

  // Step-3 form state — pasted SA JSON + parsed projection +
  // acknowledgment checkbox. The textarea text lives in state for
  // the duration of the wizard; on a successful step-4 it gets
  // dropped from state. NEVER persisted to localStorage / cookies.
  const [saText, setSAText] = useState("");
  const [saAcknowledged, setSAAcknowledged] = useState(false);

  // Step-4 / step-5 in-flight + result state.
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [createdConnection, setCreatedConnection] =
    useState<GCPConnection | null>(null);
  const [validateResult, setValidateResult] =
    useState<ValidateGCPResponse | null>(null);
  const [scanResult, setScanResult] = useState<ScanGCPResponse | null>(null);

  const stepCount = GCP_STEP_IDS.length;
  const currentStepID = GCP_STEP_IDS[stepIndex];
  const isLastStep = stepIndex === stepCount - 1;

  // Parsed SA — re-runs on every keystroke. Cheap (a JSON.parse on
  // ~2KB) so no memoization needed; useMemo would just trade a
  // dependency-array bug surface for a micro-optimization.
  const saParse = useMemo(() => parseServiceAccount(saText), [saText]);
  const saValid = saParse.ok;
  const saParsed: ParsedServiceAccount | null = saParse.ok ? saParse.sa : null;
  const saError = !saParse.ok ? saParse.err.message : null;

  // Step-1 field validation. project_id is required + regex-checked;
  // region is optional but, when non-empty, must match the GCP
  // region-naming shape.
  const projectIDValid =
    projectID !== "" && GCP_PROJECT_ID_REGEX.test(projectID);
  const regionValid = region === "" || GCP_REGION_REGEX.test(region);
  const displayNameValid = displayName.trim() !== "";

  // Next-enablement matrix per step. Mirrors the IaCGitHubWizard
  // pattern: a switch on currentStepID populates a single boolean
  // the global Next button reads.
  let nextEnabled = false;
  switch (currentStepID) {
    case GCP_STEP_PROJECT:
      nextEnabled = displayNameValid && projectIDValid && regionValid;
      break;
    case GCP_STEP_SERVICE_ACCOUNT:
      // The SA-create + role-binding step is read-only instructions
      // — Next is always advanceable.
      nextEnabled = true;
      break;
    case GCP_STEP_KEY_PASTE:
      nextEnabled = saValid && saAcknowledged;
      break;
    case GCP_STEP_VALIDATE:
      // Validate step's primary action is the Validate button — the
      // global Next only enables after a successful validate run.
      nextEnabled = validateResult?.ok === true;
      break;
    case GCP_STEP_SCAN:
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
  // Two-stage: createGCPConnection persists the row (encoded SA JSON
  // gets credstore-sealed server-side), then validateGCPConnection
  // dry-runs a compute.instances.list against the configured region.
  // On a permission_denied / project_not_found / network failure the
  // connection still exists but is unhealthy; the operator gets a
  // remediation-specific banner and can re-validate after fixing the
  // upstream IAM / project / network state. (We do NOT delete the
  // connection on validation failure — partial setup beats no setup,
  // mirroring the IaCGitHubWizard two-stage submit.)
  const handleValidate = useCallback(async () => {
    if (submitting) return;
    if (!saParsed) return;
    setSubmitting(true);
    setSubmitError(null);
    setValidateResult(null);
    try {
      // If we already created the connection on a prior validate
      // attempt, reuse it — don't create a duplicate row.
      let conn = createdConnection;
      if (!conn) {
        conn = await createGCPConnection({
          display_name: displayName.trim(),
          project_id: projectID,
          sealed_sa: encodeServiceAccountForWire(saText),
          region: region.trim(),
        });
        setCreatedConnection(conn);
      }
      const v = await validateGCPConnection(conn.id);
      setValidateResult(v);
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }, [
    submitting,
    saParsed,
    createdConnection,
    displayName,
    projectID,
    saText,
    region,
  ]);

  // handleScan — Step 5 primary action.
  const handleScan = useCallback(async () => {
    if (submitting) return;
    if (!createdConnection) return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      const s = await scanGCPConnection(createdConnection.id);
      setScanResult(s);
      // Hand off to the page so the Inventory tab loads with the
      // result. Drop the pasted SA text from state on success — the
      // wizard is done with it.
      onComplete(createdConnection, s);
      setSAText("");
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
          {GCP_STEP_TITLES[currentStepID]}
        </h3>

        <div className="mt-4">
          {currentStepID === GCP_STEP_PROJECT && (
            <ProjectStep
              displayName={displayName}
              onDisplayNameChange={setDisplayName}
              projectID={projectID}
              onProjectIDChange={setProjectID}
              region={region}
              onRegionChange={setRegion}
              projectIDValid={projectIDValid}
              regionValid={regionValid}
              showWhyExplainer={showWhyExplainer}
              onToggleWhyExplainer={() => setShowWhyExplainer((v) => !v)}
            />
          )}

          {currentStepID === GCP_STEP_SERVICE_ACCOUNT && (
            <ServiceAccountStep
              projectID={projectID}
              onCopy={handleCopy}
            />
          )}

          {currentStepID === GCP_STEP_KEY_PASTE && (
            <KeyPasteStep
              projectID={projectID}
              saText={saText}
              onSATextChange={setSAText}
              saValid={saValid}
              saError={saError}
              acknowledged={saAcknowledged}
              onAcknowledgeChange={setSAAcknowledged}
              onCopy={handleCopy}
            />
          )}

          {currentStepID === GCP_STEP_VALIDATE && (
            <ValidateStep
              submitting={submitting}
              submitError={submitError}
              result={validateResult}
              connectionProjectID={projectID}
              saProjectID={saParsed?.project_id ?? ""}
              onValidate={handleValidate}
            />
          )}

          {currentStepID === GCP_STEP_SCAN && (
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
          {GCP_STEP_TITLES[GCP_STEP_IDS[stepIndex]]}
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

// --- Step 1: Connect a GCP project ----------------------------------

function ProjectStep({
  displayName,
  onDisplayNameChange,
  projectID,
  onProjectIDChange,
  region,
  onRegionChange,
  projectIDValid,
  regionValid,
  showWhyExplainer,
  onToggleWhyExplainer,
}: {
  displayName: string;
  onDisplayNameChange: (v: string) => void;
  projectID: string;
  onProjectIDChange: (v: string) => void;
  region: string;
  onRegionChange: (v: string) => void;
  projectIDValid: boolean;
  regionValid: boolean;
  showWhyExplainer: boolean;
  onToggleWhyExplainer: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Tell Squadron which GCP project to scan. We&apos;ll never write to it —
        compute.viewer is the only role we&apos;ll ask the service account to
        carry.
      </p>

      <div className="space-y-2">
        <Label htmlFor="gcp-display-name">Display name</Label>
        <Input
          id="gcp-display-name"
          value={displayName}
          onChange={(e) => onDisplayNameChange(e.target.value)}
          placeholder="Production GCP"
        />
        <p className="text-xs text-muted-foreground">
          A human-friendly label shown in the connection selector. Editable
          later.
        </p>
      </div>

      <div className="space-y-2">
        <Label htmlFor="gcp-project-id">Project ID</Label>
        <Input
          id="gcp-project-id"
          value={projectID}
          onChange={(e) => onProjectIDChange(e.target.value)}
          placeholder="my-production-project"
          aria-invalid={projectID !== "" && !projectIDValid}
        />
        {projectID !== "" && !projectIDValid && (
          <p className="text-xs text-destructive">
            Project IDs must be 6 to 30 characters, lowercase letters, digits,
            or hyphens, starting with a letter and not ending with a hyphen.
          </p>
        )}
        {projectID === "" && (
          <p className="text-xs text-muted-foreground">
            The project ID (not the project number or display name). Visible in
            the GCP console URL: console.cloud.google.com/?project=&lt;id&gt;.
          </p>
        )}
      </div>

      <div className="space-y-2">
        <Label htmlFor="gcp-region">Region (optional)</Label>
        <Input
          id="gcp-region"
          value={region}
          onChange={(e) => onRegionChange(e.target.value)}
          placeholder="us-central1 (leave empty to scan all regions)"
          aria-invalid={region !== "" && !regionValid}
        />
        {region !== "" && !regionValid && (
          <p className="text-xs text-destructive">
            That doesn&apos;t look like a GCP region name. Examples:
            us-central1, europe-west4, asia-northeast3.
          </p>
        )}
        {region === "" && (
          <p className="text-xs text-muted-foreground">
            Empty means &quot;scan every region the service account can see.&quot;
            Slice 1 ships single-region per scan; pick one region if your
            project is large.
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
            Squadron walks your Compute Engine inventory and flags instances
            that lack the OpenTelemetry label heuristic the proposer reads.
            The connection here is the credential + scope tuple Squadron uses
            to call compute.instances.list — nothing else. You can disconnect
            at any time; the sealed SA JSON is removed from the credstore on
            delete.
          </p>
          <p className="mt-2">
            <a
              href={GCP_DOC_LINK}
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

// --- Step 2: Create a Service Account -------------------------------

function ServiceAccountStep({
  projectID,
  onCopy,
}: {
  projectID: string;
  onCopy: (v: string) => void;
}) {
  const createCmd = substituteProject(GCP_SA_CREATE_CMD_TEMPLATE, projectID);
  const bindCmd = substituteProject(GCP_SA_ROLE_BIND_CMD_TEMPLATE, projectID);
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Run these two gcloud commands in a shell where you&apos;re authenticated
        as a project owner. The first creates the service account; the second
        grants it compute.viewer. Squadron never asks for anything more
        permissive than read.
      </p>
      <CommandBlock
        label="Create the service account"
        cmd={createCmd}
        onCopy={() => onCopy(createCmd)}
      />
      <CommandBlock
        label="Grant compute.viewer"
        cmd={bindCmd}
        onCopy={() => onCopy(bindCmd)}
      />
      <p className="text-xs text-muted-foreground">
        <a
          href={GCP_IAM_DOC_LINK}
          target="_blank"
          rel="noreferrer"
          className="text-primary underline inline-flex items-center gap-1"
        >
          Learn more about roles/compute.viewer
          <ExternalLink className="h-3 w-3" aria-hidden />
        </a>
      </p>
    </div>
  );
}

// CommandBlock renders a monospace pre + Copy button for one gcloud
// command. Used three times by the wizard (steps 2 and 3) so factoring
// it out keeps the rendered HTML uniform.
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

// --- Step 3: Download key and paste --------------------------------

function KeyPasteStep({
  projectID,
  saText,
  onSATextChange,
  saValid,
  saError,
  acknowledged,
  onAcknowledgeChange,
  onCopy,
}: {
  projectID: string;
  saText: string;
  onSATextChange: (v: string) => void;
  saValid: boolean;
  saError: string | null;
  acknowledged: boolean;
  onAcknowledgeChange: (v: boolean) => void;
  onCopy: (v: string) => void;
}) {
  const keyCmd = substituteProject(GCP_SA_KEY_CREATE_CMD_TEMPLATE, projectID);
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Generate a key for the service account from step 2, then paste the
        contents of <code>key.json</code> below.
      </p>
      <CommandBlock
        label="Generate the key"
        cmd={keyCmd}
        onCopy={() => onCopy(keyCmd)}
      />

      <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-amber-900 dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-200">
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 h-4 w-4 flex-shrink-0" aria-hidden />
          <p className="text-xs">
            This key grants read access to your GCE inventory. Squadron seals it
            at rest with AES-GCM; the bytes never appear in audit payloads or
            logs. Treat this as a credential.
          </p>
        </div>
      </div>

      <div className="space-y-2">
        <Label htmlFor="gcp-sa-json">Service-account key (key.json)</Label>
        <Textarea
          id="gcp-sa-json"
          value={saText}
          onChange={(e) => onSATextChange(e.target.value)}
          placeholder='{ "type": "service_account", "project_id": "...", "client_email": "...", "private_key": "..." }'
          rows={10}
          className="font-mono text-xs"
          aria-invalid={saText !== "" && !saValid}
          autoComplete="off"
          spellCheck={false}
          data-1p-ignore
        />
        {saText !== "" && !saValid && saError && (
          <p className="text-xs text-destructive">{saError}</p>
        )}
        {saValid && (
          <p className="inline-flex items-center gap-1 text-xs text-emerald-600 dark:text-emerald-400">
            <CheckCircle2 className="h-3 w-3" aria-hidden />
            Parsed successfully.
          </p>
        )}
      </div>

      <div className="flex items-start gap-2">
        <Checkbox
          id="gcp-sa-ack"
          checked={acknowledged}
          onCheckedChange={(v) => onAcknowledgeChange(v === true)}
          disabled={!saValid}
        />
        <Label
          htmlFor="gcp-sa-ack"
          className="text-xs font-normal leading-tight text-muted-foreground"
        >
          I have read this warning and understand that the pasted key is a
          credential.
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
  connectionProjectID,
  saProjectID,
  onValidate,
}: {
  submitting: boolean;
  submitError: string | null;
  result: ValidateGCPResponse | null;
  connectionProjectID: string;
  saProjectID: string;
  onValidate: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Squadron will dry-run a compute.instances.list against your project to
        confirm the service account has the right scopes. No data is persisted
        from this call — it&apos;s a confidence check.
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
              Connected — {result.instance_count ?? 0} instances visible.
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
            {validateErrorRemediation(result.error_kind as GCPValidateErrorKind, {
              connectionProjectID,
              saProjectID,
            })}
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
  result: ScanGCPResponse | null;
  onScan: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Walk Compute Engine across the configured region and inventory every
        instance. Single-region per slice-1 — this can take a minute or two on
        large projects.
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
              Scan complete: {result.compute.length} instances ({result.instrumented_count} instrumented, {result.uninstrumented_count} uninstrumented).
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
  scan: ScanGCPResponse | undefined;
  onJumpToWizard: () => void;
}) {
  if (!hasConnections) {
    return (
      <div className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
        Connect a GCP project from the Wizard tab to populate inventory.
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
  // Database tier slice 2 (v0.89.66, #695 Stream 93) — Inventory
  // splits into Compute (existing slice 1 table) and Databases
  // (the Cloud SQL inventory from the chunk 2 scanner extension).
  // Default sub-tab is Compute so existing UX is preserved; the
  // operator opts into Databases explicitly.
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
      </Tabs>
    </div>
  );
}

function InventorySummary({ scan }: { scan: ScanGCPResponse }) {
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-md border p-3 text-sm">
      <span>
        Project: <code className="text-xs">{scan.project_id}</code>
      </span>
      <span>Region: {scan.region || "all"}</span>
      <span>Instances: {scan.compute.length}</span>
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
        Scan completed but no instances were returned. Either the project is
        empty or the scan was scoped to a region with no instances.
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">Resource ID</th>
            <th className="px-3 py-2 font-medium">Instance Type</th>
            <th className="px-3 py-2 font-medium">OS Family</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">OTel?</th>
            <th className="px-3 py-2 font-medium">Last seen</th>
            <th className="px-3 py-2 font-medium">Labels</th>
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
// Stream 93). Renders the Cloud SQL inventory from the chunk 2
// scanner extension. The instrumentation column reads from
// query_insights_enabled (the GCP single-axis observability lever);
// rows where the field is undefined render "No" because absence is
// the uncovered signal per design doc §3.1.
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
            <th className="px-3 py-2 font-medium">Instance Class</th>
            <th className="px-3 py-2 font-medium">Query Insights enabled?</th>
            <th className="px-3 py-2 font-medium">Last seen</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">Labels</th>
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
                {row.query_insights_enabled ? (
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
// Stream 100). Renders the GKE cluster inventory from the v0.89.70
// chunk 2 scanner extension. The instrumentation column reads from
// managed_prometheus_enabled (the GCP single-axis observability
// lever); rows where the field is undefined render "No" because
// absence is the uncovered signal per design doc §3.1.
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
            <th className="px-3 py-2 font-medium">Managed Prometheus?</th>
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
                {row.managed_prometheus_enabled ? (
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

// --- Recommendations tab --------------------------------------------

function RecommendationsTab() {
  return (
    <div className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
      <p>
        Recommendations are pending — proposer integration ships in chunk 5 of
        this arc.
      </p>
      <p className="mt-2 text-xs">
        Chunk 5 extends the discovery proposer with a Provider discriminator
        and the gce-otel-label recommendation kind, then wires this tab to the
        same generate-recommendations flow the AWS page uses.
      </p>
    </div>
  );
}

// LastSeenCell — v0.89.77 trace integration slice 1 chunk 4. Renders
// the per-row Last seen value as a compact relative time string,
// falling back to "never" with an amber warning icon when the
// traceindex has no observation for the projected resource key.
// Shared across Compute / Databases / Kubernetes sub-tabs.
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
