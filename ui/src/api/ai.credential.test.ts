import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../config", () => ({ apiBaseUrl: "http://test.local/api/v1" }));
vi.mock("./auth-store", () => ({
  getAuthToken: () => "test-token",
  onAuthChallenge: vi.fn(),
}));

import {
  type AICapabilities,
  clearAiCredential,
  getAICapabilities,
  setAiCredential,
} from "./ai";

describe("AI credential API (write-only key)", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("setAiCredential PUTs the key + provider and returns capabilities", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          enabled: true,
          provider: "anthropic",
          key_source: "stored",
          key_last4: "1234",
        } satisfies AICapabilities),
        { status: 200 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const caps = await setAiCredential(
      "fixture-secret-value-1234",
      "anthropic",
    );

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("http://test.local/api/v1/ai/credential");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body)).toEqual({
      api_key: "fixture-secret-value-1234",
      provider: "anthropic",
    });
    // The response is the non-secret capabilities; the key never round-trips.
    expect(caps.key_source).toBe("stored");
    expect(caps.key_last4).toBe("1234");
  });

  it("setAiCredential omits provider when not given", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(JSON.stringify({ enabled: true }), { status: 200 }),
      );
    vi.stubGlobal("fetch", fetchMock);

    await setAiCredential("tk-only-key");

    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(init.body)).toEqual({ api_key: "tk-only-key" });
  });

  it("clearAiCredential issues a DELETE", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ enabled: false, key_source: "none" }), {
        status: 200,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const caps = await clearAiCredential();

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("http://test.local/api/v1/ai/credential");
    expect(init.method).toBe("DELETE");
    expect(caps.key_source).toBe("none");
  });

  it("getAICapabilities parses the key_source + key_last4 hints", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            enabled: true,
            key_source: "env",
            key_last4: "9876",
          }),
          { status: 200 },
        ),
      ),
    );

    const caps = await getAICapabilities();
    expect(caps.key_source).toBe("env");
    expect(caps.key_last4).toBe("9876");
  });
});
