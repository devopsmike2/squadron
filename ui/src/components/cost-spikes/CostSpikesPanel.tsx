/**
 * Cost Spikes panel — v0.29.
 *
 * Two visual modes off the same hook:
 *   - <CostSpikesBanner/> — Dashboard heads-up. One line, dismissible.
 *   - <CostSpikesPanel/>  — Savings-page detail. Per-spike attribution.
 *
 * Data flows from /api/v1/alerts/cost-spikes. Acknowledging hides the
 * Dashboard banner for that spike but keeps it in the Savings panel
 * — the detector auto-closes it when projection recovers.
 */

import {
  AlertTriangleIcon,
  CheckIcon,
  RefreshCwIcon,
  TrendingUpIcon,
  UsersIcon,
} from "lucide-react";
import { useMemo, useState } from "react";
import useSWR from "swr";

import {
  acknowledgeCostSpike,
  listCostSpikes,
  parseAttribution,
  type CostSpikeEvent,
} from "@/api/costspikes";
import { formatUSD } from "@/api/pricing";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";

const SWR_KEY = "cost-spikes-open";

export function useOpenCostSpikes() {
  return useSWR(SWR_KEY, () => listCostSpikes("open", 50), {
    refreshInterval: 30_000,
  });
}

/**
 * Heads-up banner — renders on the Dashboard above the fleet
 * status. Suppresses spikes the operator has already acknowledged.
 */
export function CostSpikesBanner() {
  const { data } = useOpenCostSpikes();
  const unack = useMemo(
    () => (data?.items ?? []).filter((s) => !s.acknowledged_at),
    [data],
  );
  if (!unack.length) return null;

  const worst = unack.reduce((a, b) =>
    a.peak_pct_above_baseline >= b.peak_pct_above_baseline ? a : b,
  );
  const isCritical = unack.some((s) => s.severity === "critical");
  return (
    <a
      href="/savings"
      data-tour="cost-spike-banner"
      className={`flex items-center gap-3 rounded-md border p-3 text-sm transition-colors ${
        isCritical
          ? "border-destructive/50 bg-destructive/10"
          : "border-warning/50 bg-warning/10"
      }`}
      style={{
        // shadcn doesn't always ship a --warning token; fall back to
        // the chart's amber stop so the banner is visible on both
        // themes.
        borderColor: isCritical ? "var(--destructive)" : "var(--chart-4)",
      }}
    >
      <AlertTriangleIcon
        className="h-4 w-4 shrink-0"
        style={{
          color: isCritical ? "var(--destructive)" : "var(--chart-4)",
        }}
      />
      <div className="flex-1">
        <div className="font-medium">
          {unack.length === 1
            ? "Cost spike detected"
            : `${unack.length} cost spikes detected`}
        </div>
        <div className="mt-0.5 text-xs text-muted-foreground">
          Projection jumped{" "}
          <span className="font-tabular text-foreground">
            +{(worst.peak_pct_above_baseline * 100).toFixed(0)}%
          </span>{" "}
          over the rolling baseline (now{" "}
          <span className="font-tabular text-foreground">
            {formatUSD(worst.peak_monthly_usd)}/mo
          </span>
          ). Click for attribution.
        </div>
      </div>
      <Badge
        variant="outline"
        className="text-[10px] uppercase"
        style={{
          color: isCritical ? "var(--destructive)" : "var(--chart-4)",
        }}
      >
        {isCritical ? "Critical" : "Warn"}
      </Badge>
    </a>
  );
}

/**
 * Detailed panel — renders on the Savings page when there are open
 * spikes. Each row exposes the attribution (which agents / which
 * attribute keys grew most) and an Acknowledge button.
 */
