// Vitest coverage for the shared ErrorBoundary — the durable half of
// the Groups `labels: null` blank-page fix. The Layout now wraps the
// route <Outlet /> in this boundary so a single crashing route degrades
// to a fallback instead of unmounting the whole app shell. These tests
// pin that behavior: a throwing child renders the fallback (not a blank
// tree), sibling chrome outside the boundary survives, and a healthy
// child renders untouched.

import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ErrorBoundary } from "./error-boundary";

function Boom(): never {
  throw new Error("kaboom");
}

describe("ErrorBoundary", () => {
  it("renders the fallback instead of a blank tree when a child throws", () => {
    // React logs the caught error to console.error; silence it so the
    // test output stays clean without hiding real failures.
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <ErrorBoundary fallback={<div>graceful fallback</div>}>
        <Boom />
      </ErrorBoundary>,
    );
    expect(screen.getByText("graceful fallback")).toBeInTheDocument();
    spy.mockRestore();
  });

  it("keeps sibling chrome outside the boundary mounted when a route crashes", () => {
    // Models the Layout shape: the sidebar/header sit OUTSIDE the
    // boundary, the crashing page sits inside. The chrome must survive.
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <div>
        <nav>squadron-shell-nav</nav>
        <ErrorBoundary fallback={<div role="alert">page failed to render</div>}>
          <Boom />
        </ErrorBoundary>
      </div>,
    );
    expect(screen.getByText("squadron-shell-nav")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "page failed to render",
    );
    spy.mockRestore();
  });

  it("renders children untouched when nothing throws", () => {
    render(
      <ErrorBoundary fallback={<div>should not show</div>}>
        <div>healthy page</div>
      </ErrorBoundary>,
    );
    expect(screen.getByText("healthy page")).toBeInTheDocument();
    expect(screen.queryByText("should not show")).not.toBeInTheDocument();
  });
});
