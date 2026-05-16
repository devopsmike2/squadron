/**
 * Agents page — the highest-traffic page in Squadron.
 *
 * v0.21 reframes this from "a database table" to "a fleet view":
 * cards by default, table on demand. Card-grid is the right primary
 * because operators almost always come here looking for "is anything
 * wrong?", which a wall of color-coded tiles answers in a glance —
 * tables only beat cards once you're scanning for specific text.
 *
 * Filter bar above the grid handles the four scopes operators
 * actually use: search by name/label, drift state, status, group.
 * The drift filter is wired to the URL (?drift_status=) so the
 * Fleet Status dashboard donut and the Drifted hero card both
 * deeplink directly into a filtered view.
 */

import {
  AlertCircleIcon,
  LayoutGridIcon,
  RefreshCwIcon,
  RowsIcon,
  SearchIcon,
  ServerIcon,
  XIcon,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import useSWR from "swr";

import { getAgents } from "@/api/agents";
import { getGroups } from "@/api/groups";
import { AgentDetailsDrawer } from "@/components/AgentDetailsDrawer";
import { AgentCard } from "@/components/agents/AgentCard";
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

  // Other filters stay in-page. They're noise to URL-bind because
  // operators rarely deeplink a name search.
  const [status, setStatus] = useState<StatusFilter>("any");
  const [groupId, setGroupId] = useState<string>("any");
  const [search, setSearch] = useState("");

  // Layout preference persists per-operator. localStorage keeps the
  // choice across reloads without bothering the server.
  const [layout, setLayout] = useState<LayoutMode>(() => {
    const stored = typeof window !== "undefined"
      ? localStorage.getItem(LAYOUT_KEY)
      : null;
    return stored === "table" ? "table" : "cards";
  });
  useEffect(() => {
    localStorage.setItem(LAYOUT_KEY, layout);
  }, [layout]);

  const {
    data: agentsData,
    error: agentsError,
    mutate: mutateAgents,
  } = useSWR("agents", getAgents, { refreshInterval: 30000 });

  const { data: groupsData } = useSWR("groups", getGroups, {
    refreshInterval: 30000,
  });

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

  const allAgents: Agent[] = useMemo(
    () => (agentsData?.agents ? Object.values(agentsData.agents) : []),
    [agentsData],
  );

  const agents = useMemo(() => {
    const q = search.trim().toLowerCase();
    return allAgents.filter((a) => {
      if (status !== "any" && a.status !== status) return false;
      if (drift !== "any") {
        const d = a.drift_status ?? "unknown";
        if (d !== drift) return false;
      }
      if (groupId !== "any") {
        if (groupId === "none") {
          if (a.group_id) return false;
        } else if (a.group_id !== groupId) return false;
      }
      if (q) {
        const haystack = [
          a.name,
          a.id,
          ...Object.entries(a.labels ?? {}).map(([k, v]) => `${k}=${v}`),
        ]
          .join(" ")
          .toLowerCase();
        if (!haystack.includes(q)) return false;
      }
      return true;
    });
  }, [allAgents, status, drift, groupId, search]);

  // Sort by online-first, then drifted-second, then alpha. Operators
  // care about anything-wrong-first; the rest is alpha for muscle memory.
  const sortedAgents = useMemo(() => {
    const rank = (a: Agent) => {
      if (a.drift_status === "drifted") return 0;
      if (a.status === "error") return 1;
      if (a.status === "offline") return 2;
      return 3;
    };
    return [...agents].sort((a, b) => {
      const r = rank(a) - rank(b);
      if (r !== 0) return r;
      return a.name.localeCompare(b.name);
    });
  }, [agents]);

  const openAgent = (id: string) => {
    setSelectedAgentId(id);
    setDrawerOpen(true);
  };

  const openGroup = (id: string) => {
    setSelectedGroupId(id);
    setGroupDrawerOpen(true);
  };

  // Header counts derived from unfiltered data so the "1 drifted"
  // chip count reflects fleet reality, not what's currently filtered.
  const fleetSummary = useMemo(() => {
    let online = 0;
    let drifted = 0;
    for (const a of allAgents) {
      if (a.status === "online") online++;
      if (a.drift_status === "drifted") drifted++;
    }
    return { total: allAgents.length, online, drifted };
  }, [allAgents]);

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
    setSearch("");
  };

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
            {fleetSummary.total > 0 ? (
              <>
                <span className="font-tabular text-foreground">
                  {fleetSummary.online}/{fleetSummary.total}
                </span>{" "}
                reporting
                {fleetSummary.drifted > 0 && (
                  <>
                    {" · "}
                    <span
                      className="font-tabular"
                      style={{ color: "var(--destructive)" }}
                    >
                      {fleetSummary.drifted} drifted
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

      {/* Filter bar */}
      <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-card/60 px-3 py-2 backdrop-blur">
        <div className="relative">
          <SearchIcon className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground/70" />
          <Input
            placeholder="Search by name, ID, or label…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="h-8 w-72 pl-8 text-sm"
          />
        </div>

        {/* Drift filter chips */}
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

        {/* Status select */}
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

        {/* Group select */}
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

      {/* Result count */}
      <div className="-mb-1 flex items-center justify-between text-xs text-muted-foreground">
        <span>
          Showing{" "}
          <span className="font-tabular text-foreground">
            {sortedAgents.length}
          </span>{" "}
          of {fleetSummary.total}
        </span>
      </div>

      {/* Body: cards or table */}
      {sortedAgents.length === 0 ? (
        <EmptyState
          hasFilters={hasAnyFilter}
          totalAgents={fleetSummary.total}
          onClearFilters={clearAll}
        />
      ) : layout === "cards" ? (
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {sortedAgents.map((a) => (
            <AgentCard
              key={a.id}
              agent={a}
              groupName={a.group_id ? groupIdToName[a.group_id] : undefined}
              onClick={() => openAgent(a.id)}
              onGroupClick={openGroup}
            />
          ))}
        </div>
      ) : (
        <AgentTable
          agents={sortedAgents}
          groupIdToName={groupIdToName}
          onAgentClick={openAgent}
          onGroupClick={openGroup}
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
// Table fallback
// ============================================================

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

// ============================================================
// Empty state
// ============================================================

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
