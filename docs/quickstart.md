# Quickstart

The Quickstart wizard is Squadron's answer to "I just installed
this, what do I do?" It gets your first agent into the fleet view
in minutes, whether you're starting from scratch or adopting
collectors you already have running.

It lives at `/quickstart` in the UI and also surfaces as a
dismissable banner on the Dashboard whenever the fleet has zero
agents.

## Two paths

### Path A — "Start fresh"

You don't have an OpenTelemetry Collector running yet. Squadron
asks which backend you're sending telemetry to
(Datadog / Honeycomb / New Relic / SigNoz / Grafana Cloud /
Generic OTLP) and generates:

1. A complete, ready-to-use collector config wired to that
   backend AND to this Squadron's OpAMP server.
2. A required-environment-variables checklist (the config
   references env vars by name; you set them on the collector
   host — Squadron never sees your API keys).
3. A per-platform install command:
   - **Docker** — `docker run …` with the config mounted
   - **Bare metal / systemd** — `curl` to fetch the binary +
     `systemctl` to launch
   - **Kubernetes (Helm)** — `helm upgrade --install` with the
     generated config in `values.yaml`

When you run the agent, the wizard's final step polls every 3
seconds and lights up the moment a new agent connects.

### Path B — "I have collectors already running"

You already have OpenTelemetry Collectors deployed and shipping
telemetry. Squadron generates just the **OpAMP extension snippet**
to paste into each existing config — no re-deploy, no swap of the
exporter, no disruption to current pipelines.

The wizard also includes a **bulk mode**: paste a list of
hostnames and Squadron generates one ssh-ready one-liner per host
that appends the snippet and restarts the collector. Run them
from a host with ssh reach to your fleet.

Restart commands are provided for every common deployment shape:
systemd, Docker, Helm, bare process.

## What you'll need

For Path A: an account with one of the supported backends
(Datadog, Honeycomb, New Relic, SigNoz Cloud, Grafana Cloud, or
any OTLP-compatible backend) and the API key / ingest token. The
wizard's "required environment variables" checklist tells you
exactly which env vars to export before starting the collector.

For Path B: the path to your existing collector config(s) and the
ability to restart the collector(s). The opamp snippet is two top-
level YAML keys (`extensions:` and `service:`) — you merge them
into your existing config without replacing anything.

## How it works under the hood

Squadron's fleet view is powered by the OpenTelemetry **OpAMP**
(Open Agent Management Protocol). Agents connect to Squadron's
OpAMP server (default port 4320) over a WebSocket and announce
themselves; Squadron tracks them, pushes config changes, and reads
their effective configs back.

The wizard's job is to give you the OpAMP extension snippet
pointed at the right place. Once a collector has the snippet and
restarts, it shows up in the Fleet view within seconds.

The OpAMP URL the wizard generates is built from the request's
Host header by default, so a Squadron running on `localhost:8080`
hands out `ws://localhost:4320/v1/opamp` to its own UI. For
remote agents, pass `?host=squadron.example.com` to the
quickstart endpoints (the UI exposes a host override for remote agents).

## API endpoints

| Endpoint | What it returns |
|---|---|
| `GET /api/v1/quickstart/backends` | The backend catalog the UI's picker renders |
| `GET /api/v1/quickstart/starter-config?backend=...` | Complete starter collector config for the chosen backend |
| `GET /api/v1/quickstart/opamp-snippet` | The bare OpAMP extension YAML for pasting into existing configs |
| `GET /api/v1/quickstart/adoption-snippet` | The OpAMP snippet plus `host.name` and identifying labels, so each adopted host lands as its own fleet card (avoids the one-host-many-cards collision) |

All read-only, all behind `ScopeAgentsRead`. The host
substitution honors a `?host=` query param when present.

## Supported backends

| Backend | Exporter type | Required env vars |
|---|---|---|
| **Datadog** | datadog (native) | `DD_API_KEY` |
| **Honeycomb** | otlp/honeycomb (gRPC) | `HONEYCOMB_API_KEY` |
| **New Relic** | otlphttp/newrelic | `NEW_RELIC_LICENSE_KEY` |
| **SigNoz Cloud** | otlp/signoz (gRPC) | `SIGNOZ_INGESTION_KEY` |
| **Grafana Cloud** | otlphttp/grafana + basicauth | `GRAFANA_INSTANCE_ID`, `GRAFANA_OTLP_TOKEN`, `GRAFANA_OTLP_ENDPOINT` |
| **Generic OTLP** | otlp (gRPC) | `OTLP_ENDPOINT` (auth headers commented in template) |

All templates ship with the OTLP receivers (`grpc:4317` and
`http:4318`), a `batch` processor (the one processor every config
should have), and full traces/metrics/logs pipelines wired to the
backend's exporter. Production tuning (memory_limiter, sampling,
resource detection) is left for you to add — the starter is a
minimum-viable starting point, not a production-ready template.

## Deliberately out of scope

By design, the wizard generates commands and configs — it never
reaches out to your hosts or handles your secrets:

- **Active network discovery** — probing a CIDR for open OTel
  ports. Full of security and false-positive landmines; no plans.
- **Backend health-check** — verifying that the API key you set
  actually works before the wizard moves on. The wizard relies on
  the collector itself logging failures; Squadron never sees your
  keys and won't.
- **Automated `docker run` / `helm upgrade`** — Squadron generates
  the commands; you run them. It won't ssh out to hosts on the
  operator's behalf.

## See also

- `docs/operating.md` — broader operational guidance for Squadron
  (OpAMP configuration, agent health, troubleshooting)
- `docs/savings.md` — the Savings dashboard the wizard
  drops you into after your first agent connects
