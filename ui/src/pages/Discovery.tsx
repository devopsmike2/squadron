// Discovery — the v0.89.62 #689 Stream 87 (slice-1 chunk 2) unified
// Discovery dashboard. Renders the aggregation endpoint shipped by
// chunk 1 (internal/api/handlers/discovery_summary.go, v0.89.61) into
// a single screen so an operator with multi-cloud fleets sees the
// four-cloud claim concretely instead of having to click through
// /discovery/aws → /discovery/gcp → /discovery/azure → /discovery/oci.
//
// Slice 1 honesty:
//   - The page is read-only — it aggregates counts, it doesn't
//     create / edit / delete connections. Operators click the
//     per-provider "View details →" link to land on the wizard.
//   - The page polls the backend on mount and on the manual refresh
//     button. Real-time SSE updates are a slice-2 candidate
//     (design doc §2 non-goals).
//   - The recent recommendations table is the 10 most recent across
//     all four providers, newest first. Clicking a row navigates to
//     the per-provider page root; deep-linking to the specific
//     recommendation is slice 2.
//   - The coverage ring is inline SVG (no Chart.js / d3 dependency)
//     so the page stays a single-component flat file with no
//     additional bundle weight. Three thresholds drive the stroke
//     color: green ≥ 80, yellow 50–79, red < 50.
//   - When all four providers report enabled=false AND zero
//     connections, the dashboard renders a welcome state with four
//     Connect buttons. Coverage panel + recommendations table are
//     suppressed so the welcome state is the visual focus.
//
// Strategic frame (design doc §11): this is the screenshot Squadron
// uses to demonstrate the four-cloud claim. A screenshot of
// /discovery/aws is "Squadron scans AWS." A screenshot of
// /discovery is "Squadron is the universal observability control
// plane." The difference matters.

import { Cloud, RefreshCw } from "lucide-react";
import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import {
  getDiscoverySummary,
  type DiscoverySummary,
  type ProviderSummary,
  type RecentRecommendation,
} from "@/api/discoverySummary";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

// The SWR cache key the page reads. Lives at /discovery/summary to
// match the wire endpoint — a future cross-page integration (the
// command palette, say) can read the same cache without re-fetching.
const SWR_KEY_SUMMARY = "/discovery/summary";

// PROVIDER_ORDER is the deterministic render order for the four
// provider cards. Mirrors the design doc §6 order (AWS first, OCI
// last) so the cards always read left-to-right in the same order
// regardless of which providers are enabled in the deployment.
const PROVIDER_ORDER: Array<keyof DiscoverySummary["providers"]> = [
  "aws",
  "gcp",
  "azure",
  "oci",
];

// PROVIDER_LABEL maps the wire-format provider key to the operator-
// visible uppercase label rendered on each card. AWS / GCP / OCI are
// already uppercase; Azure renders as "AZURE" for visual parity with
// the others in the 4-card grid header row.
const PROVIDER_LABEL: Record<keyof DiscoverySummary["providers"], string> = {
  aws: "AWS",
  gcp: "GCP",
  azure: "AZURE",
  oci: "OCI",
};

// PROVIDER_PATH maps a provider key to its per-provider page. The
// dashboard's "View details →" + Connect buttons all route here.
const PROVIDER_PATH: Record<keyof DiscoverySummary["providers"], string> = {
  aws: "/discovery/aws",
  gcp: "/discovery/gcp",
  azure: "/discovery/azure",
  oci: "/discovery/oci",
};

// PROVIDER_BADGE picks the tone for the provider badge in the
// recent-recommendations table. Tones are abstract (slate / amber /
// cyan / rose) rather than per-cloud brand colors — the cards above
// already carry the provider name in large type, so the table badge
// is just a quick visual distinguisher.
const PROVIDER_BADGE: Record<RecentRecommendation["provider"], string> = {
  aws: "border-amber-300 bg-amber-50 text-amber-900 dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-200",
  gcp: "border-sky-300 bg-sky-50 text-sky-900 dark:border-sky-700 dark:bg-sky-950/40 dark:text-sky-200",
  azure:
    "border-cyan-300 bg-cyan-50 text-cyan-900 dark:border-cyan-700 dark:bg-cyan-950/40 dark:text-cyan-200",
  oci: "border-rose-300 bg-rose-50 text-rose-900 dark:border-rose-700 dark:bg-rose-950/40 dark:text-rose-200",
};

