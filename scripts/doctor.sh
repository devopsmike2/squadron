#!/bin/sh
# Squadron doctor — preflight + health diagnostics.
#
#   ./scripts/doctor.sh            # check this host + a running instance on :8080
#   SQUADRON_PORT=9090 ./scripts/doctor.sh
#
# Exit 0 if Squadron is reachable and healthy (or the host is ready to run
# it); non-zero if a hard prerequisite is missing.
set -u

PORT="${SQUADRON_PORT:-8080}"
HOST="${SQUADRON_HOST:-localhost}"
BASE="http://$HOST:$PORT"
fail=0

ok()   { printf '\033[32m  ok  \033[0m %s\n' "$1"; }
warn() { printf '\033[33m warn \033[0m %s\n' "$1"; }
bad()  { printf '\033[31m fail \033[0m %s\n' "$1"; fail=1; }
hdr()  { printf '\n\033[36m%s\033[0m\n' "$1"; }

hdr "Host prerequisites"
if command -v docker >/dev/null 2>&1; then ok "docker: $(docker --version 2>/dev/null | sed 's/,.*//')"
else bad "docker not installed — https://docs.docker.com/get-docker/"; fi
if docker info >/dev/null 2>&1; then ok "docker daemon running"
else bad "docker daemon not reachable — start Docker and retry"; fi
if docker compose version >/dev/null 2>&1; then ok "compose: $(docker compose version --short 2>/dev/null)"
elif command -v docker-compose >/dev/null 2>&1; then ok "compose (legacy): $(docker-compose version --short 2>/dev/null)"
else bad "docker compose v2 not found"; fi

hdr "Port availability (host)"
check_port() {
  p="$1"; label="$2"
  if command -v lsof >/dev/null 2>&1; then
    holder=$(lsof -nP -iTCP:"$p" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $1}')
    if [ -n "$holder" ]; then warn "port $p ($label) in use by '$holder' — fine if that's Squadron"; else ok "port $p ($label) free"; fi
  else
    ok "port $p ($label) — (install lsof for an in-use check)"
  fi
}
check_port "$PORT" "UI+API"
check_port 4320 "OpAMP"
check_port 4317 "OTLP gRPC"
check_port 4318 "OTLP HTTP"

hdr "Running instance at $BASE"
code=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "$BASE/health" 2>/dev/null || echo 000)
if [ "$code" = "200" ]; then
  ok "/health = 200 (healthy)"
  ai=$(curl -s -m 5 "$BASE/api/v1/ai/status" 2>/dev/null)
  prov=$(printf '%s' "$ai" | sed -n 's/.*"provider":[ ]*"\([^"]*\)".*/\1/p')
  case "$ai" in
    *'"enabled":true'*) ok "AI assist: enabled${prov:+ (provider: $prov)}" ;;
    *) warn "AI assist: disabled (set an AI provider API key to enable)" ;;
  esac
  ag=$(curl -s -m 5 "$BASE/api/v1/agents" 2>/dev/null)
  n=$(printf '%s' "$ag" | sed -n 's/.*"totalCount":[ ]*\([0-9]*\).*/\1/p')
  [ -n "$n" ] && ok "fleet: $n agent(s) registered" || warn "fleet: could not read agent count"
  printf '\n\033[32mSquadron is healthy.\033[0m Open %s/quickstart\n' "$BASE"
else
  warn "no healthy instance at $BASE (code=$code) — start one with: docker compose up -d"
fi

[ "$fail" -eq 0 ] && exit 0 || { printf '\n\033[31mOne or more hard prerequisites are missing.\033[0m\n'; exit 1; }
