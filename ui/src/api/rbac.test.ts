import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("./base", () => ({
  apiGet: vi.fn(),
  apiPost: vi.fn(),
  apiDelete: vi.fn(),
}));

import { apiGet } from "./base";
import { getCatalog } from "./rbac";

const mockedApiGet = vi.mocked(apiGet);

describe("getCatalog", () => {
  beforeEach(() => vi.clearAllMocks());

  it("fetches /rbac/catalog and passes scopes + resource types through", async () => {
    mockedApiGet.mockResolvedValue({
      scopes: ["*", "agents:read"],
      resource_types: ["agent", "rollout"],
    });

    const cat = await getCatalog();

    expect(mockedApiGet).toHaveBeenCalledWith("/rbac/catalog");
    expect(cat.scopes).toEqual(["*", "agents:read"]);
    expect(cat.resource_types).toEqual(["agent", "rollout"]);
  });

  it("coalesces missing fields to empty arrays (so callers fall back to free-text)", async () => {
    mockedApiGet.mockResolvedValue({});

    const cat = await getCatalog();

    expect(cat.scopes).toEqual([]);
    expect(cat.resource_types).toEqual([]);
  });
});
