// IaCGitHubWizard — the v0.89.3 #603 Stream 19 Connect-IaC-repo
// wizard. Six steps, mirroring the AWS connector wizard's shell shape
// (progress bar / Card body / Back-Next footer) but with step bodies
// the AWS wizard's declarative ConnectorWizard shell cannot express:
//   1. Provider — single-select GitHub (GitLab / Bitbucket tiles
//      ship disabled so the slice-2 plan is visible from day one).
//   2. Authentication — PAT for slice 1, GitHub App disabled.
//      Captures `token` into component state only — NEVER localStorage,
//      NEVER sessionStorage, NEVER any persisted client storage.
//   3. Repo — free-text owner/repo input with regex format check.
//      No autocomplete via the GitHub API in slice 1; named in
//      non-goals.
//   4. Layout + branch + Squadron settings — repo_layout toggle
//      (mono | multi), default_branch (auto-detected on validate),
//      optional branch_prefix and reviewer_team_handle behind an
//      Advanced disclosure.
//   5. Placement map — seven rows pre-populated from the slice-1
//      canonical resource_kind list. Per-row file_path input,
//      placeholder examples flip based on repo_layout. Per-row Skip
//      toggle + bulk Skip All.
//   6. Validate + Save — calls /iac/github/validate, renders the
//      preflight-row panel (one row per resource_kind, ✓ / ✗ / ⊘),
//      then enables Save. On Save success, renders a Connected card.
//
// Token discipline. The PAT lives in component state for the lifetime
// of the wizard. It is sent on validate / save only. There is no
// reveal-toggle (the field is a plain password input with
// autocomplete="off"); the operator can re-paste if needed.
// `data-1p-ignore` opts password managers out of suggesting saves.
//
// Symmetry with the AWS wizard's shell. Where the AWS shell uses a
// declarative ConnectorWizard def + step renderers, this wizard uses a
// stepIndex state machine with explicit step bodies. Reason: the
// placement-map step needs per-row state the declarative shell would
// have to grow a new action kind to represent. Documented as a
// known divergence; if slice 2 grows more wizard surfaces, the right
// move is to factor the shell's progress bar / Back-Next chrome into
// a thin `<WizardShell>` both can mount.

import {
  AlertCircle,
  CheckCircle2,
  ChevronLeft,
  Copy,
  ExternalLink,
  Github,
  Loader2,
  MinusCircle,
  XCircle,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  type IaCGitHubPreflightRow,
  type IaCGitHubValidateResponse,
  type IaCHumanizedError,
  type IaCPlacementEntry,
  saveIaCGitHubConnection,
  updateIaCGitHubPlacementMap,
  validateIaCGitHub,
} from "@/api/iacGithub";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  DEFAULT_BRANCH_PREFIX,
  GITHUB_CREATE_PAT_URL,
  IAC_GITHUB_PLACEMENT_KINDS,
  REPO_FULL_NAME_RE,
} from "@/data/iacGithubWizard";
import { cn } from "@/lib/utils";

// STEP_IDS — stable string keys for each wizard step. Used as the
// jump-back targets for HumanizedError.suggested_step values: the
// server's iac_github.go handler uses `pat` / `pick-repo` /
// `placement-map` / `validate` / `save` strings; this client uses the
// same identifiers so the jump-back is one-hop.
const STEP_PROVIDER = "provider";
const STEP_PAT = "pat";
const STEP_PICK_REPO = "pick-repo";
const STEP_REPO_LAYOUT = "repo-layout";
const STEP_PLACEMENT_MAP = "placement-map";
const STEP_VALIDATE = "validate";

const STEP_IDS = [
  STEP_PROVIDER,
  STEP_PAT,
  STEP_PICK_REPO,
  STEP_REPO_LAYOUT,
  STEP_PLACEMENT_MAP,
  STEP_VALIDATE,
] as const;

const STEP_TITLES: Record<string, string> = {
  [STEP_PROVIDER]: "Pick an IaC provider",
  [STEP_PAT]: "Authenticate with GitHub",
  [STEP_PICK_REPO]: "Choose the repository",
  [STEP_REPO_LAYOUT]: "Describe the repository",
  [STEP_PLACEMENT_MAP]: "Map resource kinds to files",
  [STEP_VALIDATE]: "Validate and save",
};

// PlacementRowState carries the per-row state the placement-map step
// edits. We keep file_path and skipped separate so a toggle off does
// not lose the operator's typed path.
interface PlacementRowState {
  provider: string;
  resource_kind: string;
  display_name: string;
  description: string;
  file_path: string;
  skipped: boolean;
}

// IaCGitHubWizardEditPlacementMode is the v0.89.4 #610 deep-link
// shape. When supplied, the wizard renders ONLY the placement-map
// step (no provider / PAT / repo / layout / validate chrome) and
// Save calls PATCH /iac/github/connections/:id/placement-map instead
// of POST /iac/github/connections. The page builds this from the
// query-param triple ?connection_id=...&step=placement&kind=...
// after looking up the connection in the SWR list cache.
export interface IaCGitHubWizardEditPlacementMode {
  kind: "edit-placement";
  connectionID: string;
  repoFullName: string;
  repoLayout: "mono" | "multi";
  // Seed rows for the placement-map step. Built from the connection's
  // existing placement_map joined against the canonical seven kinds —
  // rows the operator did not configure at create time render as
  // empty + non-skipped so they can be filled in here.
  initialRows: IaCPlacementEntry[];
  // The resource_kind from the URL ?kind=<...> query param. The
  // wizard scrolls this row into view + outlines it on first render.
  // null when the URL param was missing or unknown.
  focusedResourceKind: string | null;
}

export interface IaCGitHubWizardProps {
  // onComplete fires after a successful save. The page calls mutate()
  // on the connection-list SWR key and closes the dialog. The shape
  // matches both create + edit-placement modes; in the edit-placement
  // mode the connection_id is the existing row's ID (not a fresh one).
  onComplete: (connection: { connection_id: string; repo_full_name: string }) => void;
  // editMode opt-in for the v0.89.4 #610 deep-link path. When unset,
  // the wizard runs the full six-step create flow as before
  // (regression posture for the Phase-3 callers).
  editMode?: IaCGitHubWizardEditPlacementMode;
}

