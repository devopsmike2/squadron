/**
 * Pipeline Health surfaces — v0.31.
 *
 * Three components, all hitting /api/v1/pipeline-health/*:
 *
 *   - <PipelineHealthBadge agentID/>      one chip per row, color-coded by verdict
 *   - <PipelineHealthAgentPanel agentID/> per-agent panel for the agent drawer
 *   - <FleetHealthSummary/>               fleet-wide stacked bar for the Dashboard
 *
 * The data model mirrors the backend:
 *   verdict ∈ {healthy, degraded, broken, unknown}
 *   signals are the contributing findings, severity-sorted (critical first)
 *   latest is metric_name → list of (label set, value, unit) rows
 *
 * Refresh interval (10s) matches the OTel collector's default
 * self-metric reporting interval. No reason to poll faster than
 * the underlying data changes.
 */

import { AlertCircleIcon, ZapIcon, CheckCircle2Icon } from "lucide-react";
import { useMemo } from "react";
import useSWR from "swr";

import {
  fetchAgentPipelineHealth,
  fetchFleetPipelineHealth,
  verdictColor,
  verdictLabel,
  type PipelineHealthAgentSnapshot,
  type PipelineHealthVerdict,
} from "@/api/pipelinehealth";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { InfoTooltip } from "@/components/ui/info-tooltip";

const REFRESH_MS = 10_000;

// ---------------------------------------------------------------------------
// Badge — one chip per agent row. Suitable for the Agents table, the
// Fleet Map node tooltip, and the agent detail header.
// ---------------------------------------------------------------------------

export function PipelineHealthBadge({ agentID }: { agentID: string }) {
  const { data } = useSWR(
    ["ph-snap", agentID],
    () => fetchAgentPipelineHealth(agentID),
    { refreshInterval: REFRESH_MS, shouldRetryOnError: false },
  );
  const verdict: PipelineHealthVerdict = data?.verdict ?? "unknown";
  return (
    <Badge
      variant="outline"
      title={
        data && data.signals.length
          ? data.signals.map((s) => `${s.severity.toUpperCase()}: ${s.message}`).join("\n")
          : `Pipeline ${verdictLabel(verdict).toLowerCase()}`
      }
      style={{
        borderColor: verdictColor(verdict),
        color: verdictColor(verdict),
        fontVariantNumeric: "tabular-nums",
      }}
      className="text-xs font-medium"
    >
      <span
        className="mr-1.5 inline-block h-1.5 w-1.5 rounded-full"
        style={{ background: verdictColor(verdict) }}
      />
      {verdictLabel(verdict)}
    </Badge>
  );
}

// ---------------------------------------------------------------------------
// Agent panel — the detail surface mounted inside AgentDetailsDrawer.
// Shows the verdict + contributing signals + a tidy table of latest
// values for the captured metrics. No sparklines in v0.31 — we'll
// layer those in v0.31.1 once the timeseries endpoint has been
// exercised in the field.
// ---------------------------------------------------------------------------

export function PipelineHealthAgentPanel({ agentID }: { agentID: string }) {
  const { data, isLoading, error } = useSWR(
    ["ph-snap-panel", agentID],
    () => fetchAgentPipelineHealth(agentID),
    { refreshInterval: REFRESH_MS, shouldRetryOnError: false },
  );

  if (isLoading) {
    return (
      <Card>
        <CardContent className="p-4 text-sm text-muted-foreground">
          Loading pipeline health…
        </CardContent>
      </Card>
    );
  }
  if (error || !data) {
    return (
      <Card>
        <CardContent className="p-4 text-sm text-muted-foreground">
          Pipeline health unavailable. (The collector may not be
          reporting its self-metrics yet — they take one scrape
          interval to land.)
        </CardContent>
      </Card>
    );
  }

  const verdict = data.verdict;
  const Icon =
    verdict === "broken"
      ? AlertCircleIcon
      : verdict === "degraded"
        ? ZapIcon
        : CheckCircle2Icon;

  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <div className="flex items-center gap-2">
          <Icon
            className="h-5 w-5"
            style={{ color: verdictColor(verdict) }}
            aria-hidden
          />
          <h3 className="font-semibold">Pipeline health</h3>
          <Badge
            variant="outline"
            style={{
              borderColor: verdictColor(verdict),
              color: verdictColor(verdict),
            }}
            className="ml-auto"
          >
            {verdictLabel(verdict)}
          </Badge>
        </div>

        {data.signals.length > 0 && (
          <ul className="space-y-1 rounded-md border bg-muted/40 p-2 text-xs">
            {data.signals.map((s, i) => (
              <li key={i} className="flex items-start gap-2">
                <span
                  className="mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full"
                  style={{
                    background:
                      s.severity === "critical"
                        ? "var(--destructive, #ef4444)"
                        : "var(--chart-4, #eab308)",
                  }}
                  aria-hidden
                />
                <span>
                  <span className="font-medium">{s.severity}:</span>{" "}
                  {s.message}
                </span>
              </li>
            ))}
          </ul>
        )}

        <MetricsTable data={data} />

        <div className="text-[10px] text-muted-foreground">
          last sample{" "}
          <span className="font-tabular">
            {data.last_sample
              ? new Date(data.last_sample).toLocaleTimeString()
              : "—"}
          </span>
        </div>
      </CardContent>
    </Card>
  );
}

