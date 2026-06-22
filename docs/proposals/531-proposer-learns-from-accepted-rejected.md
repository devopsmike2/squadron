# #531 — Proposer learns from accepted/rejected proposals

**Status:** slice 1 SHIPPED across v0.89.17 (engine + storage +
prompt + audit), v0.89.18 (API + UI surface for the per-group
flag), v0.89.19 (operator runbook), and v0.89.22 (audit-timeline
humanizer that surfaces verdict citation counts on
`proposal.created` events). Operators read
[proposer-learning-loop.md](../proposer-learning-loop.md) for the
runbook. This page remains the locked design that the
implementation promoted; slice 2+ candidates are open at the end.
**See also:** [ai-features.md](../ai-features.md),
[multi-step-plans-design.md](../multi-step-plans-design.md),
[audit-log.md](../audit-log.md),
[proposer-learning-loop.md](../proposer-learning-loop.md).

## 1. Problem

The cost-spike proposer
([`proposer.go:228`](../../internal/ai/proposer.go)) drafts a
rollout/plan, the operator approves/rejects it via
`RolloutService.Approve`/`Reject`
([`rollout_service_impl.go:1242,1264`](../../internal/services/rollout_service_impl.go)),
audit records `rollout.approved`/`rollout.rejected` — and the next
call to the proposer for the same group makes the same mistakes
again. Signal evaporates. Slice 1 closes the loop by feeding prior
verdicts back as in-context few-shot examples on the next call.

## 2. Non-goals (slice 1)

- Fine-tuning, distillation, RAG / embedding stores. Prompt-only.
- Automatic prompt evolution / self-edit of the system prompt.
- Cross-tenant / cross-Squadron learning. Examples stay local.
- Cross-group sharing (slice 1: same-group only — see §5).
- RL-style reward weighting.
- Discovery proposer integration (follow-on, separate spec).
- "Preferences" UI editor. Ground truth = past verdicts, nothing
  else.

## 3. Signal source

Every datum is on the existing `Rollout` row — no new persistence
for the signal itself.

- **Verdict fields**: `ApprovedBy/ApprovedAt`, `RejectedBy/RejectedAt`,
  `ApprovalNotes`
  ([`rollout_service.go:312–316`](../../internal/services/rollout_service.go)),
  set by `Approve`/`Reject`
  ([`rollout_service_impl.go:1242,1264`](../../internal/services/rollout_service_impl.go)).
- **Provenance**: `ProposedBy="ai"` filter
  ([`rollout_service.go:377`](../../internal/services/rollout_service.go)),
  `ProposalReasoning`, `EvidenceRefs`
  ([`rollout_service.go:338–340`](../../internal/services/rollout_service.go)).
- **Audit**: `proposal.created`
  ([`bridge.go:326`](../../internal/proposer/bridge.go)),
  `rollout.approved`/`rollout.rejected`
  ([`audit_service.go:135–137`](../../internal/services/audit_service.go),
  [`rollout_service_impl.go:1248,1294`](../../internal/services/rollout_service_impl.go)).

## 4. Storage

No new table. Slice 1 adds one query method on `ApplicationStore`:

```go
ListAIVerdictsForGroup(ctx, groupID string, since time.Time,
    limit int) ([]Rollout, error)
```

`SELECT … FROM rollouts WHERE group_id=? AND proposed_by='ai' AND
(approved_at IS NOT NULL OR rejected_at IS NOT NULL) AND
COALESCE(approved_at, rejected_at) >= ? ORDER BY COALESCE(...) DESC
LIMIT ?`. SQLite store
([`sqlite/sqlite.go`](../../internal/storage/applicationstore/sqlite/sqlite.go),
[`migrations.go`](../../internal/storage/applicationstore/sqlite/migrations.go)
schema v3): add an index on `(group_id, proposed_by,
COALESCE(approved_at, rejected_at) DESC)`, bump to v4.

Opt-out flag on `groups`
([`types.go:674`](../../internal/storage/applicationstore/types/types.go)):
new `LearnFromVerdicts bool`, default `true`, added in the same
migration.

## 5. Selection policy

`Bridge.assembleVerdicts(groupID)` returns up to `N=4` examples,
deterministic given `(group_id, now)`:

- **Scope**: same `group_id` only.
- **Recency**: `since = now - 30d`.
- **Mix**: ≤2 approved + ≤2 rejected, newest first within each
  bucket. Rejections weighted higher — denser "don't do this again"
  signal.
- **Cold start**: zero rows → empty block, prompt identical to v0.79.
- **Cap**: each example's reasoning passes through
  `summarize(.., 240)`
  ([`bridge.go:337`](../../internal/proposer/bridge.go)); total
  example payload bounded to ~1.5K tokens.

## 6. Prompt integration

