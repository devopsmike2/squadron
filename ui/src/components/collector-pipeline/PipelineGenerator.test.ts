// Vitest coverage for generatePipelineNodes — the agent Pipeline tab
// entry point (CollectorPipelineView → generatePipelineNodes).
//
// yaml.load() returns undefined/null for a config that is empty,
// whitespace-only, or comment-only. The `!effectiveConfig` guard only
// catches a falsy string, so a comment-only reported effective config
// (plausible for a brownfield agent) reached `parsedConfig.service` and
// threw "Cannot read properties of undefined", crashing the tab. These
// tests assert the function returns a graceful fallback node instead of
// throwing. RED on the unguarded source, GREEN after the null guard.

import { describe, expect, it } from "vitest";

import { generatePipelineNodes } from "./PipelineGenerator";

describe("generatePipelineNodes null-safety", () => {
  it("returns the no-config fallback for an empty string", () => {
    const { nodes, edges } = generatePipelineNodes("");
    expect(edges).toEqual([]);
    expect(nodes.map((n) => n.id)).toContain("no-config");
  });

  it("does not throw on a comment-only config (yaml.load -> undefined)", () => {
    expect(() =>
      generatePipelineNodes("# managed by squadron, nothing here yet"),
    ).not.toThrow();
    const { nodes } = generatePipelineNodes(
      "# managed by squadron, nothing here yet",
    );
    // yaml.load returns undefined -> the no-service fallback, not a crash.
    expect(nodes.map((n) => n.id)).toContain("no-service");
  });

  it("does not throw on a whitespace-only config", () => {
    expect(() => generatePipelineNodes("   \n   \n")).not.toThrow();
    expect(
      generatePipelineNodes("   \n   \n").nodes.map((n) => n.id),
    ).toContain("no-service");
  });

  it("does not throw on a scalar config (yaml.load -> string)", () => {
    // A non-object YAML document (a bare scalar) parses to a string, so
    // `parsedConfig.service` would be undefined but the typeof guard
    // catches it before any deref.
    expect(() => generatePipelineNodes("just-a-string")).not.toThrow();
    expect(
      generatePipelineNodes("just-a-string").nodes.map((n) => n.id),
    ).toContain("no-service");
  });

  it("still builds pipeline nodes for a real config", () => {
    const cfg = `
receivers:
  otlp:
    protocols:
      grpc:
exporters:
  otlphttp:
    endpoint: https://example.com
service:
  pipelines:
    metrics:
      receivers: [otlp]
      exporters: [otlphttp]
`;
    const { nodes } = generatePipelineNodes(cfg);
    // Real config produces more than a single fallback node.
    expect(nodes.length).toBeGreaterThan(1);
    expect(nodes.map((n) => n.id)).not.toContain("no-service");
  });
});
