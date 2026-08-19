// Vitest coverage for the Groups page labels cell — the Southern-pilot
// blank-page field bug (v0.89.472).
//
// GET /api/v1/groups returned `labels: null` for the `southern-pilot`
// group. The Groups table row cell called `Object.entries(group.labels)`
// with no null guard, so `Object.entries(null)` threw
// `TypeError: Cannot convert undefined or null to object`, which — with
// nothing catching it — unmounted the whole app shell to a blank page.
//
// These tests render the real GroupsPage against a mocked groups API
// with three groups: one whose labels are `null`, one `{}`, and one
// populated. On unfixed source the null-labels row throws during render
// and the test goes RED; the `?? {}` guard makes it GREEN while still
// rendering "No labels".
//
// SWR's cache is per-provider; each render gets a fresh SWRConfig
// (provider: () => new Map()) so a prior test's mocked list can't leak.

import { render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { SWRConfig } from "swr";
import { beforeEach, describe, expect, it, vi } from "vitest";

import GroupsPage from "./Groups";

import { getGroups, createGroup, deleteGroup, updateGroup } from "@/api/groups";
import type { Group } from "@/api/groups";

// jsdom polyfills for any Radix pointer-capture / scroll lookups the
// drawer/modal chrome touches on mount (they render closed, but the
// providers still instantiate). Mirrors the DiscoveryAWS test shim.
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false;
}
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

vi.mock("@/api/groups", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/groups")>("@/api/groups");
  return {
    ...actual,
    getGroups: vi.fn(),
    createGroup: vi.fn(),
    deleteGroup: vi.fn(),
    updateGroup: vi.fn(),
  };
});

const mockedGetGroups = vi.mocked(getGroups);
void createGroup;
void deleteGroup;
void updateGroup;

function makeGroup(overrides: Partial<Group>): Group {
  return {
    id: "id",
    name: "group",
    // Cast through unknown so a test can put `null` on the wire even
    // though the Group type declares labels as a non-null record — the
    // whole point is that a real backend regression did exactly that.
    labels: {} as Record<string, string>,
    agent_count: 0,
    created_at: new Date("2026-01-01T00:00:00Z").toISOString(),
    updated_at: new Date("2026-01-02T00:00:00Z").toISOString(),
    ...overrides,
  };
}

function renderPage() {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig
        value={{
          provider: () => new Map(),
          dedupingInterval: 0,
          revalidateOnFocus: false,
          revalidateOnReconnect: false,
        }}
      >
        <MemoryRouter initialEntries={["/groups"]}>{children}</MemoryRouter>
      </SWRConfig>
    );
  }
  return render(<GroupsPage />, { wrapper: Wrapper });
}

describe("GroupsPage labels cell", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders without crashing when a group's labels are null and shows 'No labels'", async () => {
    // The southern-pilot repro: labels arrived as null on the wire.
    const nullLabelsGroup = makeGroup({
      id: "southern-pilot",
      name: "Southern Pilot",
      labels: null as unknown as Record<string, string>,
    });
    const emptyLabelsGroup = makeGroup({
      id: "empty",
      name: "Empty Labels",
      labels: {},
    });
    const populatedGroup = makeGroup({
      id: "web-prod",
      name: "Web Prod",
      labels: { env: "prod", team: "sre" },
    });

    mockedGetGroups.mockResolvedValue({
      groups: [nullLabelsGroup, emptyLabelsGroup, populatedGroup],
      count: 3,
    });

    renderPage();

    // The table renders all three rows without throwing. On unfixed
    // source the null-labels row's Object.entries(null) throws here.
    await waitFor(() => {
      expect(screen.getByText("Southern Pilot")).toBeInTheDocument();
    });
    expect(screen.getByText("Empty Labels")).toBeInTheDocument();
    expect(screen.getByText("Web Prod")).toBeInTheDocument();

    // Populated group shows its label chips.
    expect(screen.getByText("env=prod")).toBeInTheDocument();
    expect(screen.getByText("team=sre")).toBeInTheDocument();

    // Both the null-labels and empty-labels groups fall through to the
    // "No labels" empty state — exactly two of them.
    expect(screen.getAllByText("No labels")).toHaveLength(2);
  });
});
