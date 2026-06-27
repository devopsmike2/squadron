# Cross-cloud verdict citations

Status: building (slice 1). Author: autonomous session.

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
   excludeConnectionID, since, limit)` — recent pr_merged / pr_closed_not_
   merged audit rows from connections != excludeConnectionID, projecting
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

## Scope / honest framing

- Slice 1 gates on the *current* connection's opt-in only; a source
  connection's own opt-out is not yet honored for cross-propagation (one
  tenant, one control plane — acceptable for slice 1, noted for follow-up).
- Redaction/truncation unchanged (verdictprompt.reasonField, 240-char cap,
  RedactSecrets).
- Release notes will state plainly that cross-cloud citation shipped after
  the post that described it.
