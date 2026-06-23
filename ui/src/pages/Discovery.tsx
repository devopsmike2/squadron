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

import { AlertTriangle, Cloud, RefreshCw } from "lucide-react";
import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import useSWR from "swr";

import {
  fetchSpanQuality,
  type ProviderSpanQuality,
  type SpanQualityResponse,
} from "@/api/discoverySpanQuality";
import {
  getDiscoverySummary,
  type DiscoverySummary,
  type ProviderSummary,
  type RecentRecommendation,
} from "@/api/discoverySummary";
import {
  getTraceCoverage,
  type ProviderTraceCoverage,
  type TraceCoverage,
} from "@/api/discoveryTraceCoverage";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

// The SWR cache key the page reads. Lives at /discovery/summary to
// match the wire endpoint — a future cross-page integration (the
// command palette, say) can read the same cache without re-fetching.
const SWR_KEY_SUMMARY = "/discovery/summary";

// SWR cache key for the v0.89.76 #707 trace coverage endpoint. Pairs
// 1:1 with /discovery/trace_coverage so the same cache key works for
// cross-page consumers too.
const SWR_KEY_TRACE_COVERAGE = "/discovery/trace_coverage";

// SWR cache key for the v0.89.87 #718 span quality endpoint (span
// quality slice 1 chunk 3). Pairs 1:1 with /discovery/span_quality so
// the same cache key works for cross-page consumers too.
const SWR_KEY_SPAN_QUALITY = "/discovery/span_quality";

// Filter-chip kinds the dashboard panel deep-links into. Three
// recommendation kinds match 1:1 with the three columns of the
// SPAN QUALITY panel. When the operator clicks a column, the
// dashboard navigates to /discovery/aws#recommendations and seeds
// the filter chip with the matching kind via a URL hash fragment
// (the per-provider Recommendations tab reads it on mount). Mirrors
// the v0.89.82 trace-emission sub-indicator deeplink pattern — the
// AWS page is the only provider whose Recommendations tab renders
// span-quality drafts in this slice; the other three carry stub
// tabs (matching the slice-2-chunk-3 disposition).
const SPAN_QUALITY_KIND_ORPHAN = "span-quality-orphan-trace";
const SPAN_QUALITY_KIND_MISSING_ATTRS = "span-quality-missing-resource-attrs";
const SPAN_QUALITY_KIND_MISMATCH = "span-quality-attribute-mismatch";

// Threshold above which a provider's weak_match_pct triggers a caveat
// icon next to its chip per design doc §6. 20% is the slice-1 floor —
// a provider where >20% of matches keyed by host.name or service.name
// alone is noisy enough to flag.
const WEAK_MATCH_CAVEAT_THRESHOLD_PCT = 20;

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

  // Trace coverage rides on the same poll cadence as the summary —
  // refresh both together so the dashboard's two coverage numbers
  // (primitive instrumentation vs trace emission) describe the same
  // moment in time. A failure here does NOT block the summary card
  // render; the trace coverage panel renders an error state in-place
  // while the rest of the dashboard stays useful.
  const {
    data: traceData,
    error: traceError,
    mutate: traceMutate,
  } = useSWR<TraceCoverage>(SWR_KEY_TRACE_COVERAGE, getTraceCoverage, {
    revalidateOnFocus: false,
    revalidateOnReconnect: false,
  });

  // Span quality rides the same cadence (v0.89.87 chunk 3). The
  // SPAN QUALITY panel hides itself when all three totals are zero
  // (design doc §10 acceptance test 12) so a failed fetch surfaces
  // silently rather than crowding the dashboard with an error
  // banner — the operator's primary signal is the panel either
  // appearing or not. The non-fatal posture matches trace coverage's
  // panel-level failure handling.
  const { data: spanQuality, mutate: spanQualityMutate } =
    useSWR<SpanQualityResponse>(SWR_KEY_SPAN_QUALITY, fetchSpanQuality, {
      revalidateOnFocus: false,
      revalidateOnReconnect: false,
      // Silence the SWR error log path; the panel hides itself on
      // missing data, which is the right UX for a slice-1 backend
      // endpoint that may not yet exist on older deployments.
      shouldRetryOnError: false,
    });

  const handleRefresh = useCallback(() => {
    void mutate();
    void traceMutate();
    void spanQualityMutate();
  }, [mutate, traceMutate, spanQualityMutate]);

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

      {traceError && !traceData && (
        <div
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
          data-testid="trace-coverage-error"
        >
          Failed to load trace coverage:{" "}
          {traceError instanceof Error
            ? traceError.message
            : String(traceError)}
        </div>
      )}

      {data && (
        <DashboardBody
          summary={data}
          traceCoverage={traceData}
          spanQuality={spanQuality}
        />
      )}
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

