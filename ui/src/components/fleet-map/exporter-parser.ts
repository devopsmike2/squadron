/**
 * Cross-fleet exporter parser.
 *
 * Reads an agent's effective_config YAML and projects out the
 * (signal, exporter type, destination) triples we draw edges from on
 * the Data Flow tab. Pure function — no React, no SWR, easy to test.
 *
 * Why this is a separate module: the Data Flow canvas needs to
 * aggregate across MANY agents, so we can't just call the existing
 * PipelineGenerator (which builds a per-collector layout). We need a
 * normalized view that lets us union destinations across the fleet.
 */

import * as yaml from "js-yaml";

import type { Agent } from "@/types/agent";

export type Signal = "traces" | "metrics" | "logs";

/**
 * Known external destinations. The buckets are deliberately broad —
 * the exporter type name from OTel is the source of truth, but we
 * collapse vendor variants into recognizable labels because trace UIs
 * (and operators) think in terms of "where the data ends up", not
 * which Go module did the encoding.
 */
export type DestinationKind =
  | "squadron" // back to Squadron's own OTLP receiver — the "loop"
  | "otlp" // generic OTLP exporter to an unspecified backend
  | "honeycomb"
  | "datadog"
  | "tempo"
  | "jaeger"
  | "loki"
  | "prometheus"
  | "kafka"
  | "elasticsearch"
  | "splunk"
  | "newrelic"
  | "dynatrace"
  | "lightstep"
  | "file"
  | "debug"
  | "logging"
  | "agent" // another collector instance in the fleet (peer)
  | "unknown";

/** A single edge from an agent to a destination, for a given signal. */
export interface FleetFlow {
  agentId: string;
  agentName: string;
  signal: Signal;
  exporterId: string; // the YAML key, e.g. "otlphttp/honeycomb"
  destinationKind: DestinationKind;
  destinationLabel: string; // human-readable, e.g. "Honeycomb (api.honeycomb.io)"
  endpoint?: string; // raw endpoint if we could extract one
}

/**
 * Bucketize an exporter type + its config into one of the known
 * destination kinds. The matching order matters — vendor-specific
 * exporters are recognized by their type name; generic OTLP is
 * classified by inspecting the endpoint URL for known hosts.
 */
function classifyExporter(
  type: string,
  spec: Record<string, unknown> | undefined,
): { kind: DestinationKind; endpoint?: string } {
  const t = type.toLowerCase();
  const endpoint = pickEndpoint(spec);
  const lowerEndpoint = endpoint?.toLowerCase() ?? "";

  // Vendor-specific exporters first — most precise classifications.
  if (t.startsWith("datadog")) return { kind: "datadog", endpoint };
  if (t.startsWith("newrelic")) return { kind: "newrelic", endpoint };
  if (t.startsWith("dynatrace")) return { kind: "dynatrace", endpoint };
  if (t.startsWith("lightstep")) return { kind: "lightstep", endpoint };
  if (t.startsWith("signalfx") || t.startsWith("splunk"))
    return { kind: "splunk", endpoint };
  if (t.startsWith("loki")) return { kind: "loki", endpoint };
  if (t.startsWith("prometheus")) return { kind: "prometheus", endpoint };
  if (t.startsWith("kafka")) return { kind: "kafka", endpoint };
  if (
    t.startsWith("elasticsearch") ||
    t.startsWith("opensearch") ||
    t.startsWith("elastic")
  )
    return { kind: "elasticsearch", endpoint };
  if (t.startsWith("jaeger")) return { kind: "jaeger", endpoint };

  if (t === "file") return { kind: "file", endpoint };
  if (t === "debug" || t === "logging") return { kind: "debug", endpoint };

  // OTLP variants — drill into the endpoint URL to recognize known
  // backends that just speak OTLP (Honeycomb, Tempo, etc.) and
  // anything pointing at Squadron itself.
  if (t.startsWith("otlphttp") || t.startsWith("otlp")) {
    if (
      lowerEndpoint.includes("honeycomb.io") ||
      lowerEndpoint.includes("honeycomb.com")
    )
      return { kind: "honeycomb", endpoint };
    if (lowerEndpoint.includes("tempo")) return { kind: "tempo", endpoint };
    if (lowerEndpoint.includes("squadron"))
      return { kind: "squadron", endpoint };
    // Heuristic for "back to Squadron" via the local OTLP receiver: a
    // localhost or :4317/:4318 endpoint on a non-loopback hostname
    // generally still means the demo-collector pattern.
    if (
      lowerEndpoint.endsWith(":4317") ||
      lowerEndpoint.endsWith(":4318") ||
      lowerEndpoint.includes("localhost:4317") ||
      lowerEndpoint.includes("localhost:4318")
    )
      return { kind: "squadron", endpoint };
    // If the endpoint is a hostname Squadron knows about (another
    // agent in the fleet), the page-level resolver can promote this
    // to "agent" later — too expensive to do here.
    return { kind: "otlp", endpoint };
  }

  return { kind: "unknown", endpoint };
}

/** Best-effort endpoint extraction from an exporter spec. OTLP and
 *  most vendor exporters put the destination at `.endpoint`; a few
 *  (kafka, file) use other shapes. Returns undefined when no obvious
 *  candidate exists rather than fabricating one. */