export function CostSpikesPanel() {
  const { data, mutate } = useOpenCostSpikes();
  const spikes = data?.items ?? [];
  if (!spikes.length) return null;
  return (
    <Card>
      <CardContent className="p-4">
        <div className="mb-3 flex items-baseline justify-between">
          <div>
            <div className="flex items-center gap-2 text-xs uppercase tracking-[0.16em] text-muted-foreground">
              <AlertTriangleIcon
                className="h-3.5 w-3.5"
                style={{ color: "var(--destructive)" }}
              />
              Open cost spikes
            </div>
            <div className="text-sm text-muted-foreground">
              The projection is meaningfully higher than the rolling baseline.
              Auto-closes when it drops back.
            </div>
          </div>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => mutate()}
            className="h-7 gap-1"
          >
            <RefreshCwIcon className="h-3.5 w-3.5" />
            Refresh
          </Button>
        </div>
        <div className="space-y-3">
          {spikes.map((s) => (
            <SpikeRow key={s.id} spike={s} onChanged={() => mutate()} />
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

function SpikeRow({
  spike,
  onChanged,
}: {
  spike: CostSpikeEvent;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const attribution = parseAttribution(spike.attribution_json);
  const isCritical = spike.severity === "critical";

  const handleAck = async () => {
    if (busy) return;
    setBusy(true);
    try {
      await acknowledgeCostSpike(spike.id);
      await onChanged();
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      className={`rounded-md border p-3 ${
        isCritical ? "border-destructive/40" : "border-border"
      }`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <Badge
              variant="outline"
              className="text-[10px] uppercase"
              style={{
                color: isCritical ? "var(--destructive)" : "var(--chart-4)",
              }}
            >
              {spike.severity}
            </Badge>
            {spike.signal && (
              <Badge variant="outline" className="text-[10px] uppercase">
                {spike.signal}
              </Badge>
            )}
            <span className="text-[11px] text-muted-foreground">
              Started{" "}
              <span className="font-tabular">
                {new Date(spike.started_at).toLocaleString()}
              </span>
              {spike.acknowledged_at && (
                <>
                  {" · acknowledged by "}
                  <span className="font-mono">{spike.acknowledged_by}</span>
                </>
              )}
            </span>
          </div>
          <div className="mt-1.5 font-tabular text-sm">
            <span className="font-semibold">
              {formatUSD(spike.peak_monthly_usd)}/mo
            </span>{" "}
            peak vs.{" "}
            <span className="text-muted-foreground">
              {formatUSD(spike.baseline_monthly_usd)}/mo baseline
            </span>{" "}
            <span
              className="inline-flex items-center gap-0.5"
              style={{
                color: isCritical ? "var(--destructive)" : "var(--chart-4)",
              }}
            >
              <TrendingUpIcon className="h-3 w-3" />+
              {(spike.peak_pct_above_baseline * 100).toFixed(0)}%
            </span>
          </div>
        </div>
        {!spike.acknowledged_at && (
          <Button
            variant="outline"
            size="sm"
            onClick={handleAck}
            disabled={busy}
            className="h-7 gap-1"
          >
            <CheckIcon className="h-3.5 w-3.5" />
            {busy ? "Acking…" : "Acknowledge"}
          </Button>
        )}
      </div>
      {attribution && (
        <div className="mt-3 grid gap-3 text-xs sm:grid-cols-2">
          {attribution.top_agents && attribution.top_agents.length > 0 && (
            <div>
              <div className="mb-1 flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-muted-foreground">
                <UsersIcon className="h-3 w-3" />
                Top agents
              </div>
              <ul className="space-y-0.5">
                {attribution.top_agents.map((a) => (
                  <li
                    key={a.agent_id}
                    className="flex justify-between gap-2 truncate"
                  >
                    <span className="truncate">
                      {a.agent_name || a.agent_id}
                    </span>
                    <span className="font-tabular shrink-0 text-muted-foreground">
                      {a.bytes_pct || "—"}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
          {attribution.top_attributes &&
            attribution.top_attributes.length > 0 && (
              <div>
                <div className="mb-1 text-[10px] uppercase tracking-wider text-muted-foreground">
                  Top attributes
                </div>
                <ul className="space-y-0.5">
                  {attribution.top_attributes.map((a) => (
                    <li
                      key={a.key}
                      className="flex justify-between gap-2 truncate"
                    >
                      <code className="truncate font-mono">{a.key}</code>
                      <span className="font-tabular shrink-0 text-muted-foreground">
                        {a.bytes_pct || "—"}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            )}
        </div>
      )}
    </div>
  );
}
