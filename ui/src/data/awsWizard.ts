// Slice-1 AWS connector wizard, hardcoded client-side.
//
// This is the TypeScript mirror of internal/discovery/wizard.AWSWizard().
// Both definitions exist because the React shell can render the wizard
// without a round-trip to the server, and the Go layer keeps a
// canonical definition so future provider integrations (GCP, Azure)
// and server-side rendering can grow from the same source.
//
// Known trade-off: keeping two definitions risks drift. The Go-side
// tests assert step count + required fields; the TypeScript-side
// tests in ConnectorWizard.test.tsx assert the same shape against
// this constant. A future slice should add a /api/v1/discovery/wizard
// endpoint and switch the shell to fetch the wizard, removing this
// file. Documented as a known trade-off in the Stream 2D agent
// report.

import type { ConnectorWizard } from "@/api/discovery";

// Trust-policy JSON template. The wizard substitutes
// <UUID-PLACEHOLDER> with crypto.randomUUID() the first time the
// trust-policy step renders, and keeps the SQUADRON_ACCOUNT_ID literal
// for OSS deployments (Compliance Pack will replace it with a real
// value later).
//
// The template lives in this file rather than the component so a
// future drift check can compare it against the Go-side template
// without parsing JSX.
export const AWS_TRUST_POLICY_TEMPLATE = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::SQUADRON_ACCOUNT_ID:role/SquadronDiscovery"
      },
      "Action": "sts:AssumeRole",
      "Condition": {
        "StringEquals": {
          "sts:ExternalId": "<UUID-PLACEHOLDER>"
        }
      }
    }
  ]
}`;

// AWS IAM role creation deep-link. Lands the operator on the role
// creation flow itself rather than the IAM home — principle 4 in the
// design doc's eleven-principles section.
export const AWS_IAM_ROLE_CREATE_URL =
  "https://console.aws.amazon.com/iam/home#/roles$new?step=type&roleType=crossAccount";

export const awsWizard: ConnectorWizard = {
  provider: "aws",
  title: "Connect an AWS account",
  steps: [
    {
      id: "account-id",
      title: "Enter your AWS account ID",
      description:
        "The 12-digit account ID for the AWS account you want Squadron to scan. You can find this in the AWS Console under Account or in any role ARN.",
      action: {
        kind: "fill_field",
        payload: {
          field: "account_id",
          placeholder: "123456789012",
        },
      },
      validation: {
        kind: "regex",
        pattern: "^\\d{12}$",
        message: "Account ID must be exactly 12 digits.",
      },
      doc_link: "https://docs.squadron.example/discovery/aws#account-id",
      recovery_hint:
        "Verify the account ID in your AWS Console. Common typo: confusing the account ID (12 digits) with the access key ID (20 chars).",
    },
    {
      id: "trust-policy",
      title: "Create the IAM role with this trust policy",
      description:
        "Squadron generated a per-deployment ExternalId for you. Copy the trust policy below verbatim and paste it into the AWS IAM role creation flow. The ExternalId defeats the confused-deputy problem — never share it.",
      action: {
        kind: "copy_value",
        payload: {
          field: "trust_policy",
          language: "json",
        },
      },
      validation: { kind: "none" },
      doc_link: "https://docs.squadron.example/discovery/aws#trust-policy",
      recovery_hint:
        "Re-copy the trust policy from this step — don't edit the JSON. The ExternalId condition is required; removing it makes the role unsafe.",
    },
    {
      id: "role-arn",
      title: "Paste the role ARN AWS created",
      description:
        "After IAM creates the role, AWS shows you the ARN. Paste it here. Squadron uses this to call sts:AssumeRole against your account.",
      action: {
        kind: "fill_field",
        payload: {
          field: "role_arn",
          placeholder: "arn:aws:iam::123456789012:role/SquadronDiscovery",
        },
      },
      validation: {
        kind: "regex",
        pattern: "^arn:aws:iam::\\d{12}:role\\/[\\w+=,.@-]+$",
        message:
          "Role ARN must look like arn:aws:iam::123456789012:role/RoleName.",
      },
      doc_link: "https://docs.squadron.example/discovery/aws#role-arn",
      recovery_hint:
        "Confirm the ARN you pasted matches the role AWS created. The account ID embedded in the ARN must match the value from Step 1.",
    },
    {
      id: "validate",
      title: "Validate the connection",
      description:
        "Squadron will run sts:AssumeRole and a tiny EC2 + Lambda probe to confirm the role works. No records are created until you click Save on the next step.",
      action: { kind: "test_connection" },
      validation: { kind: "none" },
      doc_link: "https://docs.squadron.example/discovery/aws#validate",
      recovery_hint:
        "If validation fails, the error message names the step to revisit. Re-validate after each fix; no records are created until Save.",
    },
    {
      id: "save",
      title: "Save the connection",
      description:
        "Squadron will encrypt and store the role ARN and ExternalId, run one final validation to catch last-second edits, and emit a discovery.aws.connection_created audit event. The ExternalId never appears in the audit payload.",
      action: { kind: "test_connection" },
      validation: { kind: "none" },
      doc_link: "https://docs.squadron.example/discovery/aws#save",
      recovery_hint:
        "If Save fails after Validate passed, the role was likely edited between steps. Return to the Validate step and re-run.",
    },
  ],
};