function pickEndpoint(
  spec: Record<string, unknown> | undefined,
): string | undefined {
  if (!spec || typeof spec !== "object") return undefined;
  const candidates = ["endpoint", "url", "api_url", "broker_address"];
  for (const k of candidates) {
    const v = (spec as Record<string, unknown>)[k];
    if (typeof v === "string" && v.length > 0) return v;
  }
  // Some exporters nest under .traces / .metrics / .logs sub-blocks
  // each with their own endpoint (Datadog does this). Pick the first
  // we find to give the operator something to read; per-signal
  // resolution happens at the edge level.
  for (const subKey of ["traces", "metrics", "logs"]) {
    const sub = (spec as Record<string, unknown>)[subKey];
    if (sub && typeof sub === "object") {
      const ep = pickEndpoint(sub as Record<string, unknown>);
      if (ep) return ep;
    }
  }
  return undefined;
}

/** Human-readable label for a destination kind. The endpoint, when
 *  available, gets appended so operators can tell two Honeycomb
 *  environments apart at a glance. */
export function destinationLabel(
  kind: DestinationKind,
  endpoint?: string,
): string {
  const base: Record<DestinationKind, string> = {
    squadron: "Squadron",
    otlp: "OTLP backend",
    honeycomb: "Honeycomb",
    datadog: "Datadog",
    tempo: "Tempo",
    jaeger: "Jaeger",
    loki: "Loki",
    prometheus: "Prometheus",
    kafka: "Kafka",
    elasticsearch: "Elasticsearch",
    splunk: "Splunk",
    newrelic: "New Relic",
    dynatrace: "Dynatrace",
    lightstep: "Lightstep",
    file: "File",
    debug: "Debug sink",
    logging: "Debug sink",
    agent: "Peer collector",
    unknown: "Unknown",
  };
  const label = base[kind];
  if (endpoint && kind !== "debug" && kind !== "logging") {
    // Strip protocol + port for compact rendering — operators rarely
    // need the scheme in a topology view.
    const compact = endpoint.replace(/^https?:\/\//, "").replace(/:\d+$/, "");
    if (compact && compact !== label.toLowerCase()) {
      return `${label} (${compact})`;
    }
  }
  return label;
}

/**
 * Parse a single agent's effective_config and return one FleetFlow
 * per (signal, exporter) pair. Returns [] when the config is missing,
 * unparseable, or contains no service pipelines — never throws.
 *
 * The matching against service.pipelines is what makes this
 * signal-aware: a single OTLP exporter referenced by both the
 * traces pipeline and the metrics pipeline produces two edges, one
 * per signal, so the Data Flow canvas can color them differently.
 */
export function parseAgentFlows(agent: Agent): FleetFlow[] {
  if (!agent.effective_config) return [];

  let cfg: Record<string, unknown>;
  try {
    cfg = yaml.load(agent.effective_config) as Record<string, unknown>;
  } catch {
    return [];
  }
  if (!cfg || typeof cfg !== "object") return [];

  const exporters = (cfg.exporters ?? {}) as Record<string, unknown>;
  const service = (cfg.service ?? {}) as Record<string, unknown>;
  const pipelines = (service.pipelines ?? {}) as Record<string, unknown>;

  const out: FleetFlow[] = [];
  for (const [pipelineKey, pipelineRaw] of Object.entries(pipelines)) {
    // Pipeline keys can be "traces", "metrics", "logs" or namespaced
    // (e.g. "traces/auth", "metrics/k8s"). The signal is the prefix.
    const signal = pipelineKey.split("/")[0] as Signal;
    if (signal !== "traces" && signal !== "metrics" && signal !== "logs") {
      continue;
    }
    const pipeline = pipelineRaw as Record<string, unknown>;
    const exporterIds = (pipeline.exporters ?? []) as string[];
    for (const id of exporterIds) {
      const spec = exporters[id] as Record<string, unknown> | undefined;
      // Exporter type is the bit before any "/" suffix:
      // "otlphttp/honeycomb" → type "otlphttp", instance "honeycomb".
      const type = id.split("/")[0];
      const { kind, endpoint } = classifyExporter(type, spec);
      out.push({
        agentId: agent.id,
        agentName: agent.name,
        signal,
        exporterId: id,
        destinationKind: kind,
        destinationLabel: destinationLabel(kind, endpoint),
        endpoint,
      });
    }
  }
  return out;
}

/**
 * Aggregate flows across the fleet and reduce to (destinationKey,
 * signal) groups so the canvas can size + color edges by total
 * throughput later without re-walking every flow.
 *
 * The destinationKey is `${kind}:${label}` — same kind + same label
 * means same destination node on the canvas, so a Honeycomb prod
 * env and a Honeycomb staging env stay distinct.
 */
export interface DestinationGroup {
  key: string;
  kind: DestinationKind;
  label: string;
  flows: FleetFlow[];
}

export function groupFlowsByDestination(
  flows: FleetFlow[],
): DestinationGroup[] {
  const map = new Map<string, DestinationGroup>();
  for (const f of flows) {
    const key = `${f.destinationKind}:${f.destinationLabel}`;
    let g = map.get(key);
    if (!g) {
      g = {
        key,
        kind: f.destinationKind,
        label: f.destinationLabel,
        flows: [],
      };
      map.set(key, g);
    }
    g.flows.push(f);
  }
  return Array.from(map.values()).sort((a, b) => {
    // Squadron first (it's the focal point), then known destinations,
    // unknown last. Within a tier, sort by label.
    const tier = (k: DestinationKind) => {
      if (k === "squadron") return 0;
      if (k === "unknown" || k === "debug" || k === "logging") return 2;
      return 1;
    };
    const t = tier(a.kind) - tier(b.kind);
    if (t !== 0) return t;
    return a.label.localeCompare(b.label);
  });
}
