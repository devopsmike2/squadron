/**
 * DeployMetricsPanel — DORA-style KPI strip for the Deploy page.
 *
 * Four tiles: deploy frequency, change failure rate, MTTR, lead
 * time. Each tile shows the number prominently with the contributing
 * counts beneath. A window selector (7d / 30d / 90d) above the strip
 * lets the operator scope the analysis.
 *
 * The numbers come from /api/v1/deploy/metrics — pure read, no
 * mutation. SWR keeps the panel live without driving the backend
 * into the ground; the underlying query reduces a bounded slice of
 * deploy_runs in-memory, so polling every 60s is cheap.
 *
 * Added in v0.39.0 (insights expansion).
 */

import {
  GaugeIcon,
  RocketIcon,
  TimerIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { useState } from "react";
import useSWR from "swr";

import {
  fetchDeployMetrics,
  formatMinutes,
  type DORAWindow,
} from "@/api/deploy";
import { Card, CardContent } from "@/components/ui/card";
import { InfoTooltip } from "@/components/ui/info-tooltip";

const WINDOW_OPTIONS: { value: DORAWindow; label: string }[] = [
  { value: "7d", label: "7d" },
  { value: "30d", label: "30d" },
  { value: "90d", label: "90d" },
];

// Refresh interval. DORA numbers don't move minute-to-minute; even
// a director glancing at this once a day would tolerate a 60s lag.
// Tighter polling just adds backend work for no perceived benefit.
const REFRESH_MS = 60_000;

export function DeployMetricsPanel() {
  const [window, setWindow] = useState<DORAWindow>("30d");
  const { data, isLoading } = useSWR(
    ["deploy-metrics", window],
    () => fetchDeployMetrics(window),
    { refreshInterval: REFRESH_MS },
  );

  // The metric panel is informational — a missing data response or
  // an empty ledger renders the tiles with "—" rather than hiding
  // the panel. That way operators always know it's there once they
  // start using deploys.
  const metrics = data;
  const failPct = metrics
    ? (metrics.change_failure_rate * 100).toFixed(0) + "%"
    : "—";
  const freq = metrics
    ? metrics.deploys_per_day < 1
      ? `${(metrics.deploys_per_day * 7).toFixed(1)}/wk`
      : `${metrics.deploys_per_day.toFixed(1)}/day`
    : "—";

  return (
    <section className="space-y-2">
      <div className="flex items-end justify-between">
        <div>
          <div className="flex items-center gap-1.5">
            <h2 className="text-sm font-semibold uppercase tracking-wider text-muted-foreground">
              Deploy performance
            </h2>
            <InfoTooltip label="About DORA metrics" maxWidth={340}>
              The four DORA metrics are the industry-standard yardstick for
              software delivery performance. Computed from the deploy_runs
              ledger over the selected window. <b>Failed</b> counts failure,
              cancelled, and timed_out terminal states; skipped is excluded as a
              no-op.
            </InfoTooltip>
          </div>
          <p className="text-xs text-muted-foreground">
            {metrics
              ? `${metrics.completed_runs} of ${metrics.total_runs} runs completed in window`
              : "No deploy data yet"}
          </p>
        </div>
        <div className="flex gap-1 rounded-md border border-border bg-card/40 p-0.5">
          {WINDOW_OPTIONS.map((o) => (
            <button
              key={o.value}
              type="button"
              onClick={() => setWindow(o.value)}
              className={`rounded px-2.5 py-1 text-xs transition-colors ${
                window === o.value
                  ? "bg-primary/20 text-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {o.label}
            </button>
          ))}
        </div>
      </div>

      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-4">
        <KpiTile
          icon={<RocketIcon className="h-3.5 w-3.5" />}
          label="Deploy frequency"
          value={freq}
          sub={
            metrics && metrics.completed_runs > 0
              ? `${metrics.completed_runs} runs in ${window}`
              : "No completed runs in window"
          }
          tone="info"
          loading={isLoading}
        />
        <KpiTile
          icon={<TriangleAlertIcon className="h-3.5 w-3.5" />}
          label="Change failure rate"
          value={failPct}
          sub={
            metrics && metrics.completed_runs > 0
              ? `${metrics.failed_runs} of ${metrics.completed_runs} failed`
              : "No data"
          }
          tone={
            metrics && metrics.change_failure_rate > 0.15
              ? "warn"
              : metrics && metrics.change_failure_rate > 0.05
                ? "neutral"
                : "good"
          }
          loading={isLoading}
        />
        <KpiTile
          icon={<TimerIcon className="h-3.5 w-3.5" />}
          label="MTTR"
          value={formatMinutes(metrics?.mttr_minutes)}
          sub={
            metrics?.mttr_minutes
              ? "Avg failure → success"
              : "No recovery cycles observed"
          }
          tone={
            metrics && metrics.mttr_minutes > 120
              ? "warn"
              : metrics && metrics.mttr_minutes > 30
                ? "neutral"
                : "good"
          }
          loading={isLoading}
        />
        <KpiTile
          icon={<GaugeIcon className="h-3.5 w-3.5" />}
          label="Lead time"
          value={formatMinutes(metrics?.lead_time_minutes)}
          sub="Request → completion"
          tone="info"
          loading={isLoading}
        />
      </div>
    </section>
  );
}

interface KpiTileProps {
  icon: React.ReactNode;
  label: string;
  value: string;
  sub: string;
  // Tone tints the tile border and value color. We deliberately
  // stay restrained — too many alarming reds desensitize the
  // operator and undermine real warnings elsewhere on the page.
  tone: "good" | "neutral" | "warn" | "info";
  loading?: boolean;
}

function KpiTile({ icon, label, value, sub, tone, loading }: KpiTileProps) {
  const color =
    tone === "good"
      ? "var(--success)"
      : tone === "warn"
        ? "var(--destructive)"
        : tone === "neutral"
          ? "var(--warning)"
          : "var(--primary)";
  return (
    <Card>
      <CardContent className="p-3.5">
        <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-muted-foreground">
          <span style={{ color }}>{icon}</span>
          <span>{label}</span>
        </div>
        <div
          className="mt-1.5 font-tabular text-2xl font-semibold"
          style={{ color: loading ? "var(--muted-foreground)" : color }}
        >
          {loading ? "…" : value}
        </div>
        <div className="mt-1 text-[11px] text-muted-foreground">{sub}</div>
      </CardContent>
    </Card>
  );
}
