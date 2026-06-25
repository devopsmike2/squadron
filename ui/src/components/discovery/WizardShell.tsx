// WizardShell — the shared connection-wizard chrome (v0.89.190).
//
// Every provider's connect flow (AWS, GCP, Azure, OCI, IaC) shares the
// same outer presentation: a step-progress header, a bordered card
// wrapping the current step's body, and a Back / Next footer. Before
// this component each Discovery page carried its own copy of that
// chrome — a duplicated `WizardHeader` plus an inline footer per page
// (DiscoveryGCP / DiscoveryAzure / DiscoveryOCI were byte-for-byte
// identical). This component owns the frame once; providers render
// their provider-specific step *bodies* as children and drive
// navigation via props.
//
// The shell is intentionally presentation-only: it holds no wizard
// state. Each provider keeps its own state machine (step index, field
// state, validate / scan results) because those genuinely differ per
// cloud — GCP parses a pasted service-account JSON, AWS generates an
// ExternalId, OCI takes an API-key fingerprint, etc. The shell unifies
// the frame, not the contents, so no flow is downgraded to fit a
// lowest-common-denominator data model.

import { ChevronLeft } from "lucide-react";
import type { ReactNode } from "react";

import { Button } from "../ui/button";

export interface WizardShellProps {
  // 0-based index of the current step.
  stepIndex: number;
  // Total number of steps in this provider's flow.
  stepCount: number;
  // Display title of the current step. Rendered in both the progress
  // header (right span) and the card heading, matching the prior
  // per-page chrome.
  stepTitle: string;
  // Whether Next is enabled. The provider computes this from its
  // per-step validation matrix.
  canAdvance: boolean;
  // When true, Back is disabled and Next is disabled while an async
  // step action (validate / scan / save) is in flight.
  submitting?: boolean;
  onBack: () => void;
  onNext: () => void;
  // The current step's provider-specific body.
  children: ReactNode;
}

// WizardShell renders the shared progress header, the bordered step
// card, and the Back / Next footer. The final step intentionally has
// no Next button — its body owns the terminal primary action (Scan /
// Save) — so the footer suppresses Next when on the last step.
export function WizardShell({
  stepIndex,
  stepCount,
  stepTitle,
  canAdvance,
  submitting = false,
  onBack,
  onNext,
  children,
}: WizardShellProps) {
  const isLastStep = stepIndex === stepCount - 1;
  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <div className="flex items-center justify-between">
          <span className="text-xs uppercase tracking-wider text-muted-foreground">
            Step {stepIndex + 1} of {stepCount}
          </span>
          <span className="text-xs text-muted-foreground">{stepTitle}</span>
        </div>
        <div className="h-1 w-full rounded bg-muted">
          <div
            className="h-1 rounded bg-primary transition-all"
            style={{ width: `${((stepIndex + 1) / stepCount) * 100}%` }}
          />
        </div>
      </div>

      <div className="rounded-lg border bg-card p-6">
        <h3 className="text-base font-semibold">{stepTitle}</h3>
        <div className="mt-4">{children}</div>
      </div>

      <div className="flex items-center justify-between">
        <Button
          type="button"
          variant="ghost"
          onClick={onBack}
          disabled={stepIndex === 0 || submitting}
        >
          <ChevronLeft className="mr-1 h-4 w-4" aria-hidden />
          Back
        </Button>
        {!isLastStep && (
          <Button
            type="button"
            onClick={onNext}
            disabled={!canAdvance || submitting}
          >
            Next
          </Button>
        )}
      </div>
    </div>
  );
}
