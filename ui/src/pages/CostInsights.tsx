/**
 * Cost Insights — v0.24 read-only data surface. Answers "where are
 * my telemetry bytes actually going" in five seconds:
 *
 *   - Fleet-wide total + per-signal split (via VolumePanel)
 *   - Top-N outlier agents by byte contribution
 *   - Top-N attribute keys by approximate byte share per signal
 *   - Destination breakdown derived from each agent's configured
 *     exporters (parsed from effective_config). Marked "estimated"
 *     because we don't measure real egress.
 *
 * v0.25 layers cost projection (in $) and recommendations on top
 * of these same endpoints. The response shapes are stable.
 */

import {
  AlertCircleIcon,
  RefreshCwIcon,
  ServerIcon,
} from "lucide-react";
import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import useSWR from "swr";

import { getAgents } from "@/api/agents";
import {
  getTopAgents,
  getTopAttributes,
  type AgentVolume,
  type InsightsSignal,
  type InsightsWindow,
} from "@/api/insights";
import {
  groupFlowsByDestination,
  parseAgentFlows,
} from "@/components/fleet-map/exporter-parser";
import { VolumePanel, formatBytes } from "@/components/insights/VolumePanel";
import { RecommendationsPanel } from "@/components/recommendations/RecommendationsPanel";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

const WINDOW_OPTIONS: { value: InsightsWindow; label: string }[] = [
  { value: "5m", label: "5 min" },
  { value: "1h", label: "1 hour" },
  { value: "24h", label: "24 hours" },
];

const SIGNAL_OPTIONS: { value: InsightsSignal; label: string; color: string }[] = [
  { value: "traces", label: "Traces", color: "var(--chart-1)" },
  { value: "metrics", label: "Metrics", color: "var(--chart-2)" },
  { value: "logs", label: "Logs", color: "var(--chart-3)" },
];

