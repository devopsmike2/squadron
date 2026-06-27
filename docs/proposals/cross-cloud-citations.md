# Cross-cloud verdict citations

Status: shipped + corrected v0.89.249 (store slice 1 = v0.89.247; first cut v0.89.248 was inert — see Correction). Author: autonomous session.

## Problem

The discovery proposer's verdict-learning loop (a decline note recorded on
a recommendation surfaces as a citation on a later, similar recommendation)
is **scoped per cloud account**. `DiscoveryBridge.AssembleDiscoveryVerdicts`
selects verdicts with the policy "same connection_id + account_id + region
only" (531-proposer-learning-slice2 §6). A decline recorded on an AWS
account is filed under that AWS scope; a GCP scan's verdict block never sees
it.

The published LinkedIn post ("The citation crosses clouds", Sat) depicts a
decline on AWS surfacing on a GCP recommendation. That behavior is **not yet
reproducible** — the post ran ahead of the build. This arc closes that gap so
the claim is dogfoodable from the repo.

## Approach

Verdicts already carry the natural cross-cloud key: `recommendation_kind`
(the pattern, e.g. a collector metrics-volume drop). Rather than build
explicit pattern-matching, widen the verdict pool: include a small, capped
set of recent verdicts from OTHER scopes, each labeled with its origin
cloud, and let the proposer correlate by kind in its reasoning (which is
exactly what the post shows the model doing).

- The substrate makes cross-cloud verdicts *available + attributed*.
- The proposer does the pattern correlation (it already reasons over the
  verdict block).

Same-scope verdicts keep their existing budget and dominate; cross-scope
adds at most a couple of citations so the block stays tight.

## Changes

1. **types.DiscoveryVerdict** gains `Provider` + `ScopeID` (origin). Empty
   for same-scope rows; populated for cross-scope.
2. **Store** (memory + sqlite): `ListCrossScopeDiscoveryVerdicts(ctx,
   excludeScopeID, since, limit)` — recent pr_merged / pr_closed_not_
   merged audit rows whose scope (account/project/subscription/tenancy) !=
   excludeScopeID, projecting
   origin Provider (inferred from which scope key is set: account_id→aws,
   project_id→gcp, subscription_id→azure, tenancy_ocid→oci) + ScopeID +
   kind.
3. **Bridge**: after the same-scope Select, select a capped cross-scope set
   (DefaultMaxTotal/2), prefix each Body with `[seen on <provider>/<scope>]`,
   append to the rejected/approved slices + exampleURLs. Opt-out on the
   *current* connection still short-circuits everything first.
4. **Prompt**: no signature change — the origin label rides in the Body and
   renders on the existing `reason:` line. (A dedicated `seen on:` line is a
   possible slice-2 polish.)

## Correction (v0.89.249) — exclude by scope, not connection; opt-in flag

Preparing the live e2e surfaced that the first cut (v0.89.248) was **inert in
the common deployment**: it excluded cross-scope verdicts by *connection_id*.
But one IaC GitHub repo serves PRs for every cloud, so with a single connected
repo the cross-scope query always returned empty — the citation never crossed
clouds. The v0.89.248 unit test passed only because it used a separate
connection per cloud, an unrealistic shape that masked the bug.

Two corrections:

1. **Exclude by SCOPE, not connection.** `ListCrossScopeDiscoveryVerdicts`
   now excludes rows whose scope (account/project/subscription/tenancy) equals
   the current scan's scope. A decline on AWS (account X) surfaces on a GCP
   scan (project Y) even when both ran through the same IaC repo.
2. **Opt-in flag.** Cross-cloud pooling relaxes a deliberately-tested
   per-provider isolation invariant (`*ProviderIsolation` tests). Rather than
   flip that default for every deployment, it is gated behind the
   `SQUADRON_DISCOVERY_CROSS_CLOUD_CITATIONS` env flag (default off). Off
   preserves isolation (those tests pass unchanged); on enables pooling.

Proof: `TestDiscoveryProposerLearning_CrossCloudCitation` (bridge) and
`TestAdapterCrossCloudCitation_WiredPath` (production assembly adapter, single
shared IaC connection, both flag states) are deterministic. The released claim
("the citation crosses clouds") is reproducible by setting the flag.

## Scope / honest framing

- Slice 1 gates on the *current* connection's opt-in only; a source
  connection's own opt-out is not yet honored for cross-propagation (one
  tenant, one control plane — acceptable for slice 1, noted for follow-up).
- Redaction/truncation unchanged (verdictprompt.reasonField, 240-char cap,
  RedactSecrets).
- Release notes state plainly that (a) cross-cloud citation shipped after
  the post that described it, and (b) the first cut (v0.89.248) was inert and
  v0.89.249 made it actually work behind an opt-in flag.
- Default is OFF: operators opt in via SQUADRON_DISCOVERY_CROSS_CLOUD_CITATIONS.
  Same-cloud, cross-repo verdicts (same scope, different connection) are out of
  scope and not surfaced as cross-cloud citations.
