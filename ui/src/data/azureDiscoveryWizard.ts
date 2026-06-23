// Static data for the v0.89.53 #677 Stream 75 (slice-1 chunk 4) Azure
// discovery wizard.
//
// Mirrors the gcpDiscoveryWizard.ts pattern: declarative constants
// live here so the imperative renderer in pages/DiscoveryAzure.tsx
// stays focused on the state machine + step bodies. Regexes are the
// authoritative client-side validators; the Go server re-validates
// before persisting so a stale client cannot bypass the regex (per
// design doc §7 + the chunk-3 handler's azureUUIDPattern check the
// canonical validators live on the server).

// --- Wizard step identity --------------------------------------------

// Stable string keys for each wizard step. Mirrors the GCP_STEP_*
// pattern in gcpDiscoveryWizard.ts — the wizard renderer indexes
// AZURE_STEP_IDS[stepIndex] to pick the body component.
export const AZURE_STEP_SUBSCRIPTION = "subscription";
export const AZURE_STEP_SERVICE_PRINCIPAL = "service-principal";
export const AZURE_STEP_CREDENTIALS = "credentials";
export const AZURE_STEP_VALIDATE = "validate";
export const AZURE_STEP_SCAN = "scan";

export const AZURE_STEP_IDS = [
  AZURE_STEP_SUBSCRIPTION,
  AZURE_STEP_SERVICE_PRINCIPAL,
  AZURE_STEP_CREDENTIALS,
  AZURE_STEP_VALIDATE,
  AZURE_STEP_SCAN,
] as const;

export const AZURE_STEP_TITLES: Record<string, string> = {
  [AZURE_STEP_SUBSCRIPTION]: "Connect an Azure subscription",
  [AZURE_STEP_SERVICE_PRINCIPAL]: "Create the Service Principal",
  [AZURE_STEP_CREDENTIALS]: "Paste credentials",
  [AZURE_STEP_VALIDATE]: "Validate",
  [AZURE_STEP_SCAN]: "Scan",
};

// --- Validation regexes ----------------------------------------------

// AZURE_UUID_REGEX matches the canonical Azure UUID shape (8-4-4-4-12
// hex with hyphens, case-insensitive). Per design doc §8 step 1 the
// wizard validates tenant_id and subscription_id as UUIDs; step 3
// validates client_id (the SP appId) under the same shape. The Go
// handler in internal/api/handlers/discovery_azure.go::azureUUIDPattern
// uses the IDENTICAL pattern; client + server must agree on the parse
// — if you change one, change both.
export const AZURE_UUID_REGEX =
  /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

// AZURE_LOCATION_REGEX is a permissive shape check for the optional
// Location field. Azure region names are lowercase compounds like
// "eastus", "westeurope", "centralindia", "francecentral", etc. —
// they're not strictly alphanumeric in the public docs but every
// production Azure region uses [a-z0-9]+ with no hyphens or dots.
// Empty is a valid value and means "scan every location the SP can
// see"; slice 1 ships single-location filtering.
export const AZURE_LOCATION_REGEX = /^[a-z][a-z0-9]+$/;

// --- Documentation links --------------------------------------------

// AZURE_DOC_LINK is the relative docs path the wizard's "Why am I
// doing this?" expandable links to. Co-located here rather than
// hardcoded inline so a future docs reshuffle is one-edit. The
// runbook itself ships in chunk 6 of this arc; until then the link
// 404s gracefully against the static docs server.
export const AZURE_DOC_LINK = "/docs/discovery-azure-first-time-setup.md";

// AZURE_RBAC_DOC_LINK points at Microsoft's own documentation for the
// Reader role so an operator with deeper RBAC questions can jump
// straight to the source. External link — the wizard's "Learn more
// about the Reader role" affordance opens it in a new tab.
export const AZURE_RBAC_DOC_LINK =
  "https://learn.microsoft.com/en-us/azure/role-based-access-control/built-in-roles#reader";

// --- az CLI command template ----------------------------------------

// The wizard renders this as a copy-able instruction block on step
// 2. The `<subscription_id>` placeholder is substituted with the
// subscription_id from step 1 in the rendered text so the operator
// can copy-paste directly without hand-editing.
//
// Service-principal display name ("Squadron Discovery") matches the
// design doc §8 + the runbook (when it lands in chunk 6). Operators
// with org-policy naming requirements override by hand — the
// placeholder in the rendered text makes the substitution location
// obvious.
export const AZURE_SP_NAME = "Squadron Discovery";

export const AZURE_SP_CREATE_CMD_TEMPLATE = `az ad sp create-for-rbac \\
  --name "${AZURE_SP_NAME}" \\
  --role "Reader" \\
  --scopes "/subscriptions/<subscription_id>"`;

// substituteSubscription is the inline templating helper the wizard
// uses to fill <subscription_id> placeholders before display.
// Centralized so the command block renders through a single code
// path; tests assert against this function rather than the rendered
// HTML to keep them resistant to layout churn. Mirrors
// substituteProject in gcpDiscoveryWizard.ts.
export function substituteSubscription(
  template: string,
  subscriptionID: string,
): string {
  return template.replace(
    /<subscription_id>/g,
    subscriptionID || "<subscription_id>",
  );
}

// --- Validate-step remediation copy ----------------------------------

// validateErrorRemediation maps an AzureValidateErrorKind to the prose
// the wizard's Validate step renders under the red banner on failure.
// The copy is intentionally specific to the failure mode — the §7.1
// design-doc rationale is "operator gets an immediate, actionable
// error instead of a silent half-empty scan."
//
// The connectionTenantID + connectionSubscriptionID parameters are
// substituted into the per-kind branches; kinds that don't depend on
// them ignore the args. Returning a plain string keeps the wizard
// renderer's job to "show this text" — no React node interpolation
// needed.
export function validateErrorRemediation(
  kind: string | undefined,
  args: {
    connectionTenantID: string;
    connectionSubscriptionID: string;
  },
): string {
  switch (kind) {
    case "permission_denied":
      return `Verify the Service Principal has the Reader role on subscription ${args.connectionSubscriptionID}. Re-run the az ad sp create-for-rbac command from Step 2 if needed — RBAC changes can take up to 60 seconds to propagate.`;
    case "subscription_not_found":
      return `Verify ${args.connectionSubscriptionID} is correct and the Service Principal has access. The subscription may not exist, or the SP's scope may not include it.`;
    case "tenant_invalid":
      return `Verify ${args.connectionTenantID} matches the Azure AD tenant where the Service Principal was created. The tenant ID is visible in the Azure portal under Azure Active Directory → Overview.`;
    case "credentials_invalid":
      return "Re-check the Client ID and Client Secret. The secret may have expired (Azure SP secrets default to 1 year). Regenerate the secret via `az ad sp credential reset` and re-paste it into the wizard.";
    case "network":
      return "Squadron's outbound connectivity to management.azure.com may be blocked. Check egress firewalls and proxy configuration.";
    case "subscription_mismatch":
      return `The Service Principal's subscription scope doesn't match the configured subscription_id (${args.connectionSubscriptionID}). Re-run az ad sp create-for-rbac with the correct --scopes value.`;
    case "unknown":
    default:
      return "Validation failed for an unexpected reason. See the message above; if it doesn't help, file an issue with the error text.";
  }
}
