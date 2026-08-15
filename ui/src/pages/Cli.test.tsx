import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, it, expect } from "vitest";

import CliPage from "./Cli";

const renderPage = () =>
  render(
    <MemoryRouter>
      <CliPage />
    </MemoryRouter>,
  );

describe("CliPage", () => {
  it("lists the per-platform release assets", () => {
    renderPage();
    // Table-only assets (the curl example only references darwin-arm64).
    expect(screen.getByText("squadronctl-linux-amd64")).toBeInTheDocument();
    expect(screen.getByText("squadronctl-linux-arm64")).toBeInTheDocument();
    expect(
      screen.getByText("squadronctl-windows-amd64.exe"),
    ).toBeInTheDocument();
    expect(
      screen.getAllByText("squadronctl-darwin-arm64").length,
    ).toBeGreaterThan(0);
  });

  it("links to the API tokens page and the releases page", () => {
    renderPage();
    const tokenLink = screen.getByRole("link", { name: /API tokens/ });
    expect(tokenLink).toHaveAttribute("href", "/settings/tokens");

    const releasesLink = screen.getByRole("link", { name: /latest release/ });
    expect(releasesLink).toHaveAttribute(
      "href",
      "https://github.com/devopsmike2/squadron/releases/latest",
    );
  });

  it("renders the example workflow commands as copyable snippets", () => {
    renderPage();
    expect(screen.getByText("squadronctl agents list")).toBeInTheDocument();
    expect(
      screen.getByText(
        "squadronctl groups assign-config <group-id> --config <config-id>",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText("squadronctl agents get <agent-id> --effective"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("squadronctl agents restart <agent-id>"),
    ).toBeInTheDocument();
  });
});
