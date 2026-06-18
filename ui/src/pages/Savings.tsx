/**
 * Savings — v0.27 dollar-projected cost dashboard.
 *
 * Designed for SMB / mid-market operators who think in $/month,
 * not bytes. Three things on this page, top to bottom:
 *
 *   1. Hero: "Estimated $X/month at current ingest rate" + agent count.
 *      The big number. If pricing is disabled this collapses to a
 *      one-liner pointing to docs/savings.md.
 *
 *   2. Quick Wins: top recommendations ranked by $/month saved.
 *      Each card reuses the recommendation actions (copy snippet,
 *      open in editor, dismiss). Apply via existing v0.25 deep-link.
 *
 *   3. Per-destination breakdown: bar list showing $/month routed
 *      to each configured destination (computed client-side from
 *      the v0.24 destination attribution + the pricing rules).
 *
 * Pricing assumptions footer is always visible so operators see
 * the inputs feeding their dollar figures. Link to docs/savings.md
 * for tuning.
 */

import {
  ArrowRightIcon,
  CheckCircle2Icon,
  CircleDashedIcon,
  CoinsIcon,
  HistoryIcon,
  InfoIcon,
  PiggyBankIcon,
  RefreshCwIcon,
  SparklesIcon,
  TrendingDownIcon,
  WalletIcon,
} from "lucide-react";
import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import useSWR from "swr";

import { getAgents } from "@/api/agents";
import { fetchBillingSnapshot, formatBytes } from "@/api/billing";
import {
  getFleetVolume,
  getTopAgents,
  type AgentVolume,
  type FleetSummary,
  type InsightsWindow,
} from "@/api/insights";
import {
  formatUSD,
  getPricingConfig,
  getPricingForecast,
  getPricingProjection,
  matchPricingRule,
  monthlyUSDFor,
  type PricingConfig,
  type PricingProjection,
} from "@/api/pricing";
import {
  applyRecommendation,
  getRecommendations,
  type Recommendation,
} from "@/api/recommendations";
import {
  getRealizedSavings,
  type RealizedSavingsResponse,
} from "@/api/savings";
import { CostSpikesPanel } from "@/components/cost-spikes/CostSpikesPanel";
import {
  groupFlowsByDestination,
  parseAgentFlows,
} from "@/components/fleet-map/exporter-parser";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

const WINDOWS: { value: InsightsWindow; label: string }[] = [
  { value: "1h", label: "1 hour" },
  { value: "24h", label: "24 hours" },
];

