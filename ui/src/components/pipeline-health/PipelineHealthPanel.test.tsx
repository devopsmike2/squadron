import { render, screen } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { PipelineHealthAgentPanel } from "./PipelineHealthPanel";

import { fetchAgentPipelineHealth } from "@/api/pipelinehealth";
import type { PipelineHealthAgentSnapshot } from "@/api/pipelinehealth";

// Keep the real verdictColor/verdictLabel helpers; only stub the fetch.
vi.mock("@/api/pipelinehealth", async () => {
  const actual = await vi.importActual<typeof import("@/api/pipelinehealth")>(
    "@/api/pipelinehealth",
  );
  return { ...actual, fetchAgentPipelineHealth: vi.fn() };
});

const mockedFetch = vi.mocked(fetchAgentPipelineHealth);

const renderPanel = () =>
  render(
    <SWRConfig value={{ provider: () => new Map() }}>
      <PipelineHealthAgentPanel agentID="agent-1" />
    </SWRConfig>,
  );

describe("PipelineHealthAgentPanel — label-less self-metrics", () => {
  beforeEach(() => vi.clearAllMocks());

  it("renders process rows without throwing when a sample's labels is null", async () => {
    // The collector's otelcol_process_* self-metrics arrive with no labels;
    // the backend serializes that as `labels: null`. This must not crash the
    // renderer (previously `row.labels.filter(...)` threw and blanked the
    // whole agent-detail drawer).
    const snapshot: PipelineHealthAgentSnapshot = {
      agent_id: "agent-1",
      verdict: "healthy",
      signals: [],
      latest: {
        // labels-less samples — the crashing series.
        otelcol_process_cpu_seconds: [{ labels: null, value: 12.5 } as never],
        otelcol_process_memory_rss: [
          { labels: null, value: 2_048_000 } as never,
        ],
        // a normal labelled exporter row alongside them, to be sure the
        // guard doesn't regress the happy path.
        otelcol_exporter_sent_metric_points: [
          {
            labels: [{ key: "exporter", value: "otlp/datadog" }],
            value: 42,
          },
        ],
      },
      last_sample: "2026-08-06T00:00:00Z",
    };
    mockedFetch.mockResolvedValue(snapshot);

    expect(() => renderPanel()).not.toThrow();

    // Process group + its rows render from the null-labels samples.
    expect(await screen.findByText("Process")).toBeInTheDocument();
    expect(screen.getByText("process_cpu_seconds")).toBeInTheDocument();
    expect(screen.getByText("process_memory_rss")).toBeInTheDocument();

    // Labelled exporter row still renders its label tag.
    expect(
      screen.getByText("exporter_sent_metric_points (otlp/datadog)"),
    ).toBeInTheDocument();
  });
});
