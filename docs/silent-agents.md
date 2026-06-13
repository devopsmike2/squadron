# Silent-Agent Alerts (v0.33)

Squadron's silent-agent watcher fires a webhook when one of your
collectors stops checking in via OpAMP. It's the "tell me when
something breaks" surface that closes the loop on the v0.31
pipeline-health and v0.32 inventory surfaces.

## How it works

A background goroutine polls the agents table on a fixed cadence
(default 60s). On each tick, it classifies every agent into two
buckets:

- **healthy** — `time.Since(last_seen) <= silence_threshold`
- **silent** — `time.Since(last_seen) > silence_threshold`

When an agent transitions between buckets (healthy → silent, or
silent → healthy), the watcher dispatches a webhook event. There
are no spurious "still silent" events — only edges.

To avoid a noisy burst at startup, the watcher does **not** fire
for agents that are already silent on its first poll. You'll get
events for transitions that happen during normal operation, not
for the install-time state of the world.

## Enabling

Disabled by default. Edit `squadron.yaml`:

```yaml
silent_agents:
  enabled: true
  silence_threshold: 10m      # default; how long quiet → silent
  poll_interval: 60s          # default; how often to check
  webhook_url: https://hooks.example.com/silent-agents
```

A restart of Squadron will start the watcher. Watch the logs:

```
INFO  silent-agent watcher started
   poll_interval=1m0s silence_threshold=10m0s webhook_configured=true
```

## Webhook payload

The watcher POSTs JSON to your webhook URL. The shape:

```json
{
  "kind": "silent_agent",
  "state": "firing",
  "agent_id": "9b2a-…",
  "hostname": "host01.example.com",
  "source": "gha-otel-deploy",
  "labels": { "env": "prod" },
  "last_seen": "2026-06-13T08:42:11Z",
  "silence_for": "11m12s",
  "at": "2026-06-13T08:53:23Z"
}
```

`state` is `firing` for healthy → silent and `resolved` for
silent → healthy. `source` and `labels` are filled in from the
v0.32 expected-agents table when the silent host matches an
expected entry.

`Content-Type: application/json`, `User-Agent: Squadron/silent-agents`.

The watcher waits 10 seconds for a response. Non-2xx responses are
logged but not retried — in v0.34 we'll add a retry queue with
exponential backoff.

## Pairing with the existing SquadronQL alerts

The alerting layer (v0.11+) handles operator-authored
SquadronQL queries with custom thresholds. The silent-agent watcher
is the complement: it doesn't need a query because the firing
condition is structural (the agent is gone).

Use SquadronQL alerts for "metric value crossed a threshold". Use
the silent-agent watcher for "I want a page when a collector dies".
Both write to webhooks, so the same receiver can handle both — just
key off the `kind` field (silent-agent events have `kind:
"silent_agent"`; SquadronQL alerts don't set `kind`).

## Receiver examples

**Slack incoming webhook** — wrap with a tiny relay:

```python
@app.post("/squadron/silent")
def relay(evt: dict):
    slack.post(
        text=f":warning: {evt['hostname']} silent for {evt['silence_for']}",
        channel="#fleet-ops",
    )
```

**PagerDuty Events API v2** — emit one event per transition:

```python
@app.post("/squadron/silent")
def relay(evt: dict):
    pd.events.trigger(
        routing_key=ROUTING_KEY,
        event_action="trigger" if evt["state"] == "firing" else "resolve",
        dedup_key=f"silent-{evt['agent_id']}",
        payload={
            "summary": f"{evt['hostname']} silent for {evt['silence_for']}",
            "source": evt.get("source", "unknown-pipeline"),
            "severity": "warning",
        },
    )
```

The `dedup_key` lines up with the `agent_id` so the `firing` event
opens an incident and the matching `resolved` event closes it.

## Tuning the threshold

Default 10 minutes is the right answer for most installs — it
absorbs a single missed heartbeat without false-positives. Bump it
higher (30m, 1h) for hosts that legitimately go quiet for long
intervals (cron-driven batch collectors that fire once an hour).
Drop it lower (2-3 minutes) for collectors with aggressive
`heartbeat_interval` settings on the OpAMP client side.

## Roadmap

v0.34 adds:

- A retry queue with exponential backoff for webhook delivery
  failures.
- Per-source webhook routing — different CI pipelines can hit
  different webhook URLs.
- A dedup window so a flapping agent doesn't generate a webhook
  storm.

See also `docs/pipeline-health.md` and `docs/inventory.md` for the
two surfaces this watcher complements.
