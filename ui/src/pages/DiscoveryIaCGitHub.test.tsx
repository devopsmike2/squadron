// Vitest coverage for the v0.89.3 #603 Stream 19 DiscoveryIaCGitHub
// page (the connections list at /discovery/iac/github).
//
// Scope:
//   - Empty state when no connections exist.
//   - List rendering when connections exist (2 rows).
//   - The Delete button surfaces the confirm dialog and calls the API.

import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { SWRConfig } from "swr";
import { beforeEach, describe, expect, it, vi } from "vitest";

import DiscoveryIaCGitHubPage from "./DiscoveryIaCGitHub";

import {
  deleteIaCGitHubConnection,
  listIaCGitHubConnections,
  type IaCGitHubConnection,
} from "@/api/iacGithub";

// Polyfill the pointer-capture lookups Radix Dialog / Switch reach for
// before the test exercises any user interaction.
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false;
}
if (!Element.prototype.releasePointerCapture) {
  Element.prototype.releasePointerCapture = () => {};
}
if (!Element.prototype.setPointerCapture) {
  Element.prototype.setPointerCapture = () => {};
}

vi.mock("@/api/iacGithub", async () => {
  const actual = await vi.importActual<typeof import("@/api/iacGithub")>(
    "@/api/iacGithub",
  );
  return {
    ...actual,
    listIaCGitHubConnections: vi.fn(),
    deleteIaCGitHubConnection: vi.fn(),
    validateIaCGitHub: vi.fn(),
    saveIaCGitHubConnection: vi.fn(),
  };
});

const mockedList = vi.mocked(listIaCGitHubConnections);
const mockedDelete = vi.mocked(deleteIaCGitHubConnection);

function renderPage() {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter>{children}</MemoryRouter>
      </SWRConfig>
    );
  }
  return render(<DiscoveryIaCGitHubPage />, { wrapper: Wrapper });
}

const sampleConnections: IaCGitHubConnection[] = [
  {
    connection_id: "conn-1",
    provider: "github",
    auth_kind: "pat",
    repo_full_name: "octo/infra",
    default_branch: "main",
    repo_layout: "multi",
    branch_prefix: "",
    reviewer_team_handle: "",
    placement_map: [
      {
        provider: "aws",
        resource_kind: "lambda-otel-layer",
        file_path: "modules/lambda/main.tf",
      },
    ],
    created_at: new Date().toISOString(),
  },
  {
    connection_id: "conn-2",
    provider: "github",
    auth_kind: "pat",
    repo_full_name: "octo/platform",
    default_branch: "trunk",
    repo_layout: "mono",
    branch_prefix: "sq/rec",
    reviewer_team_handle: "octo/platform-reviewers",
    placement_map: [
      {
        provider: "aws",
        resource_kind: "eks-cluster-logging",
        file_path: "environments/prod/eks/main.tf",
      },
      {
        provider: "aws",
        resource_kind: "eks-observability-addon",
        file_path: "environments/prod/eks/addons.tf",
      },
    ],
    created_at: new Date().toISOString(),
  },
];

describe("DiscoveryIaCGitHubPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders the empty state when no connections exist", async () => {
    mockedList.mockResolvedValue({ connections: [] });
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByText(/No IaC repositories connected yet/i),
      ).toBeInTheDocument();
    });
    // CTA button is present.
    expect(
      screen.getByRole("button", { name: /Connect IaC repo/i }),
    ).toBeInTheDocument();
    // Read-only / branch-protection reassurance reinforces the
    // trust thesis at the empty state.
    expect(
      screen.getByText(/never your default branch/i),
    ).toBeInTheDocument();
  });

  it("renders two connection cards when populated", async () => {
    mockedList.mockResolvedValue({ connections: sampleConnections });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("octo/infra")).toBeInTheDocument();
    });
    expect(screen.getByText("octo/platform")).toBeInTheDocument();
    // Layout badges visible per row.
    expect(within(document.body).getAllByText(/^multi$/).length).toBeGreaterThan(0);
    expect(within(document.body).getAllByText(/^mono$/).length).toBeGreaterThan(0);
    // Per-row placement counts visible — uses pluralized labels.
    expect(screen.getByText(/1 placement/)).toBeInTheDocument();
    expect(screen.getByText(/2 placements/)).toBeInTheDocument();
    // Reviewer team handle visible on the row that set it.
    expect(screen.getByText(/octo\/platform-reviewers/)).toBeInTheDocument();
  });

  it("delete confirm modal calls the API and refreshes", async () => {
    mockedList.mockResolvedValue({ connections: sampleConnections });
    mockedDelete.mockResolvedValue(undefined);
    renderPage();

    await waitFor(() => {
      expect(screen.getByText("octo/infra")).toBeInTheDocument();
    });

    // Per-row delete button uses an aria-label that names the repo.
    fireEvent.click(
      screen.getByRole("button", {
        name: /Delete connection for octo\/infra/i,
      }),
    );
    // Confirm modal renders the repo in the body.
    await waitFor(() => {
      expect(
        screen.getByText(/Delete IaC connection/i),
      ).toBeInTheDocument();
    });
    // The confirm "Delete" button in the modal footer (not the row's).
    const deleteButtons = screen.getAllByRole("button", { name: /^Delete$/i });
    fireEvent.click(deleteButtons[deleteButtons.length - 1]);

    await waitFor(() => {
      expect(mockedDelete).toHaveBeenCalledWith("conn-1");
    });
  });
});
