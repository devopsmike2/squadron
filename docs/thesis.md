# Squadron thesis

> **Squadron — universal insight, dynamic discovery, intuitive
> remediation, user friendly.**
>
> The OSS observability control plane that connects every
> environment, recommends what to observe, orchestrates the
> change through your existing IaC, and never holds your
> production keys.

Last revised: v0.84.0.

This document exists so anyone reading the codebase — contributor,
security reviewer, future-self, potential commercial partner —
understands what Squadron is trying to be. It is the load-bearing
constraint that every design choice in this repository should be
checked against. When a feature proposal fights this thesis, the
proposal loses.

## What Squadron is

Squadron is the OSS control plane for OpenTelemetry. It manages
collector configurations, sequences rollouts safely, and runs an
AI proposer that turns cost spikes and incidents into actionable
plans with operator approval gates.

It is not a backend (Datadog, Honeycomb, Grafana, Splunk own
that). It is not an SDK (OpenTelemetry owns that). It is the
layer between the two — the place where decisions about *what to
observe, how to configure it, and when to ship changes* live.

## The thesis

Every serious organization is heading toward the same end state:
multiple observability backends (logs in one place, traces in
another, metrics in a third), agents converging on OpenTelemetry,
clouds that drift constantly, and engineers who can't keep up
manually. The bottleneck is no longer ingestion price or query
performance — it's the human cost of deciding *what should flow,
how it should be shaped, and when to roll the change out*.

Squadron's bet is that this decision layer becomes its own
product category — the observability control plane — and that
the winner of that category will be:

1. **OSS-native**, so it can sit between heterogeneous backends
   without vendor capture
2. **AI-augmented**, so engineers don't have to make every
   decision manually
3. **Audit-complete**, so security and compliance reviews don't
   block adoption
4. **Orchestrating, not executing** for any change that touches
   the customer's cloud or production — Squadron emits the
   change; the customer's existing IaC pipeline runs it
5. **Universally aware** of the customer's environment — cloud
   accounts, on-prem fleets, k8s clusters, bare metal — so the
   proposer can reason about the whole picture, not just the
   slice it can see

Today (v0.84.0) Squadron has #1, #2, and most of #3. Building
#4 properly and #5 honestly is the multi-year arc.

## Why now

Three windows opened in the last 24 months and they overlap.

**OpenTelemetry adoption hit its tipping point.** OTel is now the
default for new instrumentation in most languages. The backends
that lock you to their proprietary agent are losing relative
share. The OSS collector is mature. The OpAMP spec for control-
plane traffic shipped. This means a vendor-neutral control plane
*can actually work* across the customer base — five years ago it
couldn't, because OTel adoption was too low.

**AI reasoning matured enough to be in the loop.** Frontier
models reliably emit structured JSON, follow decision frameworks,
and respect refusal modes. The proposer pattern Squadron has
hardened across v0.79 → v0.84 (plan-kind output, live corpus
bench, playground) is now a stable substrate. Five years ago the
AI half of the thesis would have been a bet; today it's
operational.

**The control plane slot is unowned.** Datadog and Dynatrace
have auto-discovery but you have to use their agent and their
backend. Honeycomb and Grafana are backends without a control
plane layer. The OpenTelemetry Operator manages collectors on
k8s but doesn't think about them strategically. There is no OSS
project positioned at "I know your whole environment, I decide
what to observe, I orchestrate the rollout safely, I'm
vendor-neutral on the backend." That slot is open and large.

## What we win if we land this

The strategic position is not "compete with Datadog" — it's
*sit above the backends as the layer that decides what flows
into them*. From that altitude:

- **Backends become interchangeable** from the customer's
  perspective. The customer's commitment is to Squadron's
  decisions and audit trail, not to any particular ingest URL.
- **Multi-cloud and hybrid become solvable**. The customer
  describes their environment once; Squadron reasons across
  AWS + GCP + on-prem k8s + bare metal as one fleet.
- **Compliance becomes a feature**, not friction. Every AI
  decision is in the audit timeline. Every cloud-mutating
  action goes through the customer's IaC. Regulated industries
  get a story they can take to their auditor.
