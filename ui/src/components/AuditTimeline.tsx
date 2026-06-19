// AuditTimeline renders Squadron's audit log filtered by target. Mount it
// in an agent drawer, group panel, or unfiltered on a standalone /audit
// page. Each row is one event; expanding shows the JSON payload.
//
// Lives on a stable SWR cache key so the global EventSubscriber can
// invalidate it whenever an audit_event_recorded comes in over SSE.

import {
  Activity,
  AlertCircle,
  AlertTriangle,
  Bell,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  FileText,
  Layers,
  RefreshCw,
  Rocket,
  RotateCcw,
  Server,
  SkipForward,
  Sparkles,
  XCircle,
} from "lucide-react";
import { useState } from "react";
import ReactMarkdown from "react-markdown";
import useSWR, { mutate as swrMutate } from "swr";

import { explainAuditEvent, listAuditEvents } from "@/api/audit";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type { AuditEvent, AuditExplainResponse } from "@/types/audit";

interface AuditTimelineProps {
  /** Filter to a specific entity, e.g. agent/group/config/rule. */
  targetType?: string;
  targetId?: string;
  /** Max events to render (server-side cap is 1000). Default 50. */
  limit?: number;
  /** Heading shown above the list. Hidden when empty. */
  heading?: string;
}

export function AuditTimeline({
  targetType,
  targetId,
  limit = 50,
  heading,
}: AuditTimelineProps) {
  // Build a stable SWR key so multiple AuditTimeline instances filtered
  // to the same target share the cache, and the global EventSubscriber
  // can revalidate by key prefix if it wants to later.
  const key = `audit/${targetType ?? "*"}/${targetId ?? "*"}/${limit}`;

  const { data, error, isLoading } = useSWR<AuditEvent[]>(key, () =>
    listAuditEvents({
      target_type: targetType,
      target_id: targetId,
      limit,
    }),
  );

  if (isLoading) {
    return (
      <div className="text-xs text-muted-foreground px-2 py-3">
        Loading history…
      </div>
    );
  }
  if (error) {
    return (
      <div className="text-xs text-red-600 px-2 py-3">
        Failed to load audit log:{" "}
        {error instanceof Error ? error.message : String(error)}
      </div>
    );
  }
  if (!data || data.length === 0) {
    return (
      <div className="text-xs text-muted-foreground px-2 py-3">
        No audit events recorded yet for this target. As state changes happen
        (config pushes, drift transitions, rule edits) they'll appear here.
      </div>
    );
  }

  return (
    <div className="space-y-1">
      {heading && (
        <div className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-1">
          {heading}
        </div>
      )}
      <ul className="space-y-0.5">
        {data.map((e) => (
          <AuditRow key={e.id} event={e} />
        ))}
      </ul>
    </div>
  );
}

function AuditRow({ event }: { event: AuditEvent }) {
  const [expanded, setExpanded] = useState(false);
  const hasPayload =
    event.payload != null && Object.keys(event.payload).length > 0;
  // v0.57 — the explain affordance is available on every row, not just
  // rows with a payload, so we expand on click even when payload is
  // empty. Operators get more value from being able to explain
  // payloadless rows than from having a tighter list.
  const expandable = true;

  const icon = iconFor(event);
  const ts = new Date(event.timestamp);

  // Rollout stage_applied + empty_canary events get a first-class
  // "agents this stage touched" rendering, because that list is the
  // single most useful artifact for a post-mortem ("which hosts saw the
  // new config at stage 2?"). The full payload is still available below.
  const agentIDs = extractAgentIDs(event);

  return (
    <li>
      <button
        type="button"
        onClick={() => expandable && setExpanded((v) => !v)}
        disabled={!expandable}
        className="w-full text-left flex items-start gap-2 px-2 py-1.5 rounded hover:bg-muted/40 disabled:cursor-default"
      >
        <span className="shrink-0 mt-0.5">
          {expandable ? (
            expanded ? (
              <ChevronDown className="h-3 w-3 text-muted-foreground" />
            ) : (
              <ChevronRight className="h-3 w-3 text-muted-foreground" />
            )
          ) : (
            <span className="block w-3" />
          )}
        </span>
        <span className="shrink-0 mt-0.5">{icon}</span>
        <span className="min-w-0 flex-1">
          <span className="text-sm">{describe(event)}</span>
          <span className="block text-[11px] text-muted-foreground mt-0.5">
            {timeAgo(ts)} ·{" "}
            <span className="font-mono">{event.event_type}</span>
            {event.actor && (
              <>
                {" · "}
                <span>{event.actor}</span>
              </>
            )}
          </span>
        </span>
        <Badge
          variant="outline"
          className="text-[10px] uppercase shrink-0 mt-0.5"
        >
          {event.action}
        </Badge>
      </button>
      {expanded && (
        <div className="mx-6 mb-2 space-y-2">
          {agentIDs && agentIDs.length > 0 && (
            <div className="space-y-1">
              <div className="text-[11px] uppercase tracking-wider text-muted-foreground">
                Canary agents ({agentIDs.length})
              </div>
              <div className="flex flex-wrap gap-1">
                {agentIDs.map((id) => (
                  <span
                    key={id}
                    className="inline-block font-mono text-[10px] px-1.5 py-0.5 rounded bg-muted/60 border"
                    title={id}
                  >
                    {id.slice(0, 8)}
                  </span>
                ))}
              </div>
            </div>
          )}
          {hasPayload && (
            <pre className="text-[11px] font-mono whitespace-pre-wrap break-all bg-muted/40 rounded p-2 overflow-auto max-h-48">
              {JSON.stringify(event.payload, null, 2)}
            </pre>
          )}
          <ExplainPanel event={event} />
        </div>
      )}
    </li>
  );
}