export default function SavingsPage() {
  const [win, setWin] = useState<InsightsWindow>("24h");
  const [refreshing, setRefreshing] = useState(false);

  const { data: projection, mutate: mutateProjection } =
    useSWR<PricingProjection>(
      `pricing-projection-${win}`,
      () => getPricingProjection(win),
      { refreshInterval: 60_000 },
    );
  const { data: pricingCfg } = useSWR<PricingConfig>(
    "pricing-config",
    getPricingConfig,
    { refreshInterval: 300_000 },
  );
  const { data: recs, mutate: mutateRecs } = useSWR(
    `savings-recs-${win}`,
    () => getRecommendations(win, 10),
    { refreshInterval: 60_000 },
  );
  const { data: topAgents, mutate: mutateTopAgents } = useSWR(
    `savings-top-agents-${win}`,
    () => getTopAgents({ window: win, limit: 500 }),
    { refreshInterval: 60_000 },
  );
  const { data: agentsResp, mutate: mutateAgents } = useSWR(
    "savings-agents",
    () => getAgents({ limit: 500 }),
  );
  const { data: fleet } = useSWR<FleetSummary>(`savings-fleet-${win}`, () =>
    getFleetVolume({ window: win }),
  );
  // v0.28 retrospective: refreshes on every page load, plus every
  // minute alongside the other panels. Re-observation happens lazily
  // on the GET, so a refresh here also recomputes realized savings.
  const { data: realized, mutate: mutateRealized } =
    useSWR<RealizedSavingsResponse>("savings-realized", getRealizedSavings, {
      refreshInterval: 60_000,
    });

  // Per-destination $/month breakdown — computed client-side from
  // the v0.24 destination attribution + the pricing rules. Matches
  // how the v0.24 DestinationBreakdown panel does its byte math.
  const destinations = useMemo(() => {
    if (!agentsResp || !topAgents || !pricingCfg) return [];
    const windowSeconds = win === "1h" ? 3600 : win === "24h" ? 86400 : 300;
    type Row = {
      key: string;
      label: string;
      bytes: number;
      monthlyUSD: number;
    };
    // Build destination → bytes map by walking each agent's
    // effective_config + their TopAgents byte total.
    const byKey = new Map<string, Row>();
    for (const ag of topAgents.items as AgentVolume[]) {
      const agent = agentsResp.items.find((a) => a.id === ag.agent_id);
      if (!agent || !agent.effective_config) continue;
      const flows = parseAgentFlows(agent);
      const groups = groupFlowsByDestination(flows);
      if (!groups.length) continue;
      const equalShare = ag.total_bytes / groups.length;
      for (const g of groups) {
        const existing = byKey.get(g.key) ?? {
          key: g.key,
          label: g.label,
          bytes: 0,
          monthlyUSD: 0,
        };
        existing.bytes += equalShare;
        byKey.set(g.key, existing);
      }
    }
    // Price each row using the matched rule.
    const out: Row[] = [];
    for (const row of byKey.values()) {
      const rule = matchPricingRule(row.key, pricingCfg.rules);
      // Don't differentiate by signal at this level (we don't have
      // per-destination signal split); use base rate.
      row.monthlyUSD = monthlyUSDFor(row.bytes, windowSeconds, undefined, rule);
      out.push(row);
    }
    out.sort((a, b) => b.monthlyUSD - a.monthlyUSD);
    return out;
  }, [agentsResp, topAgents, pricingCfg, win]);

  // Total of recommendation $/month for the Potential Savings hero.
  const potentialMonthly = useMemo(() => {
    if (!recs) return 0;
    return recs.items.reduce(
      (sum, r) => sum + (r.est_savings_per_month_usd ?? 0),
      0,
    );
  }, [recs]);

  const refreshAll = async () => {
    setRefreshing(true);
    await Promise.all([
      mutateProjection(),
      mutateRecs(),
      mutateTopAgents(),
      mutateAgents(),
      mutateRealized(),
    ]);
    setRefreshing(false);
  };

  const pricingEnabled = projection?.enabled ?? pricingCfg?.enabled ?? false;

  return (
    <div className="flex flex-col gap-6 p-6">
      <header className="flex items-start justify-between gap-4">
        <div>
          <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
            Squadron
          </div>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">
            Savings
          </h1>
          <p className="text-sm text-muted-foreground">
            What you're spending today, and what you could save by applying the
            engine's recommendations. Estimates use the configured pricing
            assumptions.
          </p>
        </div>
        <div className="flex items-center gap-3">
          <Tabs value={win} onValueChange={(v) => setWin(v as InsightsWindow)}>
            <TabsList>
              {WINDOWS.map((w) => (
                <TabsTrigger key={w.value} value={w.value}>
                  {w.label}
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

      {/* v0.29 cost-spike alerts. Renders above the hero block
          so operators see "the bill is about to spike" before
          they see the current projection. */}
      <CostSpikesPanel />

      {/* Hero block */}
      {!pricingEnabled ? (
        <DisabledNotice />
      ) : (
        <>
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
            <HeroSpend projection={projection} fleet={fleet} />
            <HeroPotential
              potentialMonthly={potentialMonthly}
              currency={projection?.currency ?? "USD"}
              recCount={recs?.items.length ?? 0}
            />
            <HeroRealized realized={realized} />
          </div>
          {/* v0.39 month-end forecast strip. Sits below the hero
              tiles because it derives from the same projection, but
              tells a different story: 'at the current rate, here's
              where you'll land by the end of the calendar month'
              with a progress indicator showing how much of the
              month has already been spent. */}
          <ForecastStrip />
          {/* v0.42 billing connector — surfaces ACTUAL ingest from
              the destination's billing API (Splunk for v0.42).
              Silently hides when no connector is configured (the
              fetcher returns null on 204). */}
          <BillingStrip />
        </>
      )}

      {/* Quick Wins — recommendations ranked by $ saved. */}
      {pricingEnabled && (
        <QuickWinsPanel
          recs={recs?.items ?? []}
          onApplied={async () => {
            await Promise.all([mutateRecs(), mutateRealized()]);
          }}
        />
      )}

      {/* v0.28: realized savings audit trail. Lists the outcomes
          we've already tracked and their post-apply status. */}
      {pricingEnabled && realized && realized.outcomes?.length > 0 && (
        <RealizedOutcomesPanel realized={realized} />
      )}

      {/* Per-destination breakdown. */}
      {pricingEnabled && destinations.length > 0 && (
        <DestinationSpend rows={destinations} />
      )}

      {/* Pricing assumptions footer. */}
      {pricingEnabled && projection?.assumptions && (
        <AssumptionsFooter rules={projection.assumptions} />
      )}
    </div>
  );
}

// ----------------------------------------------------------------
// v0.42 billing-connector strip
// ----------------------------------------------------------------
//
// Renders the ACTUAL ingest reported by the configured billing
// connector (Splunk for v0.42) next to the estimated number from
// Squadron's own receiver. Hidden silently when no connector is
// configured — the API returns 204 / null and fetchBillingSnapshot
// surfaces that as null.

function BillingStrip() {
  const { data } = useSWR("billing-snapshot", fetchBillingSnapshot, {
    refreshInterval: 60_000,
  });
  if (!data) return null;
  return (
    <Card>
      <CardContent className="space-y-2 p-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-muted-foreground">
              Actual ingest · {data.provider} · {data.window}
            </div>
            <div className="mt-1 flex items-baseline gap-2">
              <div className="font-tabular text-3xl font-semibold text-foreground">
                {formatBytes(data.bytes)}
              </div>
              <div className="text-xs text-muted-foreground">
                reported by destination
              </div>
            </div>
          </div>
          <div className="text-right text-[11px] text-muted-foreground">
            <div>
              Collected{" "}
              <span className="font-tabular text-foreground">
                {new Date(data.at).toLocaleString()}
              </span>
            </div>
            {data.source_url && (
              <div>
                <a
                  href={data.source_url}
                  target="_blank"
                  rel="noreferrer"
                  className="underline hover:text-foreground"
                >
                  Open in {data.provider}
                </a>
              </div>
            )}
          </div>
        </div>
        <div className="text-[11px] text-muted-foreground">
          Compares against the estimated ingest above. If the destination number
          is materially lower, your dedup / filtering rules are eating the
          difference — usually a good thing.
        </div>
      </CardContent>
    </Card>
  );
}

// ----------------------------------------------------------------
// v0.39 forecast strip
// ----------------------------------------------------------------
//
// Horizontal panel sitting under the three hero tiles. Two columns:
// the left shows the projected calendar-month spend with a delta vs
// the steady-state, the right shows a progress bar split into
// elapsed ($ already spent this month) and remaining ($ to go).
//
// Intentionally minimalist — this is one of those panels operators
// will glance at and either care about (because the forecast is
// climbing) or completely ignore. No chart, no animations, no
// destination breakdown.

function ForecastStrip() {
  const { data, isLoading } = useSWR("pricing-forecast", getPricingForecast, {
    refreshInterval: 60_000,
  });

  // Hide when pricing is off or first poll is in-flight. We don't
  // want a flickering "—" tile to confuse first-time visitors.
  if (!data || !data.enabled || isLoading) return null;

  const forecast = data.forecast_usd ?? 0;
  const spent = data.spent_so_far_usd ?? 0;
  const remaining = data.remaining_usd ?? 0;
  const fraction = Math.min(Math.max(data.fraction_elapsed ?? 0, 0), 1);
  const month = data.calendar_month ?? "this month";

  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-muted-foreground">
              Forecasted spend · {month}
            </div>
            <div className="mt-1 flex items-baseline gap-2">
              <div className="font-tabular text-3xl font-semibold text-foreground">
                {formatUSD(forecast)}
              </div>
              <div className="text-xs text-muted-foreground">
                projected at current rate
              </div>
            </div>
          </div>
          <div className="text-right text-xs text-muted-foreground">
            <div>
              <span className="font-tabular text-foreground">
                {formatUSD(spent)}
              </span>{" "}
              spent so far
            </div>
            <div>
              <span className="font-tabular text-foreground">
                {formatUSD(remaining)}
              </span>{" "}
              remaining
            </div>
          </div>
        </div>
        {/* Progress bar. Two segments — spent (cyan) + remaining
            (muted) — proportional to elapsed vs remaining fraction
            of the month. The width snaps to whole percent for stable
            rendering when fraction nudges between polls. */}
        <div className="flex h-2 w-full overflow-hidden rounded">
          <div
            style={{
              width: `${Math.round(fraction * 100)}%`,
              background: "var(--primary)",
            }}
          />
          <div
            style={{
              width: `${100 - Math.round(fraction * 100)}%`,
              background:
                "color-mix(in oklch, var(--muted-foreground) 30%, transparent)",
            }}
          />
        </div>
        <div className="flex items-center justify-between text-[11px] text-muted-foreground">
          <span>
            Day {Math.floor(data.days_elapsed ?? 0) + 1} of {data.days_in_month}
          </span>
          <span>
            Linear extrapolation from the last 24h ingest rate. Real invoices
            will vary.
          </span>
        </div>
      </CardContent>
    </Card>
  );
}

// ----------------------------------------------------------------
// Hero blocks
// ----------------------------------------------------------------

function HeroSpend({
  projection,
  fleet,
}: {
  projection: PricingProjection | undefined;
  fleet: FleetSummary | undefined;
}) {
  const monthly = projection?.monthly_usd ?? 0;
  return (
    <Card>
      <CardContent className="p-6">
        <div className="flex items-center gap-2 text-xs uppercase tracking-[0.16em] text-muted-foreground">
          <WalletIcon className="h-3.5 w-3.5" />
          Estimated monthly spend
        </div>
        <div className="mt-2 font-tabular text-4xl font-semibold">
          {formatUSD(monthly)}
          <span className="ml-2 text-sm font-normal text-muted-foreground">
            / month
          </span>
        </div>
        <div className="mt-3 text-sm text-muted-foreground">
          Projected from the last{" "}
          {projection?.window === "1h" ? "hour" : "24 hours"} of ingest at the
          current pricing assumptions
          {fleet?.agent_count
            ? ` across ${fleet.agent_count} agent${fleet.agent_count === 1 ? "" : "s"}.`
            : "."}{" "}
          Real invoices vary by retention tier and contract.
        </div>
      </CardContent>
    </Card>
  );
}

function HeroPotential({
  potentialMonthly,
  currency: _currency,
  recCount,
}: {
  potentialMonthly: number;
  currency: string;
  recCount: number;
}) {
  return (
    <Card>
      <CardContent className="p-6">
        <div className="flex items-center gap-2 text-xs uppercase tracking-[0.16em] text-muted-foreground">
          <TrendingDownIcon
            className="h-3.5 w-3.5"
            style={{ color: "var(--success)" }}
          />
          Potential monthly savings
        </div>
        <div
          className="mt-2 font-tabular text-4xl font-semibold"
          style={{ color: "var(--success)" }}
        >
          {formatUSD(potentialMonthly)}
          <span className="ml-2 text-sm font-normal text-muted-foreground">
            / month
          </span>
        </div>
        <div className="mt-3 text-sm text-muted-foreground">
          If you apply{" "}
          <span className="font-medium text-foreground">
            {recCount} {recCount === 1 ? "recommendation" : "recommendations"}
          </span>{" "}
          ranked below. Estimates are sampled; validate against your invoice
          before adopting.
        </div>
      </CardContent>
    </Card>
  );
}

function HeroRealized({
  realized,
}: {
  realized: RealizedSavingsResponse | undefined;
}) {
  const monthly = realized?.monthly_realized_usd ?? 0;
  const realizedCount = realized?.counts?.realized ?? 0;
  const pendingCount = realized?.counts?.pending ?? 0;
  return (
    <Card>
      <CardContent className="p-6">
        <div className="flex items-center gap-2 text-xs uppercase tracking-[0.16em] text-muted-foreground">
          <PiggyBankIcon
            className="h-3.5 w-3.5"
            style={{ color: "var(--chart-2)" }}
          />
          Saved this month
        </div>
        <div
          className="mt-2 font-tabular text-4xl font-semibold"
          style={{ color: "var(--chart-2)" }}
        >
          {formatUSD(monthly)}
          <span className="ml-2 text-sm font-normal text-muted-foreground">
            / month
          </span>
        </div>
        <div className="mt-3 text-sm text-muted-foreground">
          {realizedCount === 0 && pendingCount === 0 ? (
            <>
              Click <span className="font-medium text-foreground">Apply</span>{" "}
              on a Quick Win above to start tracking. Squadron measures
              post-apply byte rates and reports the delta here.
            </>
          ) : (
            <>
              <span className="font-medium text-foreground">
                {realizedCount}
              </span>{" "}
              {realizedCount === 1 ? "recommendation" : "recommendations"}{" "}
              measurably reduced byte rate
              {pendingCount > 0 ? `, ${pendingCount} still settling.` : "."}
            </>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

// ----------------------------------------------------------------
// Quick Wins
// ----------------------------------------------------------------

function QuickWinsPanel({
  recs,
  onApplied,
}: {
  recs: Recommendation[];
  onApplied: () => Promise<void> | void;
}) {
  // Rank by $/month desc; drop ones with zero savings.
  const ranked = useMemo(() => {
    return [...recs]
      .filter((r) => (r.est_savings_per_month_usd ?? 0) > 0)
      .sort(
        (a, b) =>
          (b.est_savings_per_month_usd ?? 0) -
          (a.est_savings_per_month_usd ?? 0),
      );
  }, [recs]);

  if (ranked.length === 0) {
    return (
      <Card>
        <CardContent className="flex items-center gap-2 p-6 text-sm text-muted-foreground">
          <InfoIcon className="h-4 w-4" />
          No high-impact recommendations active right now. The engine
          re-evaluates every 30 seconds; check back as your fleet ingests more
          data, or look at Cost Insights for the underlying byte rankings.
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardContent className="p-4">
        <div className="mb-3 flex items-baseline justify-between">
          <div>
            <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
              Quick wins
            </div>
            <div className="text-sm text-muted-foreground">
              Recommendations ranked by projected monthly savings
            </div>
          </div>
          <div className="font-tabular text-[11px] text-muted-foreground">
            {ranked.length} actionable
          </div>
        </div>
        <div className="space-y-2">
          {ranked.map((r) => (
            <QuickWinRow key={r.id} rec={r} onApplied={onApplied} />
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

function QuickWinRow({
  rec,
  onApplied,
}: {
  rec: Recommendation;
  onApplied: () => Promise<void> | void;
}) {
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const savings = rec.est_savings_per_month_usd ?? 0;

  // Apply records the click server-side (snapshots the current
  // baseline + frozen recommendation view) and then opens the
  // editor pre-filled. If recording fails we still navigate —
  // the editor flow is the operator's actual goal and we don't
  // want to block on the audit-trail write.
  const handleApply = async () => {
    if (busy) return;
    setBusy(true);
    try {
      await applyRecommendation(rec.id, {
        title: rec.title,
        category: rec.category,
        signal: rec.signal,
        est_savings_per_month_usd: rec.est_savings_per_month_usd,
        est_savings_bytes: rec.est_savings_bytes,
      });
    } catch {
      // Best-effort; swallow.
    } finally {
      setBusy(false);
    }
    void onApplied();
    if (rec.snippet) {
      navigate("/configs/new", {
        state: {
          prefillName: rec.title.slice(0, 60),
          prefillSnippet: rec.snippet,
          source: "savings",
          recommendationId: rec.id,
        },
      });
    }
  };

  return (
    <div className="flex items-center justify-between gap-4 rounded-md border border-border bg-background/40 p-3 transition-colors hover:bg-background/70">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          <div className="text-sm font-medium leading-snug">{rec.title}</div>
          <Badge variant="outline" className="text-[10px] uppercase">
            {rec.severity}
          </Badge>
        </div>
        <div className="mt-0.5 text-xs text-muted-foreground line-clamp-1">
          {rec.detail}
        </div>
      </div>
      <div className="font-tabular shrink-0 text-right">
        <div
          className="text-base font-semibold"
          style={{ color: "var(--success)" }}
        >
          {formatUSD(savings)}
          <span className="ml-1 text-xs font-normal text-muted-foreground">
            /mo
          </span>
        </div>
      </div>
      {rec.snippet ? (
        <button
          type="button"
          onClick={handleApply}
          disabled={busy}
          className="inline-flex shrink-0 items-center gap-1 rounded-md border border-border px-3 py-1.5 text-xs text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-60"
        >
          <SparklesIcon className="h-3 w-3" />
          {busy ? "Applying…" : "Apply"}
          <ArrowRightIcon className="h-3 w-3" />
        </button>
      ) : (
        <button
          type="button"
          onClick={handleApply}
          disabled={busy}
          className="inline-flex shrink-0 items-center gap-1 rounded-md border border-border px-3 py-1.5 text-xs text-muted-foreground hover:bg-accent hover:text-accent-foreground disabled:opacity-60"
          title="No copy-paste snippet for this recommendation — clicking still records the apply for savings tracking."
        >
          <SparklesIcon className="h-3 w-3" />
          {busy ? "Recording…" : "Mark applied"}
        </button>
      )}
    </div>
  );
}

// ----------------------------------------------------------------
// Realized outcomes (v0.28 retrospective tracker)
// ----------------------------------------------------------------

function RealizedOutcomesPanel({
  realized,
}: {
  realized: RealizedSavingsResponse;
}) {
  const outcomes = useMemo(
    () =>
      [...(realized.outcomes ?? [])].sort(
        (a, b) =>
          new Date(b.applied_at).getTime() - new Date(a.applied_at).getTime(),
      ),
    [realized.outcomes],
  );
  if (outcomes.length === 0) return null;
  return (
    <Card>
      <CardContent className="p-4">
        <div className="mb-3 flex items-baseline justify-between">
          <div>
            <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
              Tracked outcomes
            </div>
            <div className="text-sm text-muted-foreground">
              Recommendations the team has applied, with measured post-apply
              byte rate
            </div>
          </div>
          <div className="font-tabular text-[11px] text-muted-foreground">
            {outcomes.length} total
          </div>
        </div>
        <div className="space-y-2">
          {outcomes.map((o) => (
            <div
              key={o.id}
              className="flex items-center justify-between gap-4 rounded-md border border-border p-3"
            >
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <StatusIcon status={o.status} />
                  <div className="truncate text-sm font-medium">{o.title}</div>
                </div>
                <div className="mt-0.5 text-[11px] text-muted-foreground">
                  Applied{" "}
                  <span className="font-tabular">
                    {new Date(o.applied_at).toLocaleString()}
                  </span>{" "}
                  by{" "}
                  <span className="font-mono text-[11px]">{o.applied_by}</span>
                  {" — "}
                  baseline{" "}
                  <span className="font-tabular">
                    {humanBytes(o.baseline_bytes_per_hour)}/h
                  </span>
                  {o.last_observed_at && (
                    <>
                      {" → "}observed{" "}
                      <span className="font-tabular">
                        {humanBytes(o.last_observed_bytes_per_hour)}/h
                      </span>
                    </>
                  )}
                </div>
              </div>
              <div className="font-tabular shrink-0 text-right">
                <div
                  className="text-sm font-semibold"
                  style={{
                    color:
                      o.status === "realized"
                        ? "var(--chart-2)"
                        : "var(--muted-foreground)",
                  }}
                >
                  {formatUSD(o.realized_savings_per_month_usd)}
                  <span className="ml-1 text-[10px] font-normal text-muted-foreground">
                    /mo
                  </span>
                </div>
                <div className="text-[10px] uppercase tracking-[0.12em] text-muted-foreground">
                  {o.status.replace("_", " ")}
                </div>
              </div>
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

function StatusIcon({ status }: { status: string }) {
  if (status === "realized") {
    return (
      <CheckCircle2Icon
        className="h-3.5 w-3.5 shrink-0"
        style={{ color: "var(--chart-2)" }}
      />
    );
  }
  if (status === "pending") {
    return (
      <CircleDashedIcon className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
    );
  }
  return <HistoryIcon className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />;
}

function humanBytes(b: number): string {
  if (!b || b < 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = b;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
}

// ----------------------------------------------------------------
// Per-destination breakdown
// ----------------------------------------------------------------

function DestinationSpend({
  rows,
}: {
  rows: { key: string; label: string; bytes: number; monthlyUSD: number }[];
}) {
  const maxMonthly = Math.max(...rows.map((r) => r.monthlyUSD), 1);
  return (
    <Card>
      <CardContent className="p-4">
        <div className="mb-3 flex items-baseline justify-between">
          <div>
            <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
              Destination spend
            </div>
            <div className="text-sm text-muted-foreground">
              Estimated $/month routed to each destination
            </div>
          </div>
          <Badge variant="outline" className="text-[10px] uppercase">
            Estimated
          </Badge>
        </div>
        <div className="space-y-2">
          {rows.map((row) => (
            <div
              key={row.key}
              className="flex items-center gap-3 rounded-md border border-border p-2.5"
            >
              <CoinsIcon className="h-4 w-4 shrink-0 text-muted-foreground" />
              <div className="min-w-0 flex-1">
                <div className="text-sm font-medium">{row.label}</div>
                <div className="mt-1 h-1.5 w-full overflow-hidden rounded-sm bg-muted/40">
                  <div
                    className="h-full rounded-sm"
                    style={{
                      width: `${Math.max(2, (row.monthlyUSD / maxMonthly) * 100)}%`,
                      background: "var(--chart-2)",
                    }}
                  />
                </div>
              </div>
              <div className="font-tabular shrink-0 text-right text-sm font-semibold">
                {formatUSD(row.monthlyUSD)}
                <span className="ml-1 text-xs font-normal text-muted-foreground">
                  /mo
                </span>
              </div>
            </div>
          ))}
        </div>
        <div className="mt-3 text-[11px] text-muted-foreground">
          Bytes are attributed by evenly pro-rating each agent's volume across
          its configured exporters. True per-destination egress measurement is
          on the v0.27+ roadmap.
        </div>
      </CardContent>
    </Card>
  );
}

// ----------------------------------------------------------------
// Assumptions footer
// ----------------------------------------------------------------

function AssumptionsFooter({
  rules,
}: {
  rules: { match: string; label?: string; price_per_gb: number }[];
}) {
  return (
    <Card>
      <CardContent className="p-4">
        <div className="flex items-baseline justify-between">
          <div>
            <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
              Pricing assumptions
            </div>
            <div className="text-sm text-muted-foreground">
              Tune these in{" "}
              <code className="font-mono text-xs">squadron.yaml</code> under{" "}
              <code className="font-mono text-xs">pricing.rules</code>.
            </div>
          </div>
        </div>
        <div className="mt-2 flex flex-wrap gap-1.5 text-[11px]">
          {rules.map((r, i) => (
            <span
              key={i}
              className="font-tabular rounded-md border border-border px-2 py-0.5 text-muted-foreground"
            >
              {r.label || r.match || "Other"}: ${r.price_per_gb.toFixed(2)}/GB
            </span>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

// ----------------------------------------------------------------
// Disabled-state notice
// ----------------------------------------------------------------

function DisabledNotice() {
  return (
    <Card>
      <CardContent className="p-6">
        <div className="text-xs uppercase tracking-[0.16em] text-muted-foreground">
          Pricing projection is off
        </div>
        <div className="mt-2 text-sm">
          Set <code className="font-mono text-xs">pricing.enabled: true</code>{" "}
          in <code className="font-mono text-xs">squadron.yaml</code> to see
          $/month estimates here. Once enabled, Squadron uses a starter rate set
          covering the major destinations; tune the per-destination rates
          against your actual invoice for accurate projections.
        </div>
      </CardContent>
    </Card>
  );
}
