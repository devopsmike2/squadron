// Vitest coverage for the v0.89.87 #718 Stream 116 (span quality slice
// 1 chunk 3) discoverySpanQuality API client. Stubs fetch at the
// global boundary, then asserts the parsed shape + the 404→null
// contract for the per-resource fetcher.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  fetchResourceSpanQuality,
  fetchSpanQuality,
  type ProviderSpanQuality,
  type SpanQualityResponse,
} from "./discoverySpanQuality";

function provider(over: Partial<ProviderSpanQuality> = {}): ProviderSpanQuality {
  return {
    resource_count: 0,
    resources_with_issues: 0,
    orphan_pct: 0,
    missing_attr_pct: 0,
    attr_mismatch_pct: 0,
    ...over,
  };
}

const NON_ZERO_RESPONSE: SpanQualityResponse = {
  providers: {
    aws: provider({
      resource_count: 47,
      resources_with_issues: 12,
      orphan_pct: 3.2,
      missing_attr_pct: 8.1,
      attr_mismatch_pct: 1.7,
    }),
    gcp: provider({
      resource_count: 30,
      resources_with_issues: 6,
      missing_attr_pct: 4.4,
    }),
    azure: provider({
      resource_count: 40,
      resources_with_issues: 14,
      orphan_pct: 5.1,
      missing_attr_pct: 6.0,
      attr_mismatch_pct: 2.0,
    }),
    oci: provider({
      resource_count: 25,
      resources_with_issues: 6,
      orphan_pct: 2.0,
      missing_attr_pct: 5.5,
      attr_mismatch_pct: 1.0,
    }),
  },
  totals: {
    resource_count: 142,
    resources_with_issues: 38,
    orphan_pct: 4.1,
    missing_attr_pct: 6.3,
    attr_mismatch_pct: 2.0,
  },
};

const ALL_ZERO_RESPONSE: SpanQualityResponse = {
  providers: {
    aws: provider(),
    gcp: provider(),
    azure: provider(),
    oci: provider(),
  },
  totals: {
    resource_count: 0,
    resources_with_issues: 0,
    orphan_pct: 0,
    missing_attr_pct: 0,
    attr_mismatch_pct: 0,
  },
};

// makeFetchResponse builds a minimal fetch Response-shaped object the
// simpleRequest wrapper consumes: { ok, status, statusText, json() }.
// Avoids the full Response polyfill that jsdom does not provide.
function makeFetchResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 404 ? "Not Found" : "OK",
    json: async () => body,
  } as unknown as Response;
}

describe("discoverySpanQuality", () => {
  let fetchSpy: ReturnType<typeof vi.spyOn>;
  beforeEach(() => {
    fetchSpy = vi.spyOn(globalThis, "fetch");
  });
  afterEach(() => {
    fetchSpy.mockRestore();
  });

  it("TestFetchSpanQuality_HandlesNonZeroResponse", async () => {
    fetchSpy.mockResolvedValueOnce(makeFetchResponse(200, NON_ZERO_RESPONSE));
    const got = await fetchSpanQuality();

    // Wire path includes the /discovery/span_quality endpoint.
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const calledUrl = String(fetchSpy.mock.calls[0][0]);
    expect(calledUrl).toContain("/discovery/span_quality");

    // Shape parses cleanly — all four providers + totals.
    expect(got.totals.orphan_pct).toBe(4.1);
    expect(got.totals.missing_attr_pct).toBe(6.3);
    expect(got.totals.attr_mismatch_pct).toBe(2.0);
    expect(got.totals.resources_with_issues).toBe(38);
    expect(got.providers.aws.resource_count).toBe(47);
    expect(got.providers.gcp.missing_attr_pct).toBe(4.4);
    expect(got.providers.azure.orphan_pct).toBe(5.1);
    expect(got.providers.oci.attr_mismatch_pct).toBe(1.0);
  });

  it("TestFetchSpanQuality_HandlesAllZeroResponse", async () => {
    fetchSpy.mockResolvedValueOnce(makeFetchResponse(200, ALL_ZERO_RESPONSE));
    const got = await fetchSpanQuality();

    // The endpoint legitimately returns all-zero on cold start. The
    // client must NOT treat that as an error — the dashboard panel
    // owns the hide-when-all-zero behavior (Discovery.test.tsx
    // §10 test 12 coverage).
    expect(got.totals.orphan_pct).toBe(0);
    expect(got.totals.missing_attr_pct).toBe(0);
    expect(got.totals.attr_mismatch_pct).toBe(0);
    expect(got.providers.aws.resource_count).toBe(0);
    expect(got.providers.gcp.resources_with_issues).toBe(0);
  });

  it("TestFetchResourceSpanQuality_HandlesNotFound", async () => {
    // 404 → null per the slice-1 design doc §6.2: a resource Squadron
    // has inventoried but for which no spans have been observed in
    // the current window has no counter row to surface. The caller
    // renders a gray "no observations" dot rather than an error
    // state. The error envelope shape mirrors what the Go handler
    // emits on 404 (humanized envelope or string).
    fetchSpy.mockResolvedValueOnce(
      makeFetchResponse(404, { error: "resource not found" }),
    );
    const got = await fetchResourceSpanQuality(
      "aws",
      "compute",
      "i-0abc",
    );
    expect(got).toBeNull();

    // Wire path includes the four-segment per-resource shape.
    const calledUrl = String(fetchSpy.mock.calls[0][0]);
    expect(calledUrl).toContain(
      "/discovery/aws/inventory/compute/i-0abc/span_quality",
    );
  });

  it("TestFetchResourceSpanQuality_PropagatesNon404Errors", async () => {
    // 5xx (or any non-404 error) must still surface so the dashboard
    // can render an error toast. Returning null silently on every
    // error would mask backend outages — the 404 branch is the ONLY
    // null-mapping path.
    fetchSpy.mockResolvedValueOnce(
      makeFetchResponse(500, { error: "internal error" }),
    );
    await expect(
      fetchResourceSpanQuality("aws", "compute", "i-0abc"),
    ).rejects.toThrow();
  });
});
