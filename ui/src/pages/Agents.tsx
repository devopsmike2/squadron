/**
 * Agents page — paginated + virtualized as of v0.23.
 *
 * Two architectural changes from the v0.21 version:
 *
 *   1. **Server-side filtering + pagination.** The filter bar (search,
 *      drift, status, group) now flows to /api/v1/agents query
 *      params, and we use SWR's infinite-loader to walk pages of
 *      100 items as the operator scrolls. At 1000+ agents this keeps
 *      both the network payload AND the time-to-first-paint bounded.
 *
 *   2. **Virtualization above 200 rows.** Small fleets render the
 *      grid/table directly — virtualization has measurable overhead
 *      that's not worth paying for 50 cards. At 200+ items, we swap
 *      in @tanstack/react-virtual to render only the rows actually
 *      in viewport.
 *
 * @tanstack/react-virtual was chosen over react-window because the
 * tanstack ecosystem is already in our deps (@tanstack/react-table),
 * its API maps cleanly to both row-virtualized tables and
 * row-virtualized grids (multi-card-per-row), and the bundle hit is
 * smaller than the legacy react-window.
 */

import { useVirtualizer } from "@tanstack/react-virtual";
import {
  AlertCircleIcon,
  LayoutGridIcon,
  Loader2Icon,
  RefreshCwIcon,
  RowsIcon,
  SearchIcon,
  ServerIcon,
  XIcon,
} from "lucide-react";
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useSearchParams } from "react-router-dom";
import useSWR from "swr";
import useSWRInfinite from "swr/infinite";

import {
  getAgents,
  type GetAgentsParams,
  type GetAgentsResponse,
} from "@/api/agents";
import { getGroups } from "@/api/groups";
import { AgentDetailsDrawer } from "@/components/AgentDetailsDrawer";
import { AgentCard } from "@/components/agents/AgentCard";
import { AskAIStrip } from "@/components/agents/AskAIStrip";
import { SavedFilters } from "@/components/agents/SavedFilters";
import { GroupDetailsDrawer } from "@/components/GroupDetailsDrawer";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { Agent, ConfigDriftStatus } from "@/types/agent";

// ============================================================
// Filter atoms
// ============================================================

type AgentStatus = Agent["status"];
type DriftFilter = ConfigDriftStatus | "any";
type StatusFilter = AgentStatus | "any";
type LayoutMode = "cards" | "table";

const LAYOUT_KEY = "squadron.agents.layout";

// Page size matches the server's defaultAgentsLimit. Bumping this
// would reduce round-trips on long scrolls but trade off first-paint
// time — operators often filter heavily and don't need the full
// page anyway.
const PAGE_SIZE = 100;

// Virtualization kicks in at this threshold. Below it, we render
// the full list directly because @tanstack/react-virtual carries
// a small per-render cost (measure-then-layout) that small lists
// would notice as jank, not benefit.
const VIRT_THRESHOLD = 200;

const DRIFT_OPTIONS: { value: DriftFilter; label: string; color: string }[] = [
  { value: "any", label: "All", color: "var(--muted-foreground)" },
  { value: "synced", label: "Synced", color: "var(--success)" },
  { value: "drifted", label: "Drifted", color: "var(--destructive)" },
  { value: "no_effective", label: "Awaiting", color: "var(--warning)" },
  { value: "no_intent", label: "No intent", color: "var(--muted-foreground)" },
];

const STATUS_OPTIONS: { value: StatusFilter; label: string }[] = [
  { value: "any", label: "All" },
  { value: "online", label: "Online" },
  { value: "offline", label: "Offline" },
  { value: "error", label: "Error" },
];

// ============================================================
// Paginated fetch hook
// ============================================================

/**
 * Wraps useSWRInfinite with our /agents query shape. Returns the
 * flattened items array plus pagination controls. The `params`
 * object becomes part of the SWR key so changing a filter blows
 * away the cached pages and refetches from the start — which is
 * what an operator expects when they type into search.
 */
