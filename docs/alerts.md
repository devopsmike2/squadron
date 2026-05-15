# Alerts

Squadron's alert system evaluates threshold rules against Squadron QL
queries on a periodic interval and dispatches a webhook when a rule
fires.

- [Alert rules](#alert-rules)
- [The evaluator](#the-evaluator)
- [Webhook payloads](#webhook-payloads)
- [Example rules](#example-rules)
- [API reference](#api-reference)

## Alert rules

A rule has:

| Field                | Type           | Notes                                         |
|----------------------|----------------|-----------------------------------------------|
| `name`               | string         | Human-readable label.                         |
| `description`        | string         | Optional context (shown in payloads).         |
| `query`              | string         | Squadron QL that must return a scalar.        |
| `threshold_operator` | enum           | `>`, `>=`, `<`, `<=`, `==`, `!=`.             |
| `threshold_value`    | float64        | The number the query result is compared to.   |
| `interval_seconds`   | int            | Evaluation cadence. Floor is 10s.             |
| `severity`           | enum           | `info`, `warning`, `critical`.                |
| `enabled`            | bool           | Disabled rules are skipped by the evaluator.  |
| `webhook_url`        | string         | Where the firing payload is POSTed. Optional. |

Rules are stored in the application store. The UI's Alerts page lists
them with edit, enable/disable, and delete affordances.

## The evaluator

A background goroutine ticks every 5 seconds. On each tick it inspects
every enabled rule and, for rules whose `interval_seconds` has elapsed
since their last evaluation:

1. Runs the rule's `query` against Squadron's telemetry store.
2. Compares the scalar result to `threshold_value` using
   `threshold_operator`.
3. If the comparison is true, the rule is **firing**:
   - The first firing dispatches the webhook (if `webhook_url` is set).
   - Subsequent firings while the rule stays in the firing state are
     suppressed — no duplicate webhooks per evaluation.
4. If the comparison is false and the rule was previously firing, the
   rule **resolves**: an `alert.resolved` audit event is recorded and a
   resolve payload is dispatched to the webhook.

State is in-memory. A Squadron restart loses the "currently firing"
record, so a rule that's been firing across the restart will re-fire its
first post-restart evaluation. The audit log preserves history regardless.

## Webhook payloads

The fired payload:

```json
{
  "rule_id": "...",
  "rule_name": "high drift rate",
  "severity": "warning",
  "query": "fleet_drift_status_drifted",
  "operator": ">",
  "threshold": 5,
  "value": 7,
  "fired_at": "2026-05-15T14:23:01.001Z"
}
```

The resolved payload has the same shape with `resolved_at` instead of
`fired_at` and the `value` that caused it to resolve.

Webhook calls are best-effort: a 5xx, timeout, or DNS failure is logged
but does not retry. The audit log captures the durable record under
`event_type=alert.fired` / `event_type=alert.resolved`.

## Example rules

### Drift threshold

```bash
curl -X POST http://localhost:8080/api/v1/alerts/rules \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "high drift rate",
    "query": "fleet_drift_status_drifted",
    "threshold_operator": ">",
    "threshold_value": 5,
    "interval_seconds": 60,
    "severity": "warning",
    "enabled": true,
    "webhook_url": "https://hooks.example.com/squadron-alerts"
  }'
```

Fires when more than 5 agents are in the drifted state. Useful as a fleet
health alarm — drift count climbing usually means a rollout went wrong or
a deployment pipeline is fighting Squadron.

### Offline agents

```json
{
  "name": "agents offline",
  "query": "agent_status_offline",
  "threshold_operator": ">",
  "threshold_value": 0,
  "interval_seconds": 30,
  "severity": "critical",
  "enabled": true
}
```

### Error log spike

```json
{
  "name": "fleet error log rate",
  "query": "log_records_per_minute{severity=\"error\"}",
  "threshold_operator": ">",
  "threshold_value": 100,
  "interval_seconds": 60,
  "severity": "critical",
  "enabled": true
}
```

Note: rollouts have their own per-canary error-rate abort criterion
(see [Rollouts → abort criteria](./rollouts.md#dwell-and-abort-criteria)).
Fleet-wide alerts complement rollout criteria rather than replace them.

## API reference

| Method | Path                              | Purpose                          |
|--------|-----------------------------------|----------------------------------|
| GET    | `/api/v1/alerts/rules`            | List                             |
| POST   | `/api/v1/alerts/rules`            | Create                           |
| GET    | `/api/v1/alerts/rules/:id`        | Get one                          |
| PUT    | `/api/v1/alerts/rules/:id`        | Update                           |
| DELETE | `/api/v1/alerts/rules/:id`        | Delete                           |
