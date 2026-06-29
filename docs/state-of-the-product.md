# Squadron — State of the Product (inventory + priority)

_Last updated: 2026-06-29 against `main` @ v0.89.304. Decision-support doc:
consolidates what is validated, what is left to build for OSS, and what is
scoped for Enterprise, so we can sequence work. Companion to
`docs/oss-working-features-audit.md` (deeper per-feature audit) and
`docs/oss-ga-and-enterprise-boundary.md` (rationale + GTM)._

Legend: **✅ Verified live** = exercised end-to-end against real infra.
**🟢 Works** = test-covered / previously live, not re-walked head-to-toe.
**🟡 Partial** = runs with documented gaps. **⛔ Not built** = scoped, not started.

---

## 1. OSS — validated & working today

**Marquee loop (most battle-tested):**

- ✅ Multi-cloud discovery — AWS, GCP, Azure, OCI. All four connectors validated
  against real accounts; inventory checked vs an independent oracle. Compute, DBs,
  K8s, serverless, object stores, load balancers, event sources, orchestration
  (depth varies per cloud — see `docs/detection-coverage.md`).
- ✅ AI recommendations via the real LLM — ranked, plain-English, merge-ready
  Terraform (BYO `ANTHROPIC_API_KEY`).
- ✅ IaC GitHub remediation loop — opens a real merge-ready Terraform PR
  (HCL-aware merge + `terraform validate` gate); merge/close webhook feeds
  verdict learning (decline → future citation). Real PRs opened + merged.
- ✅ env → Terraform import-block generation (OCI verified live to a real PR).

**OTel fleet control plane:**

- ✅ OTel agent deploy → config injection → Fleet registration. **New this week:**
  a pipeline-deployed collector, configured by Squadron's injected `otlphttp`
  exporter (delivered as a PR), ships telemetry and auto-registers in Fleet.
  Verified live on a real EC2 host (v0.89.301–304).
- ✅ Passive OTLP discovery — **widened this week (v0.89.304):** any standard
  collector now registers, even without a UUID `service.instance.id` (identity is
  synthesized from `host.name`/`service.name`). Previously dropped silently.
- 🟢 Fleet map / Agents / Groups; staged rollouts (dwell + auto-abort); config
  editor (Monaco + AI Assist + Lint + live pipeline view). Fleet + pipeline-health
  surfaces moved to ✅ after the hardening pass.
- 🟢 Cost Insights + Savings ($/mo projection, Quick Wins); Alerts; Audit log + AI
  explain; Incidents drafter; Demo mode (no cloud account); continuous discovery
  (persistence, history, scheduled re-scans, drift); cross-cloud verdict learning.

**Onboarding / platform:**

- ✅ Frictionless first run — single-port `:8080` image, quickstart wizard, demo
  mode. New-user happy path validated.
- 🟢 Auth — Bearer tokens + scopes, opt-in, loud warning when disabled.
- ✅ OTLP/HTTP ingestion robustness — **new this week (v0.89.302):** receiver now
  decompresses gzip (the default for OTel exporters); previously every standard
  client got a 400 and dropped silently.

---

## 2. OSS — still to build

### 2a. GA / launch-readiness (mostly collateral, finite — this is the gate)

- ⛔ **Public demo asset** — hosted sandbox OR a 2–3 min recorded walkthrough of
  discovery → AI rec → PR. **#1 conversion gap.** Highest leverage.
- ⛔ **Community plumbing** — CONTRIBUTING, issue/PR templates, a support channel
  (Discussions/Discord), clear "report a bug" path.
- 🟢 OSS-vs-paid page, README known-limitations, security one-pager, CI headline
  smoke gate — done; keep current.
- (nice-to-have, not gating) opt-in anonymous usage telemetry; richer seeded
  screenshot dataset.

### 2b. Product depth / honest gaps (incremental, demand-ordered)

- 🟡 **Detection coverage is not uniform.** Real metric-backed on many axes;
  honest-deferred elsewhere. Known: AWS Lambda + Azure Functions cold-start need
  Lambda Insights / App Insights (→ Enterprise); OCI queue poison-rate has no
  native metric (#159); AWS SQS depth-based poison-rate is an open enhancement
  (#156).
- 🟡 **Event-source breadth.** Covered: EventBridge, Pub/Sub, Service Bus, OCI
  Streaming, SQS. Tail (SNS-w/TF, Cloud Tasks, Event Grid, Event Hubs, Pub/Sub
  Lite, ONS, Queues) + serverless/orchestration kinds are incremental adds.
- 🟡 **OTLP ingestion breadth** — gzip done; JSON content-type, other
  Content-Encodings, and partial-success responses are not yet handled (a
  defensive sweep, surfaced by the gzip find).
- 🟡 Cost projection is directional (depends on configured backend rates).
- 🟡 Recommendation quality is LLM-dependent — deterministic snippets audited;
  free-form reasoning is review-gated by design (every fix is a PR).

---

## 3. Enterprise — to build (open-core boundary)

Principle: **breadth + core loop = OSS; depth + scale + governance + support =
Enterprise.** None of §1 moves out of OSS. All of the below is ⛔ not built.

- ⛔ **Identity & access** — SSO (SAML/OIDC), SCIM, full RBAC, multi-tenancy.
  (OSS ships Bearer tokens + scopes.)
- ⛔ **Rollout governance** — approval chains, change windows, policy-as-code on
  what AI may propose.
- ⛔ **Compliance** — long-term + tamper-evident audit retention, SOC 2 evidence
  exports, access reviews.
- ⛔ **Scale & HA** — clustered control plane, Postgres/managed store, 10k+
  agents, multi-region.
- ⛔ **Advanced detection** — signals needing paid telemetry layers (Lambda
  Insights, App Insights) + anomaly/ML.
- ⛔ **Cost at org scale** — showback/chargeback, budgets + forecasting,
  multi-backend rate management.
- ⛔ **Deployment options** — air-gapped, BYO/on-prem LLM.
- ⛔ **Support** — SLAs, managed SaaS, onboarding services.

---

## 4. Recommended priority

The product core is GA-quality; the gap to launch is **collateral + conversion**,
not features. Sequence:

1. **Finish the OSS GA bar (this week).** Public demo asset (recorded walkthrough
   of discovery → AI rec → PR is the cheapest credible version) + community
   plumbing. These convert the verified product into adoption. Everything else in
   §2a is done.
2. **Launch OSS + open the enterprise scope in parallel.** Don't gate launch on
   closing every §2b gap — they're incremental and demand-ordered.
3. **First enterprise wedge: Identity (SSO/RBAC/multi-tenant).** It's the first
   thing a second team asks for and the most common paid trigger in the
   OSS-led playbook; it gates every other enterprise feature.
4. **Opportunistic OSS depth** as adoption reveals demand (event-source tail,
   OTLP ingestion breadth, the deferred detection axes that don't require paid
   telemetry layers).

The discipline that got us here — dogfood the real path end-to-end before
claiming it works — is what to keep doing: the OTel-agent e2e this week found
four real bugs (v0.89.301–304) that unit tests missed. Walk every marquee path on
a real deployment before it goes in a demo.
