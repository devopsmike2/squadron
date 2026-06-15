/**
 * Single agent tile in the Squadron fleet grid.
 *
 * Three rows of information designed for a 1-2 second glance:
 *   1. Status + name + version
 *   2. Drift state (pill) + group chip
 *   3. Last-seen relative time + label sprinkle
 *
 * The card's left edge picks up a 2px accent strip tinted by the
 * agent's drift status — that's the visual cue an operator catches
 * when scanning a wall of cards looking for "what's wrong".
 *
 * Clicking the card opens the existing AgentDetailsDrawer; clicking
 * the group chip stops propagation and opens the group drawer
 * instead. Both routes are unchanged from the legacy table view.
 */

import { Badge } from "@/components/ui/badge";
import type { Agent, ConfigDriftStatus } from "@/types/agent";

const STATUS_TONE: Record<
  Agent["status"],
  { dot: string; label: string }
> = {
  online: { dot: "var(--success)", label: "Online" },
  offline: { dot: "var(--muted-foreground)", label: "Offline" },
  error: { dot: "var(--destructive)", label: "Error" },
};

const DRIFT_TONE: Record<
  ConfigDriftStatus | "default",
  { label: string; color: string }
> = {
  synced: { label: "Synced", color: "var(--success)" },
  drifted: { label: "Drifted", color: "var(--destructive)" },
  no_intent: { label: "No intent", color: "var(--muted-foreground)" },
  no_effective: { label: "Awaiting telemetry", color: "var(--warning)" },
  unknown: { label: "Unknown", color: "var(--muted-foreground)" },
  default: { label: "Unknown", color: "var(--muted-foreground)" },
};

// Environment color stripe across the top of the card. The mapping
// covers the standard deployment.environment values we see in the
// wild; everything else falls back to a neutral muted stripe so the
// strip is always present but never alarming.
//
// Added in v0.38 — operators with 100+ identical-looking
// synthetic-collector cards in a grid couldn't tell at a glance
// which environment they were scanning.
function envColor(env: string | undefined): string {
  switch ((env ?? "").toLowerCase()) {
    case "prod":
    case "production":
      return "var(--chart-1, #06b6d4)"; // cyan
    case "staging":
    case "stage":
      return "var(--chart-3, #eab308)"; // amber
    case "dev":
    case "development":
      return "var(--chart-2, #a855f7)"; // purple
    case "test":
    case "qa":
      return "var(--chart-4, #f97316)"; // orange
    default:
      return "var(--border)";
  }
}

// Recency threshold for the staleness fade. The collector OpAMP
// heartbeat is typically 30s; we give it 10x that before considering
// the card stale enough to dim. Tight enough to flag silent agents,
// loose enough to ignore network blips.
const STALE_MS = 5 * 60 * 1000;

interface AgentCardProps {
  agent: Agent;
  groupName?: string;
  onClick: () => void;
  onGroupClick?: (groupId: string) => void;
}

