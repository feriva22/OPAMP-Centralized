#!/usr/bin/env bash
set -Eeuo pipefail
umask 027

readonly SUPERVISOR_VERSION="${SUPERVISOR_VERSION:-0.159.0}"
readonly COLLECTOR_VERSION="${COLLECTOR_VERSION:-0.159.0}"
readonly INSTALL_ROOT="/opt/opamp"
readonly CONFIG_DIR="/etc/opamp"
readonly DATA_DIR="/var/lib/opamp"
readonly SERVICE_USER="opamp"
readonly SERVICE_FILE="/etc/systemd/system/opamp-supervisor.service"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: sudo bash ./install-agent.sh ws://SERVER_HOST:4320/v1/opamp

Installs the OpenTelemetry Collector Contrib and OpAMP Supervisor, writes
local configs if they do not already exist, prompts securely for an agent
token, and enables the systemd service.

Optional environment variables:
  SUPERVISOR_VERSION  OpAMP Supervisor release version (default: 0.159.0)
  COLLECTOR_VERSION   Collector Contrib release version (default: 0.159.0)
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

[[ $# -eq 1 ]] || {
  usage >&2
  exit 2
}

readonly OPAMP_ENDPOINT="$1"
if [[ ! "$OPAMP_ENDPOINT" =~ ^ws://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?/v1/opamp$ ]]; then
  fail "endpoint must be ws://host[:port]/v1/opamp for this plaintext staging server"
fi
if [[ "$OPAMP_ENDPOINT" =~ :([0-9]+)/v1/opamp$ ]]; then
  endpoint_port="${BASH_REMATCH[1]}"
  [[ "${#endpoint_port}" -le 5 ]] || fail "endpoint port must be between 1 and 65535"
  endpoint_port=$((10#${endpoint_port}))
  (( endpoint_port >= 1 && endpoint_port <= 65535 )) ||
    fail "endpoint port must be between 1 and 65535"
fi

if [[ "${EUID}" -ne 0 ]]; then
  fail "run this installer as root (for example: sudo bash ./install-agent.sh 'ws://10.0.0.10:4320/v1/opamp')"
fi

SUPERVISOR_CONFIG_CREATED=0
if [[ ! -e "/etc/opamp/supervisor.yaml" ]]; then
  if [[ -t 0 ]]; then
    read -r -s -p "Paste the agent token from the control-plane UI: " OPAMP_AGENT_TOKEN
    printf '\n'
  else
    read -r OPAMP_AGENT_TOKEN || fail "could not read the agent token from standard input"
  fi
  [[ "$OPAMP_AGENT_TOKEN" =~ ^[A-Za-z0-9_-]{43}$ ]] ||
    fail "agent token is invalid; create/copy a token from the control-plane UI"
fi

[[ -f /etc/os-release ]] || fail "cannot identify Linux distribution"
# shellcheck disable=SC1091
. /etc/os-release
[[ "${ID:-}" == "ubuntu" || "${ID:-}" == "debian" ]] ||
  fail "supported operating systems are Debian and Ubuntu; detected ${ID:-unknown}"

for command in curl tar sha256sum systemctl useradd getent install; do
  command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done
systemctl is-system-running >/dev/null 2>&1 || {
  state="$(systemctl is-system-running 2>/dev/null || true)"
  [[ "$state" == "degraded" ]] || fail "systemd is not running (state: ${state:-unknown})"
}

[[ "$SUPERVISOR_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "SUPERVISOR_VERSION must be a stable x.y.z version"
[[ "$COLLECTOR_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "COLLECTOR_VERSION must be a stable x.y.z version"

case "$(uname -m)" in
  x86_64|amd64) readonly ARCH="amd64" ;;
  aarch64|arm64) readonly ARCH="arm64" ;;
  *) fail "unsupported CPU architecture: $(uname -m); supported: x86_64 and aarch64" ;;
esac

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly TEMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf -- "$TEMP_DIR"
}
trap cleanup EXIT

download_verified() {
  local url="$1"
  local destination="$2"
  local checksum_file="${destination}.sha256"
  local expected_hash

  curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
    --retry 3 --output "$destination" "$url"
  curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
    --retry 3 --output "$checksum_file" "${url}.sha256"

  expected_hash="$(tr -d '[:space:]' < "$checksum_file")"
  [[ "$expected_hash" =~ ^[[:xdigit:]]{64}$ ]] ||
    fail "invalid SHA-256 file for ${url}"
  printf '%s  %s\n' "$expected_hash" "$destination" | sha256sum --check --status - ||
    fail "SHA-256 verification failed for ${url}"
}

collector_asset="otelcol-contrib_${COLLECTOR_VERSION}_linux_${ARCH}.tar.gz"
collector_url="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${COLLECTOR_VERSION}/${collector_asset}"
supervisor_asset="opampsupervisor_${SUPERVISOR_VERSION}_linux_${ARCH}"
supervisor_url="https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/cmd%2Fopampsupervisor%2Fv${SUPERVISOR_VERSION}/${supervisor_asset}"

printf 'Downloading OpenTelemetry Collector Contrib %s (%s)...\n' "$COLLECTOR_VERSION" "$ARCH"
download_verified "$collector_url" "${TEMP_DIR}/${collector_asset}"
tar -xzf "${TEMP_DIR}/${collector_asset}" -C "$TEMP_DIR" otelcol-contrib
[[ -x "${TEMP_DIR}/otelcol-contrib" ]] || fail "Collector binary missing from release archive"

printf 'Downloading OpAMP Supervisor %s (%s)...\n' "$SUPERVISOR_VERSION" "$ARCH"
download_verified "$supervisor_url" "${TEMP_DIR}/${supervisor_asset}"

if ! getent passwd "$SERVICE_USER" >/dev/null; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin --user-group "$SERVICE_USER"
fi

install -d -o root -g root -m 0755 "$INSTALL_ROOT" "$INSTALL_ROOT/bin" "$CONFIG_DIR"
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DATA_DIR" "$DATA_DIR/supervisor"
install -o root -g root -m 0755 "${TEMP_DIR}/otelcol-contrib" "${INSTALL_ROOT}/bin/otelcol-contrib"
install -o root -g root -m 0755 "${TEMP_DIR}/${supervisor_asset}" "${INSTALL_ROOT}/bin/opampsupervisor"

if [[ ! -e "${CONFIG_DIR}/collector.yaml" ]]; then
  install -o root -g "$SERVICE_USER" -m 0640 "${SCRIPT_DIR}/collector.yaml" "${CONFIG_DIR}/collector.yaml"
fi
if [[ ! -e "${CONFIG_DIR}/supervisor.yaml" ]]; then
  escaped_endpoint="${OPAMP_ENDPOINT//&/\\&}"
  escaped_endpoint="${escaped_endpoint//|/\\|}"
  escaped_endpoint="${escaped_endpoint//\\/\\\\}"
  sed "s|__OPAMP_ENDPOINT__|${escaped_endpoint}|;s|__OPAMP_AGENT_TOKEN__|${OPAMP_AGENT_TOKEN}|" "${SCRIPT_DIR}/supervisor.yaml.in" \
    | install -o root -g "$SERVICE_USER" -m 0640 /dev/stdin "${CONFIG_DIR}/supervisor.yaml"
  SUPERVISOR_CONFIG_CREATED=1
fi

"${INSTALL_ROOT}/bin/otelcol-contrib" validate --config="${CONFIG_DIR}/collector.yaml"

cat > "$SERVICE_FILE" <<'EOF'
[Unit]
Description=OpenTelemetry OpAMP Supervisor
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=opamp
Group=opamp
ExecStart=/opt/opamp/bin/opampsupervisor --config=/etc/opamp/supervisor.yaml
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
Environment=HOME=/var/lib/opamp
WorkingDirectory=/var/lib/opamp
StateDirectory=opamp
StateDirectoryMode=0750
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ReadWritePaths=/var/lib/opamp

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$SERVICE_FILE"

systemctl daemon-reload
systemctl enable --now opamp-supervisor.service

printf '\nInstalled and started opamp-supervisor.service.\n'
printf 'Status:  systemctl status opamp-supervisor\n'
printf 'Logs:    journalctl -u opamp-supervisor -f\n'
printf 'Config:  %s/supervisor.yaml and %s/collector.yaml\n' "$CONFIG_DIR" "$CONFIG_DIR"
printf '\nWARNING: OpAMP uses unencrypted ws://. The bearer token and telemetry can be intercepted on the network.\n'
printf 'Use only on a trusted, isolated network; do not send sensitive telemetry over this connection.\n'
if [[ "$SUPERVISOR_CONFIG_CREATED" -eq 0 ]]; then
  printf 'Existing Supervisor config was preserved. If rotating a token, update its Authorization header and restart the service.\n'
fi
