// Vitest coverage for the v0.89.3 #603 Stream 19 IaCGitHubWizard.
//
// Scope: load-bearing flow + UX moments the human partner called out
// explicitly:
//   - Happy path: walk all 6 steps; final POST body shape matches
//     the wizard's collected state, including repo_layout.
//   - Validate-error path: a 4xx response from /iac/github/validate
//     renders the humanized error AND the jump-back button.
//   - Placement-step placeholders flip with repo_layout (mono | multi).
//   - PAT field never round-trips through localStorage or sessionStorage.
//
// We mock the iacGithub API module at the boundary the wizard uses
// (validateIaCGitHub + saveIaCGitHubConnection) so tests don't reach
// the network.

import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { IaCGitHubWizard } from "./IaCGitHubWizard";

import {
  saveIaCGitHubConnection,
  updateIaCGitHubConnection,
  validateIaCGitHub,
  type IaCGitHubValidateResponse,
} from "@/api/iacGithub";

// Same Radix shim DiscoveryAWS.test.tsx uses — Switch reaches for
// pointer-capture APIs jsdom does not implement.
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
    validateIaCGitHub: vi.fn(),
    saveIaCGitHubConnection: vi.fn(),
    updateIaCGitHubConnection: vi.fn(),
    listIaCGitHubConnections: vi.fn(),
    deleteIaCGitHubConnection: vi.fn(),
  };
});

const mockedValidate = vi.mocked(validateIaCGitHub);
const mockedSave = vi.mocked(saveIaCGitHubConnection);
const mockedUpdate = vi.mocked(updateIaCGitHubConnection);

// Test helper — walk the webhook-secret step quickly with the
// "Use the global env-var secret" choice. The pre-v0.89.32 tests
// expected the PAT step to be followed immediately by the pick-repo
// step; the new step inserts between them so each pre-existing test
// needs to traverse it. Tests that exercise the new step explicitly
// use the more precise pickWebhookSourceGenerate / pickWebhookSourceSkip
// helpers below.
function pickWebhookSourceUseGlobal() {
  fireEvent.click(
    screen.getByRole("radio", { name: /Use the global env-var secret/i }),
  );
}
function pickWebhookSourceGenerate() {
  fireEvent.click(
    screen.getByRole("radio", { name: /Generate a new secret/i }),
  );
}
function pickWebhookSourceSkip() {
  fireEvent.click(
    screen.getByRole("radio", { name: /Skip and configure later/i }),
  );
}

// Standard happy-path validate response — repo OK, all 7 rows
// preflighted.
function happyValidateResponse(): IaCGitHubValidateResponse {
  return {
    repo_full_name: "my-org/infra",
    default_branch: "main",
    preflight_results: [
      {
        provider: "aws",
        resource_kind: "lambda-otel-layer",
        file_path: "modules/lambda/main.tf",
        exists: true,
        sha_short: "abcd123",
      },
    ],
  };
}

