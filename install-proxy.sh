#!/usr/bin/env bash
set -euo pipefail

umask 077

MODE="install"
DOMAIN=""
EMAIL="admin@example.com"
FORCE=false

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    install|update|repair) MODE="$1"; shift ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --email) EMAIL="$2"; shift 2 ;;
    --force) FORCE=true; shift ;;
    *) echo "Unknown parameter: $1" >&2; exit 1 ;;
  esac
done

HY2_PORT="${HY2_PORT:-443}"
NODE_NAME="${NODE_NAME:-AWS-HY2}"
SNI="${SNI:-www.bing.com}"

# Standard Directories
BASE_DIR="/etc/singbox-sub-manager"
CONFIG_ENV="$BASE_DIR/config.env"
NODES_CONF="$BASE_DIR/nodes.conf"
CERTS_DIR="$BASE_DIR/certs"
STATE_DIR="/var/lib/singbox-sub-manager"
TOKEN_FILE="$STATE_DIR/token"
MIGRATION_FILE="$STATE_DIR/migration-v0.2.done"
LOG_DIR="/var/log/singbox-sub-manager"
LOG_FILE="$LOG_DIR/installer.log"
SUB_ROOT="/var/www/proxy-sub"
LOCK_FILE="/run/lock/singbox-sub-manager.lock"

# Not fully used in v0.2.0, pre-created for v0.3.0
TEMPLATE_DIR="/usr/share/singbox-sub-manager/templates"
EXAMPLES_DIR="/usr/share/singbox-sub-manager/examples"

mkdir -p "$BASE_DIR" "$CERTS_DIR" "$STATE_DIR" "$LOG_DIR" "$SUB_ROOT" "$TEMPLATE_DIR" "$EXAMPLES_DIR"

exec 9> "$LOCK_FILE"
if ! flock -n 9; then
  echo "Another installation is in progress. Please wait." >&2
  exit 1
fi

log() {
  local msg="$1"
  echo -e "\n>>> $msg"
  echo -e ">>> $msg" >> "$LOG_FILE"
}

log_warn() {
  local msg="$1"
  echo -e "WARN: $msg" >&2
  echo -e "WARN: $msg" >> "$LOG_FILE"
}

log_error() {
  local msg="$1"
  echo -e "ERROR: $msg" >&2
  echo -e "ERROR: $msg" >> "$LOG_FILE"
}

die() {
  log_error "$1"
  exit 1
}

# 1. OS & Root Check
[[ $(id -u) -eq 0 ]] || die "Please run as root (sudo)"
if [[ -f /etc/os-release ]]; then
  # shellcheck disable=SC1091
  source /etc/os-release
  OS="${ID:-}"
  VER="${VERSION_ID:-}"
  if [[ "$OS" == "ubuntu" ]]; then
    if [[ "$VER" != "22.04" && "$VER" != "24.04" ]]; then
      die "Only Ubuntu 22.04 and 24.04 are supported"
    fi
  elif [[ "$OS" == "debian" ]]; then
    if [[ "$VER" != "12" ]]; then
      die "Only Debian 12 is supported"
    fi
  else
    die "Only Ubuntu and Debian are supported"
  fi
else
  die "Could not determine OS"
fi

arch(){ case "$(uname -m)" in x86_64|amd64) echo amd64;; aarch64|arm64) echo arm64;; *) die "Unsupported architecture";; esac; }

# 2. Disk Check
check_disk() {
  local mnt="$1"
  local avail
  avail=$(df -m "$mnt" | awk 'NR==2 {print $4}')
  if [[ "$avail" -lt 512 ]]; then
    die "Insufficient disk space on $mnt. At least 512MB required."
  fi
}
check_disk "/"
check_disk "/tmp"

