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
  ArrowLeftIcon,
  ChevronRightIcon,
  GitForkIcon,
  RefreshCwIcon,
  ShipIcon,
  Workflow,
} from "lucide-react";
import { useCallback, useEffect, useState } from "react";
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
        active
          ? ({ ["color" as string]: meta.color } as React.CSSProperties)
          : undefined
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
  // We store the full Agent object alongside the id so the Pipeline
  // tab can render immediately on click — independent of whether the
  // page-level /agents SWR fetch has finished yet.
  const [selectedAgent, setSelectedAgent] = useState<Agent | null>(null);
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
  // Fleet Map needs effective_config per agent for the Data Flow
  // tab's exporter parsing. /topology returns a slim projection;
  // /agents gives us the full Agent object including config. We
  // ask for 500 in one shot — at fleets >500 the Pipeline thumbnail
  // grid would be 100+ rows tall anyway and benefits from a
  // dedicated "scoped by group/label" filter on the rail (planned
  // for v0.24 alongside cost optimization). 500 is the API cap.
  const { data: agentsResp, mutate: mutateAgents } = useSWR(
    "/agents-fleetmap",
    () => getAgents({ limit: 500 }),
    { refreshInterval: 30000 },
  );
  const agents: Agent[] = agentsResp?.items ?? [];

  const { nodes, edges } = useTopologyLayout(topologyData, topologyLevel);

  const handleRefresh = async () => {
    setRefreshing(true);
    await Promise.all([mutateTopology(), mutateAgents()]);
    setRefreshing(false);
  };

  // Pipeline tab needs to know which agent the operator picked. The
  // DisplaySidebar's onNodeSelect passes the full Agent object via
  // node.data, so we keep that as our source of truth — going
  // through `agents.find(a => a.id === selectedAgentId)` is brittle
  // because the sidebar and the page use different SWR cache keys
  // ("agents" vs "/agents") and can momentarily diverge.
  const handleNodeSelect = useCallback((node: TopologyNode) => {
    if (node.type === "agent") {
      setSelectedAgentId(node.id);
      setSelectedAgent((node.data as Agent | undefined) ?? null);
    } else if (node.type === "group") {
      setSelectedGroupId(node.id);
    }
  }, []);

  // Drop the current agent selection — used by the breadcrumb back
  // button and the ESC keybinding below. Wrapping in useCallback so
  // the breadcrumb click handler stays referentially stable across
  // renders (matters because the header re-renders on every SWR tick).
  const clearAgentSelection = useCallback(() => {
    setSelectedAgentId(null);
    setSelectedAgent(null);
  }, []);

  // ESC clears the agent drill-down on the Pipeline tab. Scoped to
  // that tab because on Data Flow and Fleet there's no equivalent
  // 'drilled into' state and the key would be confusing.
  // We also skip the binding when a drawer is open so ESC there
  // closes the drawer first (the drawer's own ESC handler runs and
  // stopPropagation prevents our handler from firing).
  useEffect(() => {
    if (view !== "pipeline" || !selectedAgent) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !drawerOpen && !groupDrawerOpen) {
        clearAgentSelection();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [view, selectedAgent, drawerOpen, groupDrawerOpen, clearAgentSelection]);

  const onCanvasNodeClick = useCallback(
    (_event: unknown, node: { id: string; type?: string; data?: unknown }) => {
      if (node.type === "agent") {
        const agentId = node.id.replace("agent-", "");
        setSelectedAgentId(agentId);
        // node.data here is the topology projection (TopologyNodeData),
        // not a full Agent. We don't update selectedAgent so the
        // Pipeline tab stays on whatever was previously picked from
        // the sidebar — the canvas click on the Fleet tab is meant
        // to open the drawer, not switch the pipeline view.
        setDrawerOpen(true);
      } else if (node.type === "group") {
        const groupId = node.id.replace("group-", "");
        setSelectedGroupId(groupId);
        setGroupDrawerOpen(true);
      }
    },
    [],
  );

  const rpsByAgent = new Map<string, number>();
  if (topologyData) {
    for (const n of topologyData.nodes) {
      if (n.type === "agent" && n.metrics) {
        rpsByAgent.set(n.id, n.metrics.throughput_rps);
      }
    }
  }

  // Keep `selectedAgent` in sync if the agents list refreshes (e.g.
  // the operator deletes the selected agent in another tab) — but
  // only update when we have a match; otherwise leave the current
  // selectedAgent in place so a transient empty list doesn't blank
  // the canvas.
  // (Selection is primarily driven by sidebar/thumbnail clicks which
  // set selectedAgent directly with the Agent object.)

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
        <div className="min-w-0">
          <div className="text-[10px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
            Fleet Map
          </div>
          {view === "pipeline" && selectedAgent ? (
            // Drill-down state: render a real breadcrumb so the
            // operator has a one-click way back to the fleet
            // overview without reaching for the sidebar. The
            // parent crumb is the same element that would be
            // rendered as the page title when no agent is
            // selected, so the navigation feels consistent.
            <h1 className="flex items-center gap-1.5 text-lg font-semibold tracking-tight text-foreground">
              <button
                type="button"
                onClick={clearAgentSelection}
                className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 -mx-1.5 text-muted-foreground transition-colors hover:bg-accent/40 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                title="Back to all pipelines (Esc)"
              >
                <ArrowLeftIcon className="h-4 w-4" />
                <span>All pipelines</span>
              </button>
              <ChevronRightIcon className="h-3.5 w-3.5 text-muted-foreground/60" />
              <span className="truncate">{selectedAgent.name}</span>
            </h1>
          ) : (
            <h1 className="text-lg font-semibold tracking-tight text-foreground">
              {view === "pipeline"
                ? "Collector Pipelines"
                : view === "data-flow"
                  ? "Data Flow"
                  : "Fleet Overview"}
            </h1>
          )}
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
          {/* PIPELINE TAB
           *
           * The wrapper is `h-full w-full` (block, not flex) so the
           * CollectorPipelineView's own `h-full flex flex-col`
           * resolves correctly. An earlier version used `flex h-full
           * w-full` here, which made the wrapper a flex parent and
           * collapsed the child's width to 0 — pipeline rendered
           * but was invisible because the ReactFlow canvas reported
           * width=0. */}
          {view === "pipeline" && (
            <div className="h-full w-full">
              {selectedAgent ? (
                <CollectorPipelineView
                  agentId={selectedAgent.id}
                  agentName={selectedAgent.name}
                  effectiveConfig={selectedAgent.effective_config}
                />
              ) : (
                <PipelineThumbnailGrid
                  agents={agents}
                  onPick={(agent) => {
                    setSelectedAgentId(agent.id);
                    setSelectedAgent(agent);
                  }}
                />
              )}
            </div>
          )}

          {/* DATA FLOW TAB
           *
           * DataFlowCanvas internally uses ReactFlow which needs a
           * sized parent. We give it `h-full w-full` directly. */}
          {view === "data-flow" && (
            <div className="h-full w-full">
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
  onPick: (agent: Agent) => void;
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
            onClick={() => onPick(a)}
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
    const receivers = Object.keys(
      (cfg.receivers ?? {}) as Record<string, unknown>,
    );
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
