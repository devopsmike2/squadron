# Squadron Roadmap: The AI Ops Copilot for Engineering Teams

This roadmap reflects the post-v0.52 strategic pivot. Squadron is
no longer marketed as "compliance gate for AI-proposed OTel changes
in regulated industries." It is now marketed as "the AI ops copilot
that catches the problem, drafts the fix, runs it with your
approval, and writes the post-mortem ticket before you finish your
coffee." The compliance pack stays in the product as a feature
pillar; regulated buyers are the second act, not the first.

The buyer is the engineering manager or platform team lead who is
exhausted from their team writing incident tickets at 2 AM. The
distribution channels are engineer-to-engineer: Hacker News, Reddit
r/devops, dev.to, the OpenTelemetry community Slack, engineering
podcasts, and LinkedIn posts framed as "I built this because the
problem broke my team."

Each move below is a ticket. Sub tasks are checkboxes. Effort
estimates are focused engineering days; double them for contractor
after hours calendar time. Dependencies are stated explicitly.

---

## Dependency Graph

```
Move 8 (Content)  ──────────────────────────────────► always parallel
Move 1 (Demo loop, config-change) ──┬─► Move 2 (Action runner)
                                     ├─► Move 3 (Ticket auto-draft)
                                     └─► Move 4 (Proposals refactor)
                                              └─► Move 5 (Multi-cloud)
                                              └─► Move 6 (Agent SDK)
                                              └─► Move 7 (AI governance evidence)
```

## Quarter Plan

| Quarter           | Work                                                              |
| ----------------- | ----------------------------------------------------------------- |
| Q1 (4-6 weeks)    | Finish Move 1 demo loop, draft Move 2 action runner design        |
| Q2 (6-12 weeks)   | Ship Move 2 (action runner MVP) plus Move 3 (auto-drafted ticket) |
| Q3                | Move 4 proposals refactor, Move 8 content compounds, first paid pilots |
| Q4                | Move 5 multi-cloud, Move 6 agent SDK, Move 7 evidence updates     |

---

## Move 1: Build the Demo Loop (telemetry config-change flow)

**Status:** In progress, SQ-1.1 through SQ-1.6 shipped; SQ-1.7 next
**Effort:** ~7 focused days remaining, ~3 calendar weeks
**Dependencies:** None, foundational

**Goal.** Produce a working two-minute video showing the end-to-end
loop for the telemetry config-change case: cost spike fires, Claude
consumes the alert plus context, AI emits a structured rollout
proposal back to Squadron's API, require_approval forces the
rollout into pending_approval, human gets a Slack notification with
Claude's reasoning attached, human approves, rollout stages, cost
recovers. This becomes the first half of the engineer-facing demo.

The action runner version of the loop (Move 2) extends this with
node actions; the two demos together tell the full story.

**Sub tasks** (status as of pivot)

- [x] SQ-1.1: schema migration (proposed_by, proposal_reasoning, evidence_refs)
- [x] SQ-1.3: ProposeFromCostSpike service method in internal/ai
- [x] SQ-1.4: bridge daemon wires cost spikes to AI proposer
- [x] SQ-1.5: proposal.created / evidence_linked / declined audit events
- [x] SQ-1.6: webhook payload surfaces AI reasoning + evidence
- [ ] SQ-1.7: UI badge on rollouts page rows when proposed_by=ai
- [ ] SQ-1.8: UI reasoning panel + evidence section in approval drawer
- [ ] SQ-1.9: seed demo scenario in fleetsim (make target)
- [ ] SQ-1.10: stress test proposer (50 runs against seeded scenario)
- [ ] SQ-1.11: record screen capture via Chrome MCP
- [ ] SQ-1.12: generate voiceover via ElevenLabs clone
- [ ] SQ-1.13: composite final MP4 with ffmpeg

**Acceptance criteria.** A two-minute MP4 with audio narration shows
the full config-change loop. Reproducible from a single make
target. Audit timeline contains every step of the chain.

---

