// Static data for the v0.89.48 #670 Stream 68 (slice-1 chunk 4) GCP
// discovery wizard.
//
// Mirrors the iacGithubWizard.ts pattern: declarative constants live
// here so the imperative renderer in pages/DiscoveryGCP.tsx stays
// focused on the state machine + step bodies. Regexes are the
// authoritative client-side validators; the Go server re-validates
// before persisting so a stale client cannot bypass the regex (per
// design doc §6 the canonical validators live on the server).

// --- Wizard step identity --------------------------------------------

// Stable string keys for each wizard step. Mirrors the STEP_PROVIDER
// /STEP_PAT pattern in iacGithubWizard.tsx — the wizard renderer
// indexes STEP_IDS[stepIndex] to pick the body component.
export const GCP_STEP_PROJECT = "project";
export const GCP_STEP_SERVICE_ACCOUNT = "service-account";
export const GCP_STEP_KEY_PASTE = "key-paste";
export const GCP_STEP_VALIDATE = "validate";
export const GCP_STEP_SCAN = "scan";

export const GCP_STEP_IDS = [
  GCP_STEP_PROJECT,
  GCP_STEP_SERVICE_ACCOUNT,
  GCP_STEP_KEY_PASTE,
  GCP_STEP_VALIDATE,
  GCP_STEP_SCAN,
] as const;

export const GCP_STEP_TITLES: Record<string, string> = {
  [GCP_STEP_PROJECT]: "Connect a GCP project",
  [GCP_STEP_SERVICE_ACCOUNT]: "Create a Service Account",
  [GCP_STEP_KEY_PASTE]: "Download key and paste into Squadron",
  [GCP_STEP_VALIDATE]: "Validate",
  [GCP_STEP_SCAN]: "Scan",
};

// --- Validation regexes ----------------------------------------------

// GCP_PROJECT_ID_REGEX mirrors the GCP project naming rule documented
// in https://cloud.google.com/resource-manager/reference/rest/v1/projects:
// 6 to 30 lowercase letters, digits, or hyphens; must start with a
// letter; must not end with a hyphen. The design doc §7 wizard step 1
// names this validator explicitly so the operator gets a fast inline
// signal rather than a server round-trip failure.
//
// The regex is intentionally identical to the validator in
// internal/api/handlers/discovery_gcp.go::projectIDRE so client +
// server agree on the parse. If you change one, change both.
export const GCP_PROJECT_ID_REGEX = /^[a-z][-a-z0-9]{4,28}[a-z0-9]$/;

// GCP_REGION_REGEX is a permissive shape check for the optional Region
// field. GCP region names follow `<continent>-<location><digit>` (e.g.
// "us-central1", "europe-west4", "asia-northeast3"); empty is a valid
// value and means "scan every region the SA can see" — slice-1 doesn't
// orchestrate per-region scans yet, so the empty path is the common
// case.
export const GCP_REGION_REGEX = /^[a-z]+-[a-z0-9]+$/;

// --- Documentation links --------------------------------------------

// GCP_DOC_LINK is the relative docs path the wizard's "Why am I doing
// this?" expandable links to. Co-located here rather than hardcoded
// inline so a future docs reshuffle is one-edit. The runbook itself
// ships in chunk 6 of this arc; until then the link 404s gracefully
// against the static docs server.
export const GCP_DOC_LINK = "/docs/discovery-gcp-first-time-setup.md";

// GCP_IAM_DOC_LINK points at Google's own documentation for the
// compute.viewer role so an operator with deeper IAM questions can
// jump straight to the source. External link — the wizard's "Learn
// more about compute.viewer" affordance opens it in a new tab.
export const GCP_IAM_DOC_LINK =
  "https://cloud.google.com/iam/docs/understanding-roles#compute.viewer";

// --- gcloud command templates ---------------------------------------

