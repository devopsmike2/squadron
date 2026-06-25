// ProposerPlayground — v0.84 (Arc C slice 3 close).
//
// Operator-facing tool for dogfooding the proposer without seeding
// a fake cost spike into the application store. The operator fills
// in a CostSpikeContext, clicks Run, and sees the proposer's
// response (kind, reasoning, step count, tokens, estimated cost)
// without any side effects — no rollouts, no plans, no audit
// events. Useful for:
//
//   - Validating a prompt change before pushing it through CI
//   - Demoing the proposer's decision framework to a stakeholder
//   - Sanity-checking what the model would do under a real spike
//     before the bridge daemon fires
//
// The starter scenarios are a hand-picked subset of the v0.83
// bench corpus — same shapes, surfaced as one-click prefills so
// the operator can iterate without retyping. Future v0.85+ work
// can refactor the bench corpus into a shared module + endpoint
// so the playground stays in sync without copy.

import { Loader2, Play, Sparkles } from "lucide-react";
import { useState } from "react";

import {
  proposerPreview,
  useAICapabilities,
  type ProposerPreviewRequest,
  type ProposerPreviewResponse,
} from "@/api/ai";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

// emptyForm is the form's initial state — every field present so
// React doesn't churn between controlled/uncontrolled inputs.
const emptyForm: ProposerPreviewRequest = {
  spike_id: "",
  signal: "metrics",
  severity: "warn",
  baseline_monthly_usd: 0,
  peak_monthly_usd: 0,
  peak_pct_above_baseline: 0,
  top_agents: [],
  top_attributes: [],
  group_id: "",
  group_name: "",
  recent_lint_findings: [],
  recent_recommendations: [],
};

// Starter scenarios — same shapes as the v0.83 bench corpus.
// Subset chosen to demonstrate the three response classes the
// operator most often needs to reason about.
const scenarios: Array<{
  name: string;
  blurb: string;
  request: ProposerPreviewRequest;
}> = [
  {
    name: "Single attr → rollout",
    blurb: "1 high-cardinality attribute. Should produce a single rollout.",
    request: {
      ...emptyForm,
      spike_id: "playground-rollout-1",
      severity: "warn",
      baseline_monthly_usd: 400,
      peak_monthly_usd: 800,
      peak_pct_above_baseline: 100,
      group_id: "prod-utility-fleet",
      group_name: "Prod utility fleet",
      top_attributes: ["container.id"],
      top_agents: ["agent-1"],
    },
  },
  {
    name: "Two attrs → plan",
    blurb:
      "2 independent attrs (the v0.82 #550 scenario). Should produce a 2-step plan.",
    request: {
      ...emptyForm,
      spike_id: "playground-plan-1",
      severity: "critical",
      baseline_monthly_usd: 400,
      peak_monthly_usd: 1648,
      peak_pct_above_baseline: 312,
      group_id: "prod-utility-fleet",
      group_name: "Prod utility fleet",
      top_attributes: ["container.id", "k8s.pod.uid"],
      top_agents: ["agent-1"],
    },
  },
  {
    name: "Empty attribution → decline",
    blurb: "No top attributes; model should decline rather than guess.",
    request: {
      ...emptyForm,
      spike_id: "playground-decline-1",
      severity: "warn",
      baseline_monthly_usd: 500,
      peak_monthly_usd: 1500,
      peak_pct_above_baseline: 200,
      group_id: "prod-utility-fleet",
      group_name: "Prod utility fleet",
    },
  },
];

// listAsText / textToList — top_attributes and top_agents are
// string[] on the wire; the form renders them as one-per-line
// textareas. These helpers do the round-trip without imposing
// strict validation (an empty line is dropped; leading/trailing
// whitespace stripped).
function listAsText(values: string[]): string {
  return values.join("\n");
}
function textToList(s: string): string[] {
  return s
    .split("\n")
    .map((v) => v.trim())
    .filter((v) => v.length > 0);
}

