/**
 * Fleet Status — the Squadron landing page.
 *
 * This is intentionally not a "telemetry dashboard with 40 charts".
 * It's mission-control glance copy: are agents reporting in, is
 * anything drifted, are rollouts healthy, are alerts firing, who
 * touched what recently. Everything links to a deeper view.
 *
 * Data sources are existing v1 APIs — no new backend endpoints
 * required for v0.19. SWR keys mirror the page-level pages so any
 * SSE-driven invalidation that lives elsewhere also refreshes the
 * dashboard for free.
 */

import {
  ActivityIcon,
  AlertTriangleIcon,
  ArrowRightIcon,
  CircleAlertIcon,
  CircleCheckIcon,
  CircleDotIcon,
  CircleHelpIcon,
  CircleSlashIcon,
  Inbox,
  RocketIcon,
  ServerIcon,
} from "lucide-react";
import * as React from "react";
import { Link } from "react-router-dom";
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip } from "recharts";
import useSWR from "swr";

import { getAgents, getAgentStats } from "@/api/agents";
import { listAlertRules } from "@/api/alerts";
import { listAuditEvents } from "@/api/audit";
import { listIncidentDrafts } from "@/api/incidents";
import { listRollouts } from "@/api/rollouts";
import { AskSquadronHero } from "@/components/AskSquadronHero";
import { SquadronMark } from "@/components/brand/SquadronMark";
import { CostSpikesBanner } from "@/components/cost-spikes/CostSpikesPanel";
import { InventorySummary } from "@/components/inventory/InventoryPanel";
import { FleetHealthSummary } from "@/components/pipeline-health/PipelineHealthPanel";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import type { Agent, ConfigDriftStatus } from "@/types/agent";
import type { Rollout } from "@/types/rollout";

// ============================================================
// Hero metrics — the "everything fine?" strip across the top
// ============================================================

interface HeroStat {
  label: string;
  value: string | number;
  hint?: string;
  trend?: "good" | "neutral" | "warn" | "bad";
  icon: React.ComponentType<{ className?: string }>;
  href?: string;
}

function HeroMetric({ stat }: { stat: HeroStat }) {
  const Icon = stat.icon;
  const trendDot = {
    good: "var(--success)",
    neutral: "var(--muted-foreground)",
    warn: "var(--warning)",
    bad: "var(--destructive)",
  }[stat.trend ?? "neutral"];

  const inner = (
    <Card className="relative overflow-hidden bg-card/80 backdrop-blur transition-colors hover:bg-card border-border/70 hover:border-border">
      <CardContent className="p-5">
        <div className="flex items-center justify-between gap-2">
          <span className="text-xs uppercase tracking-wider text-muted-foreground font-medium">
            {stat.label}
          </span>
          <Icon className="h-4 w-4 text-muted-foreground" />
        </div>
        <div className="mt-3 flex items-end gap-3">
          <span className="font-tabular text-4xl font-semibold leading-none tracking-tight text-foreground">
            {stat.value}
          </span>
          {stat.trend && (
            <span
              className="status-dot mb-1"
              style={{ ["--dot" as string]: trendDot }}
            />
          )}
        </div>
        {stat.hint && (
          <div className="mt-2 text-xs text-muted-foreground">{stat.hint}</div>
        )}
      </CardContent>
    </Card>
  );
  return stat.href ? (
    <Link to={stat.href} className="block">
      {inner}
    </Link>
  ) : (
    inner
  );
}

// ============================================================
// Drift status donut — the centerpiece
// ============================================================

const DRIFT_META: Record<
  ConfigDriftStatus,
  { label: string; color: string; iconBg: string }
> = {
  synced: {
    label: "Synced",
    color: "var(--success)",
    iconBg: "bg-[oklch(0.75_0.14_165/.18)]",
  },
  drifted: {
    label: "Drifted",
    color: "var(--destructive)",
    iconBg: "bg-[oklch(0.7_0.2_20/.18)]",
  },
  no_intent: {
    label: "No intent",
    color: "var(--muted-foreground)",
    iconBg: "bg-[oklch(0.7_0.018_250/.18)]",
  },
  no_effective: {
    label: "Awaiting agent",
    color: "var(--warning)",
    iconBg: "bg-[oklch(0.8_0.16_75/.18)]",
  },
  unknown: {
    label: "Unknown",
    color: "var(--muted-foreground)",
    iconBg: "bg-[oklch(0.7_0.018_250/.18)]",
  },
};