- **The wedge compounds**. Engineers who run Squadron for OTel
  control plane discover the proposer. Then the bench. Then the
  playground. Then discovery. Each surface deepens the
  commitment. The switching cost grows organically.

Historical analogies that match this trajectory:

- **Terraform won the IaC slot** by being multi-cloud, OSS, and
  state-as-API. The cloud vendors integrated *with* it rather
  than killing it.
- **Kubernetes won the orchestration slot** by being OSS,
  opinionated, and substrate-agnostic.
- **OpenTelemetry won the SDK slot** by being neutral on the
  backend.

Squadron's path is adjacent to all three — win the *control
plane* slot the same way OTel won the SDK slot.

## What we do not do

Some of these are temporary; some are permanent. Both kinds are
load-bearing constraints that any feature proposal must respect
or explicitly rebut.

**We do not run a backend.** No metric storage, no log search,
no trace UI. Squadron's UI shows fleet state, rollouts, audit
events, recommendations — not customer telemetry. The customer
keeps their existing backend (or picks a new one) and Squadron
orchestrates collectors to point at it.

**We do not hold long-lived cloud write credentials.** Discovery
postures use short-lived STS tokens via `sts:AssumeRole` with
`ExternalId`, scoped to read-only IAM. Any change Squadron
recommends that mutates the customer's cloud is emitted as a
Terraform / CDK / Pulumi snippet for their IaC pipeline to
execute. Squadron never has the credentials to do it directly.

**We do not replace the operator.** Every change with side
effects requires explicit operator approval. The two-person
rule (v0.61) is enforced server-side. AI proposals never
auto-merge.

**We do not break the OSS posture.** Compliance Pack hardenings
live in a separate private repository. The OSS edition is
fully functional; the Compliance Pack adds policy enforcement,
not features.

**We do not chase commercial adoption that requires gutting
this list.** If a deal is conditional on "let the AI deploy to
prod without approval" or "give Squadron our AWS root keys",
we lose the deal. The constraints are the moat.

## The phased path

Roughly five years if execution holds. Honest about timelines.

**Years 0-1 (where we are):** OpAMP-managed OTel control plane,
proposer pattern proven, plan engine, bench, playground.
Squadron is a solid OSS project that a smaller team can adopt
without security review pain. Anchor users are platform-eng
teams at series B-D companies.

**Year 1-2:** Universal discovery slice 1 — AWS read-only,
recommendations only. EC2 + Lambda + ECS first, then RDS / S3
/ ALB. No cloud-mutating actions; recommendations emit IaC
snippets. Compliance Pack adds enterprise IAM controls. First
serious enterprise pilots.

