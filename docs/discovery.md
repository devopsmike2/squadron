# Agent discovery (v0.36+)

Squadron has two discovery paths. Knowing which path an agent
arrived through tells you what you can do with it.

## OpAMP discovery (default, "managed")

The collector's OpAMP supervisor opens a WebSocket connection to
Squadron's OpAMP port (default `:4320`). Squadron sees the
connection, records the agent with `discovery_source: "opamp"`,
and now has bidirectional control: it can push configs, request
restarts, observe drift, and run staged rollouts.

This is the standard path and is what every Squadron-deployed
collector uses out of the box.

## Passive OTLP discovery (v0.36+, "telemetry-only")

Some collectors send OTLP data to Squadron's OTLP receivers
(`:4317` / `:4318`) but never opened an OpAMP connection. These
are typically collectors that were deployed before Squadron
existed in the environment, or deployed with hardcoded configs
that don't include an OpAMP supervisor block.

Squadron's worker pool extracts the `service.instance.id` resource
attribute from every incoming OTLP batch. When it sees an ID that
has no corresponding agent record, it creates one with
`discovery_source: "otlp"` and `status: "online"`. The agent shows
up in the agents list with a **Telemetry-only** badge.

### What you can do with telemetry-only agents

- ✓ See them in the agents list
- ✓ Query their telemetry via SquadronQL
- ✓ See them in cost insights and pipeline health
- ✓ Include them in v0.32 inventory reconciliation (they count as
  "actual" hosts)
- ✗ Push config to them (no OpAMP socket)
- ✗ Restart them (no control channel)
- ✗ Include them in staged rollouts

### Bringing a telemetry-only agent under management

The collector needs to be reconfigured to include an OpAMP
supervisor block pointing at Squadron, then restarted. This is
necessarily out-of-band — Squadron can't push a new config to a
collector it doesn't have a control channel for.

Typical flows:

1. **Ansible push** — your existing deploy pipeline (e.g.
   `win_deploy.yml`) rewrites the collector config to include the
   OpAMP block and restarts the service. On next start the
   collector opens OpAMP, gets matched to the existing
   telemetry-only agent record by its `service.instance.id`, and
   gets promoted to fully-managed.

2. **Manual conversion** — operator adds the OpAMP supervisor
   block to the collector config on the host, restarts the
   binary. Same match-by-instance-id de-dupe applies.

The OpAMP supervisor block you'd add looks like:

```yaml
extensions:
  opamp:
    server:
      ws:
        endpoint: ws://squadron.example.com:4320/v1/opamp
    instance_uid: ${env:OTEL_RESOURCE_ATTRIBUTES_INSTANCE_ID}

service:
  extensions: [opamp]
```

The `instance_uid` line is critical for the de-dupe — it ensures
the OpAMP-connected agent reports the same UUID as the
telemetry-only record, so Squadron updates rather than creates a
new agent.

## Tuning

The discovery hot path has an in-process LRU cache that
throttles re-upserts: once Squadron has seen an `agent_id`
recently it short-circuits before hitting the store. The default
window is 5 minutes, which is plenty for typical OTel collector
reporting intervals (10–60s).

There's no config knob today — if you need to tune the window,
open an issue and we'll add one in v0.36.2.

## Disabling discovery

If you want Squadron to *only* manage agents that opened an OpAMP
connection (e.g. for strict-inventory environments), comment out
the `workerPool.SetDiscovery(...)` line in `cmd/all-in-one/main.go`
and rebuild. The discovery hook is wired through `SetDiscovery`
specifically so it's easy to skip without surgery on the worker
hot path.

## Roadmap

- **v0.36.1** — GitHub Actions history walker. Squadron walks
  past successful deploy runs, fetches `inventory.ini` at each
  commit SHA, and registers hosts as expected so missing
  agents surface automatically.
- **v0.36.2** — Active host probing. For expected hosts that
  haven't checked in, Squadron tries scraping
  `http://<host>:8888/metrics` to detect collectors running
  without OpAMP. (Requires network reachability from Squadron to
  the hosts.)
- **v0.36.3** — One-click "convert to managed" affordance that
  fires the deploy pipeline with an OpAMP-enabled config for a
  specific telemetry-only host.
