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
  Server,
} from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import { listAuditEvents } from "@/api/audit";
import { Badge } from "@/components/ui/badge";
import type { AuditEvent } from "@/types/audit";

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

  const icon = iconFor(event);
  const ts = new Date(event.timestamp);

  return (
    <li>
      <button
        type="button"
        onClick={() => hasPayload && setExpanded((v) => !v)}
        disabled={!hasPayload}
        className="w-full text-left flex items-start gap-2 px-2 py-1.5 rounded hover:bg-muted/40 disabled:cursor-default"
      >
        <span className="shrink-0 mt-0.5">
          {hasPayload ? (
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
            {timeAgo(ts)} · <span className="font-mono">{event.event_type}</span>
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
      {expanded && hasPayload && (
        <pre className="mx-6 mb-2 text-[11px] font-mono whitespace-pre-wrap break-all bg-muted/40 rounded p-2 overflow-auto max-h-48">
          {JSON.stringify(event.payload, null, 2)}
        </pre>
      )}
    </li>
  );
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
  }
  // Generic fallback: humanize the event type.
  return `${e.event_type} ${e.action}`;
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
