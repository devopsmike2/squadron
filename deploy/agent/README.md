# On-prem agent onboarding

Turn a bare Linux server (data center, VM, bare metal) into a **Squadron-managed
OTel agent** that reports its **config, host metrics, and logs** — in one command.

## Prerequisites

A running Squadron reachable from your servers on three ports:

| Port | Purpose |
|---|---|
| `4320` | OpAMP — agent management (registration, config, health) |
| `4318` | OTLP/HTTP — telemetry ingest (metrics, logs, traces) |
| `8080` | UI / API |

(`4317` OTLP/gRPC is optional — the agent also opens a local `4317`/`4318`
receiver so apps on the same host can ship to `localhost`.)

## One command per server

```bash
curl -fsSL https://raw.githubusercontent.com/devopsmike2/squadron/main/deploy/agent/install-agent.sh \
  | sudo bash -s -- --squadron-host 10.0.0.5 --service-name web-01
```

`--squadron-host` is the Squadron host/IP reachable from that server. The script
installs the OpenTelemetry Collector Contrib, drops a combined config, and starts
it as a systemd service. The server appears in **Squadron → Agents** within ~30s,
OpAMP-managed, with metric + log data flowing.

## Fleet rollout (Ansible example)

```yaml
- hosts: servers
  become: true
  tasks:
    - name: Onboard to Squadron
      ansible.builtin.shell: |
        curl -fsSL https://raw.githubusercontent.com/devopsmike2/squadron/main/deploy/agent/install-agent.sh \
          | bash -s -- --squadron-host {{ squadron_host }} --service-name {{ inventory_hostname }}
      args: { creates: /etc/systemd/system/otelcol-contrib.service.d/10-root.conf }
```

After the first bootstrap you manage every agent's config **centrally from
Squadron** (OpAMP remote config + Rollouts) — you don't touch the boxes again.

## What this gives you (and how it maps to the UI)

| Squadron shows | Comes from |
|---|---|
| Agent online + host/version in Fleet | opamp extension |
| **Config** tab (effective config) | opamp `reports_effective_config` |
| **Metrics** data (CPU/mem/disk/net/…) | `hostmetrics` → `otlphttp` to Squadron |
| **Logs** tab | `filelog` (`/var/log/syslog`, `/var/log/messages`) → `otlphttp` |
| App telemetry from this host | local `otlp` receiver on `:4317`/`:4318` |

## Production notes

- **One host = one agent card.** Squadron derives an agent's fleet identity from
  its `service.instance.id` (falling back to `host.name`), the same way for the
  OpAMP-management channel and the OTLP-telemetry channel — so config + telemetry
  land on a single card even though the opamp extension mints its own ULID for
  the wire. This template does that for you. If you hand-roll a collector and see
  the **same host as two cards**, make sure the value it reports as
  `service.instance.id` over OpAMP matches the one on its OTLP resource (set it
  once via a `resource`/`resourcedetection` processor shared by both), or omit
  `service.instance.id` entirely so both channels fall back to `host.name`.
- The default config talks **plaintext** (`ws://`, `http://`) — fine on a trusted
  network. For untrusted paths, front Squadron with TLS (switch the config to
  `wss://` / `https://` and drop `tls.insecure`) and enable Squadron's bearer auth.
- The collector runs as **root** so it can read `/var/log` and per-process metrics.
- Files: [`install-agent.sh`](./install-agent.sh), [`otelcol-squadron.yaml.tmpl`](./otelcol-squadron.yaml.tmpl).