function useAgentsPaginated(params: GetAgentsParams) {
  const getKey = (pageIndex: number, prev: GetAgentsResponse | null) => {
    // Stop fetching when the previous page returned everything.
    if (prev && prev.items.length === 0) return null;
    if (prev && pageIndex * PAGE_SIZE >= prev.total) return null;
    return [
      "/agents",
      pageIndex,
      params.drift_status ?? "",
      params.status ?? "",
      params.group_id ?? "",
      params.q ?? "",
    ];
  };

  const swr = useSWRInfinite<GetAgentsResponse>(
    getKey,
    ([, pageIndex, drift_status, status, group_id, q]) =>
      getAgents({
        offset: (pageIndex as number) * PAGE_SIZE,
        limit: PAGE_SIZE,
        drift_status: drift_status || undefined,
        status: status || undefined,
        group_id: group_id || undefined,
        q: q || undefined,
      }),
    {
      // Keep already-fetched pages in cache while a new filter is
      // being typed — the UI stays warm rather than flashing empty.
      keepPreviousData: true,
      revalidateFirstPage: false,
    },
  );

  const items: Agent[] = useMemo(
    () => (swr.data ?? []).flatMap((p) => p.items),
    [swr.data],
  );
  const total = swr.data?.[0]?.total ?? 0;
  const loadingMore =
    swr.isValidating &&
    swr.data !== undefined &&
    swr.data.length > 0 &&
    items.length < total;
  const reachedEnd = items.length >= total;

  return {
    items,
    total,
    loadingMore,
    reachedEnd,
    size: swr.size,
    setSize: swr.setSize,
    isLoading: swr.isLoading,
    error: swr.error,
    mutate: swr.mutate,
  };
}

// ============================================================
// Page
// ============================================================

