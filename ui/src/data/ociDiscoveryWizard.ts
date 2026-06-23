// Static data for the v0.89.58 #684 Stream 82 (slice-1 chunk 4) OCI
// discovery wizard.
//
// Mirrors the azureDiscoveryWizard.ts pattern: declarative constants
// live here so the imperative renderer in pages/DiscoveryOCI.tsx
// stays focused on the state machine + step bodies. Regexes are the
// authoritative client-side validators; the Go server re-validates
// before persisting so a stale client cannot bypass the regex (per
// design doc §7 + the chunk-3 handler's ociTenancyOCIDPattern /
// ociUserOCIDPattern / ociFingerprintPattern — the canonical
// validators live on the server).

// --- Wizard step identity --------------------------------------------

// Stable string keys for each wizard step. Mirrors the AZURE_STEP_*
// pattern in azureDiscoveryWizard.ts — the wizard renderer indexes
// OCI_STEP_IDS[stepIndex] to pick the body component.
export const OCI_STEP_TENANCY = "tenancy";
export const OCI_STEP_GENERATE_KEY = "generate-key";
export const OCI_STEP_UPLOAD_KEY = "upload-key";
export const OCI_STEP_CREDENTIALS = "credentials";
export const OCI_STEP_VALIDATE_SCAN = "validate-scan";

export const OCI_STEP_IDS = [
  OCI_STEP_TENANCY,
  OCI_STEP_GENERATE_KEY,
  OCI_STEP_UPLOAD_KEY,
  OCI_STEP_CREDENTIALS,
  OCI_STEP_VALIDATE_SCAN,
] as const;

export const OCI_STEP_TITLES: Record<string, string> = {
  [OCI_STEP_TENANCY]: "Connect an OCI tenancy",
  [OCI_STEP_GENERATE_KEY]: "Generate the API signing key",
  [OCI_STEP_UPLOAD_KEY]: "Upload public key to OCI Console",
  [OCI_STEP_CREDENTIALS]: "Paste credentials into Squadron",
  [OCI_STEP_VALIDATE_SCAN]: "Validate + Scan",
};

// --- Validation regexes ----------------------------------------------

// OCI_TENANCY_OCID_REGEX matches the canonical OCI tenancy OCID
// shape (ocid1.tenancy.oc1..<unique_id>). Per design doc §8 step 1
// the wizard validates tenancy_ocid against this shape. The Go
// handler in internal/api/handlers/discovery_oci.go::ociTenancyOCIDPattern
// uses the IDENTICAL pattern; client + server must agree on the
// parse — if you change one, change both.
export const OCI_TENANCY_OCID_REGEX = /^ocid1\.tenancy\.oc1\..+/;

// OCI_USER_OCID_REGEX matches the canonical OCI user OCID shape
// (ocid1.user.oc1..<unique_id>). Same rationale as
// OCI_TENANCY_OCID_REGEX; mirrors the handler's ociUserOCIDPattern.
export const OCI_USER_OCID_REGEX = /^ocid1\.user\.oc1\..+/;

// OCI_FINGERPRINT_REGEX matches the OCI API Signing Key fingerprint
// shape — 16 colon-separated hex pairs (e.g.
// "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"). The OCI
// Console returns this exact 16-pair MD5 fingerprint when a public
// key is uploaded. The wizard enforces the 16-pair count
// client-side; the handler's ociFingerprintPattern uses a more
// permissive "any number of colon pairs" pattern for forward
// compatibility with future fingerprint algorithms — the client is
// stricter than the server here intentionally so wizard users see
// an immediate validation failure on a typo rather than round-trip
// to the server.
export const OCI_FINGERPRINT_REGEX = /^([a-f0-9]{2}:){15}[a-f0-9]{2}$/;

// --- OCI regions -----------------------------------------------------

// OCI_REGIONS is the dropdown's option set. OCI operates dozens of
// regions; the slice 1 wizard exposes the most commonly-deployed
// commercial regions to keep the dropdown UX scannable. The wizard
// also accepts any string the operator types (the input falls back
// to free-text when the dropdown doesn't carry the operator's
// region) — this matches OCI Console's posture of "common regions
// surfaced, full catalog browsable."
//
// Per design doc §5: OCI requires Region always — unlike AWS / GCP /
// Azure which allow empty Region for "scan all". OCI's API endpoints
// are regional, so the scanner must know which region to query.
// Slice 1 ships single-region per connection.
export const OCI_REGIONS: Array<{ id: string; label: string }> = [
  { id: "us-phoenix-1", label: "US West (Phoenix)" },
  { id: "us-ashburn-1", label: "US East (Ashburn)" },
  { id: "us-sanjose-1", label: "US West (San Jose)" },
  { id: "us-chicago-1", label: "US Central (Chicago)" },
  { id: "ca-toronto-1", label: "Canada Southeast (Toronto)" },
  { id: "ca-montreal-1", label: "Canada Southeast (Montreal)" },
  { id: "sa-saopaulo-1", label: "Brazil East (Sao Paulo)" },
  { id: "sa-santiago-1", label: "Chile (Santiago)" },
  { id: "uk-london-1", label: "UK South (London)" },
  { id: "eu-frankfurt-1", label: "Germany Central (Frankfurt)" },
  { id: "eu-amsterdam-1", label: "Netherlands Northwest (Amsterdam)" },
  { id: "eu-zurich-1", label: "Switzerland North (Zurich)" },
  { id: "eu-paris-1", label: "France Central (Paris)" },
  { id: "eu-madrid-1", label: "Spain Central (Madrid)" },
  { id: "eu-milan-1", label: "Italy Northwest (Milan)" },
  { id: "eu-stockholm-1", label: "Sweden Central (Stockholm)" },
  { id: "me-jeddah-1", label: "Saudi Arabia West (Jeddah)" },
  { id: "me-dubai-1", label: "UAE East (Dubai)" },
  { id: "il-jerusalem-1", label: "Israel Central (Jerusalem)" },
  { id: "ap-mumbai-1", label: "India West (Mumbai)" },
  { id: "ap-hyderabad-1", label: "India South (Hyderabad)" },
  { id: "ap-singapore-1", label: "Singapore (Singapore)" },
  { id: "ap-tokyo-1", label: "Japan East (Tokyo)" },
  { id: "ap-osaka-1", label: "Japan Central (Osaka)" },
  { id: "ap-seoul-1", label: "South Korea Central (Seoul)" },
  { id: "ap-chuncheon-1", label: "South Korea North (Chuncheon)" },
  { id: "ap-sydney-1", label: "Australia East (Sydney)" },
  { id: "ap-melbourne-1", label: "Australia Southeast (Melbourne)" },
];

