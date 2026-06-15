/**
 * Deploy page — v0.34.
 *
 * Surfaces the GitHub Actions deploy integration. Two main sections:
 *
 *   1. Targets — the saved workflow_dispatch configurations
 *      (GitHub coords + encrypted PAT + default inputs).
 *   2. Recent runs — the history of triggers, with status/conclusion
 *      and a link out to the GitHub run.
 *
 * "Run deployment" opens a modal that pre-loads default inputs from
 * the target, lets you override per-run, takes an expected-host list
 * for v0.32 inventory reconciliation, and POSTs to /api/v1/deploy/runs.
 *
 * Lint errors come back as a 422 with a `lint_findings` array — the
 * modal renders them inline so the operator can go fix the config
 * and retry without leaving the page.
 */

import {
  AlertTriangleIcon,
  CheckCircle2Icon,
  GitBranchIcon,
  PlayIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  ShieldCheckIcon,
  Trash2Icon,
  XCircleIcon,
} from "lucide-react";
import { useMemo, useState } from "react";
import useSWR from "swr";

import {
  createDeployTarget,
  deleteDeployTarget,
  fetchDeployInventory,
  hostStatusColor,
  listDeployRuns,
  listDeployTargets,
  redeployRun,
  runColor,
  runLabel,
  triggerDeployRun,
  validateDeployTarget,
  type DeployTarget,
  type LintFinding,
  type ValidationResult,
} from "@/api/deploy";
import { DeployMetricsPanel } from "@/components/deploy/DeployMetricsPanel";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";