// initialPlacementRows seeds the placement-map step from the canonical
// resource-kind list. The state machine mutates these rows in place;
// the wizard does not re-seed across step navigations so the operator
// keeps any partial work when they jump back from the validate step.
//
// When existing is supplied (v0.89.4 #610 edit-placement deep link),
// each canonical row is joined against the existing connection: rows
// the operator already configured carry the saved file_path; rows
// they didn't render empty + non-skipped so they can be filled in
// here from the deep-linked entry point.
function initialPlacementRows(
  existing?: IaCPlacementEntry[],
): PlacementRowState[] {
  const byKind = new Map<string, IaCPlacementEntry>();
  for (const e of existing ?? []) {
    byKind.set(e.resource_kind, e);
  }
  return IAC_GITHUB_PLACEMENT_KINDS.map((k) => {
    const ex = byKind.get(k.resource_kind);
    return {
      provider: k.provider,
      resource_kind: k.resource_kind,
      display_name: k.display_name,
      description: k.description,
      file_path: ex?.file_path ?? "",
      skipped: false,
    };
  });
}

// placementRowsToEntries flattens the wizard's row state to the wire
// shape. Skipped rows are dropped — they never reach the validate /
// save payload (per design doc §6, skipped kinds simply don't have a
// placement-map row).
function placementRowsToEntries(rows: PlacementRowState[]): IaCPlacementEntry[] {
  return rows
    .filter((r) => !r.skipped && r.file_path.trim() !== "")
    .map((r) => ({
      provider: r.provider,
      resource_kind: r.resource_kind,
      file_path: r.file_path.trim(),
    }));
}

// IaCGitHubWizard is the top-level wizard entry point. It branches
// between two component implementations based on editMode so the
// hooks-rule contract per implementation stays clean: the create
// flow uses ~15 useState calls + memos; the placement-only edit
// flow uses a smaller set. Switching between them inside one
// function body would violate the rules-of-hooks contract.
export function IaCGitHubWizard({ onComplete, editMode }: IaCGitHubWizardProps) {
  if (editMode) {
    return <PlacementOnlyEditor editMode={editMode} onComplete={onComplete} />;
  }
  return <IaCGitHubWizardCreate onComplete={onComplete} />;
}

