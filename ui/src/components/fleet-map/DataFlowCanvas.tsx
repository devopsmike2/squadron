/**
 * Cross-fleet data flow visualization.
 *
 * The killer feature of Fleet Map: look at the whole squadron and
 * see where every byte of telemetry actually ends up. Agents on the
 * left, destinations on the right, edges colored per signal
 * (traces / metrics / logs) and sized by throughput.
 *
 * Built on @xyflow/react like the other canvases so the panning,
 * zooming, and minimap idioms operators already learned on the
 * Pipeline view transfer here. We layout left→right ourselves —
 * dagre would work but the structure is bipartite with a known
 * pinout, so manual placement is cheaper and renders identically.
 */

import {
  Background,
  Handle,
  Position,
  ReactFlow,
  type Edge,
  type EdgeProps,
  type Node,
  type NodeProps,
  getBezierPath,
} from "@xyflow/react";
import {
  ActivityIcon,
  ArrowRightIcon,
  CloudIcon,
  DatabaseIcon,
  FileTextIcon,
  RadioIcon,
  ServerIcon,
  TerminalIcon,
} from "lucide-react";
import * as React from "react";

import "@xyflow/react/dist/style.css";

import { SquadronMark } from "@/components/brand/SquadronMark";
import {
  groupFlowsByDestination,
  parseAgentFlows,
  type DestinationGroup,
  type DestinationKind,
  type FleetFlow,
  type Signal,
} from "@/components/fleet-map/exporter-parser";
import type { Agent } from "@/types/agent";

// ============================================================
// Signal -> color. Same tokens as the rest of the app so palette
// switches (and the dashboard) carry through.
// ============================================================
const SIGNAL_COLOR: Record<Signal, string> = {
  traces: "var(--chart-1)", // cyan
  metrics: "var(--chart-2)", // teal
  logs: "var(--chart-3)", // violet
};

const SIGNAL_LABEL: Record<Signal, string> = {
  traces: "Traces",
  metrics: "Metrics",
  logs: "Logs",
};

// Destination icon — chosen so a glance at the right column tells
// you what kind of sink each one is without reading the label.
const DEST_ICON: Record<DestinationKind, React.ComponentType<{ className?: string }>> = {
  squadron: SquadronMark as React.ComponentType<{ className?: string }>,
  otlp: CloudIcon,
  honeycomb: DatabaseIcon,
  datadog: DatabaseIcon,
  tempo: DatabaseIcon,
  jaeger: DatabaseIcon,
  loki: DatabaseIcon,
  prometheus: DatabaseIcon,
  kafka: RadioIcon,
  elasticsearch: DatabaseIcon,
  splunk: DatabaseIcon,
  newrelic: DatabaseIcon,
  dynatrace: DatabaseIcon,
  lightstep: DatabaseIcon,
  file: FileTextIcon,
  debug: TerminalIcon,
  logging: TerminalIcon,
  agent: ServerIcon,
  unknown: CloudIcon,
};

// ============================================================
// Custom nodes
// ============================================================

interface AgentSourceData {
  agentName: string;
  agentStatus: string;
  signalCount: number;
  flowCount: number;
  rps?: number;
  driftStatus?: string;
}

function AgentSourceNode({ data }: NodeProps<Node<AgentSourceData>>) {
  return (
    <div className="rounded-lg border border-border bg-card px-3 py-2 shadow-sm transition-colors hover:border-primary/70 min-w-[180px]">
      <Handle
        type="source"
        position={Position.Right}
        className="!h-2 !w-2 !border-0 !bg-muted-foreground/40"
      />
      <div className="flex items-center gap-2">
        <div
          className="status-dot"
          style={{
            ["--dot" as string]:
              data.agentStatus === "online"
                ? "var(--success)"
                : data.agentStatus === "offline"
                  ? "var(--muted-foreground)"
                  : "var(--destructive)",
          }}
        />
        <ServerIcon className="h-3.5 w-3.5 text-muted-foreground" />
        <span className="truncate text-sm font-medium text-foreground">
          {data.agentName}
        </span>
      </div>
      <div className="mt-1.5 flex items-center gap-2 text-[10px] uppercase tracking-wider text-muted-foreground">
        {data.flowCount} {data.flowCount === 1 ? "edge" : "edges"}
        {typeof data.rps === "number" && data.rps > 0 && (
          <>
            <span className="text-muted-foreground/40">·</span>
            <ActivityIcon className="h-3 w-3" />
            <span className="font-tabular normal-case tracking-normal">
              {formatRps(data.rps)}
            </span>
          </>
        )}
      </div>
    </div>
  );
}

interface DestinationNodeData {
  kind: DestinationKind;
  label: string;
  signals: Set<Signal>;
  flowCount: number;
}

