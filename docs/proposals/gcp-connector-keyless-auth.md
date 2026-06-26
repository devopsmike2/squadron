# GCP connector: keyless auth (the SA-key default-deny wall)

## Problem

Squadron's GCP discovery connector authenticates **only** via a
downloadable Service Account JSON key: the wizard's step 3 asks the
operator to run `gcloud iam service-accounts keys create key.json` and
paste the result (`sealed_sa`). The whole GCP path (validate, scan,
recommendations) is built around that key.

Google now **disables Service Account key creation by default** on newly
created projects via the managed org-policy constraint
`constraints/iam.disableServiceAccountKeyCreation` (part of Google's
"secure by default" rollout). The result:

```
ERROR: (gcloud.iam.service-accounts.keys.create) FAILED_PRECONDITION:
Key creation is not allowed on this service account.
  type: constraints/iam.disableServiceAccountKeyCreation
```

This was reproduced end-to-end on **two** fresh accounts during live
testing (2026-06-26):

1. An organization-managed account (`peptidepal.app`) — override denied:
   the constraint is org-level and the project Owner is not an Org Policy
   Admin (`setOrgPolicy` permission denied).
2. A brand-new **consumer (gmail.com) account with no organization** —
   the constraint is *still* enforced, and even the project **Owner**
   cannot override it (`setOrgPolicy` permission denied on the project).

So a new GCP user cannot complete the wizard: the connector requires a
credential Google refuses to mint, and the operator has no permission to
relax the policy. The wizard UI is otherwise fully functional (steps 1–3
complete cleanly); the blocker is entirely the missing credential.

## Impact

A large and growing share of GCP tenants — every new project under the
secure-by-default regime, plus every enterprise org that enforces the
same constraint deliberately — cannot connect a GCP project to Squadron.
SA keys are the *least* recommended GCP auth method precisely because
they are long-lived exfiltratable secrets; Squadron currently mandates
the one method the platform is actively deprecating.

## Options

1. **Workload Identity Federation (WIF) — recommended.** Squadron's
   backend presents an external workload credential (e.g. an OIDC token
   minted for the Squadron deployment) to GCP STS and exchanges it for a
   short-lived access token scoped to the operator's Service Account via
   a workload-identity-pool binding. No downloadable key ever exists.
   The operator's setup becomes "create the SA, grant `compute.viewer`,
   add a WIF pool binding" — all allowed under the default policy.
2. **Service Account impersonation.** The operator grants Squadron's own
   identity `roles/iam.serviceAccountTokenCreator` on the target SA;
   Squadron mints short-lived tokens via `generateAccessToken`. Clean,
   but assumes Squadron runs with a resolvable GCP identity.
3. **Application Default Credentials (ADC).** Works only when Squadron
   itself runs inside GCP with an attached SA — not viable for a
   self-hostable-anywhere OSS tool, but worth supporting as an option.
4. **Document the manual override** for the minority of operators who
   *are* org-policy admins (disable-enforce the constraint at the org or
   project). A stopgap, not a fix — and it tells users to weaken a
   security control the connector shouldn't need.

## Recommendation

Add a keyless auth mode to the GCP connector — WIF as the primary path
(option 1), with SA-key retained as a fallback for the shrinking set of
tenants that still permit it. The wizard gains an auth-method choice at
step 2; the scanner/credstore substrate gains a non-key credential
shape. Until then, the GCP wizard is only usable by operators who can
still create SA keys, which the README/runbook should state plainly.

## Status

**Implemented v0.89.223** (option 1+ via the polymorphic loader): the GCP
scanner now builds its token source with `google.CredentialsFromJSON`
instead of `JWTConfigFromJSON`, so it accepts `service_account`,
`external_account` (Workload Identity Federation), `impersonated_service_account`,
and `authorized_user` (gcloud ADC). The handler validator and the wizard's
client-side validator accept the same set. Operators on tenants that forbid
SA keys can now connect with WIF / impersonation / ADC — no downloadable key.
WIF setup runbook + a wizard auth-method picker remain as polish follow-ups.

Filed 2026-06-26 from live GCP-connection testing. Blocking real GCP
onboarding for default-configured projects. No code change in this
commit — design only, mirroring the adot-arn-freshness-design.md
posture.