function IaCGitHubWizardCreate({
  onComplete,
}: {
  onComplete: IaCGitHubWizardProps["onComplete"];
}) {
  const [stepIndex, setStepIndex] = useState(0);

  // Token — held in component state ONLY. No localStorage. No
  // sessionStorage. Cleared when the dialog unmounts.
  const [token, setToken] = useState("");

  // Repo + layout fields.
  const [repoFullName, setRepoFullName] = useState("");
  const [repoLayout, setRepoLayout] = useState<"mono" | "multi">("multi");
  const [defaultBranch, setDefaultBranch] = useState("main");
  const [branchPrefix, setBranchPrefix] = useState("");
  const [reviewerTeamHandle, setReviewerTeamHandle] = useState("");
  const [showAdvanced, setShowAdvanced] = useState(false);

  // Placement map — per-row file_path + skipped flag.
  const [placementRows, setPlacementRows] = useState<PlacementRowState[]>(
    initialPlacementRows,
  );

  // Bulk-apply pattern input (e.g. "modules/{kind}/main.tf"). Slice 1
  // nice-to-have — typing a pattern and clicking Apply substitutes
  // {kind} per row across every non-skipped row that's still empty.
  const [bulkPattern, setBulkPattern] = useState("");

  // Validate / save state.
  const [validateResult, setValidateResult] =
    useState<IaCGitHubValidateResponse | null>(null);
  const [validating, setValidating] = useState(false);
  const [validateError, setValidateError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [savedConnection, setSavedConnection] = useState<{
    connection_id: string;
    repo_full_name: string;
  } | null>(null);

  const stepCount = STEP_IDS.length;
  const currentStepId = STEP_IDS[stepIndex];
  const isLastStep = stepIndex === stepCount - 1;

  const repoFormatValid = REPO_FULL_NAME_RE.test(repoFullName);
  const placementEntries = useMemo(
    () => placementRowsToEntries(placementRows),
    [placementRows],
  );

  // Next-enablement matrix per step. The PAT step requires a non-empty
  // token; the repo step requires a well-formed owner/repo; the layout
  // step requires a non-empty default_branch (defaults to "main");
  // placement step is always advanceable (skipping every row is allowed
  // per spec §6 — operator can configure per-kind later).
  let nextEnabled = false;
  switch (currentStepId) {
    case STEP_PROVIDER:
      nextEnabled = true; // GitHub is preselected
      break;
    case STEP_PAT:
      nextEnabled = token.trim() !== "";
      break;
    case STEP_PICK_REPO:
      nextEnabled = repoFormatValid;
      break;
    case STEP_REPO_LAYOUT:
      nextEnabled = defaultBranch.trim() !== "";
      break;
    case STEP_PLACEMENT_MAP:
      nextEnabled = true;
      break;
    case STEP_VALIDATE:
      // The validate step uses its own primary actions (Validate,
      // Save) rather than the global Next button.
      nextEnabled = false;
      break;
  }

  const handleNext = useCallback(() => {
    if (!nextEnabled) return;
    setStepIndex((i) => Math.min(stepCount - 1, i + 1));
  }, [nextEnabled, stepCount]);

  const handleBack = useCallback(() => {
    setStepIndex((i) => Math.max(0, i - 1));
    // Any back-navigation invalidates the prior preflight — the
    // operator must re-run validate before saving.
    setValidateResult(null);
    setValidateError(null);
  }, []);

  const jumpToStepId = useCallback((id: string) => {
    const idx = STEP_IDS.indexOf(id as (typeof STEP_IDS)[number]);
    if (idx >= 0) {
      setStepIndex(idx);
      setValidateResult(null);
      setValidateError(null);
    }
  }, []);

  const handleCopy = useCallback((value: string) => {
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(value);
    }
  }, []);

  // Placement-map row edits ----------------------------------------

  const setRowFilePath = useCallback((idx: number, file_path: string) => {
    setPlacementRows((rows) =>
      rows.map((r, i) => (i === idx ? { ...r, file_path } : r)),
    );
  }, []);

  const toggleRowSkipped = useCallback((idx: number) => {
    setPlacementRows((rows) =>
      rows.map((r, i) => (i === idx ? { ...r, skipped: !r.skipped } : r)),
    );
  }, []);

  const skipAll = useCallback(() => {
    setPlacementRows((rows) => rows.map((r) => ({ ...r, skipped: true })));
  }, []);

  const unskipAll = useCallback(() => {
    setPlacementRows((rows) => rows.map((r) => ({ ...r, skipped: false })));
  }, []);

  const applyBulkPattern = useCallback(() => {
    const pat = bulkPattern.trim();
    if (pat === "") return;
    setPlacementRows((rows) =>
      rows.map((r) => {
        if (r.skipped) return r;
        if (r.file_path.trim() !== "") return r;
        // Substitute {kind} with the row's resource_kind. Operators
        // who type a literal path with no placeholder also get it
        // applied uniformly — both are useful one-step affordances.
        return { ...r, file_path: pat.replace(/\{kind\}/g, r.resource_kind) };
      }),
    );
  }, [bulkPattern]);

  // Validate + Save ------------------------------------------------

  const handleValidate = useCallback(async () => {
    setValidating(true);
    setValidateError(null);
    setValidateResult(null);
    try {
      const res = await validateIaCGitHub({
        token,
        repo_full_name: repoFullName,
        default_branch: defaultBranch.trim() === "main" ? undefined : defaultBranch.trim(),
        placement_map: placementEntries,
      });
      setValidateResult(res);
      // If the server filled in default_branch (because we sent
      // empty), update the local field so the Save payload matches.
      if (res.default_branch && res.default_branch !== defaultBranch) {
        setDefaultBranch(res.default_branch);
      }
    } catch (e) {
      setValidateError(e instanceof Error ? e.message : String(e));
    } finally {
      setValidating(false);
    }
  }, [token, repoFullName, defaultBranch, placementEntries]);

  const handleSave = useCallback(async () => {
    setSaving(true);
    setSaveError(null);
    try {
      const res = await saveIaCGitHubConnection({
        token,
        repo_full_name: repoFullName,
        default_branch: defaultBranch,
        repo_layout: repoLayout,
        branch_prefix: branchPrefix.trim() || undefined,
        reviewer_team_handle: reviewerTeamHandle.trim() || undefined,
        placement_map: placementEntries,
      });
      setSavedConnection({
        connection_id: res.connection_id,
        repo_full_name: res.repo_full_name,
      });
      onComplete({
        connection_id: res.connection_id,
        repo_full_name: res.repo_full_name,
      });
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }, [
    token,
    repoFullName,
    defaultBranch,
    repoLayout,
    branchPrefix,
    reviewerTeamHandle,
    placementEntries,
    onComplete,
  ]);

  // Saved state — render the Connected card and stop. Matches the AWS
  // wizard's post-save UX so operator muscle memory carries.
  if (savedConnection) {
    return (
      <ConnectedCard
        connectionID={savedConnection.connection_id}
        repoFullName={savedConnection.repo_full_name}
        placementCount={placementEntries.length}
      />
    );
  }

  return (
    <div className="space-y-6">
      <Header stepIndex={stepIndex} stepCount={stepCount} />

      <div className="rounded-lg border bg-card p-6">
        <div>
          <h3 className="text-base font-semibold">
            {STEP_TITLES[currentStepId]}
          </h3>
        </div>

        <div className="mt-4">
          {currentStepId === STEP_PROVIDER && <ProviderStep />}

          {currentStepId === STEP_PAT && (
            <PATStep
              token={token}
              onTokenChange={setToken}
              onCopyURL={() => handleCopy(GITHUB_CREATE_PAT_URL)}
            />
          )}

          {currentStepId === STEP_PICK_REPO && (
            <PickRepoStep
              repoFullName={repoFullName}
              onChange={setRepoFullName}
              formatValid={repoFormatValid}
            />
          )}

          {currentStepId === STEP_REPO_LAYOUT && (
            <RepoLayoutStep
              repoLayout={repoLayout}
              onRepoLayoutChange={setRepoLayout}
              defaultBranch={defaultBranch}
              onDefaultBranchChange={setDefaultBranch}
              branchPrefix={branchPrefix}
              onBranchPrefixChange={setBranchPrefix}
              reviewerTeamHandle={reviewerTeamHandle}
              onReviewerTeamHandleChange={setReviewerTeamHandle}
              showAdvanced={showAdvanced}
              onToggleAdvanced={() => setShowAdvanced((v) => !v)}
            />
          )}

          {currentStepId === STEP_PLACEMENT_MAP && (
            <PlacementMapStep
              rows={placementRows}
              repoLayout={repoLayout}
              bulkPattern={bulkPattern}
              onBulkPatternChange={setBulkPattern}
              onApplyBulkPattern={applyBulkPattern}
              onRowFilePathChange={setRowFilePath}
              onToggleRowSkipped={toggleRowSkipped}
              onSkipAll={skipAll}
              onUnskipAll={unskipAll}
            />
          )}

          {currentStepId === STEP_VALIDATE && (
            <ValidateStep
              validating={validating}
              validateError={validateError}
              validateResult={validateResult}
              saving={saving}
              saveError={saveError}
              onValidate={handleValidate}
              onSave={handleSave}
              onJumpToStep={jumpToStepId}
              placementEntryCount={placementEntries.length}
              repoFullName={repoFullName}
            />
          )}
        </div>
      </div>

      <div className="flex items-center justify-between">
        <Button
          type="button"
          variant="ghost"
          onClick={handleBack}
          disabled={stepIndex === 0}
        >
          <ChevronLeft className="mr-1 h-4 w-4" aria-hidden />
          Back
        </Button>
        {!isLastStep && (
          <Button type="button" onClick={handleNext} disabled={!nextEnabled}>
            Next
          </Button>
        )}
      </div>
    </div>
  );
}

// --- Header / progress bar ----------------------------------------

function Header({
  stepIndex,
  stepCount,
}: {
  stepIndex: number;
  stepCount: number;
}) {
  return (
    <div>
      <h2 className="text-lg font-semibold">Connect an IaC repository</h2>
      <div
        className="mt-3 flex items-center gap-2"
        role="progressbar"
        aria-valuenow={stepIndex + 1}
        aria-valuemin={1}
        aria-valuemax={stepCount}
      >
        {Array.from({ length: stepCount }).map((_, i) => (
          <div
            key={i}
            className={cn(
              "h-2 flex-1 rounded-full",
              i <= stepIndex ? "bg-primary" : "bg-muted",
            )}
          />
        ))}
      </div>
      <p className="mt-2 text-xs text-muted-foreground">
        Step {stepIndex + 1} of {stepCount}
      </p>
    </div>
  );
}

// --- Step 1: Provider ---------------------------------------------

function ProviderStep() {
  return (
    <div className="space-y-3">
      <p className="text-sm text-muted-foreground">
        Squadron opens PRs against your IaC repo when a recommendation
        is acted on. Slice 1 supports GitHub; GitLab and Bitbucket land
        in slice 2.
      </p>
      <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
        <ProviderTile name="GitHub" selected enabled icon={Github} />
        <ProviderTile name="GitLab" badge="slice 2" />
        <ProviderTile name="Bitbucket" badge="slice 2" />
      </div>
    </div>
  );
}

function ProviderTile({
  name,
  selected,
  enabled,
  badge,
  icon: Icon,
}: {
  name: string;
  selected?: boolean;
  enabled?: boolean;
  badge?: string;
  icon?: React.ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
}) {
  return (
    <div
      className={cn(
        "flex items-center justify-between gap-2 rounded-md border p-3",
        selected && "border-primary bg-primary/5",
        !enabled && "opacity-50",
      )}
      aria-disabled={!enabled}
    >
      <div className="flex items-center gap-2">
        {Icon && <Icon className="h-4 w-4" aria-hidden />}
        <span className="text-sm font-medium">{name}</span>
      </div>
      {badge && (
        <Badge variant="outline" className="text-[10px]">
          {badge}
        </Badge>
      )}
      {selected && (
        <CheckCircle2 className="h-4 w-4 text-primary" aria-hidden />
      )}
    </div>
  );
}

// --- Step 2: PAT ---------------------------------------------------

function PATStep({
  token,
  onTokenChange,
  onCopyURL,
}: {
  token: string;
  onTokenChange: (v: string) => void;
  onCopyURL: () => void;
}) {
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
        <AuthTile
          name="GitHub App"
          recommended
          badge="slice 2"
          description="Per-repo scope, short-lived tokens. Lands in slice 2."
        />
        <AuthTile
          name="Personal Access Token"
          enabled
          selected
          description="Classic PAT with the `repo` scope. Org-wide; see warning below."
        />
      </div>

      <div className="space-y-2 rounded-md border bg-muted/30 p-3">
        <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
          How to create the PAT
        </p>
        <ol className="ml-4 list-decimal space-y-1 text-sm">
          <li>
            Open GitHub&apos;s create-token page (pre-filled with the{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">repo</code>{" "}
            scope and a Squadron description).
          </li>
          <li>Confirm the scope, click Generate, copy the token.</li>
          <li>Paste it in the field below — it never leaves your browser before save.</li>
        </ol>
        <div className="flex flex-wrap gap-2 pt-1">
          <Button
            type="button"
            size="sm"
            variant="secondary"
            onClick={() =>
              window.open(GITHUB_CREATE_PAT_URL, "_blank", "noopener,noreferrer")
            }
          >
            <ExternalLink className="mr-1 h-3.5 w-3.5" aria-hidden />
            Open GitHub PAT creation
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={onCopyURL}>
            <Copy className="mr-1 h-3.5 w-3.5" aria-hidden />
            Copy URL
          </Button>
        </div>
      </div>

      <div className="space-y-1">
        <Label htmlFor="iac-github-pat" className="text-xs font-semibold">
          GitHub Personal Access Token
        </Label>
        <Input
          id="iac-github-pat"
          aria-label="GitHub Personal Access Token"
          type="password"
          autoComplete="off"
          spellCheck={false}
          data-1p-ignore="true"
          data-lpignore="true"
          placeholder="ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxx"
          value={token}
          onChange={(e) => onTokenChange(e.target.value)}
        />
        <p className="text-xs text-muted-foreground">
          The token stays in this browser tab until you click Save. Squadron
          will only create branches and open pull requests — Squadron will
          never push to your default branch.
        </p>
      </div>

      <div className="flex items-start gap-2 rounded-md border border-yellow-500/40 bg-yellow-500/5 p-3 text-xs">
        <AlertCircle
          className="mt-0.5 h-3.5 w-3.5 shrink-0 text-yellow-600 dark:text-yellow-400"
          aria-hidden
        />
        <p className="text-muted-foreground">
          PAT is org-wide. For per-repo scoping, wait for the GitHub App
          path in slice 2.
        </p>
      </div>
    </div>
  );
}

function AuthTile({
  name,
  description,
  badge,
  selected,
  enabled,
  recommended,
}: {
  name: string;
  description: string;
  badge?: string;
  selected?: boolean;
  enabled?: boolean;
  recommended?: boolean;
}) {
  return (
    <div
      className={cn(
        "rounded-md border p-3",
        selected && "border-primary bg-primary/5",
        !enabled && "opacity-50",
      )}
      aria-disabled={!enabled}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium">
          {name}
          {recommended && (
            <span className="ml-1 text-[10px] uppercase tracking-wider text-muted-foreground">
              (recommended)
            </span>
          )}
        </span>
        {badge && (
          <Badge variant="outline" className="text-[10px]">
            {badge}
          </Badge>
        )}
        {selected && (
          <CheckCircle2 className="h-4 w-4 text-primary" aria-hidden />
        )}
      </div>
      <p className="mt-1 text-xs text-muted-foreground">{description}</p>
    </div>
  );
}

// --- Step 3: Pick repo --------------------------------------------

function PickRepoStep({
  repoFullName,
  onChange,
  formatValid,
}: {
  repoFullName: string;
  onChange: (v: string) => void;
  formatValid: boolean;
}) {
  const showError = repoFullName !== "" && !formatValid;
  return (
    <div className="space-y-2">
      <p className="text-sm text-muted-foreground">
        Type the repository as <code>owner/repo</code>. Slice 1 ships
        one repo per connection; slice 2 will autocomplete from the
        token&apos;s reachable repos.
      </p>
      <Label htmlFor="iac-github-repo" className="text-xs font-semibold">
        Repository
      </Label>
      <Input
        id="iac-github-repo"
        aria-label="Repository full name"
        aria-invalid={showError}
        placeholder="my-org/infra-terraform"
        value={repoFullName}
        onChange={(e) => onChange(e.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
      {showError && (
        <p className="text-xs text-destructive">
          Must be in <code>owner/repo</code> form (letters, digits, dashes,
          dots, underscores).
        </p>
      )}
      <p className="text-xs text-muted-foreground">
        Example: <code>my-org/infra-terraform</code>
      </p>
    </div>
  );
}

// --- Step 4: Repo layout + branch + advanced ----------------------

function RepoLayoutStep({
  repoLayout,
  onRepoLayoutChange,
  defaultBranch,
  onDefaultBranchChange,
  branchPrefix,
  onBranchPrefixChange,
  reviewerTeamHandle,
  onReviewerTeamHandleChange,
  showAdvanced,
  onToggleAdvanced,
}: {
  repoLayout: "mono" | "multi";
  onRepoLayoutChange: (v: "mono" | "multi") => void;
  defaultBranch: string;
  onDefaultBranchChange: (v: string) => void;
  branchPrefix: string;
  onBranchPrefixChange: (v: string) => void;
  reviewerTeamHandle: string;
  onReviewerTeamHandleChange: (v: string) => void;
  showAdvanced: boolean;
  onToggleAdvanced: () => void;
}) {
  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <Label className="text-xs font-semibold">Repository layout</Label>
        <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
          <LayoutTile
            label="Multi-repo"
            sub="This repo holds one environment or domain"
            selected={repoLayout === "multi"}
            onClick={() => onRepoLayoutChange("multi")}
          />
          <LayoutTile
            label="Mono-repo"
            sub="One repo with multiple environments"
            selected={repoLayout === "mono"}
            onClick={() => onRepoLayoutChange("mono")}
          />
        </div>
        <p className="text-xs text-muted-foreground">
          {repoLayout === "mono"
            ? "We'll show you path examples like environments/prod/eks/main.tf. Each placement file in the next step can use whatever depth you have."
            : "We'll show you path examples like modules/eks/main.tf. Each placement maps to one file in this repo."}
        </p>
      </div>

      <div className="space-y-1">
        <Label htmlFor="iac-github-default-branch" className="text-xs font-semibold">
          Default branch
        </Label>
        <Input
          id="iac-github-default-branch"
          aria-label="Default branch"
          placeholder="main"
          value={defaultBranch}
          onChange={(e) => onDefaultBranchChange(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <p className="text-xs text-muted-foreground">
          We&apos;ll auto-detect this on validate. If GitHub disagrees,
          the server&apos;s value wins.
        </p>
      </div>

      <div>
        <Button
          type="button"
          variant="link"
          size="sm"
          className="h-auto p-0 text-xs"
          onClick={onToggleAdvanced}
          aria-expanded={showAdvanced}
        >
          {showAdvanced ? "Hide advanced options" : "Advanced options"}
        </Button>
      </div>

      {showAdvanced && (
        <div className="space-y-4 rounded-md border bg-muted/30 p-3">
          <div className="space-y-1">
            <Label htmlFor="iac-github-branch-prefix" className="text-xs font-semibold">
              Branch prefix
            </Label>
            <Input
              id="iac-github-branch-prefix"
              aria-label="Branch prefix"
              placeholder={DEFAULT_BRANCH_PREFIX}
              value={branchPrefix}
              onChange={(e) => onBranchPrefixChange(e.target.value)}
              autoComplete="off"
              spellCheck={false}
            />
            <p className="text-xs text-muted-foreground">
              Squadron&apos;s PR branches will be named{" "}
              <code>&lt;this&gt;-&lt;scan-id&gt;-&lt;step&gt;</code>.
              Defaults to <code>{DEFAULT_BRANCH_PREFIX}</code>.
            </p>
          </div>
          <div className="space-y-1">
            <Label htmlFor="iac-github-reviewer-team" className="text-xs font-semibold">
              Reviewer team handle
            </Label>
            <Input
              id="iac-github-reviewer-team"
              aria-label="Reviewer team handle"
              placeholder="my-org/platform-reviewers"
              value={reviewerTeamHandle}
              onChange={(e) => onReviewerTeamHandleChange(e.target.value)}
              autoComplete="off"
              spellCheck={false}
            />
            <p className="text-xs text-muted-foreground">
              If set, Squadron requests review from this team on every PR.
              Leave empty to skip.
            </p>
          </div>
        </div>
      )}
    </div>
  );
}

function LayoutTile({
  label,
  sub,
  selected,
  onClick,
}: {
  label: string;
  sub: string;
  selected: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={selected}
      className={cn(
        "rounded-md border p-3 text-left transition-colors",
        selected
          ? "border-primary bg-primary/5"
          : "hover:border-foreground/30",
      )}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium">{label}</span>
        {selected && (
          <CheckCircle2 className="h-4 w-4 text-primary" aria-hidden />
        )}
      </div>
      <p className="mt-1 text-xs text-muted-foreground">{sub}</p>
    </button>
  );
}

// --- Step 5: Placement map ----------------------------------------

function PlacementMapStep({
  rows,
  repoLayout,
  bulkPattern,
  onBulkPatternChange,
  onApplyBulkPattern,
  onRowFilePathChange,
  onToggleRowSkipped,
  onSkipAll,
  onUnskipAll,
  focusedResourceKind,
}: {
  rows: PlacementRowState[];
  repoLayout: "mono" | "multi";
  bulkPattern: string;
  onBulkPatternChange: (v: string) => void;
  onApplyBulkPattern: () => void;
  onRowFilePathChange: (idx: number, v: string) => void;
  onToggleRowSkipped: (idx: number) => void;
  onSkipAll: () => void;
  onUnskipAll: () => void;
  // v0.89.4 #610 — when set, the row matching this resource_kind is
  // scrolled into view + outlined so operators landing here via the
  // deep-link see exactly which row needs attention without scanning
  // a list of seven.
  focusedResourceKind?: string | null;
}) {
  const allSkipped = rows.every((r) => r.skipped);
  const activeCount = rows.filter((r) => !r.skipped).length;
  const placeholderExample =
    repoLayout === "mono"
      ? "environments/prod/{kind}/main.tf"
      : "modules/{kind}/main.tf";

  // Focus-row plumbing. The ref lands on the <li> that matches
  // focusedResourceKind; the effect scrolls it into view once on
  // first render (no infinite-scroll loop because the dep array
  // tracks the kind itself, not the ref). The outline ring is
  // expressed via a CSS class on the <li>.
  const focusedRowRef = useRef<HTMLLIElement | null>(null);
  useEffect(() => {
    if (!focusedResourceKind) return;
    const el = focusedRowRef.current;
    if (el && typeof el.scrollIntoView === "function") {
      el.scrollIntoView({ block: "center", behavior: "auto" });
    }
  }, [focusedResourceKind]);

  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Tell Squadron which file to append each kind&apos;s Terraform snippet
        to. Skip the kinds you don&apos;t manage in this repo — you can
        configure them later from the connection&apos;s settings.
      </p>

      <div className="space-y-2 rounded-md border bg-muted/30 p-3">
        <Label htmlFor="iac-github-bulk-pattern" className="text-xs font-semibold">
          Apply a pattern to all empty rows
        </Label>
        <div className="flex flex-col gap-2 md:flex-row">
          <Input
            id="iac-github-bulk-pattern"
            aria-label="Bulk pattern"
            placeholder={placeholderExample}
            value={bulkPattern}
            onChange={(e) => onBulkPatternChange(e.target.value)}
            autoComplete="off"
            spellCheck={false}
          />
          <Button
            type="button"
            variant="secondary"
            size="sm"
            onClick={onApplyBulkPattern}
            disabled={bulkPattern.trim() === ""}
          >
            Apply pattern
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          <code>{"{kind}"}</code> is substituted with the row&apos;s{" "}
          <code>resource_kind</code>. Only empty, non-skipped rows are
          updated.
        </p>
      </div>

      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span>
          {activeCount} of {rows.length} placements active
        </span>
        {allSkipped ? (
          <Button
            type="button"
            variant="link"
            size="sm"
            className="h-auto p-0 text-xs"
            onClick={onUnskipAll}
          >
            Un-skip all
          </Button>
        ) : (
          <Button
            type="button"
            variant="link"
            size="sm"
            className="h-auto p-0 text-xs"
            onClick={onSkipAll}
          >
            Skip all for now — configure per-kind later
          </Button>
        )}
      </div>

      <ul className="space-y-2">
        {rows.map((row, idx) => {
          const isFocused =
            focusedResourceKind != null &&
            row.resource_kind === focusedResourceKind;
          return (
          <li
            key={row.resource_kind}
            ref={isFocused ? focusedRowRef : undefined}
            data-focused={isFocused ? "true" : undefined}
            data-testid={`iac-github-placement-row-${row.resource_kind}`}
            className={cn(
              "rounded-md border bg-card p-3",
              row.skipped && "opacity-60",
              isFocused &&
                "border-violet-500 ring-2 ring-violet-500/50",
            )}
          >
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <code className="font-mono text-xs">{row.resource_kind}</code>
                  <Badge variant="outline" className="text-[10px]">
                    {row.provider}
                  </Badge>
                  {row.skipped && (
                    <Badge variant="outline" className="text-[10px] text-muted-foreground">
                      <MinusCircle className="mr-1 h-3 w-3" aria-hidden />
                      skipped
                    </Badge>
                  )}
                </div>
                <p className="text-xs font-medium">{row.display_name}</p>
                <p className="text-xs text-muted-foreground">
                  {row.description}
                </p>
              </div>
              <div className="flex shrink-0 items-center gap-2">
                <Label
                  htmlFor={`iac-github-skip-${idx}`}
                  className="text-[10px] uppercase text-muted-foreground"
                >
                  Skip
                </Label>
                <Switch
                  id={`iac-github-skip-${idx}`}
                  aria-label={`Skip ${row.resource_kind}`}
                  checked={row.skipped}
                  onCheckedChange={() => onToggleRowSkipped(idx)}
                />
              </div>
            </div>
            <div className="mt-2">
              <Label
                htmlFor={`iac-github-path-${idx}`}
                className="sr-only"
              >
                File path for {row.resource_kind}
              </Label>
              <Input
                id={`iac-github-path-${idx}`}
                aria-label={`File path for ${row.resource_kind}`}
                placeholder={placeholderExample.replace(
                  "{kind}",
                  row.resource_kind.split("-")[0],
                )}
                value={row.file_path}
                onChange={(e) => onRowFilePathChange(idx, e.target.value)}
                disabled={row.skipped}
                autoComplete="off"
                spellCheck={false}
              />
            </div>
          </li>
          );
        })}
      </ul>
    </div>
  );
}

// --- Step 6: Validate + Save --------------------------------------

function ValidateStep({
  validating,
  validateError,
  validateResult,
  saving,
  saveError,
  onValidate,
  onSave,
  onJumpToStep,
  placementEntryCount,
  repoFullName,
}: {
  validating: boolean;
  validateError: string | null;
  validateResult: IaCGitHubValidateResponse | null;
  saving: boolean;
  saveError: string | null;
  onValidate: () => void;
  onSave: () => void;
  onJumpToStep: (id: string) => void;
  placementEntryCount: number;
  repoFullName: string;
}) {
  // Save-enable matrix: must have a validate result, repo_err must be
  // null (the repo itself must be reachable), and we must not still be
  // validating. Per-row errors do NOT block Save — those rows are
  // simply skipped at PR time, surfaced as a notice next to the
  // Save button.
  const canSave =
    !!validateResult && !validateResult.repo_err && !validating && !saving;

  // Count per-row outcomes for the Save-time notice.
  const rowsWithError = validateResult?.preflight_results.filter((r) => !!r.err) ?? [];

  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Squadron will check it can reach <code>{repoFullName || "your repo"}</code>{" "}
        and that each non-skipped placement file is readable. No records
        are created until Save.
      </p>

      <Button type="button" onClick={onValidate} disabled={validating}>
        {validating && (
          <Loader2 className="mr-1 h-4 w-4 animate-spin" aria-hidden />
        )}
        {validateResult ? "Re-validate" : "Validate"}
      </Button>

      {validateError && (
        <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
          {validateError}
        </div>
      )}

      {validateResult && (
        <PreflightPanel
          result={validateResult}
          onJumpToStep={onJumpToStep}
        />
      )}

      {validateResult && !validateResult.repo_err && (
        <div className="space-y-2 rounded-md border bg-muted/30 p-3">
          {rowsWithError.length > 0 && (
            <p className="text-xs text-muted-foreground">
              We&apos;ll save the {placementEntryCount - rowsWithError.length}{" "}
              passing placements and skip {rowsWithError.length} that errored.
              You can re-run the wizard to fix them.
            </p>
          )}
          <div className="flex items-center gap-2">
            <Button
              type="button"
              onClick={onSave}
              disabled={!canSave}
            >
              {saving && (
                <Loader2 className="mr-1 h-4 w-4 animate-spin" aria-hidden />
              )}
              Save connection
            </Button>
            <span className="text-xs text-muted-foreground">
              Saving {placementEntryCount} placement
              {placementEntryCount === 1 ? "" : "s"} for{" "}
              <code>{repoFullName}</code>.
            </span>
          </div>
          {saveError && (
            <div className="rounded-md border border-destructive/40 bg-destructive/5 p-2 text-sm text-destructive">
              {saveError}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function PreflightPanel({
  result,
  onJumpToStep,
}: {
  result: IaCGitHubValidateResponse;
  onJumpToStep: (id: string) => void;
}) {
  return (
    <div className="rounded-md border bg-muted/30 p-3">
      <h4 className="text-sm font-semibold">What just happened</h4>
      <div className="mt-2 space-y-2">
        <StatusRow
          ok={!result.repo_err}
          label={
            <>
              Reach repo{" "}
              <code className="font-mono text-xs">{result.repo_full_name}</code>
              {result.default_branch && (
                <span className="ml-1 text-xs text-muted-foreground">
                  · default branch <code>{result.default_branch}</code>
                </span>
              )}
            </>
          }
        />
        {result.repo_err && (
          <ErrorRow err={result.repo_err} onJumpToStep={onJumpToStep} />
        )}
        {result.preflight_results.map((row) => (
          <PreflightRowView
            key={row.resource_kind}
            row={row}
            onJumpToStep={onJumpToStep}
          />
        ))}
        {result.preflight_results.length === 0 && (
          <p className="text-xs text-muted-foreground">
            No placement-map rows to check — all were skipped. You can
            still save the connection and configure paths later.
          </p>
        )}
      </div>
    </div>
  );
}

function PreflightRowView({
  row,
  onJumpToStep,
}: {
  row: IaCGitHubPreflightRow;
  onJumpToStep: (id: string) => void;
}) {
  if (row.err) {
    return (
      <div>
        <StatusRow
          ok={false}
          label={
            <>
              <code className="font-mono text-xs">{row.resource_kind}</code>{" "}
              · <code className="text-xs">{row.file_path}</code>
            </>
          }
        />
        <ErrorRow err={row.err} onJumpToStep={onJumpToStep} />
      </div>
    );
  }
  return (
    <StatusRow
      ok={true}
      label={
        <>
          <code className="font-mono text-xs">{row.resource_kind}</code> ·{" "}
          <code className="text-xs">{row.file_path}</code>
        </>
      }
      suffix={
        row.exists
          ? row.sha_short
            ? `found (${row.sha_short})`
            : "found"
          : "will be created on first PR"
      }
    />
  );
}

function StatusRow({
  ok,
  label,
  suffix,
  skipped,
}: {
  ok: boolean;
  label: React.ReactNode;
  suffix?: string;
  skipped?: boolean;
}) {
  return (
    <div className="flex items-center gap-2 text-sm">
      {skipped ? (
        <MinusCircle className="h-4 w-4 text-muted-foreground" aria-hidden />
      ) : ok ? (
        <CheckCircle2 className="h-4 w-4 text-green-600" aria-hidden />
      ) : (
        <XCircle className="h-4 w-4 text-destructive" aria-hidden />
      )}
      <span className="font-medium">{label}</span>
      {suffix && <span className="text-xs text-muted-foreground">{suffix}</span>}
    </div>
  );
}

function ErrorRow({
  err,
  onJumpToStep,
}: {
  err: IaCHumanizedError;
  onJumpToStep: (id: string) => void;
}) {
  const targetTitle = STEP_TITLES[err.suggested_step];
  return (
    <div className="ml-6 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-xs">
      <p className="text-destructive">{err.message}</p>
      {targetTitle && (
        <Button
          type="button"
          variant="link"
          size="sm"
          className="h-auto p-0 text-xs"
          onClick={() => onJumpToStep(err.suggested_step)}
        >
          Return to: {targetTitle}
        </Button>
      )}
    </div>
  );
}

// --- Saved state ---------------------------------------------------

function ConnectedCard({
  connectionID,
  repoFullName,
  placementCount,
}: {
  connectionID: string;
  repoFullName: string;
  placementCount: number;
}) {
  return (
    <div className="rounded-lg border bg-card p-6">
      <div className="flex items-center gap-3">
        <CheckCircle2 className="h-6 w-6 text-green-600" aria-hidden />
        <h2 className="text-lg font-semibold">Repository connected</h2>
      </div>
      <p className="mt-2 text-sm text-muted-foreground">
        Squadron will open PRs against{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-xs">
          {repoFullName}
        </code>{" "}
        when a recommendation matches one of the{" "}
        <strong>{placementCount}</strong> placement
        {placementCount === 1 ? "" : "s"} you saved.
      </p>
      <p className="mt-2 text-xs text-muted-foreground">
        Connection ID:{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-[10px]">
          {connectionID}
        </code>
      </p>
    </div>
  );
}

// --- v0.89.4 #610 placement-only editor ---------------------------
//
// PlacementOnlyEditor is the deep-linked-wizard render path. The
// page (/discovery/iac/github) constructs it from query params:
//   ?connection_id=<uuid>&step=placement&kind=<resource_kind>
// after looking up the connection in the SWR list cache. The
// editor reuses PlacementMapStep verbatim (same bulk-pattern
// affordance, same Skip toggles, same focus-row plumbing) and adds
// a Save button that calls PATCH /iac/github/connections/:id/
// placement-map. NO token round-trip — the substrate's stored
// ciphertext is preserved untouched. NO Validate step — Open-PR's
// own preflight catches a broken row at PR time, and forcing a
// re-run of GitHub I/O here would slow the "fix the row and retry
// Open PR" loop the deep link exists to optimize.
function PlacementOnlyEditor({
  editMode,
  onComplete,
}: {
  editMode: IaCGitHubWizardEditPlacementMode;
  onComplete: (c: { connection_id: string; repo_full_name: string }) => void;
}) {
  // Pre-populated row state. Skipped flag is always false on first
  // mount — operators reach this surface to ADD a row (the deep
  // link is fired specifically because Open-PR failed on a missing
  // row), so unifying around "all rows visible + editable" beats
  // re-running the wizard's skip-all affordance.
  const [rows, setRows] = useState<PlacementRowState[]>(() =>
    initialPlacementRows(editMode.initialRows),
  );
  const [bulkPattern, setBulkPattern] = useState("");
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const entries = useMemo(() => placementRowsToEntries(rows), [rows]);

  const setRowFilePath = useCallback((idx: number, file_path: string) => {
    setRows((rs) => rs.map((r, i) => (i === idx ? { ...r, file_path } : r)));
  }, []);
  const toggleRowSkipped = useCallback((idx: number) => {
    setRows((rs) =>
      rs.map((r, i) => (i === idx ? { ...r, skipped: !r.skipped } : r)),
    );
  }, []);
  const skipAll = useCallback(() => {
    setRows((rs) => rs.map((r) => ({ ...r, skipped: true })));
  }, []);
  const unskipAll = useCallback(() => {
    setRows((rs) => rs.map((r) => ({ ...r, skipped: false })));
  }, []);
  const applyBulkPattern = useCallback(() => {
    const pat = bulkPattern.trim();
    if (pat === "") return;
    setRows((rs) =>
      rs.map((r) => {
        if (r.skipped) return r;
        if (r.file_path.trim() !== "") return r;
        return { ...r, file_path: pat.replace(/\{kind\}/g, r.resource_kind) };
      }),
    );
  }, [bulkPattern]);

  const handleSave = useCallback(async () => {
    setSaving(true);
    setSaveError(null);
    try {
      const res = await updateIaCGitHubPlacementMap(editMode.connectionID, {
        placement_map: entries,
      });
      setSaved(true);
      onComplete({
        connection_id: res.connection_id,
        repo_full_name: res.repo_full_name,
      });
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }, [editMode.connectionID, entries, onComplete]);

  if (saved) {
    return (
      <div className="rounded-lg border bg-card p-6">
        <div className="flex items-center gap-3">
          <CheckCircle2 className="h-6 w-6 text-green-600" aria-hidden />
          <h2 className="text-lg font-semibold">Placement map updated</h2>
        </div>
        <p className="mt-2 text-sm text-muted-foreground">
          Squadron will use the new mapping when opening PRs against{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            {editMode.repoFullName}
          </code>
          .
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-lg font-semibold">Edit placement map</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Updating placement map for{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            {editMode.repoFullName}
          </code>
          . The connection&apos;s token, branch prefix, and reviewer team
          stay as they were.
        </p>
      </div>

      <div className="rounded-lg border bg-card p-6">
        <h3 className="text-base font-semibold">
          {STEP_TITLES[STEP_PLACEMENT_MAP]}
        </h3>
        <div className="mt-4">
          <PlacementMapStep
            rows={rows}
            repoLayout={editMode.repoLayout}
            bulkPattern={bulkPattern}
            onBulkPatternChange={setBulkPattern}
            onApplyBulkPattern={applyBulkPattern}
            onRowFilePathChange={setRowFilePath}
            onToggleRowSkipped={toggleRowSkipped}
            onSkipAll={skipAll}
            onUnskipAll={unskipAll}
            focusedResourceKind={editMode.focusedResourceKind}
          />
        </div>
      </div>

      {saveError && (
        <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
          {saveError}
        </div>
      )}

      <div className="flex items-center justify-end gap-2">
        <span className="text-xs text-muted-foreground">
          Saving {entries.length} placement{entries.length === 1 ? "" : "s"}.
        </span>
        <Button type="button" onClick={handleSave} disabled={saving}>
          {saving && (
            <Loader2 className="mr-1 h-4 w-4 animate-spin" aria-hidden />
          )}
          Save placement map
        </Button>
      </div>
    </div>
  );
}

