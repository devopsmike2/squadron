# Audit log

Every state change in Squadron is recorded as an `AuditEvent` row. This
is the durable record operators reach for when something needs explaining
after the fact: who pushed a config, when a rollout aborted and why,
which agents got which stage, when an alert fired.

- [Event shape](#event-shape)
- [What's recorded](#whats-recorded)
- [Filtering](#filtering)
- [The UI timeline](#the-ui-timeline)
- [Retention](#retention)
- [API reference](#api-reference)

## Event shape

```json
{
  "id": "...",
  "timestamp": "2026-05-15T14:23:01.001Z",
  "actor": "system",
  "event_type": "rollout.stage_applied",
  "target_type": "rollout",
  "target_id": "...",
  "action": "stage_applied",
  "payload": {
    "stage": 1,
    "mode": "label",
    "canary_size": 3,
    "agent_ids": ["..."],
    "label_selector": {"role": "canary"}
  },
  "created_at": "2026-05-15T14:23:01.012Z"
}
```

- **actor** — `system`, `opamp`, or `operator:<email>` if you front
  Squadron with auth. Today it's effectively always `system` since there's
  no auth layer.
- **event_type** — dotted name like `agent.drift.drifted`,
  `rollout.stage_applied`, `alert.fired`. Stable; UIs/automations can
  switch on this.
- **target_type** — `agent`, `group`, `config`, `rule`, `rollout`. Used
  to scope queries.
- **target_id** — the affected entity's ID. May be empty for fleet-wide
  events.
- **payload** — freeform JSON. Each event type documents what it puts
  here.

## What's recorded

| Event type                | Trigger                                  | Payload                                       |
|---------------------------|------------------------------------------|-----------------------------------------------|
| `agent.registered`        | New agent connects                       | name, labels                                  |
| `agent.drift.drifted`     | Agent transitions to drifted             | from (hash), to (hash)                        |
| `agent.drift.synced`      | Agent transitions to synced              | from (hash), to (hash)                        |
| `config.stored`           | Config saved                             | name, version, hash                           |
| `config.applied`          | Squadron pushed a config to an agent     | agent_id, config_id, hash                     |
| `rule.created`            | Alert rule created                       | name, query, threshold                        |
| `rule.updated`            | Alert rule edited                        | before, after                                 |
| `rule.deleted`            | Alert rule deleted                       | name                                          |
| `alert.fired`             | Alert evaluator fired a rule             | rule_id, rule_name, value, severity           |
| `alert.resolved`          | Firing alert returned below threshold    | rule_id, rule_name, value                     |
| `rollout.created`         | Rollout created                          | name, stage_count, diff_added_lines, diff_removed_lines, previous_config_id |
| `rollout.stage_applied`   | Engine pushed a stage to its canary set  | stage, mode, canary_size, agent_ids[], percentage or label_selector |
| `rollout.empty_canary`    | Stage resolved to 0 agents               | (informational)                               |
| `rollout.paused`          | Operator paused a rollout                | —                                             |
| `rollout.resumed`         | Operator resumed a rollout               | —                                             |
| `rollout.aborted`         | Auto-abort or manual abort               | reason                                        |
| `rollout.rolled_back`     | Rollback push completed                  | —                                             |
| `rollout.succeeded`       | Final stage cleared dwell                | —                                             |

The list grows as Squadron does. A new event type is one line of code at
the recording site plus a UI tweak in the timeline — adding more is
cheap and we use that liberally.

## Filtering

`GET /api/v1/audit/events` supports:

| Query param      | Notes                                                    |
|------------------|----------------------------------------------------------|
| `target_type`    | One of `agent`, `group`, `config`, `rule`, `rollout`.    |
| `target_id`      | Specific entity ID.                                      |
| `since`          | RFC3339 timestamp. Returns events with timestamp >= since. |
| `limit`          | Default 100, max 1000.                                   |

Most-recent-first ordering. Empty filter returns the most recent events
across the whole fleet.

```bash
# Last 50 events for a specific rollout
curl 'http://localhost:8080/api/v1/audit/events?target_type=rollout&target_id=<id>&limit=50'

# Everything that happened in the last hour
curl 'http://localhost:8080/api/v1/audit/events?since=2026-05-15T13:00:00Z'
```

## The UI timeline

Several pages in the UI mount an `AuditTimeline` filtered to whatever
entity you're looking at:

- The Audit page (unfiltered fleet-wide view) for browsing recent activity.
- The agent detail drawer (filtered to that agent) for "what's the
  history with this host?".
- The rollout card's **Show history** toggle (filtered to that rollout)
  for inline post-mortems — every stage application, every state
  transition, with the resolved agent IDs surfaced as chip clouds for
  the `stage_applied` and `empty_canary` events.

Live-updating via SSE: an `audit_event_recorded` event over the broker
revalidates whichever timelines are mounted, so you don't have to refresh.

## Retention

The audit log is **append-only and unbounded by default** — it is never
pruned unless you explicitly turn on retention. This is deliberate: the
audit log is your compliance/evidence record, and silently deleting it
would be the wrong default for anyone who relies on it after the fact.

Other Squadron tables (cost-spike events, discovery scan history, and so
on) prune on a fixed 90-day sweep. The audit log does **not**, because a
one-size retention window doesn't fit real regimes:

| Regime (typical, non-legal-advice) | Commonly cited retention |
|-----------------------------------|--------------------------|
| PCI-DSS                            | ~1 year                  |
| HIPAA                             | ~6 years                 |
| SOX                              | ~7 years                 |
| GDPR                             | keep only as long as needed; honor erasure obligations |

These are rough industry rules of thumb, not legal advice — confirm your
own obligations before choosing a window.

### Enabling retention

Retention only prunes when you turn it on **and** set a positive window.
Two ways, env takes precedence:

**Config file:**

```yaml
audit_retention:
  enabled: true
  retention_days: 365   # keep one year; older events are pruned daily
```

**Environment variable** (handy for env-only deployments):

```bash
SQUADRON_AUDIT_RETENTION_DAYS=365
```

When active, a daily sweep deletes audit events whose `timestamp` is older
than `now - retention_days`. Startup logs `audit-log retention GC started
(opt-in)` with the window so you can confirm it's on.

### Safety behavior

- **Default off.** Omit the block (or leave `enabled: false`) and the log
  grows unbounded — nothing is ever deleted.
- **Misconfiguration never wipes the log.** `enabled: true` with a
  non-positive `retention_days` (0 or negative) is treated as *inactive*,
  not "delete everything." You must set a real positive window for pruning
  to run.
- **Pruning is by event time** (`timestamp`), so "keep 365 days" means 365
  days of history regardless of when a row was written.

If your regime requires longer retention than a single Squadron instance
should hold, export the audit log to your SIEM/warehouse and enable a
shorter local window — the two are independent.

## API reference

| Method | Path                       | Purpose             |
|--------|----------------------------|---------------------|
| GET    | `/api/v1/audit/events`     | List with filters   |