export default function DiscoveryPage() {
  // lastFetchedAt is the local wall-clock timestamp at the moment the
  // SWR fetcher resolved. The header renders this as relative time
  // ("5s ago") so the operator can tell at a glance whether the page
  // is showing live data or stale.
  const [lastFetchedAt, setLastFetchedAt] = useState<Date | null>(null);

  // SWR with manual revalidation off — the dashboard polls on the
  // refresh button and on mount, not on focus or interval. Slice 1
  // intentionally drops the auto-refresh signals so the operator
  // sees the same numbers across a triage session without surprise
  // jumps mid-conversation.
  const { data, error, isLoading, mutate } = useSWR<DiscoverySummary>(
    SWR_KEY_SUMMARY,
    async () => {
      const r = await getDiscoverySummary();
      setLastFetchedAt(new Date());
      return r;
    },
    {
      revalidateOnFocus: false,
      revalidateOnReconnect: false,
    },
  );

  const handleRefresh = useCallback(() => {
    void mutate();
  }, [mutate]);

  return (
    <div className="space-y-6 p-6">
      <DashboardHeader
        summary={data}
        lastFetchedAt={lastFetchedAt}
        onRefresh={handleRefresh}
        loading={isLoading}
      />

      {error && (
        <div
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          Failed to load the unified Discovery summary:{" "}
          {error instanceof Error ? error.message : String(error)}
        </div>
      )}

      {data && <DashboardBody summary={data} />}
    </div>
  );
}

// --- Header -------------------------------------------------------------

function DashboardHeader({
  summary,
  lastFetchedAt,
  onRefresh,
  loading,
}: {
  summary: DiscoverySummary | undefined;
  lastFetchedAt: Date | null;
  onRefresh: () => void;
  loading: boolean;
}) {
  const instances = summary?.totals.instance_count ?? 0;
  const conns = summary?.totals.connection_count ?? 0;
  const connWord = conns === 1 ? "connection" : "connections";
  return (
    <header className="flex flex-wrap items-start justify-between gap-3">
      <div>
        <div className="flex items-center gap-2">
          <Cloud className="h-5 w-5 text-primary" aria-hidden />
          <h1 className="text-2xl font-semibold">Discovery</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Squadron sees {instances} resources across {conns} {connWord}.
        </p>
      </div>
      <div className="flex items-center gap-3">
        {lastFetchedAt && (
          <span
            className="text-xs text-muted-foreground"
            aria-label="Last refreshed"
          >
            Refreshed {relativeTime(lastFetchedAt)}
          </span>
        )}
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={onRefresh}
          disabled={loading}
          aria-label="Refresh dashboard"
        >
          <RefreshCw
            className={
              loading ? "mr-2 h-3.5 w-3.5 animate-spin" : "mr-2 h-3.5 w-3.5"
            }
            aria-hidden
          />
          Refresh
        </Button>
      </div>
    </header>
  );
}

// --- Body switcher ------------------------------------------------------

function DashboardBody({ summary }: { summary: DiscoverySummary }) {
  // Welcome state condition: no provider has been wired AND no
  // connection exists anywhere. Either signal alone is enough — a
  // deployment with all four stores wired but zero connections still
  // gets the welcome state, because the dashboard has nothing to
  // show. Likewise a deployment that only wires AWS but with one
  // connection skips the welcome state and renders the per-provider
  // cards (the other three render in their connect-state form).
  const allDisabled = PROVIDER_ORDER.every(
    (k) => !summary.providers[k]?.enabled,
  );
  const noConnections = summary.totals.connection_count === 0;
  if (allDisabled && noConnections) {
    return <WelcomeState />;
  }
  return (
    <div className="space-y-6">
      <CoveragePanel totals={summary.totals} />
      <ProviderGrid providers={summary.providers} />
      <RecentRecommendations rows={summary.recent_recommendations} />
    </div>
  );
}

// --- Welcome state (all four providers disabled + no connections) ------

function WelcomeState() {
  return (
    <div
      className="rounded-lg border bg-card p-8 text-center"
      data-testid="discovery-welcome"
    >
      <Cloud className="mx-auto h-10 w-10 text-primary" aria-hidden />
      <h2 className="mt-3 text-lg font-semibold">
        Welcome to Squadron Discovery.
      </h2>
      <p className="mx-auto mt-1 max-w-md text-sm text-muted-foreground">
        Connect your first cloud to start seeing observability gaps.
      </p>
      <div className="mt-5 grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-4">
        {PROVIDER_ORDER.map((p) => (
          <Button
            key={p}
            asChild
            variant="outline"
            aria-label={`Connect ${PROVIDER_LABEL[p]}`}
          >
            <Link to={PROVIDER_PATH[p]}>Connect {PROVIDER_LABEL[p]}</Link>
          </Button>
        ))}
      </div>
    </div>
  );
}