function tallyDrift(
  agents: Agent[] | undefined,
): Record<ConfigDriftStatus, number> {
  const out: Record<ConfigDriftStatus, number> = {
    synced: 0,
    drifted: 0,
    no_intent: 0,
    no_effective: 0,
    unknown: 0,
  };
  for (const a of agents ?? []) {
    const k: ConfigDriftStatus = a.drift_status ?? "unknown";
    out[k]++;
  }
  return out;
}

function DriftDonut({ agents }: { agents: Agent[] | undefined }) {
  const tally = tallyDrift(agents);
  const total = Object.values(tally).reduce((a, b) => a + b, 0);
  const order: ConfigDriftStatus[] = [
    "synced",
    "drifted",
    "no_effective",
    "no_intent",
    "unknown",
  ];
  const data = order
    .map((k) => ({ name: DRIFT_META[k].label, key: k, value: tally[k] }))
    .filter((d) => d.value > 0);
  const synced = tally.synced;
  const syncedPct = total > 0 ? Math.round((synced / total) * 100) : 0;
  const allSynced = total > 0 && synced === total;

  return (
    <Card className="bg-card/80 border-border/70 backdrop-blur">
      <CardContent className="p-6">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold uppercase tracking-wider text-foreground">
              Fleet Sync Status
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Effective config vs. intended config
            </p>
          </div>
          {allSynced ? (
            <Badge
              variant="outline"
              className="border-[var(--success)]/40 bg-[var(--success)]/10 text-[var(--success)]"
            >
              <CircleCheckIcon className="mr-1 h-3 w-3" />
              All in sync
            </Badge>
          ) : null}
        </div>

        <div className="mt-4 grid grid-cols-1 gap-6 md:grid-cols-2 items-center">
          <div className="relative h-44 w-full">
            <ResponsiveContainer>
              <PieChart>
                <Pie
                  data={
                    data.length > 0
                      ? data
                      : [{ name: "Empty", value: 1, key: "unknown" }]
                  }
                  innerRadius={56}
                  outerRadius={78}
                  paddingAngle={data.length > 1 ? 2 : 0}
                  dataKey="value"
                  stroke="var(--background)"
                  strokeWidth={2}
                  isAnimationActive={false}
                >
                  {(data.length > 0
                    ? data
                    : [{ key: "unknown" as ConfigDriftStatus }]
                  ).map((d, i) => (
                    <Cell key={i} fill={DRIFT_META[d.key].color} />
                  ))}
                </Pie>
                {data.length > 0 && (
                  <Tooltip
                    contentStyle={{
                      background: "var(--popover)",
                      border: "1px solid var(--border)",
                      borderRadius: 8,
                      fontSize: 12,
                    }}
                  />
                )}
              </PieChart>
            </ResponsiveContainer>
            <div className="absolute inset-0 flex flex-col items-center justify-center pointer-events-none">
              <div className="font-tabular text-3xl font-semibold leading-none tracking-tight text-foreground">
                {syncedPct}%
              </div>
              <div className="text-[10px] uppercase tracking-wider text-muted-foreground mt-1">
                Synced
              </div>
            </div>
          </div>

          <ul className="space-y-2">
            {order.map((k) => {
              const n = tally[k];
              if (n === 0) return null;
              const meta = DRIFT_META[k];
              return (
                <li key={k}>
                  <Link
                    to={`/agents?drift_status=${k}`}
                    className="flex items-center justify-between gap-2 rounded-md px-2 py-1.5 text-sm hover:bg-accent/40 transition-colors"
                  >
                    <span className="flex items-center gap-2">
                      <span
                        className="status-dot"
                        style={{ ["--dot" as string]: meta.color }}
                      />
                      <span className="text-foreground">{meta.label}</span>
                    </span>
                    <span className="font-tabular text-sm text-muted-foreground">
                      {n}
                    </span>
                  </Link>
                </li>
              );
            })}
            {total === 0 && (
              <li className="text-sm text-muted-foreground italic">
                No agents reporting yet.
              </li>
            )}
          </ul>
        </div>
      </CardContent>
    </Card>
  );
}

// ============================================================
// Active rollouts — three at most, with progress
// ============================================================

function ROLLOUT_STATE_COLOR(state: Rollout["state"]) {
  switch (state) {
    case "in_progress":
      return "var(--info)";
    case "paused":
      return "var(--warning)";
    case "succeeded":
      return "var(--success)";
    case "aborted":
    case "rolled_back":
      return "var(--destructive)";
    case "pending":
    default:
      return "var(--muted-foreground)";
  }
}