export default function CostInsightsPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const windowParam = (searchParams.get("window") || "1h") as InsightsWindow;
  const window: InsightsWindow = WINDOW_OPTIONS.some(
    (o) => o.value === windowParam,
  )
    ? windowParam
    : "1h";

  const setWindow = (next: InsightsWindow) => {
    const sp = new URLSearchParams(searchParams);
    sp.set("window", next);
    setSearchParams(sp, { replace: true });
  };

  const [signalFor, setSignalFor] = useState<InsightsSignal>("logs");
  const [refreshing, setRefreshing] = useState(false);

  // Top agents — drives the outliers panel.
  const {
    data: topAgentsResp,
    isLoading: topAgentsLoading,
    mutate: mutateTopAgents,
  } = useSWR(
    ["insights/top-agents", window],
    () => getTopAgents({ window, limit: 25 }),
    { refreshInterval: 30000, keepPreviousData: true },
  );

  // Top attributes for the selected signal.
  const {
    data: topAttrsResp,
    isLoading: topAttrsLoading,
    mutate: mutateTopAttrs,
  } = useSWR(
    ["insights/top-attrs", window, signalFor],
    () => getTopAttributes({ window, signal: signalFor, limit: 20 }),
    { refreshInterval: 30000, keepPreviousData: true },
  );

  // Agents (with effective_config) — needed to attribute bytes to
  // destinations. We pull a single 500-page; at fleets >500 this
  // becomes a "top 500 by some criterion" approximation but stays
  // useful for the headline destination split.
  const { data: agentsResp, mutate: mutateAgents } = useSWR(
    "/agents-cost-insights",
    () => getAgents({ limit: 500 }),
    { refreshInterval: 60000 },
  );

  const refreshAll = async () => {
    setRefreshing(true);
    await Promise.all([mutateTopAgents(), mutateTopAttrs(), mutateAgents()]);
    setRefreshing(false);
  };

  // Build a deterministic agentID → agentName map for outliers.
  const agentNameByID = useMemo(() => {
    const m: Record<string, string> = {};
    for (const a of agentsResp?.items ?? []) m[a.id] = a.name;
    return m;
  }, [agentsResp]);

  // Destination attribution — pro-rate each agent's bytes across
  // its configured exporters. Labeled "estimated" in the UI.
  const destinationRollup = useMemo(() => {
    if (!topAgentsResp || !agentsResp) return [];
    // Build agentID → flows from existing exporter parser. Flows
    // is per-signal; we want a unique set of destinations per
    // agent for the byte split (a single agent with traces +
    // metrics going to one OTLP exporter counts as ONE
    // destination, not two).
    const destBytesByKey = new Map<
      string,
      { kind: string; label: string; bytes: number }
    >();
    const agentsByID: Record<string, (typeof agentsResp.items)[number]> = {};
    for (const a of agentsResp.items) agentsByID[a.id] = a;

    for (const ag of topAgentsResp.items) {
      const a = agentsByID[ag.agent_id];
      if (!a) continue;
      const flows = parseAgentFlows(a);
      const grouped = groupFlowsByDestination(flows);
      if (grouped.length === 0) continue;
      const perDest = ag.total_bytes / grouped.length; // even split
      for (const g of grouped) {
        const prev = destBytesByKey.get(g.key);
        if (prev) {
          prev.bytes += perDest;
        } else {
          destBytesByKey.set(g.key, {
            kind: g.kind,
            label: g.label,
            bytes: perDest,
          });
        }
      }
    }
    return Array.from(destBytesByKey.entries())
      .map(([key, v]) => ({ key, ...v }))
      .sort((a, b) => b.bytes - a.bytes);
  }, [topAgentsResp, agentsResp]);

  const topAgents = topAgentsResp?.items ?? [];

  // Outliers cutoff: the top 10% of agents OR the top 5, whichever
  // is more — operators want to see at least a few names even when
  // the fleet is small. Caps at 25 so the panel doesn't sprawl.
  const outlierCount = Math.min(
    25,
    Math.max(5, Math.ceil(topAgents.length * 0.1)),
  );
  const outliers = topAgents.slice(0, outlierCount);

  return (
    <div className="flex flex-col gap-6">
      {/* Page header */}
      <header className="flex items-end justify-between gap-4">
        <div>
          <div className="text-[10px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
            Squadron
          </div>
          <h1 className="text-2xl font-semibold tracking-tight text-foreground">
            Cost Insights
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Where your telemetry bytes are going. Use this to find
            outliers and noisy attributes before they show up on the
            invoice.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Tabs value={window} onValueChange={(v) => setWindow(v as InsightsWindow)}>
            <TabsList>
              {WINDOW_OPTIONS.map((o) => (
                <TabsTrigger key={o.value} value={o.value}>
                  {o.label}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
          <Button
            variant="outline"
            size="sm"
            onClick={refreshAll}
            disabled={refreshing}
          >
            <RefreshCwIcon
              className={`mr-2 h-3.5 w-3.5 ${refreshing ? "animate-spin" : ""}`}
            />
            Refresh
          </Button>
        </div>
      </header>

      {/* Fleet top-line */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <VolumePanel mode="fleet" window={window} />
        <DestinationBreakdown
          destinations={destinationRollup}
          isLoading={!agentsResp || topAgentsLoading}
        />
      </div>

      {/* Recommendations — v0.25 cost-optimization engine. Sits
          between fleet summary and outliers so operators see
          actionable advice before diving into raw rankings. */}
      <RecommendationsPanel mode="fleet" window={window} limit={6} />

      {/* Outliers + attributes */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <OutliersPanel
          outliers={outliers}
          agentNameByID={agentNameByID}
          isLoading={topAgentsLoading}
          window={window}
        />
        <AttributesPanel
          attributes={topAttrsResp?.items ?? []}
          isLoading={topAttrsLoading}
          signal={signalFor}
          onSignalChange={setSignalFor}
        />
      </div>
    </div>
  );
}

// ============================================================
// Outliers panel
// ============================================================

function OutliersPanel({
  outliers,
  agentNameByID,
  isLoading,
  window,
}: {
  outliers: AgentVolume[];
  agentNameByID: Record<string, string>;
  isLoading: boolean;
  window: InsightsWindow;
}) {
  const totalShown = outliers.reduce((sum, a) => sum + a.total_bytes, 0);
  return (
    <Card className="bg-card/60 border-border/70 backdrop-blur">
      <CardContent className="p-5">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold uppercase tracking-wider text-foreground">
              Outlier Agents
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Top byte contributors in the last {window}
            </p>
          </div>
        </div>
        <ul className="mt-4 space-y-2">
          {outliers.length === 0 && !isLoading && (
            <li className="rounded-md border border-dashed border-border/60 px-4 py-8 text-center text-xs text-muted-foreground">
              No agents have sent telemetry in this window yet.
            </li>
          )}
          {outliers.map((a) => {
            const name = agentNameByID[a.agent_id] || a.agent_id.slice(0, 8);
            const pctOfShown = totalShown > 0 ? (a.total_bytes / totalShown) * 100 : 0;
            return (
              <li
                key={a.agent_id}
                className="rounded-md border border-border/40 px-3 py-2"
              >
                <div className="flex items-center justify-between gap-3">
                  <span className="flex min-w-0 items-center gap-2">
                    <ServerIcon className="h-3.5 w-3.5 flex-shrink-0 text-muted-foreground" />
                    <span className="truncate text-sm font-medium text-foreground">
                      {name}
                    </span>
                  </span>
                  <span className="flex-shrink-0 font-tabular text-sm text-foreground">
                    {formatBytes(a.total_bytes)}
                  </span>
                </div>
                {/* Mini per-signal bar */}
                <div className="mt-1.5 flex h-1.5 w-full overflow-hidden rounded-full bg-muted/40">
                  {a.by_signal
                    .filter((s) => s.bytes > 0)
                    .map((s) => {
                      const sigColor =
                        s.signal === "traces"
                          ? "var(--chart-1)"
                          : s.signal === "metrics"
                            ? "var(--chart-2)"
                            : s.signal === "logs"
                              ? "var(--chart-3)"
                              : "var(--muted-foreground)";
                      const pct = (s.bytes / a.total_bytes) * 100;
                      return (
                        <div
                          key={s.signal || "unknown"}
                          style={{ width: `${pct}%`, background: sigColor }}
                          title={`${s.signal}: ${formatBytes(s.bytes)}`}
                        />
                      );
                    })}
                </div>
                {pctOfShown > 0 && (
                  <div className="mt-1 text-[10px] text-muted-foreground">
                    {pctOfShown.toFixed(1)}% of shown outlier total
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      </CardContent>
    </Card>
  );
}

// ============================================================
// Attributes panel
// ============================================================

function AttributesPanel({
  attributes,
  isLoading,
  signal,
  onSignalChange,
}: {
  attributes: { key: string; bytes: number; pct_of_signal: number; estimated: boolean }[];
  isLoading: boolean;
  signal: InsightsSignal;
  onSignalChange: (s: InsightsSignal) => void;
}) {
  return (
    <Card className="bg-card/60 border-border/70 backdrop-blur">
      <CardContent className="p-5">
        <div className="flex items-center justify-between gap-2">
          <div>
            <h3 className="text-sm font-semibold uppercase tracking-wider text-foreground">
              Top Attributes
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Attribute keys by byte share within the signal
            </p>
          </div>
          <Tabs
            value={signal}
            onValueChange={(v) => onSignalChange(v as InsightsSignal)}
          >
            <TabsList>
              {SIGNAL_OPTIONS.map((o) => (
                <TabsTrigger key={o.value} value={o.value} className="text-xs">
                  {o.label}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        </div>

        <ul className="mt-4 space-y-1">
          {attributes.length === 0 && !isLoading && (
            <li className="rounded-md border border-dashed border-border/60 px-4 py-8 text-center text-xs text-muted-foreground">
              No {signal} data captured in this window yet.
            </li>
          )}
          {attributes.map((a) => {
            const pct = Math.min(100, Math.round(a.pct_of_signal * 100));
            return (
              <li
                key={a.key}
                className="grid grid-cols-[1fr_3fr_auto] items-center gap-3 rounded-md px-2 py-1 hover:bg-accent/30"
              >
                <span
                  className="truncate font-mono text-[11px] text-foreground"
                  title={a.key}
                >
                  {a.key}
                </span>
                <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted/40">
                  <div
                    className="h-full"
                    style={{
                      width: `${pct}%`,
                      background: "var(--primary)",
                    }}
                  />
                </div>
                <span className="font-tabular text-[11px] text-foreground tabular-nums">
                  {formatBytes(a.bytes)} · {pct}%
                </span>
              </li>
            );
          })}
        </ul>

        <div className="mt-3 flex items-center gap-1.5 text-[10px] text-muted-foreground">
          <AlertCircleIcon className="h-3 w-3" />
          <span>
            Estimated via sampled aggregation (~2,000 rows per query).
            Use the value to find candidates for a <code className="font-mono text-[10px]">delete</code> processor; treat exact numbers as approximate.
          </span>
        </div>
      </CardContent>
    </Card>
  );
}

// ============================================================
// Destination breakdown
// ============================================================

function DestinationBreakdown({
  destinations,
  isLoading,
}: {
  destinations: { key: string; kind: string; label: string; bytes: number }[];
  isLoading: boolean;
}) {
  const total = destinations.reduce((s, d) => s + d.bytes, 0);
  return (
    <Card className="bg-card/60 border-border/70 backdrop-blur">
      <CardContent className="p-5">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold uppercase tracking-wider text-foreground">
              Destination Spend
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Where the bytes go
            </p>
          </div>
          <Badge
            variant="outline"
            className="text-[10px] uppercase tracking-wider text-muted-foreground"
          >
            Estimated
          </Badge>
        </div>

        <ul className="mt-4 space-y-2">
          {destinations.length === 0 && !isLoading && (
            <li className="rounded-md border border-dashed border-border/60 px-4 py-8 text-center text-xs text-muted-foreground">
              No exporter configurations detected on the contributing agents.
            </li>
          )}
          {destinations.slice(0, 8).map((d) => {
            const pct = total > 0 ? (d.bytes / total) * 100 : 0;
            return (
              <li key={d.key}>
                <div className="flex items-center justify-between gap-2">
                  <span className="truncate text-sm text-foreground">
                    {d.label}
                  </span>
                  <span className="flex-shrink-0 font-tabular text-sm text-foreground">
                    {formatBytes(d.bytes)}
                  </span>
                </div>
                <div className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-muted/40">
                  <div
                    className="h-full"
                    style={{
                      width: `${pct}%`,
                      background:
                        d.kind === "squadron"
                          ? "var(--brand)"
                          : "var(--primary)",
                    }}
                  />
                </div>
              </li>
            );
          })}
        </ul>

        <div className="mt-3 flex items-center gap-1.5 text-[10px] text-muted-foreground">
          <AlertCircleIcon className="h-3 w-3" />
          <span>
            Bytes attributed by pro-rating each agent's volume across
            its configured exporters. True per-destination egress
            measurement is on the v0.26+ roadmap.
          </span>
        </div>
      </CardContent>
    </Card>
  );
}
