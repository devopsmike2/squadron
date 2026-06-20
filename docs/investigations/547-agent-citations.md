# Investigation: #547 — Ask context bag emits no kind=agent citations

**Status:** Findings only (no fix landed in this commit)
**Investigated:** 2026-06-19
**Investigator:** Stream 3 sub-agent

## Background

Ask Squadron's context bag (the small `map[string]string` the
handler hands to `ai.Service.Ask`) was widened in v0.68 to
include agents alongside rollouts, audit events, cost spikes, and
recommendations. The handler quotes a slim per-agent summary —
name, status, drift status, group name, last seen — under the bag
key `agent:<id>`. The system prompt enumerates `agent` as one of
the five valid citation kinds. The UI's `AskSquadronDialog`
already routes `agent` chips to `/agents?agent=<id>` and colors
them emerald. Yet on the live deployment, observed Ask answers
emit `rollout`, `audit`, `spike`, and `rec` citations but never
`agent`. This task is to find out why.

## What I checked

- `internal/api/handlers/ask.go:300-318` — the `buildBag` block
  that pulls agents via `AskAgentLister.ListForAsk` and writes
  them into the bag as `agent:<id>` entries
- `internal/api/handlers/ask.go:74-77` — the `AskAgentLister`
  interface contract (`ListForAsk(ctx, limit) ([]AskAgent,
  error)`)
- `internal/api/handlers/ask.go:62-71` — the `AskAgent` slim
  shape; confirms `Name`, `Status`, `DriftStatus`, `GroupName`,
  `LastSeen` are the fields surfaced
- `internal/api/handlers/ask.go:395-413` — `summarizeAgent`,
  which emits a one-line `name=... status=... drift=...
  group=... last_seen=...` summary verbatim into the bag
- `internal/api/server.go:434-510` — the wiring layer.
  `newAskAgentsAdapter` adapts `services.AgentService`; the
  important block is the prioritization at lines 469-505
- `internal/ai/ask.go:54` — confirms `AskCitation.Kind` is
  documented as `rollout | agent | audit | spike | rec`
- `internal/ai/ask.go:60-78` — `askSystemPrompt`. The kinds enum
  in the prompt rule lists agent: "The kind is one of rollout,
  agent, audit, spike, rec."
- `internal/ai/ask.go:171-216` — `parseAskAnswer`. The
  citation tag regex (`[cite:kind:id]`) treats all five kinds
  identically; there is no per-kind filter
- `internal/api/handlers/ask_test.go:248-307` —
  `TestAskHandler_IncludesAgentsInBag`. Stubs an AI response of
  `[cite:agent:a-9] [cite:agent:a-12]` and asserts the handler
  emits `Kind: "agent"` citations. The test passes today,
  confirming the parser + handler end-to-end path works when
  the model emits agent tags
- `ui/src/components/AskSquadronDialog.tsx:280-320` —
  `citationPath` and `chipKindColor` both have a `case "agent"`
  branch. The UI is not the gap

## Findings

The end-to-end mechanical path for `kind=agent` citations is
fully implemented and tested. The handler builds the bag, the
prompt names the kind, the parser accepts the tag, the UI
renders it. The path is not broken.

The actual gap is in the **wiring adapter's prioritization**, at
`internal/api/server.go:469-505`. The adapter walks
`AgentService.ListAgents`, partitions every agent into one of
three buckets — `offline`, `drifted`, or `rest` — and then
**explicitly discards the `rest` bucket**:

```go
out := append([]handlers.AskAgent{}, offline...)
out = append(out, drifted...)
// Skip the rest bucket entirely: healthy synced agents do not
// belong in the bag. An operator asking about the fleet by
// name will hit the next bag widening (agent name search) in
// a follow on release; for v0.68 we keep the bag focused on
// what's actually wrong.
_ = rest
```

