# Testing Squadron locally (v0.37+)

A guide for exercising every major Squadron feature on your laptop
without touching a real fleet or OpenShift cluster. The setup
takes about 5 minutes from scratch and lets you validate
everything before cloning the repo into your company environment.

## What you get

```
┌──────────────────────────────────────────────────────────────┐
│  Your Mac                                                    │
│                                                              │
│   Squadron (local binary)                                    │
│   ├── API on :8090                                           │
│   ├── OpAMP on :4330                                         │
│   └── OTLP on :4327 / :4328                                  │
│           ▲                                                  │
│           │ host.docker.internal                             │
│           │                                                  │
│   ┌───────┴────────────────────────────────────────────┐     │
│   │  Docker compose (deploy/test/)                     │     │
│   │   • collector-prod      (OpAMP-managed)            │     │
│   │   • collector-staging   (OpAMP-managed)            │     │
│   │   • collector-otlp-only (telemetry-only — v0.36.0) │     │
│   │   • webhook-echo        (receives test webhooks)   │     │
│   └────────────────────────────────────────────────────┘     │
│                                                              │
│   fleetsim (CLI)  →  50 synthetic OpAMP agents on demand     │
└──────────────────────────────────────────────────────────────┘
```

## Prerequisites

- Docker Desktop (or compatible)
- Go 1.23+
- A free port at `9001` (the webhook receiver) — every other port
  matches what your local Squadron is already using

## One-time setup

From the repo root:

```bash
make test-env-up
```

This builds the binary, generates a `SQUADRON_DEPLOY_KEY` for the
session, and starts the docker fleet. Squadron itself is NOT
containerized — you'll keep running it via your existing
`./bin/squadron --config /tmp/squadron-local.yaml` invocation so
you can hot-reload code changes without rebuilding images.

After about 30 seconds:

```bash
open http://localhost:8090/agents
```

You should see three agents:

- **test-prod-01** — green status, OpAMP-managed
- **test-staging-01** — green status, OpAMP-managed
- **test-rogue-01** — yellow "Telemetry-only" badge (v0.36.0
  discovery picked it up via OTLP, no OpAMP)

Open `/fleet-map` and you'll see the pipeline graph for the
OpAMP collectors. Open any agent → the Pipeline Health panel
will show real otelcol_* self-metrics flowing.

## Testing each feature

### v0.31 — Pipeline health

Open `test-prod-01` in the agents drawer. The Pipeline Health
panel should show **healthy** with non-zero queue and throughput
values pulled from the collector's own `otelcol_*` metrics.

To simulate degradation: edit `deploy/test/collectors/opamp-prod.yaml`
and add a broken exporter (e.g. set `endpoint: host.docker.internal:9999`
with no listener). Restart with `docker compose -f deploy/test/docker-compose.yml restart collector-prod`.
Squadron will start reporting send_failed > 0 and the verdict will
flip to **degraded** within 30 seconds.

### v0.32 — Inventory reconciliation

```bash
curl -X PUT http://localhost:8090/api/v1/inventory/expected \
  -H "Content-Type: application/json" \
  -d '{
    "source": "manual-test",
    "entries": [
      {"hostname": "test-prod-01"},
      {"hostname": "test-staging-01"},
      {"hostname": "ghost-host-that-does-not-exist"}
    ]
  }'
```

Open `/inventory`. You'll see the two real hosts as **healthy**
and `ghost-host-that-does-not-exist` as **missing**.

### v0.33 — Silent-agent webhooks

Edit `squadron.yaml` and add:

```yaml
silent_agents:
  enabled: true
  silence_threshold: 30s         # tight for testing
  webhook_url: http://localhost:9001/silent
```

Restart Squadron. Then stop one of the collectors:

```bash
docker stop squadron-test-collector-prod
```

Within ~60 seconds you'll see a webhook hit the echo server. Watch:

```bash
docker logs -f squadron-test-webhook-echo
```

Restart the collector — you'll get the matching `resolved` webhook
about 30s later.

### v0.34 / v0.35 — Deploy integration

This needs a real GitHub repo because the integration actually
calls the GitHub Actions API. Cheapest setup:

