# Post 9: Observability is fragmented across backends and that's fine

**Pillar:** Universal insight
**Tag at publish:** v0.85.0
**Visual evidence:** A side-by-side. Left frame is the
`docs/diagrams/architecture-current-and-future.svg` diagram showing
the three-tier control plane (Surface / Decision / Substrate) with
the customer's backend (Datadog / Honeycomb / Grafana) called out
in the footer as interchangeable. Right frame is a screenshot of
the `/discovery/aws` Account tab on the live deployment at the
v0.85.0 tag, showing one connected account card. Together: the
diagram makes the claim, the screenshot proves the claim is
running.
**Hashtags:** #OpenTelemetry #ObservabilityControlPlane
**Target word count:** 200-400

## Draft

Most enterprises run more than one observability backend. Logs in
one place, traces in another, metrics in a third, an archive tier
in S3 because nobody wants to pay the hot store for last quarter's
debug logs. The slide deck says this is messy and ought to be
consolidated. The reality is that each backend exists because it
was the right tool for some team's question.

The fragmentation is not the bug. The missing layer is.

The missing layer is the control plane that sits above the
backends and decides what flows where. What the operator commits
to is the decisions and the audit trail, not any one ingest URL.
That layer is what Squadron is. It is OSS. It is vendor-neutral
on the backend by construction — the architecture diagram in the
repo names Datadog, Honeycomb, and Grafana in the footer as
interchangeable substrate, not as targets.

v0.85.0 shipped the first universal-observation slice. The
operator connects an AWS account through a read-only IAM
assume-role, scans EC2 and Lambda, sees what is uninstrumented,
and gets AI-emitted Terraform snippets for their existing IaC
pipeline. The customer's existing backend — whichever one — is
where the new instrumentation flows. Squadron does not pick the
backend. Squadron decides what to observe; the backend is the
customer's call.

That posture is why "fragmentation is fine" is not a concession.
A control plane that picked the backend would be a backend. A
control plane that refused to work across them would be a wrapper.
The architecture above the backends is the slot that does not
exist yet and the one the next ten years of observability has to
fill.

Repo at the v0.85.0 tag. The discovery surface is at
`/discovery/aws`.

#OpenTelemetry #ObservabilityControlPlane

## Visual asset spec

- **Filename:** `assets/post-9-fragmentation-architecture-plus-discovery.png`
- **Surface — left frame:** the SVG at
  `docs/diagrams/architecture-current-and-future.svg`, rendered at
  680x470 and exported as PNG. The three tiers (Surface,
  Decision, Substrate) and the dashed Future tier are visible;
  the footer line naming Datadog / Honeycomb / Grafana / etc. as
  interchangeable customer backends is the load-bearing element.
- **Surface — right frame:** the `/discovery/aws` Account tab on
  the live deployment at the v0.85.0 tag, showing the connected-
  account card after a real wizard run. The page header
  ("AWS Discovery") and the three tabs (Account / Inventory /
  Recommendations) are visible so the surface is recognizable.
- **Annotations:** one thin line connecting the dashed Future
  tier in the left frame to the Account-tab card in the right
  frame, with the caption "what shipped at v0.85.0" added in
  post-processing. No annotation on the backends footer — let it
  read.
- **Crop:** include the diagram's footer line and the browser
  address bar on the right frame.

## Anti-pattern guard

Resists **the competitor takedown** from linkedin-rollout.md
"Anti-patterns to avoid". The pull is to frame multi-backend as
"why Datadog is the past" or "why Honeycomb is missing a layer."
The post instead frames the backends structurally — they are the
substrate the thesis assumes — and points to the diagram footer
which names them by name as interchangeable. The argument is that
the slot above them is unowned, not that the backends below are
wrong. The reader infers the position; the post does not pitch
against anyone.
