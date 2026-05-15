# Squadron OTEL Lab

Spin up the Squadron all-in-one container plus 10 synthetic OTEL agents for quick local testing.

## Usage

1. Copy `.env.example` to `.env` and tweak `TRACE_RATE`, `TRACE_DURATION`, or `SQUADRON_OTLP_GRPC_ENDPOINT` if needed.
2. From the repo root, run:

   ```bash
   docker compose -f profiles/testing/otel-lab/docker-compose.yml up --build -d
   ```

3. Open <http://localhost:8080> to access the Squadron UI. The synthetic agents appear as `agent-01` … `agent-10` with tags `env=test, profile=squadron-lab`.

## Cleanup

```bash
docker compose -f profiles/testing/otel-lab/docker-compose.yml down -v
```

## Notes

- The telemetry generators send spans over OTLP/gRPC to `squadron:4317` by default. Override `SQUADRON_OTLP_GRPC_ENDPOINT` in `.env` to target a different Squadron deployment.
- Adjust `TRACE_RATE`/`TRACE_DURATION` to simulate heavier or shorter bursts of traffic.