function DestinationNode({ data }: NodeProps<Node<DestinationNodeData>>) {
  const Icon = DEST_ICON[data.kind] ?? CloudIcon;
  // Squadron destination gets the brand tile treatment — it's the
  // "home" point and visually deserves to stand out as the apex of
  // the fleet's data flow.
  const isSquadron = data.kind === "squadron";
  return (
    <div
      className={`rounded-lg border px-3 py-2 shadow-sm transition-colors min-w-[180px] ${
        isSquadron
          ? "border-primary/40 bg-card brand-glow"
          : "border-border bg-card hover:border-primary/70"
      }`}
    >
      <Handle
        type="target"
        position={Position.Left}
        className="!h-2 !w-2 !border-0 !bg-muted-foreground/40"
      />
      <div className="flex items-center gap-2">
        <Icon
          className={`h-3.5 w-3.5 ${isSquadron ? "text-brand" : "text-muted-foreground"}`}
        />
        <span className="truncate text-sm font-medium text-foreground">
          {data.label}
        </span>
      </div>
      <div className="mt-1.5 flex items-center gap-2 text-[10px] uppercase tracking-wider text-muted-foreground">
        {data.flowCount} {data.flowCount === 1 ? "stream" : "streams"}
        <span className="ml-auto flex items-center gap-1 normal-case tracking-normal">
          {Array.from(data.signals).map((s) => (
            <span
              key={s}
              className="status-dot"
              style={{ ["--dot" as string]: SIGNAL_COLOR[s] }}
              title={SIGNAL_LABEL[s]}
            />
          ))}
        </span>
      </div>
    </div>
  );
}

// ============================================================
// Custom edge with animated flow indicator
// ============================================================

interface FlowEdgeData {
  signal: Signal;
  rps: number;
  active: boolean;
}

function FlowEdge(props: EdgeProps<Edge<FlowEdgeData>>) {
  const {
    id,
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    data,
  } = props;
  const [path] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
  });

  const signal = data?.signal ?? "traces";
  const color = SIGNAL_COLOR[signal];
  // Edge thickness scales gently with RPS so a 0.01 rps and a 100
  // rps stream are visibly different but neither blows out the
  // canvas. Logarithmic clamp tuned to the dev demo's 0.15 / 233 rps
  // pair without making smaller streams invisible.
  const rps = data?.rps ?? 0;
  const width = rps > 0 ? Math.min(3.5, 1.2 + Math.log10(rps + 1) * 0.6) : 1;

  return (
    <>
      <path
        id={id}
        d={path}
        stroke={color}
        strokeWidth={width}
        strokeOpacity={0.55}
        fill="none"
      />
      {data?.active && rps > 0 && (
        // Animated dot riding along the path — the magical bit.
        // Speed scaled inversely to log(RPS) so high-throughput edges
        // feel busier without being chaotic.
        <circle r={2.5} fill={color} opacity={0.95}>
          <animateMotion
            dur={`${Math.max(1.2, 3 - Math.log10(rps + 1) * 0.7)}s`}
            repeatCount="indefinite"
            path={path}
          />
        </circle>
      )}
    </>
  );
}

// ============================================================
// Layout — pure function from (agents, flows) to (nodes, edges)
// ============================================================

const COL_AGENT_X = 40;
const COL_DEST_X = 520;
const ROW_HEIGHT = 80;
const ROW_GAP = 16;

interface BuildArgs {
  agents: Agent[];
  flowsByAgent: Map<string, FleetFlow[]>;
  groupedDestinations: DestinationGroup[];
  signalFilter: Set<Signal>;
  rpsByAgent: Map<string, number>;
}

