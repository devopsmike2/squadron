/**
 * Fleet Map — three-in-one visualization of how the squadron is
 * wired.
 *
 *   Pipeline   — what each collector actually does, end-to-end.
 *   Data Flow  — where every byte of telemetry leaves the fleet.
 *   Fleet      — the agents-and-groups physical view.
 *
 * Replaces the old "Topology" page which was just two cards floating
 * on a canvas with no edges. Topology lives on at /topology as an
 * alias for back-compat with bookmarked URLs.
 */

import { ReactFlowProvider } from "@xyflow/react";
import * as yaml from "js-yaml";
import {
  GitForkIcon,
  RefreshCwIcon,
  ShipIcon,
  Workflow,
} from "lucide-react";
import { useCallback, useState } from "react";
import useSWR from "swr";

import { getAgents } from "@/api/agents";
import { getTopology } from "@/api/topology";
import { AgentDetailsDrawer } from "@/components/AgentDetailsDrawer";
import { CollectorPipelineView } from "@/components/collector-pipeline";
import { DataFlowCanvas } from "@/components/fleet-map/DataFlowCanvas";
import type { Signal } from "@/components/fleet-map/exporter-parser";
import { GroupDetailsDrawer } from "@/components/GroupDetailsDrawer";
import {
  TopologyCanvas,
  DisplaySidebar,
  useTopologyLayout,
  type TopologyNode,
} from "@/components/topology";
import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { Agent } from "@/types/agent";

type FleetView = "pipeline" | "data-flow" | "fleet";

// ============================================================
// Signal filter chip — used in both Data Flow and (eventually)
// Pipeline view. Toggling rounds down the visible edges.
// ============================================================

const SIGNAL_META: Record<Signal, { label: string; color: string }> = {
  traces: { label: "Traces", color: "var(--chart-1)" },
  metrics: { label: "Metrics", color: "var(--chart-2)" },
  logs: { label: "Logs", color: "var(--chart-3)" },
};