// ExplainPanel renders the AI explanation surface on an expanded
// audit row. Three states:
// - The row already has a cached explanation: render it immediately
//   with a model + relative-time caption and a Regenerate button.
// - The operator has not asked yet: render a small "Explain" trigger.
// - The operator just clicked: show a loading spinner; on success
//   render the explanation; on failure render an inline retry.
function ExplainPanel({ event }: { event: AuditEvent }) {
  const [local, setLocal] = useState<AuditExplainResponse | null>(
    event.ai_explanation
      ? {
          explanation: event.ai_explanation,
          model: event.ai_explanation_model ?? "",
          generated_at: event.ai_explanation_generated_at ?? "",
          cached: true,
        }
      : null,
  );
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = async (regenerate = false) => {
    setLoading(true);
    setError(null);
    try {
      const resp = await explainAuditEvent(event.id, regenerate);
      setLocal(resp);
      // Invalidate any list caches that include this row so a fresh
      // fetch picks up the newly persisted ai_explanation. We do not
      // know which exact keys to bust, so we bust by prefix.
      void swrMutate(
        (key) => typeof key === "string" && key.startsWith("audit/"),
        undefined,
        { revalidate: true },
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  if (!local && !loading && !error) {
    return (
      <div className="flex items-center gap-2 pt-1">
        <Button
          size="sm"
          variant="outline"
          className="h-7 text-xs gap-1.5"
          onClick={(e) => {
            e.stopPropagation();
            void run(false);
          }}
        >
          <Sparkles className="h-3 w-3" />
          Explain this
        </Button>
        <span className="text-[10px] text-muted-foreground">
          AI summary of what happened. Cached on the row.
        </span>
      </div>
    );
  }

  if (loading) {
    return (
      <div className="text-xs text-muted-foreground italic pt-1 flex items-center gap-1.5">
        <Sparkles className="h-3 w-3 animate-pulse" />
        Generating explanation…
      </div>
    );
  }

  if (error && !local) {
    return (
      <div className="pt-1 space-y-1.5">
        <div className="text-xs text-rose-600">
          Could not generate an explanation. {error}
        </div>
        <Button
          size="sm"
          variant="outline"
          className="h-7 text-xs"
          onClick={(e) => {
            e.stopPropagation();
            void run(false);
          }}
        >
          Try again
        </Button>
      </div>
    );
  }

  if (!local) return null;

  return (
    <div className="border border-violet-500/20 rounded bg-violet-500/5 p-2 space-y-1.5">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-violet-700 dark:text-violet-300">
          <Sparkles className="h-3 w-3" />
          AI Explanation
        </div>
        <Button
          size="sm"
          variant="ghost"
          className="h-6 text-[10px] gap-1"
          onClick={(e) => {
            e.stopPropagation();
            void run(true);
          }}
          title="Regenerate, bypassing the cached explanation"
        >
          <RefreshCw className="h-3 w-3" />
          Regenerate
        </Button>
      </div>
      <div className="text-xs prose prose-xs max-w-none dark:prose-invert">
        <ReactMarkdown>{local.explanation}</ReactMarkdown>
      </div>
      <div className="text-[10px] text-muted-foreground">
        {local.model && <span>Generated by {local.model}</span>}
        {local.cached && <span> · served from cache</span>}
        {local.redaction_summary && (
          <span> · redacted: {local.redaction_summary}</span>
        )}
      </div>
      {error && (
        <div className="text-[11px] text-rose-600">
          Regenerate failed: {error}
        </div>
      )}
    </div>
  );
}

// extractAgentIDs returns the resolved agent ID list from a rollout
// stage event, if present. Mirrors the payload shape produced by
// engine.stageAuditPayload — defensive against missing/wrongly-typed
// fields because event payloads are freeform JSON.
function extractAgentIDs(event: AuditEvent): string[] | null {
  if (
    event.event_type !== "rollout.stage_applied" &&
    event.event_type !== "rollout.empty_canary"
  ) {
    return null;
  }
  const raw = event.payload?.agent_ids;
  if (!Array.isArray(raw)) return null;
  return raw.filter((v): v is string => typeof v === "string");
}

// iconFor picks a visual cue based on the event type prefix. Falls back to
// the generic Activity icon for unknown types.
function iconFor(e: AuditEvent) {
  if (e.event_type.startsWith("agent.drift.drifted"))
    return <AlertTriangle className="h-4 w-4 text-amber-600" />;
  if (e.event_type.startsWith("agent.drift.synced"))
    return <CheckCircle2 className="h-4 w-4 text-emerald-600" />;
  if (e.event_type.startsWith("agent."))
    return <Server className="h-4 w-4 text-blue-600" />;
  if (e.event_type.startsWith("config."))
    return <FileText className="h-4 w-4 text-indigo-600" />;
  if (e.event_type === "alert.fired")
    return <AlertCircle className="h-4 w-4 text-red-600" />;
  if (e.event_type === "alert.resolved")
    return <CheckCircle2 className="h-4 w-4 text-emerald-600" />;
  if (e.event_type.startsWith("alert"))
    return <Bell className="h-4 w-4 text-amber-600" />;
  if (e.event_type === "rollout.empty_canary")
    return <AlertTriangle className="h-4 w-4 text-amber-600" />;
  if (e.event_type === "rollout.aborted")
    return <AlertCircle className="h-4 w-4 text-red-600" />;
  if (e.event_type === "rollout.succeeded")
    return <CheckCircle2 className="h-4 w-4 text-emerald-600" />;
  // v0.61 — rollback_completed gets the same checkmark as succeeded
  // but in amber so an operator scanning the timeline can spot the
  // undo terminating cleanly without parsing the label.
  if (e.event_type === "rollout.rollback_completed")
    return <CheckCircle2 className="h-4 w-4 text-amber-600" />;
  if (e.event_type.startsWith("rollout."))
    return <Rocket className="h-4 w-4 text-blue-600" />;
  // v0.76 — multi step plan lifecycle. plan.created lands violet
  // to match the Plan badge color on rollout cards. Terminal
  // outcomes inherit per type colors so an operator scanning
  // the timeline can tell at a glance whether a plan succeeded,
  // cancelled, or rolled back.
  if (e.event_type === "plan.created")
    return <Layers className="h-4 w-4 text-violet-600" />;
  if (e.event_type === "plan.completed")
    return <CheckCircle2 className="h-4 w-4 text-emerald-600" />;
  if (e.event_type === "plan.step_started")
    return <Rocket className="h-4 w-4 text-violet-600" />;
  if (e.event_type === "plan.step_cancelled")
    return <XCircle className="h-4 w-4 text-zinc-500" />;
  if (e.event_type === "plan.cancelled")
    return <XCircle className="h-4 w-4 text-zinc-600" />;
  if (e.event_type === "plan.rejected")
    return <XCircle className="h-4 w-4 text-zinc-700" />;
  if (e.event_type === "plan.rolled_back")
    return <RotateCcw className="h-4 w-4 text-amber-600" />;
  if (e.event_type.startsWith("plan."))
    return <Layers className="h-4 w-4 text-violet-600" />;
  // v0.59 — AI proposer lifecycle. created is a violet sparkle to
  // match the AI explain panel; skipped is a quieter zinc icon so
  // the timeline does not pretend a non-action is an action.
  if (e.event_type === "proposal.created")
    return <Sparkles className="h-4 w-4 text-violet-600" />;
  if (e.event_type === "proposal.declined")
    return <Sparkles className="h-4 w-4 text-amber-600" />;
  if (e.event_type === "proposal.skipped")
    return <SkipForward className="h-4 w-4 text-zinc-500" />;
  return <Activity className="h-4 w-4 text-muted-foreground" />;
}

// describe builds the one-line human-readable summary of an event.
function describe(e: AuditEvent): string {
  switch (e.event_type) {
    case "agent.registered":
      return `Agent registered${e.payload?.name ? ` (${e.payload.name})` : ""}`;
    case "agent.drift.drifted":
      return `Agent drifted${e.payload?.from ? ` from ${e.payload.from}` : ""}`;
    case "agent.drift.synced":
      return `Agent synced${e.payload?.from ? ` from ${e.payload.from}` : ""}`;
    case "config.stored":
      return `Configuration stored${
        e.payload?.version ? ` (v${e.payload.version})` : ""
      }`;
    case "config.applied":
      return "Configuration pushed to agent";
    case "alert.fired":
      return `Alert fired${e.payload?.rule_name ? `: ${e.payload.rule_name}` : ""}`;
    case "alert.resolved":
      return `Alert resolved${e.payload?.rule_name ? `: ${e.payload.rule_name}` : ""}`;
    case "rollout.created": {
      const name =
        typeof e.payload?.name === "string" ? `: ${e.payload.name}` : "";
      // If the audit payload carries the diff fingerprint (post-v0.6
      // rollouts always do), surface it inline so a glance at the
      // timeline tells you how big each rollout's change was.
      const added =
        typeof e.payload?.diff_added_lines === "number"
          ? e.payload.diff_added_lines
          : null;
      const removed =
        typeof e.payload?.diff_removed_lines === "number"
          ? e.payload.diff_removed_lines
          : null;
      const identical = e.payload?.diff_identical === true;
      let suffix = "";
      if (identical) suffix = " (identical to current)";
      else if (added !== null && removed !== null)
        suffix = ` (+${added} / -${removed} lines)`;
      return `Rollout created${name}${suffix}`;
    }
    case "rollout.stage_applied": {
      const stage =
        typeof e.payload?.stage === "number" ? e.payload.stage : null;
      const mode =
        typeof e.payload?.mode === "string" ? e.payload.mode : "percent";
      const size =
        typeof e.payload?.canary_size === "number"
          ? e.payload.canary_size
          : null;
      const where =
        mode === "label"
          ? labelSelectorSummary(e.payload?.label_selector)
          : `${e.payload?.percentage ?? "?"}%`;
      const stageStr = stage !== null ? `Stage ${stage + 1}` : "Stage";
      const sizeStr =
        size !== null ? `, ${size} agent${size === 1 ? "" : "s"}` : "";
      return `${stageStr} applied (${where}${sizeStr})`;
    }
    case "rollout.empty_canary":
      return "Stage resolved to 0 canary agents";
    case "rollout.aborted":
      return `Rollout aborted${e.payload?.reason ? `: ${e.payload.reason}` : ""}`;
    case "rollout.paused":
      return "Rollout paused";
    case "rollout.resumed":
      return "Rollout resumed";
    case "rollout.succeeded":
      return "Rollout succeeded";
    case "rollout.rolled_back":
      return "Rollback applied";
    case "proposal.created":
      return "AI proposed a rollout";
    case "proposal.declined": {
      const reason =
        typeof e.payload?.reason === "string" ? `: ${e.payload.reason}` : "";
      return `AI declined to propose${reason}`;
    }
    case "proposal.skipped": {
      // v0.59 — pre-LLM refusal. Translate the structured reason
      // code into a one-line human summary so the timeline reads
      // naturally without an Explain click.
      const reason = e.payload?.reason;
      if (reason === "group_inference_failed")
        return "AI skipped: could not infer target group";
      if (reason === "missing_current_config")
        return "AI skipped: no current config for group";
      return "AI skipped this spike";
    }
    case "rollout.rollback_requested": {
      // v0.60 — operator clicked Roll back. The payload carries the
      // new rollout's ID so the timeline can hint at the chain
      // without a click.
      const rbID =
        typeof e.payload?.rollback_rollout_id === "string"
          ? e.payload.rollback_rollout_id
          : null;
      return rbID
        ? `Rollback requested · new rollout ${rbID.slice(0, 8)}…`
        : "Rollback requested";
    }
    case "rollout.rollback_completed": {
      // v0.61 — engine emits this when a rollback rollout (the new
      // rollout created by /rollouts/:id/rollback) reaches
      // succeeded. The payload carries the source rollout id so
      // the timeline can show the full arc without a click.
      const srcID =
        typeof e.payload?.rolled_back_from_id === "string"
          ? e.payload.rolled_back_from_id
          : null;
      return srcID
        ? `Rollback completed · undid ${srcID.slice(0, 8)}…`
        : "Rollback completed";
    }
    // v0.76 — multi step plan lifecycle. The payloads carry the
    // plan id so operators can correlate events across the full
    // forward + backward arc without opening each rollout.
    case "plan.created": {
      const count =
        typeof e.payload?.step_count === "number"
          ? e.payload.step_count
          : null;
      const planID = planIDOf(e);
      return planID
        ? `Plan created · ${count ?? "?"} steps · ${planID.slice(0, 8)}…`
        : `Plan created · ${count ?? "?"} steps`;
    }
    case "plan.step_started": {
      const idx = e.payload?.plan_step_index;
      const prev = e.payload?.previous_step;
      return typeof idx === "number"
        ? `Plan step ${idx} started${typeof prev === "number" ? ` (after step ${prev})` : ""}`
        : "Plan step started";
    }
    case "plan.completed": {
      const final = e.payload?.final_step;
      const total = e.payload?.total_steps;
      return typeof total === "number"
        ? `Plan completed · ${total} steps shipped`
        : typeof final === "number"
          ? `Plan completed · final step ${final}`
          : "Plan completed";
    }
    case "plan.step_cancelled": {
      const idx = e.payload?.plan_step_index;
      const reason =
        typeof e.payload?.reason === "string" ? e.payload.reason : null;
      return typeof idx === "number"
        ? `Plan step ${idx} cancelled${reason ? ` · ${reason}` : ""}`
        : "Plan step cancelled";
    }
    case "plan.cancelled": {
      const count =
        typeof e.payload?.cancelled_count === "number"
          ? e.payload.cancelled_count
          : null;
      const failed = e.payload?.failed_step_index;
      return typeof count === "number"
        ? `Plan cancelled · step ${typeof failed === "number" ? failed : "?"} failed, ${count} steps skipped`
        : "Plan cancelled";
    }
    case "plan.rejected": {
      const count =
        typeof e.payload?.cancelled_count === "number"
          ? e.payload.cancelled_count
          : null;
      return typeof count === "number"
        ? `Plan rejected at approval · ${count} steps cancelled`
        : "Plan rejected at approval";
    }
    case "plan.rolled_back": {
      const count =
        typeof e.payload?.rollback_count === "number"
          ? e.payload.rollback_count
          : null;
      const failed = e.payload?.failed_step_index;
      return typeof count === "number"
        ? `Plan rolling back · step ${typeof failed === "number" ? failed : "?"} failed, ${count} predecessors being undone`
        : "Plan rolling back";
    }
  }
  // Generic fallback: humanize the event type.
  return `${e.event_type} ${e.action}`;
}

// planIDOf reaches into the v0.76 plan.* event payload to pull
// out the plan id. Returns null when the payload doesn't carry
// one — defensive because audit payloads are freeform JSON and
// we'd rather render "Plan created" than crash if a future event
// shape changes.
function planIDOf(e: AuditEvent): string | null {
  if (typeof e.payload?.plan_id === "string") {
    return e.payload.plan_id;
  }
  return null;
}

// labelSelectorSummary turns a {k:v} map from an audit payload into a
// compact "k=v, k=v" string for the one-line event summary. Defensive
// against malformed payloads (audit payloads are freeform JSON).
function labelSelectorSummary(raw: unknown): string {
  if (!raw || typeof raw !== "object") return "label";
  const entries = Object.entries(raw as Record<string, unknown>)
    .filter(([k, v]) => typeof k === "string" && typeof v === "string")
    .map(([k, v]) => `${k}=${String(v)}`);
  if (entries.length === 0) return "label";
  // Cap at 3 pairs so a sprawling selector doesn't blow out the row.
  const head = entries.slice(0, 3).join(", ");
  return entries.length > 3 ? `${head}, …` : head;
}

// timeAgo returns "5m ago", "2h ago", etc. Tight format for dense lists.
function timeAgo(d: Date): string {
  const seconds = Math.floor((Date.now() - d.getTime()) / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}
