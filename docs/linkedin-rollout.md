# LinkedIn drumbeat plan

Companion to `docs/thesis.md`. This document maps the public
rollout of Squadron's universal-observability-control-plane
thesis to a sequence of LinkedIn posts, slow and technical, that
land over months — not weeks. The goal is to surface Squadron to
SREs and platform engineers in a way that earns trust through
evidence, not hype.

## Brand line

Every post lands under one consistent positioning statement.

> **Squadron — universal insight, dynamic discovery, intuitive
> remediation, user friendly.**

The five-word pillars are the post-title vocabulary. A post about
the proposer playground is about *intuitive remediation*. A post
about discovery is about *dynamic discovery*. A post about the
audit timeline is about *universal insight*. A post about the
v0.81.3 in-app approve dialog is about *user friendly*. The
language is consistent because the product is consistent.

## The visual-evidence rule

**Every post must include at least one of:**

- (a) A screenshot from a real run against the live deployment
- (b) A short GIF or screen recording of an actual interaction
- (c) An embedded interactive widget hosted somewhere the
  audience can reach
- (d) A static SVG diagram exported from the design docs

**Posts that do not meet one of those four criteria do not
publish.** Wall-of-text posts in this audience get scanned,
discounted, and forgotten. The visual artifact is what makes a
post earn the click and the reshare.

Production guidance:
- Screenshots come from the actual running deployment, not
  mockups. A real proposer reasoning panel beats any hand-drawn
  approximation.
- GIFs are short — 6 to 12 seconds. Loop cleanly.
- Widgets are saved as standalone HTML files in
  `docs/widgets/` so they can be embedded in future docs or
  blog posts without rebuilding.
- SVGs are committed to `docs/diagrams/` and referenced from
  multiple posts when relevant.

## The principle

Every post is dogfooded before it's published. If the post claims
Squadron does X, X must be reproducible from the OSS repo at the
tagged version referenced in the post. Screenshots come from
the running deployment. Code blocks come from the actual files.
Logs come from real runs.

This is the explicit anti-pattern to what most OSS projects do
on LinkedIn — which is to post features that almost work,
mockups that don't reflect the build, and "we're working on"
posts that the audience correctly discounts.

The drumbeat is **slow because trust accrues slowly** in this
audience. A series B SRE who follows Squadron for six months
because every post has technical substance will adopt it. The
same SRE who sees one hype post in week one will mute the
account permanently.

## Tone guide

**Use:**

- Concrete numbers. "Token output: 1090. Cap: 4096. 26% of cap."
- Honest postmortem language. "Caught during the v0.81 E2E sweep;
  the docker-compose mount has been broken since v0.50, ~6 weeks."
- Mechanism, not magic. "The proposer's decision framework
  picked plan-kind over rollout-kind because two attributes
  argued for staging the drops."
- Operator's perspective. "An SRE looking at this on Monday
  morning sees..."
- Quiet confidence. "Squadron is the OSS layer that decides..."

**Avoid:**

- Adjectives that don't carry information. "Revolutionary",
  "next-generation", "unprecedented", "game-changing".
- Anthropomorphizing the AI. "Squadron *thinks*..." → "The
  proposer evaluates..."
- Comparisons to incumbents framed competitively. Frame them
  structurally instead: "Datadog owns the backend; Squadron
  sits above it as the control plane." Never "Squadron beats
  Datadog at X."
- Outcome promises. "Reduce your observability bill by 40%."
  Show the mechanism; let the reader infer the outcome.
- Hashtag spam. Two hashtags max per post: `#OpenTelemetry` and
  one specific to the topic (`#SRE`, `#PlatformEngineering`,
  `#ObservabilityControlPlane`).

## Phase 1 — what's already real (months 1-3)

The first 8-10 posts show what the OSS repo already ships at
v0.84.0. Every post links to a tagged release. Every demo is
reproducible by anyone who clones the repo.

| # | Topic | Hook | Evidence | Tag |
|---|-------|------|----------|-----|
| 1 | "Squadron is the OSS control plane for OpenTelemetry" | One-paragraph thesis tease. Not the full pitch. | Repo link, README, single screenshot of the fleet status page. | v0.84.0 |
| 2 | "The proposer reasons about cost spikes" | Plain-text walkthrough: a 312% spike arrives, the AI emits a 2-step plan, operator approves, engine sequences. | Screenshot of the proposer playground showing the actual reasoning text from the v0.84 dogfood run. | v0.84.0 |
| 3 | "Plans are sequences, not single rollouts" | The v0.79 plan-kind dispatch. Why staging the drops matters when two attributes drive a spike. | Diagram of step 0 → step 1 → completed; screenshot of the Plans detail page. | v0.79.0 |
| 4 | "We benchmark the proposer against the real API" | The v0.83 bench. 8 scenarios, graded outcome buckets, headroom against cap. | Screenshot of bench output showing token usage + cost + outcome buckets. | v0.83.0 |
| 5 | "Operators dogfood the proposer in the playground" | The v0.84 playground. Hand-craft a spike, see what the AI would do, no side effects. | Screenshot of the playground form + result panel. Reasoning text in full. | v0.84.0 |
| 6 | "Every AI decision is in the audit timeline" | The v0.76 humanizer + v0.81.4 server-side. Plan.created → proposal.created → rollout.approved as one trail. | Screenshot of the Timeline page recent events showing the humanized chain. | v0.81.4 |
| 7 | "Two-person approval is enforced server-side" | The v0.61 rule. The AI is the requester; a human is the approver. Not the same hand. | Screenshot of the Approve dialog with the v0.81.3 in-app UX. The 409 error message when same actor tries both sides. | v0.81.3 |
| 8 | "We caught and shipped 7 bugs in one E2E sweep" | The session that produced v0.81.1 through v0.81.4. Anti-hype, story-driven. | Commit log screenshot of the 4 tags. | v0.81.4 |

