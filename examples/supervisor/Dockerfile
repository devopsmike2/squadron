FROM otel/opentelemetry-collector-opampsupervisor:latest as supervisor

# Get OpenTelemetry collector
FROM otel/opentelemetry-collector-contrib:latest AS otel

# Final stage
FROM alpine:latest

# Copy from builder - use the correct source path
COPY --from=supervisor /usr/local/bin/opampsupervisor ./opampsupervisor
COPY supervisor.yaml .
COPY collector.yaml .

# Credential-injecting exec shim: agent.executable in supervisor.yaml points at
# this. It injects secrets into the env then exec's the collector with the
# supervisor's args verbatim. See otel-cred-exec.sh for the exec/passthrough/
# exit-code rationale.
COPY otel-cred-exec.sh /otel-cred-exec.sh
RUN chmod +x /otel-cred-exec.sh

# Copy from OpenTelemetry
COPY --from=otel /otelcol-contrib /otelcol-contrib

EXPOSE 4317 4318

ENTRYPOINT ["./opampsupervisor", "--config", "supervisor.yaml"]