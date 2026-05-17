/**
 * Compact "Volume (last 24h)" panel.
 *
 * Used in two places: the AgentDetailsDrawer (single agent) and as
 * a building block on the Cost Insights page (fleet summary).
 *
 * Rendering modes:
 *   - mode="agent" with agentId → fetches /insights/volume/agents/:id
 *   - mode="fleet" → fetches /insights/volume
 *
 * Either mode shows: total bytes header, per-signal stacked bar,
 * per-signal breakdown rows. The agent variant adds a "View on
 * Cost Insights" link deeplink.
 */

import { ArrowRightIcon } from "lucide-react";
import { useMemo } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import {
  getAgentVolume,
  getFleetVolume,
  type AgentVolume,
  type FleetSummary,
  type InsightsSignal,
  type InsightsWindow,
  type SignalVolume,
} from "@/api/insights";
import { Card, CardContent } from "@/components/ui/card";

const SIGNAL_COLOR: Record<InsightsSignal, string> = {
  traces: "var(--chart-1)",
  metrics: "var(--chart-2)",
  logs: "var(--chart-3)",
};
const SIGNAL_LABEL: Record<InsightsSignal, string> = {
  traces: "Traces",
  metrics: "Metrics",
  logs: "Logs",
};

/**
 * formatBytes is a quick humanizer. We don't pull a dependency for
 * this — operators read these numbers as "ballpark", not precise
 * accounting, and the implementation fits in 10 lines.
 */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

interface VolumePanelProps {
  mode: "agent" | "fleet";
  agentId?: string;
  window?: InsightsWindow; // default "24h"
  compact?: boolean; // drawer variant collapses padding + hides agent count
}

export function VolumePanel({
  mode,
  agentId,
  window = "24h",
  compact = false,
}: VolumePanelProps) {
  // SWR key includes the mode + ID + window so switching between
  // drawer agents doesn't reuse a stale cache.
  const swrKey =
    mode === "agent"
      ? agentId
        ? ["volume/agent", agentId, window]
        : null
      : ["volume/fleet", window];

  const { data, error, isLoading } = useSWR<AgentVolume | FleetSummary>(
    swrKey,
    () =>
      mode === "agent"
        ? getAgentVolume(agentId!, { window })
        : getFleetVolume({ window }),
    {
      refreshInterval: 30000,
      // Keep the previous panel visible during refetches so window
      // toggles don't flicker.
      keepPreviousData: true,
    },
  );

  const { totalBytes, bySignal } = useMemo(() => {
    if (!data) return { totalBytes: 0, bySignal: [] as SignalVolume[] };
    if (mode === "agent") {
      const a = data as AgentVolume;
      return { totalBytes: a.total_bytes, bySignal: a.by_signal };
    }
    const f = data as FleetSummary;
    return { totalBytes: f.totals.bytes, bySignal: f.by_signal };
  }, [data, mode]);

  if (error) {
    return (
      <Card className="bg-card/60 border-border/70">
        <CardContent className={compact ? "p-3" : "p-4"}>
          <p className="text-xs text-destructive">
            Couldn't load volume data: {(error as Error).message}
          </p>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card className="bg-card/60 border-border/70 backdrop-blur">
      <CardContent className={compact ? "p-3" : "p-5"}>
        <div className="flex items-baseline justify-between">
          <div>
            <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-muted-foreground">
              Volume · last {window}
            </div>
            <div className="mt-1 font-tabular text-2xl font-semibold leading-none tracking-tight text-foreground">
              {isLoading && !data ? "—" : formatBytes(totalBytes)}
            </div>
          </div>
          {mode === "agent" && agentId && (
            <Link
              to={`/cost-insights?agent_id=${agentId}`}
              className="inline-flex items-center gap-1 text-[11px] text-muted-foreground hover:text-foreground"
            >
              Cost Insights <ArrowRightIcon className="h-3 w-3" />
            </Link>
          )}
        </div>

        {/* Stacked bar */}
        {totalBytes > 0 ? (
          <div className="mt-3 flex h-2 w-full overflow-hidden rounded-full bg-muted/40">
            {bySignal
              .filter((s) => s.bytes > 0)
              .map((s) => {
                const pct = (s.bytes / totalBytes) * 100;
                const color =
                  s.signal && SIGNAL_COLOR[s.signal as InsightsSignal]
                    ? SIGNAL_COLOR[s.signal as InsightsSignal]
                    : "var(--muted-foreground)";
                return (
                  <div
                    key={s.signal || "unknown"}
                    style={{ width: `${pct}%`, background: color }}
                    title={`${SIGNAL_LABEL[s.signal as InsightsSignal] ?? s.signal} · ${formatBytes(s.bytes)}`}
                  />
                );
              })}
          </div>
        ) : (
          <div className="mt-3 h-2 w-full rounded-full bg-muted/30" />
        )}

        {/* Per-signal rows */}
        <ul className="mt-3 space-y-1.5">
          {bySignal.length === 0 && (
            <li className="text-[11px] text-muted-foreground italic">
              No telemetry recorded in this window.
            </li>
          )}
          {bySignal.map((s) => {
            const color =
              s.signal && SIGNAL_COLOR[s.signal as InsightsSignal]
                ? SIGNAL_COLOR[s.signal as InsightsSignal]
                : "var(--muted-foreground)";
            const label = SIGNAL_LABEL[s.signal as InsightsSignal] ?? s.signal;
            const pct =
              totalBytes > 0 ? Math.round((s.bytes / totalBytes) * 100) : 0;
            return (
              <li
                key={s.signal || "unknown"}
                className="flex items-center justify-between text-xs"
              >
                <span className="flex items-center gap-2">
                  <span
                    className="status-dot"
                    style={{ ["--dot" as string]: color }}
                  />
                  <span className="text-foreground">{label}</span>
                </span>
                <span className="flex items-center gap-2">
                  <span className="font-tabular text-foreground">
                    {formatBytes(s.bytes)}
                  </span>
                  <span className="font-tabular text-[10px] text-muted-foreground">
                    {pct}%
                  </span>
                  {s.dropped_count > 0 && (
                    <span
                      className="font-tabular text-[10px]"
                      style={{ color: "var(--destructive)" }}
                      title="Items dropped (worker queue rejection or write failure)"
                    >
                      −{s.dropped_count}
                    </span>
                  )}
                </span>
              </li>
            );
          })}
        </ul>

        {mode === "fleet" && (data as FleetSummary | undefined)?.agent_count !== undefined && (
          <div className="mt-3 border-t border-border/40 pt-2 text-[10px] uppercase tracking-wider text-muted-foreground">
            {(data as FleetSummary).agent_count} agents contributed
          </div>
        )}
      </CardContent>
    </Card>
  );
}
