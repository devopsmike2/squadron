// Vitest coverage for the v0.85 Stream 2D ConnectorWizard shell.
//
// Scope: the slice-1 AWS wizard happy path + the suggested_step jump
// UX. These tests cover the load-bearing UX the design doc's eleven
// principles call out — inline validation enabling Next, the
// "what just happened" panel, and the humanized-error jump button.
// They do NOT exercise crypto.randomUUID() (the shell stubs it
// gracefully); the integration test elsewhere covers that path.

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import {
  ConnectorWizard,
  effectiveExternalId,
  renderTrustPolicy,
} from "./ConnectorWizard";

import {
  type SaveConnectionRequest,
  type ValidateRequest,
  type ValidationResult,
} from "@/api/discovery";
import { awsWizard, AWS_PERMISSIONS_POLICY_TEMPLATE } from "@/data/awsWizard";


// makeProps builds default props for each test, defaulting onValidate
// to a successful result and onSave to a 1-row response. Tests
// override the callbacks they care about.
function makeProps(overrides?: {
  onValidate?: (req: ValidateRequest) => Promise<ValidationResult>;
  onSave?: (req: SaveConnectionRequest) => Promise<{ connection_id: string }>;
  onComplete?: (id: string) => void;
}) {
  return {
    wizard: awsWizard,
    onValidate:
      overrides?.onValidate ??
      vi.fn(
        async (): Promise<ValidationResult> => ({
          assume_role_ok: true,
          preflight: [
            { service: "ec2", ok: true, sample_count: 3 },
            { service: "lambda", ok: true, sample_count: 1 },
          ],
        }),
      ),
    onSave:
      overrides?.onSave ??
      vi.fn(async () => ({ connection_id: "123456789012" })),
    onComplete: overrides?.onComplete ?? vi.fn(),
  };
}

