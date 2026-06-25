// DiscoveryAWS — v0.85 Stream 2E first user-facing payoff for the
// universal-observation arc. Lands the Account / Inventory /
// Recommendations triptych the design doc's "Recommendation surface"
// section calls out.
//
// Slice 1 honesty:
//   - Account tab is fully wired: list connections + open the wizard
//     dialog to connect a new one.
//   - Inventory tab is on-demand only: scans are not persisted, the
//     operator re-scans to see fresh data. The note under the picker
//     calls this out so an SRE doesn't expect a scan history.
//   - Recommendations tab is a placeholder for the proposer's
//     observation mode (lands in the next slice).
//
// The page reuses the v0.84 ProposerPlayground's result-panel idiom:
// a summary header, then collapsible sections per resource category,
// with badges for OTel detection. Same UX shape the operator already
// recognizes from Squadron's AI surface.

import {
  AlertTriangle,
  Ban,
  Check,
  ChevronDown,
  ChevronRight,
  Cloud,
  Copy,
  ExternalLink,
  GitPullRequest,
  Layers,
  Loader2,
  Play,
  RotateCcw,
  Sparkles,
  X,
} from "lucide-react";
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactElement,
} from "react";
import { Link, useSearchParams } from "react-router-dom";
import useSWR from "swr";

import {
  generateAWSRecommendations,
  listAWSConnections,
  listExcludedRecommendations,
  runAWSScan,
  saveAWSConnection,
  scanAllAWS,
  setRecommendationExclusion,
  validateAWSConnection,
  type AWSScanAllResponse,
  type CloudConnection,
  type GenerateRecommendationsResponse,
  type RowSpanQuality,
  type ScanResult,
  type ServerlessRow,
  type OrchestrationRow,
  type EventSourceRow,
} from "@/api/discovery";
import {
  IaCGitHubOpenPRError,
  listIaCGitHubConnections,
  openIaCGitHubPullRequest,
  type IaCGitHubConnection,
  type IaCGitHubOpenPRResponse,
} from "@/api/iacGithub";
import type { Recommendation } from "@/api/recommendations";
import { ConnectorWizard } from "@/components/discovery/ConnectorWizard";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { awsWizard } from "@/data/awsWizard";
import { relativeTime } from "@/lib/relativeTime";

// ACCOUNT_TAB / INVENTORY_TAB / RECS_TAB — string literals used both as
// Radix Tabs values and as a stable key for tests to query by role.
const ACCOUNT_TAB = "account";
const INVENTORY_TAB = "inventory";
const RECS_TAB = "recommendations";

// v0.89.7b (#619 Stream 23) — query-param key + sentinel value for the
// account selector. URL is the source of truth so refresh + deep-link
// land the operator in the same view they bookmarked. Component state
// mirrors the URL.
//
// The aggregate sentinel "all" is a string literal rather than the
// absence of the param because "missing param" and "explicit all" are
// the same operator intent. Using a sentinel lets the AccountSelector
// be a controlled component without a tri-state default.
const ACCOUNT_QUERY_PARAM = "account";
const ACCOUNT_ALL = "all";

// shortenAccountID renders the per-account badge label. AWS account
// IDs are 12 digits and uninformative in full; the last 4 are enough
// to distinguish accounts in a one-glance scan of a recommendation
// list. Falls back to the full id if it's shorter than the cutoff
// (defensive — should never happen in production).
function shortenAccountID(accountID: string): string {
  if (accountID.length <= 4) return accountID;
  return accountID.slice(-4);
}

// IAC_GITHUB_CONNECTIONS_SWR_KEY — shared with DiscoveryIaCGitHub.tsx
// so the Recommendations tab and the IaC connections page sip from
// the same SWR cache. Defined as a string literal here (rather than
// imported from the page file) so a circular import doesn't slip in
// when the page file is later wrapped in its own lazy import.
const IAC_GITHUB_CONNECTIONS_SWR_KEY = "/iac/github/connections";

// formatTime renders an ISO timestamp as a relative ("3m ago") string
// for recent values and falls back to a locale date for older ones.
// Inlined to avoid a date-fns dependency on a single-use page —
// mirrors the helper Plan.tsx ships.
function formatTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const secs = Math.floor((Date.now() - t) / 1000);
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return new Date(iso).toLocaleDateString();
}

export default function DiscoveryAWSPage() {
  // Tabs are controlled at the page level so the Inventory tab's
  // "Generate recommendations" button can hop the operator straight
  // into the Recommendations tab after the proposer responds. Without
  // a controlled value the auto-switch would require ref-driven
  // imperative trigger-clicks, which Radix Tabs handles cleanly but
  // is harder to test.
  const [activeTab, setActiveTab] = useState<string>(ACCOUNT_TAB);

  // Recommendations live at the page level too — Inventory writes
  // them, Recommendations reads them. State is per-session: refreshing
  // the page clears the panel, matching the slice-1 posture that
  // recommendations themselves aren't persisted.
  const [recs, setRecs] = useState<GenerateRecommendationsResponse | null>(
    null,
  );
  // v0.89.3 #603 Stream 19 Phase 4: account_id + scan_id of the scan
  // the proposer just generated against. Threaded down to the Open-PR
  // button so the per-card POST carries the right (scan_id, step_idx,
  // account_id) tuple. Both are reset every time Inventory hands us
  // a new recommendations response.
  const [recsAccountID, setRecsAccountID] = useState<string>("");
  const [recsScanID, setRecsScanID] = useState<string>("");
  // v0.89.38 #658 Stream 56 (#531 slice 2 chunk 5) — region of the
  // scan the proposer just generated against. Threaded down to the
  // per-card exclude affordance so the POST carries the right scope
  // tuple. Single-region per chunk-5 v1 — multi-region scans use the
  // first region in the result's list. Empty until Inventory hands us
  // a recommendations response.
  const [recsRegion, setRecsRegion] = useState<string>("");

  // v0.89.7b (#619 Stream 23) — last scan-all result kept at page
  // level so the Inventory tab's aggregate summary card and the
  // top-of-page scan-all status grid sip from the same source. Reset
  // is per-session; the operator re-runs Scan all to refresh.
  const [scanAllResult, setScanAllResult] = useState<AWSScanAllResponse | null>(
    null,
  );
  const [scanAllInFlight, setScanAllInFlight] = useState(false);
  const [scanAllError, setScanAllError] = useState<string | null>(null);

  // v0.89.7b (#619 Stream 23) — account selector reads/writes the URL.
  // The selector lives at the top of the page so it's visible
  // regardless of which tab is active; switching accounts in the
  // middle of an Inventory scan flow swaps the InventoryTab's mounted
  // child via the `key` prop below, which resets the per-account
  // scan state cleanly.
  const [searchParams, setSearchParams] = useSearchParams();
  const accountParam = searchParams.get(ACCOUNT_QUERY_PARAM) ?? ACCOUNT_ALL;
  const isAggregateView = accountParam === ACCOUNT_ALL;
  const selectedAccountID = isAggregateView ? "" : accountParam;

  const onAccountChange = useCallback(
    (next: string) => {
      const params = new URLSearchParams(searchParams);
      if (next === ACCOUNT_ALL) {
        params.delete(ACCOUNT_QUERY_PARAM);
      } else {
        params.set(ACCOUNT_QUERY_PARAM, next);
      }
      setSearchParams(params, { replace: false });
    },
    [searchParams, setSearchParams],
  );

  // Connections drive the selector + the scan-all per-account grid.
  // Fetched at page level so the selector and the scan-all status
  // grid share the same SWR cache as the Account tab.
  const { data: connData } = useSWR("/discovery/aws/connections", () =>
    listAWSConnections(),
  );
  const connections = connData?.connections ?? [];

  const onScanAll = useCallback(async () => {
    if (scanAllInFlight) return;
    setScanAllInFlight(true);
    setScanAllError(null);
    // Clear the previous result so the in-flight grid shows
    // "scanning…" for every connection, not stale per-account ticks.
    setScanAllResult(null);
    try {
      const r = await scanAllAWS({});
      setScanAllResult(r);
    } catch (e) {
      setScanAllError(e instanceof Error ? e.message : String(e));
    } finally {
      setScanAllInFlight(false);
    }
  }, [scanAllInFlight]);

  return (
    <div className="space-y-4 p-6">
      <header>
        <div className="flex items-center gap-2">
          <Cloud className="h-5 w-5 text-violet-500" />
          <h1 className="text-2xl font-semibold">AWS Discovery</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Connect AWS accounts and discover what&apos;s uninstrumented.
        </p>
      </header>

      <AccountSelectorBar
        connections={connections}
        selectedAccountID={accountParam}
        onAccountChange={onAccountChange}
        isAggregateView={isAggregateView}
        scanAllInFlight={scanAllInFlight}
        onScanAll={onScanAll}
      />

      {isAggregateView &&
        (scanAllInFlight || scanAllResult || scanAllError) && (
          <ScanAllStatusGrid
            connections={connections}
            inFlight={scanAllInFlight}
            result={scanAllResult}
            error={scanAllError}
            onPickAccount={onAccountChange}
          />
        )}

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList>
          <TabsTrigger value={ACCOUNT_TAB}>Wizard</TabsTrigger>
          <TabsTrigger value={INVENTORY_TAB}>Inventory</TabsTrigger>
          <TabsTrigger value={RECS_TAB}>Recommendations</TabsTrigger>
        </TabsList>

        <TabsContent value={ACCOUNT_TAB} className="mt-4">
          <AccountTab />
        </TabsContent>
        <TabsContent value={INVENTORY_TAB} className="mt-4">
          {isAggregateView ? (
            <AggregateInventorySummary
              result={scanAllResult}
              onPickAccount={onAccountChange}
            />
          ) : (
            <InventoryTab
              // Remount when the operator picks a different single
              // account — the InventoryTab owns per-account scan
              // state which would be stale across an account swap.
              key={selectedAccountID}
              initialAccountID={selectedAccountID}
              onRecommendations={(r, accountID, scanID, region) => {
                setRecs(r);
                setRecsAccountID(accountID);
                setRecsScanID(scanID);
                setRecsRegion(region);
                setActiveTab(RECS_TAB);
              }}
            />
          )}
        </TabsContent>
        <TabsContent value={RECS_TAB} className="mt-4">
          {isAggregateView ? (
            <AggregateRecommendationsNotice
              onPickAccount={onAccountChange}
              connections={connections}
            />
          ) : (
            <RecommendationsTab
              recs={recs}
              accountID={recsAccountID}
              scanID={recsScanID}
              region={recsRegion}
            />
          )}
        </TabsContent>
      </Tabs>
    </div>
  );
}

