#!/usr/bin/env bash
# HA S5 proof orchestrator (ADR 0035). Two enterprise instances, one Postgres.
#
# Proves: (1) leader election — exactly one runner per singleton; (2) failover —
# the survivor takes over with no dual ownership; (3) cross-instance config
# delivery — a rollout driven on the leader reaches an agent on the non-leader
# via the S3a reconcile loop and converges (S3d).
#
# Run from this directory in a NORMAL shell. Not idempotent-safe against a
# pre-existing Squadron on the same ports — the ports below are deliberately
# high/uncommon to avoid a local dev instance.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OSS="$(cd "$HERE/../.." && pwd)"
BIN="${BIN:-$OSS/../squadron-enterprise/bin/squadron-enterprise}"
DATA=/tmp/ha-proof
LOGS=$DATA/logs
PSQL="docker exec ha-proof-postgres psql -U squadron -d squadron -tA"

pg()   { docker exec ha-proof-postgres psql -U squadron -d squadron -tA -c "$1"; }
locks(){ pg "SELECT granted,count(*) FROM pg_locks WHERE locktype='advisory' GROUP BY granted ORDER BY granted DESC;" | tr '\n' ' '; }
acq()  { grep -c 'acquired leadership' "$1" 2>/dev/null || echo 0; }

down() {
  echo "== teardown =="
  pkill -9 -f 'squadron-enterprise --config' 2>/dev/null
  pkill -9 -f 'ha-proof/agentsim' 2>/dev/null
  docker rm -f ha-proof-agent >/dev/null 2>&1
  docker compose -f "$HERE/docker-compose.postgres.yml" down -v >/dev/null 2>&1
  echo "done."
}
[ "${1:-}" = "--down" ] && { down; exit 0; }

[ -x "$BIN" ] || { echo "enterprise binary not found at $BIN — build it first (see README)"; exit 1; }
mkdir -p "$DATA/a" "$DATA/b" "$LOGS"
export SQUADRON_SECRETS_KEY="$(head -c 32 /dev/urandom | base64)"
export SQUADRON_DEPLOY_KEY="$(head -c 32 /dev/urandom | base64)"
export SQUADRON_USAGE_ENABLED=true SQUADRON_USAGE_ENDPOINT="http://127.0.0.1:59999/usage"
export SQUADRON_DISCOVERY_SCAN_INTERVAL=6h

echo "== [0] Postgres =="
docker compose -f "$HERE/docker-compose.postgres.yml" up -d
for i in $(seq 1 30); do docker exec ha-proof-postgres pg_isready -U squadron -d squadron >/dev/null 2>&1 && break; sleep 1; done

echo "== [1] start instance A, wait for leadership =="
( cd "$DATA/a" && nohup "$BIN" --config "$HERE/instance-a.yaml" >"$LOGS/a.log" 2>&1 & echo $! >"$DATA/a.pid"; disown )
for i in $(seq 1 20); do [ "$(acq "$LOGS/a.log")" = 8 ] && break; sleep 1; done
echo "   A acquired singletons: $(acq "$LOGS/a.log") (expect 8)"

echo "== [2] start instance B (must acquire 0 — A holds all locks) =="
( cd "$DATA/b" && nohup "$BIN" --config "$HERE/instance-b.yaml" >"$LOGS/b.log" 2>&1 & echo $! >"$DATA/b.pid"; disown )
sleep 12
echo "   B acquired singletons: $(acq "$LOGS/b.log") (expect 0)"
echo "   pg_locks (expect 't|8 f|8'): $(locks)"

echo "== [3] FAILOVER: kill leader A =="
kill -9 "$(cat "$DATA/a.pid")" 2>/dev/null
for i in $(seq 1 20); do [ "$(acq "$LOGS/b.log")" = 8 ] && break; sleep 1; done
echo "   B acquired after failover: $(acq "$LOGS/b.log") (expect 8)"
echo "   pg_locks (expect 't|8'): $(locks)"

echo "== [4] restart A as NON-leader; connect agentsim to it; drive a rollout =="
( cd "$DATA/a" && nohup "$BIN" --config "$HERE/instance-a.yaml" >"$LOGS/a.log" 2>&1 & echo $! >"$DATA/a.pid"; disown )
sleep 10
go build -o "$DATA/agentsim" "$HERE/agentsim" || { echo "agentsim build failed"; exit 1; }
( cd "$DATA" && nohup "$DATA/agentsim" -target ws://127.0.0.1:14320/v1/opamp -tenant default -group ha-proof-group >"$LOGS/agentsim.log" 2>&1 & echo $! >"$DATA/agentsim.pid"; disown )
sleep 6
RID=$(python3 "$HERE/drive-rollout.py" --api http://127.0.0.1:18081 | sed -n 's/^ROLLOUT_ID=//p')
echo "   rollout: $RID (engine runs on leader B; agent is on non-leader A)"
for i in $(seq 1 15); do
  st=$(curl -s "http://127.0.0.1:18081/api/v1/rollouts/$RID" | python3 -c "import sys,json;print(json.load(sys.stdin).get('state'))" 2>/dev/null)
  echo "   rollout[$st]"; echo "$st" | grep -qE 'succeeded|rolled_back|aborted' && break; sleep 4
done
echo "   leader push-gap log:"; grep "$RID" "$LOGS/b.log" | grep -o 'stage direct-push failed[^"]*' | head -1
echo "   agent drift: $(curl -s http://127.0.0.1:18081/api/v1/agents | python3 -c "import sys,json;print(json.load(sys.stdin)['items'][0]['drift_status'])" 2>/dev/null)"

echo
echo "== DONE.  './run-proof.sh --down' to tear down. =="
