import { render } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

import AuthCallbackPage from "./AuthCallback";

import { getAuthToken, clearAuthToken } from "@/api/auth-store";

// Capture react-router navigation without mounting a real router.
const navSpy = vi.fn();
vi.mock("react-router-dom", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => navSpy };
});

describe("AuthCallbackPage — OIDC→frontend bearer handoff (ADR 0014)", () => {
  beforeEach(() => {
    navSpy.mockClear();
    clearAuthToken();
    // Reset the URL (path + fragment) between cases.
    window.history.replaceState(null, "", "/auth/callback");
  });

  it("stores the token from the URL fragment, scrubs it, and lands on /", () => {
    window.location.hash =
      "#squadron_token=" +
      encodeURIComponent("tok-abc.123") +
      "&expires_at=2026-07-06T12:00:00Z";

    render(<AuthCallbackPage />);

    expect(getAuthToken()).toBe("tok-abc.123");
    // Fragment (with the secret) is scrubbed from the URL/history.
    expect(window.location.hash).toBe("");
    expect(navSpy).toHaveBeenCalledWith("/", { replace: true });
  });

  it("url-decodes a token that was percent-escaped in the fragment", () => {
    // The enterprise callback url-escapes the token; the receiver must decode it.
    window.location.hash = "#squadron_token=a%2Bb%2Fc%3D&expires_at=x";
    render(<AuthCallbackPage />);
    expect(getAuthToken()).toBe("a+b/c=");
  });

  it("redirects to /login when no token is present", () => {
    window.location.hash = "";
    render(<AuthCallbackPage />);
    expect(getAuthToken()).toBeNull();
    expect(navSpy).toHaveBeenCalledWith("/login", { replace: true });
  });
});