// --- Coverage panel ----------------------------------------------------

function CoveragePanel({ totals }: { totals: DiscoverySummary["totals"] }) {
  const pct = totals.coverage_pct;
  const color = coverageColor(pct);
  return (
    <div className="rounded-lg border bg-card p-6">
      <div className="flex flex-wrap items-center gap-6">
        <CoverageRing pct={pct} color={color} />
        <div>
          <div
            className="text-4xl font-semibold tabular-nums"
            data-testid="coverage-pct"
          >
            {pct}%
          </div>
          <p className="mt-1 text-sm text-muted-foreground">
            {totals.instrumented_count} of {totals.instance_count} instances
            instrumented across all providers
          </p>
        </div>
      </div>
    </div>
  );
}

// CoverageRing renders the SVG ring with stroke-dasharray driving the
// partial arc length. Circumference = 2 * π * r; the dashoffset is
// circumference minus the visible arc length so the ring "fills" from
// the 12 o'clock position clockwise. The unfilled portion shows as a
// muted track ring underneath. Color comes from the parent so the
// threshold logic lives in one place (coverageColor).
function CoverageRing({ pct, color }: { pct: number; color: string }) {
  const size = 96;
  const stroke = 10;
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  // Clamp displayed arc to [0, 100] so a server-side rounding glitch
  // can't render an over-full ring.
  const filled = Math.max(0, Math.min(100, pct));
  const offset = c - (filled / 100) * c;
  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${size} ${size}`}
      role="img"
      aria-label={`Overall coverage ${pct}%`}
      data-testid="coverage-ring"
      data-color={color}
    >
      <circle
        cx={size / 2}
        cy={size / 2}
        r={r}
        fill="none"
        stroke="currentColor"
        strokeOpacity={0.15}
        strokeWidth={stroke}
        className="text-muted-foreground"
      />
      <circle
        cx={size / 2}
        cy={size / 2}
        r={r}
        fill="none"
        stroke={color}
        strokeWidth={stroke}
        strokeDasharray={c}
        strokeDashoffset={offset}
        strokeLinecap="round"
        transform={`rotate(-90 ${size / 2} ${size / 2})`}
        data-testid="coverage-ring-fill"
      />
    </svg>
  );
}

// coverageColor picks the threshold band per design doc §6:
//   - green for >= 80
//   - yellow for 50..79
//   - red for < 50
// The values are hex literals so the SVG `stroke=` attribute reads
// cleanly across both dark and light modes; Tailwind tokens would
// require a CSS var indirection that the SVG circle doesn't honor.
function coverageColor(pct: number): string {
  if (pct >= 80) return "#16a34a"; // green-600
  if (pct >= 50) return "#ca8a04"; // yellow-600
  return "#dc2626"; // red-600
}

// --- Provider cards ---------------------------------------------------

function ProviderGrid({
  providers,
}: {
  providers: DiscoverySummary["providers"];
}) {
  return (
    <div
      className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4"
      data-testid="provider-grid"
    >
      {PROVIDER_ORDER.map((p) => (
        <ProviderCard key={p} provider={p} summary={providers[p]} />
      ))}
    </div>
  );
}

function ProviderCard({
  provider,
  summary,
}: {
  provider: keyof DiscoverySummary["providers"];
  summary: ProviderSummary | undefined;
}) {
  const label = PROVIDER_LABEL[provider];
  const path = PROVIDER_PATH[provider];

  // The backend always returns all four keys so summary should never
  // be undefined; the optional-chain guards against a partial older
  // response (which can occur during a rolling upgrade where the new
  // UI talks to the old backend that hasn't shipped the summary
  // handler yet).
  if (!summary || !summary.enabled) {
    return (
      <div
        className="rounded-lg border border-dashed bg-muted/20 p-4 opacity-90"
        data-testid={`provider-card-${provider}`}
        data-enabled="false"
      >
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold text-muted-foreground">
            {label}
          </h3>
        </div>
        <p className="mt-2 text-xs text-muted-foreground">
          Connect {label} to add to your fleet view.
        </p>
        <div className="mt-3">
          <Button
            asChild
            size="sm"
            variant="outline"
            aria-label={`Connect ${label}`}
          >
            <Link to={path}>Connect {label}</Link>
          </Button>
        </div>
      </div>
    );
  }

  const pct = computeProviderCoverage(
    summary.instrumented_count,
    summary.instance_count,
  );
  return (
    <div
      className="rounded-lg border bg-card p-4"
      data-testid={`provider-card-${provider}`}
      data-enabled="true"
    >
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-semibold">{label}</h3>
        <span
          className="text-xs tabular-nums text-muted-foreground"
          aria-label={`${label} coverage`}
        >
          {pct}%
        </span>
      </div>
      <dl className="mt-3 space-y-1 text-xs">
        <div className="flex justify-between">
          <dt className="text-muted-foreground">Connections</dt>
          <dd
            className="tabular-nums"
            data-testid={`provider-${provider}-connections`}
          >
            {summary.connection_count}
          </dd>
        </div>
        <div className="flex justify-between">
          <dt className="text-muted-foreground">Instances</dt>
          <dd
            className="tabular-nums"
            data-testid={`provider-${provider}-instances`}
          >
            {summary.instance_count}
          </dd>
        </div>
      </dl>
      <div className="mt-3 text-right">
        <Link
          to={path}
          className="text-xs font-medium text-primary hover:underline"
        >
          View details →
        </Link>
      </div>
    </div>
  );
}

// computeProviderCoverage mirrors the server-side coverage_pct
// computation per provider. Zero-safe: returns 0 when instance count
// is 0. Rounded to one decimal so a provider with 1 of 3 instrumented
// renders as 33.3%, not 33.33333%.
function computeProviderCoverage(
  instrumented: number,
  instance: number,
): number {
  if (instance <= 0) return 0;
  const pct = (instrumented / instance) * 100;
  return Math.round(pct * 10) / 10;
}

// --- Recent recommendations table -------------------------------------

function RecentRecommendations({ rows }: { rows: RecentRecommendation[] }) {
  if (rows.length === 0) {
    return (
      <div
        className="rounded-md border border-dashed p-6 text-sm text-muted-foreground"
        data-testid="recommendations-empty"
      >
        No recommendations yet. Run a scan from a connection wizard to draft
        your first.
      </div>
    );
  }
  return (
    <div
      className="overflow-x-auto rounded-md border"
      data-testid="recommendations-table"
    >
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="text-left">
            <th className="px-3 py-2 font-medium">Provider</th>
            <th className="px-3 py-2 font-medium">Kind</th>
            <th className="px-3 py-2 font-medium">Resource</th>
            <th className="px-3 py-2 font-medium">Scope</th>
            <th className="px-3 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">Generated</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <RecommendationRow key={`${r.provider}-${i}`} row={r} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RecommendationRow({ row }: { row: RecentRecommendation }) {
  const path = PROVIDER_PATH[row.provider];
  const generatedDate = parseDate(row.generated_at);
  return (
    <tr
      className="border-t hover:bg-muted/30"
      data-testid={`rec-row-${row.provider}`}
    >
      <td className="px-3 py-2">
        <Link to={path} className="inline-block">
          <Badge
            variant="outline"
            className={PROVIDER_BADGE[row.provider]}
            data-testid={`rec-badge-${row.provider}`}
          >
            {PROVIDER_LABEL[row.provider]}
          </Badge>
        </Link>
      </td>
      <td className="px-3 py-2 text-xs">{row.kind}</td>
      <td className="px-3 py-2 font-mono text-xs">{row.resource_id || "-"}</td>
      <td className="px-3 py-2 font-mono text-xs">{row.scope_id}</td>
      <td className="px-3 py-2 text-xs">{row.region}</td>
      <td className="px-3 py-2 text-xs text-muted-foreground">
        {generatedDate ? relativeTime(generatedDate) : row.generated_at}
      </td>
    </tr>
  );
}

// --- Helpers ----------------------------------------------------------

// parseDate turns the wire ISO-8601 string into a Date, returning
// null on parse failure so the table renders the raw string rather
// than NaN-ago when the server hands back an unexpected value.
function parseDate(s: string): Date | null {
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return null;
  return d;
}

// relativeTime formats a Date as a coarse human string ("5s ago",
// "2m ago", "3h ago", "2d ago"). The dashboard uses this for both
// the header's Refreshed timestamp and the recommendations table's
// Generated column. Slice 1 avoids date-fns to keep the bundle
// lean; the rendering granularity matches what an operator scanning
// a triage queue wants — coarser than seconds-since, finer than
// "today / yesterday".
function relativeTime(d: Date): string {
  const diffMs = Date.now() - d.getTime();
  const sec = Math.max(0, Math.floor(diffMs / 1000));
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  return `${day}d ago`;
}
