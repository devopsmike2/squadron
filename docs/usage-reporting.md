# Anonymous usage reporting (opt-in)

Squadron can send a small, **anonymized, aggregate** usage report on an interval
so the project can understand roughly how Squadron is deployed. It is **off by
default** and Squadron sends nothing unless you both enable it *and* configure an
endpoint. It is entirely separate from the `telemetry:` block (which exports your
instance's own operational metrics to *your own* OTLP backend, not to us).

## What is sent

Only low-cardinality **counts and labels** — you can see the exact bytes in
`internal/usage` (`Snapshot` + `BuildPayload`) and in the `usage.v1` schema:

| field | example | meaning |
|---|---|---|
| `squadron_version` | `0.1.0` | the running build's version |
| `edition` | `squadron-oss` | OSS vs enterprise build |
| `agents` | `42` | number of registered agents (a count) |
| `rollouts` | `3` | number of rollouts (a count) |
| `reported_at` | `2026-07-06T12:00:00Z` | when the report was taken |
| `schema` | `usage.v1` | payload schema version |

## What is NOT sent

No tenant, host, account, or agent identifiers. No IP addresses. No config
content, no resource names/ARNs, no telemetry data, no cloud credentials. The
payload is a flat map of the fields above — a test (`TestBuildPayload_ShapeAndAnonymity`)
fails the build if any other key is ever added.

## Enabling it

YAML (`squadron.yaml`):

```yaml
usage_reporting:
  enabled: true
  endpoint: https://usage.example.com/report   # required; empty ⇒ disabled
  interval_hours: 24                            # optional; defaults to 24h
```

or environment (wins over YAML):

```
SQUADRON_USAGE_ENABLED=true
SQUADRON_USAGE_ENDPOINT=https://usage.example.com/report
```

## Behavior

Best-effort and non-blocking: the first report is sent ~30s after start (so a
crash-looping instance doesn't hammer the endpoint), then every `interval_hours`.
Each POST has a hard 10s timeout; any collection or delivery error is logged at
debug and dropped — usage reporting can never slow or crash the server.
