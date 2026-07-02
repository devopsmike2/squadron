#!/usr/bin/env bash
# Squadron on-prem agent onboarding — one command to turn a bare Linux server
# into a Squadron-managed OTel agent that reports its config, host metrics, and logs.
#
#   curl -fsSL https://raw.githubusercontent.com/devopsmike2/squadron/main/deploy/agent/install-agent.sh \
#     | sudo bash -s -- --squadron-host 10.0.0.5
#
# Or with a pinned collector version / custom service name:
#   sudo ./install-agent.sh --squadron-host squadron.internal --service-name web-01 --otelcol-version 0.111.0
#
# Idempotent: safe to re-run (re-renders config + restarts). Installs the
# OpenTelemetry Collector Contrib and runs it as root (host agents need to read
# /var/log and per-process metrics).
set -euo pipefail

SQUADRON_HOST=""
SERVICE_NAME="onprem-server"
INSTANCE_ID=""
OTELCOL_VERSION="0.111.0"
CONFIG_URL="https://raw.githubusercontent.com/devopsmike2/squadron/main/deploy/agent/otelcol-squadron.yaml.tmpl"

while [ $# -gt 0 ]; do
  case "$1" in
    --squadron-host) SQUADRON_HOST="$2"; shift 2;;
    --service-name)  SERVICE_NAME="$2"; shift 2;;
    --instance-id)   INSTANCE_ID="$2"; shift 2;;
    --otelcol-version) OTELCOL_VERSION="$2"; shift 2;;
    --config-url)    CONFIG_URL="$2"; shift 2;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

[ -n "$SQUADRON_HOST" ] || { echo "ERROR: --squadron-host is required (Squadron's host/IP reachable from this server)" >&2; exit 2; }
[ "$(id -u)" = "0" ] || { echo "ERROR: run as root (sudo)" >&2; exit 2; }
[ -n "$INSTANCE_ID" ] || INSTANCE_ID="$(hostname)"

# ---- detect arch + package format --------------------------------------------
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64;;
  aarch64|arm64) ARCH=arm64;;
  *) echo "ERROR: unsupported arch $(uname -m)" >&2; exit 1;;
esac
REL="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${OTELCOL_VERSION}"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

echo "==> installing otelcol-contrib v${OTELCOL_VERSION} (${ARCH})"
if command -v dpkg >/dev/null 2>&1; then
  curl -fsSLo "$tmp/otelcol.deb" "${REL}/otelcol-contrib_${OTELCOL_VERSION}_linux_${ARCH}.deb"
  dpkg -i "$tmp/otelcol.deb" || apt-get -f install -y
elif command -v rpm >/dev/null 2>&1; then
  RPMARCH=$([ "$ARCH" = amd64 ] && echo x86_64 || echo aarch64)
  curl -fsSLo "$tmp/otelcol.rpm" "${REL}/otelcol-contrib_${OTELCOL_VERSION}_linux_${RPMARCH}.rpm"
  rpm -Uvh --replacepkgs "$tmp/otelcol.rpm"
else
  echo "ERROR: no dpkg or rpm found; unsupported distro" >&2; exit 1
fi

# ---- render config -----------------------------------------------------------
echo "==> writing /etc/otelcol-contrib/config.yaml (Squadron: ${SQUADRON_HOST})"
mkdir -p /etc/otelcol-contrib
curl -fsSLo "$tmp/config.tmpl" "$CONFIG_URL"
sed -e "s|__SQUADRON_HOST__|${SQUADRON_HOST}|g" \
    -e "s|__SERVICE_NAME__|${SERVICE_NAME}|g" \
    -e "s|__INSTANCE_ID__|${INSTANCE_ID}|g" \
    "$tmp/config.tmpl" > /etc/otelcol-contrib/config.yaml

# ---- run as root (host agent needs /var/log + per-process metrics) ------------
mkdir -p /etc/systemd/system/otelcol-contrib.service.d
cat > /etc/systemd/system/otelcol-contrib.service.d/10-root.conf <<'UNIT'
[Service]
User=root
Group=root
UNIT

systemctl daemon-reload
systemctl enable otelcol-contrib >/dev/null 2>&1 || true
systemctl restart otelcol-contrib

sleep 2
if systemctl is-active --quiet otelcol-contrib; then
  echo "==> otelcol-contrib is running. This server should appear in Squadron's Fleet within ~30s."
  echo "    Squadron UI: http://${SQUADRON_HOST}:8080/agents  (name: ${SERVICE_NAME}, instance: ${INSTANCE_ID})"
else
  echo "ERROR: otelcol-contrib failed to start. Recent logs:" >&2
  journalctl -u otelcol-contrib --no-pager -n 30 >&2 || true
  exit 1
fi
