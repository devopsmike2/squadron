// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package wizard defines the declarative ConnectorWizard model the
// universal-discovery design doc calls for. Each provider (AWS today,
// GCP / Azure / on-prem in later slices) exports one ConnectorWizard
// value; the React shell consumes the value and renders the same
// stepper + copy helpers + deep-link helpers + validation helpers
// regardless of provider.
//
// The eleven principles in
// docs/universal-discovery-design.md "Connector workflow design"
// drive the field set: every step ships with a Title + Description +
// DocLink + RecoveryHint so a stuck operator always has a path
// forward, and the wizard itself is enough documentation to complete
// the connect flow under five minutes (a release-blocking
// invariant — connector setup is the first impression and first
// impressions don't get hotfixes).
package wizard

import (
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// ConnectorWizard is the declarative definition of a provider's
// connect-account flow. The React shell consumes this; the only
// per-provider Go code is the ValidateFn / PersistFn that lands at
// the API layer (handlers.HandleAWSValidate /
// handlers.HandleAWSSaveConnection for AWS).
//
// Adding a new provider is: export a new ConnectorWizard value,
// implement a Scanner.Validate + a Save handler, and wire one route.
// No new React components, no new wizard framework code.
type ConnectorWizard struct {
	// Provider names the cloud (or on-prem source) this wizard
	// targets. Used by the UI to route /discovery/<provider>/connect
	// to the right wizard value.
	Provider credstore.Provider `json:"provider"`

	// Title is the wizard's H1, shown above the stepper. Plain prose,
	// addressed to the operator (e.g. "Connect an AWS account").
	Title string `json:"title"`

	// Steps is the ordered list of wizard steps. Slice 1's AWS wizard
	// ships five steps: Account ID → Trust policy → Role ARN →
	// Validate → Save. Future slices may add or remove steps per
	// provider without changing the type.
	Steps []WizardStep `json:"steps"`
}

// WizardStep is one screen in the wizard. The React shell renders the
// step based on its Action.Kind — a FillField step gets an input, a
// CopyValue step gets a code block with a copy button, etc.
//
// Every step carries a DocLink + RecoveryHint so the inline "why this
// step?" panel and the post-failure jump-back UX both have content
// without per-step UI code. A step missing either field would fail the
// foolproof-or-release-blocked invariant; the tests enforce
// non-emptiness.
type WizardStep struct {
	// ID is the step's stable identifier. Used by the
	// HumanizedError.SuggestedStep pointer to deep-link back to the
	// failing step, by the audit trail, and by URL fragments
	// (/discovery/aws/connect#account-id) so an operator can bookmark
	// mid-flow. Must be unique within a ConnectorWizard.
	ID string `json:"id"`

	// Title is the step heading, e.g. "Enter your AWS account ID".
	// Imperative voice — the step is asking the operator to do
	// something.
	Title string `json:"title"`

	// Description is the prose under the title. Explains what the
	// operator is doing and why it matters in one or two sentences.
	Description string `json:"description"`

	// Action drives the step's interactive surface. See WizardAction
	// for the four kinds the slice-1 React shell supports.
	Action WizardAction `json:"action"`

	// Validation is the inline check the React shell runs on every
	// input change. Empty rule (ValidationNone) means the step is
	// always considered valid — used by CopyValue / DeepLink /
	// TestConnection steps where there is nothing for the operator to
	// type.
	Validation ValidationRule `json:"validation"`

	// DocLink is the destination of the inline "why this step?" panel.
	// Each step has one — the foolproof-or-release-blocked invariant
	// demands an answer to the security-reviewer question for every
	// step, not just the obviously-load-bearing ones.
	DocLink string `json:"doc_link"`

	// RecoveryHint is the prose shown when a later step fails with a
	// HumanizedError.SuggestedStep pointing here. The UI surfaces this
	// alongside the humanized error message so the operator knows
	// what to change before re-running validation.
	RecoveryHint string `json:"recovery_hint"`
}

// ActionKind enumerates the slice-1 step types the React shell can
// render. The list is closed: a new kind requires a new React renderer
// branch, which is intentional — the shell is the only place wizard
// behavior lives.
type ActionKind string

const (
	// ActionCopyValue renders a code block with the value to copy
	// (e.g. the trust-policy JSON) and a copy-to-clipboard button.
	// Payload shape: map[string]string with keys "value" and
	// optionally "language" (for syntax highlighting).
	ActionCopyValue ActionKind = "copy_value"

	// ActionFillField renders a text input with inline validation.
	// Payload shape: map[string]string with keys "field" (the draft
	// key the value populates) and optionally "placeholder".
	ActionFillField ActionKind = "fill_field"

	// ActionDeepLink renders a button that opens the provider console
	// in a new tab at the exact page the operator needs (e.g. the
	// IAM role-creation form, not the IAM home).
	// Payload shape: map[string]string with key "url".
	ActionDeepLink ActionKind = "deep_link"

	// ActionTestConnection renders a "Validate" button that POSTs the
	// draft to the provider's validation endpoint and shows the
	// "what just happened" panel below. The wizard cannot advance
	// past this step until the validation passes.
	// Payload shape: nil (the React shell knows which endpoint to
	// call from the wizard's Provider field).
	ActionTestConnection ActionKind = "test_connection"
)

// WizardAction packages an ActionKind with its kind-specific payload.
// Payload is `any` because each kind carries different fields; the
// React shell branches on Kind and reads the payload's known shape.
//
// The payload is `any` rather than a typed union because slice 1's
// shell is the only consumer and a typed union here would require a
// codegen step to keep the TS side in sync. Future slices may
// formalize this if more providers add kinds.
type WizardAction struct {
	Kind    ActionKind `json:"kind"`
	Payload any        `json:"payload,omitempty"`
}

// ValidationKind enumerates the slice-1 inline-validation rules. The
// shell runs the rule on every keystroke and disables the Next button
// when the rule fails. Server-side validation still runs at the
// validate-endpoint step; the inline rule is purely a UX accelerator.
type ValidationKind string

const (
	// ValidationNone means no inline check. The step is always
	// considered valid for purposes of enabling the Next button.
	// Used by non-input steps (CopyValue, DeepLink, TestConnection).
	ValidationNone ValidationKind = "none"

	// ValidationNotEmpty passes when the trimmed input is non-empty.
	ValidationNotEmpty ValidationKind = "not_empty"

	// ValidationRegex passes when the input matches Pattern. The
	// shell compiles the pattern once per step; an invalid pattern
	// is a programming error and the tests catch it at build time.
	ValidationRegex ValidationKind = "regex"
)

// ValidationRule is the per-step inline-validation contract. Empty
// Pattern is required for ValidationNone and ValidationNotEmpty;
// non-empty Pattern is required for ValidationRegex.
//
// Message is the inline error the shell renders under the input when
// the rule fails. Imperative voice ("Account ID must be exactly 12
// digits.") rather than declarative ("invalid input") so the operator
// always knows what to do.
type ValidationRule struct {
	Kind    ValidationKind `json:"kind"`
	Pattern string         `json:"pattern,omitempty"`
	Message string         `json:"message,omitempty"`
}

// AWSWizard returns the slice-1 AWS ConnectorWizard. The six steps
// mirror the design doc's "Connector workflow design > Architecture"
// section: Account ID → Trust policy → Permissions policy →
// Role ARN → Validate → Save. (Permissions policy was added in
// v0.87.1 by #575; see the trust-policy step's comment for context.)
//
// The function returns a value (not a package-level var) so callers
// get an independent copy — a future test or admin tool that mutates
// the wizard for a what-if check doesn't leak state across calls.
func AWSWizard() ConnectorWizard {
	return ConnectorWizard{
		Provider: credstore.ProviderAWS,
		Title:    "Connect an AWS account",
		Steps: []WizardStep{
			{
				ID:          "account-id",
				Title:       "Enter your AWS account ID",
				Description: "The 12-digit account ID for the AWS account you want Squadron to scan. You can find this in the AWS Console under Account or in any role ARN.",
				Action: WizardAction{
					Kind: ActionFillField,
					Payload: map[string]string{
						"field":       "account_id",
						"placeholder": "123456789012",
					},
				},
				Validation: ValidationRule{
					Kind:    ValidationRegex,
					Pattern: `^\d{12}$`,
					Message: "Account ID must be exactly 12 digits.",
				},
				DocLink:      "https://docs.squadron.example/discovery/aws#account-id",
				RecoveryHint: "Verify the account ID in your AWS Console. Common typo: confusing the account ID (12 digits) with the access key ID (20 chars).",
			},
			{
				ID:          "trust-policy",
				Title:       "Create the IAM role with this trust policy",
				Description: "Squadron generated a per-deployment ExternalId for you. By default this trust policy lets any IAM identity in your AWS account assume the SquadronDiscovery role, provided that identity has sts:AssumeRole permission and passes the ExternalId — the AWS-recommended bootstrap shape for self-hosted Squadron. Copy the trust policy below verbatim and paste it into the AWS IAM role creation flow. Use the Advanced section to scope to a single IAM identity, or to resume with an ExternalId you already pasted into AWS.",
				Action: WizardAction{
					Kind: ActionCopyValue,
					Payload: map[string]string{
						"field":    "trust_policy",
						"language": "json",
					},
				},
				Validation: ValidationRule{
					Kind: ValidationNone,
				},
				DocLink:      "https://docs.squadron.example/discovery/aws#trust-policy",
				RecoveryHint: "Re-copy the trust policy from this step — don't edit the JSON. The ExternalId condition is required; removing it makes the role unsafe. If you previously pasted a different ExternalId into AWS, use the Advanced > Resume with existing ExternalId field to match it.",
			},
			{
				// permissions-policy was added in v0.87.1 (#575). The
				// validate step's sts:AssumeRole succeeds even with no
				// permissions attached; the operator only finds out at
				// scan time that the role can't actually list anything.
				// Surfacing the policy as its own copy_value step makes
				// the failure mode impossible to miss. The template
				// JSON lives in AWS_PERMISSIONS_POLICY_TEMPLATE in
				// ui/src/data/awsWizard.ts — same drift-tradeoff as
				// the trust-policy template. Slice 3a (v0.88.0)
				// extended the template with 8 new actions (5 S3 + 3
				// ELBv2); the description is updated to mention the
				// five service categories Squadron now reads from.
				ID:          "permissions-policy",
				Title:       "Add this permissions policy to the role",
				Description: "Squadron needs read-only access to EC2, Lambda, RDS, S3, and ELBv2 (ALB / NLB) in your account to discover what's uninstrumented. Copy this policy verbatim and attach it to the SquadronDiscovery role you just created — either as an inline policy or a separate managed policy. Squadron never executes write/modify actions; only the actions in this list are granted.",
				Action: WizardAction{
					Kind: ActionCopyValue,
					Payload: map[string]string{
						"field":    "permissions_policy",
						"language": "json",
					},
				},
				Validation: ValidationRule{
					Kind: ValidationNone,
				},
				DocLink:      "https://docs.squadron.example/discovery/aws#permissions-policy",
				RecoveryHint: "If the validate step's sts:AssumeRole succeeds but the EC2/Lambda/RDS/S3/ALB probes return AccessDenied, the permissions policy is missing or scoped wrong. Re-copy the policy from this step.",
			},
			{
				ID:          "role-arn",
				Title:       "Paste the role ARN AWS created",
				Description: "After IAM creates the role, AWS shows you the ARN. Paste it here. Squadron uses this to call sts:AssumeRole against your account.",
				Action: WizardAction{
					Kind: ActionFillField,
					Payload: map[string]string{
						"field":       "role_arn",
						"placeholder": "arn:aws:iam::123456789012:role/SquadronDiscovery",
					},
				},
				Validation: ValidationRule{
					Kind:    ValidationRegex,
					Pattern: `^arn:aws:iam::\d{12}:role\/[\w+=,.@-]+$`,
					Message: "Role ARN must look like arn:aws:iam::123456789012:role/RoleName.",
				},
				DocLink:      "https://docs.squadron.example/discovery/aws#role-arn",
				RecoveryHint: "Confirm the ARN you pasted matches the role AWS created. The account ID embedded in the ARN must match the value from Step 1.",
			},
			{
				ID:          "validate",
				Title:       "Validate the connection",
				Description: "Squadron will run sts:AssumeRole and tiny EC2 + Lambda + RDS + S3 + ALB probes to confirm the role works. No records are created until you click Save on the next step.",
				Action: WizardAction{
					Kind: ActionTestConnection,
				},
				Validation: ValidationRule{
					Kind: ValidationNone,
				},
				DocLink:      "https://docs.squadron.example/discovery/aws#validate",
				RecoveryHint: "If validation fails, the error message names the step to revisit. Re-validate after each fix; no records are created until Save.",
			},
			{
				ID:          "save",
				Title:       "Save the connection",
				Description: "Squadron will encrypt and store the role ARN and ExternalId, run one final validation to catch last-second edits, and emit a discovery.aws.connection_created audit event. The ExternalId never appears in the audit payload.",
				Action: WizardAction{
					Kind: ActionTestConnection,
				},
				Validation: ValidationRule{
					Kind: ValidationNone,
				},
				DocLink:      "https://docs.squadron.example/discovery/aws#save",
				RecoveryHint: "If Save fails after Validate passed, the role was likely edited between steps. Return to the Validate step and re-run.",
			},
		},
	}
}