## Move 2: Action Runner (NEW centerpiece)

**Status:** Next after Move 1 demo lands
**Effort:** ~6-8 weeks for MVP
**Dependencies:** Move 1 (proves the propose-approve-execute loop
shape works for config changes; the action runner extends the
same loop to node actions)

**Goal.** Build the opt-in daemon that lets Squadron execute scoped
node actions after approval. This is what turns Squadron from a
config-push tool into an AI ops copilot that can actually fix
problems. Design doc lives at `docs/action-runner-design.md`; this
roadmap entry tracks delivery.

**Sub tasks** (sketch, expanded in the design doc)

- [ ] SQ-2.1: action protocol spec (request/response, signing,
  capability declaration) finalized in design doc
- [ ] SQ-2.2: storage schema for actions (action_definitions table,
  action_runs table, capabilities per runner)
- [ ] SQ-2.3: extend Proposal model so a proposal can carry either
  a rollout spec or an action spec or both
- [ ] SQ-2.4: cmd/squadron-action-runner Go binary
- [ ] SQ-2.5: first action type, restart-systemd-service
- [ ] SQ-2.6: signed action dispatch (Squadron signs, runner verifies)
- [ ] SQ-2.7: action audit events (action.proposed, action.approved,
  action.executed, action.failed)
- [ ] SQ-2.8: UI surfaces action proposals in approval drawer with
  dry-run output preview
- [ ] SQ-2.9: end-to-end demo (cost spike → AI proposes action →
  human approves → runner executes restart → metrics confirm)
- [ ] SQ-2.10: paid external security review of the signing scheme

**Acceptance criteria.** A signed action request flows from
Squadron to an installed runner, executes the restart-systemd-service
action against a specified unit name, returns the result, and
records the full chain in audit. Runner refuses any action not in
its declared capability set.

---

## Move 3: Auto-Drafted Incident Ticket

**Status:** After Move 2 ships the action runner
**Effort:** ~2-3 weeks
**Dependencies:** Move 2 (need a completed action execution to draft
the ticket; can ship a partial version against config-change-only
loops earlier if needed)

**Goal.** Close the engineer's loop. After a proposal executes
(either a config change or an action), Squadron uses the assembled
context plus the AI to draft a complete incident ticket in the
engineer's issue tracker, with title, description, investigation
trail, resolution, verification metrics, and a placeholder for
lessons learned. The engineer reviews, edits, and posts. This is
the moment that turns "I saved 30 seconds clicking through Squadron"
into "I saved 30 minutes writing this up."

**Sub tasks**

- [ ] SQ-3.1: ticket draft service in internal/ai that consumes the
  full audit chain for a spike+proposal+execution and emits a draft
- [ ] SQ-3.2: integration with Jira, Linear, and GitHub Issues
- [ ] SQ-3.3: UI "draft ticket" button on completed proposals
- [ ] SQ-3.4: configurable templates per organization (matches their
  incident format)
- [ ] SQ-3.5: audit event ticket.drafted with the issue-tracker URL

**Acceptance criteria.** From a completed proposal, one click
produces a draft ticket in the configured tracker with the full
incident narrative pre-populated.

---

## Move 4: Formalize the "Proposal" Concept

**Status:** Backlog, after Move 3
**Effort:** ~3-4 weeks
**Dependencies:** Move 2 and Move 3 (so the unified Proposal type
covers config-change, action, and ticket-draft origins from the
start)

**Goal.** Refactor rollouts, recommendations, actions, and alert
actions into a unified `proposals` model. Single API surface for
every change. Compliance evidence trail consistent across change
types.

(Sub tasks unchanged from prior roadmap; preserved here for
continuity. See git history of this file for the full list.)

---

## Move 5: Multi-Cloud and Cross-Environment Polish

**Status:** Backlog
**Effort:** ~1-2 weeks
**Dependencies:** Move 4

**Goal.** First-class cloud provider and environment attributes.
Per-environment policy. Action runners can target a specific
environment slice. Fleet Map clusters by cloud provider.

