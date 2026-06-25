import { render } from "@testing-library/react";
import { SWRConfig } from "swr";
import { describe, it, expect } from "vitest";

import App from "./App";

// SWR has no app-level <SWRConfig>, so tests must isolate the cache and
// disable focus/reconnect revalidation. Otherwise SWR's default focus
// listener calls document.visibilityState after jsdom tears the
// environment down, surfacing as "document is not defined" unhandled
// rejections (a teardown race, not a real failure).
const swrTestConfig = {
  provider: () => new Map(),
  revalidateOnFocus: false,
  revalidateOnReconnect: false,
};
const renderApp = () =>
  render(
    <SWRConfig value={swrTestConfig}>
      <App />
    </SWRConfig>,
  );

describe("App", () => {
  it("renders without crashing", () => {
    renderApp();
    expect(document.body).toBeInTheDocument();
  });

  it("renders app container", () => {
    const { container } = renderApp();
    expect(container.firstChild).toBeTruthy();
  });
});
