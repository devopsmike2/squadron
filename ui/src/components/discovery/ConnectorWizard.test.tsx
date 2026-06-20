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

import { ConnectorWizard } from "./ConnectorWizard";

import {
  type SaveConnectionRequest,
  type ValidateRequest,
  type ValidationResult,
} from "@/api/discovery";
import { awsWizard } from "@/data/awsWizard";


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
    // Stepper shows "Step 1 of 5".
    expect(screen.getByText(/Step 1 of 5/)).toBeInTheDocument();
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
    fireEvent.click(screen.getByRole("button", { name: /Next/i }));

    // Step 2: trust policy (Next is always enabled on copy_value).
    fireEvent.click(screen.getByRole("button", { name: /Next/i }));

    // Step 3: role ARN.
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
    fireEvent.click(screen.getByRole("button", { name: /Next/i }));

    // Step 4: validate. Click the Validate button and wait for the
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
    // and click Next four times. (Trust-policy step has no input.)
    fireEvent.change(screen.getByPlaceholderText("123456789012"), {
      target: { value: "123456789012" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Next/i }));
    fireEvent.click(screen.getByRole("button", { name: /Next/i }));
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
    fireEvent.click(screen.getByRole("button", { name: /Next/i }));

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
});
