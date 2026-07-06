import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

vi.mock("../config", () => ({ apiBaseUrl: "http://test.local/api/v1" }));
vi.mock("./auth-store", () => ({
  getAuthToken: () => "tok-123",
  onAuthChallenge: vi.fn(),
}));

import { getAuditExportCapabilities, streamAuditExport } from "./audit";

describe("getAuditExportCapabilities (ADR 0020 6d-part-2 feature-detect)", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("returns the capabilities payload in enterprise", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(
            JSON.stringify({ formats: ["csv", "ndjson"], cross_tenant: true }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        ),
    );
    const caps = await getAuditExportCapabilities();
    expect(caps.cross_tenant).toBe(true);
    expect(caps.formats).toContain("ndjson");
  });

  it("rejects with .status === 404 in OSS (seam 404 → not enterprise)", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(JSON.stringify({ error: "not found" }), { status: 404 }),
        ),
    );
    await expect(getAuditExportCapabilities()).rejects.toMatchObject({
      status: 404,
    });
  });
});

describe("streamAuditExport (streaming reader + progress)", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    // jsdom doesn't implement these; stub so the download path is inert.
    URL.createObjectURL = vi.fn(() => "blob:mock");
    URL.revokeObjectURL = vi.fn();
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
  });
  afterEach(() => vi.unstubAllGlobals());

  const streamResponse = (lines: string[]): Response => {
    const enc = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        for (const l of lines) controller.enqueue(enc.encode(l));
        controller.close();
      },
    });
    return new Response(body, {
      status: 200,
      headers: { "Content-Type": "application/x-ndjson" },
    });
  };

  it("streams NDJSON, reports row progress, and returns the total", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        streamResponse(['{"id":"a"}\n', '{"id":"b"}\n{"id":"c"}\n']),
      );
    vi.stubGlobal("fetch", fetchMock);

    const progress: number[] = [];
    const result = await streamAuditExport({ actor: "operator:x" }, "ndjson", {
      onProgress: (p) => progress.push(p.rows),
    });

    expect(result.rows).toBe(3);
    expect(result.bytes).toBeGreaterThan(0);
    expect(progress.length).toBeGreaterThan(0);
    expect(progress[progress.length - 1]).toBe(3);

    // The request hit /audit-export with format + the actor filter.
    const calledUrl = fetchMock.mock.calls[0][0] as string;
    expect(calledUrl).toContain("/audit-export?");
    expect(calledUrl).toContain("format=ndjson");
    expect(calledUrl).toContain("actor=operator");
    // Bearer rides along on the raw fetch.
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect((init.headers as Record<string, string>)["Authorization"]).toBe(
      "Bearer tok-123",
    );
  });

  it("adds ?tenant= only for a cross-tenant export", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(streamResponse(['{"id":"a"}\n']));
    vi.stubGlobal("fetch", fetchMock);

    await streamAuditExport({}, "csv", { tenant: "beta" });
    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toContain("tenant=beta");
    expect(url).toContain("format=csv");
  });

  it("omits ?tenant= for a self-tenant export", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(streamResponse(['{"id":"a"}\n']));
    vi.stubGlobal("fetch", fetchMock);

    await streamAuditExport({}, "ndjson", {});
    expect(fetchMock.mock.calls[0][0] as string).not.toContain("tenant=");
  });

  it("throws on a non-ok response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("nope", { status: 403 })),
    );
    await expect(streamAuditExport({}, "ndjson")).rejects.toThrow(/403/);
  });
});
