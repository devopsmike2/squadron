# squadronctl

`squadronctl` is the Squadron command-line client. It wraps the REST
API so you can drive Squadron from CI pipelines, shell scripts, and
ad-hoc terminals without opening the UI.

- [Install](#install)
- [Configure](#configure)
- [Common commands](#common-commands)
- [CI integration](#ci-integration)
- [Exit codes](#exit-codes)
- [Output formats](#output-formats)

## Install

Pre-built binaries for macOS, Linux, and Windows are attached to every
GitHub release under [Releases](https://github.com/devopsmike2/squadron/releases).
Pick the one for your platform:

```bash
# macOS arm64 (Apple silicon)
curl -L -o squadronctl \
  https://github.com/devopsmike2/squadron/releases/latest/download/squadronctl-darwin-arm64
chmod +x squadronctl
sudo mv squadronctl /usr/local/bin/

# Linux amd64
curl -L -o squadronctl \
  https://github.com/devopsmike2/squadron/releases/latest/download/squadronctl-linux-amd64
chmod +x squadronctl
sudo mv squadronctl /usr/local/bin/
```

To build from source:

```bash
git clone https://github.com/devopsmike2/squadron.git
cd squadron
make build-cli              # binary lands at bin/squadronctl
```

Verify:

```bash
squadronctl version
```

## Configure

`squadronctl` reads its server URL and API token from, in order of
precedence (highest wins):

1. Command-line flags: `--server`, `--token`
2. Environment variables: `SQUADRON_URL`, `SQUADRON_TOKEN`
3. Config file at `~/.squadronctl/config.yaml`

The config file is optional:

```yaml
# ~/.squadronctl/config.yaml
server: https://squadron.example.com
token: sqd_yourtokenhere
```

For local development:

```bash
export SQUADRON_URL=http://localhost:8080
export SQUADRON_TOKEN=sqd_yourtokenhere
squadronctl auth whoami
```

If the Squadron server has `auth.enabled: false`, the token can be
omitted — the server accepts unauthenticated requests. Production
deployments should always have auth on; see [Authentication](./auth.md).

## Common commands

### Verify connectivity

```bash
squadronctl auth whoami
# Server: http://localhost:8080
# Token:  sqd_abcd… (sha-prefix only shown)
# Status: authenticated (3 active tokens visible)
```

### List agents

```bash
squadronctl agents list
squadronctl agents list --status online
squadronctl agents list --group prod-collectors --drifted
squadronctl agents get <agent-id>
```

### Upload a config and roll it out

The two-step CI flow: `apply` uploads the YAML, then `rollouts create`
ships it.

```bash
# Lint locally first (catches errors before the network round-trip).
squadronctl configs lint --file ./otel-collector.yaml

# Upload as a new versioned config bound to a group.
CONFIG=$(squadronctl configs apply \
  --file ./otel-collector.yaml \
  --name "collector v2" \
  --group prod-collectors \
  -o json | jq -r .id)

# Preview the diff before committing.
squadronctl rollouts preview --group prod-collectors --target-config $CONFIG

# Ship it using a server-curated template.
squadronctl rollouts create \
  --group prod-collectors \
  --target-config $CONFIG \
  --template standard-percent-ramp \
  --wait
```

`--wait` blocks until the rollout reaches a terminal state. Exit code
0 = succeeded, 2 = rolled back, 3 = wait timeout. See
[Exit codes](#exit-codes).

### List rollouts

```bash
squadronctl rollouts list
squadronctl rollouts list --state in_progress
squadronctl rollouts get <rollout-id>
squadronctl rollouts templates
```

### Custom stages

If a template doesn't fit, specify stages directly. Each `percent:dwell`
pair becomes one stage:

```bash
squadronctl rollouts create \
  --group prod-collectors \
  --target-config $CONFIG \
  --stages 5:300,25:300,100:120 \
  --max-drifted-agents 0 \
  --max-error-logs-per-minute 5 \
  --warmup-seconds 60
```

For label-mode rollouts, drive the API directly with curl — the CLI's
`--stages` flag is percent-mode only.

### Manage tokens

```bash
squadronctl auth tokens
squadronctl auth create-token --label "deploy-pipeline"
squadronctl auth revoke-token <token-id>
```

The plaintext from `create-token` is printed ONCE. Squadron does not
keep a recoverable copy.

### Audit log

```bash
squadronctl audit list --limit 20
squadronctl audit list --target-type rollout --target-id <rollout-id>
squadronctl audit list -o json | jq '.[] | select(.event_type == "rollout.aborted")'
```

## CI integration

Minimum GitHub Actions snippet for a config-push workflow:

```yaml
name: Push collector config

on:
  push:
    branches: [main]
    paths: ['collector-config/**']

jobs:
  rollout:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5

      - name: Install squadronctl
        run: |
          curl -L -o squadronctl \
            https://github.com/devopsmike2/squadron/releases/latest/download/squadronctl-linux-amd64
          chmod +x squadronctl
          sudo mv squadronctl /usr/local/bin/

      - name: Lint config
        run: squadronctl configs lint --file collector-config/prod.yaml
        env:
          SQUADRON_URL: ${{ vars.SQUADRON_URL }}
          SQUADRON_TOKEN: ${{ secrets.SQUADRON_TOKEN }}

      - name: Upload and roll out
        env:
          SQUADRON_URL: ${{ vars.SQUADRON_URL }}
          SQUADRON_TOKEN: ${{ secrets.SQUADRON_TOKEN }}
        run: |
          CONFIG=$(squadronctl configs apply \
            --file collector-config/prod.yaml \
            --name "collector ${{ github.sha }}" \
            --group prod-collectors \
            -o json | jq -r .id)

          squadronctl rollouts create \
            --name "deploy ${{ github.sha }}" \
            --group prod-collectors \
            --target-config $CONFIG \
            --template standard-percent-ramp \
            --notify ${{ secrets.SLACK_WEBHOOK }} \
            --wait \
            --wait-timeout 30m
```

The job fails on exit 2 (rolled back) or exit 3 (timeout), which
GitHub renders as a failed workflow — wire your usual notification on
that.

## Exit codes

| Code | Meaning                                                  |
|------|----------------------------------------------------------|
| 0    | Success.                                                 |
| 1    | Generic error (network, validation, bad flags).          |
| 2    | `rollouts wait` / `rollouts create --wait`: the rollout reached `rolled_back`. |
| 3    | `rollouts wait` / `rollouts create --wait`: timeout elapsed before the rollout reached a terminal state. |

Match on the specific codes in CI rather than checking for any non-zero,
so timeouts can be retried while real failures escalate.

## Output formats

Every command supports `-o json`. Without the flag, output is
human-friendly tables and prose.

```bash
squadronctl rollouts list                # tabular, one row per rollout
squadronctl rollouts list -o json | jq   # pipe through jq for filtering
squadronctl audit list -o json \
  | jq '.[] | select(.actor != "system")'  # find operator-attributed events
```

The `human` output format is intentionally not stable — it's optimized
for terminal readability and may change between releases. Pipelines
should use `-o json`.

## Tracing

Since v0.18, `squadronctl` participates in W3C TraceContext
propagation, so an operation that starts in CI and ends inside an
agent ends up in a single trace. There are two ways to turn it on,
and they compose.

### Wrap with a tracing CI runner (TRACEPARENT inheritance)

If your CI runner (`otel-cli`, GitHub Actions OpenTelemetry, GitLab's
OTel integration, etc.) already injects a `TRACEPARENT` env var into
spawned child processes, `squadronctl` honors it automatically.
Every API call carries the inherited traceparent forward, so the
server-side request span — and everything that runs under it on the
Squadron side — becomes a child of the CI span:

```bash
otel-cli exec --service-name ci-deploy --name "deploy v2.3" -- \
  squadronctl rollouts create \
    --group prod-collectors \
    --target-config $CONFIG \
    --template standard-percent-ramp \
    --wait
```

No env vars on `squadronctl` itself are required for this path —
`TRACEPARENT` is the only thing it reads from the environment.

### Native OTLP export from squadronctl

If you want `squadronctl` to emit its own span (covering the entire
CLI invocation including command-side overhead, not just the HTTP
calls) as a real OTLP record, set the standard OTel env vars:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc           # or http
export OTEL_EXPORTER_OTLP_INSECURE=true           # plain-tcp local
export OTEL_SERVICE_NAME=squadronctl              # optional; default squadronctl
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=...     # optional; override for traces

squadronctl rollouts create ...
```

The span hierarchy looks like:

- `squadronctl rollouts create` (Client, root or child of TRACEPARENT
  if set)
  - `POST /api/v1/rollouts` (auto-instrumented by otelhttp)
    - server-side spans rooted in Squadron (rollout engine, etc.)

This composes with the TRACEPARENT path: if both are set, the
`squadronctl` span nests under the inherited CI span AND exports
its own record. Operators querying their trace tool see the full
chain.

### Trace flush on exit

`squadronctl` ends its root span and shuts down the OTLP exporter
inline before the process exits (3-second timeout). The last span
in the invocation always lands, even on the error path. If the
OTLP endpoint is unreachable, the export silently fails — the CLI
never blocks user feedback on telemetry.

See [self-monitoring.md](./self-monitoring.md) for the server-side
of the picture (the Squadron API's gin middleware extracts the
traceparent and roots its internal spans there).