// AccountSelectorBar — the top-of-page picker + Scan-all CTA. v0.89.7b
// (#619 Stream 23). Renders a Select with "All accounts" + one option
// per connected account; selection drives the URL via the parent's
// onAccountChange. The Scan-all CTA only renders in aggregate view —
// a per-account "Run scan" CTA already lives in the Inventory tab and
// shadowing it at the page level would double the operator's choices
// without adding clarity.
function AccountSelectorBar({
  connections,
  selectedAccountID,
  onAccountChange,
  isAggregateView,
  scanAllInFlight,
  onScanAll,
}: {
  connections: CloudConnection[];
  selectedAccountID: string;
  onAccountChange: (next: string) => void;
  isAggregateView: boolean;
  scanAllInFlight: boolean;
  onScanAll: () => void;
}) {
  const hasConnections = connections.length > 0;
  return (
    <Card>
      <CardContent className="flex flex-col gap-3 p-4 md:flex-row md:items-center md:justify-between">
        <div className="flex flex-1 flex-col gap-2 md:flex-row md:items-center">
          <span className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
            View
          </span>
          <div className="md:w-72">
            <Select
              value={hasConnections ? selectedAccountID : ""}
              onValueChange={onAccountChange}
              disabled={!hasConnections}
            >
              <SelectTrigger
                aria-label="Account selector"
                disabled={!hasConnections}
              >
                <SelectValue
                  placeholder={
                    hasConnections
                      ? "All accounts"
                      : "Connect an AWS account first"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ACCOUNT_ALL}>All accounts</SelectItem>
                {connections.map((c) => (
                  // The page-level selector renders display_name + a
                  // separator + the short account id (last 4). The
                  // Inventory tab's per-account Select renders the
                  // full `display_name (account_id)` shape — two
                  // distinct labels mean test queries can target one
                  // or the other unambiguously.
                  <SelectItem key={c.account_id} value={c.account_id}>
                    {c.display_name} — …{shortenAccountID(c.account_id)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {!hasConnections && (
            <Link
              to="#"
              onClick={(e) => {
                // Anchor click is a no-op here — the Connect new
                // account CTA lives inside the Account tab's dialog
                // trigger. We keep the link as a hint pointing the
                // operator at the Account tab below.
                e.preventDefault();
              }}
              className="text-xs text-violet-500 hover:underline"
            >
              Connect an AWS account
            </Link>
          )}
        </div>
        {isAggregateView && hasConnections && (
          <Button
            onClick={onScanAll}
            disabled={scanAllInFlight}
            aria-label="Scan all accounts"
            className="gap-1"
          >
            {scanAllInFlight ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <Layers className="h-4 w-4" aria-hidden />
            )}
            {scanAllInFlight ? "Scanning all accounts…" : "Scan all accounts"}
          </Button>
        )}
      </CardContent>
    </Card>
  );
}

// ScanAllStatusGrid — the per-account in-flight + result grid that
// renders below the AccountSelectorBar in aggregate view. v0.89.7b
// (#619 Stream 23). While the scan-all request is in flight, every
// connection card shows "Scanning…". When the response lands all
// cards flip at once — succeeded rows render per-account counts,
// failed rows render the humanized error_code message. Forward-
// compatible with a future streaming endpoint that would feed
// per-account updates without a UI shape change.
function ScanAllStatusGrid({
  connections,
  inFlight,
  result,
  error,
  onPickAccount,
}: {
  connections: CloudConnection[];
  inFlight: boolean;
  result: AWSScanAllResponse | null;
  error: string | null;
  onPickAccount: (accountID: string) => void;
}) {
  // Build a quick lookup from account_id → succeeded / failed entry
  // so the per-connection card can pick the right shape without
  // re-walking the response arrays on every render.
  const lookup = useMemo(() => {
    const succeeded = new Map<
      string,
      AWSScanAllResponse["succeeded_accounts"][number]
    >();
    const failed = new Map<
      string,
      AWSScanAllResponse["failed_accounts"][number]
    >();
    if (result) {
      for (const s of result.succeeded_accounts) {
        succeeded.set(s.account_id, s);
      }
      for (const f of result.failed_accounts) {
        failed.set(f.account_id, f);
      }
    }
    return { succeeded, failed };
  }, [result]);

  return (
    <div className="space-y-3">
      {result && result.partial && (
        <Card role="status" className="border-yellow-500/50 bg-yellow-500/5">
          <CardContent className="p-3 text-sm">
            <span className="font-medium">Partial scan-all:</span>{" "}
            {result.failed_accounts.length} of {result.total_accounts} accounts
            failed. The per-account cards below show the humanized error; re-run
            after addressing each.
          </CardContent>
        </Card>
      )}
      {error && (
        <Card>
          <CardContent role="alert" className="p-3 text-sm text-destructive">
            Scan all failed: {error}
          </CardContent>
        </Card>
      )}
      {result && !result.partial && (
        <Card className="border-green-500/40 bg-green-500/5">
          <CardContent className="p-3 text-sm">
            <span className="font-medium">
              Scanned {result.total_accounts} account
              {result.total_accounts === 1 ? "" : "s"}:
            </span>{" "}
            {result.total_resources} resources discovered,{" "}
            {result.total_instrumented} instrumented,{" "}
            {result.total_uninstrumented} uninstrumented.
          </CardContent>
        </Card>
      )}
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
        {connections.map((c) => {
          const succ = lookup.succeeded.get(c.account_id);
          const fail = lookup.failed.get(c.account_id);
          return (
            <ScanAllAccountCard
              key={c.account_id}
              conn={c}
              inFlight={inFlight}
              succeeded={succ}
              failed={fail}
              onPickAccount={onPickAccount}
            />
          );
        })}
      </div>
    </div>
  );
}

function ScanAllAccountCard({
  conn,
  inFlight,
  succeeded,
  failed,
  onPickAccount,
}: {
  conn: CloudConnection;
  inFlight: boolean;
  succeeded?: AWSScanAllResponse["succeeded_accounts"][number];
  failed?: AWSScanAllResponse["failed_accounts"][number];
  onPickAccount: (accountID: string) => void;
}) {
  // Card status picks one of four states. In-flight wins over any
  // stale prior result so the operator's "scanning…" expectation
  // matches the visible state during a re-scan.
  const status: "scanning" | "succeeded" | "failed" | "idle" = inFlight
    ? "scanning"
    : failed
      ? "failed"
      : succeeded
        ? "succeeded"
        : "idle";

  return (
    <Card
      data-account-id={conn.account_id}
      data-status={status}
      className={
        status === "failed"
          ? "border-destructive/40 bg-destructive/5"
          : status === "succeeded"
            ? "border-green-500/40 bg-green-500/5"
            : undefined
      }
    >
      <CardHeader>
        <CardTitle className="text-sm">
          <button
            type="button"
            onClick={() => onPickAccount(conn.account_id)}
            className="text-left font-semibold hover:underline"
            aria-label={`View ${conn.display_name} only`}
          >
            {conn.display_name}
          </button>
        </CardTitle>
        <CardDescription className="text-xs">
          <code className="rounded bg-muted px-1 py-0.5 font-mono">
            {conn.account_id}
          </code>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-1 text-xs">
        {status === "scanning" && (
          <p className="flex items-center gap-1 text-muted-foreground">
            <Loader2 className="h-3 w-3 animate-spin" aria-hidden /> Scanning…
          </p>
        )}
        {status === "succeeded" && succeeded && (
          <>
            <p className="flex items-center gap-1 text-green-700 dark:text-green-400">
              <Check className="h-3 w-3" aria-hidden /> Scan succeeded
            </p>
            <p className="text-muted-foreground">
              {succeeded.resource_count} resources ·{" "}
              {succeeded.instrumented_count} instrumented ·{" "}
              {succeeded.uninstrumented_count} uninstrumented
            </p>
          </>
        )}
        {status === "failed" && failed && (
          <>
            <p className="flex items-center gap-1 text-destructive">
              <X className="h-3 w-3" aria-hidden /> {failed.error_code}
            </p>
            <p className="text-muted-foreground">{failed.humanized_message}</p>
          </>
        )}
        {status === "idle" && (
          <p className="text-muted-foreground">No recent scan-all result.</p>
        )}
      </CardContent>
    </Card>
  );
}

// AggregateInventorySummary — the slice-1 Inventory tab content for
// the aggregate view. v0.89.7b (#619 Stream 23) — the honest scope
// check from the task brief: full per-row aggregation across N
// accounts of inventory data is a follow-on, not slice 1. The summary
// card here shows the last scan-all aggregate counts; the operator
// switches to a single account for the per-row inventory detail they
// already know from the per-account view.
function AggregateInventorySummary({
  result,
  onPickAccount,
}: {
  result: AWSScanAllResponse | null;
  onPickAccount: (accountID: string) => void;
}) {
  if (!result) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center gap-2 p-8 text-center">
          <Layers className="h-8 w-8 text-muted-foreground" aria-hidden />
          <p className="text-sm font-medium">
            Run &quot;Scan all accounts&quot; to populate the aggregate summary.
          </p>
          <p className="max-w-md text-xs text-muted-foreground">
            Slice 1 surfaces aggregate counts only; switch to a single account
            to drill into per-resource inventory.
          </p>
        </CardContent>
      </Card>
    );
  }
  return (
    <div className="space-y-3">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Aggregate inventory</CardTitle>
          <CardDescription>
            Across {result.total_accounts} account
            {result.total_accounts === 1 ? "" : "s"} · scan_all_id{" "}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
              {result.scan_all_id}
            </code>
          </CardDescription>
        </CardHeader>
        <CardContent className="grid grid-cols-1 gap-3 md:grid-cols-3">
          <SummaryStat
            label="Resources"
            value={result.total_resources}
            tone="neutral"
          />
          <SummaryStat
            label="Instrumented"
            value={result.total_instrumented}
            tone="ok"
          />
          <SummaryStat
            label="Uninstrumented"
            value={result.total_uninstrumented}
            tone="warn"
          />
        </CardContent>
      </Card>
      <Card>
        <CardContent className="flex items-start gap-2 p-4 text-sm">
          <Sparkles className="mt-0.5 h-4 w-4 text-violet-500" aria-hidden />
          <div>
            <p className="font-medium">
              Switch to a single account to see per-resource inventory.
            </p>
            <p className="mt-1 text-xs text-muted-foreground">
              Aggregate per-row inventory rendering is a follow-on; slice 1
              ships the count summary above. Pick an account from the dropdown
              to see EC2 / Lambda / RDS / S3 / ALB / EKS / DynamoDB rows.
            </p>
            {result.succeeded_accounts.length > 0 && (
              <div className="mt-2 flex flex-wrap gap-1">
                {result.succeeded_accounts.map((s) => (
                  <Button
                    key={s.account_id}
                    variant="outline"
                    size="sm"
                    onClick={() => onPickAccount(s.account_id)}
                    className="h-7 text-xs"
                  >
                    <code className="font-mono">{s.account_id}</code>
                  </Button>
                ))}
              </div>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function SummaryStat({
  label,
  value,
  tone,
}: {
  label: string;
  value: number;
  tone: "ok" | "warn" | "neutral";
}) {
  const toneClass =
    tone === "ok"
      ? "text-green-700 dark:text-green-400"
      : tone === "warn"
        ? "text-yellow-700 dark:text-yellow-400"
        : "text-foreground";
  return (
    <div className="rounded-md border bg-muted/20 p-3">
      <p className="text-xs uppercase tracking-wider text-muted-foreground">
        {label}
      </p>
      <p className={`mt-1 text-2xl font-semibold ${toneClass}`}>{value}</p>
    </div>
  );
}

// AggregateRecommendationsNotice — slice-1 Recommendations tab content
// for aggregate view. v0.89.7b (#619 Stream 23) — the aggregate
// Recommendations endpoint doesn't exist yet (proposer is per-scan,
// and a multi-scan synthesis is its own follow-on). This panel tells
// the operator what slice 1 supports and offers one-click pickers to
// hop into a single-account view where Generate-recommendations
// works exactly as today.
function AggregateRecommendationsNotice({
  onPickAccount,
  connections,
}: {
  onPickAccount: (accountID: string) => void;
  connections: CloudConnection[];
}) {
  return (
    <Card>
      <CardContent className="flex flex-col items-start gap-3 p-6">
        <div className="flex items-center gap-2">
          <AlertTriangle
            className="h-5 w-5 text-yellow-600 dark:text-yellow-400"
            aria-hidden
          />
          <p className="text-base font-medium">
            Switch to a single account to see recommendations.
          </p>
        </div>
        <p className="text-sm text-muted-foreground">
          Slice 1 punts on cross-account recommendation aggregation —
          recommendations are still generated per-scan via the proposer. Pick an
          account below to see Inventory + Recommendations work exactly as in
          the single-account view.
        </p>
        {connections.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {connections.map((c) => (
              <Button
                key={c.account_id}
                variant="outline"
                size="sm"
                onClick={() => onPickAccount(c.account_id)}
                className="h-7 text-xs"
              >
                {c.display_name}{" "}
                <code className="ml-1 font-mono text-muted-foreground">
                  ({c.account_id})
                </code>
              </Button>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// --- Account tab ---------------------------------------------------

function AccountTab() {
  const { data, error, isLoading, mutate } = useSWR(
    "/discovery/aws/connections",
    () => listAWSConnections(),
  );
  // resumeMode threads through to the wizard. The "Resume an existing
  // connection" toggle sets this true; the wizard then renders an
  // "Existing ExternalId (optional)" field on step 1 (#622, fix for
  // #621) so an operator recovering a prior deployment can paste their
  // old UUID before the wizard generates a fresh one.
  const [resumeMode, setResumeMode] = useState(false);

  const connections = data?.connections ?? [];

  const onWizardComplete = useCallback(() => {
    // Refresh the SWR cache so the new connection card lands without a
    // page reload, then reset resumeMode so the next connect lands on
    // the standard fresh-UUID path.
    void mutate();
    setResumeMode(false);
  }, [mutate]);

  return (
    <div className="space-y-4">
      {/* Inline connect wizard — matches the GCP / Azure / OCI flow:
          the wizard lives in the Wizard tab, not behind a modal. */}
      <div className="space-y-3">
        <div className="flex items-start justify-between">
          <div>
            <h2 className="text-base font-semibold">Connect an AWS account</h2>
            <p className="text-xs text-muted-foreground">
              Grant Squadron read-only access via IAM assume-role.
            </p>
          </div>
          {/* Resume entry point (#578 / #621 / #622): an operator
              recovering from a previous deployment can toggle this to
              paste an existing ExternalId before the wizard generates a
              fresh UUID. */}
          <Button
            variant="ghost"
            size="sm"
            className="h-auto p-0 text-xs text-muted-foreground hover:bg-transparent hover:text-foreground"
            onClick={() => setResumeMode((v) => !v)}
          >
            {resumeMode
              ? "Use a fresh ExternalId instead"
              : connections.length > 0
                ? "Resume an existing connection"
                : "Already have an ExternalId? Resume an existing connection"}
          </Button>
        </div>
        <ConnectorWizard
          // Remount on resume toggle so step 1 restarts with the right
          // field set (and a fresh ExternalId on the standard path).
          key={resumeMode ? "resume" : "fresh"}
          wizard={awsWizard}
          onValidate={(req) => validateAWSConnection(req)}
          onSave={(req) =>
            saveAWSConnection(req).then((r) => ({
              connection_id: r.connection_id,
            }))
          }
          onComplete={onWizardComplete}
          resumeMode={resumeMode}
        />
      </div>

      <div>
        <h2 className="text-base font-semibold">Connected accounts</h2>
        <p className="text-xs text-muted-foreground">
          One row per AWS account Squadron is configured to scan.
        </p>
      </div>

      {error && (
        <Card>
          <CardContent className="p-4 text-sm text-destructive">
            Could not load connections: {String(error)}
          </CardContent>
        </Card>
      )}

      {isLoading && (
        <div className="space-y-2">
          <Skeleton className="h-20 w-full" />
          <Skeleton className="h-20 w-full" />
        </div>
      )}

      {!isLoading && connections.length === 0 && <AccountEmptyState />}

      {!isLoading && connections.length > 0 && (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          {connections.map((c) => (
            <ConnectionCard key={c.account_id} conn={c} />
          ))}
        </div>
      )}
    </div>
  );
}

function ConnectionCard({ conn }: { conn: CloudConnection }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{conn.display_name}</CardTitle>
        <CardDescription>
          AWS account{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
            {conn.account_id}
          </code>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="flex flex-wrap gap-1">
          {conn.regions.map((r) => (
            <Badge key={r} variant="secondary">
              {r}
            </Badge>
          ))}
        </div>
        <p className="text-xs text-muted-foreground">
          Connected {formatTime(conn.created_at)}
        </p>
      </CardContent>
    </Card>
  );
}

function AccountEmptyState() {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 p-8 text-center">
        <Cloud className="h-10 w-10 text-muted-foreground" aria-hidden />
        <div>
          <h3 className="text-base font-semibold">
            No accounts connected yet.
          </h3>
          <p className="mt-1 text-sm text-muted-foreground">
            Use the wizard above to connect your first account.
          </p>
        </div>
        <div className="rounded-md border bg-muted/30 p-3 text-xs text-muted-foreground">
          <Sparkles
            className="mr-1 inline-block h-3 w-3 text-violet-500"
            aria-hidden
          />
          Squadron never holds your AWS write credentials — recommendations land
          as Terraform snippets for your existing pipeline.
        </div>
      </CardContent>
    </Card>
  );
}

// --- Inventory tab -------------------------------------------------

function InventoryTab({
  onRecommendations,
  initialAccountID,
}: {
  // Called when the proposer responds (declined or otherwise) so the
  // page can hop to the Recommendations tab and surface the result.
  // accountID and scanID are threaded alongside the response so the
  // Recommendations tab's Open-PR button can address its per-step
  // requests correctly. v0.89.3 #603 Stream 19 Phase 4.
  // v0.89.38 #658 Stream 56 — added `region` parameter so the
  // Recommendations tab's exclude affordance can address its POST.
  // Empty string when the scan ran against zero regions (degenerate
  // — the scanner enforces non-empty); first region of result.regions
  // otherwise, matching the chunk-5 v1 single-region posture.
  onRecommendations: (
    r: GenerateRecommendationsResponse,
    accountID: string,
    scanID: string,
    region: string,
  ) => void;
  // v0.89.7b (#619 Stream 23) — preselect the account from the
  // page-level URL state so the operator who landed via
  // ?account=<id> sees the Select pre-filled. Empty falls back to
  // the legacy "operator must pick" UX. The component is remounted
  // via the parent's key when this value changes, so we read it as
  // initial state rather than syncing it on every render.
  initialAccountID?: string;
}) {
  const { data: connData } = useSWR("/discovery/aws/connections", () =>
    listAWSConnections(),
  );
  const connections = connData?.connections ?? [];

  const [selected, setSelected] = useState<string>(initialAccountID ?? "");
  const [scanning, setScanning] = useState(false);
  const [result, setResult] = useState<ScanResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Recommendations-generation loading + error state. Kept local to the
  // Inventory tab — the recommendations themselves bubble up to the
  // parent via onRecommendations.
  const [generating, setGenerating] = useState(false);
  const [genError, setGenError] = useState<string | null>(null);

  const onRun = useCallback(async () => {
    if (!selected || scanning) return;
    setScanning(true);
    setError(null);
    setResult(null);
    // Clear any stale generate state from a previous scan — a new scan
    // invalidates the prior recommendations panel.
    setGenError(null);
    try {
      const r = await runAWSScan(selected);
      setResult(r);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setScanning(false);
    }
  }, [selected, scanning]);

  const onGenerate = useCallback(async () => {
    if (!result || generating) return;
    setGenerating(true);
    setGenError(null);
    try {
      const r = await generateAWSRecommendations(result.account_id, result);
      // v0.89.38 — first region of the scan covers the chunk-5 v1
      // single-region posture. Multi-region scans surface only the
      // first region for the exclude affordance; v2 may scope per
      // recommendation row if/when proposers emit per-region rows.
      const region = result.regions[0] ?? "";
      onRecommendations(r, result.account_id, result.scan_id, region);
    } catch (e) {
      setGenError(e instanceof Error ? e.message : String(e));
    } finally {
      setGenerating(false);
    }
  }, [result, generating, onRecommendations]);

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Run an inventory scan</CardTitle>
          <CardDescription>
            Pick a connected account and trigger an on-demand scan.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex flex-col gap-3 md:flex-row md:items-end">
            <div className="flex-1">
              <Select value={selected} onValueChange={setSelected}>
                <SelectTrigger
                  aria-label="Connected account"
                  disabled={connections.length === 0}
                >
                  <SelectValue
                    placeholder={
                      connections.length === 0
                        ? "Connect an account first"
                        : "Select an account"
                    }
                  />
                </SelectTrigger>
                <SelectContent>
                  {connections.map((c) => (
                    <SelectItem key={c.account_id} value={c.account_id}>
                      {c.display_name} ({c.account_id})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button
              onClick={onRun}
              disabled={!selected || scanning}
              className="gap-1"
            >
              {scanning ? (
                <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
              ) : (
                <Play className="h-4 w-4" aria-hidden />
              )}
              {scanning ? "Scanning…" : "Run scan"}
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            Slice 1 doesn&apos;t persist scan history; run a scan to see current
            state. Each scan emits audit events.
          </p>
        </CardContent>
      </Card>

      {error && (
        <Card>
          <CardContent className="p-4 text-sm text-destructive">
            Scan failed: {error}
          </CardContent>
        </Card>
      )}

      {scanning && !result && <ScanSkeleton />}

      {!scanning && !result && !error && <InventoryEmptyState />}

      {result && (
        <ScanResultPanel
          result={result}
          generating={generating}
          genError={genError}
          onGenerate={onGenerate}
        />
      )}
    </div>
  );
}

function ScanSkeleton() {
  return (
    <Card>
      <CardContent className="space-y-2 p-4">
        <Skeleton className="h-6 w-1/2" />
        <Skeleton className="h-32 w-full" />
      </CardContent>
    </Card>
  );
}

function InventoryEmptyState() {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-2 p-8 text-center">
        <p className="text-sm text-muted-foreground">
          Run a scan to see current inventory.
        </p>
      </CardContent>
    </Card>
  );
}

function ScanResultPanel({
  result,
  generating,
  genError,
  onGenerate,
}: {
  result: ScanResult;
  generating: boolean;
  genError: string | null;
  onGenerate: () => void;
}) {
  return (
    <div className="space-y-4">
      {/* Summary header — plus the Generate-recommendations CTA. We
          stack the button under the summary so a new operator scans the
          result top-to-bottom and lands naturally on the next action,
          rather than hunting for it after the Functions list. */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            Scan result for account{" "}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-sm">
              {result.account_id}
            </code>
          </CardTitle>
          <CardDescription>
            Regions {result.regions.join(", ") || "(none)"} · completed{" "}
            {formatTime(result.scan_completed_at)} · scanned{" "}
            {result.compute.length} compute instances, {result.functions.length}{" "}
            functions, {(result.databases ?? []).length} databases,{" "}
            {(result.object_stores ?? []).length} object stores,{" "}
            {(result.load_balancers ?? []).length} load balancers, and{" "}
            {(result.clusters ?? []).length} clusters.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-2">
          <div className="flex flex-col gap-2 md:flex-row md:items-center md:justify-between">
            <p className="text-xs text-muted-foreground">
              Ask the AI proposer to draft Terraform that instruments the
              uninstrumented resources. Snippets are for your IaC pipeline —
              Squadron does not apply them.
            </p>
            <Button
              onClick={onGenerate}
              disabled={generating}
              variant="default"
              className="gap-1"
            >
              {generating ? (
                <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
              ) : (
                <Sparkles className="h-4 w-4" aria-hidden />
              )}
              {generating
                ? "Generating recommendations…"
                : "Generate recommendations"}
            </Button>
          </div>
          {genError && (
            <p className="text-xs text-destructive">
              Recommendation generation failed: {genError}
            </p>
          )}
        </CardContent>
      </Card>

      {/* Partial warning */}
      {result.partial && (
        <Card className="border-yellow-500/50 bg-yellow-500/5">
          <CardContent className="p-4 text-sm">
            <span className="font-medium">Scan was partial:</span>{" "}
            {result.partial_reason ??
              "the walk did not cover the full inventory"}
            . Re-run to capture missed resources.
          </CardContent>
        </Card>
      )}

      {/* Compute section */}
      <ComputeSection compute={result.compute} />

      {/* Functions section */}
      <FunctionsSection functions={result.functions} />

      {/* Databases section (slice 2 — v0.87) */}
      <DatabasesSection databases={result.databases ?? []} />

      {/* Object stores + Load balancers sections (slice 3a — v0.88.0) */}
      <ObjectStoresSection objectStores={result.object_stores ?? []} />
      <LoadBalancersSection loadBalancers={result.load_balancers ?? []} />

      {/* Clusters section (slice 3b — v0.89.0) */}
      <ClustersSection clusters={result.clusters ?? []} />

      {/* Serverless section (serverless tier slice 1 chunk 5 —
          v0.89.92, #725 Stream 123). */}
      <ServerlessSection serverless={result.serverless ?? []} />

      {/* Orchestration section (orchestration tier slice 1 chunk 4
          — v0.89.97, #731 Stream 129). */}
      <OrchestrationSection orchestrations={result.orchestrations ?? []} />

      {/* Event sources section (event source tier slice 1 chunk 5
          — v0.89.102, #738 Stream 136). All 4 providers render this
          tab (including OCI, unlike Orchestration which hid OCI). */}
      <EventSourcesSection eventSources={result.event_sources ?? []} />
    </div>
  );
}

function ComputeSection({ compute }: { compute: ScanResult["compute"] }) {
  const [open, setOpen] = useState(compute.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Compute instances ({compute.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {compute.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No compute instances visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {compute.map((c) => (
                <li
                  key={c.resource_id}
                  className="rounded-md border bg-muted/20 p-3"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div>
                      <code className="font-mono text-sm">{c.resource_id}</code>
                      <p className="text-xs text-muted-foreground">
                        {c.instance_type} · {c.region}
                      </p>
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      <OtelBadge ok={c.has_otel} />
                      <LastSeenCell value={c.last_seen_at} />
                      <QualityDot quality={c.span_quality} />
                    </div>
                  </div>
                  <TagPills tags={c.tags} />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

function FunctionsSection({
  functions,
}: {
  functions: ScanResult["functions"];
}) {
  const [open, setOpen] = useState(functions.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Functions ({functions.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {functions.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No functions visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {functions.map((f) => (
                <li
                  key={f.resource_id}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-md border bg-muted/20 p-3"
                >
                  <div>
                    <p className="font-medium">{f.name}</p>
                    <p className="text-xs text-muted-foreground">
                      {f.runtime} · {f.region}
                    </p>
                  </div>
                  <div className="flex flex-wrap items-center gap-1">
                    <OtelBadge ok={f.has_otel_layer} />
                    <QualityDot quality={f.span_quality} />
                  </div>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// DatabasesSection renders the RDS row list. Slice 2 (v0.87) of the
// universal-observation arc. Two badge columns surface the two
// independent observability levers — Performance Insights and
// Enhanced Monitoring — so the operator can see at a glance which
// lever is missing on each row. The proposer prompt treats them as
// independent (separate plan steps per missing lever); the UI matches
// that framing.
function DatabasesSection({
  databases,
}: {
  databases: ScanResult["databases"];
}) {
  const [open, setOpen] = useState(databases.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Databases ({databases.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {databases.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No databases visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {databases.map((d) => (
                <li
                  key={d.resource_id}
                  className="rounded-md border bg-muted/20 p-3"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div>
                      <code className="font-mono text-sm">{d.resource_id}</code>
                      <p className="text-xs text-muted-foreground">
                        {d.engine} {d.engine_version} · {d.instance_class} ·{" "}
                        {d.region}
                      </p>
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      <LeverBadge
                        ok={d.performance_insights_enabled}
                        label="Performance Insights"
                      />
                      <LeverBadge
                        ok={d.enhanced_monitoring_enabled}
                        label="Enhanced Monitoring"
                      />
                      <LastSeenCell value={d.last_seen_at} />
                      <QualityDot quality={d.span_quality} />
                    </div>
                  </div>
                  <TagPills tags={d.tags} />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// LeverBadge is the on/off badge for a single observability lever
// (Performance Insights or Enhanced Monitoring). Reuses the same
// green/muted palette as OtelBadge so the operator's visual
// vocabulary stays consistent across the three Inventory sections.
function LeverBadge({ ok, label }: { ok: boolean; label: string }) {
  if (ok) {
    return (
      <Badge
        variant="outline"
        className="border-green-600/50 text-green-700 dark:text-green-400"
      >
        <Check className="h-3 w-3" aria-hidden />
        {label}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="text-muted-foreground">
      <X className="h-3 w-3" aria-hidden />
      {label}
    </Badge>
  );
}

// ObjectStoresSection renders the S3 bucket list. Slice 3a (v0.88.0).
// Server Access Logging is the single instrumented-rule axis;
// Request Metrics is rendered alongside as informational only —
// matching the proposer prompt's "Server Access Logging is the only
// lever" framing.
function ObjectStoresSection({
  objectStores,
}: {
  objectStores: ScanResult["object_stores"];
}) {
  const [open, setOpen] = useState(objectStores.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Object stores ({objectStores.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {objectStores.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No object stores visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {objectStores.map((o) => (
                <li
                  key={o.resource_id}
                  className="rounded-md border bg-muted/20 p-3"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div>
                      <code className="font-mono text-sm">{o.resource_id}</code>
                      <p className="text-xs text-muted-foreground">
                        {o.region}
                      </p>
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      <LeverBadge
                        ok={o.server_access_logging_enabled}
                        label="Server Access Logging"
                      />
                      <LeverBadge
                        ok={o.request_metrics_enabled}
                        label="Request Metrics"
                      />
                    </div>
                  </div>
                  <TagPills tags={o.tags} />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// LoadBalancersSection renders the ALB / NLB / GWLB row list. Slice
// 3a (v0.88.0). The Access Logs badge surfaces the configured target
// bucket inline so the operator can see the ALB→S3 cross-reference
// at a glance — matches the proposer prompt's "prefer an existing
// instrumented bucket as the target" rule.
function LoadBalancersSection({
  loadBalancers,
}: {
  loadBalancers: ScanResult["load_balancers"];
}) {
  const [open, setOpen] = useState(loadBalancers.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Load balancers ({loadBalancers.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {loadBalancers.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No load balancers visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {loadBalancers.map((l) => (
                <li
                  key={l.resource_id}
                  className="rounded-md border bg-muted/20 p-3"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div>
                      <p className="font-medium">{l.name}</p>
                      <p className="text-xs text-muted-foreground">
                        {l.type} · {l.scheme} · {l.region}
                      </p>
                      <code className="mt-1 block font-mono text-[10px] text-muted-foreground">
                        {l.resource_id}
                      </code>
                    </div>
                    <div className="flex flex-col items-end gap-1">
                      <LeverBadge
                        ok={l.access_logs_enabled}
                        label="Access Logs"
                      />
                      {l.access_logs_enabled && l.access_logs_s3_bucket && (
                        <span className="text-[10px] text-muted-foreground">
                          → {l.access_logs_s3_bucket}
                        </span>
                      )}
                    </div>
                  </div>
                  <TagPills tags={l.tags} />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// ClustersSection renders the EKS / GKE / AKS cluster list. Slice
// 3b (v0.89.0). The composite instrumented rule surfaces as two
// independent badge groups per row: Control Plane Logging (one
// badge per enabled log type, with api+audit highlighted as the
// minimum-required pair) and Add-ons (one badge per add-on with
// ADOT / cloudwatch-observability highlighted as the observability
// names). The k8s version + status render alongside as
// informational columns.
function ClustersSection({ clusters }: { clusters: ScanResult["clusters"] }) {
  const [open, setOpen] = useState(clusters.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Clusters ({clusters.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {clusters.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No clusters visible in the scanned regions.
            </p>
          ) : (
            <ul className="space-y-2">
              {clusters.map((c) => (
                <li
                  key={c.resource_id}
                  className="rounded-md border bg-muted/20 p-3"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div>
                      <p className="font-medium">{c.name}</p>
                      <p className="text-xs text-muted-foreground">
                        k8s {c.kubernetes_version} · {c.status} · {c.region}
                      </p>
                      <code className="mt-1 block font-mono text-[10px] text-muted-foreground">
                        {c.resource_id}
                      </code>
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      <LastSeenCell value={c.last_seen_at} />
                      <QualityDot quality={c.span_quality} />
                    </div>
                  </div>
                  <div className="mt-2 flex flex-col gap-1">
                    <div className="flex flex-wrap items-center gap-1">
                      <span className="text-[10px] uppercase text-muted-foreground">
                        Control Plane Logging:
                      </span>
                      {c.control_plane_logging.length === 0 ? (
                        <Badge
                          variant="outline"
                          className="text-muted-foreground"
                        >
                          none
                        </Badge>
                      ) : (
                        c.control_plane_logging.map((t) => {
                          const required = t === "api" || t === "audit";
                          return (
                            <Badge
                              key={t}
                              variant="outline"
                              className={
                                required
                                  ? "border-green-600/50 text-green-700 dark:text-green-400"
                                  : "text-muted-foreground"
                              }
                            >
                              {t}
                            </Badge>
                          );
                        })
                      )}
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      <span className="text-[10px] uppercase text-muted-foreground">
                        Add-ons:
                      </span>
                      {c.addons.length === 0 ? (
                        <Badge
                          variant="outline"
                          className="text-muted-foreground"
                        >
                          none
                        </Badge>
                      ) : (
                        c.addons.map((a) => {
                          const isObs =
                            a.name === "adot" ||
                            a.name === "amazon-cloudwatch-observability";
                          const isActive = a.status === "ACTIVE";
                          const highlight = isObs && isActive;
                          return (
                            <Badge
                              key={a.name}
                              variant="outline"
                              className={
                                highlight
                                  ? "border-green-600/50 text-green-700 dark:text-green-400"
                                  : isObs
                                    ? "border-yellow-500/50 text-yellow-700 dark:text-yellow-400"
                                    : "text-muted-foreground"
                              }
                              title={`${a.status}${a.version ? " · " + a.version : ""}`}
                            >
                              {a.name}
                            </Badge>
                          );
                        })
                      )}
                    </div>
                  </div>
                  <TagPills tags={c.tags} />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// ServerlessSection — serverless tier slice 1 chunk 5 (v0.89.92, #725
// Stream 123). Renders the per-row Lambda inventory the chunk 1 AWS
// Lambda scanner produced. The columns follow §7 of the design doc:
// Resource Name, Surface, Runtime, Region, Trace axis (X-Ray active?),
// OTel distro (ADOT layer / wrapper detected?), Last seen, and the
// span-quality dot from v0.89.87. The section collapses when empty
// (same pattern as the other AWS sections) so an account with no
// Lambda still gets a header but no row noise.
function ServerlessSection({ serverless }: { serverless: ServerlessRow[] }) {
  const [open, setOpen] = useState(serverless.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Serverless ({serverless.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {serverless.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No serverless functions visible in the scanned regions.
            </p>
          ) : (
            <div className="overflow-x-auto rounded-md border">
              <table className="w-full text-sm">
                <thead className="bg-muted/40">
                  <tr className="text-left">
                    <th className="px-3 py-2 font-medium">Resource Name</th>
                    <th className="px-3 py-2 font-medium">Surface</th>
                    <th className="px-3 py-2 font-medium">Runtime</th>
                    <th className="px-3 py-2 font-medium">Region</th>
                    <th className="px-3 py-2 font-medium">Trace axis</th>
                    <th className="px-3 py-2 font-medium">OTel distro</th>
                    {/* Cold-start latency analysis slice 1 chunk 3
                        (v0.89.115, #753 Stream 151) — new Cold-start
                        P95 (24h) column between OTel distro and Last
                        seen. Mirrored on the GCP / Azure / OCI
                        Serverless tables as "—" everywhere since
                        slice 1 ships AWS Lambda only. */}
                    <th className="px-3 py-2 font-medium">
                      Cold-start P95 (24h)
                    </th>
                    {/* Sampling rate analysis slice 1 chunk 3 (v0.89.124,
                        #764 Stream 162) — new "Sampling rate (24h)"
                        column between Cold-start P95 and Last seen.
                        Mirrored on the GCP / Azure / OCI Serverless
                        tables per the slice 1 contract — all 5
                        serverless surfaces participate. */}
                    <th className="px-3 py-2 font-medium">
                      Sampling rate (24h)
                    </th>
                    {/* Error rate correlation slice 1 chunk 3
                        (v0.89.129, #769 Stream 167) — new
                        "Error rate (24h)" column between Sampling
                        rate and Last seen. Mirrored on all 4
                        provider Serverless tables. */}
                    <th className="px-3 py-2 font-medium">Error rate (24h)</th>
                    <th className="px-3 py-2 font-medium">Last seen</th>
                    <th className="px-3 py-2 font-medium">Quality</th>
                  </tr>
                </thead>
                <tbody>
                  {serverless.map((s) => (
                    <tr
                      key={s.resource_arn || s.resource_name}
                      className="border-t"
                    >
                      <td className="px-3 py-2 font-mono text-xs">
                        {s.resource_name}
                      </td>
                      <td className="px-3 py-2 text-xs">{s.surface}</td>
                      <td className="px-3 py-2 text-xs">{s.runtime || "-"}</td>
                      <td className="px-3 py-2 text-xs">{s.region}</td>
                      <td className="px-3 py-2 text-xs">
                        <AxisCheck ok={s.has_trace_axis} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <AxisCheck ok={s.has_otel_distro} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <ColdStartCell row={s} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <SamplingRateCell row={s} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <ErrorRateCell row={s} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <LastSeenCell value={s.last_seen_at} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <QualityDot quality={s.span_quality} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// AxisCheck — serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream
// 123). A minimal check / cross indicator for the two per-row
// serverless observability axes (Trace axis, OTel distro). Kept
// alongside ServerlessSection so the visual vocabulary is co-located
// with the table that consumes it.
function AxisCheck({ ok }: { ok: boolean }) {
  if (ok) {
    return (
      <span
        className="text-emerald-600"
        title="enabled"
        aria-label="enabled"
        data-testid="axis-check"
        data-value="yes"
      >
        ✓
      </span>
    );
  }
  return (
    <span
      className="text-muted-foreground"
      title="disabled"
      aria-label="disabled"
      data-testid="axis-check"
      data-value="no"
    >
      ✗
    </span>
  );
}

// ColdStartCell — Cold-start latency analysis slice 1 chunk 3
// (v0.89.115, #753 Stream 151). Renders the per-Lambda 24h P95
// cold-start observation surfaced on ServerlessRow. Three render
// states:
//
//   - undefined / null cold_start_p95_ms: render "—" (no observation
//     persisted yet — Lambda younger than the first scan window, or
//     non-AWS surface in slice 1).
//   - cold_start_exceeds_threshold === true: render the value amber.
//     The hover tooltip names the baseline-vs-current ratio so the
//     operator can confirm without drilling into the per-resource
//     endpoint.
//   - cold_start_exceeds_threshold === false / undefined: render the
//     value at the default slate color.
//
// Slice 1 ships AWS Lambda only — the column shows "—" on GCP /
// Azure / OCI Serverless tables.
function ColdStartCell({ row }: { row: ServerlessRow }) {
  if (row.cold_start_p95_ms === undefined || row.cold_start_p95_ms === null) {
    return (
      <span
        className="text-muted-foreground"
        title="No cold-start observation yet"
        data-testid="cold-start-cell"
        data-value="none"
      >
        —
      </span>
    );
  }
  const isAmber = row.cold_start_exceeds_threshold === true;
  const ms = Math.round(row.cold_start_p95_ms);
  return (
    <span
      className={isAmber ? "text-amber-600" : "text-foreground"}
      title={
        isAmber
          ? `Cold-start P95 ${ms}ms exceeds baseline threshold (>= 1.5x baseline AND >= 500ms)`
          : `Cold-start P95 ${ms}ms`
      }
      data-testid="cold-start-cell"
      data-value={isAmber ? "amber" : "ok"}
    >
      {ms}ms
    </span>
  );
}

// SamplingRateCell — Sampling rate analysis slice 1 chunk 3
// (v0.89.124, #764 Stream 162). Renders the per-row 24h sampling ratio
// surfaced on ServerlessRow. Three render states matching the
// ColdStartCell pattern:
//
//   - undefined / null sampling_ratio: render "—" at the muted color.
//     Covers the "no observation persisted yet" case (resource too new,
//     scan didn't run, invocation count below the 1000 minimum, or
//     the per-cloud MetricQuerier substrate isn't wired).
//   - sampling_exceeds_floor === true: render the percentage amber.
//     Hover tooltip names the 5% floor + 1000-invocation minimum so
//     the operator can confirm without drilling into the per-resource
//     /sampling endpoint.
//   - sampling_exceeds_floor === false / undefined: render the
//     percentage at the default slate color.
//
// All 5 serverless surfaces participate per slice 1 contract — the
// component is exported so the GCP / Azure / OCI pages can share one
// implementation. Mirrors the AWS / GCP / Azure / OCI ColdStartCell
// duplication pattern, but as a single shared export to keep the
// chunk-3 line count down.
export function SamplingRateCell({ row }: { row: ServerlessRow }) {
  if (row.sampling_ratio === undefined || row.sampling_ratio === null) {
    return (
      <span
        className="text-muted-foreground"
        title="No sampling observation yet"
        data-testid="sampling-rate-cell"
        data-value="none"
      >
        —
      </span>
    );
  }
  const pct = row.sampling_ratio * 100;
  const isAmber = row.sampling_exceeds_floor === true;
  return (
    <span
      className={isAmber ? "text-amber-600" : "text-foreground"}
      title={
        isAmber
          ? `Sampling ratio ${pct.toFixed(1)}% — below 5% floor with >= 1000 invocations`
          : `Sampling ratio ${pct.toFixed(1)}%`
      }
      data-testid="sampling-rate-cell"
      data-value={isAmber ? "amber" : "ok"}
    >
      {pct.toFixed(1)}%
    </span>
  );
}

// ErrorRateCell — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Renders the per-row 24h error rate
// surfaced on ServerlessRow. Three render states matching the
// ColdStartCell / SamplingRateCell pattern:
//
//   - undefined / null current_error_rate: render "—" at the muted
//     color. Covers the "no observation persisted yet" case.
//   - error_rate_exceeds_threshold === true: render the percentage
//     amber. Hover tooltip names the 2.0x baseline + minimums so
//     the operator can confirm without drilling into the
//     per-resource /error_rate endpoint.
//   - error_rate_exceeds_threshold === false / undefined: render
//     the percentage at the default slate color.
//
// Exported so the GCP / Azure / OCI pages can share one
// implementation — same posture as SamplingRateCell.
export function ErrorRateCell({ row }: { row: ServerlessRow }) {
  if (row.current_error_rate === undefined || row.current_error_rate === null) {
    return (
      <span
        className="text-muted-foreground"
        title="No error-rate observation yet"
        data-testid="error-rate-cell"
        data-value="none"
      >
        —
      </span>
    );
  }
  const pct = row.current_error_rate * 100;
  const isAmber = row.error_rate_exceeds_threshold === true;
  return (
    <span
      className={isAmber ? "text-amber-600" : "text-foreground"}
      title={
        isAmber
          ? `Error rate ${pct.toFixed(2)}% — exceeds 2x baseline + minimums`
          : `Error rate ${pct.toFixed(2)}%`
      }
      data-testid="error-rate-cell"
      data-value={isAmber ? "amber" : "ok"}
    >
      {pct.toFixed(2)}%
    </span>
  );
}

// OrchestrationSection — orchestration tier slice 1 chunk 4 (v0.89.97,
// #731 Stream 129). Renders the per-row AWS Step Functions inventory
// the chunk 1 scanner produced. Columns per §7 of the design doc:
// Resource Name, Surface, Type (workflow_type — STANDARD/EXPRESS),
// Region, Trace axis (X-Ray active?), Log axis (CloudWatch Logs?),
// Last seen, Quality. AWS gets the Quality column per the slice 1
// constraint mirroring v0.89.92 — GCP / Azure parity for QualityDot
// is a slice 2 candidate. The section collapses when empty so an
// account with no Step Functions still gets a header but no row noise.
function OrchestrationSection({
  orchestrations,
}: {
  orchestrations: OrchestrationRow[];
}) {
  const [open, setOpen] = useState(orchestrations.length > 0);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
        >
          <CardTitle className="text-base">
            Orchestration ({orchestrations.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {orchestrations.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No orchestration workflows visible in the scanned regions.
            </p>
          ) : (
            <div className="overflow-x-auto rounded-md border">
              <table className="w-full text-sm">
                <thead className="bg-muted/40">
                  <tr className="text-left">
                    <th className="px-3 py-2 font-medium">Resource Name</th>
                    <th className="px-3 py-2 font-medium">Surface</th>
                    <th className="px-3 py-2 font-medium">Type</th>
                    <th className="px-3 py-2 font-medium">Region</th>
                    <th className="px-3 py-2 font-medium">Trace axis</th>
                    <th className="px-3 py-2 font-medium">Log axis</th>
                    <th className="px-3 py-2 font-medium">Last seen</th>
                    <th className="px-3 py-2 font-medium">Quality</th>
                  </tr>
                </thead>
                <tbody>
                  {orchestrations.map((o) => (
                    <tr
                      key={o.resource_arn || o.resource_name}
                      className="border-t"
                    >
                      <td className="px-3 py-2 font-mono text-xs">
                        {o.resource_name}
                      </td>
                      <td className="px-3 py-2 text-xs">{o.surface}</td>
                      <td className="px-3 py-2 text-xs">
                        {o.workflow_type || "—"}
                      </td>
                      <td className="px-3 py-2 text-xs">{o.region}</td>
                      <td className="px-3 py-2 text-xs">
                        <AxisCheck ok={o.has_trace_axis} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <AxisCheck ok={o.has_log_axis} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <LastSeenCell value={o.last_seen_at} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <QualityDot quality={o.span_quality} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      )}
    </Card>
  );
}

// EventSourcesSection — event source tier slice 1 chunk 5 (v0.89.102,
// #738 Stream 136). Renders the per-row AWS EventBridge inventory the
// chunk 1 scanner produced. Columns per §7 of the design doc: Resource
// Name, Surface, Type (source_type — bus/topic/queue/namespace/stream),
// Region, Trace axis, Log axis, Last seen, Quality. AWS gets the
// Quality column per the slice 1 constraint mirroring v0.89.92 /
// v0.89.97 — GCP / Azure / OCI parity for QualityDot is a slice 2
// candidate. The section collapses when empty so an account with no
// event sources still gets a header but no row noise.
function EventSourcesSection({
  eventSources,
}: {
  eventSources: EventSourceRow[];
}) {
  const [open, setOpen] = useState(eventSources.length > 0);
  // propagationDialog — event source tier slice 2 chunk 5 (v0.89.107,
  // #745 Stream 143). Drives the side panel that surfaces all
  // propagation_notes for a row clicking the ✗ cell. Null when no
  // row is selected; the dialog stays unmounted in that case.
  const [propagationDialog, setPropagationDialog] = useState<{
    row: EventSourceRow;
    notes: string[];
  } | null>(null);
  return (
    <Card>
      <CardHeader>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex w-full items-center justify-between gap-2 text-left"
          aria-expanded={open}
          data-testid="event-sources-section-toggle"
        >
          <CardTitle className="text-base">
            Event sources ({eventSources.length})
          </CardTitle>
          {open ? (
            <ChevronDown className="h-4 w-4" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4" aria-hidden />
          )}
        </button>
      </CardHeader>
      {open && (
        <CardContent>
          {eventSources.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No event sources visible in the scanned regions.
            </p>
          ) : (
            <div
              className="overflow-x-auto rounded-md border"
              data-testid="event-sources-table"
            >
              <table className="w-full text-sm">
                <thead className="bg-muted/40">
                  <tr className="text-left">
                    <th className="px-3 py-2 font-medium">Resource Name</th>
                    <th className="px-3 py-2 font-medium">Surface</th>
                    <th className="px-3 py-2 font-medium">Type</th>
                    <th className="px-3 py-2 font-medium">Region</th>
                    <th className="px-3 py-2 font-medium">Trace axis</th>
                    <th className="px-3 py-2 font-medium">Log axis</th>
                    <th className="px-3 py-2 font-medium">Propagation</th>
                    <th className="px-3 py-2 font-medium">Last seen</th>
                    <th className="px-3 py-2 font-medium">Quality</th>
                  </tr>
                </thead>
                <tbody>
                  {eventSources.map((s) => (
                    <tr
                      key={s.resource_arn || s.resource_name}
                      className="border-t"
                      data-testid="event-sources-row"
                    >
                      <td className="px-3 py-2 font-mono text-xs">
                        {s.resource_name}
                      </td>
                      <td className="px-3 py-2 text-xs">{s.surface}</td>
                      <td className="px-3 py-2 text-xs">
                        {s.source_type || "—"}
                      </td>
                      <td className="px-3 py-2 text-xs">{s.region}</td>
                      <td className="px-3 py-2 text-xs">
                        <AxisCheck ok={s.has_trace_axis} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <AxisCheck ok={s.has_log_axis} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <PropagationCell
                          row={s}
                          onOpen={(row, notes) =>
                            setPropagationDialog({ row, notes })
                          }
                        />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <LastSeenCell value={s.last_seen_at} />
                      </td>
                      <td className="px-3 py-2 text-xs">
                        <QualityDot quality={s.span_quality} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      )}
      <PropagationNotesDialog
        state={propagationDialog}
        onClose={() => setPropagationDialog(null)}
      />
    </Card>
  );
}

// PropagationCell — event source tier slice 2 chunk 5 (v0.89.107,
// #745 Stream 143). Renders the per-row Propagation column on the
// Event sources table. Three states:
//   - undefined: nothing to evaluate → em dash
//   - true:      config preserves trace context end-to-end → ✓
//   - false:     config breaks propagation → ✗ button that opens
//                the notes side panel (matches the slice 1 palette —
//                amber for "primitive on but config gap").
function PropagationCell({
  row,
  onOpen,
}: {
  row: EventSourceRow;
  onOpen: (row: EventSourceRow, notes: string[]) => void;
}) {
  if (row.has_propagation_config === undefined) {
    return (
      <span
        aria-label="not evaluated"
        title="Propagation not evaluated for this row"
        data-testid="propagation-cell"
        data-value="unknown"
      >
        —
      </span>
    );
  }
  if (row.has_propagation_config) {
    return (
      <span
        className="text-emerald-600"
        aria-label="propagation preserved"
        title="Config preserves trace context end-to-end"
        data-testid="propagation-cell"
        data-value="yes"
      >
        ✓
      </span>
    );
  }
  const notes = row.propagation_notes ?? [];
  return (
    <button
      type="button"
      onClick={() => onOpen(row, notes)}
      className="text-amber-500 hover:text-amber-600"
      aria-label="propagation broken — click for details"
      title={notes[0] ?? "Propagation broken"}
      data-testid="propagation-cell"
      data-value="no"
    >
      ✗
    </button>
  );
}

// PropagationNotesDialog — event source tier slice 2 chunk 5
// (v0.89.107, #745 Stream 143). Side panel surfaced from the
// Propagation column ✗ button. Lists every propagation_notes entry
// for the row so the operator can read the full set of config gaps
// without leaving the Inventory tab. Uses the same Dialog primitive
// as the AWS connect flow for visual consistency.
function PropagationNotesDialog({
  state,
  onClose,
}: {
  state: { row: EventSourceRow; notes: string[] } | null;
  onClose: () => void;
}) {
  const isOpen = state !== null;
  return (
    <Dialog
      open={isOpen}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent
        className="max-w-lg"
        data-testid="propagation-notes-dialog"
      >
        <DialogHeader>
          <DialogTitle>Propagation notes</DialogTitle>
          <DialogDescription>
            {state ? `${state.row.surface} · ${state.row.resource_name}` : ""}
          </DialogDescription>
        </DialogHeader>
        {state && state.notes.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            Propagation broken; no specific notes recorded for this row.
          </p>
        ) : null}
        {state && state.notes.length > 0 ? (
          <ul
            className="space-y-2 text-sm"
            data-testid="propagation-notes-list"
          >
            {state.notes.map((note, i) => (
              <li
                key={i}
                className="rounded-md border border-amber-500/40 bg-amber-50/40 px-3 py-2 text-amber-900 dark:bg-amber-950/20 dark:text-amber-200"
              >
                {note}
              </li>
            ))}
          </ul>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function OtelBadge({ ok }: { ok: boolean }) {
  if (ok) {
    return (
      <Badge
        variant="outline"
        className="border-green-600/50 text-green-700 dark:text-green-400"
      >
        <Check className="h-3 w-3" aria-hidden />
        OTel detected
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="text-muted-foreground">
      <X className="h-3 w-3" aria-hidden />
      No OTel
    </Badge>
  );
}

function TagPills({ tags }: { tags: Record<string, string> }) {
  const entries = useMemo(() => Object.entries(tags ?? {}), [tags]);
  if (entries.length === 0) return null;
  const visible = entries.slice(0, 5);
  const overflow = entries.length - visible.length;
  return (
    <div className="mt-2 flex flex-wrap items-center gap-1">
      {visible.map(([k, v]) => (
        <Badge
          key={k}
          variant="outline"
          className="text-[10px] font-normal text-muted-foreground"
        >
          {k}
          {v ? `=${v}` : ""}
        </Badge>
      ))}
      {overflow > 0 && (
        <span className="text-[10px] text-muted-foreground">
          and {overflow} more
        </span>
      )}
    </div>
  );
}

// --- Recommendations tab -------------------------------------------

// Exported in v0.89.38 (#658 Stream 56) so the slice-2-chunk-5
// exclude-affordance tests can render the tab directly without
// driving the full page state machine.
export function RecommendationsTab({
  recs,
  accountID,
  scanID,
  region,
}: {
  recs: GenerateRecommendationsResponse | null;
  // v0.89.3 #603 Stream 19 Phase 4: scan + account context threaded
  // down to each card so the Open-PR button can address its POST.
  // Both empty before the first generate; populated after.
  accountID: string;
  scanID: string;
  // v0.89.38 #658 Stream 56 (#531 slice 2 chunk 5) — region the
  // recommendations were scanned against. Empty before the first
  // generate; populated after. Threaded into the exclude affordance's
  // POST body.
  region: string;
}) {
  // v0.89.38 #658 Stream 56 (#531 slice 2 chunk 5) — operator-set
  // exclusion state. Set keyed by recommendation_id.
  //
  // v0.89.40 (#660 Stream 58, #531 slice 2 chunk 5 follow-on):
  // hydrated from the persisted iac_recommendation_verdicts rows
  // via listExcludedRecommendations on mount, so the Excluded
  // badges survive a page refresh. The hydration effect below
  // (a) seeds the Set before the recommendation list renders for
  // the first time (the GET fires synchronously off the effect's
  // first run, and useEffect runs after the first commit but the
  // Set's next state lands on the second paint — fast enough that
  // operators don't see the unbadged flash on a healthy
  // deployment); (b) degrades gracefully on error: console.error +
  // leave the Set empty so the operator can still toggle.
  const [excludedSet, setExcludedSet] = useState<Set<string>>(() => new Set());
  // v0.89.82 (#713 Stream 111, Trace integration slice 2 chunk 3) —
  // operator-set filter chip that narrows the recommendations list to
  // the slice-2 trace-emission-* drafts. Toggled via the chip above
  // the list. Trace-emission recommendations are recognised by their
  // resource_kind prefix ("trace-emission-…"); other kinds (the
  // existing ec2-otel-layer / lambda-otel-layer / rds-pi-em / etc.)
  // are hidden while the filter is active.
  const [traceEmissionFilter, setTraceEmissionFilter] = useState(false);
  // v0.89.88 (#719 Stream 117, Span quality slice 1 chunk 4) —
  // sibling filter chip for the span-quality-* recommendation
  // kinds shipped in v0.89.86. Same toggle semantics as the
  // trace-emission filter — narrows to recommendations whose
  // resource_kind starts with "span-quality-" (orphan-trace /
  // missing-resource-attrs / attribute-mismatch). The dashboard
  // SPAN QUALITY panel deeplinks here so operators land on the
  // filtered drafts.
  const [spanQualityFilter, setSpanQualityFilter] = useState(false);
  // v0.89.204 — render a filter chip only when the current
  // recommendations actually contain that kind, so the chips are
  // contextual rather than permanent dead controls. Matters now that
  // the tab is reused by GCP/Azure/OCI (whose kinds never start with
  // trace-emission-/span-quality-), and cleans up AWS runs with no
  // such drafts too.
  const recList = recs?.recommendations ?? [];
  const hasTraceEmissionRecs = recList.some((r) =>
    (r.resource_kind ?? "").startsWith("trace-emission-"),
  );
  const hasSpanQualityRecs = recList.some((r) =>
    (r.resource_kind ?? "").startsWith("span-quality-"),
  );
  // v0.89.40 hydrate from the GET endpoint on mount and whenever the
  // scope tuple changes. The proposer's `connection_id` is today
  // equal to accountID (matching the substrate's connection_id
  // posture) so we pass it in both slots. The effect short-circuits
  // when any scope field is empty — pre-generate state, before the
  // operator picks an account, has nothing to hydrate.
  useEffect(() => {
    if (!accountID || !region) {
      return;
    }
    let cancelled = false;
    listExcludedRecommendations({
      connection_id: accountID,
      account_id: accountID,
      region,
    })
      .then((rows) => {
        if (cancelled) {
          return;
        }
        setExcludedSet(new Set(rows.map((r) => r.recommendation_id)));
      })
      .catch((err) => {
        // Graceful degradation: log + leave the Set empty so the
        // operator can still toggle. The audit timeline remains
        // the authoritative log for "was this excluded?" questions.

        console.error("listExcludedRecommendations failed", err);
      });
    return () => {
      cancelled = true;
    };
  }, [accountID, region]);
  // toast holds the most-recent success / error notification rendered
  // above the recommendations list. Self-clears after 4 seconds.
  // Modelled inline (no toast lib in slice-2 UI) — matches the
  // existing aria-live success card pattern.
  const [toast, setToast] = useState<{
    kind: "success" | "error";
    message: string;
  } | null>(null);
  // Track the auto-clear timer so we cancel on unmount or on rapid
  // re-toast — prevents a stale clear firing into an unmounted
  // component (and the "update outside act()" test-side warning that
  // such timers throw).
  const toastTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    return () => {
      if (toastTimerRef.current !== null) {
        clearTimeout(toastTimerRef.current);
      }
    };
  }, []);
  const showToast = useCallback(
    (kind: "success" | "error", message: string) => {
      if (toastTimerRef.current !== null) {
        clearTimeout(toastTimerRef.current);
      }
      setToast({ kind, message });
      toastTimerRef.current = setTimeout(() => {
        setToast(null);
        toastTimerRef.current = null;
      }, 4000);
    },
    [],
  );
  // Three states: never generated (empty), declined by the proposer
  // (informational), recommendations present (full list). Kept inline
  // rather than reusing the v0.25 RecommendationsPanel because that
  // panel fetches its own data via SWR; the discovery-source
  // recommendations are pushed in via props per scan.
  //
  // v0.89.3 #603 Stream 19 Phase 4: a single IaC-connections fetch
  // lives at this level (one round trip per Recommendations-tab
  // mount). Each card derives its connection-state (none / placement
  // present / placement missing) from the cached list. The trade-off
  // is intentional — slice 1 ships at most a few connections per
  // deployment, so re-fetching per card would be needless network
  // traffic.
  const { data: iacData } = useSWR(IAC_GITHUB_CONNECTIONS_SWR_KEY, () =>
    listIaCGitHubConnections(),
  );
  const iacConnections = iacData?.connections ?? [];

  // handleToggleExclusion drives the per-card "Don't propose this
  // again" / "Restore" affordance. Optimistic update + rollback on
  // error:
  //   1. compute the inverse desired state from the current Set
  //   2. update the Set immediately (optimistic)
  //   3. POST setRecommendationExclusion
  //   4. on error: rollback the Set, surface the err message as a
  //      toast
  //   5. on success: show the success toast (with the "session-only"
  //      caveat — see the TODO above on persistent state)
  //
  // v1 (chunk 5) scoping decision (§11 Q4): if the recommendation
  // names affected resources, exclude resource-level (passing the
  // first as resource_id); otherwise kind-level. v2 may surface a
  // dropdown letting the operator pick explicitly.
  const handleToggleExclusion = useCallback(
    async (rec: Recommendation) => {
      const wasExcluded = excludedSet.has(rec.id);
      const nextExcluded = !wasExcluded;
      const resourceID =
        rec.affected_resources && rec.affected_resources.length > 0
          ? rec.affected_resources[0]
          : "";
      // Optimistic Set update.
      setExcludedSet((prev) => {
        const next = new Set(prev);
        if (nextExcluded) {
          next.add(rec.id);
        } else {
          next.delete(rec.id);
        }
        return next;
      });
      try {
        await setRecommendationExclusion({
          recommendation_id: rec.id,
          connection_id: accountID,
          account_id: accountID,
          region,
          recommendation_kind: rec.resource_kind ?? "",
          resource_id: resourceID || undefined,
          excluded: nextExcluded,
        });
        showToast(
          "success",
          nextExcluded
            ? "Excluded — Squadron won't propose this on future scans. Click Restore to undo."
            : "Restored — Squadron will propose this again on future scans.",
        );
      } catch (e) {
        // Rollback the optimistic Set change.
        setExcludedSet((prev) => {
          const next = new Set(prev);
          if (nextExcluded) {
            next.delete(rec.id);
          } else {
            next.add(rec.id);
          }
          return next;
        });
        const message =
          e instanceof Error
            ? e.message
            : "Squadron could not save the exclusion. Retry in a moment.";
        showToast("error", message);
      }
    },
    [accountID, region, excludedSet, showToast],
  );

  if (!recs) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center gap-3 p-8 text-center">
          <Sparkles className="h-8 w-8 text-violet-500" aria-hidden />
          <div>
            <h3 className="text-base font-semibold">No recommendations yet.</h3>
            <p className="mt-2 max-w-md text-sm text-muted-foreground">
              Run a scan and click &quot;Generate recommendations&quot; from the
              Inventory tab.
            </p>
            <p className="mt-3 max-w-md text-xs text-muted-foreground">
              Recommendations arrive as Terraform snippets for your IaC pipeline
              — Squadron never mutates your cloud.
            </p>
          </div>
        </CardContent>
      </Card>
    );
  }

  if (recs.declined) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center gap-3 p-8 text-center">
          <Sparkles className="h-8 w-8 text-muted-foreground" aria-hidden />
          <div>
            <h3 className="text-base font-semibold">
              No productive recommendations for this scan.
            </h3>
            <p className="mt-2 max-w-md text-sm text-muted-foreground">
              {recs.reason ??
                "The proposer declined; nothing actionable to surface."}
            </p>
          </div>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-4">
      {toast && (
        <div
          role="status"
          aria-live="polite"
          data-testid="exclusion-toast"
          className={
            toast.kind === "success"
              ? "rounded-md border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm"
              : "rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive"
          }
        >
          {toast.message}
        </div>
      )}
      {recs.reasoning && (
        <Card>
          <CardContent className="p-4 text-sm">
            <p className="text-xs uppercase tracking-wider text-muted-foreground">
              Proposer reasoning
            </p>
            <blockquote className="mt-1 border-l-2 border-violet-500/50 pl-3 italic text-muted-foreground">
              {recs.reasoning}
            </blockquote>
          </CardContent>
        </Card>
      )}
      {/*
        v0.89.82 (#713 Stream 111) — slice-2-chunk-3 filter chip.
        Narrows the list to the trace-emission-* recommendation kinds
        the slice-2 proposer arc produces. The dashboard sub-indicator
        deeplinks here so the operator lands on the same drafts the
        fleet-wide count refers to.
      */}
      {(hasTraceEmissionRecs || hasSpanQualityRecs) && (
        <div
          className="flex items-center gap-2"
          data-testid="trace-emission-filter-row"
        >
          {hasTraceEmissionRecs && (
            <button
              type="button"
              onClick={() => setTraceEmissionFilter((v) => !v)}
              data-testid="trace-emission-filter-chip"
              data-active={traceEmissionFilter ? "true" : "false"}
              aria-pressed={traceEmissionFilter}
              className={
                traceEmissionFilter
                  ? "rounded-full bg-amber-500/20 px-3 py-1 text-xs font-medium text-amber-700 dark:text-amber-300"
                  : "rounded-full bg-muted px-3 py-1 text-xs font-medium text-muted-foreground"
              }
            >
              Show only trace-emission
            </button>
          )}
          {/*
            span quality sibling chip — toggles independently; if both
            are active the list shows the union (rows whose
            resource_kind starts with EITHER prefix).
          */}
          {hasSpanQualityRecs && (
            <button
              type="button"
              onClick={() => setSpanQualityFilter((v) => !v)}
              data-testid="span-quality-filter-chip"
              data-active={spanQualityFilter ? "true" : "false"}
              aria-pressed={spanQualityFilter}
              className={
                spanQualityFilter
                  ? "rounded-full bg-amber-500/20 px-3 py-1 text-xs font-medium text-amber-700 dark:text-amber-300"
                  : "rounded-full bg-muted px-3 py-1 text-xs font-medium text-muted-foreground"
              }
            >
              Show only span-quality
            </button>
          )}
        </div>
      )}
      <ul className="space-y-3">
        {(() => {
          const anyFilter = traceEmissionFilter || spanQualityFilter;
          return anyFilter
            ? recs.recommendations.filter((r) => {
                const kind = r.resource_kind ?? "";
                if (traceEmissionFilter && kind.startsWith("trace-emission-"))
                  return true;
                if (spanQualityFilter && kind.startsWith("span-quality-"))
                  return true;
                return false;
              })
            : recs.recommendations;
        })().map((rec, i) => (
          <li key={rec.id}>
            <DiscoveryRecommendationCard
              rec={rec}
              stepIdx={i}
              scanID={scanID || rec.source?.ref_id || ""}
              accountID={accountID}
              proposerReasoning={recs.reasoning ?? ""}
              iacConnections={iacConnections}
              excluded={excludedSet.has(rec.id)}
              onToggleExclusion={
                region ? () => handleToggleExclusion(rec) : undefined
              }
            />
          </li>
        ))}
      </ul>
    </div>
  );
}

// DiscoveryRecommendationCard renders one discovery-source
// Recommendation. Re-implements the IaC snippet pattern from
// RecommendationsPanel.tsx inline rather than reusing the panel
// component because that panel owns its own SWR fetcher; the data flow
// here is push-from-props.
//
// v0.89.3 #603 Stream 19 Phase 4: closes the slice-1 copy-paste loop.
// The card renders one of three states based on the IaC connection
// inventory:
//   A. A connection exists AND has a placement-map row for this
//      recommendation's resource_kind → Open PR button (primary) +
//      Copy snippet (secondary).
//   B. A connection exists but no placement row for resource_kind →
//      Copy only + inline notice with a link to the connections page
//      so the operator can add the row.
//   C. No connections at all → Copy only + inline notice with a link
//      to the connect flow.
// On success the button area collapses into a PR-opened success card
// (PR link target=_blank, file path, "Squadron will not push to this
// branch again" footer). On failure the card renders a humanized
// message + a recovery action keyed on the error code.
export function DiscoveryRecommendationCard({
  rec,
  stepIdx,
  scanID,
  accountID,
  proposerReasoning,
  iacConnections,
  inAggregateView = false,
  onPickAccount,
  excluded = false,
  onToggleExclusion,
}: {
  rec: Recommendation;
  stepIdx: number;
  scanID: string;
  accountID: string;
  proposerReasoning: string;
  iacConnections: IaCGitHubConnection[];
  // v0.89.7b (#619 Stream 23) — when rendered in the aggregate view
  // (?account=all), the card gains a clickable account badge that
  // filters the page down to a single account. In single-account
  // view (?account=<id>) the badge is suppressed — the operator
  // already picked an account, no signal to add.
  inAggregateView?: boolean;
  // onPickAccount drives the badge-click navigation. Required when
  // inAggregateView is true; otherwise unused.
  onPickAccount?: (accountID: string) => void;
  // v0.89.38 #658 Stream 56 (#531 slice 2 chunk 5) — operator-set
  // exclusion state for this recommendation. When true, the card
  // dims, gains an "Excluded" badge, and the exclude button flips
  // its label to "Restore as recommendation". Driven by the
  // parent's session-scoped Set so the page-level toast / rollback
  // logic stays in one place. Defaults to false.
  excluded?: boolean;
  // onToggleExclusion drives the button click. The parent computes
  // the next state and routes through setRecommendationExclusion;
  // the card just calls back. Undefined disables the affordance —
  // useful in test renders that don't exercise the exclude path.
  onToggleExclusion?: () => void;
}) {
  const [iacOpen, setIacOpen] = useState(true);
  const [copied, setCopied] = useState(false);
  const [opening, setOpening] = useState(false);
  const [openPRResult, setOpenPRResult] =
    useState<IaCGitHubOpenPRResponse | null>(null);
  const [openPRError, setOpenPRError] = useState<IaCGitHubOpenPRError | null>(
    null,
  );

  // Derive the per-card connection state from the page-level list.
  // Slice 1 ships at most one IaC connection per deployment; the
  // single-connection invariant lets us pick `iacConnections[0]` as
  // "the" connection. A future slice that supports multiple
  // connections per resource_kind will need a connection picker; for
  // now, a clear constraint beats a half-baked UX.
  const connection: IaCGitHubConnection | null = iacConnections[0] ?? null;
  const placement = connection?.placement_map.find(
    (p) => p.resource_kind === rec.resource_kind,
  );
  const placementFilePath = placement?.file_path ?? null;

  // Open-PR-eligible iff the recommendation has a resource_kind, an
  // IaC connection exists, and that connection has a placement-map
  // row matching the resource_kind. Anything else routes through the
  // State-B or State-C notices.
  const openPREligible =
    !!connection && !!placementFilePath && !!rec.resource_kind;

  const snippetSource = rec.iac?.source ?? "";

  const handleCopy = useCallback(async () => {
    if (!snippetSource) return;
    try {
      await navigator.clipboard.writeText(snippetSource);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // Clipboard may be blocked in non-https / iframe contexts.
      // Operator can still expand the card and copy from the <pre>.
    }
  }, [snippetSource]);

  const handleOpenPR = useCallback(async () => {
    if (
      !connection ||
      !rec.resource_kind ||
      !snippetSource ||
      !scanID ||
      opening
    ) {
      return;
    }
    setOpening(true);
    setOpenPRError(null);
    try {
      const res = await openIaCGitHubPullRequest(connection.connection_id, {
        scan_id: scanID,
        step_idx: stepIdx,
        resource_kind: rec.resource_kind,
        snippet: snippetSource,
        proposer_reasoning: proposerReasoning,
        // v0.89.4 (#611) — forward the proposer-emitted per-step
        // resource id list verbatim. Empty when the proposer model
        // didn't emit the field (cold-start with an old prompt);
        // the backend's PR title falls back to "for 0 resources" and
        // the body's section is omitted — same as the Phase-4
        // posture, just lit up for newer proposer outputs.
        affected_resources: rec.affected_resources ?? [],
        account_id: accountID || undefined,
        // v0.89.12 (#628 Stream 29) — slice 2 — forward the
        // proposer-emitted structured patch verbatim. When undefined
        // (slice-1.5-era recommendation, or new_file step), the
        // backend's HCL merger doesn't run and the slice-1.5
        // append-only path stays in effect — no UI-side branching
        // needed.
        hcl_patch: rec.hcl_patch,
      });
      setOpenPRResult(res);
    } catch (e) {
      if (e instanceof IaCGitHubOpenPRError) {
        setOpenPRError(e);
        if (e.code === "DefaultBranchWriteRefused") {
          // This is a code-layer regression — both the wizard and the
          // backend refuse this. If it lands in the UI it's worth a
          // diagnostic for the on-call rotation; the operator-visible
          // message stays clean.
          console.error("Open PR refused default-branch write", {
            connection_id: connection.connection_id,
            resource_kind: rec.resource_kind,
          });
        }
      } else {
        // Network failure or unexpected throw — wrap in a synthetic
        // typed error so the renderer doesn't branch on raw Error.
        setOpenPRError(
          new IaCGitHubOpenPRError(
            0,
            {
              code: "NetworkError",
              message: e instanceof Error ? e.message : "Open PR failed.",
              suggested_step: "",
            },
            "Open PR failed.",
          ),
        );
      }
    } finally {
      setOpening(false);
    }
  }, [
    connection,
    rec.resource_kind,
    rec.affected_resources,
    rec.hcl_patch,
    snippetSource,
    scanID,
    stepIdx,
    proposerReasoning,
    accountID,
    opening,
  ]);

  // v0.89.7b (#619 Stream 23) — account badge rendered only in
  // aggregate view. Clicking it filters the page to the single
  // account this recommendation came from (URL updates via the
  // page-level onPickAccount). Suppressed in single-account view
  // because the operator already picked an account.
  const showAccountBadge = inAggregateView && !!accountID;

  return (
    <Card
      className={
        excluded ? "border-muted-foreground/30 opacity-60 grayscale" : undefined
      }
      data-testid="discovery-recommendation-card"
      data-excluded={excluded ? "true" : "false"}
    >
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="text-base">{rec.title}</CardTitle>
            {excluded && (
              <Badge
                variant="outline"
                aria-label="Excluded from future recommendations"
                className="border-muted-foreground/40 text-xs uppercase tracking-wider text-muted-foreground"
              >
                Excluded
              </Badge>
            )}
          </div>
          {showAccountBadge && (
            <button
              type="button"
              onClick={() => onPickAccount?.(accountID)}
              aria-label={`Filter to account ${accountID}`}
              className="inline-flex"
            >
              <Badge
                variant="outline"
                className="cursor-pointer border-violet-500/40 text-xs text-violet-700 hover:bg-violet-500/10 dark:text-violet-300"
              >
                from {shortenAccountID(accountID)}
              </Badge>
            </button>
          )}
        </div>
        <CardDescription>
          {rec.source?.kind === "discovery_scan" && rec.source.ref_id ? (
            <>
              Discovery scan{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
                {rec.source.ref_id}
              </code>
            </>
          ) : null}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        {rec.detail && (
          <p className="text-sm text-muted-foreground">{rec.detail}</p>
        )}
        {rec.iac && (
          <div>
            <button
              type="button"
              onClick={() => setIacOpen((v) => !v)}
              className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground hover:text-foreground"
              aria-expanded={iacOpen}
            >
              {iacOpen ? "Hide" : "Show"} Terraform ({rec.iac.format})
            </button>
            {iacOpen && (
              <pre className="mt-2 max-h-64 overflow-auto rounded-sm bg-muted/60 p-2 font-mono text-[11px] leading-snug">
                {rec.iac.source}
              </pre>
            )}
          </div>
        )}

        {/* Action row: Copy + Open PR (State A), or Copy only with a
            State-B / State-C notice. Hidden entirely when there's no
            snippet to act on. */}
        {snippetSource && (
          <div className="space-y-2 pt-1">
            {/* Success card — replaces the button row once the PR
                is open. aria-live polite so screen readers
                announce the PR-opened state without yanking focus. */}
            {openPRResult ? (
              <div
                role="status"
                aria-live="polite"
                className="rounded-md border border-green-500/40 bg-green-500/5 p-3 text-sm"
              >
                <p className="font-medium">
                  PR #{openPRResult.pr_number} opened in{" "}
                  <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
                    {openPRResult.repo_full_name}
                  </code>
                </p>
                <p className="mt-1 text-xs text-muted-foreground">
                  File:{" "}
                  <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px]">
                    {openPRResult.file_path}
                  </code>
                </p>
                {/* v0.89.11 (#626 Stream 27) — slice-1.5 manual-merge
                    marker on the success card. Anchors the operator's
                    recall to the same "[needs manual merge]" prefix
                    the PR title carries on GitHub.

                    v0.89.12 (#628 Stream 29) — slice 2 — when the
                    backend's HCL merge ran cleanly (disposition_actual
                    = patch_existing_hcl_merged), surface a green
                    affirmative; when it fell back, surface the failure
                    reason so the operator knows WHY the manual-merge
                    banner is back. */}
                {openPRResult.disposition_actual ===
                  "patch_existing_hcl_merged" && (
                  <p className="mt-1 text-xs font-medium text-emerald-700 dark:text-emerald-300">
                    <Check className="mr-1 inline h-3 w-3" aria-hidden />
                    HCL-merged — Squadron parsed the placement file and applied
                    the patch in place. terraform plan accepts the result; no
                    manual integration needed.
                  </p>
                )}
                {openPRResult.lifecycle_ignored && (
                  <p className="mt-1 text-xs text-amber-700 dark:text-amber-300">
                    <AlertTriangle
                      className="mr-1 inline h-3 w-3"
                      aria-hidden
                    />
                    lifecycle.ignore_changes covers one of the patched
                    attributes — terraform apply will no-op that attribute until
                    you edit the ignore_changes entry.
                  </p>
                )}
                {openPRResult.manual_merge_required && (
                  <p className="mt-1 text-xs font-medium text-amber-700 dark:text-amber-300">
                    <AlertTriangle
                      className="mr-1 inline h-3 w-3"
                      aria-hidden
                    />
                    Manual merge required — Squadron appended the snippet to
                    your placement file. Hand-integrate before merging.
                    {openPRResult.hcl_patch_failure_reason ? (
                      <>
                        {" "}
                        (slice-2 HCL merge fell back: reason{" "}
                        <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px]">
                          {openPRResult.hcl_patch_failure_reason}
                        </code>
                        )
                      </>
                    ) : null}
                  </p>
                )}
                <p className="mt-2 text-xs text-muted-foreground">
                  Squadron will not push to this branch again.
                </p>
                <div className="mt-2 flex items-center gap-2">
                  <a
                    href={openPRResult.pr_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 text-xs font-medium text-violet-500 hover:underline"
                  >
                    <ExternalLink className="h-3 w-3" aria-hidden />
                    View PR
                  </a>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={handleCopy}
                    className="text-xs"
                  >
                    {copied ? (
                      <Check className="mr-1 h-3 w-3" aria-hidden />
                    ) : (
                      <Copy className="mr-1 h-3 w-3" aria-hidden />
                    )}
                    {copied ? "Copied" : "Copy snippet"}
                  </Button>
                </div>
              </div>
            ) : (
              <>
                <div className="flex flex-wrap items-center gap-2">
                  {openPREligible && (
                    <Button
                      type="button"
                      size="sm"
                      onClick={handleOpenPR}
                      disabled={opening}
                      aria-label={`Open PR for ${rec.title}`}
                    >
                      {opening ? (
                        <Loader2
                          className="mr-1 h-3 w-3 animate-spin"
                          aria-hidden
                        />
                      ) : (
                        <GitPullRequest className="mr-1 h-3 w-3" aria-hidden />
                      )}
                      {opening ? "Opening PR…" : "Open PR"}
                    </Button>
                  )}
                  {/* v0.89.11 (#626 Stream 27) — slice-1.5 hybrid PR
                      disposition. For patch_existing kinds (Lambda OTel
                      layer, RDS PI/EM, ALB access logs, EKS cluster
                      logging, ECS container insights), surface a
                      "Needs manual merge" badge BEFORE the operator
                      clicks Open PR so they know the slice-1 append-
                      only behavior will require hand integration. For
                      new_file kinds the badge is suppressed — the
                      implicit "clean" experience.

                      v0.89.12 (#628 Stream 29) — slice 2 — when the
                      recommendation carries an `hcl_patch` field the
                      backend's HCL-aware merger will produce a clean
                      drop-in PR; we swap the amber "Needs manual
                      merge" badge for a green "HCL-merged" checkmark
                      so the operator sees the clean experience BEFORE
                      clicking Open PR. The fallback (proposer didn't
                      emit a patch — slice 1.5 era recommendation)
                      keeps the amber badge. */}
                  {openPREligible &&
                    rec.disposition === "patch_existing" &&
                    rec.hcl_patch != null && (
                      <Badge
                        variant="outline"
                        title="Squadron will parse the placement file and apply the proposer's structured patch in place. terraform plan accepts the result; no manual integration needed."
                        aria-label="HCL-merged — clean apply"
                        className="border-emerald-500/40 text-xs text-emerald-700 dark:text-emerald-300"
                      >
                        <Check className="mr-1 h-3 w-3" aria-hidden />
                        HCL-merged
                      </Badge>
                    )}
                  {openPREligible &&
                    rec.disposition === "patch_existing" &&
                    rec.hcl_patch == null && (
                      <Badge
                        variant="outline"
                        title="Squadron appends the snippet to the placement file. terraform plan will fail with a duplicate-resource error until you hand-integrate the change. The proposer didn't emit a structured patch for this recommendation; the slice-2 HCL-aware merge could not run."
                        aria-label="Needs manual merge — patch_existing disposition"
                        className="border-amber-500/40 text-xs text-amber-700 dark:text-amber-300"
                      >
                        <AlertTriangle className="mr-1 h-3 w-3" aria-hidden />
                        Needs manual merge
                      </Badge>
                    )}
                  <Button
                    type="button"
                    size="sm"
                    variant={openPREligible ? "outline" : "default"}
                    onClick={handleCopy}
                    aria-label={`Copy Terraform snippet for ${rec.title}`}
                  >
                    {copied ? (
                      <Check className="mr-1 h-3 w-3" aria-hidden />
                    ) : (
                      <Copy className="mr-1 h-3 w-3" aria-hidden />
                    )}
                    {copied ? "Copied" : "Copy snippet"}
                  </Button>
                  {/* v0.89.38 #658 Stream 56 (#531 slice 2 chunk 5) —
                      operator-set exclusion affordance. Button label
                      flips on the excluded state. Suppressed when the
                      parent didn't wire onToggleExclusion (e.g. tests
                      that don't exercise the path, or aggregate
                      view). */}
                  {onToggleExclusion && (
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      onClick={onToggleExclusion}
                      aria-label={
                        excluded
                          ? `Restore ${rec.title} as a recommendation`
                          : `Don't propose ${rec.title} again`
                      }
                      className="text-xs"
                    >
                      {excluded ? (
                        <RotateCcw className="mr-1 h-3 w-3" aria-hidden />
                      ) : (
                        <Ban className="mr-1 h-3 w-3" aria-hidden />
                      )}
                      {excluded
                        ? "Restore as recommendation"
                        : "Don't propose this again"}
                    </Button>
                  )}
                </div>

                {/* State B — connection exists, no placement row for
                    this resource_kind. Reachable when the operator
                    skipped this kind in the wizard or when a new
                    resource_kind ships after their connect. v0.89.4
                    (#610) deep-links the link target to auto-open
                    the wizard at the right placement row in one
                    click instead of dropping the operator on the
                    connections list. */}
                {connection && rec.resource_kind && !placementFilePath && (
                  <p className="text-xs text-muted-foreground">
                    Open PR needs a Terraform file path for{" "}
                    <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px]">
                      {rec.resource_kind}
                    </code>{" "}
                    in your{" "}
                    <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px]">
                      {connection.repo_full_name}
                    </code>{" "}
                    connection.{" "}
                    <Link
                      to={buildIaCPlacementDeepLink(
                        connection.connection_id,
                        rec.resource_kind,
                      )}
                      className="text-violet-500 hover:underline"
                    >
                      Configure placement
                    </Link>
                  </p>
                )}

                {/* State C — no connections at all. */}
                {!connection && (
                  <p className="text-xs text-muted-foreground">
                    Want one-click PRs?{" "}
                    <Link
                      to="/discovery/iac/github"
                      className="text-violet-500 hover:underline"
                    >
                      Connect a Terraform repo
                    </Link>{" "}
                    to enable Open PR on this recommendation.
                  </p>
                )}

                {/* Open-PR failure — humanized message + recovery
                    link keyed on the typed error code. Renders
                    BELOW the action row so the operator can still
                    retry from the same Open PR button. */}
                {openPRError && (
                  <div
                    role="alert"
                    className={
                      openPRError.code === "DefaultBranchWriteRefused"
                        ? "rounded-md border border-destructive/40 bg-destructive/5 p-2 text-xs text-destructive"
                        : "rounded-md border border-amber-500/40 bg-amber-500/5 p-2 text-xs"
                    }
                  >
                    <p className="font-medium">{openPRError.message}</p>
                    <p className="mt-1 text-muted-foreground">
                      {openPRErrorRecoveryHint(
                        openPRError,
                        connection,
                        rec.resource_kind,
                      )}
                    </p>
                  </div>
                )}
              </>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// openPRErrorRecoveryHint renders a one-line recovery hint keyed on
// the typed error code. JSX-returning helper so the case statement
// stays out of the renderer's body.
//
// v0.89.4 (#610): the NoPlacementMapping recovery hint now deep-links
// to the wizard with the right connection + kind so the operator
// lands on the row that needs editing in one click.
function openPRErrorRecoveryHint(
  err: IaCGitHubOpenPRError,
  connection: IaCGitHubConnection | null,
  resourceKind: string | undefined,
): ReactElement {
  const bareHref = "/discovery/iac/github";
  switch (err.code) {
    case "NoPlacementMapping":
      // Deep-link when we have both pieces — we virtually always do
      // here because NoPlacementMapping is only raised after the
      // server confirmed the connection exists and resolved a
      // resource_kind. Fall through to the bare link in the
      // degenerate case so the operator still has a path forward.
      return (
        <>
          <Link
            to={
              connection && resourceKind
                ? buildIaCPlacementDeepLink(
                    connection.connection_id,
                    resourceKind,
                  )
                : bareHref
            }
            className="text-violet-500 hover:underline"
          >
            Configure the missing placement row
          </Link>{" "}
          in your IaC connection and retry.
        </>
      );
    case "RepoNotFound":
    case "AuthFailed":
    case "CredentialDecryptFailed":
      return (
        <>
          <Link to={bareHref} className="text-violet-500 hover:underline">
            Re-run the IaC connect wizard
          </Link>{" "}
          to refresh the connection
          {connection ? ` to ${connection.repo_full_name}` : ""}.
        </>
      );
    case "DefaultBranchWriteRefused":
      return (
        <>
          This is a Squadron bug. Check the audit timeline and report it —
          Squadron should never attempt to push to the default branch.
        </>
      );
    case "FileNotFound":
      return (
        <>
          Squadron could not read the placement file. Confirm the path in your{" "}
          <Link to={bareHref} className="text-violet-500 hover:underline">
            IaC connection
          </Link>{" "}
          still exists on the default branch.
        </>
      );
    default:
      return (
        <>
          Squadron couldn&apos;t open the PR. Check the audit timeline for
          details.
        </>
      );
  }
}

// buildIaCPlacementDeepLink renders the v0.89.4 (#610) query-param
// shape the DiscoveryIaCGitHub page consumes:
//   /discovery/iac/github?connection_id=<uuid>&step=placement&kind=<resource_kind>
//
// The page validates connection_id against the SWR list (renders a
// stale notice if not found) and validates kind against the canonical
// seven (silently drops + opens at the placement step without a
// focused row if not). The helper is the only site that should
// assemble these params — link sites import it rather than
// concatenating strings so a future query-shape change touches one
// place.
function buildIaCPlacementDeepLink(
  connectionID: string,
  resourceKind: string,
): string {
  const params = new URLSearchParams();
  params.set("connection_id", connectionID);
  params.set("step", "placement");
  params.set("kind", resourceKind);
  return `/discovery/iac/github?${params.toString()}`;
}

// LastSeenCell — v0.89.77 trace integration slice 1 chunk 4. The AWS
// Inventory tab uses a card layout rather than a table, so the cell
// renders as an inline pill placed next to the per-row OTel /
// instrumentation badge. The "never" branch carries the same amber
// warning indicator as the GCP / Azure / OCI tables so the operator's
// visual vocabulary is consistent across the four provider pages.
function LastSeenCell({ value }: { value?: string }) {
  const rel = relativeTime(value);
  if (rel.isNever) {
    return (
      <Badge
        variant="outline"
        className="border-amber-500/50 text-amber-700 dark:text-amber-400"
        title="No spans observed for this resource"
        data-testid="last-seen-never"
      >
        <AlertTriangle className="h-3 w-3" aria-hidden />
        Last seen: {rel.text}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="text-muted-foreground" title={value}>
      Last seen: {rel.text}
    </Badge>
  );
}

// QualityDot — v0.89.87 span quality slice 1 chunk 3. Per-row health
// indicator rendered next to LastSeenCell. Four states per design
// doc §7.2: green (no issues), yellow (1 pathology), red (2+),
// gray (no observations / chunk-2 hasn't annotated this row yet).
// Tooltip surfaces the three percentages.
//
// Per-row data sourcing: chunk 3 takes the server-side-annotation
// path. The chunk-2 sibling branch extends the scan marshalling to
// populate row.span_quality; until chunk 2 merges, every dot is
// gray. The lazy per-row fetch alternative would add N round-trips
// per scan render — overweight for slice 1.
export function QualityDot({ quality }: { quality?: RowSpanQuality | null }) {
  if (!quality) {
    return (
      <span
        className="inline-block h-2 w-2 rounded-full bg-slate-500/60 align-middle"
        title="No spans observed"
        aria-label="Span quality: no observations"
        data-testid="quality-dot"
        data-color="gray"
      />
    );
  }
  // Slice 2 (v0.89.110) extends the "issues" count to include the two
  // W3C trace context percentages. Sampling rate slice 1 chunk 3
  // (v0.89.124, #764 Stream 162) extends again to include the sampling-
  // too-aggressive percentage. Older scan responses omit the new
  // fields; treat undefined as 0 so a graceful upgrade rollout shows
  // unchanged dot colors until the backend ships the new counters.
  const malformedTp = quality.malformed_traceparent_pct ?? 0;
  const missingTpOnChild = quality.missing_traceparent_on_child_pct ?? 0;
  const samplingTooAggressive = quality.sampling_too_aggressive_pct ?? 0;
  const issues = [
    quality.orphan_pct > 0,
    quality.missing_attr_pct > 0,
    quality.attr_mismatch_pct > 0,
    malformedTp > 0,
    missingTpOnChild > 0,
    samplingTooAggressive > 0,
  ].filter(Boolean).length;
  let colorClass = "bg-emerald-500";
  let colorTag = "green";
  if (issues === 1) {
    colorClass = "bg-amber-400";
    colorTag = "yellow";
  } else if (issues >= 2) {
    colorClass = "bg-red-500";
    colorTag = "red";
  }
  const tooltip =
    `Orphan ${quality.orphan_pct.toFixed(1)}%, ` +
    `Missing attrs ${quality.missing_attr_pct.toFixed(1)}%, ` +
    `Mismatch ${quality.attr_mismatch_pct.toFixed(1)}%, ` +
    `Malformed traceparent ${malformedTp.toFixed(1)}%, ` +
    `Missing on child ${missingTpOnChild.toFixed(1)}%, ` +
    `Sampling too aggressive ${samplingTooAggressive.toFixed(1)}%`;
  return (
    <span
      className={`inline-block h-2 w-2 rounded-full align-middle ${colorClass}`}
      title={tooltip}
      aria-label={`Span quality: ${tooltip}`}
      data-testid="quality-dot"
      data-color={colorTag}
    />
  );
}
