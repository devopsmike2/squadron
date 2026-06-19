# Deployment guide

This guide walks through the four supported ways to deploy Squadron, the
components you need to wire up, and the operational concerns that come up
between "running" and "running in production."

If you just want to see Squadron move data, read
[Getting started](./getting-started.md) first — it'll have you at a
running instance with one connected collector in five minutes. This page
picks up from there.

- [Where Squadron sits](#where-squadron-sits)
- [Pick a deployment shape](#pick-a-deployment-shape)
- [Required and optional components](#required-and-optional-components)
- [Single VM](#single-vm)
- [Docker Compose](#docker-compose)
- [Kubernetes](#kubernetes)
- [OpenShift](#openshift)
- [The production checklist](#the-production-checklist)
- [Operational traps](#operational-traps)
- [Time to value](#time-to-value)

## Where Squadron sits

Squadron is a **central control plane** that sits beside your telemetry
pipeline, not inside it. The picture:

```
[Your apps] → [OTel Collectors] → [Splunk / Datadog / S3 / Loki / wherever]
                    ↑
                    │ OpAMP (control protocol)
                    ↓
              [Squadron server]
                    ↑
                    │ HTTPS UI + API
                    ↓
              [Operators, CI, SIEM]
```

The collectors are the workhorses; Squadron tells them what to do. Your
telemetry data itself never flows through Squadron — only configs,
status, and a small slice of pipeline health metrics for the cost
insights surfaces.

Two consequences flow from this:

1. **Squadron is small.** One Go process, modest CPU and memory, an
   embedded SQLite (or external Postgres) for state, an embedded DuckDB
   for telemetry rollups. No Redis, no Kafka, no message bus.
2. **Squadron is not in the hot path.** If Squadron goes down, your
   telemetry keeps flowing on its last pushed config. You lose the
   ability to push new configs and run rollouts until it's back, but
   you don't lose telemetry.

## Pick a deployment shape

The four supported shapes, in order of complexity:

| Shape | Best for | Setup time |
|---|---|---|
| Single VM | Evaluation, small fleets (< 500 collectors), self hosted demos | 15 minutes |
| Docker Compose | First real evaluation, dev/staging, teams not on Kubernetes | 30 minutes |
| Kubernetes (Helm) | Production for teams already on Kubernetes | Half day to a day |
| OpenShift | Production for enterprise customers in regulated industries | One day |

The honest recommendation for most teams: start at the **Single VM**
tier to evaluate Squadron for a week, then jump to **Kubernetes** or
**OpenShift** when you're ready for production. Docker Compose is for
teams who want a quick evaluation but plan to run prod outside
Kubernetes.

## Required and optional components

What Squadron actually needs to function, in three tiers.

### Mandatory

The floor — with these three, you can register collectors, push configs,
run rollouts, see audit events, and use the UI.

1. **A place to run the binary.** One VM, container, or pod. Modest
   sizing: 1 vCPU and 1 GB RAM handles hundreds of collectors.
2. **Network reachability from collectors to Squadron's OpAMP port.**
   Default port 4320, configurable. If collectors can't reach Squadron,
   the fleet is on its last known config and Squadron can't push changes.
3. **At least one OTel collector somewhere that speaks OpAMP.** With
   zero collectors, Squadron has nothing to control. The Quickstart
   endpoint generates the OpAMP extension snippet your collectors need;
   the [adoption snippet](./inventory.md) covers the case where you're
   adopting an existing fleet without reconfiguring everything.

### Strongly recommended for real use

4. **HTTPS termination.** Squadron speaks HTTP natively; a reverse
   proxy (Caddy, nginx, an ALB, an Ingress) handles TLS. Without this,
   tokens and configs flow in cleartext.
5. **API tokens with scopes enabled.** See [Authentication](./auth.md).
   Auth is opt in for backwards compatibility with the early version
   local dev workflow, but flip it on for anything beyond evaluation.
6. **An external Postgres** if you're past 500 collectors or want a
   high availability pair. SQLite is fine to surprisingly high scales
   but does not replicate.

### Optional, but unlocks the platform's full value

7. **AI provider key (Anthropic).** Without this, the AI proposer,
   Ask Squadron, fleet query, explain panel, incident drafter, and AI
   assist features all degrade to off. Squadron still works as a
   control plane (rollouts, approval, audit all function), it just
   isn't the operator's deputy. With the key, the JARVIS shaped
   features come alive. See [ai-features.md](./ai-features.md).
8. **A SIEM endpoint.** Splunk HEC, generic webhook, or another sink.
   The audit events are valuable; sending them to a SIEM is what turns
   Squadron into a compliance grade system. The NERC CIP, NIST CSF,
   SOC 2, and HIPAA mapping docs assume this is wired.
9. **A notification channel.** Slack, Teams, PagerDuty, Opsgenie, or
   Discord. Without one, alerts and rollout webhooks fire into the
   void. See [silent-agents.md](./silent-agents.md) and
   [alerts.md](./alerts.md).
10. **A deploy provider integration.** GitHub Actions, Azure DevOps, or
    Ansible Tower. If you want Squadron's deploy page to drive your
    existing CI/CD workflow rather than just observe it, wire this.
    See [deploy.md](./deploy.md).
11. **An action runner deployed on hosts you want to take remediation
    actions on.** Without runners, the action types
    (`restart-systemd-service`, `restart-docker`, `run-shell-allowlist`)
    are theoretical. With at least one runner, Squadron can actually
    do things, not just propose them. See
    [action-runner-design.md](./action-runner-design.md).

## Single VM

The simplest path: one Go binary, SQLite for storage, the embedded UI
served from the same process. Fine for evaluation, small fleets, and
self hosted demos.

### Install

Download the latest release tarball and extract:

```bash
curl -L https://github.com/devopsmike2/squadron/releases/latest/download/squadron-linux-amd64.tar.gz \
  | tar -xz -C /usr/local/bin/

squadron version
```

Or build from source:

```bash
git clone https://github.com/devopsmike2/squadron.git
cd squadron
make build-all-in-one
```

### Configure

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

# Opt in to bearer token auth. The first start mints a bootstrap
# token and prints it to the log; rotate it via the UI after.
auth:
  require_token: true

# Optional: enable AI assist + Ask Squadron.
ai:
  enabled: true
  api_key_env: ANTHROPIC_API_KEY

# Optional: ship Squadron's own audit events to your SIEM.
siem:
  destinations:
    - type: splunk_hec
      url: https://splunk.example.com:8088
      extra:
        token_env: SPLUNK_HEC_TOKEN
```

### Run as a systemd service

```ini
# /etc/systemd/system/squadron.service
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

```bash
useradd --system --home /var/lib/squadron squadron
mkdir -p /var/lib/squadron /etc/squadron
chown -R squadron:squadron /var/lib/squadron
systemctl daemon-reload
systemctl enable --now squadron
```

### Terminate TLS

Squadron speaks HTTP natively. Put Caddy or nginx in front for TLS:

```caddyfile
squadron.example.com {
  reverse_proxy localhost:8080
}
squadron-opamp.example.com {
  reverse_proxy localhost:4320
}
```

## Docker Compose

Same binary, containerized, with a compose file that brings up
Squadron plus a couple of synthetic collectors so you can see the
control plane driving a fleet end to end. This is the recommended path
for a first real evaluation.

The repo ships a ready to use compose under `deploy/test/`. See
[testing.md](./testing.md) for the full walkthrough; the short
version:

```bash
git clone https://github.com/devopsmike2/squadron.git
cd squadron
make test-env-up
open http://localhost:8080
```

The compose brings up:

- The Squadron control plane on `:8080` (UI/API), `:4320` (OpAMP),
  `:4317` and `:4318` (OTLP).
- Two synthetic collectors that phone home over OpAMP so the Agents
  page has rows immediately.
- A webhook receiver on `:9000` for testing the silent agent and
  rollout webhook surfaces.

For production via Compose, the same yaml file you'd use on a single
VM applies; mount it into the container at `/etc/squadron/squadron.yaml`
and persist `/data` to a named volume.

## Kubernetes

The production path for teams already on Kubernetes. Squadron runs as a
Deployment behind a Service and Ingress; SQLite gets swapped for an
external Postgres; the OpAMP port needs its own listener since it's a
long lived WebSocket and some Ingress controllers handle that
differently from regular HTTP.

A minimal manifest skeleton:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: squadron
spec:
  replicas: 1                  # bump to 2+ with Postgres for HA
  selector:
    matchLabels:
      app: squadron
  template:
    metadata:
      labels:
        app: squadron
    spec:
      containers:
        - name: squadron
          image: ghcr.io/devopsmike2/squadron:latest
          ports:
            - name: http
              containerPort: 8080
            - name: opamp
              containerPort: 4320
            - name: otlp-grpc
              containerPort: 4317
            - name: otlp-http
              containerPort: 4318
          envFrom:
            - secretRef:
                name: squadron-secrets   # ANTHROPIC_API_KEY etc.
          volumeMounts:
            - name: config
              mountPath: /etc/squadron
            - name: data
              mountPath: /data
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 5
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 30
      volumes:
        - name: config
          configMap:
            name: squadron-config
        - name: data
          persistentVolumeClaim:
            claimName: squadron-data
---
apiVersion: v1
kind: Service
metadata:
  name: squadron
spec:
  selector:
    app: squadron
  ports:
    - name: http
      port: 80
      targetPort: http
    - name: opamp
      port: 4320
      targetPort: opamp
    - name: otlp-grpc
      port: 4317
      targetPort: otlp-grpc
    - name: otlp-http
      port: 4318
      targetPort: otlp-http
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: squadron
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
spec:
  tls:
    - hosts: [squadron.example.com]
      secretName: squadron-tls
  rules:
    - host: squadron.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: squadron
                port:
                  name: http
```

The long proxy timeouts are important — OpAMP holds connections open
indefinitely and an Ingress that closes them after the default 60
seconds will cause collectors to thrash reconnecting. If your Ingress
controller doesn't handle WebSocket upgrades well, expose port 4320
separately via a LoadBalancer service.

### Postgres for HA

For high availability and fleets past 500 collectors, swap SQLite for
Postgres in the config:

```yaml
storage:
  postgres_dsn: postgres://squadron:secret@postgres:5432/squadron
```

With Postgres backing storage, you can run two or more Squadron
replicas behind the Service for active/active load balancing. The OpAMP
connections stick to whichever replica accepted them; everything else
is stateless.

## OpenShift

The dedicated path for enterprise customers in regulated industries. See
[openshift.md](./openshift.md) for the full walkthrough, including the
SecurityContextConstraint requirements and the route definitions.

The shape is the same as Kubernetes but with:

- An SCC that grants Squadron the network capabilities it needs for
  OpAMP without running as root.
- OpenShift Routes instead of Ingress for HTTPS termination.
- Image pull secrets from the OpenShift registry if you mirror the
  Squadron image internally.

The compliance pack (NERC CIP, NIST CSF, HIPAA, SOC 2 mappings) is the
typical reason an OpenShift deployment exists; the mapping docs
themselves live under `docs/compliance/`.

## The production checklist

Before flipping a Squadron deployment from "evaluation" to "production,"
walk through this list. See [operating.md](./operating.md) for the
deeper version.

- [ ] **Auth on.** `auth.require_token: true` in the config. Bootstrap
      token rotated. Long lived tokens scoped narrowly (read only
      tokens for monitoring integrations, write tokens only for the
      humans and CI jobs that need them).
- [ ] **HTTPS in front.** Reverse proxy or Ingress terminating TLS on
      every public facing port (HTTP 8080, OpAMP 4320). OTLP ingest
      (4317/4318) should be TLS too if it crosses an untrusted network.
- [ ] **Token expiry set.** Every token has an expires_at. The
      bootstrap token's expiry is short (hours, not weeks).
- [ ] **SIEM destination wired** if compliance is in scope. Verify the
      `audit.access_attempt` and `rollout.*` events arrive in your SIEM
      by triggering a test event from the UI.
- [ ] **Notify channel wired.** At least one Slack/Teams/PD/Opsgenie/
      Discord destination receives silent agent alerts and rollout
      webhooks. Test the resolve path, not just the fire path.
- [ ] **Change windows configured** for production tier groups. The
      engine respects them; if no groups have them, no advances are
      blocked.
- [ ] **`require_approval` on prod groups.** Two person approval rule
      enforced by the service layer. The token used for human approval
      must have the `rollouts:approve` scope.
- [ ] **Backup the database.** SQLite: snapshot the file. Postgres:
      whatever you do for everything else. Restore drill once.
- [ ] **Self monitoring on.** OTel traces flow into your existing
      observability stack so a Squadron incident is investigable
      through the same tools you use for everything else.
      See [self-monitoring.md](./self-monitoring.md).

## Operational traps

A few specific things that bite teams during setup, in rough order of
frequency:

**Network reachability between collectors and Squadron.** OpAMP is a
WebSocket connection that the collector holds open indefinitely.
Corporate networks that block long lived outbound connections will
break this. The standard fixes: punch a hole in the egress policy for
the OpAMP port, or run Squadron inside the same network segment as the
fleet.

**Collector version drift.** OpAMP support in the upstream OTel
collector is recent. Older custom builds may not speak it cleanly. The
adoption snippet covers common cases but assumes a current contrib
build. If you're adopting an older fleet, plan on upgrading the
collector binary alongside wiring Squadron in.

**Ingress closing the OpAMP socket.** The proxy_read_timeout default on
most Ingress controllers is 60 seconds. OpAMP needs that timeout in the
hour range or the collectors will spend their lives reconnecting.

**Group policy bootstrapping.** A fresh Squadron has zero groups, so
everything lands in a default bucket. The first day of real use is
usually spent labeling agents, creating groups, setting
`require_approval` on prod tier groups, and configuring change
windows. Not hard, but easy to forget, leaves a fleet where anyone can
rollout to prod.

**Token scope sprawl.** It's easy to create a single all scopes token
early and never rotate. The scope vocabulary exists to scope tokens
narrowly; lean on it. Use read only tokens for dashboards and
monitoring integrations; write tokens only for the humans and CI jobs
that need them.

**AI key cost surprise.** Without a per day spend cap, an aggressive
proposer config can ring up bills. The proposer dedup logic prevents
the obvious foot guns but does not enforce a daily budget. Set a
budget alert at Anthropic and document a recommended
`max_proposals_per_day` for new deployments.

## Time to value

The honest curve for an evaluator who wants to see Squadron working in
their environment:

- **15 minutes:** binary running, UI loads, demo data seeded.
- **1 hour:** one real collector phoning home over OpAMP, one config
  pushed via rollout.
- **Half day:** TLS, auth tokens, Slack webhook wired, AI key set,
  audit events visible.
- **1 to 2 days:** Kubernetes deployment, Postgres, full SIEM fan
  out, deploy provider integration, one action runner per host class.
- **1 week:** compliance pack deployed, change window policies set per
  group, approval workflow configured, NERC CIP / SOC 2 mappings
  reviewed against the organization's actual controls.

Roughly comparable to setting up a peer like Datadog or Grafana: a
small team evaluation in an afternoon, a real production deployment in
days, an enterprise grade rollout in weeks.

## See also

- [Getting started](./getting-started.md) — the 5 minute path to a
  running instance.
- [Operating Squadron](./operating.md) — environment variables,
  upgrades, backup, restore.
- [Authentication](./auth.md) — tokens, scopes, expiry, rotation.
- [OpenShift](./openshift.md) — the enterprise path in detail.
- [Self monitoring](./self-monitoring.md) — Squadron's own telemetry
  into your existing observability stack.
- [testing.md](./testing.md) — the docker compose harness for local
  evaluation.