# 3. Port Check
log "Checking ports"
check_tcp_port() {
  local port="$1"
  local allowed_proc="$2"
  if ss -lntp | grep -qE "[:.]$port[[:space:]]"; then
    local pids
    pids=$(ss -lntp | grep -E "[:.]$port[[:space:]]" | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u)
    for pid in $pids; do
      local exe
      exe=$(readlink -f /proc/"$pid"/exe 2>/dev/null || echo "unknown")
      if [[ "$exe" != *"$allowed_proc"* ]]; then
         die "TCP $port is occupied by another process: PID $pid ($exe)"
      fi
    done
  fi
}
check_udp_port() {
  local port="$1"
  local allowed_proc="$2"
  if ss -lnup | grep -qE "[:.]$port[[:space:]]"; then
    local pids
    pids=$(ss -lnup | grep -E "[:.]$port[[:space:]]" | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u)
    for pid in $pids; do
      local exe
      exe=$(readlink -f /proc/"$pid"/exe 2>/dev/null || echo "unknown")
      if [[ "$exe" != *"$allowed_proc"* ]]; then
         die "UDP $port is occupied by another process: PID $pid ($exe)"
      fi
    done
  fi
}

check_tcp_port 80 "caddy"
check_tcp_port 443 "caddy"
check_udp_port 443 "sing-box"

# 4. Old Config Migration
if [[ ! -f "$MIGRATION_FILE" ]]; then
  log "Checking migrations"
  migrated=false
  if [[ ! -f "$CONFIG_ENV" && -f "/etc/proxy-state/hy2-secret.env" ]]; then
    log "Migrating old hy2-secret.env to $CONFIG_ENV"
    cp -p "/etc/proxy-state/hy2-secret.env" "$CONFIG_ENV"
    chmod 600 "$CONFIG_ENV"
    migrated=true
  fi
  if [[ ! -f "$TOKEN_FILE" && -f "/etc/proxy-sub-token" ]]; then
    log "Migrating old proxy-sub-token to $TOKEN_FILE"
    cp -p "/etc/proxy-sub-token" "$TOKEN_FILE"
    chmod 600 "$TOKEN_FILE"
    migrated=true
  fi
  if [ "$migrated" = true ]; then
    date > "$MIGRATION_FILE"
  fi
fi

# 5. Load or Generate Secrets
log "Setting up secrets"
if [[ -f "$CONFIG_ENV" ]]; then
  # shellcheck disable=SC1090
  source "$CONFIG_ENV"
  if [[ -z "${PASSWORD:-}" || -z "${OBFS_PASSWORD:-}" ]]; then
    die "Invalid config.env: missing PASSWORD or OBFS_PASSWORD. Please repair or remove it."
  fi
else
  PASSWORD="$(openssl rand -hex 24)"
  OBFS_PASSWORD="$(openssl rand -hex 24)"
  printf 'PASSWORD=%q\nOBFS_PASSWORD=%q\n' "$PASSWORD" "$OBFS_PASSWORD" > "$CONFIG_ENV"
  chmod 600 "$CONFIG_ENV"
fi

if [[ -f "$TOKEN_FILE" ]]; then
  TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
  if [[ -z "$TOKEN" ]]; then
    die "Invalid token file: empty. Please repair or remove it."
  fi
else
  TOKEN="$(openssl rand -hex 16)"
  printf '%s\n' "$TOKEN" > "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
fi

OUT="$SUB_ROOT/$TOKEN"
mkdir -p "$OUT"

# 6. Install Dependencies
log "Installing dependencies"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y > /dev/null
apt-get install -y curl wget tar openssl ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https qrencode > /dev/null

