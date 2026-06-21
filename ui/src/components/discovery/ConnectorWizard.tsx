// ConnectorWizard — the v0.85 Stream 2D React shell that renders any
// provider's declarative ConnectorWizard. Slice 1 wires the AWS
// wizard (see ui/src/data/awsWizard.ts); future slices add GCP /
// Azure / on-prem by shipping new ConnectorWizard values and reusing
// this component verbatim.
//
// State machine:
//   - `stepIndex` (0..N-1) — the current step in the wizard's `steps`
//     array.
//   - `draft` — accumulating field state: account_id, role_arn, etc.
//   - `externalId` — generated once via crypto.randomUUID() the first
//     time the trust-policy step renders, then persisted in component
//     state for the rest of the flow.
//   - `validationResult` — set by the test_connection step's call to
//     onValidate; nulled when the operator edits a field upstream.
//   - `saveStatus` — drives the Save step's button label (idle /
//     saving / done) and the success card the final screen shows.
//
// The "suggested_step jump" UX: when validation returns a
// HumanizedError carrying suggested_step, the panel surfaces a
// "Return to <step title>" button that jumps the wizard back to that
// step. The shell maps step IDs to indices once on render — no
// imperative navigation logic in the call site.

import { CheckCircle2, ChevronLeft, Copy, ExternalLink, HelpCircle, Loader2, XCircle } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";

import { Button } from "../ui/button";
import { Input } from "../ui/input";

import {
  type ConnectorWizard as ConnectorWizardDef,
  type HumanizedError,
  type SaveConnectionRequest,
  type ValidateRequest,
  type ValidationResult,
  type WizardDraft,
  type WizardStep,
} from "@/api/discovery";
import {
  AWS_IAM_ROLE_CREATE_URL,
  AWS_PERMISSIONS_POLICY_TEMPLATE,
  AWS_TRUST_POLICY_ROOT_NOTE,
  AWS_TRUST_POLICY_TEMPLATE,
} from "@/data/awsWizard";
import { cn } from "@/lib/utils";


export interface ConnectorWizardProps {
  wizard: ConnectorWizardDef;
  // onValidate handles the test-before-commit step. The shell passes
  // the current draft (with the freshly-generated external_id) and
  // expects a ValidationResult — the shape rendered as the
  // "what just happened" panel below the Validate button.
  onValidate: (req: ValidateRequest) => Promise<ValidationResult>;
  // onSave handles the persist step. The shell calls this only after
  // a successful onValidate. Returns {connection_id} on success;
  // throws on failure.
  onSave: (req: SaveConnectionRequest) => Promise<{ connection_id: string }>;
  // onComplete fires after a successful onSave. The caller navigates
  // to /discovery/aws or renders a success card; the shell stays
  // mounted until then so the operator can re-read the result.
  onComplete: (connectionId: string) => void;
  // resumeMode signals the operator entered the wizard via the
  // "Resume an existing connection" entry point on the connections
  // list. The shell renders a small "Existing ExternalId (optional)"
  // field above the account-id input on step 1; a well-formed paste
  // flows into draft.external_id_override and is threaded through
  // the rest of the wizard via the existing #578 plumbing. Default
  // false — the wizard generates a fresh UUID like before. See #622.
  resumeMode?: boolean;
}

// fieldFromPayload extracts the typed `field` key from a fill_field
// payload. Returns "" when the payload shape doesn't match — the
// shell uses "" as a no-op draft key.
function fieldFromPayload(payload: unknown): string {
  if (
    payload &&
    typeof payload === "object" &&
    "field" in payload &&
    typeof (payload as { field: unknown }).field === "string"
  ) {
    return (payload as { field: string }).field;
  }
  return "";
}

function placeholderFromPayload(payload: unknown): string {
  if (
    payload &&
    typeof payload === "object" &&
    "placeholder" in payload &&
    typeof (payload as { placeholder: unknown }).placeholder === "string"
  ) {
    return (payload as { placeholder: string }).placeholder;
  }
  return "";
}