**Year 2-3:** GCP and Azure discovery. On-prem connector
(SSH-keyed, OpAMP-style heartbeat). Proposer memory loop
(Arc B, #531) — the AI learns from accepted/rejected
recommendations and gets calibrated. Action runner (Arc A,
#530) reaches GA for VM-level actions through registered
daemons; cloud actions remain IaC-orchestrated.

**Year 3-4:** Multi-backend routing. Squadron decides which
telemetry stream goes to which backend based on cost / query
needs / retention policy. The customer mixes Datadog + Honeycomb
+ Grafana + S3 archive without writing pipeline glue. The
proposer reasons about per-stream economics, not just per-
config economics.

**Year 4-5:** Squadron is the default OSS control plane in the
OTel ecosystem. Major cloud vendors integrate with it
(EKS-managed collector handlers, Cloud Run telemetry hooks,
AKS support). Managed-service offerings exist for teams that
don't want to host the control plane themselves. The category
"observability control plane" is recognized; Squadron defines
it.

Slip the timeline; do not skip the constraints. A five-year
schedule that holds the security + audit + IaC-orchestration
constraints beats a three-year schedule that breaks them.

## Risks and how we handle them

Honest about each, because the design choices flow from how
seriously we take them.

**Scope sprawl.** "Universal discovery" is what Datadog spent
12 years and billions building. The mitigation is ruthless
phased rollout: AWS first, EC2 + Lambda only, recommendations
only, no remediation in slice 1. Every slice ships independently
useful. The thesis is intact even if we stop after each slice.

**Backends respond.** Datadog could ship a "Squadron killer"
control plane in 18 months. The mitigation is the OSS + vendor-
neutral wedge — Datadog cannot ship a vendor-neutral product
without cannibalizing their own ingestion, and they won't.
Squadron has to be far enough along that "use Squadron with
Datadog as your backend" is a clean, well-documented path before
they notice.

**Discovery accuracy matters disproportionately.** A wrong
"you don't need to observe this Lambda" that misses an outage
becomes a viral failure story. The mitigation is the calibration
discipline already built into v0.83 (bench framework) and v0.84
(playground). Every recommendation surface needs the same:
graded benchmarks, dogfoodable playgrounds, operator-in-the-loop
escape hatches.

**Security review fatigue.** Every enterprise customer reviews
Squadron + AWS integration + GCP integration + on-prem
connector + the AI calls. Five conversations per deal slows
adoption. The mitigation is the audit-complete and
IaC-orchestrating posture from day one — security teams can
verify the trust model from the architecture, not just from
behavior. The Compliance Pack accelerates this further for
regulated industries.

**OSS-to-revenue path is unsaid.** Terraform captured value via
Terraform Cloud. Kubernetes spawned a hundred companies. The
honest answer for Squadron is "managed control plane +
enterprise compliance + multi-tenant", but committing to
that has implications for OSS feature gating that need to be
thought through before, not after, the OSS adoption curve. Worth
returning to this in 12 months when the OSS adoption signal is
clearer.

## What "won" looks like

In five years, three things are true:

1. **A new SRE bootstrapping observability at a series B
   company defaults to Squadron** to manage their OTel fleet,
   without anyone telling them to. The path is more obvious
   than the alternatives.

2. **A regulated-industry enterprise can adopt Squadron** with
   a security review that takes weeks, not quarters. The
   architecture answers the questions; the Compliance Pack
   handles the policies.

3. **The phrase "observability control plane" is a recognized
   category**, and Squadron defines it the way Terraform
   defined "infrastructure as code."

Outcomes ranked best to worst over a five-year run:

- **Best:** all three of the above. Squadron is the de facto
  OSS control plane. Cloud vendors integrate with it. A
  managed offering captures enterprise. The category is named
  and owned.
- **Median:** Squadron is the well-respected OSS tool in the
  space. Tens of thousands of installs, strong dev love, no
  commercial dominance but real influence on the OTel
  ecosystem direction. The thesis is partially validated.
- **Worst:** A backend incumbent ships something competitive
  2 years in and out-distributes Squadron. Squadron stays a
  niche but loved OSS project; the universal-control-plane
  category is owned by someone else. The bet didn't pay.

All three are better outcomes than most projects in this space
get. The asymmetry is on the right side.

## Constraints this thesis imposes on the codebase

Until this document is rewritten, the following are
non-negotiable:

- No feature that requires Squadron to hold long-lived cloud
  write credentials.
- No feature that auto-applies a Squadron-AI-generated change
  to a customer's production environment without explicit
  per-change operator approval.
- No feature that requires Squadron to ingest and store
  customer telemetry payloads (we manage configs; we don't
  become the backend).
- No commercial gating of OSS features that are already
  documented as part of the OSS baseline. Compliance Pack
  hardens; it does not gate.
- No coupling to a single backend vendor's API or data model.
  Backend integrations are pluggable.

When a feature proposal violates any of these, the proposal
either gets restructured to fit or gets explicitly rejected with
a written rationale that updates this document. Drift is
prevented by re-reading this doc at the start of every multi-
session arc.

## Closing

This is an OSS bet on a category that doesn't exist yet. It
takes years, holds constraints other tools don't, and depends
on the OTel ecosystem continuing in the direction it's already
heading. None of that is guaranteed.

If the bet pays, Squadron becomes the layer every observability
conversation has to go through. If it doesn't, Squadron remains
a useful OSS tool in a crowded market — which is still a
better outcome than most projects in this space get.

Either way, the constraints in the "We do not" section are
worth holding even at the cost of adoption speed. The architecture
they produce is the moat.