On a healthy fleet (every agent online + synced), `offline` and
`drifted` are both empty. `ListForAsk` returns `[]`. The handler's
`buildBag` iterates zero agents and writes zero `agent:*` keys.
The model has no agent rows it can cite from — and the system
prompt forbids citing anything outside the bag — so it correctly
emits no `kind=agent` citations.

The dogfood deployment and the demo seed produce healthy fleets
in the steady state. Cost spikes, recommendations, rollouts, and
audit events all surface freely (their stores keep history). The
agent bucket only fills when something is actively wrong with the
fleet. That matches the observed asymmetry in #547 exactly.

The handler-test (`TestAskHandler_IncludesAgentsInBag`) bypasses
this issue because it stubs the `AskAgentLister` with two agents
that are explicitly `offline` and `drifted`. The test verifies
the *handler* contract assuming the lister returns agents — it
does not exercise the adapter's filter behavior on a healthy
fleet.

## Root cause

The v0.68 `askAgentsAdapter` deliberately drops the `rest`
bucket (healthy online + synced agents) so the Ask context bag
never sees them. On a steady-state fleet this empties the bag
entirely, so the model cannot emit `kind=agent` citations even
though every other surface (handler, prompt, parser, UI) is
ready for them. The behavior is "by design" for the
"anything wrong?" question, but it manifests as a perceived bug
when an operator asks any other question — "what's running in
the web-prod group?", "tell me about agent host-09" — because
those questions cannot be answered with citations either.

## Recommended fix scope

The right shape is **bag widening, not adapter rewrite**. The
v0.68 commit message explicitly forecasted "agent name search"
as the next widening. That is the fix.

Concretely:

- **`internal/api/handlers/ask.go`** (~30 lines added): extend
  `AskRequest` with an optional `Focus` field (or extract entity
  references from the question itself with a cheap regex); pass
  through to the bag builder.
- **`internal/api/handlers/ask.go`** (~10 lines added): in the
  agents block, when the question references an agent by name
  or id, supplement the prioritized "interesting" list with the
  matched healthy agents so they enter the bag too.
- **`internal/api/server.go`** (~20 lines): extend
  `askAgentsAdapter` with a `ListByName(ctx, names...)` or
  similar narrow read so the handler can request specific
  agents on top of the prioritized list.
- **`internal/api/handlers/ask_test.go`** (~40 lines): new test
  covering the healthy-fleet-with-named-agent path. Verify
  agent citations land when the question names a known healthy
  agent.

Total: roughly 100 lines across three files. No schema changes,
no UI changes (the chip rendering already exists). No
migrations.

An alternative shape — surface the `rest` bucket unconditionally
up to a small cap (e.g. 3 entries) when the prioritized buckets
are empty — would land citations on the "anything wrong?"
question too, but at the cost of the JARVIS framing the v0.68
commit message argued for. Probably not the right trade. The
bag-widening shape is closer to the original design intent.

## Open questions

- **Is the original #547 task description accurate about
  observed behavior?** The description says "the Ask handler
  does not emit citations with kind=agent — only
  rollout/audit/spike/recommendation citations show." If the
  observed answers were against a steady-state healthy fleet
  this is consistent with the root cause above. If the
  observed answers were against a fleet with offline or
  drifted agents present, the root cause is elsewhere and the
  investigation needs to extend to the model side (Haiku
  declining to cite agents even when present in the bag,
  perhaps because the prompt's worked example uses a rollout
  citation only). Recommend confirming the fleet state when the
  bug was filed before scoping the fix.
- **Should the v0.68 adapter's "healthy agents are not
  interesting" choice be revisited?** The comment is explicit
  about the design intent. A change to surface healthy agents
  unconditionally would be a product call, not a bug fix.
- **Is there a measurable rate of Ask answers that should have
  cited an agent but didn't?** Without an Ask analytics
  pipeline (out of scope for v0.84), this is hard to quantify.
  The next time the dogfood deployment has an offline or
  drifted agent and an Ask question runs, that's the
  controlled test the investigation can use.