// PRINCIPAL_OVERRIDE_RE validates the optional principal_override
// input on the trust-policy step. Accepts user / role / root ARNs;
// rejects anything else so a typo can't slip into the rendered trust
// policy. The wizard falls back to the account-root default whenever
// the override is empty or fails this check.
export const PRINCIPAL_OVERRIDE_RE =
  /^arn:aws:iam::\d{12}:(user|role|root)(\/[\w+=,.@/-]*)?$/;

// EXTERNAL_ID_OVERRIDE_RE validates the optional
// external_id_override input on the trust-policy step. Lowercase
// canonical UUID shape; rejects anything else so a malformed paste
// doesn't end up in the policy or the validate payload. The wizard
// falls back to the auto-generated UUID whenever the override is
// empty or fails this check.
export const EXTERNAL_ID_OVERRIDE_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// renderTrustPolicy substitutes the principal and ExternalId into the
// trust-policy template. Centralized so the test can call it
// independently of the React tree.
//
// principalOverride: optional explicit principal ARN. When non-empty
// AND matches PRINCIPAL_OVERRIDE_RE, it replaces the default
// arn:aws:iam::<accountId>:root principal. Empty or malformed input
// falls back to the default so a bad paste never breaks the wizard.
export function renderTrustPolicy(
  externalId: string,
  accountId: string,
  principalOverride?: string,
): string {
  const defaultPrincipal = `arn:aws:iam::${accountId || "<account-id>"}:root`;
  const principal =
    principalOverride && PRINCIPAL_OVERRIDE_RE.test(principalOverride)
      ? principalOverride
      : defaultPrincipal;
  return AWS_TRUST_POLICY_TEMPLATE
    .replace("<PRINCIPAL-PLACEHOLDER>", principal)
    .replace("<UUID-PLACEHOLDER>", externalId);
}

// effectiveExternalId returns the override when present and well-formed,
// otherwise the auto-generated ExternalId. Used by both the trust-policy
// render and the validate/save payload so the operator sees one value
// across the wizard.
export function effectiveExternalId(
  generated: string,
  override?: string,
): string {
  if (override && EXTERNAL_ID_OVERRIDE_RE.test(override)) return override;
  return generated;
}

// validateInline runs a step's ValidationRule against an input value
// and returns true when the rule passes. Exported for unit tests; the
// component uses it on every keystroke to decide whether to enable the
// Next button.
export function validateInline(step: WizardStep, value: string): boolean {
  switch (step.validation.kind) {
    case "none":
      return true;
    case "not_empty":
      return value.trim() !== "";
    case "regex": {
      if (!step.validation.pattern) return true;
      try {
        return new RegExp(step.validation.pattern).test(value);
      } catch {
        // An invalid pattern is a programming error; fail closed so
        // the operator isn't blocked by a runtime regex throw.
        return false;
      }
    }
    default:
      return true;
  }
}