function buildLayout({
  agents,
  flowsByAgent,
  groupedDestinations,
  signalFilter,
  rpsByAgent,
}: BuildArgs): { nodes: Node[]; edges: Edge[] } {
  const nodes: Node[] = [];
  const edges: Edge[] = [];

  // Agents column (left). Hide agents that produce zero flows
  // matching the active signal filter — the canvas should always
  // feel populated by what the operator asked to see.
  const visibleAgents = agents.filter((a) => {
    const flows = flowsByAgent.get(a.id) ?? [];
    return flows.some((f) => signalFilter.has(f.signal));
  });

  visibleAgents.forEach((a, i) => {
    const flows = (flowsByAgent.get(a.id) ?? []).filter((f) =>
      signalFilter.has(f.signal),
    );
    const signals = new Set(flows.map((f) => f.signal));
    nodes.push({
      id: `agent-${a.id}`,
      type: "agentSource",
      position: { x: COL_AGENT_X, y: i * (ROW_HEIGHT + ROW_GAP) },
      data: {
        agentName: a.name,
        agentStatus: a.status,
        signalCount: signals.size,
        flowCount: flows.length,
        rps: rpsByAgent.get(a.id),
        driftStatus: a.drift_status,
      } satisfies AgentSourceData,
    });
  });

  // Destinations column (right). Centered vertically against the
  // agents column so the overall composition reads as two roughly
  // balanced stacks. When the destination count is small, the agents
  // column dominates — that's expected.
  const visibleDestinations = groupedDestinations
    .map((g) => ({
      ...g,
      flows: g.flows.filter((f) => signalFilter.has(f.signal)),
    }))
    .filter((g) => g.flows.length > 0);

  const totalDestHeight =
    visibleDestinations.length * (ROW_HEIGHT + ROW_GAP) - ROW_GAP;
  const totalAgentHeight =
    visibleAgents.length * (ROW_HEIGHT + ROW_GAP) - ROW_GAP;
  const destStartY = Math.max(0, (totalAgentHeight - totalDestHeight) / 2);

  visibleDestinations.forEach((g, i) => {
    const signals = new Set(g.flows.map((f) => f.signal));
    nodes.push({
      id: `dest-${g.key}`,
      type: "destination",
      position: { x: COL_DEST_X, y: destStartY + i * (ROW_HEIGHT + ROW_GAP) },
      data: {
        kind: g.kind,
        label: g.label,
        signals,
        flowCount: g.flows.length,
      } satisfies DestinationNodeData,
    });
  });

  // Edges. One per (agent, destination, signal) so traces and
  // metrics from the same pair render as separate, color-coded
  // strokes. We attribute the agent's RPS to each of its outgoing
  // edges proportionally — the topology API gives us aggregate RPS
  // per agent, not per pipeline, so this is a deliberate
  // approximation rather than fake precision.
  for (const a of visibleAgents) {
    const flows = (flowsByAgent.get(a.id) ?? []).filter((f) =>
      signalFilter.has(f.signal),
    );
    const totalRps = rpsByAgent.get(a.id) ?? 0;
    const perEdgeRps = flows.length > 0 ? totalRps / flows.length : 0;

    for (const f of flows) {
      edges.push({
        id: `edge-${a.id}-${f.exporterId}-${f.signal}-${f.destinationKind}`,
        source: `agent-${a.id}`,
        target: `dest-${groupKey(f.destinationKind, f.destinationLabel)}`,
        type: "flow",
        data: {
          signal: f.signal,
          rps: perEdgeRps,
          active: a.status === "online" && perEdgeRps > 0,
        } satisfies FlowEdgeData,
      });
    }
  }

  return { nodes, edges };
}

function groupKey(kind: DestinationKind, label: string): string {
  return `${kind}:${label}`;
}

function formatRps(rps: number): string {
  if (rps >= 1000) return `${(rps / 1000).toFixed(1)}k rps`;
  if (rps >= 1) return `${rps.toFixed(1)} rps`;
  return `${rps.toFixed(2)} rps`;
}

// ============================================================
// Page-level component
// ============================================================

const nodeTypes = { agentSource: AgentSourceNode, destination: DestinationNode };
const edgeTypes = { flow: FlowEdge };

export interface DataFlowCanvasProps {
  agents: Agent[];
  /** Per-agent throughput from the topology API, keyed by agent id. */
  rpsByAgent: Map<string, number>;
  /** Active signal filter. When all three are present the canvas
   *  renders the full picture; toggling one off hides matching edges
   *  and hides agents that produce only filtered-out flows. */
  signalFilter: Set<Signal>;
}

export function DataFlowCanvas({
  agents,
  rpsByAgent,
  signalFilter,
}: DataFlowCanvasProps) {
  const { nodes, edges, totalFlows } = React.useMemo(() => {
    const flowsByAgent = new Map<string, FleetFlow[]>();
    const allFlows: FleetFlow[] = [];
    for (const a of agents) {
      const flows = parseAgentFlows(a);
      flowsByAgent.set(a.id, flows);
      allFlows.push(...flows);
    }
    const groupedDestinations = groupFlowsByDestination(allFlows);
    const layout = buildLayout({
      agents,
      flowsByAgent,
      groupedDestinations,
      signalFilter,
      rpsByAgent,
    });
    return { ...layout, totalFlows: allFlows.length };
  }, [agents, rpsByAgent, signalFilter]);

  if (totalFlows === 0) {
    return (
      <div className="flex h-full items-center justify-center p-12">
        <div className="max-w-md text-center">
          <ArrowRightIcon className="mx-auto h-10 w-10 text-muted-foreground/40" />
          <p className="mt-3 text-sm font-medium text-foreground">
            No exporters detected yet
          </p>
          <p className="mt-1 text-xs text-muted-foreground">
            Agents need a collector config with at least one configured
            exporter for the data-flow graph to populate. Push a config
            to an agent from the Configs page and the flow will appear
            here.
          </p>
        </div>
      </div>
    );
  }

  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      edgeTypes={edgeTypes}
      fitView
      fitViewOptions={{ padding: 0.15 }}
      minZoom={0.3}
      maxZoom={2}
      proOptions={{ hideAttribution: true }}
      nodesDraggable={false}
      nodesConnectable={false}
    >
      <Background gap={16} size={1} color="var(--border)" />
    </ReactFlow>
  );
}
