// Vitest coverage for the QueryResults panel — the same null-render
// blank-page family as the Groups `labels: null` field bug.
//
// `results.results` is typed `QueryResult[]` (non-optional) but arrives
// straight off the /telemetry/query wire. A Go nil slice marshals to
// JSON `null`, so a zero-row query can return `{ "results": null }`.
// The panel called `data.length` / `data.map(...)` directly on that,
// so `null.length` threw during render. These tests render QueryResults
// against null/asymmetric payloads and assert it never throws and shows
// a graceful empty state. RED on the unguarded source, GREEN after the
// `?? []` / `meta?.` guards.

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { QueryResults } from "./QueryResults";

import type { SquadronQLResponse } from "@/api/squadron-ql";

// recharts' ResponsiveContainer measures 0x0 in jsdom; that only
// matters for the chart view, which these tests never enter (null /
// empty results can't show a chart). No shim needed.

describe("QueryResults null-safety", () => {
  it("renders without crashing when results is null (nil slice on the wire)", () => {
    const resp = {
      results: null,
      meta: {
        execution_time: 1_000_000,
        row_count: 0,
        query_type: "metrics",
        used_rollups: false,
      },
    } as unknown as SquadronQLResponse;

    expect(() => render(<QueryResults results={resp} />)).not.toThrow();
    // meta still renders its readout.
    expect(screen.getByText("results")).toBeInTheDocument();
    expect(screen.getByText("execution time")).toBeInTheDocument();
  });

  it("renders without crashing when both results and meta are null", () => {
    const resp = {
      results: null,
      meta: null,
    } as unknown as SquadronQLResponse;

    expect(() => render(<QueryResults results={resp} />)).not.toThrow();
    // Falls back to the row count from the (empty) data array.
    expect(screen.getByText("results")).toBeInTheDocument();
  });

  it("renders an asymmetric row whose labels are null without crashing", () => {
    const resp = {
      results: [
        {
          type: "metrics",
          timestamp: new Date("2026-01-01T00:00:00Z").toISOString(),
          labels: null as unknown as Record<string, string>,
          value: 42,
        },
        {
          type: "metrics",
          timestamp: new Date("2026-01-01T00:01:00Z").toISOString(),
          labels: { __name__: "cpu_seconds", host: "web-1" },
          value: 7,
        },
      ],
      meta: {
        execution_time: 2_000_000,
        row_count: 2,
        query_type: "metrics",
        used_rollups: false,
      },
    } as unknown as SquadronQLResponse;

    expect(() => render(<QueryResults results={resp} />)).not.toThrow();
    expect(screen.getByText("2")).toBeInTheDocument(); // row_count
  });
});