function DashboardBody({
  summary,
  traceCoverage,
  spanQuality,
}: {
  summary: DiscoverySummary;
  traceCoverage: TraceCoverage | undefined;
  spanQuality: SpanQualityResponse | undefined;
}) {
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
      {traceCoverage && <TraceCoveragePanel coverage={traceCoverage} />}
      {spanQuality && <SpanQualityPanel quality={spanQuality} />}
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
// threshold logic lives in one place (coverageColor). testId is
// overridable so the trace coverage panel's ring is distinguishable
// from the primary instrumentation coverage ring (v0.89.76 panel).
function CoverageRing({
  pct,
  color,
  testId = "coverage-ring",
}: {
  pct: number;
  color: string;
  testId?: string;
}) {
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
      data-testid={testId}
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

// --- Trace coverage panel ---------------------------------------------
//
// TraceCoveragePanel — v0.89.76 (#707 Stream 105, Trace integration
// slice 1 chunk 3). Renders the second-axis coverage view: of the
// resources the discovery scanner has inventoried, how many have
// actually emitted spans recently? The panel sits BELOW the existing
// CoveragePanel because the two values describe distinct gaps —
// "primitive enabled" vs "telemetry actually flowing" (design doc §1).
//
// Empty state: when every provider reports inventory_count=0, the
// panel renders an actionable hint inside its own card rather than
// disappearing. Operators landing on a freshly-installed Squadron
// see "Run a discovery scan to populate the trace coverage view."
// inside the panel — the panel itself stays visible so the operator
// can tell the feature exists.

function TraceCoveragePanel({ coverage }: { coverage: TraceCoverage }) {
  const totals = coverage.totals;
  const isEmpty = totals.inventory_count === 0;

  // v0.89.82 (#713 Stream 111, Trace integration slice 2 chunk 3) —
  // fleet-wide sum of "primitive_enabled but no recent emission" rows
  // across the four providers. Drives the conditional sub-indicator
  // below the chip row. Hidden when zero (design doc §10 acceptance
  // test 10). The Recommendations-tab deeplink points to AWS today
  // because AWS is the only provider whose RecommendationsTab renders
  // trace-emission-* drafts; the other three pages still have stub
  // tabs in this chunk.
  const totalPendingTraceEmission =
    coverage.providers.aws.pending_trace_emission_count +
    coverage.providers.gcp.pending_trace_emission_count +
    coverage.providers.azure.pending_trace_emission_count +
    coverage.providers.oci.pending_trace_emission_count;

  return (
    <div
      className="rounded-lg border bg-card p-6"
      data-testid="trace-coverage-panel"
    >
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h2 className="text-base font-semibold">Trace coverage</h2>
          <p className="text-xs text-muted-foreground">
            Is telemetry actually flowing?
          </p>
        </div>
      </div>

      {isEmpty ? (
        <div
          className="mt-4 rounded-md border border-dashed p-4 text-sm text-muted-foreground"
          data-testid="trace-coverage-empty"
        >
          Run a discovery scan to populate the trace coverage view.
        </div>
      ) : (
        <>
          <div className="mt-4 flex flex-wrap items-center gap-6">
            <CoverageRing
              pct={totals.coverage_pct}
              color={coverageColor(totals.coverage_pct)}
              testId="trace-coverage-ring"
            />
            <div>
              <div
                className="text-4xl font-semibold tabular-nums"
                data-testid="trace-coverage-pct"
              >
                {totals.coverage_pct.toFixed(1)}%
              </div>
              <p className="mt-1 text-sm text-muted-foreground">
                {totals.emitting_count} of {totals.inventory_count}{" "}
                inventoried resources have emitted spans in the last 24h
              </p>
            </div>
          </div>
          <ProviderChipRow providers={coverage.providers} />
          {totalPendingTraceEmission > 0 && (
            <div
              data-testid="trace-coverage-pending-indicator"
              className="mt-3 flex items-center gap-2 text-sm text-amber-600 dark:text-amber-400"
            >
              <AlertTriangle className="h-4 w-4" aria-hidden />
              <span>
                {totalPendingTraceEmission} resources have the primitive
                enabled but no recent emission —{" "}
                <Link
                  to="/discovery/aws#recommendations"
                  className="underline"
                >
                  see Recommendations on each provider
                </Link>{" "}
                for the drafts.
              </span>
            </div>
          )}
        </>
      )}
    </div>
  );
}

// ProviderChipRow renders the four per-provider chips beneath the
// headline ring. Each chip color-codes by the per-provider
// coverage_pct against the same threshold logic as the top ring.
// A weak_match_pct above the slice-1 floor surfaces a caveat icon
// next to the chip — the operator hovers it for the matching
// explanation per design doc §3 (confidence indicator UX).
function ProviderChipRow({
  providers,
}: {
  providers: TraceCoverage["providers"];
}) {
  return (
    <div
      className="mt-4 flex flex-wrap gap-2"
      data-testid="trace-coverage-chip-row"
    >
      {PROVIDER_ORDER.map((p) => (
        <TraceCoverageChip key={p} provider={p} coverage={providers[p]} />
      ))}
    </div>
  );
}

function TraceCoverageChip({
  provider,
  coverage,
}: {
  provider: keyof TraceCoverage["providers"];
  coverage: ProviderTraceCoverage;
}) {
  const label = PROVIDER_LABEL[provider];
  const pct = coverage.coverage_pct;
  const color = coverageColor(pct);
  const showCaveat = coverage.weak_match_pct > WEAK_MATCH_CAVEAT_THRESHOLD_PCT;
  return (
    <span
      className="inline-flex items-center gap-1 rounded-md border px-2 py-1 text-xs tabular-nums"
      style={{ borderColor: color, color }}
      data-testid={`trace-coverage-chip-${provider}`}
      data-color={color}
    >
      <span className="font-medium">{label.toLowerCase()}</span>
      <span aria-label={`${label} trace coverage`}>{pct.toFixed(0)}%</span>
      {showCaveat && (
        <span
          title={`${coverage.weak_match_pct.toFixed(0)}% of matches are best-effort (host.name or service.name). Consider deploying OTel SDKs with full host detector enabled.`}
          data-testid={`trace-coverage-caveat-${provider}`}
          aria-label={`${label} weak match caveat`}
        >
          <AlertTriangle className="h-3 w-3" aria-hidden />
        </span>
      )}
    </span>
  );
}

// --- Span quality panel ----------------------------------------------
//
// SpanQualityPanel — v0.89.87 (#718 Stream 116, Span quality slice 1
// chunk 3). Renders the third-axis health view below the
// TraceCoveragePanel — the visual hierarchy reads top-down as
// "primitive enabled" → "telemetry flowing" → "telemetry healthy"
// (design doc §1, §7.1).
//
// The panel hides entirely when all three totals percentages are
// zero (design doc §10 acceptance test 12). A clean fleet has
// nothing to surface here. Each column deep-links to the AWS
// Recommendations tab with a kind hash fragment so the slice-2
// chunk-3 filter chip lands pre-applied (matches the v0.89.82
// trace-coverage pending-sub-indicator deeplink pattern).

function SpanQualityPanel({ quality }: { quality: SpanQualityResponse }) {
  const t = quality.totals;
  // §10 test 12 — hide entirely when all three percentages are zero.
  // The check is on the cross-provider totals; a per-provider non-zero
  // still surfaces (rolled up into totals via raw span counts on the
  // server side), but a fleet-wide all-zero hides the panel.
  if (
    t.orphan_pct === 0 &&
    t.missing_attr_pct === 0 &&
    t.attr_mismatch_pct === 0
  ) {
    return null;
  }
  return (
    <div
      className="rounded-lg border bg-card p-6"
      data-testid="span-quality-panel"
    >
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h2 className="text-base font-semibold">Span quality</h2>
          <p className="text-xs text-muted-foreground">
            Are the spans Squadron receives healthy?
          </p>
        </div>
      </div>
      <div className="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-3">
        <SpanQualityColumn
          label="Orphan trace"
          pct={t.orphan_pct}
          count={countResourcesWith(
            quality.providers,
            (p) => p.orphan_pct > 0,
          )}
          kind={SPAN_QUALITY_KIND_ORPHAN}
          testIdSuffix="orphan"
        />
        <SpanQualityColumn
          label="Missing attrs"
          pct={t.missing_attr_pct}
          count={countResourcesWith(
            quality.providers,
            (p) => p.missing_attr_pct > 0,
          )}
          kind={SPAN_QUALITY_KIND_MISSING_ATTRS}
          testIdSuffix="missing-attrs"
        />
        <SpanQualityColumn
          label="Attribute mismatch"
          pct={t.attr_mismatch_pct}
          count={countResourcesWith(
            quality.providers,
            (p) => p.attr_mismatch_pct > 0,
          )}
          kind={SPAN_QUALITY_KIND_MISMATCH}
          testIdSuffix="mismatch"
        />
      </div>
    </div>
  );
}

// SpanQualityColumn is the single-column tile. The whole tile is a
// Link so the operator can click anywhere on it (design doc §7.1).
// The hash carries the kind so the chunk-4 filter chip can pre-apply
// it on mount; until that lands the hash is a graceful no-op.
function SpanQualityColumn({
  label,
  pct,
  count,
  kind,
  testIdSuffix,
}: {
  label: string;
  pct: number;
  count: number;
  kind: string;
  testIdSuffix: string;
}) {
  const resourceWord = count === 1 ? "resource" : "resources";
  return (
    <Link
      to={`/discovery/aws#recommendations:${kind}`}
      className="block rounded-md border border-transparent p-3 text-center transition hover:border-amber-400/40 hover:bg-amber-500/5"
      data-testid={`span-quality-column-${testIdSuffix}`}
      data-kind={kind}
      aria-label={`${label} — ${pct.toFixed(1)}%, ${count} ${resourceWord}`}
    >
      <div className="text-xs uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div
        className="mt-1 text-2xl font-medium tabular-nums text-amber-600 dark:text-amber-300"
        data-testid={`span-quality-pct-${testIdSuffix}`}
      >
        {pct.toFixed(1)}%
      </div>
      <div
        className="mt-1 text-xs text-muted-foreground"
        data-testid={`span-quality-count-${testIdSuffix}`}
      >
        {count} {resourceWord}
      </div>
    </Link>
  );
}

// countResourcesWith approximates the per-kind "N resources" count
// shown below each column. Slice 1's backend exposes only a fleet-
// wide resources_with_issues per provider; per-kind counts are a
// slice-2 candidate. We sum resources_with_issues across providers
// reporting a non-zero percentage for the predicate — a slight
// over-count when a provider has two pathology classes triggering
// at once, but matches the design doc §7.1 mock granularity.
function countResourcesWith(
  providers: SpanQualityResponse["providers"],
  predicate: (p: ProviderSpanQuality) => boolean,
): number {
  let total = 0;
  for (const p of PROVIDER_ORDER) {
    const row = providers[p];
    if (!row) continue;
    if (predicate(row)) {
      total += row.resources_with_issues;
    }
  }
  return total;
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