Block appended to `buildProposeUserMessage`
([`proposer_prompt.go:184`](../../internal/ai/proposer_prompt.go))
before the final "Return your proposal" line. System prompt
unchanged. Verbatim shape:

```text
Prior verdicts for this group (operator decisions on past AI proposals):

[APPROVED] rollout_id=rlt_8ax9
  reasoning: container.id was driving 60% of the spike; canary 10% / 600s.
  approver_notes: "good plan, ship it"

[REJECTED] rollout_id=rlt_7q12
  reasoning: dropped k8s.pod.uid plus k8s.namespace in one step.
  rejecter_notes: "too aggressive — split into a plan, drop one attr per step"

[REJECTED] rollout_id=rlt_6m08
  reasoning: 100% rollout in a single stage, no canary.
  rejecter_notes: "always canary at 10% first"

Use these as preference signal. Match the shape of approved
proposals; avoid the shape of rejected ones. Do NOT cite these
rollout_ids in your evidence — they're operator history, not
evidence for this spike.
```

Empty verdict list → entire block omitted.

## 7. Privacy + opt-out

Pick: **per-group flag**, not per-rollout.

- `Group.LearnFromVerdicts=false` → `assembleVerdicts` returns empty.
- Flipped via the existing group settings handler; no new endpoint.
- Per-rollout suppression is a slice-2 question (open Q3).
- Examples route through `redact.go`
  ([`internal/ai/redact.go`](../../internal/ai/redact.go)) before
  hitting the prompt.
- Inline config snippets from rejected plans are deliberately
  excluded — only reasoning + notes ship. Config bodies are where
  the sensitive material lives.

## 8. Audit trail

Extend the `proposal.created` payload
([`bridge.go:332–343`](../../internal/proposer/bridge.go)) with:

```go
"verdict_examples_used": []string{"rlt_8ax9", "rlt_7q12", "rlt_6m08"},
```

Operators see exactly which prior rollouts informed any AI proposal.
Empty array on cold-start. No new event type; SIEM fan-out inherits
the field.

## 9. Slice 1 contract

**In:**
1. `ApplicationStore.ListAIVerdictsForGroup` + index + schema v4.
2. `Group.LearnFromVerdicts` column, default true.
3. `Bridge.assembleVerdicts` + selection policy (§5).
4. Prompt block in `buildProposeUserMessage` (§6).
5. Redaction pass on examples through existing `redact.go`.
6. `verdict_examples_used` field on `proposal.created`.
7. Cost-spike proposer only.

**Out:**
- Discovery proposer integration.
- Cross-group learning.
- Per-rollout opt-out.
- UI surface for the flag (settings JSON works; visual control later).
- Any change to the system prompt
  ([`proposer_prompt.go:24`](../../internal/ai/proposer_prompt.go)).
- Fine-tuning, RAG, embedding store.

## 10. Open questions

1. **N=4 right?** Worth an offline test against
   [`proposer_live_test.go`](../../internal/ai/proposer_live_test.go)
   to see whether 2/4/6 moves the needle.
2. **Empty approval notes.** Most operators won't type a reason on
   approve. Include unannotated approvals (signal: "shape was
   fine") or only annotated rejections?
3. **Per-rollout suppression** — slice 2, or cheap enough to fold
   in? `Rollout.ExcludeFromLearning` is one column.
4. **Plan-kind rejections**: include per-step reasoning or just the
   top-level `ProposalReasoning`? Slice 1 says top-level; revisit
   if plans dominate rejections.
5. **30-day window**: config-driven on the AI service block, or
   fixed?

## 11. Acceptance tests

1. **Cold start parity.** Group with zero AI-originated rollouts: a
   call to `ProposeFromCostSpike` produces a prompt byte-for-byte
   identical to v0.79 (no examples block emitted). Existing
   `proposer_test.go` golden passes unchanged.

2. **Approved example surfaces.** Seed one approved AI rollout in
   group G with reasoning "drop container.id, canary 10%". Fire a
   new spike on G. Assert: the user message contains
   `[APPROVED] rollout_id=<that id>`, the audit event
   `proposal.created` payload contains
   `verdict_examples_used=[<that id>]`.

3. **Mix cap honored.** Seed 5 approved + 5 rejected rollouts in
   group G within 30 days. Assert: exactly 4 examples in the prompt,
   at most 2 approved, at most 2 rejected, newest first within each
   bucket.

4. **Opt-out flag respected.** Group G has
   `LearnFromVerdicts=false`. Seed 3 approved + 3 rejected. Fire a
   spike on G. Assert: no examples block in the prompt,
   `verdict_examples_used=[]` on the audit event.

5. **Recency window.** Seed one approved rollout in G dated 31 days
   ago. Fire a spike on G. Assert: that rollout does NOT appear in
   the prompt and is NOT in `verdict_examples_used`.
