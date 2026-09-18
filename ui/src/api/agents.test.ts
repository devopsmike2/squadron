import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

vi.mock("../config", () => ({ apiBaseUrl: "http://test.local/api/v1" }));
vi.mock("./auth-store", () => ({
  getAuthToken: () => "tok-123",
  onAuthChallenge: vi.fn(),
}));

import { getAgents, getAgentFacets } from "./agents";

function stubJSON(body: unknown) {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function calledUrl(fetchMock: ReturnType<typeof vi.fn>): string {
  const arg = fetchMock.mock.calls[0][0];
  return typeof arg === "string" ? arg : (arg as Request).url;
}

describe("getAgents label filtering", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("emits one repeated ?label= param per entry (AND on the wire)", async () => {
    const fetchMock = stubJSON({ items: [], total: 0 });
    await getAgents({
      labels: ["deployment.environment=prod", "k8s.cluster.name=us-east-1"],
    });
    const url = calledUrl(fetchMock);
    const params = new URL(url).searchParams;
    const labels = params.getAll("label");
    expect(labels).toEqual([
      "deployment.environment=prod",
      "k8s.cluster.name=us-east-1",
    ]);
  });

  it("omits label entirely when labels is empty", async () => {
    const fetchMock = stubJSON({ items: [], total: 0 });
    await getAgents({ labels: [], status: "online" });
    const url = calledUrl(fetchMock);
    expect(url).not.toContain("label=");
    expect(url).toContain("status=online");
  });
});

describe("getAgentFacets", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("hits /agents/facets with no keys by default", async () => {
    const fetchMock = stubJSON({ facets: {} });
    await getAgentFacets();
    expect(calledUrl(fetchMock)).toContain("/agents/facets");
    expect(calledUrl(fetchMock)).not.toContain("keys=");
  });

  it("passes ?keys= when keys are given", async () => {
    const fetchMock = stubJSON({ facets: {} });
    await getAgentFacets(["deployment.environment", "k8s.cluster.name"]);
    const params = new URL(calledUrl(fetchMock)).searchParams;
    expect(params.get("keys")).toBe("deployment.environment,k8s.cluster.name");
  });
});