describe("IaCGitHubWizard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders the first step (provider) with GitHub preselected", () => {
    render(<IaCGitHubWizard onComplete={vi.fn()} />);
    expect(screen.getByText("Pick an IaC provider")).toBeInTheDocument();
    // GitHub tile shown; GitLab + Bitbucket disabled with the slice 2 badge.
    expect(screen.getByText("GitHub")).toBeInTheDocument();
    expect(screen.getByText("GitLab")).toBeInTheDocument();
    expect(screen.getByText("Bitbucket")).toBeInTheDocument();
    // Two slice-2 badges (GitLab + Bitbucket).
    expect(screen.getAllByText(/slice 2/i).length).toBeGreaterThanOrEqual(2);
  });

  it("happy-path: walks all 8 steps and posts a save body with repo_layout=multi", async () => {
    mockedValidate.mockResolvedValue(happyValidateResponse());
    mockedSave.mockResolvedValue({
      connection_id: "conn-uuid",
      repo_full_name: "my-org/infra",
      status: "connected",
    });
    const onComplete = vi.fn();
    render(<IaCGitHubWizard onComplete={onComplete} />);

    // Step 1: provider. Click Next.
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 2: PAT. Paste a value, then Next.
    expect(screen.getByText(/Authenticate with GitHub/i)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/GitHub Personal Access Token/i), {
      target: { value: "ghp_test1234567890" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 2.5: webhook-secret (v0.89.32). Pick "use global" — the
    // load-bearing fact this test checks is the existing save-body
    // shape, NOT the per-connection PATCH. (The generate-flow PATCH
    // is asserted in its own test below.)
    expect(screen.getByText(/Set up the webhook secret/i)).toBeInTheDocument();
    pickWebhookSourceUseGlobal();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 3: pick repo.
    fireEvent.change(screen.getByLabelText(/Repository full name/i), {
      target: { value: "my-org/infra" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 4: layout. Default is multi; leave default_branch as "main".
    expect(screen.getByText(/Describe the repository/i)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 5: placement map. Fill one path.
    expect(
      screen.getByText(/Map resource kinds to files/i),
    ).toBeInTheDocument();
    fireEvent.change(
      screen.getByLabelText(/File path for lambda-otel-layer/i),
      { target: { value: "modules/lambda/main.tf" } },
    );
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 5.5: configure-github-webhook walkthrough (v0.89.32). Read-only
    // — Next advances when the operator confirms they did the setup.
    expect(
      screen.getByText(/Configure the webhook on GitHub/i),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 6: validate.
    expect(screen.getByText(/Validate and save/i)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /^Validate$/i }));
    await waitFor(() => {
      expect(screen.getByText("What just happened")).toBeInTheDocument();
    });

    // Save. The Save button is in the Save panel under the preflight.
    fireEvent.click(screen.getByRole("button", { name: /Save connection/i }));
    await waitFor(() => {
      expect(onComplete).toHaveBeenCalledTimes(1);
    });

    // v0.89.32 — use_global path does NOT call the per-connection
    // PATCH. The env-var fallback is the operator's signal.
    expect(mockedUpdate).not.toHaveBeenCalled();

    // Assert validate body shape — repo_full_name and placement_map.
    expect(mockedValidate).toHaveBeenCalledTimes(1);
    const vBody = mockedValidate.mock.calls[0][0];
    expect(vBody.token).toBe("ghp_test1234567890");
    expect(vBody.repo_full_name).toBe("my-org/infra");
    // default_branch === "main" → wizard omits it so server auto-detects.
    expect(vBody.default_branch).toBeUndefined();
    expect(vBody.placement_map).toEqual([
      {
        provider: "aws",
        resource_kind: "lambda-otel-layer",
        file_path: "modules/lambda/main.tf",
      },
    ]);

    // Assert save body shape — repo_layout was the load-bearing
    // addition the human partner explicitly asked for.
    expect(mockedSave).toHaveBeenCalledTimes(1);
    const sBody = mockedSave.mock.calls[0][0];
    expect(sBody.token).toBe("ghp_test1234567890");
    expect(sBody.repo_full_name).toBe("my-org/infra");
    expect(sBody.repo_layout).toBe("multi");
    expect(sBody.default_branch).toBe("main");
    expect(sBody.placement_map).toEqual([
      {
        provider: "aws",
        resource_kind: "lambda-otel-layer",
        file_path: "modules/lambda/main.tf",
      },
    ]);
    expect(sBody.branch_prefix).toBeUndefined();
    expect(sBody.reviewer_team_handle).toBeUndefined();

    // Connected card lands.
    await waitFor(() => {
      expect(screen.getByText(/Repository connected/i)).toBeInTheDocument();
    });
  });

  it("placement-step placeholders flip with repo_layout", () => {
    render(<IaCGitHubWizard onComplete={vi.fn()} />);

    // Walk to step 5 (placement) keeping default repo_layout = multi.
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // provider → pat
    fireEvent.change(screen.getByLabelText(/GitHub Personal Access Token/i), {
      target: { value: "ghp_x" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // pat → webhook-secret
    pickWebhookSourceUseGlobal();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // → repo
    fireEvent.change(screen.getByLabelText(/Repository full name/i), {
      target: { value: "my-org/infra" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // repo → layout
    // Stay on the layout step to verify multi is the default tile.
    expect(screen.getByRole("button", { name: /Multi-repo/i })).toHaveAttribute(
      "aria-pressed",
      "true",
    );

    // Advance to placement. Multi placeholders use "modules/...".
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    const lambdaInput = screen.getByLabelText(
      /File path for lambda-otel-layer/i,
    );
    expect(lambdaInput).toHaveAttribute(
      "placeholder",
      "modules/lambda/main.tf",
    );

    // Go back to the layout step, switch to mono, advance again.
    fireEvent.click(screen.getByRole("button", { name: /Back/i }));
    fireEvent.click(screen.getByRole("button", { name: /Mono-repo/i }));
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    const lambdaInputMono = screen.getByLabelText(
      /File path for lambda-otel-layer/i,
    );
    expect(lambdaInputMono).toHaveAttribute(
      "placeholder",
      "environments/prod/lambda/main.tf",
    );
  });

  it("validation error renders the humanized message + jump-back button", async () => {
    mockedValidate.mockResolvedValue({
      repo_full_name: "my-org/infra",
      default_branch: "",
      repo_err: {
        code: "AuthFailed",
        message:
          "GitHub rejected the token. Re-paste the value; ensure the repo scope is checked.",
        suggested_step: "pat",
      },
      preflight_results: [],
      errors: [
        {
          code: "AuthFailed",
          message:
            "GitHub rejected the token. Re-paste the value; ensure the repo scope is checked.",
          suggested_step: "pat",
        },
      ],
    });

    render(<IaCGitHubWizard onComplete={vi.fn()} />);

    // Skip through to the validate step quickly.
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.change(screen.getByLabelText(/GitHub Personal Access Token/i), {
      target: { value: "ghp_bad" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // pat → webhook-secret
    pickWebhookSourceUseGlobal();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // → pick-repo
    fireEvent.change(screen.getByLabelText(/Repository full name/i), {
      target: { value: "my-org/infra" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // layout
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // placement
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // configure-github-webhook
    expect(screen.getByText(/Validate and save/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^Validate$/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/GitHub rejected the token/i),
      ).toBeInTheDocument();
    });

    // Jump button is rendered and navigates the wizard back to the
    // PAT step.
    const jumpBtn = screen.getByRole("button", {
      name: /Return to: Authenticate with GitHub/i,
    });
    fireEvent.click(jumpBtn);
    expect(screen.getByText(/Authenticate with GitHub/i)).toBeInTheDocument();
    // The PAT input is visible — the operator can re-paste.
    expect(
      screen.getByLabelText(/GitHub Personal Access Token/i),
    ).toBeInTheDocument();

    // Save is NOT enabled (no successful validate).
    const saveCandidates = screen.queryAllByRole("button", {
      name: /Save connection/i,
    });
    expect(saveCandidates.length).toBe(0);
  });

  it("PAT input never writes to localStorage or sessionStorage", () => {
    // Sanity test for the token-discipline invariant: the wizard
    // captures the PAT in component state ONLY. We snapshot
    // localStorage / sessionStorage before and after the keystroke and
    // assert no key landed.
    const lsBefore = { ...localStorage };
    const ssBefore = { ...sessionStorage };

    render(<IaCGitHubWizard onComplete={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.change(screen.getByLabelText(/GitHub Personal Access Token/i), {
      target: { value: "ghp_supersecret" },
    });

    expect({ ...localStorage }).toEqual(lsBefore);
    expect({ ...sessionStorage }).toEqual(ssBefore);

    // Sanity check: the field itself has type=password and autoComplete off.
    const input = screen.getByLabelText(/GitHub Personal Access Token/i);
    expect(input).toHaveAttribute("type", "password");
    expect(input).toHaveAttribute("autocomplete", "off");
  });

  it("placement step supports skip-all and bulk-apply pattern", () => {
    render(<IaCGitHubWizard onComplete={vi.fn()} />);
    // Skip ahead to placement.
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.change(screen.getByLabelText(/GitHub Personal Access Token/i), {
      target: { value: "ghp_x" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // pat → webhook-secret
    pickWebhookSourceUseGlobal();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // → pick-repo
    fireEvent.change(screen.getByLabelText(/Repository full name/i), {
      target: { value: "my-org/infra" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Apply pattern.
    fireEvent.change(screen.getByLabelText(/Bulk pattern/i), {
      target: { value: "modules/{kind}/main.tf" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Apply pattern/i }));
    const ec2Input = screen.getByLabelText(/File path for ec2-otel-layer/i);
    expect(ec2Input).toHaveValue("modules/ec2-otel-layer/main.tf");

    // Skip all.
    fireEvent.click(screen.getByRole("button", { name: /Skip all/i }));
    expect(
      within(document.body).getAllByText(/skipped/i).length,
    ).toBeGreaterThan(0);
  });

  // -------------------------------------------------------------
  // v0.89.32 #651 Stream 49 — webhook-secret wizard step.
  // -------------------------------------------------------------
  //
  // The three tests below pin down each of the operator choices on
  // the new "Set up the webhook secret" step:
  //   - Generate → connection created + per-connection PATCH fires.
  //   - Use global → connection created + NO PATCH (env-var fallback).
  //   - Skip → connection created + NO PATCH + success card mentions
  //     deferred follow-up.
  // The configure-github-webhook step is honored on the generate +
  // use_global paths and elided on the skip path. The shared
  // post-Save success card surface flexes per source mode.

  // walkToWebhookSecretStep advances the wizard from a fresh mount
  // to the webhook-secret step with a token entered. Saves repetition
  // across the three new flows below.
  function walkToWebhookSecretStep() {
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.change(screen.getByLabelText(/GitHub Personal Access Token/i), {
      target: { value: "ghp_test" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    expect(screen.getByText(/Set up the webhook secret/i)).toBeInTheDocument();
  }

  // Re-used by the generate + use-global flows. The skip flow has its
  // own simpler variant because it elides the configure-webhook step.
  function walkFromWebhookSecretToValidate() {
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.change(screen.getByLabelText(/Repository full name/i), {
      target: { value: "my-org/infra" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // layout
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // placement
    // configure-github-webhook walkthrough — Next advances past it.
    expect(
      screen.getByText(/Configure the webhook on GitHub/i),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    expect(screen.getByText(/Validate and save/i)).toBeInTheDocument();
  }

  function validateAndSave() {
    fireEvent.click(screen.getByRole("button", { name: /^Validate$/i }));
    return waitFor(() => {
      expect(screen.getByText("What just happened")).toBeInTheDocument();
    }).then(() => {
      fireEvent.click(screen.getByRole("button", { name: /Save connection/i }));
    });
  }

  it("TestIaCWizard_GenerateSecretFlow_PatchesConnection", async () => {
    mockedValidate.mockResolvedValue(happyValidateResponse());
    mockedSave.mockResolvedValue({
      connection_id: "conn-gen",
      repo_full_name: "my-org/infra",
      status: "connected",
    });
    mockedUpdate.mockResolvedValue({
      connection_id: "conn-gen",
      status: "updated",
    });

    render(<IaCGitHubWizard onComplete={vi.fn()} />);

    walkToWebhookSecretStep();

    // Pick "Generate". The wizard mints a 64-char hex secret + shows it.
    pickWebhookSourceGenerate();
    const display = await screen.findByTestId(
      "iac-github-webhook-secret-display",
    );
    const displayedSecret = display.textContent ?? "";
    expect(displayedSecret).toMatch(/^[a-f0-9]{64}$/);

    // Next is disabled until the operator acknowledges.
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeDisabled();

    // Tick the acknowledgment checkbox. Next enables.
    fireEvent.click(
      screen.getByLabelText(/I have saved this secret securely/i),
    );
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();

    walkFromWebhookSecretToValidate();
    await validateAndSave();

    // Two-stage submit: createConnection then updateConnection.
    await waitFor(() => {
      expect(mockedSave).toHaveBeenCalledTimes(1);
    });
    await waitFor(() => {
      expect(mockedUpdate).toHaveBeenCalledTimes(1);
    });
    const [connID, body] = mockedUpdate.mock.calls[0];
    expect(connID).toBe("conn-gen");
    expect(body.webhook_secret).toBe(displayedSecret);

    // Success card confirms the secret was stored — and does NOT
    // re-display the plaintext (one-time-display contract).
    expect(
      await screen.findByTestId("iac-github-webhook-secret-stored"),
    ).toBeInTheDocument();
    expect(screen.queryByText(displayedSecret)).not.toBeInTheDocument();
  });

  it("TestIaCWizard_UseGlobalFlow_SkipsPatch", async () => {
    mockedValidate.mockResolvedValue(happyValidateResponse());
    mockedSave.mockResolvedValue({
      connection_id: "conn-glob",
      repo_full_name: "my-org/infra",
      status: "connected",
    });

    render(<IaCGitHubWizard onComplete={vi.fn()} />);

    walkToWebhookSecretStep();
    pickWebhookSourceUseGlobal();

    // No secret display.
    expect(
      screen.queryByTestId("iac-github-webhook-secret-display"),
    ).not.toBeInTheDocument();

    // Next enables immediately — no acknowledgment needed.
    expect(screen.getByRole("button", { name: /^Next$/i })).toBeEnabled();

    walkFromWebhookSecretToValidate();
    await validateAndSave();

    await waitFor(() => {
      expect(mockedSave).toHaveBeenCalledTimes(1);
    });
    // NO PATCH on the use_global path — env-var fallback is the signal.
    expect(mockedUpdate).not.toHaveBeenCalled();

    expect(
      await screen.findByTestId("iac-github-webhook-use-global"),
    ).toBeInTheDocument();
  });

  it("TestIaCWizard_SkipFlow_SkipsPatchAndShowsReminder", async () => {
    mockedValidate.mockResolvedValue(happyValidateResponse());
    mockedSave.mockResolvedValue({
      connection_id: "conn-skip",
      repo_full_name: "my-org/infra",
      status: "connected",
    });

    render(<IaCGitHubWizard onComplete={vi.fn()} />);

    walkToWebhookSecretStep();
    pickWebhookSourceSkip();

    // Skip path elides the configure-github-webhook step. Walk the
    // remaining steps directly to validate; we should NOT pass through
    // the "Configure the webhook on GitHub" title at any point.
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // → pick-repo
    fireEvent.change(screen.getByLabelText(/Repository full name/i), {
      target: { value: "my-org/infra" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // → layout
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // → placement
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i })); // skipped over → validate
    expect(screen.getByText(/Validate and save/i)).toBeInTheDocument();
    expect(
      screen.queryByText(/Configure the webhook on GitHub/i),
    ).not.toBeInTheDocument();

    await validateAndSave();

    await waitFor(() => {
      expect(mockedSave).toHaveBeenCalledTimes(1);
    });
    expect(mockedUpdate).not.toHaveBeenCalled();

    // Success card surfaces the deferred reminder.
    const deferred = await screen.findByTestId("iac-github-webhook-deferred");
    expect(deferred.textContent).toMatch(/configure later|deferred/i);
  });
});
