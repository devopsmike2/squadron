import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("./base", () => ({
  apiGet: vi.fn(),
  apiPost: vi.fn(),
}));

import { apiGet, apiPost } from "./base";
import {
  generateAWSRecommendations,
  pollRecommendationJob,
  type ScanResult,
} from "./discovery";

const mockedApiGet = vi.mocked(apiGet);
const mockedApiPost = vi.mocked(apiPost);

// v0.89.209 — async recommendations: kick-off (202 + job_id) then poll the
// provider-agnostic job-status endpoint.
describe("async recommendations", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("kicks off with the scan result then resolves from a succeeded poll", async () => {
    mockedApiPost.mockResolvedValue({ job_id: "job-1", status: "pending" });
    mockedApiGet.mockResolvedValue({
      job_id: "job-1",
      status: "succeeded",
      result: { declined: false, recommendations: [] },
    });

    const res = await generateAWSRecommendations(
      "123456789012",
      { account_id: "123456789012" } as unknown as ScanResult,
    );

    expect(res.declined).toBe(false);
    expect(mockedApiPost).toHaveBeenCalledWith(
      "/discovery/aws/connections/123456789012/recommendations",
      { scan_result: { account_id: "123456789012" } },
    );
    expect(mockedApiGet).toHaveBeenCalledWith(
      "/discovery/recommendations/jobs/job-1",
    );
  });

  it("rejects with the humanized error when the job fails", async () => {
    mockedApiGet.mockResolvedValue({
      job_id: "job-2",
      status: "failed",
      error: {
        code: "ProposerCallFailed",
        message: "anthropic call: context deadline exceeded",
      },
    });

    await expect(pollRecommendationJob("job-2", 1, 1000)).rejects.toThrow(
      /context deadline exceeded/,
    );
  });
});