// The wizard renders these as copy-able instruction blocks on steps 2
// and 3. The `<project>` placeholder is substituted with the
// project_id from step 1 in the rendered text so the operator can
// copy-paste directly without hand-editing.
//
// Service-account naming ("squadron-discovery") matches the design
// doc §7 + the runbook (when it lands in chunk 6). Operators with
// org-policy naming requirements override by hand — the placeholder
// in the rendered text makes the substitution location obvious.

export const GCP_SA_NAME = "squadron-discovery";

export const GCP_SA_CREATE_CMD_TEMPLATE = `gcloud iam service-accounts create ${GCP_SA_NAME} \\
  --display-name "Squadron Discovery" \\
  --project=<project>`;

export const GCP_SA_ROLE_BIND_CMD_TEMPLATE = `gcloud projects add-iam-policy-binding <project> \\
  --member="serviceAccount:${GCP_SA_NAME}@<project>.iam.gserviceaccount.com" \\
  --role="roles/compute.viewer"`;

export const GCP_SA_KEY_CREATE_CMD_TEMPLATE = `gcloud iam service-accounts keys create key.json \\
  --iam-account=${GCP_SA_NAME}@<project>.iam.gserviceaccount.com`;

// substituteProject is the inline templating helper the wizard uses to
// fill <project> placeholders before display. Centralized so the three
// command blocks render through a single code path; tests assert
// against this function rather than the rendered HTML to keep them
// resistant to layout churn.
export function substituteProject(template: string, projectID: string): string {
  return template.replace(/<project>/g, projectID || "<project>");
}

// --- SA-JSON validation ---------------------------------------------

// SARequiredFields is the minimum field set a valid SA JSON carries.
// The wizard parses the pasted text, asserts every field below is
// present and non-empty, and only then enables the acknowledgment
// checkbox + Next button on step 3.
//
// Why only three fields? The Go credstore.SealGCPServiceAccount
// helper accepts any JSON the SA-builder might emit; client-side we
// gate on the fields the wizard's downstream steps actually depend
// on:
//   - client_email : surfaced on the Validate step's "what we sent"
//                    summary for operator confidence.
//   - private_key  : the field operators most commonly forget when
//                    pasting (key.json's `private_key` is multi-line
//                    and easy to truncate on partial selection).
//   - project_id   : the field the §11.3 project_mismatch detector
//                    cross-references against the connection's
//                    configured project_id. Required so the mismatch
//                    can be detected client-side too in a future
//                    enhancement.
// GCP_SUPPORTED_CRED_TYPES are the credential JSON "type" values the
// connector accepts. "service_account" is the downloadable SA key;
// "external_account" (Workload Identity Federation), "impersonated_service_account",
// and "authorized_user" (gcloud ADC) are the keyless shapes — needed
// because Google disables SA-key creation by default on new projects
// (constraints/iam.disableServiceAccountKeyCreation).
export const GCP_SUPPORTED_CRED_TYPES = [
  "service_account",
  "external_account",
  "impersonated_service_account",
  "authorized_user",
] as const;

// ParsedServiceAccount is the typed projection of a valid SA JSON
// blob — the wizard renderer reads this rather than re-parsing.
export interface ParsedServiceAccount {
  // type is the credential kind; the keyless shapes leave
  // client_email/private_key empty and often carry no project_id.
  type: string;
  client_email: string;
  private_key: string;
  project_id: string;
  // Other fields the SA carries (type, private_key_id, client_id,
  // etc.) are intentionally NOT projected — the wizard does not look
  // at them, and the server is the source of truth for any deeper
  // validation.
}

// SAValidationError is the discriminated outcome of parseServiceAccount.
// The wizard renders the message verbatim inline under the textarea.
export interface SAValidationError {
  kind: "json-parse" | "missing-field" | "empty";
  message: string;
}