(Sub tasks unchanged.)

---

## Move 6: Agent SDK and Integration Surface

**Status:** Backlog
**Effort:** ~2-3 weeks
**Dependencies:** Move 4

**Goal.** Make Squadron trivially easy to integrate from any AI
vendor or custom proposer. Squadron becomes the gate every AI
vendor's output flows through in regulated environments.

(Sub tasks unchanged. With the engineer-copilot pivot the SDK is
less time-critical because we are now selling the complete tool to
engineers, not selling the gate to AI vendors. SDK work pushes to
Q4 unless a partnership opportunity makes it earlier.)

---

## Move 7: AI Governance Compliance Evidence (was Move 5)

**Status:** Backlog
**Effort:** ~2-3 weeks, writing-heavy
**Dependencies:** Moves 1 through 4

**Goal.** Capture the evidence regulated buyers will eventually ask
for around AI in operations. NIST AI RMF mapping plus updates to
the existing NIST CSF, NERC CIP, SOC 2, and HIPAA mapping docs.
This is the second-act story for when engineering-team customers
grow into compliance-grade buyers, or when a regulated buyer
finds Squadron through engineering-team word of mouth.

(Sub tasks unchanged.)

---

## Move 8: Content and Marketing Cadence (reframed)

**Status:** In progress, Buffer pipeline being set up
**Effort:** Ongoing, 2-3 posts per week sustained
**Dependencies:** None, parallel with everything

**Goal.** Build the LinkedIn and engineer-community audience for
the new positioning. The audience is engineering managers and
platform team leads, not compliance officers. The content threads
through the engineer's lived experience.

**Theme rotation through the launch window**

- Weeks 1-3: "OTel collector management at scale is harder than
  any vendor tells you" (lived-experience posts about the specific
  pains)
- Weeks 4-6: "Incident tickets are the second job nobody asked for"
  (the problem auto-drafted tickets solve)
- Weeks 7-9: "How we let AI run ops without giving it the keys"
  (the propose-approve-execute pattern and the action runner story)
- Week 10: Demo video launch (config-change loop first, action
  runner teased)
- Weeks 11+: Action runner posts as that feature ships

**Channels beyond LinkedIn**

- Hacker News launch when v1.0 (config + action + ticket) ships
- Reddit r/devops, r/sre, r/kubernetes for the action-runner story
- OpenTelemetry community Slack for the OTel-management story
- One engineering podcast appearance per quarter (Software Engineering
  Daily, The InfoQ Podcast, Changelog)
- Conference submissions: KubeCon, SREcon, Monitorama
  (engineer-focused, not compliance-focused)

(Sub tasks updated to match new audience.)

---

## What the Pivot Costs Us, Honestly

The NERC CIP / SOC 2 / HIPAA mapping work we did over the past two
weeks is not wasted; it stays in the product as a feature pillar
and gets reused when engineering-team customers grow into compliance
buyers. The compliance pack code we split out in v0.52 still ships;
operators who run the Enterprise binary still get policy
enforcement, change windows, signed SIEM export, and the per-call
audit trail. The story we tell about that work changes from "this
is why you should buy Squadron" to "this is what makes Squadron
defensible when your customers grow up."

The pivot gains: a buyer who is reachable through channels the
founder actually uses, a sales cycle measured in weeks rather than
quarters, a deal size that compounds rather than waits, and a
product story that maps cleanly onto the founder's own lived
experience as an engineer. The founder narrative ("I was an
engineer at a big company, managing OTel collectors was painful,
I built the tool I wished I had") is the strongest version of this
story and it is true.

## Status Conventions

- **In Progress:** active work this week
- **Next:** queued for the next sprint cycle
- **Backlog:** acknowledged but not started
- **Done:** acceptance criteria met, with a link to the artifact

## Update Cadence

Review this roadmap weekly. Move items between status columns as
work progresses. The dependency graph is the contract: do not start
a downstream move until its upstream has hit acceptance.
