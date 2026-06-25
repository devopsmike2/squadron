// Plan.tsx — v0.75. Renders the plan envelope returned by
// GET /api/v1/rollouts/plans/:id: shared metadata header, ordered
// forward steps with state badges, optional rollback steps when
// the v0.72 backwards walk fired.
//
// Discoverability: reached via the "Plan" badge on any rollout
// card that has plan_id set. A dedicated /plans listing UI can
// land later; the per plan detail page is the v0.75 MVP because
// it's the surface operators need to inspect a multi step fix
// proposed by the AI or a CI script.

import {
  AlertCircle,
  ArrowLeft,
  CheckCircle2,
  Clock,
  Layers,
  Loader2,
  PauseCircle,
  RotateCcw,
  XCircle,
} from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import useSWR from "swr";

import { getPlan } from "@/api/rollouts";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { Plan, Rollout } from "@/types/rollout";

export default function PlanPage() {
  const { id } = useParams<{ id: string }>();
  const planID = id ?? "";

  const { data, error, isLoading } = useSWR<Plan | null>(
    planID ? `plan/${planID}` : null,
    () => getPlan(planID),
    { refreshInterval: 5_000 },
  );

  if (!planID) {
    return <ErrorState message="No plan id in the URL." />;
  }
  if (isLoading) {
    return (
      <div className="flex items-center justify-center p-12 text-muted-foreground">
        <Loader2 className="mr-2 h-4 w-4 animate-spin" />
        Loading plan…
      </div>
    );
  }
  if (error) {
    return (
      <ErrorState
        message={`Failed to load plan: ${error instanceof Error ? error.message : String(error)}`}
      />
    );
  }
  if (!data) {
    return <ErrorState message="Plan not found." />;
  }

  return (
    <div className="container mx-auto p-6 space-y-6 max-w-4xl">
      <Link
        to="/rollouts"
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="h-3.5 w-3.5" />
        Back to Rollouts
      </Link>

      <PlanHeader plan={data} />
      <ForwardSteps plan={data} />
      {data.rollback_steps && data.rollback_steps.length > 0 && (
        <RollbackSteps plan={data} />
      )}
    </div>
  );
}

function PlanHeader({ plan }: { plan: Plan }) {
  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div className="space-y-1">
            <CardTitle className="flex items-center gap-2">
              <Layers className="h-5 w-5 text-violet-500" />
              Plan {plan.plan_id.slice(0, 8)}…
            </CardTitle>
            <div className="text-xs text-muted-foreground">
              Group {plan.group_id} · {plan.step_count} steps · created{" "}
              {formatTime(plan.created_at)}
            </div>
          </div>
          <PlanStateBadge state={plan.state} />
        </div>
      </CardHeader>
    </Card>
  );
}

function ForwardSteps({ plan }: { plan: Plan }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Steps</CardTitle>
        <div className="text-xs text-muted-foreground">
          The plan approves as a unit at step 0. Each subsequent step waits for
          the previous one to succeed.
        </div>
      </CardHeader>
      <CardContent className="space-y-2">
        {plan.steps.map((step) => (
          <StepRow key={step.id} step={step} />
        ))}
      </CardContent>
    </Card>
  );
}

function RollbackSteps({ plan }: { plan: Plan }) {
  const rollbacks = plan.rollback_steps ?? [];
  const [open, setOpen] = useState(true);
  return (
    <Card className="border-amber-500/30">
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen(!open)}
          className="text-left flex items-center justify-between w-full"
        >
          <CardTitle className="flex items-center gap-2 text-base">
            <RotateCcw className="h-4 w-4 text-amber-500" />
            Rollback steps ({rollbacks.length})
          </CardTitle>
          <span className="text-xs text-muted-foreground">
            {open ? "collapse" : "expand"}
          </span>
        </button>
        <div className="text-xs text-muted-foreground">
          The engine created these when a forward step failed. They undo the
          succeeded predecessors in reverse order.
        </div>
      </CardHeader>
      {open && (
        <CardContent className="space-y-2">
          {rollbacks.map((step) => (
            <StepRow key={step.id} step={step} rollback />
          ))}
        </CardContent>
      )}
    </Card>
  );
}

function StepRow({ step, rollback }: { step: Rollout; rollback?: boolean }) {
  const idx = step.plan_step_index ?? 0;
  // v0.89.14 (#630) — action runner steps in plans, slice 1.
  // Render a distinct badge for kind=action steps so the
  // operator can scan a mixed plan and spot the runner verbs at
  // a glance. The detail view is intentionally minimal in slice
  // 1; clicking the row still routes to the existing rollout
  // detail panel which surfaces the action_request_id for any
  // deeper investigation.
  const isAction = step.step_kind === "action";
  const targetLabel = isAction
    ? step.action_request_id
      ? `request ${step.action_request_id.slice(0, 8)}…`
      : "no request yet"
    : step.target_config_id
      ? `target ${step.target_config_id.slice(0, 8)}…`
      : "no target";
  return (
    <Link
      to={`/rollouts?rollout=${encodeURIComponent(step.id)}`}
      className="block rounded border p-3 hover:bg-muted/40 transition-colors"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="space-y-1 min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span
              className={`inline-flex items-center justify-center rounded px-1.5 py-0.5 text-[10px] font-mono ${
                rollback
                  ? "bg-amber-500/10 text-amber-700"
                  : "bg-muted text-muted-foreground"
              }`}
            >
              {rollback ? "rb" : "step"} {idx}
            </span>
            {isAction && (
              <span
                className="inline-flex items-center justify-center rounded px-1.5 py-0.5 text-[10px] font-mono bg-sky-500/10 text-sky-700"
                title="Action runner step (#630)"
              >
                action
              </span>
            )}
            <div className="text-sm font-medium truncate">{step.name}</div>
          </div>
          <div className="text-[11px] text-muted-foreground">
            id {step.id.slice(0, 8)}… · {targetLabel}
          </div>
        </div>
        <StepStateBadge state={step.state} />
      </div>
    </Link>
  );
}

