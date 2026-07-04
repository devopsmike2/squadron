# Quickstart: Squadron OSS on a single VM

Get Squadron running on one Linux VM in about 15 minutes, then connect your
first collector. This is the recommended path for evaluation and small,
self-hosted fleets (< 500 collectors).

Squadron OSS is **single-instance** with an embedded store (SQLite for state,
DuckDB for telemetry rollups) — no external database, no message bus. Postgres,
HA, and multi-replica clustering are commercial-tier concerns, so you won't need
them here.

## What you'll end up with

- The Squadron control plane running as a systemd service.
- TLS terminated by a reverse proxy in front of it.
- One OpenTelemetry collector phoning home and visible as **online** in the UI.

## Before you start

- A Linux VM (1 vCPU / 1 GB RAM handles hundreds of collectors).
- Network reachability from your collectors to the VM on the OpAMP port (4320).
- Optional: an `ANTHROPIC_API_KEY` if you want the AI features (proposer, Ask
  Squadron). Squadron works as a control plane without it.

Squadron listens on four ports you'll reuse throughout: **8080** (UI/API),
**4320** (OpAMP WebSocket), **4317/4318** (OTLP gRPC/HTTP).

## 1. Install the binary

```bash
curl -L https://github.com/devopsmike2/squadron/releases/latest/download/squadron-linux-amd64.tar.gz \
  | tar -xz -C /usr/local/bin/

squadron version
```

Prefer to build from source? You'll need Go 1.24+, a C compiler, and SQLite dev
libraries:

```bash
git clone https://github.com/devopsmike2/squadron.git
cd squadron
make build-all-in-one
```

## 2. Write the config

Create `/etc/squadron/squadron.yaml`:

```yaml
server:
  http_addr: ":8080"
  opamp_addr: ":4320"
  otlp_grpc_addr: ":4317"
  otlp_http_addr: ":4318"
  public_base_url: "https://squadron.example.com"

storage:
  sqlite_path: /var/lib/squadron/squadron.db

# Turn on bearer-token auth. The first start mints a bootstrap token and
# prints it to the log — rotate it from the UI afterward.
auth:
  require_token: true

# Optional: enable AI assist + Ask Squadron.
ai:
  enabled: true
  api_key_env: ANTHROPIC_API_KEY
```

## 3. Run it as a systemd service

Create `/etc/systemd/system/squadron.service`:

```ini
[Unit]
Description=Squadron control plane
After=network.target

[Service]
Type=simple
User=squadron
Group=squadron
WorkingDirectory=/var/lib/squadron
ExecStart=/usr/local/bin/squadron --config /etc/squadron/squadron.yaml
Restart=on-failure
RestartSec=5s
Environment=ANTHROPIC_API_KEY=your-key-here

[Install]
WantedBy=multi-user.target
```

Create the user and data directory, then start it:

```bash
useradd --system --home /var/lib/squadron squadron
mkdir -p /var/lib/squadron /etc/squadron
chown -R squadron:squadron /var/lib/squadron
systemctl daemon-reload
systemctl enable --now squadron
```

Grab the bootstrap token from the log for later:

```bash
journalctl -u squadron | grep -i "bootstrap token"
```

## 4. Terminate TLS in front

Squadron speaks plain HTTP; put a reverse proxy in front for TLS. A minimal
Caddyfile:

```caddyfile
squadron.example.com {
  reverse_proxy localhost:8080
}
squadron-opamp.example.com {
  reverse_proxy localhost:4320
}
```

> OpAMP is a long-lived WebSocket. If you use nginx instead of Caddy, set
> `proxy_read_timeout` and `proxy_send_timeout` into the hour range so
> collectors don't reconnect every 60 seconds.

## 5. Connect your first collector

Point an OpenTelemetry collector at the OpAMP endpoint (`:4320`) and OTLP
receiver (`:4317`). Minimal collector config:

```yaml
extensions:
  opamp:
    server:
      ws:
        endpoint: ws://YOUR_VM_HOST:4320/v1/opamp

receivers:
  hostmetrics:
    collection_interval: 30s
    scrapers:
      cpu: {}
      memory: {}

exporters:
  otlp:
    endpoint: YOUR_VM_HOST:4317
    tls:
      insecure: true

service:
  extensions: [opamp]
  pipelines:
    metrics:
      receivers: [hostmetrics]
      exporters: [otlp]
```

Start the collector. Within a few seconds it appears as **online** on the UI's
Agents page. Open the agent, click **Edit config**, save, and Squadron pushes
the new versioned config over OpAMP.

## Before you call it "production"

Evaluation is done — this checklist turns it into a real deployment:

- Auth on (`auth.require_token: true`) and the bootstrap token rotated.
- TLS on every public-facing port (8080, 4320; also 4317/4318 if they cross an
  untrusted network).
- Tokens scoped narrowly — read-only for dashboards, write only for humans/CI.
- Database backed up (snapshot the SQLite file); do a restore drill once.
- A notification channel (Slack/Teams/PagerDuty/Opsgenie/Discord) wired for
  silent-agent and rollout alerts.

## Where to go next

- `docs/deployment.md` — the full deployment reference and production checklist.
- `docs/getting-started.md` — the 5-minute Docker path and config concepts.
- `docs/operating.md` — environment variables, upgrades, backup, and restore.
- `docs/auth.md` — tokens, scopes, expiry, and rotation.
