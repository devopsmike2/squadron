import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

vi.mock("../config", () => ({ apiBaseUrl: "http://test.local/api/v1" }));
vi.mock("./auth-store", () => ({
  getAuthToken: () => null,
  onAuthChallenge: vi.fn(),
}));

import { openIaCGitHubPullRequest, IaCGitHubOpenPRError } from "./iacGithub";

describe("openIaCGitHubPullRequest — NoPlacementMapping suggestions (#183 slice 2)", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("surfaces suggested_paths from the 422 body on the typed error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            error: {
              code: "NoPlacementMapping",
              message: "No placement-map row exists...",
              suggested_step: "placement-map",
            },
            suggested_paths: ["storage/buckets.tf", "main.tf"],
          }),
          { status: 422 },
        ),
      ),
    );

    await expect(
      openIaCGitHubPullRequest("conn-1", {
        resource_kind: "gcs-logging-enable",
      } as never),
    ).rejects.toMatchObject({
      code: "NoPlacementMapping",
      suggested_paths: ["storage/buckets.tf", "main.tf"],
    });
  });

  it("defaults suggested_paths to [] when the body omits them", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(
            JSON.stringify({ error: { code: "RepoNotFound", message: "x" } }),
            { status: 422 },
          ),
        ),
    );

    try {
      await openIaCGitHubPullRequest("conn-1", {
        resource_kind: "gcs-logging-enable",
      } as never);
      throw new Error("expected throw");
    } catch (e) {
      expect(e).toBeInstanceOf(IaCGitHubOpenPRError);
      expect((e as IaCGitHubOpenPRError).suggested_paths).toEqual([]);
    }
  });
});
