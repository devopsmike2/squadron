# Production Dockerfile for Squadron
# Multi-stage build with Go backend and React frontend

# =============================================================================
# Stage 1: Build Go Backend
# =============================================================================
FROM golang:1.25-bookworm AS backend-builder

# Install build dependencies (including gcc/g++ for CGO, SQLite, and DuckDB)
RUN apt-get update && apt-get install -y \
    git \
    ca-certificates \
    tzdata \
    gcc \
    g++ \
    libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application with CGO enabled for DuckDB
RUN CGO_ENABLED=1 GOOS=linux go build -a -o squadron ./cmd/all-in-one

# =============================================================================
# Stage 2: Build React Frontend
# =============================================================================
FROM node:20-alpine AS frontend-builder

# Install pnpm
RUN npm install -g pnpm

# Set working directory
WORKDIR /app

# Allow builders to override the default backend URL used at build time.
ARG VITE_BACKEND_URL=http://localhost:8080
ENV VITE_BACKEND_URL=${VITE_BACKEND_URL}

# Copy package files
COPY ui/package.json ui/pnpm-lock.yaml ./

# Install dependencies
RUN pnpm install --frozen-lockfile

# Copy source code
COPY ui/ .

# Build the frontend
RUN pnpm build

# =============================================================================
# Stage 3: Production Image
# =============================================================================
FROM debian:bookworm-slim

# OCI labels for GitHub Container Registry
LABEL org.opencontainers.image.source="https://github.com/devopsmike2/squadron"
LABEL org.opencontainers.image.description="OpenTelemetry agent management platform with built-in observability backend"
LABEL org.opencontainers.image.licenses="Apache-2.0"

# Install runtime dependencies (including sqlite and C++ libs for DuckDB)
RUN apt-get update && apt-get install -y \
    ca-certificates \
    tzdata \
    curl \
    libsqlite3-0 \
    libstdc++6 \
    && rm -rf /var/lib/apt/lists/*

# OpenShift-friendly user setup. The image runs as UID 1001 by
# default, but on platforms like OpenShift that enforce a random
# non-root UID (restricted-v2 SCC), the USER directive is ignored
# and the pod runs under an arbitrary UID. Standard mitigation:
# put files in group 0 (root group), which every OpenShift random
# UID belongs to by default, and make writable paths group-writable.
# Result: the image works under both vanilla Docker (UID 1001) and
# OpenShift (random UID + GID 0).
RUN groupadd -g 1001 squadron && \
    useradd -u 1001 -g 0 -s /bin/bash -m squadron

# Set working directory
WORKDIR /app

# Copy backend binary
COPY --from=backend-builder /app/squadron .

# Copy frontend build
COPY --from=frontend-builder /app/dist ./ui/dist

# Copy configuration
COPY squadron.yaml .

# Copy runtime entrypoint for injecting frontend config
COPY docker/entrypoint.sh /entrypoint.sh

# Create data directory + harden ownership for OpenShift. The
# entrypoint writes squadron-config.js back into /app/ui/dist at
# startup; /app/data holds SQLite + DuckDB at runtime. Both need
# to be writable by the assigned UID, which on OpenShift is random
# but always has GID 0.
RUN chmod +x /entrypoint.sh && \
    mkdir -p /app/data && \
    chown -R 1001:0 /app /entrypoint.sh && \
    chmod -R g=u /app && \
    chmod g=u /entrypoint.sh

# Use the numeric UID so OpenShift's "no root" check passes
# (USER squadron would also work but numeric is more explicit
# and survives `oc adm pod-network` and similar tooling that
# resolves uids rather than names).
USER 1001

# Expose ports
# 8080 - HTTP API
# 4320 - OpAMP server
# 4317 - OTLP gRPC
# 4318 - OTLP HTTP
EXPOSE 8080 4320 4317 4318

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

# Set environment variables
ENV GIN_MODE=release
ENV TZ=UTC

ENTRYPOINT ["/entrypoint.sh"]
# Run the application
CMD ["./squadron"]