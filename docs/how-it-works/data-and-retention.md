# Where your data lives

*What this page answers: what Squadron stores, where it stores it, and exactly what — if anything — ever leaves your environment.*

Squadron is **self-hosted**. It runs in your infrastructure, and it keeps its
state there. Understanding what it holds and what it emits is the whole trust
story.

## Two kinds of state, both local

Squadron persists two conceptually distinct stores in your own deployment:

- **Operational state** — your configurations, rollout history, and the audit
  trail. Small, critical, and the thing to back up. Held in an embedded SQL
  store.
- **Telemetry** — the observability data Squadron analyzes, plus its
  rollups. This can grow large depending on your traffic and retention. Held in
  an embedded analytical store.

Both live on local disk in your deployment. Retention windows for telemetry are
configurable, so you decide how much history to keep against how much storage to
spend. For the storage layout, retention settings, and backup/restore steps,
see [Operating Squadron](../operating.md).

## Backup and restore, at a glance

Each store is a self-contained local file you can copy on a schedule and ship
off-host. Restoring is a matter of stopping Squadron, putting the files back,
and starting it again. The [operating guide](../operating.md) has the exact
commands.

## What leaves your environment: nothing by default

Out of the box, Squadron makes **no outbound calls** with your data. The only
egress is opt-in, and there are exactly two sources of it:

- **Anonymous usage reporting** — **off by default.** When you turn it on, it
  sends only low-cardinality counts (for example, the running version and a
  count of agents). No identifiers, no configuration content, no telemetry, no
  credentials. See [Usage reporting](../usage-reporting.md) for the exact
  fields.
- **AI features** — **off by default.** If you enable AI, the specific context
  relevant to a recommendation is sent to the LLM provider *you* choose. If you
  self-host the model, nothing leaves your environment at all. See
  [How AI proposals work](ai-proposals.md) and [AI Assist](../ai-assist.md).

Your telemetry — traces, metrics, and logs — stays in Squadron's local store.
It is not sent anywhere.

!!! tip "Air-gapped and self-hosted-model operation"
    With usage reporting off and either AI off or the model self-hosted,
    Squadron makes no outbound calls with your data at all. That posture is the
    default, so an air-gapped deployment is the starting point, not a special
    configuration you have to assemble.