// parseServiceAccount walks the pasted text:
//   1. Trim + empty check.
//   2. JSON.parse — fail with json-parse kind if it throws.
//   3. Walk SA_REQUIRED_FIELDS — fail with missing-field kind if any
//      field is missing or a non-string.
//
// Returns the typed projection on success. Pure function — no I/O,
// no DOM access — so the wizard test can call it directly without
// rendering.
export function parseServiceAccount(
  raw: string,
):
  | { ok: true; sa: ParsedServiceAccount }
  | { ok: false; err: SAValidationError } {
  const trimmed = raw.trim();
  if (trimmed === "") {
    return {
      ok: false,
      err: {
        kind: "empty",
        message: "Paste the service-account JSON above.",
      },
    };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return {
      ok: false,
      err: {
        kind: "json-parse",
        message:
          "That doesn't look like valid JSON. Paste the full contents of key.json including the surrounding {…} braces.",
      },
    };
  }
  if (typeof parsed !== "object" || parsed === null) {
    return {
      ok: false,
      err: {
        kind: "json-parse",
        message: "Service-account JSON must be an object.",
      },
    };
  }
  const obj = parsed as Record<string, unknown>;
  const credType =
    typeof obj["type"] === "string" ? (obj["type"] as string).trim() : "";
  if (credType === "") {
    return {
      ok: false,
      err: {
        kind: "missing-field",
        message:
          'The pasted JSON is missing the "type" field. Paste a full GCP credential JSON (a Service Account key, a Workload Identity Federation config, or gcloud ADC output).',
      },
    };
  }
  if (!(GCP_SUPPORTED_CRED_TYPES as readonly string[]).includes(credType)) {
    return {
      ok: false,
      err: {
        kind: "missing-field",
        message: `Credential type "${credType}" isn't supported. Use a Service Account key, a Workload Identity Federation config (external_account), impersonated_service_account, or gcloud ADC (authorized_user).`,
      },
    };
  }
  if (credType === "service_account") {
    for (const f of ["client_email", "private_key"] as const) {
      const v = obj[f];
      if (typeof v !== "string" || v.trim() === "") {
        return {
          ok: false,
          err: {
            kind: "missing-field",
            message: `The service-account JSON is missing the "${f}" field. Make sure you copied the full key.json contents.`,
          },
        };
      }
    }
  }
  const strField = (k: string): string =>
    typeof obj[k] === "string" ? (obj[k] as string) : "";
  return {
    ok: true,
    sa: {
      type: credType,
      client_email: strField("client_email"),
      private_key: strField("private_key"),
      project_id: strField("project_id"),
    },
  };
}

// --- Validate-step remediation copy ----------------------------------

// validateErrorRemediation maps a GCPValidateErrorKind to the prose
// the wizard's Validate step renders under the red banner on failure.
// The copy is intentionally specific to the failure mode — the §6.1
// design-doc rationale is "operator gets an immediate, actionable
// error instead of a silent half-empty scan."
//
// The connectionProjectID + saProjectID parameters are substituted
// into the project_mismatch branch; the other kinds ignore them.
// Returning a plain string keeps the wizard renderer's job to "show
// this text" — no React node interpolation needed.
export function validateErrorRemediation(
  kind: string | undefined,
  args: {
    connectionProjectID: string;
    saProjectID: string;
  },
): string {
  switch (kind) {
    case "permission_denied":
      return `Verify the service account has roles/compute.viewer in project ${args.connectionProjectID}. The role binding from step 2 may not have applied yet — IAM changes can take up to 60 seconds to propagate.`;
    case "project_not_found":
      return `Verify ${args.connectionProjectID} is correct. The project may not exist, or the service account may not have visibility into it.`;
    case "credentials_invalid":
      return "Re-check the SA JSON contents. The pasted key may be malformed, revoked, or for a different account than this connection expects.";
    case "network":
      return "Squadron's outbound connectivity to compute.googleapis.com may be blocked. Check egress firewalls and proxy configuration.";
    case "project_mismatch":
      return `The SA JSON's project is ${args.saProjectID || "(unknown)"} but you configured ${args.connectionProjectID}. Either change the connection's project ID, or use an SA created in ${args.connectionProjectID}.`;
    case "unknown":
    default:
      return "Validation failed for an unexpected reason. See the message above; if it doesn't help, file an issue with the error text.";
  }
}