export function AgentCard({
  agent,
  groupName,
  onClick,
  onGroupClick,
}: AgentCardProps) {
  const status = STATUS_TONE[agent.status] ?? STATUS_TONE.offline;
  const drift = DRIFT_TONE[agent.drift_status ?? "default"];
  const labels = Object.entries(agent.labels ?? {});
  const visibleLabels = labels.slice(0, 3);
  const overflow = labels.length - visibleLabels.length;

  // Environment from labels — the OTel convention is
  // deployment.environment (e.g. "prod", "staging"). Some collectors
  // use a flat "env" alias. Take whichever shows up first.
  const env =
    agent.labels?.["deployment.environment"] ??
    agent.labels?.["env"] ??
    undefined;
  const envStripe = envColor(env);

  // Staleness fade: dim the entire card when the agent hasn't been
  // heard from in STALE_MS. Compute once at render — the live clock
  // doesn't need to tick second-by-second; a parent SWR refresh
  // re-renders us anyway.
  const lastSeenMs = new Date(agent.last_seen).getTime();
  const isStale =
    !Number.isNaN(lastSeenMs) && Date.now() - lastSeenMs > STALE_MS;

  return (
    <button
      type="button"
      onClick={onClick}
      className={`group relative flex flex-col gap-2 rounded-lg border border-border bg-card p-4 text-left transition-all hover:border-primary/50 hover:bg-card/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring overflow-hidden ${
        isStale ? "opacity-60 hover:opacity-100" : ""
      }`}
    >
      {/* Environment stripe along the top edge. v0.38 visual
          differentiation — prod/staging/dev call themselves out
          before the operator reads any text. */}
      <span
        aria-hidden
        className="absolute left-0 right-0 top-0 h-[3px]"
        style={{ background: envStripe }}
      />
      {/* Drift accent strip on the left edge. Subtle but the
          fastest visual cue when scanning a wall of cards. */}
      <span
        aria-hidden
        className="absolute left-0 top-0 h-full w-[3px] opacity-80"
        style={{ background: drift.color }}
      />

      {/* Row 1: Status + name + version */}
      <div className="flex items-start gap-2.5">
        <span
          className="status-dot mt-1.5 flex-shrink-0"
          style={{ ["--dot" as string]: status.dot }}
          title={status.label}
        />
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold text-foreground">
            {agent.name}
          </div>
          <div className="mt-0.5 flex items-center gap-1.5 text-[11px] text-muted-foreground">
            <span className="font-tabular">{agent.version || "—"}</span>
            <span className="opacity-40">·</span>
            <span>{status.label}</span>
          </div>
        </div>
      </div>

      {/* Row 2: drift pill + group chip */}
      <div className="flex items-center gap-2">
        <Badge
          variant="outline"
          className="text-[10px] font-medium uppercase tracking-wider"
          style={{
            color: drift.color,
            borderColor: `color-mix(in oklch, ${drift.color} 40%, transparent)`,
            background: `color-mix(in oklch, ${drift.color} 10%, transparent)`,
          }}
        >
          {drift.label}
        </Badge>
        {agent.group_id ? (
          <span
            onClick={(e) => {
              e.stopPropagation();
              onGroupClick?.(agent.group_id!);
            }}
            className="inline-flex items-center rounded-md border border-border/60 bg-background/40 px-1.5 py-0.5 text-[10px] text-muted-foreground transition-colors hover:border-primary/60 hover:text-foreground"
          >
            {groupName ?? agent.group_id.slice(0, 8)}
          </span>
        ) : null}
        {agent.discovery_source === "otlp" && (
          <Badge
            variant="outline"
            className="text-[10px] font-medium"
            style={{
              color: "var(--status-warn, #eab308)",
              borderColor: "color-mix(in oklch, #eab308 40%, transparent)",
            }}
            title="Telemetry-only: Squadron sees this agent's data on the OTLP receiver but never opened an OpAMP control connection. Configs can't be pushed to it until it's brought under management."
          >
            Telemetry-only
          </Badge>
        )}
      </div>

      {/* Row 3: labels + relative last-seen */}
      <div className="mt-auto flex items-end justify-between gap-2">
        <div className="flex flex-wrap items-center gap-1">
          {visibleLabels.map(([k, v]) => (
            <span
              key={k}
              className="inline-flex items-center rounded-sm border border-border/40 bg-background/30 px-1 py-0.5 font-mono text-[10px] text-muted-foreground"
              title={`${k}=${v}`}
            >
              <span className="opacity-60">{k}</span>
              <span className="opacity-40">=</span>
              <span className="text-foreground/80">{truncate(v, 12)}</span>
            </span>
          ))}
          {overflow > 0 && (
            <span className="text-[10px] text-muted-foreground/70">
              +{overflow}
            </span>
          )}
        </div>
        <span
          className="font-tabular text-[10px] text-muted-foreground"
          title={new Date(agent.last_seen).toLocaleString()}
        >
          {relTime(agent.last_seen)}
        </span>
      </div>
    </button>
  );
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

function relTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "—";
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.round(h / 24);
  return `${d}d ago`;
}
