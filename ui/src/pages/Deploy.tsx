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

import { GitBranchIcon, PlayIcon, PlusIcon, RefreshCwIcon, Trash2Icon } from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import {
  createDeployTarget,
  deleteDeployTarget,
  listDeployRuns,
  listDeployTargets,
  runColor,
  runLabel,
  triggerDeployRun,
  type DeployTarget,
  type LintFinding,
} from "@/api/deploy";
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

      <section className="space-y-3">
        <h2 className="text-sm font-medium uppercase tracking-wide text-muted-foreground">
          Targets
        </h2>
        {targets.length === 0 ? (
          <Card>
            <CardContent className="p-6 text-sm text-muted-foreground">
              No deploy targets yet. Click "New target" to register a workflow.
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
                        <td className="px-3 py-2">{target?.name ?? r.target_id}</td>
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
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          {target.has_credential ? (
            <Badge variant="outline" style={{ color: "var(--status-healthy, #22c55e)" }}>
              PAT set
            </Badge>
          ) : (
            <Badge variant="outline" style={{ color: "var(--status-critical, #ef4444)" }}>
              PAT missing
            </Badge>
          )}
          {target.config_id && <span>config pinned</span>}
        </div>
        <div className="flex gap-2">
          <Button
            size="sm"
            disabled={!target.has_credential}
            onClick={onTrigger}
          >
            <PlayIcon className="mr-2 h-3.5 w-3.5" /> Run deployment
          </Button>
          <Button size="sm" variant="outline" onClick={onDelete}>
            <Trash2Icon className="h-3.5 w-3.5" />
          </Button>
        </div>
      </CardContent>
    </Card>
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
  const [owner, setOwner] = useState("");
  const [repo, setRepo] = useState("");
  const [workflow, setWorkflow] = useState("");
  const [branch, setBranch] = useState("main");
  const [pat, setPat] = useState("");
  const [defaultInputsJSON, setDefaultInputsJSON] = useState("{}");
  const [configID, setConfigID] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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
        github_owner: owner,
        github_repo: repo,
        github_workflow: workflow,
        github_branch: branch,
        default_inputs: parsed,
        config_id: configID || undefined,
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
          <SheetDescription>
            Register a GitHub Actions workflow that Squadron is allowed to
            dispatch. The PAT is encrypted at rest.
          </SheetDescription>
        </SheetHeader>
        <div className="space-y-4 p-4">
          <Field label="Name">
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Prod OTel deploy" />
          </Field>
          <div className="grid grid-cols-2 gap-2">
            <Field label="Owner">
              <Input value={owner} onChange={(e) => setOwner(e.target.value)} placeholder="my-org" />
            </Field>
            <Field label="Repo">
              <Input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="otel-deploy" />
            </Field>
          </div>
          <Field label="Workflow file">
            <Input value={workflow} onChange={(e) => setWorkflow(e.target.value)} placeholder="deploy-otel.yml" />
          </Field>
          <Field label="Branch">
            <Input value={branch} onChange={(e) => setBranch(e.target.value)} placeholder="main" />
          </Field>
          <Field label="GitHub PAT (actions:write + contents:read)">
            <Input value={pat} onChange={(e) => setPat(e.target.value)} type="password" placeholder="ghp_…" />
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
      const hosts = hostsRaw
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