/**
 * Compact table that groups captured metrics by category. Keeps
 * the agent drawer scannable — operators rarely care about every
 * exporter, they care about "is anything failing".
 */
function MetricsTable({ data }: { data: PipelineHealthAgentSnapshot }) {
  const groups = useMemo(() => groupMetrics(data.latest), [data.latest]);
  if (!groups.length) {
    return (
      <div className="text-xs text-muted-foreground">
        No self-metrics captured yet.
      </div>
    );
  }
  return (
    <div className="space-y-2">
      {groups.map((g) => (
        <div key={g.title}>
          <div className="mb-1 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            {g.title}
          </div>
          <div className="grid grid-cols-2 gap-1 text-xs">
            {g.rows.map((r, i) => (
              <div
                key={i}
                className="flex items-center justify-between rounded border bg-background px-2 py-1 font-tabular"
              >
                <span className="truncate text-muted-foreground">{r.label}</span>
                <span>{r.formatted}</span>
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

interface MetricGroup {
  title: string;
  rows: { label: string; formatted: string }[];
}

function groupMetrics(
  latest: Record<string, PipelineHealthAgentSnapshot["latest"][string]>,
): MetricGroup[] {
  const buckets: { title: string; prefixes: string[] }[] = [
    { title: "Exporters", prefixes: ["otelcol_exporter_"] },
    { title: "Receivers", prefixes: ["otelcol_receiver_"] },
    { title: "Processors", prefixes: ["otelcol_processor_"] },
    { title: "Process", prefixes: ["otelcol_process_"] },
  ];

  const groups: MetricGroup[] = [];
  for (const b of buckets) {
    const rows: MetricGroup["rows"] = [];
    for (const [name, items] of Object.entries(latest)) {
      if (!b.prefixes.some((p) => name.startsWith(p))) continue;
      const short = name.replace("otelcol_", "");
      for (const row of items) {
        const labelTag = row.labels
          .filter((l) => ["exporter", "receiver", "processor"].includes(l.key))
          .map((l) => l.value)
          .join("/");
        const fullLabel = labelTag ? `${short} (${labelTag})` : short;
        rows.push({ label: fullLabel, formatted: formatValue(row.value) });
      }
    }
    if (rows.length) groups.push({ title: b.title, rows });
  }
  return groups;
}

function formatValue(v: number): string {
  if (!isFinite(v)) return "—";
  if (Math.abs(v) >= 1_000_000) return (v / 1_000_000).toFixed(2) + "M";
  if (Math.abs(v) >= 1_000) return (v / 1_000).toFixed(2) + "k";
  if (Number.isInteger(v)) return v.toString();
  return v.toFixed(2);
}

// ---------------------------------------------------------------------------
// Fleet summary — stacked bar of how many agents fall into each
// bucket. Lives on the Dashboard.
// ---------------------------------------------------------------------------

export function FleetHealthSummary() {
  const { data } = useSWR("ph-fleet", fetchFleetPipelineHealth, {
    refreshInterval: REFRESH_MS,
    shouldRetryOnError: false,
  });
  if (!data || data.total === 0) {
    return null;
  }
  const segments: { verdict: PipelineHealthVerdict; count: number }[] = [
    { verdict: "broken", count: data.broken },
    { verdict: "degraded", count: data.degraded },
    { verdict: "healthy", count: data.healthy },
    { verdict: "unknown", count: data.unknown },
  ];

  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-1.5">
            <h3 className="text-sm font-semibold">Pipeline health</h3>
            <InfoTooltip label="About pipeline health" maxWidth={320}>
              Each agent's verdict comes from its OTel collector
              self-metrics (queue depth, send failures, processor
              drops). <b>Unknown</b> just means we haven't received
              self-metrics from that agent yet — it's normal on fresh
              installs and not an alarm. <b>Degraded</b> and{" "}
              <b>Broken</b> mean we observed a real threshold breach
              and the agent should be investigated.
            </InfoTooltip>
          </div>
          <span className="text-xs text-muted-foreground">
            {data.total} agent{data.total === 1 ? "" : "s"}
          </span>
        </div>
        <div className="flex h-3 w-full overflow-hidden rounded">
          {segments.map((s) =>
            s.count === 0 ? null : (
              <div
                key={s.verdict}
                title={`${s.count} ${s.verdict}`}
                style={{
                  background: verdictColor(s.verdict),
                  width: `${(s.count / data.total) * 100}%`,
                }}
              />
            ),
          )}
        </div>
        <div className="grid grid-cols-4 gap-2 text-[11px]">
          {segments.map((s) => (
            <div key={s.verdict} className="flex items-center gap-1.5">
              <span
                className="inline-block h-2 w-2 rounded-full"
                style={{ background: verdictColor(s.verdict) }}
              />
              <span className="font-tabular">{s.count}</span>
              <span className="text-muted-foreground">{verdictLabel(s.verdict)}</span>
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}