function ActiveRollouts({ rollouts }: { rollouts: Rollout[] | undefined }) {
  const active = (rollouts ?? []).filter(
    (r) =>
      r.state === "in_progress" ||
      r.state === "pending" ||
      r.state === "paused",
  );
  return (
    <Card className="bg-card/80 border-border/70 backdrop-blur">
      <CardContent className="p-6">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold uppercase tracking-wider text-foreground">
              Active Rollouts
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Staged config deployments in flight
            </p>
          </div>
          <Link
            to="/rollouts"
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            All rollouts <ArrowRightIcon className="h-3 w-3" />
          </Link>
        </div>
        <div className="mt-4 space-y-3">
          {active.length === 0 && (
            <div className="rounded-md border border-dashed border-border/60 px-4 py-8 text-center">
              <p className="text-sm text-muted-foreground">
                Nothing in flight. The fleet is steady.
              </p>
              <Link
                to="/rollouts"
                className="mt-2 inline-flex items-center gap-1 text-xs text-primary hover:underline"
              >
                Stage a rollout <ArrowRightIcon className="h-3 w-3" />
              </Link>
            </div>
          )}
          {active.slice(0, 5).map((r) => {
            const totalStages = r.stages.length;
            const pct =
              totalStages > 0
                ? Math.round(((r.current_stage + 1) / totalStages) * 100)
                : 0;
            return (
              <Link
                key={r.id}
                to="/rollouts"
                className="block rounded-lg border border-border/60 bg-background/40 px-4 py-3 transition-colors hover:border-border hover:bg-accent/30"
              >
                <div className="flex items-center justify-between gap-2">
                  <div className="min-w-0">
                    <div className="truncate text-sm font-medium text-foreground">
                      {r.name || "Unnamed rollout"}
                    </div>
                    <div className="mt-0.5 text-xs text-muted-foreground">
                      Stage {r.current_stage + 1} of {totalStages}
                    </div>
                  </div>
                  <Badge
                    variant="outline"
                    className="border-current/40 bg-transparent text-xs"
                    style={{ color: ROLLOUT_STATE_COLOR(r.state) }}
                  >
                    {r.state.replace("_", " ")}
                  </Badge>
                </div>
                <div className="mt-3 h-1.5 w-full overflow-hidden rounded-full bg-muted/40">
                  <div
                    className="h-full rounded-full transition-all"
                    style={{
                      width: `${pct}%`,
                      background: ROLLOUT_STATE_COLOR(r.state),
                    }}
                  />
                </div>
              </Link>
            );
          })}
        </div>
      </CardContent>
    </Card>
  );
}

// ============================================================
// Alerts + Audit — twin side-by-side panels
// ============================================================

function RecentAlerts({
  alerts,
}: {
  alerts:
    | { id: string; name: string; severity: string; enabled: boolean }[]
    | undefined;
}) {
  const list = (alerts ?? []).filter((a) => a.enabled).slice(0, 5);
  return (
    <Card className="bg-card/80 border-border/70 backdrop-blur">
      <CardContent className="p-6">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold uppercase tracking-wider text-foreground">
              Alert Rules
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Active monitors watching the fleet
            </p>
          </div>
          <Link
            to="/alerts"
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            All rules <ArrowRightIcon className="h-3 w-3" />
          </Link>
        </div>
        <ul className="mt-4 space-y-2">
          {list.length === 0 && (
            <li className="rounded-md border border-dashed border-border/60 px-4 py-8 text-center text-sm text-muted-foreground">
              No active alert rules configured.
            </li>
          )}
          {list.map((a) => (
            <li
              key={a.id}
              className="flex items-center justify-between gap-2 rounded-md border border-border/40 px-3 py-2"
            >
              <span className="flex items-center gap-2 truncate text-sm text-foreground">
                <CircleAlertIcon
                  className="h-3.5 w-3.5 flex-shrink-0"
                  style={{
                    color:
                      a.severity === "critical"
                        ? "var(--destructive)"
                        : a.severity === "warning"
                          ? "var(--warning)"
                          : "var(--info)",
                  }}
                />
                <span className="truncate">{a.name}</span>
              </span>
              <Badge variant="outline" className="text-[10px] uppercase">
                {a.severity}
              </Badge>
            </li>
          ))}
        </ul>
      </CardContent>
    </Card>
  );
}

// Compact map for audit event icons. Kept small on purpose; we only
// surface the most common types and fall back to a neutral dot for
// the rest. Don't over-special-case here — the audit page handles
// the full taxonomy.
function auditIcon(eventType: string) {
  if (eventType.startsWith("agent.")) return ServerIcon;
  if (eventType.startsWith("rollout.")) return RocketIcon;
  if (eventType.startsWith("alert.") || eventType.includes("drift"))
    return CircleAlertIcon;
  if (eventType.startsWith("config.")) return CircleDotIcon;
  return CircleHelpIcon;
}