export function ConnectorWizard({
  wizard,
  onValidate,
  onSave,
  onComplete,
  resumeMode = false,
}: ConnectorWizardProps) {
  const [stepIndex, setStepIndex] = useState(0);
  const [draft, setDraft] = useState<WizardDraft>({ regions: ["us-east-1"] });
  const [externalId, setExternalId] = useState<string>("");
  const [validationResult, setValidationResult] = useState<ValidationResult | null>(
    null,
  );
  const [validating, setValidating] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [savedConnectionId, setSavedConnectionId] = useState<string | null>(null);
  const [whyOpen, setWhyOpen] = useState<Record<string, boolean>>({});

  // Generate the ExternalId once when the wizard mounts. The
  // trust-policy step renders it; subsequent steps reference the same
  // value. Done in useEffect rather than useState's initializer to
  // tolerate environments where crypto.randomUUID() is unavailable
  // (older test browsers); we fall back to a timestamp-based stub.
  useEffect(() => {
    if (externalId !== "") return;
    let id: string;
    try {
      id = crypto.randomUUID();
    } catch {
      id = `xid-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
    }
    setExternalId(id);
  }, [externalId]);

  const stepCount = wizard.steps.length;
  const step = wizard.steps[stepIndex];

  // effectiveExternalId honours the operator's override when set and
  // well-formed; otherwise falls back to the auto-generated UUID. We
  // compute this once per render so the trust-policy display, the
  // validate payload, and the save payload all use the same value.
  const liveExternalId = effectiveExternalId(externalId, draft.external_id_override);

  // showPrincipalOverride and showExternalIdResume toggle the two
  // always-visible affordance rows under the trust-policy JSON.
  // Each row's header (label + caption) is rendered up-front so the
  // operator sees the affordance without scrolling or hunting through
  // an "Advanced" disclosure (#621 was filed when both were buried).
  // The inputs stay collapsed by default; clicking the row header
  // expands the input next to that row only.
  //
  // showExternalIdResume defaults open in resumeMode so the operator
  // who landed in the wizard via the connections-list "Resume an
  // existing connection" entry point sees the paste field unfolded.
  const [showPrincipalOverride, setShowPrincipalOverride] = useState(false);
  const [showExternalIdResume, setShowExternalIdResume] = useState(resumeMode);

  // Map step IDs to indices once per wizard so the
  // HumanizedError.suggested_step jump is O(1) and the call site can
  // ignore the array order.
  const stepIndexById = useMemo(() => {
    const m: Record<string, number> = {};
    wizard.steps.forEach((s, i) => {
      m[s.id] = i;
    });
    return m;
  }, [wizard]);

  // Per-step input value. fill_field steps read/write via the draft's
  // field key; non-input steps return "".
  const fieldKey = step.action.kind === "fill_field" ? fieldFromPayload(step.action.payload) : "";
  const currentValue = fieldKey
    ? ((draft as Record<string, unknown>)[fieldKey] as string | undefined) ?? ""
    : "";

  const inlineValid = validateInline(step, currentValue);

  // Next-button enablement matrix:
  //   - fill_field: requires inline validation to pass.
  //   - copy_value / deep_link: always enabled (the operator's action
  //     happens out-of-band; the wizard trusts they did it).
  //   - test_connection (validate step): requires
  //     validationResult.assume_role_ok.
  //   - test_connection (save step): requires successful save —
  //     handled separately because save advances via onComplete.
  let nextEnabled = false;
  if (step.action.kind === "fill_field") {
    nextEnabled = inlineValid;
  } else if (step.action.kind === "copy_value" || step.action.kind === "deep_link") {
    nextEnabled = true;
  } else if (step.action.kind === "test_connection") {
    nextEnabled = !!validationResult?.assume_role_ok;
  }

  const isLastStep = stepIndex === stepCount - 1;

  const handleFieldChange = useCallback(
    (key: string, value: string) => {
      setDraft((d) => ({ ...d, [key]: value }));
      // Any draft edit invalidates the prior validation — the
      // operator must re-run the probe before saving.
      setValidationResult(null);
    },
    [],
  );

  const handleNext = useCallback(() => {
    if (!nextEnabled) return;
    setStepIndex((i) => Math.min(stepCount - 1, i + 1));
  }, [nextEnabled, stepCount]);

  const handleBack = useCallback(() => {
    setStepIndex((i) => Math.max(0, i - 1));
  }, []);

  const handleCopy = useCallback((value: string) => {
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(value);
    }
  }, []);

  const handleValidate = useCallback(async () => {
    setValidating(true);
    try {
      const res = await onValidate({
        role_arn: draft.role_arn ?? "",
        external_id: liveExternalId,
        regions: draft.regions ?? ["us-east-1"],
        account_id: draft.account_id,
      });
      setValidationResult(res);
    } finally {
      setValidating(false);
    }
  }, [onValidate, draft, liveExternalId]);

  const handleSave = useCallback(async () => {
    setSaving(true);
    setSaveError(null);
    try {
      const res = await onSave({
        account_id: draft.account_id ?? "",
        role_arn: draft.role_arn ?? "",
        external_id: liveExternalId,
        display_name: draft.display_name ?? draft.account_id ?? "",
        regions: draft.regions ?? ["us-east-1"],
      });
      setSavedConnectionId(res.connection_id);
      onComplete(res.connection_id);
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : "Save failed.");
    } finally {
      setSaving(false);
    }
  }, [onSave, draft, liveExternalId, onComplete]);

  const jumpToStep = useCallback(
    (id: string) => {
      const idx = stepIndexById[id];
      if (typeof idx === "number") {
        setStepIndex(idx);
        setValidationResult(null);
      }
    },
    [stepIndexById],
  );

  const toggleWhy = useCallback((id: string) => {
    setWhyOpen((o) => ({ ...o, [id]: !o[id] }));
  }, []);

  // The final success card renders once savedConnectionId is set.
  // Future slices may navigate away via onComplete; we keep the card
  // so even an OSS deployment without routing has a confirmation.
  if (savedConnectionId) {
    return (
      <div className="rounded-lg border bg-card p-6">
        <div className="flex items-center gap-3">
          <CheckCircle2 className="h-6 w-6 text-green-600" aria-hidden />
          <h2 className="text-lg font-semibold">Connection saved</h2>
        </div>
        <p className="mt-2 text-sm text-muted-foreground">
          Squadron will scan account{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">{savedConnectionId}</code>{" "}
          on the next scheduled run. You can trigger an ad-hoc scan from the
          inventory tab.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Header — progress bar + title */}
      <div>
        <h2 className="text-lg font-semibold">{wizard.title}</h2>
        <div
          className="mt-3 flex items-center gap-2"
          role="progressbar"
          aria-valuenow={stepIndex + 1}
          aria-valuemin={1}
          aria-valuemax={stepCount}
        >
          {wizard.steps.map((s, i) => (
            <div
              key={s.id}
              className={cn(
                "h-2 flex-1 rounded-full",
                i <= stepIndex ? "bg-primary" : "bg-muted",
              )}
            />
          ))}
        </div>
        <p className="mt-2 text-xs text-muted-foreground">
          Step {stepIndex + 1} of {stepCount}
        </p>
      </div>

      {/* Body — current step */}
      <div className="rounded-lg border bg-card p-6">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h3 className="text-base font-semibold">{step.title}</h3>
            <p className="mt-1 text-sm text-muted-foreground">{step.description}</p>
          </div>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label="Why this step?"
            onClick={() => toggleWhy(step.id)}
          >
            <HelpCircle className="h-4 w-4" aria-hidden />
          </Button>
        </div>

        {whyOpen[step.id] && (
          <div className="mt-3 rounded-md bg-muted/50 p-3 text-sm">
            <p>{step.recovery_hint}</p>
            <a
              href={step.doc_link}
              target="_blank"
              rel="noopener noreferrer"
              className="mt-2 inline-flex items-center gap-1 text-xs text-primary underline-offset-2 hover:underline"
            >
              Read more
              <ExternalLink className="h-3 w-3" aria-hidden />
            </a>
          </div>
        )}

        {/* Action renderer */}
        <div className="mt-4">
          {step.action.kind === "fill_field" && fieldKey && (
            <div className="space-y-3">
              {/* resumeMode pre-step — when the operator entered the
                  wizard via the connections-list "Resume an existing
                  connection" entry point, the first step gets an
                  optional ExternalId paste field rendered ABOVE the
                  account-id input. A well-formed paste populates
                  draft.external_id_override; the existing #578
                  plumbing threads it through to the trust-policy
                  render and the validate/save payload. See #622. */}
              {resumeMode && stepIndex === 0 && (
                <div className="space-y-1 rounded-md border bg-muted/30 p-3">
                  <label className="text-xs font-semibold" htmlFor="resume-external-id">
                    Existing ExternalId (optional)
                  </label>
                  <p className="text-xs text-muted-foreground">
                    Paste the UUID you previously configured in AWS.
                    Leave empty to generate a fresh one.
                  </p>
                  <Input
                    id="resume-external-id"
                    aria-label="Existing ExternalId"
                    aria-invalid={
                      !!draft.external_id_override &&
                      !EXTERNAL_ID_OVERRIDE_RE.test(draft.external_id_override)
                    }
                    placeholder="00000000-0000-0000-0000-000000000000"
                    value={draft.external_id_override ?? ""}
                    onChange={(e) =>
                      handleFieldChange("external_id_override", e.target.value)
                    }
                  />
                  {draft.external_id_override &&
                    !EXTERNAL_ID_OVERRIDE_RE.test(draft.external_id_override) && (
                      <p className="text-xs text-destructive">
                        Must be a lowercase UUID v4 shape. Reverting to
                        the auto-generated ExternalId until valid.
                      </p>
                    )}
                </div>
              )}
              <Input
                aria-label={step.title}
                aria-invalid={!inlineValid && currentValue !== ""}
                placeholder={placeholderFromPayload(step.action.payload)}
                value={currentValue}
                onChange={(e) => handleFieldChange(fieldKey, e.target.value)}
              />
              {!inlineValid && currentValue !== "" && step.validation.message && (
                <p className="text-xs text-destructive">{step.validation.message}</p>
              )}
            </div>
          )}

          {step.action.kind === "copy_value" && step.id === "trust-policy" && (
            <div className="space-y-2">
              <pre className="overflow-x-auto rounded-md bg-muted p-3 text-xs">
                <code>
                  {renderTrustPolicy(
                    liveExternalId,
                    draft.account_id ?? "",
                    draft.principal_override,
                  )}
                </code>
              </pre>
              <div className="flex gap-2">
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() =>
                    handleCopy(
                      renderTrustPolicy(
                        liveExternalId,
                        draft.account_id ?? "",
                        draft.principal_override,
                      ),
                    )
                  }
                >
                  <Copy className="mr-1 h-3.5 w-3.5" aria-hidden />
                  Copy trust policy
                </Button>
                <Button
                  type="button"
                  size="sm"
                  variant="secondary"
                  onClick={() => window.open(AWS_IAM_ROLE_CREATE_URL, "_blank", "noopener,noreferrer")}
                >
                  <ExternalLink className="mr-1 h-3.5 w-3.5" aria-hidden />
                  Open AWS IAM role creation
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                ExternalId:{" "}
                <code className="rounded bg-muted px-1 py-0.5 text-[10px]">
                  {liveExternalId || "generating..."}
                </code>
              </p>

              {/* Inline :root explanation — verbatim text owned by
                  awsWizard.ts. Operators read ":root" as the AWS root
                  user; the note disambiguates without making them
                  read the description paragraph above the JSON. #621. */}
              <p className="text-xs text-muted-foreground" data-testid="root-principal-note">
                {AWS_TRUST_POLICY_ROOT_NOTE}
              </p>

              {/* Two always-visible affordance rows. #621 found both
                  hidden behind a single "Advanced options" disclosure
                  during a real-time walkthrough; replaced here per
                  #622's locked design. Each row's header (label +
                  caption) is up-front; the input expands inline when
                  the operator clicks the row. */}
              <div className="space-y-2 rounded-md border bg-muted/30 p-3">
                {/* Row 1 — scope to a specific IAM identity */}
                <div className="space-y-1">
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="-mx-2 h-auto w-[calc(100%+1rem)] justify-start px-2 py-1 text-left"
                    onClick={() => setShowPrincipalOverride((v) => !v)}
                    aria-expanded={showPrincipalOverride}
                    aria-controls="principal-override-input"
                  >
                    <div className="flex w-full flex-col gap-0.5">
                      <div className="flex items-center gap-2">
                        <span className="text-xs font-semibold">
                          Scope to a specific IAM identity
                        </span>
                        <span className="rounded bg-muted px-1 py-0.5 text-[10px] font-medium text-muted-foreground">
                          recommended for prod
                        </span>
                      </div>
                      <span className="text-xs font-normal text-muted-foreground">
                        Tighter trust-policy scoping than the default.
                      </span>
                    </div>
                  </Button>
                  {showPrincipalOverride && (
                    <div id="principal-override-input" className="space-y-1 pt-1">
                      <Input
                        id="principal-override"
                        aria-label="Principal override ARN"
                        aria-invalid={
                          !!draft.principal_override &&
                          !PRINCIPAL_OVERRIDE_RE.test(draft.principal_override)
                        }
                        placeholder="arn:aws:iam::123456789012:user/squadron-bot"
                        value={draft.principal_override ?? ""}
                        onChange={(e) =>
                          handleFieldChange("principal_override", e.target.value)
                        }
                      />
                      {draft.principal_override &&
                        !PRINCIPAL_OVERRIDE_RE.test(draft.principal_override) && (
                          <p className="text-xs text-destructive">
                            Override must look like arn:aws:iam::123456789012:user/Name
                            (or role/Name, or root). Reverting to account root until valid.
                          </p>
                        )}
                    </div>
                  )}
                </div>

                {/* Row 2 — resume with existing ExternalId */}
                <div className="space-y-1 border-t pt-2">
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="-mx-2 h-auto w-[calc(100%+1rem)] justify-start px-2 py-1 text-left"
                    onClick={() => setShowExternalIdResume((v) => !v)}
                    aria-expanded={showExternalIdResume}
                    aria-controls="external-id-override-input"
                  >
                    <div className="flex w-full flex-col gap-0.5">
                      <span className="text-xs font-semibold">
                        Resume with existing ExternalId
                      </span>
                      <span className="text-xs font-normal text-muted-foreground">
                        If you&apos;ve connected this account in a previous Squadron deployment.
                      </span>
                    </div>
                  </Button>
                  {showExternalIdResume && (
                    <div id="external-id-override-input" className="space-y-1 pt-1">
                      <Input
                        id="external-id-override"
                        aria-label="ExternalId override"
                        aria-invalid={
                          !!draft.external_id_override &&
                          !EXTERNAL_ID_OVERRIDE_RE.test(draft.external_id_override)
                        }
                        placeholder="00000000-0000-0000-0000-000000000000"
                        value={draft.external_id_override ?? ""}
                        onChange={(e) =>
                          handleFieldChange("external_id_override", e.target.value)
                        }
                      />
                      {draft.external_id_override &&
                        !EXTERNAL_ID_OVERRIDE_RE.test(draft.external_id_override) && (
                          <p className="text-xs text-destructive">
                            Must be a lowercase UUID v4 shape. Reverting to
                            the auto-generated ExternalId until valid.
                          </p>
                        )}
                    </div>
                  )}
                </div>
              </div>
            </div>
          )}

          {step.action.kind === "copy_value" && step.id === "permissions-policy" && (
            <div className="space-y-2">
              <pre className="overflow-x-auto rounded-md bg-muted p-3 text-xs">
                <code>{AWS_PERMISSIONS_POLICY_TEMPLATE}</code>
              </pre>
              <div className="flex gap-2">
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => handleCopy(AWS_PERMISSIONS_POLICY_TEMPLATE)}
                >
                  <Copy className="mr-1 h-3.5 w-3.5" aria-hidden />
                  Copy permissions policy
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                Read-only across EC2, Lambda, and RDS. No write/modify
                actions are granted.
              </p>
            </div>
          )}

          {step.action.kind === "deep_link" && (
            <Button
              type="button"
              variant="secondary"
              onClick={() => {
                const payload = step.action.payload as { url?: string } | undefined;
                if (payload?.url) {
                  window.open(payload.url, "_blank", "noopener,noreferrer");
                }
              }}
            >
              <ExternalLink className="mr-1 h-4 w-4" aria-hidden />
              Open provider console
            </Button>
          )}

          {step.action.kind === "test_connection" && step.id === "validate" && (
            <div className="space-y-3">
              <Button
                type="button"
                onClick={handleValidate}
                disabled={validating || !draft.role_arn || !liveExternalId}
              >
                {validating && <Loader2 className="mr-1 h-4 w-4 animate-spin" aria-hidden />}
                Validate connection
              </Button>
              {validationResult && (
                <WhatJustHappenedPanel
                  result={validationResult}
                  steps={wizard.steps}
                  onJumpToStep={jumpToStep}
                />
              )}
            </div>
          )}

          {step.action.kind === "test_connection" && step.id === "save" && (
            <div className="space-y-3">
              <Button type="button" onClick={handleSave} disabled={saving || !validationResult?.assume_role_ok}>
                {saving && <Loader2 className="mr-1 h-4 w-4 animate-spin" aria-hidden />}
                Save and finish
              </Button>
              {!validationResult?.assume_role_ok && (
                <p className="text-xs text-muted-foreground">
                  Return to the Validate step and run a successful probe before saving.
                </p>
              )}
              {saveError && (
                <div className="rounded-md border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive">
                  {saveError}
                </div>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Footer — Back / Next */}
      <div className="flex items-center justify-between">
        <Button
          type="button"
          variant="ghost"
          onClick={handleBack}
          disabled={stepIndex === 0}
        >
          <ChevronLeft className="mr-1 h-4 w-4" aria-hidden />
          Back
        </Button>
        {!isLastStep && (
          <Button type="button" onClick={handleNext} disabled={!nextEnabled}>
            Next
          </Button>
        )}
      </div>
    </div>
  );
}

// WhatJustHappenedPanel renders the typed ValidationResult as a
// per-service status list. Matches the v0.84 playground result-panel
// pattern: green checks for OK, red X for failure, a per-error jump
// button that uses suggested_step to return the operator to the
// failing step.
function WhatJustHappenedPanel({
  result,
  steps,
  onJumpToStep,
}: {
  result: ValidationResult;
  steps: WizardStep[];
  onJumpToStep: (id: string) => void;
}) {
  return (
    <div className="rounded-md border bg-muted/30 p-3">
      <h4 className="text-sm font-semibold">What just happened</h4>
      <div className="mt-2 space-y-2">
        <StatusRow ok={result.assume_role_ok} label="sts:AssumeRole" />
        {result.assume_role_err && (
          <ErrorRow
            err={result.assume_role_err}
            steps={steps}
            onJumpToStep={onJumpToStep}
          />
        )}
        {result.preflight.map((p) => (
          <div key={p.service}>
            <StatusRow
              ok={p.ok}
              label={`${p.service} probe`}
              suffix={p.ok ? `${p.sample_count} sample(s)` : undefined}
            />
            {p.err && (
              <ErrorRow err={p.err} steps={steps} onJumpToStep={onJumpToStep} />
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

function StatusRow({
  ok,
  label,
  suffix,
}: {
  ok: boolean;
  label: string;
  suffix?: string;
}) {
  return (
    <div className="flex items-center gap-2 text-sm">
      {ok ? (
        <CheckCircle2 className="h-4 w-4 text-green-600" aria-hidden />
      ) : (
        <XCircle className="h-4 w-4 text-destructive" aria-hidden />
      )}
      <span className="font-medium">{label}</span>
      {suffix && <span className="text-xs text-muted-foreground">{suffix}</span>}
    </div>
  );
}

function ErrorRow({
  err,
  steps,
  onJumpToStep,
}: {
  err: HumanizedError;
  steps: WizardStep[];
  onJumpToStep: (id: string) => void;
}) {
  const target = steps.find((s) => s.id === err.suggested_step);
  return (
    <div className="ml-6 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-xs">
      <p className="text-destructive">{err.message}</p>
      {target && (
        <Button
          type="button"
          variant="link"
          size="sm"
          className="h-auto p-0 text-xs"
          onClick={() => onJumpToStep(target.id)}
        >
          Return to: {target.title}
        </Button>
      )}
    </div>
  );
}
