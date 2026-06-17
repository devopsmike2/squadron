# Squadron Roadmap: The Policy Gate for AI Proposed Infrastructure Changes

This roadmap turns the strategic reframe from late v0.52 into trackable
work. Squadron remains an OpAMP based OTel collector control plane, but
its larger value, and the larger market, is as the deterministic
policy gate between AI agents and production infrastructure. The six
moves below get us from "tool that manages collectors" to "tool every
AI vendor's output flows through in regulated environments."

Each move below is a ticket. Sub tasks are checkboxes. Effort
estimates are focused engineering days; double them for contractor
after hours calendar time. Dependencies are stated explicitly so the
sequencing is unambiguous.

---

## Dependency Graph

```
Move 6 (Marketing)  ───────────────────────────► always parallel
Move 1 (Demo)  ─────┬─► Move 2 (Proposals) ─┬─► Move 3 (Agent SDK)
                    │                        └─► Move 4 (Multi cloud)
                    └─► Move 5 (AI governance evidence)
```

## Quarter Plan

| Quarter           | Work                                             |
| ----------------- | ------------------------------------------------ |
| Q1 (4-6 weeks)    | Move 1 demo loop, Move 5 evidence writing, Move 6 content |
| Q2 (6-12 weeks)   | Move 2 proposals refactor, Move 4 multi cloud polish, Move 6 demo launch post |
| Q3                | Move 3 agent SDK, conference talks if accepted, first paid pilot conversations |

---

## Move 1: Build the Demo Loop (AI proposes, Squadron gates)

**Status:** Next, start immediately
**Effort:** ~7 focused days, 2-3 calendar weeks
**Dependencies:** None, foundational

**Goal.** Produce a working two minute video that shows the end to end
loop: cost spike fires, Claude consumes the alert plus context, AI
emits a structured rollout proposal back to Squadron's API,
require_approval forces the rollout into pending_approval, human gets
a Slack notification with Claude's reasoning attached, human approves,
rollout stages, cost recovers in the next pipeline health window. This
video becomes the centerpiece of every Squadron conversation for the
next year.

**Sub tasks**

- [ ] Schema migration: `proposed_by` (enum: operator, ai, system),
  `proposal_reasoning` (text), `evidence_refs` (JSON array) on
  rollouts. ALTER TABLE on sqlite, parallel update in memory store.
  (0.5d)
- [ ] Update Go types and API JSON contracts for the three new
  fields. (bundled with schema)
- [ ] Implement `ProposeFromCostSpike(ctx, spike)` in `internal/ai`.
  Pull spike context plus recent pipeline health, configlint findings,
  and recommendations. Call Anthropic Messages API with tool use
  feature to constrain output to a valid rollout payload. Return
  payload plus reasoning text. (1.5d, most of which is prompt
  iteration)
- [ ] Background goroutine that polls cost_spike rows where
  `proposal_id IS NULL`, calls ProposeFromCostSpike, posts the result
  back through the existing rollout service. (0.5d)
- [ ] Audit event types `proposal.created` and `proposal.evidence_linked`
  with origin and reasoning fields. (0.5d)
- [ ] Slack notification template includes AI reasoning section plus
  the top three evidence links. Existing webhook notifier fires it.
  (0.5d)
- [ ] UI: "AI Proposed" badge on rollouts page rows. (0.5d)
- [ ] UI: AI reasoning panel plus evidence section in the approval
  drawer. Format reasoning to look like an engineer's note, not a
  chatbot transcript. (1.5d)
- [ ] Seed demo scenario in the fleetsim environment: one collector
  ships high cardinality metric, billing tile spikes, AI proposes
  dropping the offending metric. Wrap as a make target so any
  operator can reproduce. (0.5d)
- [ ] Stress test the proposer: 50 runs against the seeded scenario,
  verify proposal quality and structural consistency. Tune the prompt
  if needed. (0.5d)