function relTime(iso: string): string {
  const t = new Date(iso).getTime();
  const now = Date.now();
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.round(h / 24);
  return `${d}d ago`;
}

function RecentActivity({
  events,
}: {
  events:
    | { id: string; timestamp: string; actor: string; event_type: string }[]
    | undefined;
}) {
  const list = (events ?? []).slice(0, 8);
  return (
    <Card className="bg-card/80 border-border/70 backdrop-blur">
      <CardContent className="p-6">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-semibold uppercase tracking-wider text-foreground">
              Activity Log
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Latest events from across the squadron
            </p>
          </div>
          <Link
            to="/audit"
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            All events <ArrowRightIcon className="h-3 w-3" />
          </Link>
        </div>
        <ul className="mt-4 space-y-1.5">
          {list.length === 0 && (
            <li className="rounded-md border border-dashed border-border/60 px-4 py-8 text-center text-sm text-muted-foreground">
              Activity will appear here as the fleet operates.
            </li>
          )}
          {list.map((e) => {
            const Icon = auditIcon(e.event_type);
            return (
              <li
                key={e.id}
                className="flex items-start gap-3 rounded-md px-3 py-2 hover:bg-accent/30"
              >
                <Icon className="mt-0.5 h-3.5 w-3.5 flex-shrink-0 text-muted-foreground" />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm text-foreground">
                    <span className="font-mono text-xs text-muted-foreground">
                      {e.actor.split(":")[0] || "system"}
                    </span>{" "}
                    {e.event_type}
                  </div>
                </div>
                <span className="font-tabular text-xs text-muted-foreground">
                  {relTime(e.timestamp)}
                </span>
              </li>
            );
          })}
        </ul>
      </CardContent>
    </Card>
  );
}

// ============================================================
// Page
// ============================================================

