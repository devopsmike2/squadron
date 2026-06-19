# Post 1: Squadron is the OSS control plane for OpenTelemetry

**Pillar:** Squadron
**Tag at publish:** v0.84.0
**Visual evidence:** A single screenshot of the Squadron Dashboard
(`/`) from the live deployment, taken right after a real proposer
run. The screenshot shows fleet state, recent rollouts, and the
audit timeline strip — three surfaces in one frame that make the
"control plane" claim concrete on first glance.
**Hashtags:** #OpenTelemetry #ObservabilityControlPlane
**Target word count:** 200-400

## Draft

Squadron is the OSS control plane for OpenTelemetry.

It is not a backend. Datadog, Honeycomb, Grafana, Splunk own that
slot. It is not an SDK either — OpenTelemetry owns that. Squadron
sits between the two: the layer that decides what to observe, how
to shape the collector config, and when to roll the change out.

At v0.84.0, the OSS repo ships three things that make the control
plane real:

- An OpAMP-managed collector fleet. Configs are versioned in
  Squadron; rollouts are staged with operator approval gates.
- An AI proposer that turns cost spikes into actionable plans.
  Single-rollout when one change is enough; multi-step plans when
  staging the drops reduces regression risk. The operator approves
  once; the engine sequences the rest.
- An audit timeline that records every plan, proposal, approval,
  and rollback as one humanized chain. Two-person approval is
  enforced server-side — the AI is the requester; a human is the
  approver.

What Squadron does not do is also load-bearing. It does not hold
long-lived cloud write credentials. It does not store customer
telemetry. It does not auto-apply AI-generated changes. The
"explicitly not" list is what makes the architecture reviewable
by an enterprise security team in weeks, not quarters.

The five-year bet: the observability control plane becomes its own
category, and the winner is OSS-native, AI-augmented, audit-
complete, vendor-neutral on the backend, and never holds the
production keys.

Repo at the v0.84.0 tag. Coming next: posts on the proposer
reasoning loop, the bench, and the playground.

#OpenTelemetry #ObservabilityControlPlane

## Visual asset spec

- **Filename:** `assets/post-1-dashboard-v0.84.0.png`
- **Surface:** The Dashboard at route `/` on the live deployment,
  captured at the v0.84.0 tag, after a recent dogfood proposer run
  so the recent-activity strips are populated (not empty).
- **What must be visible in the crop:** the Squadron header with
  the brand line in the hero area; the fleet status summary; the
  recent rollouts strip showing at least one plan with multiple
  steps; the audit timeline strip showing a `plan.created` →
  `proposal.created` chain. One frame, four signals — that's the
  whole "control plane" claim made concrete.
- **Annotations:** none. The first post lets the surface speak. No
  red arrows, no callouts. A clean screenshot at 1920x1080,
  cropped to the dashboard chrome (drop the OS title bar).

## Anti-pattern guard

Resists **the vision dump** from linkedin-rollout.md "Anti-patterns
to avoid". The post tees the thesis in one paragraph, names what
ships today at v0.84.0, names what Squadron does not do, and points
the reader at the repo. It does not paste the whole `docs/thesis.md`
into LinkedIn — the audience earns the full pitch by following the
drumbeat. The closing line ("Coming next: posts on the proposer
reasoning loop, the bench, and the playground.") sets cadence
expectations without committing to dates.