log "Installing sing-box"
SB_VERSION="$(curl -fsSL https://api.github.com/repos/SagerNet/sing-box/releases/latest | sed -n 's/.*"tag_name": "v\([^"]*\)".*/\1/p' | head -1)"
[[ -n "$SB_VERSION" ]] || die "Failed to get sing-box version"
TMP="$(mktemp -d "/tmp/sb.tmp.XXXXXX")"; trap 'rm -rf "$TMP"' EXIT
wget -qO "$TMP/sb.tgz" "https://github.com/SagerNet/sing-box/releases/download/v${SB_VERSION}/sing-box-${SB_VERSION}-linux-$(arch).tar.gz"
tar -xzf "$TMP/sb.tgz" -C "$TMP"
install -m 0755 "$TMP/sing-box-${SB_VERSION}-linux-$(arch)/sing-box" /usr/local/bin/sing-box

log "Installing Caddy"
if ! command -v caddy >/dev/null 2>&1; then
  install -d -m 0755 /usr/share/keyrings
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -y > /dev/null
  apt-get install -y caddy > /dev/null
fi

# 7. Generate Hysteria2 Certificates
log "Checking Hysteria2 certificates"
if [[ ! -f "$CERTS_DIR/server.crt" || ! -f "$CERTS_DIR/server.key" ]]; then
  log "Generating new self-signed certificate"
  openssl req -x509 -newkey rsa:2048 -nodes -days 365 -subj "/CN=$SNI" -keyout "$CERTS_DIR/server.key" -out "$CERTS_DIR/server.crt" >/dev/null 2>&1
else
  # Verify if cert and key match
  CERT_PUB="$(openssl x509 -in "$CERTS_DIR/server.crt" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform der 2>/dev/null | sha256sum)"
  KEY_PUB="$(openssl pkey -in "$CERTS_DIR/server.key" -pubout -outform der 2>/dev/null | sha256sum)"
  if [[ -z "$CERT_PUB" || "$CERT_PUB" != "$KEY_PUB" ]]; then
    die "Certificate server.crt and server.key do not match or are corrupted. Please repair."
  fi
  if ! openssl x509 -checkend 86400 -noout -in "$CERTS_DIR/server.crt" >/dev/null 2>&1; then
    log_warn "Certificate is about to expire or has expired. Consider renewing."
  fi
fi

# 8. Render and check sing-box config
log "Configuring sing-box"
CONF_DIR="/etc/sing-box"
mkdir -p "$CONF_DIR"

SB_TMP="$(mktemp "$CONF_DIR/.config.json.tmp.XXXXXX")"
cat > "$SB_TMP" <<EOF
{
  "log": {"level":"info","timestamp":true},
  "inbounds": [{
    "type":"hysteria2","tag":"hy2-in","listen":"::","listen_port":$HY2_PORT,
    "users":[{"password":"$PASSWORD"}],
    "obfs":{"type":"salamander","password":"$OBFS_PASSWORD"},
    "tls":{"enabled":true,"server_name":"$SNI","certificate_path":"$CERTS_DIR/server.crt","key_path":"$CERTS_DIR/server.key"}
  }],
  "outbounds": [{"type":"direct","tag":"direct"}]
}
EOF

if ! sing-box check -c "$SB_TMP"; then
  rm -f "$SB_TMP"
  die "sing-box check failed."
fi

TS="$(date +%Y%m%d%H%M%S)"
if [[ -f "$CONF_DIR/config.json" ]]; then
  cp -p "$CONF_DIR/config.json" "$CONF_DIR/config.json.previous.$TS"
fi
mv "$SB_TMP" "$CONF_DIR/config.json"

cat > /etc/systemd/system/sing-box.service <<EOF
[Unit]
Description=sing-box Hysteria2
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/sing-box run -c $CONF_DIR/config.json
Restart=on-failure
RestartSec=5
LimitNOFILE=infinity
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now sing-box
systemctl restart sing-box

# Check sing-box health
sleep 1
if ! systemctl is-active --quiet sing-box || ! ss -lnup | grep -qE "[:.]$HY2_PORT[[:space:]]"; then
  log_error "sing-box failed to start or bind port, restoring previous config"
  if [[ -f "$CONF_DIR/config.json.previous.$TS" ]]; then
    mv "$CONF_DIR/config.json.previous.$TS" "$CONF_DIR/config.json"
    systemctl restart sing-box || true
    sleep 1
    if ! systemctl is-active --quiet sing-box; then
      die "sing-box health check failed even after rollback."
    fi
  fi
  die "sing-box health check failed."
fi

# 9. Configure Nodes
PUBLIC_IP="$(curl -fsS4 --max-time 10 https://api.ipify.org || true)"
[[ -n "$PUBLIC_IP" ]] || die "Failed to retrieve public IPv4"

log "Configuring nodes"
if [[ ! -f "$NODES_CONF" ]]; then
  cat > "$NODES_CONF" <<EOF
# schema-version: 1
# managed-by: installer
$NODE_NAME|$PUBLIC_IP|$HY2_PORT|$PASSWORD|$OBFS_PASSWORD|$SNI
EOF
else
  # Update existing managed node or append
  if grep -q "# managed-by: installer" "$NODES_CONF"; then
    # Modify the first line after managed-by: installer, or simplistic approach: replace it
    # We will let Go proxyctl handle updates more cleanly, but for now we won't overwrite blindly
    log "nodes.conf exists. Checking if IP needs update..."
    if ! grep -q "|$PUBLIC_IP|" "$NODES_CONF"; then
       log_warn "Public IP changed to $PUBLIC_IP but we only append/update managed nodes in Go tool (planned)."
       sed -i "s/^$NODE_NAME|.*/$NODE_NAME|$PUBLIC_IP|$HY2_PORT|$PASSWORD|$OBFS_PASSWORD|$SNI/" "$NODES_CONF"
    fi
  fi
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if command -v proxyctl >/dev/null 2>&1; then
  proxyctl merge --nodes "$NODES_CONF" --output "$OUT"
elif [[ -x "$ROOT/bin/proxyctl-linux-$(arch)" ]]; then
  "$ROOT/bin/proxyctl-linux-$(arch)" merge --nodes "$NODES_CONF" --output "$OUT"
elif command -v go >/dev/null 2>&1 && [[ -f "$ROOT/go.mod" ]]; then
  (cd "$ROOT" && go run ./cmd/proxyctl merge --nodes "$NODES_CONF" --output "$OUT")
else
  die "proxyctl not found. Please use release package or install Go to build."
fi

# 10. Configure Caddy
log "Configuring Caddy"
CADDY_TMP="$(mktemp "/etc/caddy/.Caddyfile.tmp.XXXXXX")"
cat > "$CADDY_TMP" <<EOF
{
  email $EMAIL
  http_port 80
  servers {
    protocols h1 h2
  }
}
$DOMAIN {
  root * $SUB_ROOT
  file_server
}
EOF

caddy fmt --overwrite "$CADDY_TMP" >/dev/null
if ! caddy validate --config "$CADDY_TMP" >/dev/null 2>&1; then
  rm -f "$CADDY_TMP"
  die "caddy validate failed."
fi

if [[ -f "/etc/caddy/Caddyfile" ]]; then
  cp -p "/etc/caddy/Caddyfile" "/etc/caddy/Caddyfile.previous.$TS"
fi
mv "$CADDY_TMP" "/etc/caddy/Caddyfile"

if systemctl is-active --quiet caddy; then
  if ! caddy reload --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
    log_error "caddy reload failed, restoring previous config"
    if [[ -f "/etc/caddy/Caddyfile.previous.$TS" ]]; then
      mv "/etc/caddy/Caddyfile.previous.$TS" "/etc/caddy/Caddyfile"
      caddy reload --config /etc/caddy/Caddyfile >/dev/null 2>&1 || true
      if ! systemctl is-active --quiet caddy; then
        die "caddy health check failed even after rollback."
      fi
    fi
    die "Failed to reload caddy with new config."
  fi
else
  systemctl enable --now caddy
fi

chown -R caddy:caddy "$SUB_ROOT" 2>/dev/null || true

# Check external & local access
sleep 1
if ! curl -sf --resolve "$DOMAIN:443:127.0.0.1" --connect-timeout 5 "https://$DOMAIN/$TOKEN/clash.yaml" >/dev/null; then
  log_warn "Local curl check to Caddy failed. Check Caddy logs."
fi

if ! curl -sf --connect-timeout 10 "https://$DOMAIN/$TOKEN/clash.yaml" >/dev/null; then
  log_warn "External curl check to Caddy failed. This may be due to DNS propagation."
fi

# 11. Sysctl
cat > /etc/sysctl.d/99-proxy-installer.conf <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
EOF
sysctl --system >/dev/null 2>&1 || true

# Masked Token for log
MASKED_TOKEN="${TOKEN:0:4}****${TOKEN: -4}"

echo
echo "Deployment successful"
echo "FlClash:      https://$DOMAIN/$TOKEN/clash.yaml"
echo "Shadowrocket: https://$DOMAIN/$TOKEN/sr.txt"
echo "Note: AWS Security Groups require TCP 80/443 and UDP 443 to be open."
echo "Token (masked for log): $MASKED_TOKEN"

trap - EXIT
rm -rf "$TMP"