export default function DashboardPage() {
  // Same SWR keys other pages use — invalidations triggered by SSE
  // for any of these caches benefit the dashboard at no extra cost.
  // The donut + drift counters need the full fleet's drift_status
  // tally, so we ask for a single 500-agent page. /agents/stats
  // gives us the fleet-wide online/offline totals separately. At
  // fleets >500 the donut numbers will be a "top 500" sample; a
  // future improvement is a server-side drift breakdown endpoint.
  const { data: agentsResp } = useSWR("/agents-dashboard", () =>
    getAgents({ limit: 500 }),
  );
  const { data: stats } = useSWR("/agents/stats", () => getAgentStats());
  const { data: rollouts } = useSWR<Rollout[]>("/rollouts", () =>
    listRollouts(),
  );
  const { data: alerts } = useSWR("/alerts/rules", () => listAlertRules());
  const { data: auditEvents } = useSWR("/audit/events?limit=20", () =>
    listAuditEvents({ limit: 20 }),
  );
  // v0.54 Move 3 — open incident draft count for the inbox tile.
  // Polls a bit faster than the page-level Incidents view since the
  // dashboard is where operators most often check "is there anything
  // waiting for me?".
  const { data: openDrafts } = useSWR(
    "/incidents/drafts?status=draft",
    () => listIncidentDrafts({ status: "draft" }),
    { refreshInterval: 30_000 },
  );

  const agents: Agent[] = React.useMemo(
    () => agentsResp?.items ?? [],
    [agentsResp],
  );
  const tally = tallyDrift(agents);
  const activeRollouts = (rollouts ?? []).filter(
    (r) =>
      r.state === "in_progress" ||
      r.state === "paused" ||
      r.state === "pending",
  ).length;
  const firingRules = (alerts ?? []).filter((a) => a.enabled).length;

  const heroStats: HeroStat[] = [
    {
      label: "Online agents",
      value: `${stats?.onlineAgents ?? 0}/${stats?.totalAgents ?? 0}`,
      hint:
        stats && stats.totalAgents > 0
          ? `${Math.round(((stats.onlineAgents ?? 0) / stats.totalAgents) * 100)}% reporting`
          : "no agents yet",
      trend:
        stats && stats.totalAgents > 0
          ? stats.onlineAgents === stats.totalAgents
            ? "good"
            : "warn"
          : "neutral",
      icon: ServerIcon,
      href: "/agents",
    },
    {
      label: "Drifted",
      value: tally.drifted,
      hint:
        tally.drifted > 0
          ? "Out of sync with intended config"
          : "Everything in sync",
      trend: tally.drifted > 0 ? "bad" : "good",
      icon: CircleSlashIcon,
      href: "/agents?drift_status=drifted",
    },
    {
      label: "Active rollouts",
      value: activeRollouts,
      hint: activeRollouts > 0 ? "In flight" : "Steady state",
      trend: activeRollouts > 0 ? "neutral" : "good",
      icon: RocketIcon,
      href: "/rollouts",
    },
    {
      label: "Alert rules",
      value: firingRules,
      hint: firingRules > 0 ? "Monitors enabled" : "No monitors configured",
      trend: "neutral",
      icon: AlertTriangleIcon,
      href: "/alerts",
    },
    // v0.54 Move 3 — incident drafts inbox. Surfaces the count of
    // AI drafted postmortem tickets waiting for review so the
    // operator catches them without having to open /incidents.
    {
      label: "Incident drafts",
      value: (openDrafts ?? []).length,
      hint: (openDrafts ?? []).length > 0 ? "Awaiting review" : "Inbox empty",
      trend: (openDrafts ?? []).length > 0 ? "warn" : "good",
      icon: Inbox,
      href: "/incidents",
    },
  ];

  return (
    <div className="flex flex-col gap-6">
      {/* Hero */}
      <header className="relative">
        <div className="flex items-center gap-3">
          <div className="brand-glow flex h-12 w-12 items-center justify-center rounded-lg border border-border/60 bg-card/60 backdrop-blur">
            <SquadronMark className="h-6 w-6" />
          </div>
          <div>
            <div className="text-[11px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
              Mission Control
            </div>
            <h1 className="text-2xl font-semibold tracking-tight text-foreground">
              Fleet Status
            </h1>
          </div>
          <div className="ml-auto flex items-center gap-2 text-xs text-muted-foreground">
            <ActivityIcon className="h-3.5 w-3.5 text-[var(--success)]" />
            <span className="font-tabular">Live</span>
          </div>
        </div>
      </header>

      {/* v0.81 — Ask Squadron hero. Most prominent placement so a
          new operator landing on the Dashboard sees the deputy
          first. Renders null when AI is off so a misconfigured
          deployment doesn't see a teaser for a feature that's not
          wired. Sits above the cost-spike banner because the
          deputy can ANSWER questions about the spike — and a new
          operator's first reaction to an alert is more useful if
          they know there's a conversational surface to query
          against. */}
      <AskSquadronHero />

      {/* v0.29 cost-spike heads-up. Renders only when there are
          open, unacknowledged spikes — otherwise null. Lives above
          the first-run banner so an active spike beats out the
          "no agents yet" nudge if both somehow fire. */}
      <CostSpikesBanner />

      {/* v0.31 fleet pipeline-health summary. One-row stacked bar
          of how many agents are healthy / degraded / broken /
          unknown — derived from collector self-metrics, refreshes
          every 10s. Returns null when no samples exist yet, so a
          brand-new install doesn't see an empty bar. */}
      <FleetHealthSummary />

      {/* v0.32 inventory reconciliation summary. One-row stacked
          bar of how many tracked hosts are healthy / missing /
          unexpected — derived from the expected-vs-actual diff,
          refreshes every 30s. Returns null when no CI pipeline has
          ever submitted an expected list. */}
      <InventorySummary />

      {/* v0.27.1 first-run banner. Only renders when no agent has
          ever connected; once the first agent shows up, this
          quietly disappears for the rest of the install's life. */}
      {stats?.totalAgents === 0 && (
        <Link to="/quickstart" className="block">
          <Card className="border-[var(--info)]/40 bg-[var(--info)]/10 transition-colors hover:bg-[var(--info)]/15">
            <CardContent className="flex items-center justify-between p-4">
              <div className="flex items-center gap-3">
                <div className="rounded-md border border-border bg-background/40 p-2">
                  <RocketIcon
                    className="h-4 w-4"
                    style={{ color: "var(--info)" }}
                  />
                </div>
                <div>
                  <div className="text-sm font-medium">
                    No agents yet — let's get your first one connected
                  </div>
                  <div className="text-xs text-muted-foreground">
                    The Quickstart wizard takes a few minutes whether you're
                    starting fresh or already have collectors running.
                  </div>
                </div>
              </div>
              <div className="text-xs font-medium text-foreground/80">
                Open Quickstart →
              </div>
            </CardContent>
          </Card>
        </Link>
      )}

      {/* Hero metrics */}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {heroStats.map((s) => (
          <HeroMetric key={s.label} stat={s} />
        ))}
      </div>

      {/* Donut + active rollouts */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <DriftDonut agents={agents} />
        <ActiveRollouts rollouts={rollouts} />
      </div>

      {/* Alerts + activity */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <RecentAlerts alerts={alerts} />
        <RecentActivity events={auditEvents} />
      </div>
    </div>
  );
}
