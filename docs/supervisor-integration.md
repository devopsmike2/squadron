# OpAMP supervisor integration (closed-loop config)

Squadron manages OpenTelemetry collectors over [OpAMP](https://opentelemetry.io/docs/specs/opamp/).
There are two ways a collector can connect, and they differ in one important way:
whether Squadron can only **observe** the collector or can also **push config** to it.

| Model | How the collector connects | What Squadron can do |
| --- | --- | --- |
| **Report-only** | The collector runs the `opamp` **extension** in its own config, pointing at Squadron. | See the agent, its health, and its effective config. **Cannot push config.** |
| **Supervisor (closed-loop)** | The [`opampsupervisor`](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/cmd/opampsupervisor) owns the collector process; the supervisor connects to Squadron. | Everything report-only does, **plus push managed config** and restart the collector to apply it. |

This page covers the supervisor model: how to run it against Squadron, the one
capability that turns pushing on, how it fixes duplicate agent identity, how to run
it alongside a credential-injecting launcher, and the safety guidance for pushing
config.

Reference artifacts live in [`examples/supervisor/`](https://github.com/DevOpsMike2/squadron/tree/main/examples/supervisor):
`supervisor.yaml`, a base `collector.yaml`, `otel-cred-exec.sh` (and a Python
variant), and a `Dockerfile`.

## Report-only vs supervisor

**Report-only** is the bare `opamp` **extension** inside the collector's own config:

```yaml
# report-only: the collector manages ITSELF and just reports to Squadron
extensions:
  opamp:
    server:
      ws:
        endpoint: ws://squadron:4320/v1/opamp
    capabilities:
      reports_effective_config: true
      reports_health: true
service:
  extensions: [opamp]
```

The collector reads its config from disk, and Squadron watches. Squadron shows the
agent, its health, and its effective config, but the config still lives on the host —
Squadron cannot change it. This is the right model when config is owned elsewhere
(GitOps, Ansible, a golden image) and you only want visibility.

**Supervisor** inverts control. The supervisor is the long-running process; it owns
the collector's lifecycle: it writes the merged effective config to disk and starts
(and restarts) the collector to apply it. The supervisor connects to Squadron and
can accept a pushed config, merge it, and cycle the collector. This is the model that
makes Squadron a **control plane**, not just a dashboard.

## Running the supervisor against Squadron

1. Install `opampsupervisor` and `otelcol-contrib` on the host.
2. Point the supervisor at Squadron's OpAMP endpoint and give it a base collector
   config. Use [`examples/supervisor/supervisor.yaml`](https://github.com/DevOpsMike2/squadron/blob/main/examples/supervisor/supervisor.yaml)
   as a starting point:

   ```yaml
   server:
     endpoint: ws://squadron:4320/v1/opamp
   capabilities:
     reports_effective_config: true
     reports_health: true
     accepts_remote_config: true       # turns push ON (see below)
     reports_remote_config: true
     accepts_restart_command: true
   agent:
     executable: /otelcol-contrib
     config_files:
       - /collector.yaml               # base config, merged under Squadron's push
   storage:
     directory: /var/lib/opamp         # persistent — see instance_uid below
   ```

3. Run the **supervisor** as your service (systemd, container entrypoint, etc.) —
   **not** the collector directly. The process tree is:

   ```
   systemd ─► opampsupervisor ─► otelcol-contrib
   ```

4. In the base `collector.yaml`, do **not** add an `opamp` extension. The supervisor
   injects its own opamp extension into the merged config it writes; a second,
   hand-written one would create a competing control channel. Keep pipelines and
   self-telemetry in the base config and let the supervisor overlay Squadron's
   config on top. See [`examples/supervisor/collector.yaml`](https://github.com/DevOpsMike2/squadron/blob/main/examples/supervisor/collector.yaml).

## The capability that flips push on

Squadron only pushes config to agents that advertise the **`AcceptsRemoteConfig`**
capability. Setting `accepts_remote_config: true` in `supervisor.yaml` is the single
switch that moves a collector from report-only to closed-loop.

This is enforced server-side, not a UI convention:

- The reconcile loop skips any agent that does not advertise the capability
  (`internal/opamp/reconciler.go`).
- The config sender refuses to send to such an agent — *"agent does not support
  remote config"* (`internal/opamp/config_sender.go`).

So until the supervisor advertises `accepts_remote_config`, creating a config in
Squadron and assigning it is a no-op for that agent; flip the capability and Squadron
begins delivering.

## Persistent identity: fixing duplicate agent cards

Squadron keys an agent's card on the OpAMP instance UID. A collector that does not
persist its instance UID gets a **new UID — and therefore a new card — on every
restart**, which shows up as duplicate agents for a single host.

The supervisor fixes this natively. It stores a `persistent_state.yaml` in its
`storage.directory` holding a stable instance UID (a UUIDv7), and reuses it across
restarts (`cmd/opampsupervisor/supervisor/persistence.go`). As long as
`storage.directory` is on **persistent** storage (a real path or a mounted volume —
not an ephemeral tmpfs), the collector keeps **one identity** across restarts and
redeploys. This is the correct, agent-side fix for duplicate cards; make sure
`storage.directory` survives restarts.

## Running the supervisor with a credential-injecting launcher

A common real-world constraint: the collector needs secrets (backend tokens, mTLS
material) that must be decrypted at start time and **never written to disk in
cleartext**. Before adopting the supervisor, teams often make a credential wrapper
the top-level launcher — it decrypts secrets, sets env, and then runs the collector.

That collides with the supervisor, because the supervisor wants to be the top-level
process that owns the collector. The fix is to **invert** the arrangement: run the
supervisor at the top and turn the credential wrapper into an **exec shim** that the
supervisor launches as its `agent.executable`.

The supervisor launches `agent.executable` as a single child process and passes it
`--config <the-config-the-supervisor-wrote>`. Point `agent.executable` at the shim.
The shim injects secrets into the environment, then `exec`s the collector —
**replacing its own process image** — forwarding the supervisor's arguments verbatim.
Because `exec` replaces the process, the collector inherits the shim's pid, so the
supervisor tracks the collector directly:

```
systemd ─► opampsupervisor ─► [otel-cred-exec shim; exec ─►] otelcol-contrib
```

A minimal shim ([`examples/supervisor/otel-cred-exec.sh`](https://github.com/DevOpsMike2/squadron/blob/main/examples/supervisor/otel-cred-exec.sh)):

```bash
#!/usr/bin/env bash
set -euo pipefail
# (1) materialize secrets into the environment (replace with your real retrieval)
export BACKEND_TOKEN="$(your-secret-tool read ...)"
# (2) exec the collector, forwarding the supervisor's args verbatim
exec /otelcol-contrib "$@"
```

Then in `supervisor.yaml`:

```yaml
agent:
  executable: /otel-cred-exec.sh   # the shim, not the collector
```

Secrets are consumed from the environment in the collector config via `${env:...}`,
so they never touch disk:

```yaml
exporters:
  otlphttp/backend:
    endpoint: https://backend.example:4318
    headers:
      authorization: "Bearer ${env:BACKEND_TOKEN}"
```

### Three gotchas the shim must get right

These are subtle and each one silently breaks the closed loop:

1. **`exec`, not subprocess.** Use `exec` (bash) / `os.execv` (Python) so the
   collector inherits the shim's pid. If the shim instead spawns the collector as a
   child and stays alive (or forks it to the background), the supervisor tracks the
   **shim**, not the collector — its restart/stop signals and health checks never
   reach the collector, which runs as an invisible grandchild.

2. **Forward the supervisor's args verbatim (`"$@"` / `sys.argv[1:]`).** Do **not**
   hardcode a `--config` path. The supervisor owns the config file location (inside
   its `storage.directory`) and passes it in. Hardcoding a path makes the collector
   run a different config than the one Squadron manages, silently breaking the loop.

3. **Propagate the exit code.** With `exec`/`execv` this is automatic — the
   collector's exit status becomes the shim's. A wrapper that runs the collector as a
   subprocess and then `exit 0` (regardless of the child's status) **hides crashes**:
   systemd and the supervisor see a clean exit, so a bad config becomes a silent
   restart loop instead of a surfaced failure.

The environment set by the shim reaches the collector because `exec` preserves the
process environment across the replacement. (The supervisor itself starts the shim
with its own environment plus any static `agent.env` from `supervisor.yaml`, so
credential-tool configuration — vault address, role id, etc. — can be supplied via
the service unit's `Environment=` / `EnvironmentFile=`.)

## Validate before push, and health-gate after

A collector reads its config only at process **start**. A bad managed config, once
applied, can take the collector down. Two safeguards:

- **The supervisor validates before applying.** Before (re)starting the collector on
  a new config, the supervisor runs the collector's own `validate` subcommand against
  the merged config and refuses to apply an invalid one
  (`cmd/opampsupervisor/supervisor/commander.go`). This is a real gate that a plain
  "write file + restart" pipeline does not have.
- **Confirm health after a change.** After a config change lands, verify the
  collector came back healthy (the supervisor reports health to Squadron via
  `reports_health`; you can also gate a deploy on the collector's own
  `health_check` extension / `systemctl is-active`). Treat a config change as
  incomplete until health is confirmed.

## What the closed loop does and does not guarantee

Squadron's loop is **delivery-confirmed**: when you assign a config, the reconciler
delivers it and considers it done once the agent echoes back the matching config hash
(`LastRemoteConfigHash`); a redundant push with the same content produces zero wire
sends (it is idempotent). So you get positive confirmation that the collector
received and applied the config Squadron sent.

It is **not a continuous drift auto-healer**. Squadron reconciles the config it
manages over OpAMP; it does not detect or automatically revert arbitrary changes made
to the host outside that channel (someone editing the base `config_files`, replacing
the binary, or stopping the service out-of-band). Keep the supervisor as the sole
owner of the collector process, and manage config through Squadron, so "what Squadron
delivered" and "what is running" stay the same thing.

## See also

- [Deployment guide](deployment.md) — exposing the OpAMP (4320) and OTLP (4318) endpoints.
- [Staged rollouts](rollouts.md) — pushing a config change across a fleet safely.
- [`examples/supervisor/`](https://github.com/DevOpsMike2/squadron/tree/main/examples/supervisor) — the reference supervisor, collector, and exec-shim.