- [ ] Record the screen capture via Chrome MCP. (0.5d)
- [ ] Generate the voice over with ElevenLabs voice clone. (0.25d)
- [ ] Composite and edit final MP4 with ffmpeg. (0.5d)

**Acceptance criteria.** A two minute MP4 with audio narration shows
the full loop. The seeded scenario reproduces reliably from a single
make target. The audit timeline contains every step of the chain.
Demo is shippable to LinkedIn and customer conversations.

---

## Move 2: Formalize "Proposal" as the Unifying Concept

**Status:** Backlog, start after Move 1 demo lands
**Effort:** ~3-4 weeks
**Dependencies:** Move 1 (need to see what real AI proposals look
like in production before fixing the model)

**Goal.** Refactor rollouts, recommendations, and alert backed actions
into a unified proposals model. Origin (operator, ai, system), intent
(what to do), evidence (why), policy evaluation (gates run), outcome
(applied, rolled back, rejected) become the same shape for every
change in the system. This is the durable API surface AI agents,
operator clicks, and automated triggers all target.

**Sub tasks**

- [ ] Design doc: unified Proposal type, fields, lifecycle, and
  back compat strategy for existing /api/v1/rollouts callers
- [ ] Storage migration: `proposals` table; existing rollouts table
  becomes a specialized projection or join target
- [ ] New API surface: /api/v1/proposals plus
  /api/v1/proposals/:id/approve and /reject
- [ ] Back compat: /api/v1/rollouts continues to work, internally
  delegates to proposals
- [ ] Migrate recommendations engine to emit proposals instead of a
  separate recommendation type
- [ ] Update audit events to a unified `proposal.*` shape; map old
  `rollout.*` events into the new namespace with aliases for
  compatibility
- [ ] UI: unified Proposals page replaces the standalone rollouts
  list; filters distinguish proposal origin
- [ ] Update docs and four mapping docs to reference proposals as the
  change unit

**Acceptance criteria.** Single API surface for every change. Both AI
and operator flows produce a proposal record with the same shape. The
compliance evidence trail is consistent across change types.

---

## Move 3: Agent SDK and Integration Surface

**Status:** Backlog
**Effort:** ~2-3 weeks
**Dependencies:** Move 2 (SDK targets the unified Proposals API)

**Goal.** Make Squadron trivially easy to integrate from any AI
vendor or custom proposer. Squadron becomes the gate every AI
vendor's output flows through in regulated environments.

**Sub tasks**

- [ ] Go SDK: github.com/devopsmike2/squadron-agent-sdk-go. Wraps the
  Proposals API, handles auth, retries, and the approval callback
  webhook
- [ ] Python SDK: pypi.org/project/squadron-agent. Same surface as
  Go SDK
- [ ] Public reference implementation: Claude backed cost optimizer
  agent in a new squadron-agents/ repo. This is the Move 1 demo
  repackaged as a standalone open source agent
- [ ] Integration docs: "Build an Agent for Squadron" with examples
- [ ] Webhook callback design and example: agents listen for approval
  outcomes (approved, rejected, applied, rolled back)
- [ ] Second example agent: PagerDuty fed proposer. Uses PagerDuty
  incident webhook as trigger, emits a Squadron proposal in response.
  Demonstrates that the agent contract is vendor neutral

**Acceptance criteria.** A third party engineer can build a working
Squadron agent in under an hour using the SDK and the integration
docs. Two reference implementations exist in public repos.

---

## Move 4: Multi Cloud and Cross Environment Polish

**Status:** Backlog
**Effort:** ~1-2 weeks
**Dependencies:** Move 2 (cleanest if proposals already model the
target as a structured field)

**Goal.** Make cloud provider and environment first class attributes.
Enable per environment proposal scoping. A proposal can target
"prod fleets across AWS and Azure" but not staging in GCP.

**Sub tasks**

- [ ] Add `cloud_provider`, `region`, `environment` to the Agent
  struct
- [ ] Auto detect from OpAMP attributes where available
  (`cloud.provider`, `cloud.region`, `deployment.environment`)
