# Arc: OTEL-agent deployment + config injection

## Problem

Squadron's discovery→remediation today instruments resources by editing
**IaC** (add an ADOT layer, a Monitor agent, etc.). But many shops run a
standalone **OpenTelemetry Collector / agent** on their hosts and wire it
to a backend by editing the *agent's own config file* (the OTLP exporter
endpoint). Squadron should meet those shops where they are: discover an
agent that is installed but not pointed at Squadron, and inject the
Squadron OTLP endpoint into its config so it auto-connects and starts
being tracked in Fleet.

Two real-world deployment styles, both supported:

1. **Static / file-managed agents** — the collector config lives in a
   repo (or on the host). Squadron injects its OTLP exporter endpoint
   into the config file and delivers the change as a PR (reuses the
   env→TF / remediation PR machinery).
2. **Centrally-managed agents (OpAMP)** — the collector runs under the
   OpAMP supervisor pointing at Squadron's OpAMP server (`:4320`).
   Squadron already pushes config to these (see `internal/opamp`,
   `examples/supervisor`). The agent auto-registers in Fleet with no
   file edits. This path largely exists; the arc adds a *deployable
   example* (a cloud VM, not just a compose container).

## Test repo: `squadron-test-otel-agents`

Terraform, mirroring the other `squadron-test-*` repos. Two targets:

- **inject-target VM** — boots a standalone `otelcol-contrib` whose
  config has a placeholder exporter (`endpoint: REPLACE_WITH_SQUADRON_OTLP`)
  and is therefore *installed but not connected*. This is what discovery
  flags and the injector fixes.
- **opamp-target VM** — boots `opampsupervisor` + `otelcol-contrib`
  with `server.endpoint = ws://<squadron-opamp>/v1/opamp` (replicating
  `examples/supervisor`). On boot it connects → Fleet.

The repo's collector config files are the injection surface for the PR
path and the oracle for verification.

## Squadron capability: collector-config injector (slice 1)

`internal/iac/otelconfig.InjectOTLPExporter(src []byte, endpoint string, opts Options) (Result, error)`

- Ensures an OTLP exporter (`otlp` grpc or `otlphttp`) exists with the
  given `endpoint` (+ `tls.insecure` for the dev/self-signed case).
- Wires that exporter into every `service.pipelines.*.exporters` list
  it isn't already in (traces/metrics/logs, configurable).
- **Idempotent**: a config already pointing at `endpoint` and wired in
  yields `Changed=false` (no PR/no-op), mirroring the dedup posture of
  the env→TF and merge-ready-PR arcs.
- **Minimal-diff**: edits a `yaml.Node` tree in place so untouched keys,
  ordering, and comments survive — the delivered PR diff is small and
  review-friendly.

`Options`: exporter name (default `otlp`), protocol (grpc|http),
insecure (bool), signals (traces/metrics/logs subset), and whether to
create `service`/`pipelines` scaffolding when absent.

## Slices

1. Deterministic injector + tests (this PR).
2. `squadron-test-otel-agents` Terraform repo (both targets) + validate.
3. PR-delivery endpoint that runs the injector against a connected
   repo's collector config + UI affordance.
4. OpAMP example VM live-verified into Fleet; docs.

## Non-goals (for now)

- Editing agent configs *on live hosts* (SSM/run-command). File/PR and
  OpAMP cover the two requested styles; host mutation is a later arc.
- Non-OTel agents (Datadog, etc.).