function PlanStateBadge({ state }: { state: string }) {
  const { className, icon: Icon, label } = planStateStyle(state);
  return (
    <Badge variant="outline" className={className}>
      <Icon className="h-3 w-3 mr-1" />
      {label}
    </Badge>
  );
}

function StepStateBadge({ state }: { state: string }) {
  const { className, icon: Icon, label } = stepStateStyle(state);
  return (
    <Badge variant="outline" className={`${className} shrink-0`}>
      <Icon className="h-3 w-3 mr-1" />
      {label}
    </Badge>
  );
}

// Plan-level badge palette. Maps the derived state words from the
// service to colors + icons. Best effort — the per step badges
// carry the canonical signal.
function planStateStyle(state: string) {
  switch (state) {
    case "succeeded":
      return {
        className: "bg-emerald-500/10 text-emerald-700 border-emerald-500/30",
        icon: CheckCircle2,
        label: "Succeeded",
      };
    case "pending_approval":
      return {
        className: "bg-orange-500/10 text-orange-700 border-orange-500/30",
        icon: Clock,
        label: "Pending approval",
      };
    case "in_progress":
      return {
        className: "bg-blue-500/10 text-blue-700 border-blue-500/30",
        icon: Loader2,
        label: "In progress",
      };
    case "rejected":
      return {
        className: "bg-zinc-500/10 text-zinc-700 border-zinc-500/30",
        icon: XCircle,
        label: "Rejected",
      };
    case "cancelled":
      return {
        className: "bg-zinc-500/10 text-zinc-700 border-zinc-500/30",
        icon: XCircle,
        label: "Cancelled",
      };
    case "aborted":
      return {
        className: "bg-red-500/10 text-red-700 border-red-500/30",
        icon: AlertCircle,
        label: "Aborted",
      };
    case "rolled_back":
      return {
        className: "bg-amber-500/10 text-amber-700 border-amber-500/30",
        icon: RotateCcw,
        label: "Rolled back",
      };
    default:
      return {
        className: "bg-muted text-muted-foreground",
        icon: Layers,
        label: state || "unknown",
      };
  }
}

function stepStateStyle(state: string) {
  switch (state) {
    case "succeeded":
      return {
        className: "bg-emerald-500/10 text-emerald-700 border-emerald-500/30",
        icon: CheckCircle2,
        label: "succeeded",
      };
    case "in_progress":
      return {
        className: "bg-blue-500/10 text-blue-700 border-blue-500/30",
        icon: Loader2,
        label: "in progress",
      };
    case "pending":
      return {
        className: "bg-muted text-muted-foreground",
        icon: Clock,
        label: "pending",
      };
    case "queued":
      return {
        className: "bg-muted text-muted-foreground",
        icon: Clock,
        label: "queued",
      };
    case "paused":
      return {
        className: "bg-zinc-500/10 text-zinc-700 border-zinc-500/30",
        icon: PauseCircle,
        label: "paused",
      };
    case "pending_approval":
      return {
        className: "bg-orange-500/10 text-orange-700 border-orange-500/30",
        icon: Clock,
        label: "pending approval",
      };
    case "rejected":
      return {
        className: "bg-zinc-500/10 text-zinc-700 border-zinc-500/30",
        icon: XCircle,
        label: "rejected",
      };
    case "cancelled":
      return {
        className: "bg-zinc-500/10 text-zinc-700 border-zinc-500/30",
        icon: XCircle,
        label: "cancelled",
      };
    case "aborted":
      return {
        className: "bg-red-500/10 text-red-700 border-red-500/30",
        icon: AlertCircle,
        label: "aborted",
      };
    case "rolled_back":
      return {
        className: "bg-amber-500/10 text-amber-700 border-amber-500/30",
        icon: RotateCcw,
        label: "rolled back",
      };
    default:
      return {
        className: "bg-muted text-muted-foreground",
        icon: Layers,
        label: state,
      };
  }
}

function ErrorState({ message }: { message: string }) {
  return (
    <div className="container mx-auto p-6 max-w-4xl">
      <Link
        to="/rollouts"
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-4"
      >
        <ArrowLeft className="h-3.5 w-3.5" />
        Back to Rollouts
      </Link>
      <Card>
        <CardContent className="p-6 text-sm text-muted-foreground">
          {message}
        </CardContent>
      </Card>
    </div>
  );
}

// formatTime renders an ISO timestamp as a relative phrase when
// recent ("3m ago") and falls back to an absolute date for older
// timestamps. Avoids a date-fns dependency on this page.
function formatTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const secs = Math.floor((Date.now() - t) / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return new Date(iso).toLocaleDateString();
}

// Silence the unused-import warning when React's effect hook
// isn't used (it lives here for future polling tweaks).
const _useEffect = useEffect;
void _useEffect;