- [ ] Surface in Fleet Map: cluster nodes by cloud provider, show
  region badges
- [ ] Group attributes: per environment `require_approval` and
  `change_windows` (Compliance Pack extension)
- [ ] Proposal targeting: scope to environment slice of a group
- [ ] Filter chips on Agents, Rollouts, and Proposals pages

**Acceptance criteria.** A proposal targeting "all prod fleets across
AWS and Azure" applies to exactly those agents. Fleet Map renders the
multi cloud picture cleanly.

---

## Move 5: AI Governance Compliance Evidence

**Status:** Next, run in parallel with Move 1
**Effort:** ~2-3 weeks, writing heavy
**Dependencies:** Move 1 (need the AI proposed audit events to map
against)

**Goal.** Capture the evidence regulators are starting to ask for
around AI in production operations. Position Squadron as the first
compliance grade tool for AI in regulated environments. NIST released
the AI Risk Management Framework in 2024; no OTel control plane has
mapped to it yet. We can stake the claim early.

**Sub tasks**

- [ ] Add `proposal_origin` and `proposal_reasoning` to every audit
  event type that can be linked to a proposal
- [ ] Add `proposal_id` back reference on existing event types where
  applicable
- [ ] Write the fifth compliance mapping doc: NIST AI RMF v1.0
  mapping. Cover the GOVERN, MAP, MEASURE, and MANAGE functions
- [ ] Update existing NIST CSF doc with an AI governance subsection
  pointing at the new mapping
- [ ] Update NERC CIP doc: CIP-013 supply chain mapping extends to AI
  components in the operations toolchain
- [ ] Update SOC 2 doc: CC8.1 change management coverage explicitly
  includes AI originated changes
- [ ] Update HIPAA doc: §164.308 administrative safeguards include
  AI decision logging
- [ ] Update the one pager to surface AI governance as a positioning
  pillar alongside collector management

**Acceptance criteria.** Squadron is the first published OTel control
plane with a NIST AI RMF mapping. The compliance evidence package
includes AI specific event fields that auditors can filter on.

---

## Move 6: Content and Marketing Cadence

**Status:** In progress, Buffer pipeline being set up
**Effort:** Ongoing, 2-3 posts per week sustained
**Dependencies:** None, runs in parallel with everything

**Goal.** Build the LinkedIn audience for the new positioning before
the Move 1 demo lands. By the time the demo ships, the audience is
primed to understand what they are seeing.

**Sub tasks**

- [ ] Buffer Creator tier signed up, API key delivered
- [ ] ElevenLabs voice clone trained, voice ID delivered
- [ ] First batch of 6 posts written (3 educational, 3 personal)
- [ ] Posting cadence locked: Tuesday and Thursday mornings, US
  Eastern
- [ ] Theme rotation through the launch window:
  - Weeks 1-3: "Why OTel collector management is harder than vendors
    will tell you"
  - Weeks 4-6: "How regulated industries actually adopt AI"
  - Weeks 7-9: "Deterministic gates: why AI in ops needs them"
  - Week 10: Demo video launch with companion post
- [ ] Lead magnet: NIST AI RMF mapping PDF (output of Move 5)
  available for download
- [ ] Conference talk submissions: S4 ICS, GridSecCon, HIPAA Summit,
  AICPA SOC

**Acceptance criteria.** 30 or more posts published in the first
quarter. Around 500 followers gained. At least three inbound DMs from
qualified prospects. At least one conference talk accepted.

---

## Status Conventions

- **In Progress:** active work this week
- **Next:** queued for the next sprint cycle
- **Backlog:** acknowledged but not started
- **Done:** acceptance criteria met, with a link to the artifact

## Update Cadence

Review this roadmap weekly. Move items between status columns as work
progresses. The dependency graph is the contract: do not start a
downstream move until its upstream has hit acceptance.

Squadron stays the policy gate. Everything else, including the AI
proposers, the dashboards, the integrations, plugs into that gate
through the Proposals API.
