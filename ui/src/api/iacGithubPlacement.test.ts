import { describe, it, expect, vi } from "vitest";

vi.mock("./base", () => ({
  apiGet: vi.fn(),
  apiPost: vi.fn(),
  apiPatch: vi.fn(),
  apiDelete: vi.fn(),
}));
vi.mock("../config", () => ({ apiBaseUrl: "http://test.local/api/v1" }));
vi.mock("./auth-store", () => ({
  getAuthToken: () => null,
  onAuthChallenge: vi.fn(),
}));

import { apiGet } from "./base";
import { getIaCGitHubPlacementSuggestions } from "./iacGithub";

const mockedApiGet = vi.mocked(apiGet);

describe("getIaCGitHubPlacementSuggestions (#183 slice 4)", () => {
  it("calls the placement-suggestions endpoint and returns the typed body", async () => {
    mockedApiGet.mockResolvedValue({
      connection_id: "c1",
      scanned: true,
      suggestions: [
        {
          resource_kind: "s3-access-logging",
          suggested_path: "buckets.tf",
          reason: "declares aws_s3_bucket",
          new_file: false,
        },
      ],
    });

    const r = await getIaCGitHubPlacementSuggestions("c1");

    expect(mockedApiGet).toHaveBeenCalledWith(
      "/iac/github/connections/c1/placement-suggestions",
    );
    expect(r.scanned).toBe(true);
    expect(r.suggestions[0].suggested_path).toBe("buckets.tf");
  });

  it("encodes the connection id", async () => {
    mockedApiGet.mockResolvedValue({
      connection_id: "a/b",
      scanned: false,
      suggestions: [],
    });
    await getIaCGitHubPlacementSuggestions("a/b");
    expect(mockedApiGet).toHaveBeenCalledWith(
      "/iac/github/connections/a%2Fb/placement-suggestions",
    );
  });
});