Each post 200-400 words. Includes one piece of evidence
(screenshot, log, code block). Lands one technical idea. Drives
the reader to the repo or the docs.

## Phase 2 — tease the thesis (months 3-6)

The next 6-8 posts lay groundwork for the universal-discovery
direction without committing to specifics yet. The audience
should start expecting that something larger is coming.

| # | Topic | Hook |
|---|-------|------|
| 9 | "Observability is fragmented across backends and that's fine" | Multi-backend is the future; the control plane is the missing layer. |
| 10 | "What's in your fleet that you're not observing?" | The blind-spot framing. Honest about how hard the question is. |
| 11 | "The proposer pattern generalizes" | Today it reasons about cost spikes; the same shape works for "what should we observe" if you give it inventory context. |
| 12 | "Why a control plane should never hold cloud write credentials" | The IaC-orchestrating posture. Security-first as a design constraint. |
| 13 | "Audit-complete by default is a feature, not a check-box" | What it means for regulated industries when every AI decision is auditable. |
| 14 | "OSS and vendor-neutral are the same conversation" | The strategic frame: control plane has to be both. |

These posts don't say "we're building cloud discovery" yet.
They build the *conceptual frame* the audience will use when
discovery slices ship. By the time slice 1 lands the audience
already understands why it's structured the way it is.

## Phase 3 — ship the slices (months 6+)

Posts now map 1:1 to shipped features. Each tagged release that
advances the thesis gets exactly one post. The slow drip
continues; one post per release means no faster than every 2-4
weeks.

Pattern for each post:

1. **What just shipped.** One sentence.
2. **Why it shipped this way.** Reference the constraint in
   `docs/thesis.md` ("Squadron does not hold cloud write
   credentials, so the discovery role is assume-role with
   external-id, read-only").
3. **What the operator sees.** Screenshot or short video.
4. **What's coming next.** One sentence. Not a date.
5. **Repo link to the tag.**

The first 4-6 of these are the AWS discovery slices. Each
slice is dogfoodable in the same session it's posted about.

## Anti-patterns to avoid

The list is long because the pull toward each anti-pattern is
strong; writing them down makes them easier to refuse.

- **The vision dump.** Posting the entire thesis in week one.
  The audience needs time to see the work before they care
  about the vision.
- **The hype follow-up.** Once an initial post lands well, the
  pull is to immediately post a louder version. Resist. The
  drumbeat principle: same volume, longer duration.
- **The backwards-from-marketing post.** Writing a feature claim
  then making the feature fit. Squadron has the inverse
  discipline; every post comes from a dogfooded run.
- **The competitor takedown.** Datadog, Honeycomb, Grafana are
  not the enemy. They're the substrate the thesis assumes.
  Frame them structurally, never competitively.
- **The hiring post disguised as a feature post.** If we're
  hiring, post about hiring. Don't bury it in a roadmap update.
- **The "we listened to the community" post when the
  community is 3 GitHub stars.** Earn the framing before using
  it.
- **The metrics post that's actually a vanity post.** "10,000
  stars in 6 months!" — only post numeric milestones if they
  carry a thesis-relevant message. "10,000 stars and 40% of our
  users are running it in production" carries a message.
  Stars alone don't.

## Cadence

**Months 1-3:** One post every 7-10 days. 8 posts total. Phase 1
material exhausted.

**Months 3-6:** One post every 10-14 days. 6 posts total.
Phase 2 conceptual scaffolding.

**Months 6+:** One post per shipped release tag. Approximately
one every 2-4 weeks. Phase 3 — every post is evidence.

Total: roughly 24 posts in the first 12 months, accelerating
slightly as the discovery slices ship. This is a quarter of what
a marketing-led account would post and at least 4x more
substantive per post.

## What success looks like at month 12

- Squadron's LinkedIn follower count is in the low thousands
  (target: 2-5k). Slow growth because the bar to follow is
  technical interest, not entertainment.
- At least 6 of the posts have been independently reshared by
  recognized SRE / observability voices without prompting.
- At least one hiring or partnership conversation has been
  initiated by a reader citing a specific post as their entry
  point.
- The phrase "observability control plane" has begun to appear
  in third-party writing about Squadron, indicating the
  category framing is landing.

What success does *not* look like:

- 50k followers at month 12 — that level of growth in this
  audience usually means the content has shifted toward
  entertainment rather than substance. Pull back if this
  happens.
- A single viral post with high engagement and low conversion
  to repo visits. The metric to optimize is "did serious
  practitioners look at the repo," not "did everyone like the
  post."

## When to revise this plan

Re-read this document quarterly. Update it when:

- A phase ends and the next one begins.
- The thesis document changes materially (it's the source of
  truth; this plan reflects it).
- A post lands in a way that surprises us — positively or
  negatively — and the cadence or tone should adjust.
- The first discovery slice ships, because that's the moment
  Phase 3 starts and the content shape shifts from
  "demonstrate" to "evidence."

Keep this document short. If it grows past 250 lines, prune it.
The principles are more important than the playbook.