export default function ProposerPlaygroundPage() {
  const { capabilities, loading: capLoading } = useAICapabilities();
  const [form, setForm] = useState<ProposerPreviewRequest>(emptyForm);
  const [submitting, setSubmitting] = useState(false);
  const [response, setResponse] = useState<ProposerPreviewResponse | null>(
    null,
  );
  const [error, setError] = useState<string | null>(null);

  const onRun = async () => {
    if (submitting) return;
    setSubmitting(true);
    setError(null);
    setResponse(null);
    try {
      const r = await proposerPreview(form);
      setResponse(r);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };

  if (capLoading) return null;
  if (!capabilities?.enabled) {
    return (
      <div className="p-6">
        <h1 className="text-2xl font-semibold">Proposer playground</h1>
        <p className="mt-2 text-sm text-muted-foreground">
          AI assist is not configured on this Squadron deployment. Set
          <code className="mx-1 rounded bg-muted px-1 py-0.5 font-mono text-xs">
            ANTHROPIC_API_KEY
          </code>
          in the server config and enable <code>ai.enabled</code> to use the
          playground.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-4 p-6">
      <div>
        <div className="flex items-center gap-2">
          <Sparkles className="h-5 w-5 text-violet-500" />
          <h1 className="text-2xl font-semibold">Proposer playground</h1>
        </div>
        <p className="text-muted-foreground text-sm">
          Hand-craft a cost spike context and see what the proposer would
          create. No side effects — nothing is persisted, no rollouts or plans
          are created.
        </p>
      </div>

      {/* Starter scenarios — one-click prefill for the operator. */}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">
            Starter scenarios
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2">
          {scenarios.map((s) => (
            <Button
              key={s.name}
              variant="outline"
              size="sm"
              onClick={() => {
                setForm(s.request);
                setResponse(null);
                setError(null);
              }}
              title={s.blurb}
            >
              {s.name}
            </Button>
          ))}
        </CardContent>
      </Card>

      {/* Form. Two-column grid for desktop; collapses on narrow
          viewports. Fields grouped by purpose. */}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">
            Cost spike context
          </CardTitle>
        </CardHeader>
        <CardContent className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div className="space-y-1">
            <Label htmlFor="spike_id">Spike ID</Label>
            <Input
              id="spike_id"
              value={form.spike_id}
              onChange={(e) => setForm({ ...form, spike_id: e.target.value })}
              placeholder="spike-abc"
            />
          </div>
          <div className="grid grid-cols-2 gap-2">
            <div className="space-y-1">
              <Label htmlFor="signal">Signal</Label>
              <Input
                id="signal"
                value={form.signal}
                onChange={(e) => setForm({ ...form, signal: e.target.value })}
                placeholder="metrics"
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="severity">Severity</Label>
              <Input
                id="severity"
                value={form.severity}
                onChange={(e) => setForm({ ...form, severity: e.target.value })}
                placeholder="warn / critical"
              />
            </div>
          </div>

          <div className="space-y-1">
            <Label htmlFor="baseline">Baseline $/mo</Label>
            <Input
              id="baseline"
              type="number"
              value={form.baseline_monthly_usd}
              onChange={(e) =>
                setForm({
                  ...form,
                  baseline_monthly_usd: Number(e.target.value),
                })
              }
            />
          </div>
          <div className="grid grid-cols-2 gap-2">
            <div className="space-y-1">
              <Label htmlFor="peak">Peak $/mo</Label>
              <Input
                id="peak"
                type="number"
                value={form.peak_monthly_usd}
                onChange={(e) =>
                  setForm({
                    ...form,
                    peak_monthly_usd: Number(e.target.value),
                  })
                }
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="pct">% over baseline</Label>
              <Input
                id="pct"
                type="number"
                value={form.peak_pct_above_baseline}
                onChange={(e) =>
                  setForm({
                    ...form,
                    peak_pct_above_baseline: Number(e.target.value),
                  })
                }
              />
            </div>
          </div>

          <div className="space-y-1">
            <Label htmlFor="group_id">Group ID</Label>
            <Input
              id="group_id"
              value={form.group_id}
              onChange={(e) => setForm({ ...form, group_id: e.target.value })}
              placeholder="prod-utility-fleet"
            />
          </div>
          <div className="space-y-1">
            <Label htmlFor="group_name">Group name</Label>
            <Input
              id="group_name"
              value={form.group_name}
              onChange={(e) => setForm({ ...form, group_name: e.target.value })}
              placeholder="Prod utility fleet"
            />
          </div>

          <div className="space-y-1 md:col-span-2">
            <Label htmlFor="top_attrs">Top attributes (one per line)</Label>
            <textarea
              id="top_attrs"
              className="w-full min-h-[60px] rounded-md border bg-background px-3 py-2 font-mono text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              value={listAsText(form.top_attributes)}
              onChange={(e) =>
                setForm({ ...form, top_attributes: textToList(e.target.value) })
              }
              placeholder="container.id&#10;k8s.pod.uid"
            />
          </div>
          <div className="space-y-1 md:col-span-2">
            <Label htmlFor="top_agents">Top agents (one per line)</Label>
            <textarea
              id="top_agents"
              className="w-full min-h-[60px] rounded-md border bg-background px-3 py-2 font-mono text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              value={listAsText(form.top_agents)}
              onChange={(e) =>
                setForm({ ...form, top_agents: textToList(e.target.value) })
              }
              placeholder="agent-1&#10;agent-2"
            />
          </div>

          <div className="space-y-1 md:col-span-2">
            <Label htmlFor="findings">
              Recent lint findings (one per line)
            </Label>
            <textarea
              id="findings"
              className="w-full min-h-[60px] rounded-md border bg-background px-3 py-2 font-mono text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              value={listAsText(form.recent_lint_findings)}
              onChange={(e) =>
                setForm({
                  ...form,
                  recent_lint_findings: textToList(e.target.value),
                })
              }
              placeholder="high-cardinality-label"
            />
          </div>
          <div className="space-y-1 md:col-span-2">
            <Label htmlFor="recs">Recent recommendations (one per line)</Label>
            <textarea
              id="recs"
              className="w-full min-h-[60px] rounded-md border bg-background px-3 py-2 font-mono text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              value={listAsText(form.recent_recommendations)}
              onChange={(e) =>
                setForm({
                  ...form,
                  recent_recommendations: textToList(e.target.value),
                })
              }
              placeholder="Add k8sattributes processor"
            />
          </div>
        </CardContent>
      </Card>

      <div className="flex justify-end">
        <Button onClick={onRun} disabled={submitting} className="gap-1">
          {submitting ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <Play className="h-4 w-4" />
          )}
          {submitting ? "Running…" : "Run proposer"}
        </Button>
      </div>

      {error && (
        <Card>
          <CardContent className="p-4">
            <div className="text-sm font-medium text-red-700 dark:text-red-300">
              Proposer call failed
            </div>
            <div className="mt-1 text-xs text-muted-foreground">{error}</div>
          </CardContent>
        </Card>
      )}

      {response && <ResultPanel response={response} />}
    </div>
  );
}

// ResultPanel renders the proposer's response. We surface the
// fields the operator cares about: declined / kind / step count /
// reasoning / token usage / cost. The raw JSON is collapsed at the
// bottom for the operator who wants the exact wire shape (e.g.
// when diffing prompt changes).
function ResultPanel({ response }: { response: ProposerPreviewResponse }) {
  if (response.declined) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">
            Proposer declined
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-2 text-sm">
          <div className="text-muted-foreground">{response.reason}</div>
          <MeteringStrip response={response} />
        </CardContent>
      </Card>
    );
  }
  const kind = response.kind || "rollout";
  const stepCount = kind === "plan" ? (response.plan?.steps?.length ?? 0) : 1;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm font-medium">
          {kind === "plan"
            ? `Plan with ${stepCount} step${stepCount === 1 ? "" : "s"}`
            : "Single rollout"}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        {response.reasoning && (
          <div className="rounded border bg-muted/30 p-3 text-sm leading-relaxed">
            {response.reasoning}
          </div>
        )}
        {response.evidence && response.evidence.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {response.evidence.map((e, i) => (
              <span
                key={i}
                className="rounded border bg-muted/40 px-2 py-0.5 text-[11px] font-mono text-muted-foreground"
                title={e.description}
              >
                {e.kind} {e.id ? e.id.slice(0, 8) : ""}
              </span>
            ))}
          </div>
        )}
        <MeteringStrip response={response} />
        <details className="rounded border bg-muted/20 p-3">
          <summary className="cursor-pointer text-xs font-medium text-muted-foreground">
            Raw JSON
          </summary>
          <pre className="mt-2 overflow-auto text-[11px] leading-snug">
            {JSON.stringify(response, null, 2)}
          </pre>
        </details>
      </CardContent>
    </Card>
  );
}

function MeteringStrip({ response }: { response: ProposerPreviewResponse }) {
  return (
    <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
      <span>
        tokens in <span className="font-mono">{response.tokens_in ?? 0}</span>
      </span>
      <span>
        tokens out <span className="font-mono">{response.tokens_out ?? 0}</span>
      </span>
      <span>
        cost{" "}
        <span className="font-mono">${response.estimated_usd.toFixed(4)}</span>
      </span>
      {response.model && (
        <span>
          model <span className="font-mono">{response.model}</span>
        </span>
      )}
    </div>
  );
}