function SignalChip({
  signal,
  active,
  onToggle,
}: {
  signal: Signal;
  active: boolean;
  onToggle: () => void;
}) {
  const meta = SIGNAL_META[signal];
  return (
    <button
      type="button"
      onClick={onToggle}
      className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors ${
        active
          ? "border-current/40 bg-current/10 text-foreground"
          : "border-border bg-transparent text-muted-foreground hover:bg-accent/40"
      }`}
      style={
        active ? ({ ["color" as string]: meta.color } as React.CSSProperties) : undefined
      }
    >
      <span
        className="status-dot"
        style={{ ["--dot" as string]: meta.color }}
      />
      <span className={active ? "text-foreground" : ""}>{meta.label}</span>
    </button>
  );
}

// ============================================================
// Page
// ============================================================

export default function FleetMapPage() {
  const [view, setView] = useState<FleetView>("pipeline");
  const [refreshing, setRefreshing] = useState(false);
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [selectedGroupId, setSelectedGroupId] = useState<string | null>(null);
  const [groupDrawerOpen, setGroupDrawerOpen] = useState(false);
  const [topologyLevel, setTopologyLevel] = useState<"instance" | "group">(
    "instance",
  );

  // Signal filter for the Data Flow tab. Defaults to "all on" because
  // most operators want the full picture when they land; toggling
  // rounds down. Persisted across tab switches but not in storage —
  // tab is the natural UI for "give me a different cut".
  const [signalFilter, setSignalFilter] = useState<Set<Signal>>(
    new Set(["traces", "metrics", "logs"]),
  );

  const {
    data: topologyData,
    error,
    mutate: mutateTopology,
  } = useSWR("topology", getTopology, { refreshInterval: 30000 });

  // The Data Flow tab needs full agent objects (effective_config) —
  // /topology returns a slim projection. /agents has what we need
  // and is already cached by the agents page.
  const { data: agentsResp, mutate: mutateAgents } = useSWR(
    "/agents",
    () => getAgents(),
    { refreshInterval: 30000 },
  );
  const agents: Agent[] = agentsResp ? Object.values(agentsResp.agents) : [];

  const { nodes, edges } = useTopologyLayout(topologyData, topologyLevel);

  const handleRefresh = async () => {
    setRefreshing(true);
    await Promise.all([mutateTopology(), mutateAgents()]);
    setRefreshing(false);
  };

  // Pipeline tab needs to know which agent the operator picked. The
  // DisplaySidebar's onNodeSelect doubles as our selection handler.
  const handleNodeSelect = useCallback((node: TopologyNode) => {
    if (node.type === "agent") {
      setSelectedAgentId(node.id);
      // Stay on the current tab — selection is shared. The drawer
      // is only summoned by explicit canvas clicks (handled below)
      // because pipeline view already shows the full per-agent
      // story without it.
    } else if (node.type === "group") {
      setSelectedGroupId(node.id);
    }
  }, []);

  const onCanvasNodeClick = useCallback((_event: unknown, node: { id: string; type?: string }) => {
    if (node.type === "agent") {
      const agentId = node.id.replace("agent-", "");
      setSelectedAgentId(agentId);
      setDrawerOpen(true);
    } else if (node.type === "group") {
      const groupId = node.id.replace("group-", "");
      setSelectedGroupId(groupId);
      setGroupDrawerOpen(true);
    }
  }, []);

  const rpsByAgent = new Map<string, number>();
  if (topologyData) {
    for (const n of topologyData.nodes) {
      if (n.type === "agent" && n.metrics) {
        rpsByAgent.set(n.id, n.metrics.throughput_rps);
      }
    }
  }

  const selectedAgent = selectedAgentId
    ? agents.find((a) => a.id === selectedAgentId)
    : undefined;

  if (error) {
    return (
      <div className="container mx-auto p-6">
        <div className="text-center">
          <h1 className="mb-2 text-2xl font-semibold text-destructive">
            Couldn't load fleet map
          </h1>
          <p className="text-muted-foreground">{error.message}</p>
          <Button onClick={handleRefresh} className="mt-4">
            <RefreshCwIcon className="mr-2 h-4 w-4" /> Retry
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="-m-4 flex h-full w-full flex-col">
      {/* Header */}
      <header className="flex flex-shrink-0 items-center justify-between gap-4 border-b border-border bg-background/60 px-6 py-3 backdrop-blur">
        <div>
          <div className="text-[10px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
            Fleet Map
          </div>
          <h1 className="text-lg font-semibold tracking-tight text-foreground">
            {view === "pipeline"
              ? selectedAgent
                ? selectedAgent.name + " · Pipeline"
                : "Collector Pipelines"
              : view === "data-flow"
                ? "Data Flow"
                : "Fleet Overview"}
          </h1>
        </div>

        <div className="flex items-center gap-3">
          {/* View tabs */}
          <Tabs value={view} onValueChange={(v) => setView(v as FleetView)}>
            <TabsList>
              <TabsTrigger value="pipeline" className="gap-1.5">
                <Workflow className="h-3.5 w-3.5" /> Pipeline
              </TabsTrigger>
              <TabsTrigger value="data-flow" className="gap-1.5">
                <GitForkIcon className="h-3.5 w-3.5" /> Data Flow
              </TabsTrigger>
              <TabsTrigger value="fleet" className="gap-1.5">
                <ShipIcon className="h-3.5 w-3.5" /> Fleet
              </TabsTrigger>
            </TabsList>
          </Tabs>

          {/* Signal filter — only meaningful on the data-flow tab; we
              still mount it on pipeline so the toggle state survives
              tab switches without disrupting layout. */}
          {view === "data-flow" && (
            <div className="flex items-center gap-1.5">
              {(Object.keys(SIGNAL_META) as Signal[]).map((s) => (
                <SignalChip
                  key={s}
                  signal={s}
                  active={signalFilter.has(s)}
                  onToggle={() =>
                    setSignalFilter((prev) => {
                      const next = new Set(prev);
                      if (next.has(s)) next.delete(s);
                      else next.add(s);
                      return next;
                    })
                  }
                />
              ))}
            </div>
          )}

          {/* Fleet tab keeps its instance / group level toggle here so
              the same canvas can render the agent view or the group
              view. Hide on the other tabs where it doesn't apply. */}
          {view === "fleet" && (
            <Tabs
              value={topologyLevel}
              onValueChange={(v) => setTopologyLevel(v as "instance" | "group")}
            >
              <TabsList>
                <TabsTrigger value="instance">Instance</TabsTrigger>
                <TabsTrigger value="group">Group</TabsTrigger>
              </TabsList>
            </Tabs>
          )}

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

      <div className="flex min-h-0 flex-1">
        <DisplaySidebar
          onNodeSelect={handleNodeSelect}
          className="w-72 flex-shrink-0"
        />

        {/* Canvas slot. flex layout so children's flex-1 lays them
            out correctly — TopologyCanvas in particular relies on a
            flex parent for its own flex-1 sizing. */}
        <div className="relative flex min-h-0 flex-1">
          {/* PIPELINE TAB */}
          {view === "pipeline" && (
            <div className="flex h-full w-full">
              {selectedAgent ? (
                <CollectorPipelineView
                  agentId={selectedAgent.id}
                  agentName={selectedAgent.name}
                  effectiveConfig={selectedAgent.effective_config}
                />
              ) : (
                <PipelineThumbnailGrid
                  agents={agents}
                  onPick={(id) => setSelectedAgentId(id)}
                />
              )}
            </div>
          )}

          {/* DATA FLOW TAB */}
          {view === "data-flow" && (
            <div className="flex h-full w-full">
              <ReactFlowProvider>
                <DataFlowCanvas
                  agents={agents}
                  rpsByAgent={rpsByAgent}
                  signalFilter={signalFilter}
                />
              </ReactFlowProvider>
            </div>
          )}

          {/* FLEET TAB */}
          {view === "fleet" && topologyData && (
            <TopologyCanvas
              nodes={nodes}
              edges={edges}
              onNodeClick={onCanvasNodeClick}
            />
          )}
        </div>

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
    </div>
  );
}

/**
 * When nothing is selected on the Pipeline tab, show a thumbnail grid
 * so operators can pick visually rather than from a list. Each tile
 * is a click-target into the per-agent pipeline detail. We render
 * lightweight tiles here (not full ReactFlow miniatures) so the page
 * stays snappy with dozens of agents.
 */
function PipelineThumbnailGrid({
  agents,
  onPick,
}: {
  agents: Agent[];
  onPick: (id: string) => void;
}) {
  if (agents.length === 0) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        No agents reporting yet.
      </div>
    );
  }
  return (
    <div className="grid h-full auto-rows-min gap-3 overflow-auto p-6 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
      {agents.map((a) => {
        const summary = summarizePipelineConfig(a.effective_config);
        return (
          <button
            key={a.id}
            type="button"
            onClick={() => onPick(a.id)}
            className="group rounded-lg border border-border bg-card p-4 text-left transition-colors hover:border-primary/60 hover:bg-card/80"
          >
            <div className="flex items-center gap-2">
              <span
                className="status-dot"
                style={{
                  ["--dot" as string]:
                    a.status === "online"
                      ? "var(--success)"
                      : "var(--muted-foreground)",
                }}
              />
              <span className="truncate text-sm font-medium text-foreground">
                {a.name}
              </span>
            </div>
            <div className="mt-1 text-[10px] uppercase tracking-wider text-muted-foreground">
              {a.version}
            </div>
            <div className="mt-3 flex flex-wrap gap-1.5">
              {summary.receiverTypes.slice(0, 4).map((t) => (
                <span
                  key={`r-${t}`}
                  className="rounded-md border border-border bg-background/40 px-1.5 py-0.5 text-[10px] font-mono text-muted-foreground"
                  title={`Receiver: ${t}`}
                >
                  {t}
                </span>
              ))}
              {summary.receiverTypes.length === 0 && (
                <span className="text-[11px] italic text-muted-foreground">
                  No pipeline configured
                </span>
              )}
            </div>
            <div className="mt-2 flex flex-wrap gap-1.5">
              {summary.signals.map((s) => (
                <span
                  key={s}
                  className="text-[10px] uppercase tracking-wider"
                  style={{ color: SIGNAL_META[s].color }}
                >
                  {SIGNAL_META[s].label}
                </span>
              ))}
            </div>
            <div className="mt-3 text-[10px] text-muted-foreground group-hover:text-foreground">
              Open pipeline →
            </div>
          </button>
        );
      })}
    </div>
  );
}

/**
 * Tiny config summary used by the thumbnail tiles. Picks the
 * receivers + active signals without paying for a full pipeline
 * layout. Kept inline here because it's view-specific projection
 * logic and not worth a shared module.
 */
function summarizePipelineConfig(effectiveConfig?: string): {
  receiverTypes: string[];
  signals: Signal[];
} {
  if (!effectiveConfig) return { receiverTypes: [], signals: [] };
  try {
    const cfg = parseYAMLLite(effectiveConfig);
    const receivers = Object.keys((cfg.receivers ?? {}) as Record<string, unknown>);
    const receiverTypes = Array.from(
      new Set(receivers.map((r) => r.split("/")[0])),
    );
    const pipelines = Object.keys(
      ((cfg.service ?? {}) as Record<string, unknown>).pipelines ?? {},
    );
    const signals: Signal[] = [];
    for (const p of pipelines) {
      const sig = p.split("/")[0];
      if (
        (sig === "traces" || sig === "metrics" || sig === "logs") &&
        !signals.includes(sig as Signal)
      ) {
        signals.push(sig as Signal);
      }
    }
    return { receiverTypes, signals };
  } catch {
    return { receiverTypes: [], signals: [] };
  }
}

// js-yaml is already imported at the top of this file; this helper
// just narrows the return type so callers don't have to cast.
function parseYAMLLite(text: string): Record<string, unknown> {
  return yaml.load(text) as Record<string, unknown>;
}