export default function AgentsPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [refreshing, setRefreshing] = useState(false);
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [selectedGroupId, setSelectedGroupId] = useState<string | null>(null);
  const [groupDrawerOpen, setGroupDrawerOpen] = useState(false);

  // Drift filter is URL-bound so the Fleet Status dashboard donut
  // deeplinks work and the back button is sensible.
  const urlDrift = searchParams.get("drift_status") as DriftFilter | null;
  const drift: DriftFilter =
    urlDrift && DRIFT_OPTIONS.some((o) => o.value === urlDrift) ? urlDrift : "any";
  const setDrift = (next: DriftFilter) => {
    const sp = new URLSearchParams(searchParams);
    if (next === "any") sp.delete("drift_status");
    else sp.set("drift_status", next);
    setSearchParams(sp, { replace: true });
  };

  const [status, setStatus] = useState<StatusFilter>("any");
  const [groupId, setGroupId] = useState<string>("any");
  // Debounce search so we don't fire a query on every keystroke at
  // the API. 200ms is the sweet spot between "feels live" and "not
  // hammering the server while the operator types a long string".
  const [searchInput, setSearchInput] = useState("");
  const [search, setSearch] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setSearch(searchInput), 200);
    return () => clearTimeout(t);
  }, [searchInput]);

  const [layout, setLayout] = useState<LayoutMode>(() => {
    const stored = typeof window !== "undefined"
      ? localStorage.getItem(LAYOUT_KEY)
      : null;
    return stored === "table" ? "table" : "cards";
  });
  useEffect(() => {
    localStorage.setItem(LAYOUT_KEY, layout);
  }, [layout]);

  // Server-side filter params. Build once per filter set so
  // useAgentsPaginated's key only churns on real changes.
  const apiParams = useMemo<GetAgentsParams>(
    () => ({
      drift_status: drift !== "any" ? drift : undefined,
      status: status !== "any" ? status : undefined,
      // group_id="none" is a UI sentinel for "ungrouped"; the
      // server doesn't have that filter today so we fall back to
      // local filtering for that single case. Anything else is
      // server-side.
      group_id: groupId !== "any" && groupId !== "none" ? groupId : undefined,
      q: search.trim() || undefined,
    }),
    [drift, status, groupId, search],
  );

  const {
    items: serverItems,
    total: serverTotal,
    loadingMore,
    reachedEnd,
    size,
    setSize,
    isLoading,
    error: agentsError,
    mutate: mutateAgents,
  } = useAgentsPaginated(apiParams);

  // Apply the "ungrouped" sentinel locally on top of server results.
  const agents = useMemo(() => {
    if (groupId === "none") {
      return serverItems.filter((a) => !a.group_id);
    }
    return serverItems;
  }, [serverItems, groupId]);
  const total = groupId === "none" ? agents.length : serverTotal;

  const { data: groupsData } = useSWR("groups", getGroups, {
    refreshInterval: 30000,
  });

  // Fleet-wide summary numbers. Uses a separate small query so the
  // header counters stay accurate when the main grid is filtered.
  // limit=1 + total is the cheapest way to get a server-side count.
  const { data: fleetTotal } = useSWR(
    "/agents-total",
    () => getAgents({ limit: 1 }).then((r) => r.total),
    { refreshInterval: 60000 },
  );
  const { data: fleetOnline } = useSWR(
    "/agents-total-online",
    () => getAgents({ status: "online", limit: 1 }).then((r) => r.total),
    { refreshInterval: 60000 },
  );
  const { data: fleetDrifted } = useSWR(
    "/agents-total-drifted",
    () => getAgents({ drift_status: "drifted", limit: 1 }).then((r) => r.total),
    { refreshInterval: 60000 },
  );

  const handleRefresh = async () => {
    setRefreshing(true);
    await mutateAgents();
    setRefreshing(false);
  };

  const groupIdToName = useMemo(() => {
    const map: Record<string, string> = {};
    for (const g of groupsData?.groups ?? []) map[g.id] = g.name;
    return map;
  }, [groupsData]);

  const openAgent = (id: string) => {
    setSelectedAgentId(id);
    setDrawerOpen(true);
  };

  const openGroup = (id: string) => {
    setSelectedGroupId(id);
    setGroupDrawerOpen(true);
  };

  if (agentsError) {
    return (
      <div className="container mx-auto p-6">
        <div className="text-center">
          <h1 className="text-2xl font-semibold text-destructive">
            Couldn't load agents
          </h1>
          <p className="mt-2 text-muted-foreground">
            {agentsError?.message || "Failed to load agent data"}
          </p>
          <Button onClick={handleRefresh} className="mt-4">
            <RefreshCwIcon className="mr-2 h-4 w-4" /> Retry
          </Button>
        </div>
      </div>
    );
  }

  const hasAnyFilter =
    drift !== "any" ||
    status !== "any" ||
    groupId !== "any" ||
    search.trim() !== "";
  const clearAll = () => {
    setDrift("any");
    setStatus("any");
    setGroupId("any");
    setSearchInput("");
    setSearch("");
  };

  // Saved-filter integration: flatten the four filter atoms into a
  // single params object that SavedFilters can store and recall.
  // Empty strings serialize away on the SavedFilters side via
  // paramsMatch, so two combos with the same effective filter set
  // compare equal even if one has explicit empties.
  const filterParams: Record<string, string> = {
    drift: drift === "any" ? "" : drift,
    status: status === "any" ? "" : status,
    group_id: groupId === "any" ? "" : groupId,
    q: search.trim(),
  };
  const applySaved = (params: Record<string, string>) => {
    const d = (params.drift || "any") as DriftFilter;
    if (DRIFT_OPTIONS.some((o) => o.value === d)) setDrift(d);
    const s = (params.status || "any") as StatusFilter;
    if (STATUS_OPTIONS.some((o) => o.value === s)) setStatus(s);
    setGroupId(params.group_id || "any");
    setSearchInput(params.q || "");
    setSearch(params.q || "");
  };

  // Infinite-scroll: when the bottom sentinel comes into view, ask
  // SWR for the next page. The handler is shared between the
  // virtualized + non-virtualized paths.
  const onNeedMore = useCallback(() => {
    if (loadingMore || reachedEnd) return;
    setSize(size + 1);
  }, [loadingMore, reachedEnd, setSize, size]);

  return (
    <div className="flex flex-col gap-4">
      {/* Page header */}
      <header className="flex items-end justify-between gap-4">
        <div>
          <div className="text-[10px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
            Squadron
          </div>
          <h1 className="text-2xl font-semibold tracking-tight text-foreground">
            Agents
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            {fleetTotal !== undefined && fleetTotal > 0 ? (
              <>
                <span className="font-tabular text-foreground">
                  {fleetOnline ?? "—"}/{fleetTotal}
                </span>{" "}
                reporting
                {fleetDrifted !== undefined && fleetDrifted > 0 && (
                  <>
                    {" · "}
                    <span
                      className="font-tabular"
                      style={{ color: "var(--destructive)" }}
                    >
                      {fleetDrifted} drifted
                    </span>
                  </>
                )}
              </>
            ) : (
              "No agents reporting yet"
            )}
          </p>
        </div>

        <div className="flex items-center gap-2">
          <Tabs
            value={layout}
            onValueChange={(v) => setLayout(v as LayoutMode)}
          >
            <TabsList>
              <TabsTrigger value="cards" className="gap-1.5">
                <LayoutGridIcon className="h-3.5 w-3.5" />
                <span>Cards</span>
              </TabsTrigger>
              <TabsTrigger value="table" className="gap-1.5">
                <RowsIcon className="h-3.5 w-3.5" />
                <span>Table</span>
              </TabsTrigger>
            </TabsList>
          </Tabs>
          <Button
            variant="outline"
            size="sm"
            onClick={handleRefresh}
            disabled={refreshing}
          >
            <RefreshCwIcon
              className={`mr-2 h-3.5 w-3.5 ${refreshing ? "animate-spin" : ""}`}
            />
            Refresh
          </Button>
        </div>
      </header>

      {/* v0.44 — natural language fleet query. The model translates
          plain English into the same filter atoms the SavedFilters
          strip stores, so applying one is exactly like clicking a
          saved chip. Hidden entirely when AI is disabled. */}
      <AskAIStrip
        labelKeys={Array.from(
          new Set(
            agents.flatMap((a) => Object.keys(a.labels ?? {})),
          ),
        ).slice(0, 30)}
        groupNames={(groupsData?.groups ?? []).map((g) => g.name)}
        onApply={(resp) => {
          if (resp.drift_status) setDrift(resp.drift_status);
          if (resp.status) setStatus(resp.status);
          if (resp.group_id) setGroupId(resp.group_id);
          if (resp.q !== undefined) {
            setSearchInput(resp.q);
            setSearch(resp.q);
          }
        }}
      />

      {/* Saved filter chips. v0.38 — operators name the questions
          they ask every day and recall them with one click. */}
      <SavedFilters
        scope="agents"
        currentParams={filterParams}
        onApply={applySaved}
        hasAnyFilter={hasAnyFilter}
      />

      {/* Filter bar */}
      <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-card/60 px-3 py-2 backdrop-blur">
        <div className="relative">
          <SearchIcon className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground/70" />
          <Input
            placeholder="Search by name, ID, or label…"
            value={searchInput}
            onChange={(e) => setSearchInput(e.target.value)}
            className="h-8 w-72 pl-8 text-sm"
          />
        </div>

        <div className="ml-2 flex items-center gap-1">
          {DRIFT_OPTIONS.map((o) => {
            const active = drift === o.value;
            return (
              <button
                key={o.value}
                type="button"
                onClick={() => setDrift(o.value)}
                className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors ${
                  active
                    ? "border-current/40 bg-current/10"
                    : "border-border bg-transparent text-muted-foreground hover:bg-accent/40"
                }`}
                style={
                  active
                    ? ({ color: o.color } as React.CSSProperties)
                    : undefined
                }
              >
                {o.value !== "any" && (
                  <span
                    className="status-dot"
                    style={{ ["--dot" as string]: o.color }}
                  />
                )}
                <span className={active ? "text-foreground" : ""}>
                  {o.label}
                </span>
              </button>
            );
          })}
        </div>

        <Select
          value={status}
          onValueChange={(v) => setStatus(v as StatusFilter)}
        >
          <SelectTrigger className="h-8 w-32 text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {STATUS_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                Status: {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={groupId} onValueChange={setGroupId}>
          <SelectTrigger className="h-8 w-44 text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="any">Group: All</SelectItem>
            <SelectItem value="none">Group: Ungrouped</SelectItem>
            {groupsData?.groups?.map((g) => (
              <SelectItem key={g.id} value={g.id}>
                Group: {g.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        {hasAnyFilter && (
          <button
            type="button"
            onClick={clearAll}
            className="ml-auto inline-flex items-center gap-1 rounded-md border border-border bg-transparent px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent/40 hover:text-foreground"
          >
            <XIcon className="h-3 w-3" />
            Clear filters
          </button>
        )}
      </div>

      <div className="-mb-1 flex items-center justify-between text-xs text-muted-foreground">
        <span>
          Showing{" "}
          <span className="font-tabular text-foreground">{agents.length}</span>
          {agents.length < total ? (
            <> of {total}</>
          ) : null}
          {fleetTotal !== undefined && total < fleetTotal ? (
            <> · {fleetTotal} in fleet</>
          ) : null}
        </span>
        {agents.length >= VIRT_THRESHOLD && (
          <span className="font-tabular text-[10px] uppercase tracking-wider text-muted-foreground/70">
            Virtualized
          </span>
        )}
      </div>

      {isLoading && agents.length === 0 ? (
        <LoadingState />
      ) : agents.length === 0 ? (
        <EmptyState
          hasFilters={hasAnyFilter}
          totalAgents={fleetTotal ?? 0}
          onClearFilters={clearAll}
        />
      ) : layout === "cards" ? (
        agents.length >= VIRT_THRESHOLD ? (
          <VirtualizedCardGrid
            agents={agents}
            groupIdToName={groupIdToName}
            onAgentClick={openAgent}
            onGroupClick={openGroup}
            onNeedMore={onNeedMore}
            loadingMore={loadingMore}
          />
        ) : (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
            {agents.map((a) => (
              <AgentCard
                key={a.id}
                agent={a}
                groupName={a.group_id ? groupIdToName[a.group_id] : undefined}
                onClick={() => openAgent(a.id)}
                onGroupClick={openGroup}
              />
            ))}
          </div>
        )
      ) : agents.length >= VIRT_THRESHOLD ? (
        <VirtualizedAgentTable
          agents={agents}
          groupIdToName={groupIdToName}
          onAgentClick={openAgent}
          onGroupClick={openGroup}
          onNeedMore={onNeedMore}
          loadingMore={loadingMore}
        />
      ) : (
        <AgentTable
          agents={agents}
          groupIdToName={groupIdToName}
          onAgentClick={openAgent}
          onGroupClick={openGroup}
        />
      )}

      {/* Bottom sentinel: non-virtualized path triggers next page
          when this comes into view. Virtualized paths handle their
          own onNeedMore via the scroll listener. */}
      {agents.length < VIRT_THRESHOLD && !reachedEnd && (
        <NonVirtualSentinel
          onVisible={onNeedMore}
          loading={loadingMore}
        />
      )}

      <AgentDetailsDrawer
        agentId={selectedAgentId}
        open={drawerOpen}
        onOpenChange={setDrawerOpen}
      />

      <GroupDetailsDrawer
        groupId={selectedGroupId}
        open={groupDrawerOpen}
        onOpenChange={setGroupDrawerOpen}
      />
    </div>
  );
}

// ============================================================
// Virtualized card grid
//
// We virtualize rows-of-cards rather than individual cards because
// the grid layout reflows on viewport width — virtualizing each
// card would mean recomputing positions on every resize. Rows are
// stable: N cards per row × ceil(items/N) rows.
// ============================================================

function useCardsPerRow(): number {
  // Mirror Tailwind's grid breakpoints from the non-virtualized
  // path: 1 / 2 / 3 / 4 columns at sm/lg/xl. Measured on resize so
  // virtualization recomputes row counts when the operator drags
  // the window.
  const [n, setN] = useState(() => columnsForWidth(window.innerWidth));
  useEffect(() => {
    const onResize = () => setN(columnsForWidth(window.innerWidth));
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  return n;
}

function columnsForWidth(w: number): number {
  if (w >= 1280) return 4; // xl
  if (w >= 1024) return 3; // lg
  if (w >= 640) return 2; // sm
  return 1;
}

interface VirtualGridProps {
  agents: Agent[];
  groupIdToName: Record<string, string>;
  onAgentClick: (id: string) => void;
  onGroupClick: (id: string) => void;
  onNeedMore: () => void;
  loadingMore: boolean;
}

function VirtualizedCardGrid({
  agents,
  groupIdToName,
  onAgentClick,
  onGroupClick,
  onNeedMore,
  loadingMore,
}: VirtualGridProps) {
  const cols = useCardsPerRow();
  const rows = Math.ceil(agents.length / cols);
  const parentRef = useRef<HTMLDivElement>(null);

  const virt = useVirtualizer({
    count: rows + (loadingMore ? 1 : 0),
    getScrollElement: () => parentRef.current,
    estimateSize: () => 168, // card height (~150) + grid gap (12) + padding fudge
    overscan: 4,
  });

  // Trigger next-page fetch when the user scrolls within ~3 rows
  // of the bottom. Cheap: just compare the highest virtualized
  // index against total.
  useEffect(() => {
    const items = virt.getVirtualItems();
    if (items.length === 0) return;
    const last = items[items.length - 1];
    if (last.index >= rows - 3) {
      onNeedMore();
    }
  }, [virt, rows, onNeedMore]);

  return (
    <div
      ref={parentRef}
      className="relative h-[calc(100vh-280px)] min-h-[400px] overflow-auto rounded-lg"
    >
      <div
        style={{
          height: virt.getTotalSize(),
          position: "relative",
          width: "100%",
        }}
      >
        {virt.getVirtualItems().map((vRow) => {
          const start = vRow.index * cols;
          const slice = agents.slice(start, start + cols);
          const isLoaderRow = vRow.index >= rows;
          return (
            <div
              key={vRow.key}
              data-index={vRow.index}
              ref={virt.measureElement}
              style={{
                position: "absolute",
                top: 0,
                left: 0,
                width: "100%",
                transform: `translateY(${vRow.start}px)`,
              }}
            >
              {isLoaderRow ? (
                <div className="flex items-center justify-center py-4 text-xs text-muted-foreground">
                  <Loader2Icon className="mr-2 h-3 w-3 animate-spin" />
                  Loading more…
                </div>
              ) : (
                <div
                  className="grid gap-3 pb-3"
                  style={{
                    gridTemplateColumns: `repeat(${cols}, minmax(0, 1fr))`,
                  }}
                >
                  {slice.map((a) => (
                    <AgentCard
                      key={a.id}
                      agent={a}
                      groupName={
                        a.group_id ? groupIdToName[a.group_id] : undefined
                      }
                      onClick={() => onAgentClick(a.id)}
                      onGroupClick={onGroupClick}
                    />
                  ))}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

// ============================================================
// Virtualized + non-virtualized table fallbacks
// ============================================================

function VirtualizedAgentTable({
  agents,
  groupIdToName,
  onAgentClick,
  onGroupClick,
  onNeedMore,
  loadingMore,
}: VirtualGridProps) {
  const parentRef = useRef<HTMLDivElement>(null);
  const virt = useVirtualizer({
    count: agents.length + (loadingMore ? 1 : 0),
    getScrollElement: () => parentRef.current,
    estimateSize: () => 56, // row height
    overscan: 8,
  });

  useEffect(() => {
    const items = virt.getVirtualItems();
    if (items.length === 0) return;
    const last = items[items.length - 1];
    if (last.index >= agents.length - 10) {
      onNeedMore();
    }
  }, [virt, agents.length, onNeedMore]);

  return (
    <div
      ref={parentRef}
      className="h-[calc(100vh-280px)] min-h-[400px] overflow-auto rounded-lg border border-border bg-card/60 backdrop-blur"
    >
      {/* Header is sticky so column labels stay put while the body
          scrolls. The virtualized rows sit inside a relative
          spacer; matching column widths between header and rows
          uses fixed widths to avoid full-row remeasurement. */}
      <div
        className="sticky top-0 z-10 grid border-b border-border bg-card/95 backdrop-blur"
        style={{ gridTemplateColumns: TABLE_COLS }}
      >
        {TABLE_HEADS.map((h) => (
          <div
            key={h}
            className="px-4 py-2 text-left text-xs font-medium text-muted-foreground"
          >
            {h}
          </div>
        ))}
      </div>
      <div style={{ height: virt.getTotalSize(), position: "relative" }}>
        {virt.getVirtualItems().map((vr) => {
          const isLoader = vr.index >= agents.length;
          const a = agents[vr.index];
          return (
            <div
              key={vr.key}
              data-index={vr.index}
              ref={virt.measureElement}
              style={{
                position: "absolute",
                top: 0,
                left: 0,
                width: "100%",
                transform: `translateY(${vr.start}px)`,
              }}
            >
              {isLoader ? (
                <div className="flex items-center justify-center py-3 text-xs text-muted-foreground">
                  <Loader2Icon className="mr-2 h-3 w-3 animate-spin" />
                  Loading more…
                </div>
              ) : (
                <button
                  type="button"
                  onClick={() => onAgentClick(a.id)}
                  className="grid w-full cursor-pointer items-center border-b border-border/40 px-0 py-0 text-left hover:bg-accent/30"
                  style={{ gridTemplateColumns: TABLE_COLS }}
                >
                  <div className="px-4 py-3">
                    <StatusBadge status={a.status} />
                  </div>
                  <div className="px-4 py-3">
                    <DriftBadge status={a.drift_status} />
                  </div>
                  <div className="px-4 py-3 truncate font-medium text-foreground">
                    {a.name}
                  </div>
                  <div className="px-4 py-3 font-tabular text-xs text-muted-foreground">
                    {a.version}
                  </div>
                  <div className="px-4 py-3">
                    {a.group_id ? (
                      <span
                        onClick={(e) => {
                          e.stopPropagation();
                          onGroupClick(a.group_id!);
                        }}
                        className="text-primary hover:underline"
                      >
                        {groupIdToName[a.group_id] || a.group_id.slice(0, 8)}
                      </span>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </div>
                  <div className="px-4 py-3 font-tabular text-xs text-muted-foreground">
                    {new Date(a.last_seen).toLocaleString()}
                  </div>
                </button>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

const TABLE_COLS = "112px 128px 1fr 96px 192px 192px";
const TABLE_HEADS = ["Status", "Drift", "Name", "Version", "Group", "Last Seen"];

function AgentTable({
  agents,
  groupIdToName,
  onAgentClick,
  onGroupClick,
}: {
  agents: Agent[];
  groupIdToName: Record<string, string>;
  onAgentClick: (id: string) => void;
  onGroupClick: (id: string) => void;
}) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-card/60 backdrop-blur">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-24">Status</TableHead>
            <TableHead className="w-32">Drift</TableHead>
            <TableHead>Name</TableHead>
            <TableHead className="w-24">Version</TableHead>
            <TableHead className="w-40">Group</TableHead>
            <TableHead className="w-40">Last Seen</TableHead>
            <TableHead>Labels</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {agents.map((a) => (
            <TableRow
              key={a.id}
              className="cursor-pointer"
              onClick={() => onAgentClick(a.id)}
            >
              <TableCell>
                <StatusBadge status={a.status} />
              </TableCell>
              <TableCell>
                <DriftBadge status={a.drift_status} />
              </TableCell>
              <TableCell className="font-medium">{a.name}</TableCell>
              <TableCell className="font-tabular text-xs text-muted-foreground">
                {a.version}
              </TableCell>
              <TableCell>
                {a.group_id ? (
                  <button
                    type="button"
                    onClick={(e) => {
                      e.stopPropagation();
                      onGroupClick(a.group_id!);
                    }}
                    className="text-primary hover:underline"
                  >
                    {groupIdToName[a.group_id] || a.group_id.slice(0, 8)}
                  </button>
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              <TableCell
                className="font-tabular text-xs text-muted-foreground"
                title={new Date(a.last_seen).toLocaleString()}
              >
                {new Date(a.last_seen).toLocaleString()}
              </TableCell>
              <TableCell>
                <div className="flex flex-wrap gap-1">
                  {Object.entries(a.labels ?? {})
                    .slice(0, 3)
                    .map(([k, v]) => (
                      <Badge
                        key={k}
                        variant="outline"
                        className="font-mono text-[10px]"
                      >
                        {k}={v}
                      </Badge>
                    ))}
                  {Object.keys(a.labels ?? {}).length > 3 && (
                    <span className="text-[10px] text-muted-foreground">
                      +{Object.keys(a.labels ?? {}).length - 3}
                    </span>
                  )}
                </div>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

// ============================================================
// Helpers
// ============================================================

function StatusBadge({ status }: { status: Agent["status"] }) {
  const color =
    status === "online"
      ? "var(--success)"
      : status === "error"
        ? "var(--destructive)"
        : "var(--muted-foreground)";
  return (
    <span className="inline-flex items-center gap-1.5">
      <span
        className="status-dot"
        style={{ ["--dot" as string]: color }}
      />
      <span className="text-xs capitalize text-foreground">{status}</span>
    </span>
  );
}

function DriftBadge({ status }: { status?: ConfigDriftStatus }) {
  const meta = {
    synced: { label: "Synced", color: "var(--success)" },
    drifted: { label: "Drifted", color: "var(--destructive)" },
    no_intent: { label: "No intent", color: "var(--muted-foreground)" },
    no_effective: { label: "Awaiting", color: "var(--warning)" },
    unknown: { label: "Unknown", color: "var(--muted-foreground)" },
  }[status ?? "unknown"];
  return (
    <Badge
      variant="outline"
      className="text-[10px] uppercase tracking-wider"
      style={{
        color: meta.color,
        borderColor: `color-mix(in oklch, ${meta.color} 40%, transparent)`,
        background: `color-mix(in oklch, ${meta.color} 10%, transparent)`,
      }}
    >
      {meta.label}
    </Badge>
  );
}

// Sentinel for the non-virtualized path. We can't rely on
// IntersectionObserver with the default viewport root because the
// Layout wraps the page content in an overflow-auto container —
// the sentinel can be visible inside that container but never
// intersects the document viewport in a way the default observer
// notices. Instead we attach a scroll listener to the actual
// scrolling ancestor and fire onVisible when the user is within
// `triggerDistance` pixels of the bottom.
function NonVirtualSentinel({
  onVisible,
  loading,
}: {
  onVisible: () => void;
  loading: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  // Keep onVisible in a ref so the scroll listener can call the
  // latest version without re-attaching every render (which would
  // miss in-flight scroll events).
  const onVisibleRef = useRef(onVisible);
  onVisibleRef.current = onVisible;

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    // Walk up the DOM to find the first overflow-y:auto|scroll
    // ancestor — that's the layout's main scroll container.
    let scroller: HTMLElement | Window = window;
    let p: HTMLElement | null = el.parentElement;
    while (p) {
      const style = getComputedStyle(p);
      if (
        (style.overflowY === "auto" || style.overflowY === "scroll") &&
        p.scrollHeight > p.clientHeight
      ) {
        scroller = p;
        break;
      }
      p = p.parentElement;
    }

    const triggerDistance = 400; // px from the bottom
    const onScroll = () => {
      const rect = el.getBoundingClientRect();
      // The sentinel is "approaching the bottom" when its top is
      // within the viewport (or scroller viewport) minus the
      // trigger distance.
      const viewportBottom =
        scroller === window
          ? window.innerHeight
          : (scroller as HTMLElement).getBoundingClientRect().bottom;
      if (rect.top - triggerDistance <= viewportBottom) {
        onVisibleRef.current();
      }
    };
    // Run once immediately in case the sentinel is already near
    // the bottom on mount (small fleets, or after a filter
    // narrows the list).
    onScroll();
    scroller.addEventListener("scroll", onScroll, { passive: true });
    return () => scroller.removeEventListener("scroll", onScroll);
  }, []);

  return (
    <div
      ref={ref}
      className="flex items-center justify-center py-4 text-xs text-muted-foreground"
    >
      {loading ? (
        <>
          <Loader2Icon className="mr-2 h-3 w-3 animate-spin" />
          Loading more…
        </>
      ) : (
        <span className="opacity-0">Loading…</span>
      )}
    </div>
  );
}

function LoadingState() {
  return (
    <div className="flex items-center justify-center rounded-lg border border-dashed border-border/60 bg-card/40 px-6 py-12 text-sm text-muted-foreground">
      <Loader2Icon className="mr-2 h-4 w-4 animate-spin" />
      Loading agents…
    </div>
  );
}

function EmptyState({
  hasFilters,
  totalAgents,
  onClearFilters,
}: {
  hasFilters: boolean;
  totalAgents: number;
  onClearFilters: () => void;
}) {
  return (
    <div className="rounded-lg border border-dashed border-border/60 bg-card/40 px-6 py-16 text-center">
      {hasFilters ? (
        <>
          <AlertCircleIcon className="mx-auto h-10 w-10 text-muted-foreground/40" />
          <p className="mt-3 text-sm font-medium text-foreground">
            No agents match the current filters
          </p>
          <p className="mt-1 text-xs text-muted-foreground">
            Try widening the search or clearing filters to see the rest of the
            fleet.
          </p>
          <Button
            variant="outline"
            size="sm"
            onClick={onClearFilters}
            className="mt-4"
          >
            Clear filters
          </Button>
        </>
      ) : (
        <>
          <ServerIcon className="mx-auto h-10 w-10 text-muted-foreground/40" />
          <p className="mt-3 text-sm font-medium text-foreground">
            {totalAgents === 0
              ? "No agents reporting yet"
              : "No agents found"}
          </p>
          <p className="mt-1 text-xs text-muted-foreground">
            Agents appear here automatically when they connect to Squadron's
            OpAMP endpoint.
          </p>
        </>
      )}
    </div>
  );
}