1. Create a personal repo, e.g. `mihea-otel-test`.
2. Add `.github/workflows/test-deploy.yml`:

   ```yaml
   name: Test deploy
   on:
     workflow_dispatch:
       inputs:
         filelog:
           description: yes or no
           required: true
           default: "no"
   jobs:
     simulate:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v4
         - run: |
             echo "Simulated deploy with filelog=${{ inputs.filelog }}"
             cat winOtel/ansible/inventory.ini
             sleep 5
   ```

3. Add a sample inventory file `winOtel/ansible/inventory.ini`:

   ```
   [windows]
   test-prod-01
   test-staging-01
   ```

4. Mint a fine-grained PAT scoped to just this repo with
   `actions:write` + `contents:read`.

5. In Squadron UI → Deploy → New target → fill in your username +
   `mihea-otel-test` + `test-deploy.yml` + `main` + paste the PAT
   + set inventory path `winOtel/ansible/inventory.ini`.

6. Click **Validate** — you should see all four checks pass.

7. Click **Run deployment** — the trigger sheet will show the
   live host status of `test-prod-01` and `test-staging-01`
   (green dots — they're checking in), inventory parsed at
   trigger time, runs through the lint gate, fires
   `workflow_dispatch`, attaches the run ID, polls for status.

8. After it succeeds, configure the deploy completion webhook
   in `squadron.yaml`:

   ```yaml
   deploy:
     completion_webhook_url: http://localhost:9001/deploy
   ```

   Trigger another deploy and watch the echo server for the
   payload.

### v0.36.0 — Passive OTLP discovery

Already exercised — `test-rogue-01` shows up as telemetry-only
on initial bringup. To validate explicitly: stop it (`docker stop
squadron-test-collector-otlp-only`), wait a minute, see its
last_seen freeze. Restart and watch the timestamp tick forward.

### v0.36.1 — GHA history walker

After triggering 2-3 deploys against your test repo, the walker
runs every 6 hours. To run it on-demand for testing, restart
Squadron — the walker fires immediately on startup.

Open `/inventory` and you'll see entries with source
`gha-history:<target-id>` and notes referencing the actual run
numbers + commit SHAs from your test repo.

## Scale testing

To validate that the UI scales:

```bash
make test-env-fleetsim
```

This adds 50 synthetic OpAMP agents that look like real
collectors. Combined with your 3 real ones, you'll have 53 in
the fleet. The agents list is virtualized so scrolling stays
smooth — if it doesn't, that's a real bug to file.

## Tearing down

```bash
make test-env-down       # stops the fleet, keeps Squadron's data
make test-env-reset      # full reset (also wipes Squadron's data)
```

Squadron itself stays running across `test-env-down/up` cycles —
kill it manually if you need to (`pkill -f bin/squadron`).

## Troubleshooting

**"Squadron not reachable from containers"** — make sure Squadron
is bound to all interfaces (not just 127.0.0.1). The local-run
config uses `0.0.0.0:` prefixes for OTLP endpoints, which is
correct. If you've customized: ensure the OpAMP server listens
on `:4330` (not `127.0.0.1:4330`).

**"No agents showing up"** — check `docker logs squadron-test-collector-prod`.
Most common issue: the OpAMP supervisor can't reach the server
because Squadron isn't running. Start Squadron first, then run
`docker compose -f deploy/test/docker-compose.yml restart`.

**"test-rogue-01 has the wrong badge"** — v0.36.0 needs a couple
of seconds after first OTLP batch to materialize the agent. If
you see no badge at all, hard-refresh the Agents page.

## What this doesn't test

- Multi-cluster scenarios (OpenShift, cross-AZ, etc.)
- Real network conditions (latency, retries, partial outages)
- TLS / mTLS to GitHub Enterprise (set
  `GitHubBaseURL` in `NewGitHubProvider("https://github.your-co.com/api/v3")`)
- Your actual collector configs and inventory shape

For those, you'll need to clone Squadron into your company
environment and deploy to a non-prod project on OpenShift. See
`docs/openshift.md` for that walkthrough.
