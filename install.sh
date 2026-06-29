#!/bin/sh
# Squadron one-step installer — no clone, no build.
#   curl -fsSL https://raw.githubusercontent.com/devopsmike2/squadron/main/install.sh | sh
#
# Fetches the standalone compose into ./squadron and starts it. Re-running
# is safe (it pulls the latest image and recreates). Set SQUADRON_VERSION to
# pin a tag, ANTHROPIC_API_KEY to enable AI.
set -eu

RAW="https://raw.githubusercontent.com/devopsmike2/squadron/main"
DIR="${SQUADRON_DIR:-squadron}"
PORT="${SQUADRON_PORT:-8080}"

say()  { printf '\033[36m==>\033[0m %s\n' "$1"; }
ok()   { printf '\033[32m  ok\033[0m %s\n' "$1"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$1" >&2; exit 1; }

say "Checking prerequisites"
command -v docker >/dev/null 2>&1 || die "Docker is not installed. See https://docs.docker.com/get-docker/"
docker info >/dev/null 2>&1 || die "Docker is installed but the daemon isn't running. Start Docker and retry."
if docker compose version >/dev/null 2>&1; then DC="docker compose";
elif command -v docker-compose >/dev/null 2>&1; then DC="docker-compose";
else die "Docker Compose v2 not found. Install Docker Desktop or the compose plugin."; fi
ok "docker + compose present"

# Warn (don't fail) if the API port is already in use by something else.
if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  printf '\033[33m  warn\033[0m port %s is already in use — Squadron may fail to bind. Set SQUADRON_PORT to change.\n' "$PORT"
fi

say "Fetching the standalone compose into ./$DIR"
mkdir -p "$DIR"
if command -v curl >/dev/null 2>&1; then curl -fsSL "$RAW/deploy/docker-compose.yml" -o "$DIR/docker-compose.yml";
elif command -v wget >/dev/null 2>&1; then wget -qO "$DIR/docker-compose.yml" "$RAW/deploy/docker-compose.yml";
else die "Need curl or wget to download the compose file."; fi
ok "wrote $DIR/docker-compose.yml"

say "Starting Squadron (docker compose up -d)"
( cd "$DIR" && $DC up -d ) || die "compose up failed — see the output above."

say "Waiting for Squadron to become healthy"
i=0
while [ "$i" -lt 60 ]; do
  code=$(curl -s -m 3 -o /dev/null -w "%{http_code}" "http://localhost:$PORT/health" 2>/dev/null || echo 000)
  [ "$code" = "200" ] && break
  i=$((i+1)); sleep 2
done
[ "$code" = "200" ] || die "Squadron did not become healthy in time. Check: ( cd $DIR && $DC logs squadron )"

printf '\n\033[32mSquadron is up.\033[0m\n'
printf '  Dashboard:  http://localhost:%s/quickstart\n' "$PORT"
printf '  Stop:       ( cd %s && %s down )\n' "$DIR" "$DC"
printf '  AI assist:  add ANTHROPIC_API_KEY to %s/.env and re-run %s up -d\n\n' "$DIR" "$DC"