// --- Documentation links --------------------------------------------

// OCI_DOC_LINK is the relative docs path the wizard's "Why am I
// doing this?" expandable links to. Co-located here rather than
// hardcoded inline so a future docs reshuffle is one-edit. The
// runbook itself ships in chunk 6 of this arc; until then the link
// 404s gracefully against the static docs server.
export const OCI_DOC_LINK = "/docs/discovery-oci-first-time-setup.md";

// OCI_IAM_DOC_LINK points at Oracle's own documentation for the
// IAM policy concept so an operator with deeper RBAC questions can
// jump straight to the source. External link — the wizard's "Learn
// more about OCI IAM policies" affordance opens it in a new tab.
export const OCI_IAM_DOC_LINK =
  "https://docs.oracle.com/en-us/iaas/Content/Identity/Concepts/policygetstarted.htm";

// OCI_API_KEYS_DOC_LINK points at Oracle's API Signing Keys
// documentation so an operator can read the canonical flow for
// generating a keypair + uploading the public half.
export const OCI_API_KEYS_DOC_LINK =
  "https://docs.oracle.com/en-us/iaas/Content/API/Concepts/apisigningkey.htm";

// --- CLI command snippets --------------------------------------------

// The wizard renders these as copy-able instruction blocks on step
// 2. Two options per design doc §8 step 2: OCI CLI (the easier path)
// or openssl (the path that doesn't require the OCI CLI to be
// installed).

// OCI_SETUP_KEYS_CMD is the OCI CLI one-liner that generates a key
// pair + writes the fingerprint to ~/.oci. Recommended path when
// the operator has `oci` installed.
export const OCI_SETUP_KEYS_CMD = `oci setup keys --output-dir ~/.oci`;

// OCI_OPENSSL_GENERATE_CMD generates a 2048-bit RSA key pair via
// openssl and computes the fingerprint via openssl + md5. The final
// command outputs the colon-pair fingerprint the operator will
// paste into Step 4.
export const OCI_OPENSSL_GENERATE_CMD = `openssl genrsa -out ~/.oci/oci_api_key.pem 2048
openssl rsa -pubout -in ~/.oci/oci_api_key.pem -out ~/.oci/oci_api_key_public.pem
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem | openssl md5 -c`;

// --- Validate-step remediation copy ----------------------------------

// validateErrorRemediation maps an OCIValidateErrorKind to the prose
// the wizard's Validate step renders under the red banner on
// failure. The copy is intentionally specific to the failure mode —
// the §7.1 design-doc rationale is "operator gets an immediate,
// actionable error instead of a silent half-empty scan."
//
// The connectionRegion + connectionTenancyOCID parameters are
// substituted into the per-kind branches; kinds that don't depend on
// them ignore the args. Returning a plain string keeps the wizard
// renderer's job to "show this text" — no React node interpolation
// needed.
export function validateErrorRemediation(
  kind: string | undefined,
  args: {
    connectionRegion: string;
    connectionTenancyOCID: string;
  },
): string {
  switch (kind) {
    case "permission_denied":
      return `Verify the user has compute.instances:read permission on the tenancy. Add a policy: \`Allow group Administrators to read instances in tenancy\`.`;
    case "tenancy_not_found":
      return `Verify the tenancy OCID matches an existing tenancy in region ${args.connectionRegion || "<region>"}.`;
    case "fingerprint_mismatch":
      return "The fingerprint doesn't match the public key uploaded to OCI Console for this user. Re-verify the fingerprint from `openssl rsa -pubout -outform DER -in oci_api_key.pem | openssl md5 -c`.";
    case "private_key_invalid":
      return "The pasted PEM is malformed or not an RSA key. Re-paste including the BEGIN PRIVATE KEY / END PRIVATE KEY markers.";
    case "network":
      return "Squadron's outbound connectivity to *.oraclecloud.com may be blocked. Check egress firewalls and proxy configuration.";
    case "unknown":
    default:
      return "Validation failed for an unexpected reason. See the message above; if it doesn't help, file an issue with the error text.";
  }
}
