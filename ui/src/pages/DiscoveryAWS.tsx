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
  Sparkles,
  X,
} from "lucide-react";
import { useCallback, useMemo, useState, type ReactElement } from "react";
import { Link, useSearchParams } from "react-router-dom";
import useSWR from "swr";

import {
  generateAWSRecommendations,
  listAWSConnections,
  runAWSScan,
  saveAWSConnection,
  scanAllAWS,
  validateAWSConnection,
  type AWSScanAllResponse,
  type CloudConnection,
  type GenerateRecommendationsResponse,
  type ScanResult,
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
  const [recs, setRecs] =
    useState<GenerateRecommendationsResponse | null>(null);
  // v0.89.3 #603 Stream 19 Phase 4: account_id + scan_id of the scan
  // the proposer just generated against. Threaded down to the Open-PR
  // button so the per-card POST carries the right (scan_id, step_idx,
  // account_id) tuple. Both are reset every time Inventory hands us
  // a new recommendations response.
  const [recsAccountID, setRecsAccountID] = useState<string>("");
  const [recsScanID, setRecsScanID] = useState<string>("");

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
          <TabsTrigger value={ACCOUNT_TAB}>Account</TabsTrigger>
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
              onRecommendations={(r, accountID, scanID) => {
                setRecs(r);
                setRecsAccountID(accountID);
                setRecsScanID(scanID);
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
        <Card
          role="status"
          className="border-yellow-500/50 bg-yellow-500/5"
        >
          <CardContent className="p-3 text-sm">
            <span className="font-medium">Partial scan-all:</span>{" "}
            {result.failed_accounts.length} of {result.total_accounts} accounts
            failed. The per-account cards below show the humanized error;
            re-run after addressing each.
          </CardContent>
        </Card>
      )}
      {error && (
        <Card>
          <CardContent
            role="alert"
            className="p-3 text-sm text-destructive"
          >
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
            {result.total_accounts === 1 ? "" : "s"} ·{" "}
            scan_all_id{" "}
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
          <Sparkles
            className="mt-0.5 h-4 w-4 text-violet-500"
            aria-hidden
          />
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
          recommendations are still generated per-scan via the proposer.
          Pick an account below to see Inventory + Recommendations work
          exactly as in the single-account view.
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
  const [open, setOpen] = useState(false);
  // resumeMode threads through to the wizard. The connections-list
  // "Resume an existing connection" entry point sets this true before
  // opening the dialog; the wizard then renders an "Existing
  // ExternalId (optional)" field on step 1 (#622, fix for #621).
  const [resumeMode, setResumeMode] = useState(false);

  const connections = data?.connections ?? [];

  const openWizardFresh = useCallback(() => {
    setResumeMode(false);
    setOpen(true);
  }, []);
  const openWizardWithResume = useCallback(() => {
    setResumeMode(true);
    setOpen(true);
  }, []);

  const onWizardComplete = useCallback(() => {
    // Refresh the SWR cache so the new connection card lands without a
    // page reload. The dialog auto-closes after a short delay so the
    // operator can read the wizard's success card.
    void mutate();
    setOpen(false);
    // Reset resumeMode so the next "Connect new account" click lands
    // on the standard fresh-UUID path.
    setResumeMode(false);
  }, [mutate]);

  const handleOpenChange = useCallback((next: boolean) => {
    setOpen(next);
    if (!next) setResumeMode(false);
  }, []);

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between">
        <div>
          <h2 className="text-base font-semibold">Connected accounts</h2>
          <p className="text-xs text-muted-foreground">
            One row per AWS account Squadron is configured to scan.
          </p>
        </div>
        <Dialog open={open} onOpenChange={handleOpenChange}>
          <div className="flex flex-col items-end gap-1">
            <Button onClick={openWizardFresh}>Connect new account</Button>
            {/* Secondary "resume an existing connection" entry point.
                Surfaces the #578 ExternalId-resume path from the
                connections list so an operator recovering from a
                previous deployment (Docker→local swap, reinstall,
                etc.) can paste their old UUID before the wizard
                generates a fresh one. #621 → #622. */}
            <Button
              variant="ghost"
              size="sm"
              className="h-auto p-0 text-xs text-muted-foreground hover:bg-transparent hover:text-foreground"
              onClick={openWizardWithResume}
            >
              {connections.length > 0
                ? "Resume an existing connection"
                : "Already have an ExternalId? Resume an existing connection"}
            </Button>
          </div>
          {/*
           * Height-bounded flex column so the wizard body scrolls inside
           * the viewport instead of clipping the dialog header and footer
           * on shorter screens (#620, same class as the IaC wizard fix
           * in #618). DialogContent itself has a max-h-[90vh] default from
           * the shared component, but the per-instance flex column with
           * a min-h-0 overflow-y-auto body keeps the DialogHeader pinned
           * at top while the wizard's internal Back/Next + step body
           * scroll together inside the bounded body section.
           */}
          <DialogContent className="flex max-w-2xl flex-col overflow-hidden">
            <DialogHeader className="shrink-0">
              <DialogTitle>Connect AWS account</DialogTitle>
              <DialogDescription>
                Walk through the five steps to grant Squadron read-only access
                via IAM assume-role.
              </DialogDescription>
            </DialogHeader>
            <div className="flex-1 min-h-0 overflow-y-auto pr-1">
              <ConnectorWizard
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
          </DialogContent>
        </Dialog>
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
            Click &quot;Connect new account&quot; to start.
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
  onRecommendations: (
    r: GenerateRecommendationsResponse,
    accountID: string,
    scanID: string,
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
      onRecommendations(r, result.account_id, result.scan_id);
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
            {result.compute.length} compute instances,{" "}
            {result.functions.length} functions,{" "}
            {(result.databases ?? []).length} databases,{" "}
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
              {generating ? "Generating recommendations…" : "Generate recommendations"}
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
            {result.partial_reason ?? "the walk did not cover the full inventory"}.
            Re-run to capture missed resources.
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
    </div>
  );
}

function ComputeSection({
  compute,
}: {
  compute: ScanResult["compute"];
}) {
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
                    <OtelBadge ok={c.has_otel} />
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
                  <OtelBadge ok={f.has_otel_layer} />
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
function ClustersSection({
  clusters,
}: {
  clusters: ScanResult["clusters"];
}) {
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
                  </div>
                  <div className="mt-2 flex flex-col gap-1">
                    <div className="flex flex-wrap items-center gap-1">
                      <span className="text-[10px] uppercase text-muted-foreground">
                        Control Plane Logging:
                      </span>
                      {c.control_plane_logging.length === 0 ? (
                        <Badge variant="outline" className="text-muted-foreground">
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
                        <Badge variant="outline" className="text-muted-foreground">
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

function RecommendationsTab({
  recs,
  accountID,
  scanID,
}: {
  recs: GenerateRecommendationsResponse | null;
  // v0.89.3 #603 Stream 19 Phase 4: scan + account context threaded
  // down to each card so the Open-PR button can address its POST.
  // Both empty before the first generate; populated after.
  accountID: string;
  scanID: string;
}) {
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
  const { data: iacData } = useSWR(
    IAC_GITHUB_CONNECTIONS_SWR_KEY,
    () => listIaCGitHubConnections(),
  );
  const iacConnections = iacData?.connections ?? [];

  if (!recs) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center gap-3 p-8 text-center">
          <Sparkles className="h-8 w-8 text-violet-500" aria-hidden />
          <div>
            <h3 className="text-base font-semibold">No recommendations yet.</h3>
            <p className="mt-2 max-w-md text-sm text-muted-foreground">
              Run a scan and click &quot;Generate recommendations&quot; from
              the Inventory tab.
            </p>
            <p className="mt-3 max-w-md text-xs text-muted-foreground">
              Recommendations arrive as Terraform snippets for your IaC
              pipeline — Squadron never mutates your cloud.
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
      <ul className="space-y-3">
        {recs.recommendations.map((rec, i) => (
          <li key={rec.id}>
            <DiscoveryRecommendationCard
              rec={rec}
              stepIdx={i}
              scanID={scanID || rec.source?.ref_id || ""}
              accountID={accountID}
              proposerReasoning={recs.reasoning ?? ""}
              iacConnections={iacConnections}
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
}) {
  const [iacOpen, setIacOpen] = useState(true);
  const [copied, setCopied] = useState(false);
  const [opening, setOpening] = useState(false);
  const [openPRResult, setOpenPRResult] =
    useState<IaCGitHubOpenPRResponse | null>(null);
  const [openPRError, setOpenPRError] =
    useState<IaCGitHubOpenPRError | null>(null);

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
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-2">
          <CardTitle className="text-base">{rec.title}</CardTitle>
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
                        <GitPullRequest
                          className="mr-1 h-3 w-3"
                          aria-hidden
                        />
                      )}
                      {opening ? "Opening PR…" : "Open PR"}
                    </Button>
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
                </div>

                {/* State B — connection exists, no placement row for
                    this resource_kind. Reachable when the operator
                    skipped this kind in the wizard or when a new
                    resource_kind ships after their connect. v0.89.4
                    (#610) deep-links the link target to auto-open
                    the wizard at the right placement row in one
                    click instead of dropping the operator on the
                    connections list. */}
                {connection &&
                  rec.resource_kind &&
                  !placementFilePath && (
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
          Squadron could not read the placement file. Confirm the path in
          your{" "}
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
