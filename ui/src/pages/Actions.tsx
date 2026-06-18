// Squadron Actions page — operational visibility into the action
// runner system (Move 2 of the engineer copilot roadmap, SQ-2.8).
//
// Every signed request Squadron dispatches to a runner shows up here.
// Operators need this page to answer questions like: did the dry run
// for that restart actually finish? Why did this request get denied?
// Which runner picked it up?
//
// The page is intentionally a flat list rather than the inbox / detail
// shape we use for incidents. Action requests are short-lived and
// numerous; an operator usually wants to scan the last hour and
// expand one row at a time. A status filter pins to the top so a
// failure-hunt is one click.

import { useMemo, useState, type ReactNode } from "react";
import useSWR from "swr";

import { listActionRequests, listActionRunners } from "@/api/actions";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type {
  ActionPhase,
  ActionRequest,
  ActionStatus,
  ActionRunner,
} from "@/types/action";

type StatusFilter = "all" | ActionStatus;

const STATUS_OPTIONS: { value: StatusFilter; label: string }[] = [
  { value: "all", label: "All" },
  { value: "pending", label: "Pending" },
  { value: "success", label: "Success" },
  { value: "failure", label: "Failure" },
  { value: "denied", label: "Denied" },
];

const statusToneClass: Record<ActionStatus, string> = {
  pending: "bg-amber-500/10 text-amber-700 border-amber-500/20",
  success: "bg-emerald-500/10 text-emerald-700 border-emerald-500/20",
  failure: "bg-rose-500/10 text-rose-700 border-rose-500/20",
  denied: "bg-zinc-500/10 text-zinc-600 border-zinc-500/20",
};

const phaseToneClass: Record<ActionPhase, string> = {
  dry_run: "bg-sky-500/10 text-sky-700 border-sky-500/20",
  execute: "bg-violet-500/10 text-violet-700 border-violet-500/20",
};

function formatRelative(iso: string): string {
  const then = new Date(iso).getTime();
  const now = Date.now();
  const seconds = Math.round((now - then) / 1000);
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
}

function tryPretty(jsonString: string | undefined): string {
  if (!jsonString) return "";
  try {
    return JSON.stringify(JSON.parse(jsonString), null, 2);
  } catch {
    return jsonString;
  }
}

function StatusBadge({ status }: { status: ActionStatus }) {
  return (
    <Badge variant="outline" className={statusToneClass[status]}>
      {status}
    </Badge>
  );
}

function PhaseBadge({ phase }: { phase: ActionPhase }) {
  return (
    <Badge variant="outline" className={phaseToneClass[phase]}>
      {phase === "dry_run" ? "dry run" : "execute"}
    </Badge>
  );
}

export default function ActionsPage() {
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [expanded, setExpanded] = useState<string | null>(null);

  const { data: requests, isLoading } = useSWR<ActionRequest[]>(
    `/actions?status=${statusFilter}`,
    () =>
      listActionRequests(
        statusFilter === "all" ? {} : { status: statusFilter },
      ),
    { refreshInterval: 15000 },
  );

  // Runner roster used to render hostnames next to runner IDs. SWR
  // dedupes against the Runners page so navigating between them is
  // free.
  const { data: runners } = useSWR<ActionRunner[]>(
    "/runners",
    () => listActionRunners(),
    { refreshInterval: 60000 },
  );

  const runnerHostByID = useMemo(() => {
    const map: Record<string, string> = {};
    (runners ?? []).forEach((r) => {
      map[r.runner_id] = r.hostname;
    });
    return map;
  }, [runners]);

  const visible = requests ?? [];

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Actions</h1>
          <p className="text-sm text-muted-foreground mt-1 max-w-2xl">
            Every signed request Squadron dispatched to a runner. Use the
            filter to hunt down failures or denied requests, or expand a row
            to see the dry run output before the matching execute fires.
          </p>
        </div>
        <div className="w-40 shrink-0">
          <Select
            value={statusFilter}
            onValueChange={(v) => setStatusFilter(v as StatusFilter)}
          >
            <SelectTrigger>
              <SelectValue placeholder="Status" />
            </SelectTrigger>
            <SelectContent>
              {STATUS_OPTIONS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      {isLoading && !requests ? (
        <Card>
          <CardContent className="py-8 text-sm text-muted-foreground">
            Loading action requests...
          </CardContent>
        </Card>
      ) : visible.length === 0 ? (
        <Card>
          <CardContent className="py-8 text-sm text-muted-foreground">
            No action requests match this filter. The action runner system is
            quiet right now, or no requests have been dispatched yet.
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-2">
          {visible.map((req) => {
            const open = expanded === req.id;
            const host = runnerHostByID[req.runner_id] ?? req.runner_id;
            return (
              <Card key={req.id}>
                <CardHeader
                  className="cursor-pointer hover:bg-accent/40 transition-colors"
                  onClick={() => setExpanded(open ? null : req.id)}
                >
                  <div className="flex items-center justify-between gap-3 flex-wrap">
                    <div className="flex items-center gap-2 flex-wrap min-w-0">
                      <CardTitle className="text-sm font-medium truncate">
                        {req.action_type}
                      </CardTitle>
                      <PhaseBadge phase={req.phase} />
                      <StatusBadge status={req.status} />
                      <span className="text-xs text-muted-foreground">
                        on {host}
                      </span>
                    </div>
                    <div className="text-xs text-muted-foreground shrink-0">
                      {formatRelative(req.issued_at)}
                    </div>
                  </div>
                </CardHeader>
                {open ? (
                  <CardContent className="pt-0 space-y-4">
                    <Section label="Request">
                      <KV k="ID" v={req.id} mono />
                      {req.proposal_id ? (
                        <KV k="Proposal" v={req.proposal_id} mono />
                      ) : null}
                      <KV k="Runner" v={`${host} (${req.runner_id})`} mono />
                      <KV k="Issued" v={req.issued_at} />
                      <KV k="Expires" v={req.expires_at} />
                      {req.started_at ? (
                        <KV k="Started" v={req.started_at} />
                      ) : null}
                      {req.completed_at ? (
                        <KV k="Completed" v={req.completed_at} />
                      ) : null}
                      {req.denied_for ? (
                        <KV k="Denied for" v={req.denied_for} />
                      ) : null}
                    </Section>
                    <Section label="Parameters">
                      <pre className="text-xs bg-muted/40 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all">
                        {tryPretty(req.parameters_json) || "(none)"}
                      </pre>
                    </Section>
                    {req.dry_run_output_json ? (
                      <Section label="Dry run output">
                        <pre className="text-xs bg-muted/40 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all">
                          {tryPretty(req.dry_run_output_json)}
                        </pre>
                      </Section>
                    ) : null}
                    {req.execution_output_json ? (
                      <Section label="Execution output">
                        <pre className="text-xs bg-muted/40 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all">
                          {tryPretty(req.execution_output_json)}
                        </pre>
                      </Section>
                    ) : null}
                    <Section label="Signature">
                      <p className="text-xs font-mono text-muted-foreground break-all">
                        {req.signature}
                      </p>
                    </Section>
                  </CardContent>
                ) : null}
              </Card>
            );
          })}
        </div>
      )}
    </div>
  );
}

function Section({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="space-y-2">
      <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      {children}
    </div>
  );
}

function KV({ k, v, mono = false }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="flex items-baseline gap-2 text-xs">
      <span className="text-muted-foreground shrink-0 w-24">{k}</span>
      <span className={mono ? "font-mono break-all" : "break-all"}>{v}</span>
    </div>
  );
}

