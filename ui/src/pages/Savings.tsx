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
  CoinsIcon,
  InfoIcon,
  RefreshCwIcon,
  SparklesIcon,
  TrendingDownIcon,
  WalletIcon,
} from "lucide-react";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import { getAgents } from "@/api/agents";
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
  getPricingProjection,
  matchPricingRule,
  monthlyUSDFor,
  type PricingConfig,
  type PricingProjection,
} from "@/api/pricing";
import {
  getRecommendations,
  type Recommendation,
} from "@/api/recommendations";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  groupFlowsByDestination,
  parseAgentFlows,
} from "@/components/fleet-map/exporter-parser";

const WINDOWS: { value: InsightsWindow; label: string }[] = [
  { value: "1h", label: "1 hour" },
  { value: "24h", label: "24 hours" },
];

export default function SavingsPage() {
  const [win, setWin] = useState<InsightsWindow>("24h");
  const [refreshing, setRefreshing] = useState(false);

  const {
    data: projection,
    mutate: mutateProjection,
  } = useSWR<PricingProjection>(
    `pricing-projection-${win}`,
    () => getPricingProjection(win),
    { refreshInterval: 60_000 },
  );
  const { data: pricingCfg } = useSWR<PricingConfig>(
    "pricing-config",
    getPricingConfig,
    { refreshInterval: 300_000 },
  );
  const {
    data: recs,
    mutate: mutateRecs,
  } = useSWR(
    `savings-recs-${win}`,
    () => getRecommendations(win, 10),
    { refreshInterval: 60_000 },
  );
  const {
    data: topAgents,
    mutate: mutateTopAgents,
  } = useSWR(
    `savings-top-agents-${win}`,
    () => getTopAgents({ window: win, limit: 500 }),
    { refreshInterval: 60_000 },
  );
  const { data: agentsResp, mutate: mutateAgents } = useSWR(
    "savings-agents",
    () => getAgents({ limit: 500 }),
  );
  const { data: fleet } = useSWR<FleetSummary>(
    `savings-fleet-${win}`,
    () => getFleetVolume({ window: win }),
  );

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
      const flows = parseAgentFlows(agent.effective_config);
      const groups = groupFlowsByDestination(flows);
      if (!groups.length) continue;
      const equalShare = ag.total_bytes / groups.length;
      for (const g of groups) {
        const existing = byKey.get(g.destinationKey) ?? {
          key: g.destinationKey,
          label: g.label,
          bytes: 0,
          monthlyUSD: 0,
        };
        existing.bytes += equalShare;
        byKey.set(g.destinationKey, existing);
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

      {/* Hero block */}
      {!pricingEnabled ? (
        <DisabledNotice />
      ) : (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <HeroSpend projection={projection} fleet={fleet} />
          <HeroPotential
            potentialMonthly={potentialMonthly}
            currency={projection?.currency ?? "USD"}
            recCount={recs?.items.length ?? 0}
          />
        </div>
      )}

      {/* Quick Wins — recommendations ranked by $ saved. */}
      {pricingEnabled && (
        <QuickWinsPanel recs={recs?.items ?? []} onChanged={() => mutateRecs()} />
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

// ----------------------------------------------------------------
// Quick Wins
// ----------------------------------------------------------------

function QuickWinsPanel({
  recs,
  onChanged: _onChanged,
}: {
  recs: Recommendation[];
  onChanged: () => void;
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
            <QuickWinRow key={r.id} rec={r} />
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

function QuickWinRow({ rec }: { rec: Recommendation }) {
  const savings = rec.est_savings_per_month_usd ?? 0;
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
      {rec.snippet && (
        <Link
          to="/configs/new"
          state={{
            prefillName: rec.title.slice(0, 60),
            prefillSnippet: rec.snippet,
            source: "savings",
            recommendationId: rec.id,
          }}
          className="inline-flex shrink-0 items-center gap-1 rounded-md border border-border px-3 py-1.5 text-xs text-muted-foreground hover:bg-accent hover:text-accent-foreground"
        >
          <SparklesIcon className="h-3 w-3" />
          Apply
          <ArrowRightIcon className="h-3 w-3" />
        </Link>
      )}
    </div>
  );
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
              Tune these in <code className="font-mono text-xs">squadron.yaml</code>{" "}
              under <code className="font-mono text-xs">pricing.rules</code>.
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
          $/month estimates here. Once enabled, Squadron uses a starter rate
          set covering the major destinations; tune the per-destination rates
          against your actual invoice for accurate projections.
        </div>
      </CardContent>
    </Card>
  );
}
