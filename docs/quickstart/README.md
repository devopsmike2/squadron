# Squadron OSS quickstarts

Short, new-user paths to a running Squadron OSS instance. Pick the one that
matches where you want to run it.

| Guide | Best for | Time |
|---|---|---|
| [Single VM](./vm-quickstart.md) | Evaluation, small fleets (< 500 collectors), self-hosted demos | ~15 min |
| [Kubernetes (Helm)](./kubernetes-quickstart.md) | Production for teams already on Kubernetes | ~30 min |

Just want to see it move in five minutes? The Docker path in
[`../getting-started.md`](../getting-started.md) is the fastest look.

## Things that are true for every OSS deployment

- **Single instance.** OSS uses an embedded store (SQLite + DuckDB). Postgres,
  HA, and multi-replica clustering are commercial-tier — keep it to one instance.
- **Four ports.** 8080 (UI/API), 4320 (OpAMP WebSocket to collectors),
  4317/4318 (OTLP gRPC/HTTP).
- **Not in the hot path.** Your telemetry never flows through Squadron — only
  configs, status, and health. If Squadron is down, collectors keep running on
  their last pushed config.
- **OpAMP needs long timeouts.** It's a persistent WebSocket; any proxy in front
  of port 4320 needs hour-range read/send timeouts, not the usual 60 seconds.

For the full reference — deployment shapes, production checklist, and
operational traps — see [`../deployment.md`](../deployment.md). For what OSS
includes vs. the commercial tier, see
[`../oss-vs-enterprise.md`](../oss-vs-enterprise.md).