export default function DeployPage() {
  const targetsQ = useSWR("deploy-targets", listDeployTargets, {
    refreshInterval: 15_000,
  });
  const runsQ = useSWR("deploy-runs", () => listDeployRuns(), {
    refreshInterval: 5_000,
  });

  const [showNew, setShowNew] = useState(false);
  const [triggerFor, setTriggerFor] = useState<DeployTarget | null>(null);

  if (targetsQ.data && targetsQ.data.enabled === false) {
    return (
      <div className="space-y-4 p-6">
        <h1 className="text-2xl font-semibold">Deploy</h1>
        <Card>
          <CardContent className="space-y-2 p-6 text-sm">
            <p className="font-medium">Deploy integration not configured.</p>
            <p className="text-muted-foreground">
              The deploy feature requires a 32-byte secretbox key in the{" "}
              <code className="font-mono">SQUADRON_DEPLOY_KEY</code>{" "}
              environment variable. Generate one with:
            </p>
            <pre className="rounded bg-muted p-2 text-xs">
              head -c 32 /dev/urandom | base64
            </pre>
            <p className="text-muted-foreground">
              Set the variable in your environment (or
              {" "}
              <code className="font-mono">.env</code> /
              {" "}
              <code className="font-mono">squadron.yaml</code>'s process env)
              and restart Squadron. See{" "}
              <a className="underline" href="https://github.com/devopsmike2/squadron/blob/main/docs/deploy.md">docs/deploy.md</a>.
            </p>
          </CardContent>
        </Card>
      </div>
    );
  }

  const targets = targetsQ.data?.items ?? [];
  const runs = runsQ.data?.items ?? [];

  // v0.35: highlight any deploy currently moving through the
  // pipeline so the operator doesn't fire a second one
  // accidentally.
  const inFlight = useMemo(
    () => runs.filter((r) => r.status === "queued" || r.status === "in_progress"),
    [runs],
  );

  return (
    <div className="space-y-6 p-6">
      <header className="flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Deploy</h1>
          <p className="text-sm text-muted-foreground">
            Fire GitHub Actions workflows that deploy OTel collectors; pre-flight
            config lint + post-deploy verification close the loop.
          </p>
        </div>
        <Button onClick={() => setShowNew(true)}>
          <PlusIcon className="mr-2 h-4 w-4" /> New target
        </Button>
      </header>

      {inFlight.length > 0 && (
        <Card style={{ borderColor: "var(--info, #3b82f6)" }}>
          <CardContent className="flex items-center gap-3 p-3 text-sm">
            <span
              className="inline-block h-2 w-2 animate-pulse rounded-full"
              style={{ background: "var(--info, #3b82f6)" }}
            />
            <span>
              <span className="font-medium">{inFlight.length}</span> deploy
              {inFlight.length === 1 ? " is" : "s are"} in flight.{" "}
              {inFlight.slice(0, 3).map((r, i) => {
                const t = targets.find((tt) => tt.id === r.target_id);
                return (
                  <span key={r.id} className="text-muted-foreground">
                    {i > 0 ? ", " : ""}
                    {t?.name ?? r.target_id} ({r.status})
                  </span>
                );
              })}
            </span>
          </CardContent>
        </Card>
      )}

      {/* v0.39 DORA metrics strip. Sits above Targets so an SRE
          director glancing at this page sees the operational
          health of the deploy pipeline before the per-target
          rows. Hidden gracefully (renders "—" tiles) when no runs
          exist yet, since the visual KPI tile is more informative
          than an extra "no data" placeholder. */}
      <DeployMetricsPanel />

      <section className="space-y-3">
        <h2 className="text-sm font-medium uppercase tracking-wide text-muted-foreground">
          Targets
        </h2>
        {targets.length === 0 ? (
          <Card>
            <CardContent className="space-y-3 p-6 text-sm">
              <div className="text-base font-medium">
                Your first deploy in 3 steps
              </div>
              <ol className="list-decimal space-y-2 pl-5 text-muted-foreground">
                <li>
                  Mint a fine-grained GitHub PAT on the repo with{" "}
                  <code className="font-mono">actions:write</code> +{" "}
                  <code className="font-mono">contents:read</code>.
                </li>
                <li>
                  Click <span className="font-medium text-foreground">New target</span> and fill in
                  owner / repo / workflow file / branch + paste the PAT. If your
                  workflow uses an Ansible-style{" "}
                  <code className="font-mono">inventory.ini</code>, set its path
                  too — Squadron reads it at trigger time.
                </li>
                <li>
                  Click <span className="font-medium text-foreground">Validate</span> on the
                  resulting card to confirm Squadron can reach the workflow and
                  read the inventory before your first real deploy.
                </li>
              </ol>
              <Button className="mt-2" onClick={() => setShowNew(true)}>
                <PlusIcon className="mr-2 h-4 w-4" /> New target
              </Button>
            </CardContent>
          </Card>
        ) : (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            {targets.map((t) => (
              <TargetCard
                key={t.id}
                target={t}
                onDelete={async () => {
                  if (!confirm(`Delete deploy target "${t.name}"?`)) return;
                  await deleteDeployTarget(t.id);
                  targetsQ.mutate();
                }}
                onTrigger={() => setTriggerFor(t)}
              />
            ))}
          </div>
        )}
      </section>

      <section className="space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-medium uppercase tracking-wide text-muted-foreground">
            Recent runs
          </h2>
          <Button size="sm" variant="ghost" onClick={() => runsQ.mutate()}>
            <RefreshCwIcon className="mr-2 h-3.5 w-3.5" /> Refresh
          </Button>
        </div>
        {runs.length === 0 ? (
          <Card>
            <CardContent className="p-6 text-sm text-muted-foreground">
              No deploys yet. Click "Run deployment" on a target to dispatch.
            </CardContent>
          </Card>
        ) : (
          <Card>
            <CardContent className="p-0">
              <table className="w-full text-sm">
                <thead className="border-b text-left text-xs uppercase tracking-wide text-muted-foreground">
                  <tr>
                    <th className="px-3 py-2">When</th>
                    <th className="px-3 py-2">Target</th>
                    <th className="px-3 py-2">By</th>
                    <th className="px-3 py-2">Status</th>
                    <th className="px-3 py-2">Hosts</th>
                    <th className="px-3 py-2">GitHub</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.map((r) => {
                    const target = targets.find((t) => t.id === r.target_id);
                    return (
                      <tr key={r.id} className="border-b">
                        <td className="px-3 py-2 font-tabular text-xs text-muted-foreground">
                          {new Date(r.requested_at).toLocaleString()}
                        </td>
                        <td className="px-3 py-2">
                          {target?.name ?? r.target_id}
                          {r.status === "completed" && (
                            <Button
                              size="sm"
                              variant="ghost"
                              className="ml-2 h-6 px-2 text-[11px]"
                              title="Re-fire this exact deploy with the same inputs"
                              onClick={async () => {
                                if (!confirm("Redeploy with the same inputs as this run?")) return;
                                try {
                                  await redeployRun(r.id);
                                  runsQ.mutate();
                                } catch (e) {
                                  alert(String((e as Error).message ?? e));
                                }
                              }}
                            >
                              <RotateCcwIcon className="mr-1 h-3 w-3" /> Redeploy
                            </Button>
                          )}
                        </td>
                        <td className="px-3 py-2 text-xs text-muted-foreground">
                          {r.requested_by || "—"}
                        </td>
                        <td className="px-3 py-2">
                          <Badge
                            variant="outline"
                            style={{
                              borderColor: runColor(r),
                              color: runColor(r),
                            }}
                          >
                            {runLabel(r)}
                          </Badge>
                        </td>
                        <td className="px-3 py-2 text-xs">
                          {r.expected_hosts?.length ?? 0}
                        </td>
                        <td className="px-3 py-2 text-xs">
                          {r.github_run_url ? (
                            <a
                              className="text-primary underline"
                              href={r.github_run_url}
                              target="_blank"
                              rel="noreferrer"
                            >
                              run #{r.github_run_id}
                            </a>
                          ) : (
                            "—"
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </CardContent>
          </Card>
        )}
      </section>

      <NewTargetSheet
        open={showNew}
        onClose={() => setShowNew(false)}
        onCreated={() => {
          setShowNew(false);
          targetsQ.mutate();
        }}
      />
      {triggerFor && (
        <TriggerSheet
          target={triggerFor}
          onClose={() => setTriggerFor(null)}
          onTriggered={() => {
            setTriggerFor(null);
            runsQ.mutate();
          }}
        />
      )}
    </div>
  );
}

function TargetCard({
  target,
  onDelete,
  onTrigger,
}: {
  target: DeployTarget;
  onDelete: () => void;
  onTrigger: () => void;
}) {
  const [validation, setValidation] = useState<ValidationResult | null>(null);
  const [validating, setValidating] = useState(false);
  const last = target.last_run;
  const lastTone = last
    ? last.status !== "completed"
      ? "var(--info, #3b82f6)"
      : last.conclusion === "success"
        ? "var(--status-healthy, #22c55e)"
        : "var(--status-critical, #ef4444)"
    : "var(--muted-foreground)";
  const lastWhen = last ? new Date(last.requested_at).toLocaleString() : null;
  const lastVerb = last
    ? last.status !== "completed"
      ? "running"
      : last.conclusion === "success"
        ? "succeeded"
        : last.conclusion || "completed"
    : "never deployed";

  async function runValidate() {
    setValidating(true);
    try {
      const r = await validateDeployTarget(target.id);
      setValidation(r);
    } finally {
      setValidating(false);
    }
  }

  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <div className="flex items-start justify-between gap-3">
          <div>
            <div className="font-medium">{target.name}</div>
            <div className="text-xs text-muted-foreground">
              {target.github_owner}/{target.github_repo} →{" "}
              <code className="font-mono">{target.github_workflow}</code>
            </div>
          </div>
          <Badge variant="outline" className="shrink-0">
            <GitBranchIcon className="mr-1 h-3 w-3" />
            {target.github_branch}
          </Badge>
        </div>
        <div className="flex flex-wrap items-center gap-2 text-xs">
          {target.has_credential ? (
            <Badge variant="outline" style={{ color: "var(--status-healthy, #22c55e)" }}>
              PAT set
            </Badge>
          ) : (
            <Badge variant="outline" style={{ color: "var(--status-critical, #ef4444)" }}>
              PAT missing
            </Badge>
          )}
          {target.inventory_path && (
            <Badge variant="outline" className="text-muted-foreground">
              inventory: {target.inventory_path.split("/").pop()}
            </Badge>
          )}
          {target.config_id && (
            <Badge variant="outline" className="text-muted-foreground">
              config pinned
            </Badge>
          )}
          <Badge variant="outline" style={{ color: lastTone, borderColor: lastTone }}>
            Last: {lastVerb}
            {lastWhen ? ` · ${lastWhen}` : ""}
          </Badge>
        </div>
        {validation && (
          <ValidationChecklist result={validation} onDismiss={() => setValidation(null)} />
        )}
        <div className="flex gap-2">
          <Button
            size="sm"
            disabled={!target.has_credential}
            onClick={onTrigger}
          >
            <PlayIcon className="mr-2 h-3.5 w-3.5" /> Run deployment
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={!target.has_credential || validating}
            onClick={runValidate}
            title="Probe GitHub auth + workflow + inventory + lint without firing a deploy"
          >
            <ShieldCheckIcon className="mr-2 h-3.5 w-3.5" />
            {validating ? "Validating…" : "Validate"}
          </Button>
          <Button size="sm" variant="outline" onClick={onDelete}>
            <Trash2Icon className="h-3.5 w-3.5" />
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

function ValidationChecklist({
  result,
  onDismiss,
}: {
  result: ValidationResult;
  onDismiss: () => void;
}) {
  const rows: { key: string; label: string; check: ValidationResult["github_auth"] }[] = [
    { key: "auth", label: "GitHub auth", check: result.github_auth },
    { key: "wf", label: "Workflow exists", check: result.workflow_exists },
    { key: "inv", label: "Inventory readable", check: result.inventory },
    { key: "lint", label: "Lint check", check: result.lint_check },
  ];
  return (
    <div className="rounded border bg-muted/30 p-2 text-xs">
      <div className="mb-1 flex items-center justify-between">
        <div className="font-medium">
          {result.overall_ok ? "All checks passed" : "Some checks failed"}
        </div>
        <button
          type="button"
          className="text-[10px] text-muted-foreground underline"
          onClick={onDismiss}
        >
          dismiss
        </button>
      </div>
      <ul className="space-y-1">
        {rows.map((r) => {
          const Icon =
            r.check.status === "ok"
              ? CheckCircle2Icon
              : r.check.status === "fail"
                ? XCircleIcon
                : AlertTriangleIcon;
          const color =
            r.check.status === "ok"
              ? "var(--status-healthy, #22c55e)"
              : r.check.status === "fail"
                ? "var(--status-critical, #ef4444)"
                : r.check.status === "skip"
                  ? "var(--muted-foreground)"
                  : "var(--status-warn, #eab308)";
          return (
            <li key={r.key} className="flex items-start gap-2">
              <Icon className="mt-0.5 h-3.5 w-3.5 shrink-0" style={{ color }} aria-hidden />
              <span>
                <span className="font-medium">{r.label}:</span> {r.check.message}
              </span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function NewTargetSheet({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  // v0.41 — provider selector. "github" is the default to preserve
  // legacy behavior; "azure_devops" routes through the Azure DevOps
  // Pipelines provider. The same backend column stores both.
  const [provider, setProvider] = useState<
    "github" | "azure_devops" | "ansible_tower"
  >("github");
  const [owner, setOwner] = useState("");
  const [repo, setRepo] = useState("");
  const [workflow, setWorkflow] = useState("");
  const [branch, setBranch] = useState("main");
  const [pat, setPat] = useState("");
  const [defaultInputsJSON, setDefaultInputsJSON] = useState("{}");
  const [configID, setConfigID] = useState("");
  const [inventoryPath, setInventoryPath] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Per-provider field labels. The shared backend columns map to
  // very different concepts (GitHub repo vs Azure DevOps project,
  // workflow file vs pipeline ID), and operators get confused if
  // we just call everything "GitHub" in the form. Swap labels
  // based on the picker so each provider's mental model is honored.
  // Per-provider field labels. Each backend uses very different
  // language for the same conceptual fields; swapping labels keeps
  // each provider's mental model intact.
  const labelsByProvider = {
    github: {
      sectionHelp:
        "Register a GitHub Actions workflow that Squadron is allowed to dispatch. The PAT is encrypted at rest.",
      owner: "Owner",
      ownerPlaceholder: "my-org",
      repo: "Repo",
      repoPlaceholder: "otel-deploy",
      workflow: "Workflow file",
      workflowPlaceholder: "deploy-otel.yml",
      patHint: "GitHub PAT (actions:write + contents:read)",
      patPlaceholder: "ghp_…",
    },
    azure_devops: {
      sectionHelp:
        "Register an Azure DevOps Pipelines run that Squadron is allowed to dispatch. The PAT is encrypted at rest.",
      owner: "Organization",
      ownerPlaceholder: "my-azdo-org",
      repo: "Project (or Project/Repo)",
      repoPlaceholder: "MyProject",
      workflow: "Pipeline ID (numeric)",
      workflowPlaceholder: "42",
      patHint: "Azure DevOps PAT (Build: read & execute, Code: read)",
      patPlaceholder: "azp_…",
    },
    ansible_tower: {
      sectionHelp:
        "Register an Ansible Tower / AWX job template that Squadron is allowed to launch. The token is encrypted at rest.",
      owner: "Tower host",
      ownerPlaceholder: "tower.your-co.com",
      repo: "(not used)",
      repoPlaceholder: "",
      workflow: "Job template ID (numeric)",
      workflowPlaceholder: "42",
      patHint: "Tower OAuth2 token (Bearer auth)",
      patPlaceholder: "token…",
    },
  } as const;
  const labels = labelsByProvider[provider];

  async function submit() {
    setError(null);
    setBusy(true);
    try {
      let parsed: Record<string, string> = {};
      try {
        parsed = JSON.parse(defaultInputsJSON || "{}");
      } catch {
        setError("Default inputs must be valid JSON (object of string→string).");
        setBusy(false);
        return;
      }
      await createDeployTarget({
        name,
        provider,
        github_owner: owner,
        github_repo: repo,
        github_workflow: workflow,
        github_branch: branch,
        default_inputs: parsed,
        config_id: configID || undefined,
        inventory_path: inventoryPath || undefined,
        pat,
      });
      onCreated();
    } catch (e) {
      setError(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-[480px] sm:max-w-[480px]">
        <SheetHeader>
          <SheetTitle>New deploy target</SheetTitle>
          <SheetDescription>{labels.sectionHelp}</SheetDescription>
        </SheetHeader>
        <div className="space-y-4 p-4">
          <Field label="Name">
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Prod OTel deploy" />
          </Field>
          {/* v0.41 — provider picker. Two cards side-by-side so the
              choice is visible up-front and operators don't get
              halfway through the form before discovering they
              picked the wrong one. */}
          <Field label="Provider">
            <div className="grid grid-cols-3 gap-2">
              {(
                [
                  { id: "github", title: "GitHub Actions", sub: "workflow_dispatch + Contents API" },
                  { id: "azure_devops", title: "Azure DevOps", sub: "Pipelines + Git Items API" },
                  { id: "ansible_tower", title: "Ansible Tower", sub: "Job templates + Bearer auth" },
                ] as const
              ).map((opt) => (
                <button
                  key={opt.id}
                  type="button"
                  onClick={() => setProvider(opt.id)}
                  className={`rounded-md border px-3 py-2 text-left text-xs transition-colors ${
                    provider === opt.id
                      ? "border-primary/60 bg-primary/10 text-foreground"
                      : "border-border bg-card/40 text-muted-foreground hover:bg-accent/30"
                  }`}
                >
                  <div className="font-medium text-foreground">{opt.title}</div>
                  <div className="text-[10px] text-muted-foreground">
                    {opt.sub}
                  </div>
                </button>
              ))}
            </div>
          </Field>
          <div className="grid grid-cols-2 gap-2">
            <Field label={labels.owner}>
              <Input
                value={owner}
                onChange={(e) => setOwner(e.target.value)}
                placeholder={labels.ownerPlaceholder}
              />
            </Field>
            <Field label={labels.repo}>
              <Input
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                placeholder={labels.repoPlaceholder}
              />
            </Field>
          </div>
          <Field label={labels.workflow}>
            <Input
              value={workflow}
              onChange={(e) => setWorkflow(e.target.value)}
              placeholder={labels.workflowPlaceholder}
            />
          </Field>
          <Field label="Branch">
            <Input value={branch} onChange={(e) => setBranch(e.target.value)} placeholder="main" />
          </Field>
          <Field label={labels.patHint}>
            <Input
              value={pat}
              onChange={(e) => setPat(e.target.value)}
              type="password"
              placeholder={labels.patPlaceholder}
            />
          </Field>
          <Field label="Inventory path (optional — Ansible inventory.ini inside the repo)">
            <Input
              value={inventoryPath}
              onChange={(e) => setInventoryPath(e.target.value)}
              placeholder="winOtel/ansible/inventory.ini"
              className="font-mono text-xs"
            />
            <div className="mt-1 text-[11px] text-muted-foreground">
              When set, Squadron reads this file at trigger time and uses the
              parsed host list as the deploy's expected hosts. Match your
              workflow's <code>-i</code> path.
            </div>
          </Field>
          <Field label="Pinned config ID (optional — lint-checks before deploy)">
            <Input value={configID} onChange={(e) => setConfigID(e.target.value)} placeholder="" />
          </Field>
          <Field label="Default inputs (JSON object of string→string)">
            <Textarea
              value={defaultInputsJSON}
              onChange={(e) => setDefaultInputsJSON(e.target.value)}
              rows={4}
              placeholder='{"environment": "prod"}'
              className="font-mono text-xs"
            />
          </Field>
          {error && <div className="text-xs text-destructive">{error}</div>}
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={onClose}>Cancel</Button>
            <Button disabled={busy || !name || !owner || !repo || !workflow || !pat} onClick={submit}>
              {busy ? "Saving…" : "Save target"}
            </Button>
          </div>
        </div>
      </SheetContent>
    </Sheet>
  );
}

function TriggerSheet({
  target,
  onClose,
  onTriggered,
}: {
  target: DeployTarget;
  onClose: () => void;
  onTriggered: () => void;
}) {
  const [inputsJSON, setInputsJSON] = useState(
    JSON.stringify(target.default_inputs ?? {}, null, 2),
  );
  const [hostsRaw, setHostsRaw] = useState("");
  const [notes, setNotes] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lintFindings, setLintFindings] = useState<LintFinding[] | null>(null);

  // v0.34.1: when the target points at an inventory file, fetch
  // the parsed host list from GitHub so the operator can see
  // what's about to be deployed to. SWR refreshes on the modal
  // re-open, which is exactly when the file may have changed.
  const inventoryQ = useSWR(
    target.inventory_path
      ? ["deploy-inventory", target.id]
      : null,
    () => fetchDeployInventory(target.id),
    { revalidateOnFocus: false },
  );
  const usingInventoryFile = Boolean(target.inventory_path);

  async function fire() {
    setBusy(true);
    setError(null);
    setLintFindings(null);
    try {
      let inputs: Record<string, string> = {};
      try {
        inputs = JSON.parse(inputsJSON || "{}");
      } catch {
        setError("Inputs must be valid JSON.");
        setBusy(false);
        return;
      }
      // When the target uses an inventory file, the server reads
      // it at trigger time and ignores whatever client sends here —
      // we explicitly omit expected_hosts to make that intent clear
      // in the request log.
      const hosts = usingInventoryFile
        ? undefined
        : hostsRaw
            .split(/[\s,]+/)
            .map((s) => s.trim())
            .filter(Boolean);
      await triggerDeployRun({
        target_id: target.id,
        inputs,
        expected_hosts: hosts,
        notes: notes || undefined,
      });
      onTriggered();
    } catch (e: any) {
      // 422 lint-blocked: surface the findings inline
      const msg = String(e?.message ?? e);
      if (e?.payload?.lint_findings) {
        setLintFindings(e.payload.lint_findings);
      } else {
        setError(msg);
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <Sheet open onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-[520px] sm:max-w-[520px]">
        <SheetHeader>
          <SheetTitle>Run "{target.name}"</SheetTitle>
          <SheetDescription>
            Dispatches{" "}
            <code className="font-mono">
              {target.github_owner}/{target.github_repo}@{target.github_branch}
            </code>{" "}
            workflow{" "}
            <code className="font-mono">{target.github_workflow}</code>.
          </SheetDescription>
        </SheetHeader>
        <div className="space-y-4 p-4">
          <Field label="Inputs (merged with the target's defaults)">
            <Textarea
              value={inputsJSON}
              onChange={(e) => setInputsJSON(e.target.value)}
              rows={6}
              className="font-mono text-xs"
            />
          </Field>
          {usingInventoryFile ? (
            <Field label={`Hosts from inventory file (${target.inventory_path})`}>
              <div className="rounded border bg-muted/40 p-2 text-xs max-h-48 overflow-auto">
                {inventoryQ.isLoading ? (
                  <span className="text-muted-foreground">Loading inventory…</span>
                ) : inventoryQ.data?.fetch_error ? (
                  <span className="text-destructive">
                    Couldn't read inventory.ini: {inventoryQ.data.fetch_error}
                  </span>
                ) : inventoryQ.data?.hosts && inventoryQ.data.hosts.length > 0 ? (
                  <ul className="space-y-1 font-mono">
                    {inventoryQ.data.hosts.map((h) => (
                      <li key={h.hostname} className="flex items-center gap-2">
                        <span
                          className="inline-block h-2 w-2 shrink-0 rounded-full"
                          style={{ background: hostStatusColor(h.status) }}
                          title={
                            h.status === "healthy"
                              ? `Healthy — last seen ${
                                  h.last_seen
                                    ? new Date(h.last_seen).toLocaleString()
                                    : "?"
                                }`
                              : h.status === "silent"
                                ? `Silent for ${h.silence_for}`
                                : "Never seen by Squadron"
                          }
                        />
                        <span>{h.hostname}</span>
                        {h.status === "silent" && (
                          <span className="text-[10px] text-muted-foreground">
                            silent {h.silence_for}
                          </span>
                        )}
                        {h.status === "never_seen" && (
                          <span className="text-[10px] text-muted-foreground">
                            never seen
                          </span>
                        )}
                      </li>
                    ))}
                  </ul>
                ) : (
                  <span className="text-muted-foreground">
                    No hosts parsed from {target.inventory_path}.
                  </span>
                )}
              </div>
              <div className="mt-1 text-[11px] text-muted-foreground">
                Green = healthy / yellow = silent / red = never seen by Squadron.
                Re-fetched from GitHub at trigger time and registered into
                expected_agents so v0.32 reconciliation can flag any that don't
                check in.{" "}
                <button
                  type="button"
                  className="underline"
                  onClick={() => inventoryQ.mutate()}
                >
                  Refresh
                </button>
              </div>
            </Field>
          ) : (
            <Field label="Expected hosts (comma- or whitespace-separated)">
              <Textarea
                value={hostsRaw}
                onChange={(e) => setHostsRaw(e.target.value)}
                rows={3}
                placeholder="host01, host02, host03"
                className="font-mono text-xs"
              />
              <div className="mt-1 text-[11px] text-muted-foreground">
                These get registered into expected_agents on success, so v0.32
                inventory reconciliation can flag any that don't check in.
              </div>
            </Field>
          )}
          <Field label="Notes (optional)">
            <Input value={notes} onChange={(e) => setNotes(e.target.value)} placeholder="canary batch 3" />
          </Field>
          {lintFindings && (
            <div className="space-y-1 rounded border border-destructive/40 bg-destructive/10 p-3 text-xs">
              <div className="font-medium text-destructive">
                Config lint blocked deploy — fix these first:
              </div>
              <ul className="space-y-1">
                {lintFindings.map((f, i) => (
                  <li key={i}>
                    <span className="font-mono">[{f.rule}]</span> {f.message}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {error && <div className="text-xs text-destructive">{error}</div>}
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={onClose}>Cancel</Button>
            <Button disabled={busy} onClick={fire}>
              {busy ? "Firing…" : "Run deployment"}
            </Button>
          </div>
        </div>
      </SheetContent>
    </Sheet>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <Label className="text-xs font-medium">{label}</Label>
      {children}
    </div>
  );
}