describe("ConnectorWizard", () => {
  it("renders the first step", () => {
    render(<ConnectorWizard {...makeProps()} />);
    expect(screen.getByText("Enter your AWS account ID")).toBeInTheDocument();
    // Stepper shows "Step 1 of 6" (v0.87.1 added permissions-policy).
    expect(screen.getByText(/Step 1 of 6/)).toBeInTheDocument();
  });

  it("Next button is disabled when the field is invalid", () => {
    render(<ConnectorWizard {...makeProps()} />);
    const next = screen.getByRole("button", { name: /Next/i });
    // Initial state: empty input — Next is disabled.
    expect(next).toBeDisabled();

    // A non-matching value (5 chars) keeps Next disabled and shows
    // the inline error message.
    const input = screen.getByPlaceholderText("123456789012");
    fireEvent.change(input, { target: { value: "12345" } });
    expect(next).toBeDisabled();
    expect(
      screen.getByText("Account ID must be exactly 12 digits."),
    ).toBeInTheDocument();
  });

  it("Next button enables on valid input", () => {
    render(<ConnectorWizard {...makeProps()} />);
    const input = screen.getByPlaceholderText("123456789012");
    fireEvent.change(input, { target: { value: "123456789012" } });
    const next = screen.getByRole("button", { name: /Next/i });
    expect(next).not.toBeDisabled();
  });

  it('shows the "what just happened" panel after a successful validate', async () => {
    const props = makeProps();
    render(<ConnectorWizard {...props} />);

    // Walk through to the validate step. Step 1: account ID.
    fireEvent.change(screen.getByPlaceholderText("123456789012"), {
      target: { value: "123456789012" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 2: trust policy (Next is always enabled on copy_value).
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 3: permissions policy (added in v0.87.1; Next always
    // enabled on copy_value).
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 4: role ARN.
    fireEvent.change(
      screen.getByPlaceholderText(
        "arn:aws:iam::123456789012:role/SquadronDiscovery",
      ),
      {
        target: {
          value: "arn:aws:iam::123456789012:role/SquadronDiscovery",
        },
      },
    );
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 5: validate. Click the Validate button and wait for the
    // result panel to land.
    fireEvent.click(screen.getByRole("button", { name: /Validate connection/i }));
    await waitFor(() => {
      expect(screen.getByText("What just happened")).toBeInTheDocument();
    });
    expect(screen.getByText("sts:AssumeRole")).toBeInTheDocument();
    expect(screen.getByText("ec2 probe")).toBeInTheDocument();
    expect(screen.getByText("lambda probe")).toBeInTheDocument();
    expect(props.onValidate).toHaveBeenCalledTimes(1);
  });

  it("validation error shows humanized message + jump button", async () => {
    const onValidate = vi.fn(async (): Promise<ValidationResult> => ({
      assume_role_ok: false,
      assume_role_err: {
        code: "AccessDenied",
        message: "The role's trust policy doesn't authorize Squadron's principal.",
        suggested_step: "trust-policy",
      },
      preflight: [],
    }));
    const props = makeProps({ onValidate });
    render(<ConnectorWizard {...props} />);

    // Walk to the validate step quickly — fill account ID + role ARN
    // and click Next four times. (Trust-policy and permissions-policy
    // steps have no input; v0.87.1 added permissions-policy after
    // trust-policy.)
    fireEvent.change(screen.getByPlaceholderText("123456789012"), {
      target: { value: "123456789012" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    fireEvent.change(
      screen.getByPlaceholderText(
        "arn:aws:iam::123456789012:role/SquadronDiscovery",
      ),
      {
        target: {
          value: "arn:aws:iam::123456789012:role/SquadronDiscovery",
        },
      },
    );
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    fireEvent.click(screen.getByRole("button", { name: /Validate connection/i }));
    await waitFor(() => {
      expect(
        screen.getByText(/trust policy doesn't authorize/i),
      ).toBeInTheDocument();
    });

    // Jump button surfaces the target step's title and, when
    // clicked, returns the operator to that step.
    const jumpBtn = screen.getByRole("button", {
      name: /Return to: Create the IAM role with this trust policy/i,
    });
    expect(jumpBtn).toBeInTheDocument();
    fireEvent.click(jumpBtn);
    expect(
      screen.getByText("Create the IAM role with this trust policy"),
    ).toBeInTheDocument();
  });

  // --- v0.87.1 hotfix coverage -------------------------------------

  it("renders the permissions-policy step body with EC2/Lambda/RDS actions", () => {
    render(<ConnectorWizard {...makeProps()} />);

    // Walk to step 3 (permissions-policy). Step 1: account ID.
    fireEvent.change(screen.getByPlaceholderText("123456789012"), {
      target: { value: "123456789012" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));
    // Step 2: trust policy.
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Step 3: permissions-policy is on screen — title, copy button,
    // and the actions list rendered verbatim from the template.
    expect(
      screen.getByText("Add this permissions policy to the role"),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Copy permissions policy/i }),
    ).toBeInTheDocument();
    expect(screen.getByText(/ec2:DescribeInstances/)).toBeInTheDocument();
    expect(screen.getByText(/lambda:ListFunctions/)).toBeInTheDocument();
    expect(screen.getByText(/rds:DescribeDBInstances/)).toBeInTheDocument();
  });
});

// --- renderTrustPolicy (#574) -------------------------------------

describe("renderTrustPolicy", () => {
  it("defaults the principal to arn:aws:iam::<account>:root", () => {
    const out = renderTrustPolicy("test-uuid", "111122223333");
    expect(out).toContain('"AWS": "arn:aws:iam::111122223333:root"');
    expect(out).toContain('"sts:ExternalId": "test-uuid"');
  });

  it("accepts a principal override verbatim", () => {
    const out = renderTrustPolicy(
      "test-uuid",
      "111122223333",
      "arn:aws:iam::111122223333:user/squadron-bot",
    );
    expect(out).toContain(
      '"AWS": "arn:aws:iam::111122223333:user/squadron-bot"',
    );
    // The account-root default must NOT appear when the override
    // takes precedence — guards against double-substitution.
    expect(out).not.toContain('"AWS": "arn:aws:iam::111122223333:root"');
  });

  it("falls back to root when the principal override is malformed", () => {
    // Garbage in -> account-root default, never the malformed value.
    const out = renderTrustPolicy("test-uuid", "111122223333", "not-an-arn");
    expect(out).toContain('"AWS": "arn:aws:iam::111122223333:root"');
    expect(out).not.toContain("not-an-arn");
  });

  it("leaves no <PRINCIPAL-PLACEHOLDER> or <UUID-PLACEHOLDER> in the output", () => {
    // Regression guard against the pre-v0.87.1 bug where the
    // placeholder shipped to the operator unchanged.
    const out = renderTrustPolicy("test-uuid", "111122223333");
    expect(out).not.toContain("<PRINCIPAL-PLACEHOLDER>");
    expect(out).not.toContain("<UUID-PLACEHOLDER>");
  });

  it("never contains the literal SQUADRON_ACCOUNT_ID", () => {
    // Regression guard against the original #574 bug pattern: the
    // template used to ship arn:aws:iam::SQUADRON_ACCOUNT_ID:role/...
    // as the principal and nothing substituted it.
    const out = renderTrustPolicy("test-uuid", "111122223333");
    expect(out).not.toContain("SQUADRON_ACCOUNT_ID");

    const outWithOverride = renderTrustPolicy(
      "test-uuid",
      "111122223333",
      "arn:aws:iam::111122223333:user/x",
    );
    expect(outWithOverride).not.toContain("SQUADRON_ACCOUNT_ID");
  });
});

// --- ExternalId resume (#578) -------------------------------------

describe("effectiveExternalId", () => {
  it("uses a well-formed override in place of the auto-generated value", () => {
    expect(
      effectiveExternalId(
        "auto-uuid",
        "12345678-1234-1234-1234-123456789012",
      ),
    ).toBe("12345678-1234-1234-1234-123456789012");
  });

  it("falls back to the auto-generated value when the override is malformed", () => {
    // Bad shape: not a UUID. Wizard must not break on bad input.
    expect(effectiveExternalId("auto-uuid", "obviously-not-a-uuid")).toBe(
      "auto-uuid",
    );
    // Bad shape: uppercase. We pin lowercase canonical form.
    expect(
      effectiveExternalId(
        "auto-uuid",
        "12345678-1234-1234-1234-12345678901A",
      ),
    ).toBe("auto-uuid");
  });

  it("falls back to the auto-generated value when the override is empty", () => {
    expect(effectiveExternalId("auto-uuid", "")).toBe("auto-uuid");
    expect(effectiveExternalId("auto-uuid", undefined)).toBe("auto-uuid");
  });
});

describe("ExternalId override UX", () => {
  it("substitutes a well-formed override into the rendered trust policy", () => {
    render(<ConnectorWizard {...makeProps()} />);

    // Get to the trust-policy step.
    fireEvent.change(screen.getByPlaceholderText("123456789012"), {
      target: { value: "111122223333" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    // Expand the Advanced disclosure to reveal the override input.
    fireEvent.click(
      screen.getByRole("button", { name: /Advanced options/i }),
    );

    const override = "abcdef12-3456-7890-abcd-ef1234567890";
    fireEvent.change(
      screen.getByLabelText(/ExternalId override/i),
      { target: { value: override } },
    );

    // The trust-policy <code> block must now reflect the override.
    const codeBlock = document.querySelector("pre code");
    expect(codeBlock?.textContent).toContain(override);
    // Account-root principal must still default to the account ID
    // from step 1.
    expect(codeBlock?.textContent).toContain(
      '"AWS": "arn:aws:iam::111122223333:root"',
    );
  });

  it("does NOT substitute a malformed override into the rendered trust policy", () => {
    render(<ConnectorWizard {...makeProps()} />);
    fireEvent.change(screen.getByPlaceholderText("123456789012"), {
      target: { value: "111122223333" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^Next$/i }));

    fireEvent.click(
      screen.getByRole("button", { name: /Advanced options/i }),
    );

    fireEvent.change(
      screen.getByLabelText(/ExternalId override/i),
      { target: { value: "not-a-uuid" } },
    );

    // The malformed string must NOT end up in the trust policy. The
    // ExternalId field below the policy continues to show the
    // auto-generated UUID.
    const codeBlock = document.querySelector("pre code");
    expect(codeBlock?.textContent).not.toContain("not-a-uuid");

    // Inline error guidance surfaces.
    expect(
      screen.getByText(/lowercase UUID v4 shape/i),
    ).toBeInTheDocument();
  });
});

// Sanity: AWS_PERMISSIONS_POLICY_TEMPLATE carries the actions the
// scanners depend on. The shell renders this verbatim, so a typo
// here would silently break the operator's IAM paste.
describe("AWS_PERMISSIONS_POLICY_TEMPLATE", () => {
  it("includes the EC2, Lambda, and RDS actions slice 1+2 needs", () => {
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).toContain("ec2:DescribeInstances");
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).toContain("lambda:ListFunctions");
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).toContain(
      "rds:DescribeDBInstances",
    );
  });

  it("includes no write/modify/delete actions (least privilege)", () => {
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).not.toMatch(/:Create\w+/);
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).not.toMatch(/:Delete\w+/);
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).not.toMatch(/:Modify\w+/);
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).not.toMatch(/:Put\w+/);
    expect(AWS_PERMISSIONS_POLICY_TEMPLATE).not.toMatch(/iam:/);
  });
});
