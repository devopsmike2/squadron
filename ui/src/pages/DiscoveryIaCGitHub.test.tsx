// Vitest coverage for the v0.89.3 #603 Stream 19 DiscoveryIaCGitHub
// page (the connections list at /discovery/iac/github).
//
// Scope:
//   - Empty state when no connections exist.
//   - List rendering when connections exist (2 rows).
//   - The Delete button surfaces the confirm dialog and calls the API.
//
// v0.89.4 (#610) — deep-link query-param coverage. Adds:
//   - DiscoveryIaCGitHub_renders_connections_list_when_no_query_params
//     (regression guard for the bare-URL path).
//   - DiscoveryIaCGitHub_auto_opens_wizard_at_placement_step_when_connection_id_and_step_present
//   - DiscoveryIaCGitHub_focuses_target_row_when_kind_param_matches_canonical_kind
//   - DiscoveryIaCGitHub_ignores_kind_param_when_kind_is_unknown
//   - DiscoveryIaCGitHub_shows_stale_link_notice_when_connection_id_does_not_exist

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
  const actual =
    await vi.importActual<typeof import("@/api/iacGithub")>("@/api/iacGithub");
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

function renderPage(initialEntries: string[] = ["/discovery/iac/github"]) {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <MemoryRouter initialEntries={initialEntries}>{children}</MemoryRouter>
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
    expect(screen.getByText(/never your default branch/i)).toBeInTheDocument();
  });

  it("renders two connection cards when populated", async () => {
    mockedList.mockResolvedValue({ connections: sampleConnections });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("octo/infra")).toBeInTheDocument();
    });
    expect(screen.getByText("octo/platform")).toBeInTheDocument();
    // Layout badges visible per row.
    expect(
      within(document.body).getAllByText(/^multi$/).length,
    ).toBeGreaterThan(0);
    expect(within(document.body).getAllByText(/^mono$/).length).toBeGreaterThan(
      0,
    );
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
      expect(screen.getByText(/Delete IaC connection/i)).toBeInTheDocument();
    });
    // The confirm "Delete" button in the modal footer (not the row's).
    const deleteButtons = screen.getAllByRole("button", { name: /^Delete$/i });
    fireEvent.click(deleteButtons[deleteButtons.length - 1]);

    await waitFor(() => {
      expect(mockedDelete).toHaveBeenCalledWith("conn-1");
    });
  });

  // --- v0.89.4 #610 deep-link coverage --------------------------------

  it("DiscoveryIaCGitHub_renders_connections_list_when_no_query_params", async () => {
    // Regression guard: the bare /discovery/iac/github URL (no query
    // params) still renders the connections list AND does not auto-
    // open the wizard. If this fires the deep-link plumbing has
    // silently shifted the no-params posture.
    mockedList.mockResolvedValue({ connections: sampleConnections });
    renderPage(["/discovery/iac/github"]);
    await waitFor(() => {
      expect(screen.getByText("octo/infra")).toBeInTheDocument();
    });
    // The wizard's "Edit placement map" dialog title would appear if
    // the wizard auto-opened in edit mode; the create-flow dialog
    // title would appear if it opened in create mode. Neither is
    // expected with no query params.
    expect(
      screen.queryByRole("dialog", { name: /Edit placement map/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("dialog", { name: /Connect IaC repository/i }),
    ).not.toBeInTheDocument();
  });

  it("DiscoveryIaCGitHub_auto_opens_wizard_at_placement_step_when_connection_id_and_step_present", async () => {
    // The deep-link triple { connection_id, step=placement } is
    // enough to auto-open the wizard at the placement-only edit
    // shell. No ?kind=... → no focused row (separate test below).
    mockedList.mockResolvedValue({ connections: sampleConnections });
    renderPage(["/discovery/iac/github?connection_id=conn-1&step=placement"]);
    // The placement-only-edit dialog mounts with the "Edit
    // placement map" title (a string only the edit shell renders).
    await waitFor(() => {
      expect(
        screen.getByRole("dialog", { name: /Edit placement map/i }),
      ).toBeInTheDocument();
    });
    // The seeded row's file_path lands prefilled in the placement
    // input. The deep-link's promise is that operators see their
    // existing rows without re-typing them.
    expect(
      screen.getByDisplayValue("modules/lambda/main.tf"),
    ).toBeInTheDocument();
  });

  it("DiscoveryIaCGitHub_focuses_target_row_when_kind_param_matches_canonical_kind", async () => {
    // With a canonical ?kind=<...> param, the matching row in the
    // placement list carries data-focused="true" so the operator's
    // eye lands on the right row without scanning seven.
    mockedList.mockResolvedValue({ connections: sampleConnections });
    renderPage([
      "/discovery/iac/github?connection_id=conn-2&step=placement&kind=eks-observability-addon",
    ]);
    await waitFor(() => {
      expect(
        screen.getByRole("dialog", { name: /Edit placement map/i }),
      ).toBeInTheDocument();
    });
    const focusedRow = screen.getByTestId(
      "iac-github-placement-row-eks-observability-addon",
    );
    expect(focusedRow.getAttribute("data-focused")).toBe("true");
    // None of the other rows carry the focused marker.
    const otherRow = screen.getByTestId(
      "iac-github-placement-row-eks-cluster-logging",
    );
    expect(otherRow.getAttribute("data-focused")).toBeNull();
  });

  it("DiscoveryIaCGitHub_ignores_kind_param_when_kind_is_unknown", async () => {
    // An unknown kind is silently dropped: the wizard still opens at
    // the placement step (because connection_id + step=placement are
    // valid), but no row carries the focused marker.
    mockedList.mockResolvedValue({ connections: sampleConnections });
    renderPage([
      "/discovery/iac/github?connection_id=conn-1&step=placement&kind=not-a-real-kind",
    ]);
    await waitFor(() => {
      expect(
        screen.getByRole("dialog", { name: /Edit placement map/i }),
      ).toBeInTheDocument();
    });
    // No data-focused="true" anywhere in the document.
    expect(document.querySelector("[data-focused='true']")).toBeNull();
  });

  it("DiscoveryIaCGitHub_shows_stale_link_notice_when_connection_id_does_not_exist", async () => {
    // The deep link points at a connection that doesn't exist in
    // the SWR list. The page shows the stale-link banner AND
    // renders the connections list so the operator has a path
    // forward (pick a different row or re-run the wizard). The
    // wizard itself does NOT auto-open.
    mockedList.mockResolvedValue({ connections: sampleConnections });
    renderPage([
      "/discovery/iac/github?connection_id=conn-deleted&step=placement&kind=lambda-otel-layer",
    ]);
    await waitFor(() => {
      expect(screen.getByText(/no longer exists/i)).toBeInTheDocument();
    });
    // The connection list still renders.
    expect(screen.getByText("octo/infra")).toBeInTheDocument();
    // The edit-placement dialog did NOT open.
    expect(
      screen.queryByRole("dialog", { name: /Edit placement map/i }),
    ).not.toBeInTheDocument();
  });
});
