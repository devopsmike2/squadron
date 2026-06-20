# Post 14: OSS and vendor-neutral are the same conversation

**Pillar:** Squadron
**Tag at publish:** v0.85.0
**Visual evidence:** A screenshot of the architecture diagram at
`docs/diagrams/architecture-current-and-future.svg`, cropped to
include the dashed Future tier (Cloud connectors, Discovery
engine, IaC orchestrator) and the footer line naming the
customer backend (Datadog, Honeycomb, Grafana, etc.) as
interchangeable. The footer line is the load-bearing element —
the architectural commitment to "the backend is the customer's,
not Squadron's" is right there in the same frame as the slice-1
discovery components.
**Hashtags:** #OpenTelemetry #ObservabilityControlPlane
**Target word count:** 200-400

## Draft

A control plane has to be OSS to be trustworthy across backends.
A control plane has to be vendor-neutral to be useful across
backends. These are not two arguments. They are the same
argument.

A non-OSS control plane is a vendor pretending the layer above
the backends is theirs alone to define. The customer is asked
to take the decisions about what to observe — and the audit
trail of those decisions — on the vendor's word. That works
for a single-backend customer. It breaks the moment the
customer adds a second backend, because now the closed-source
control plane has a commercial reason to prefer one over the
other. The vendor neutrality is the first thing to go.

A non-vendor-neutral control plane is just a vendor's agent. It
will dispatch instrumentation to the vendor's backend. It will
optimize the recommendations for the vendor's pricing. It will
not exist after the vendor's next pivot.

Squadron's posture is OSS, the Apache 2.0 license is in the
repo root, and the architecture diagram in the design docs
names Datadog, Honeycomb, and Grafana in the footer as
interchangeable customer-side substrate. The v0.85.0 discovery
slice extends the same posture into the cloud: the operator
connects an AWS account, the proposer reasons about what is
uninstrumented, and the recommendation lands as a Terraform
snippet the operator's IaC pipeline runs. Squadron does not
pick the customer's backend. The discovery context carries a
`PreferredBackend` field for cases where the operator has
already chosen — and the proposer respects it — but Squadron's
own architecture is indifferent.

That indifference is what "OSS and vendor-neutral are the same
conversation" means concretely. The IaC orchestration commits
to the customer's existing infrastructure pipeline as the
execution layer. The backend commits to the customer's
existing telemetry layer as the destination. Squadron sits
above both, decides, and audits. Neither commitment can be
made by a tool that has a commercial preference for one
backend or one IaC vendor.

Repo at the v0.85.0 tag. License is Apache 2.0. Backends are
not picked.

#OpenTelemetry #ObservabilityControlPlane

## Visual asset spec

- **Filename:** `assets/post-14-architecture-future-tier-with-backend-footer.png`
- **Surface:** the SVG at
  `docs/diagrams/architecture-current-and-future.svg`, exported
  as PNG. Crop to include the dashed Future tier (Cloud
  connectors, Discovery engine, IaC orchestrator) plus the
  footer line naming the customer backend (Datadog,
  Honeycomb, Grafana, etc.) as interchangeable. The Surface,
  Decision, and Substrate tiers can be present in the upper
  half of the frame for context — the footer line is the
  load-bearing element.
- **Annotations:** one thin underline on the footer line
  naming the backends, captioned "interchangeable — Squadron
  is neutral", added in post-processing. One thin underline
  on the Discovery engine box in the Future tier, captioned
  "v0.85.0 slice 1 — AWS". Two annotations total; they make
  the architectural commitment to vendor neutrality visible
  alongside the slice that proves the commitment is recent.
- **Crop:** include the SVG's title bar so the reader can see
  this is the same architecture diagram referenced from
  `docs/universal-discovery-design.md`.

## Anti-pattern guard

Resists **the competitor takedown** from linkedin-rollout.md
"Anti-patterns to avoid". The pull is hard on this topic — the
OSS-versus-closed-source frame invites swipes at every closed
control plane on the market. The post instead frames the
position structurally: a non-OSS control plane has commercial
reasons to prefer one backend; a non-neutral control plane is
the vendor's agent. Neither sentence names a competitor by
name. The architecture diagram does — Datadog, Honeycomb,
Grafana appear in the footer as substrate, not as targets.
The reader infers the position; the post does not run anyone
down.
