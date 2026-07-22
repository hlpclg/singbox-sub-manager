#!/usr/bin/env bash
set -euo pipefail

umask 077

DOMAIN=""
EMAIL="admin@example.com"
EMAIL_EXPLICIT=false

usage() {
  cat >&2 <<'EOF'
Usage:
  sudo ./install-proxy.sh <domain> [email]
  sudo ./install-proxy.sh --domain <domain> [--email <email>]

Example:
  sudo ./install-proxy.sh sub.example.com admin@example.com
EOF
}

POSITIONAL=()

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    install) shift ;;
    --domain)
      [[ $# -ge 2 && "$2" != --* ]] || { echo "Missing value for --domain" >&2; usage; exit 1; }
      DOMAIN="$2"
      shift 2
      ;;
    --email)
      [[ $# -ge 2 && "$2" != --* ]] || { echo "Missing value for --email" >&2; usage; exit 1; }
      EMAIL="$2"
      EMAIL_EXPLICIT=true
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "Unknown parameter: $1" >&2; usage; exit 1 ;;
    *) POSITIONAL+=("$1"); shift ;;
  esac
done

if [[ ${#POSITIONAL[@]} -gt 2 ]]; then
  echo "Too many positional parameters: ${POSITIONAL[*]}" >&2
  usage
  exit 1
fi

if [[ ${#POSITIONAL[@]} -ge 1 ]]; then
  if [[ -n "$DOMAIN" ]]; then
    echo "Domain provided both positionally and with --domain" >&2
    usage
    exit 1
  fi
  DOMAIN="${POSITIONAL[0]}"
fi

if [[ ${#POSITIONAL[@]} -ge 2 ]]; then
  if [[ "$EMAIL_EXPLICIT" == true ]]; then
    echo "Email provided both positionally and with --email" >&2
    usage
    exit 1
  fi
  EMAIL="${POSITIONAL[1]}"
fi

if [[ -z "$DOMAIN" ]]; then
  echo "Missing required domain" >&2
  usage
  exit 1
fi

if [[ "$DOMAIN" =~ :// || "$DOMAIN" == */* || "$DOMAIN" == *:* || "$DOMAIN" == *[[:space:]]* || "$DOMAIN" != *.* ]]; then
  echo "Domain must be a hostname only, for example: sub.example.com" >&2
  exit 1
fi

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
CADDY_KEYRING="/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
CADDY_APT_LIST="/etc/apt/sources.list.d/caddy-stable.list"
CADDY_GPG_URL="https://dl.cloudsmith.io/public/caddy/stable/gpg.key"
CADDY_APT_URL="https://dl.cloudsmith.io/public/caddy/stable/deb/debian"
PROXYCTL_BIN="/usr/local/bin/proxyctl"
PROXYCTL_REPOSITORY="${PROXYCTL_REPOSITORY:-hlpclg/singbox-sub-manager}"
PROXYCTL_VERSION="${PROXYCTL_VERSION:-v0.2.1}"

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

apt_update_or_die() {
  local err
  err="$(mktemp "/tmp/apt-update.err.XXXXXX")"
  if apt-get update -y > /dev/null 2>"$err"; then
    rm -f "$err"
    return
  fi

  if grep -qE 'NO_PUBKEY|EXPKEYSIG|KEYEXPIRED|not signed' "$err"; then
    log_error "apt-get update failed because an APT repository signature could not be verified."
  else
    log_error "apt-get update failed."
  fi
  sed 's/^/apt: /' "$err" >&2
  sed 's/^/apt: /' "$err" >> "$LOG_FILE"
  rm -f "$err"
  exit 1
}

reset_caddy_apt_source() {
  install -d -m 0755 /etc/apt/sources.list.d /usr/share/keyrings
  rm -f "$CADDY_APT_LIST" "$CADDY_KEYRING"
}

install_caddy_apt_source() {
  local source_file
  local expected_source="deb [signed-by=$CADDY_KEYRING] $CADDY_APT_URL any-version main"

  while IFS= read -r -d '' source_file; do
    if [[ "$source_file" != "$CADDY_APT_LIST" ]] && grep -q 'dl.cloudsmith.io/public/caddy/stable' "$source_file" 2>/dev/null; then
      die "Conflicting Caddy APT source found: $source_file. Remove it or merge it into $CADDY_APT_LIST."
    fi
  done < <(find /etc/apt/sources.list.d -maxdepth 1 -type f \( -name '*.list' -o -name '*.sources' \) -print0 2>/dev/null)

  if [[ -f "$CADDY_KEYRING" && -f "$CADDY_APT_LIST" ]] && grep -Fxq "$expected_source" "$CADDY_APT_LIST"; then
    return
  fi

  reset_caddy_apt_source

  if ! curl -fsSL "$CADDY_GPG_URL" | gpg --dearmor --yes -o "$CADDY_KEYRING"; then
    rm -f "$CADDY_KEYRING"
    die "Failed to install Caddy APT signing key"
  fi
  chmod 0644 "$CADDY_KEYRING"

  cat > "$CADDY_APT_LIST" <<EOF
$expected_source
EOF
  chmod 0644 "$CADDY_APT_LIST"
}

install_proxyctl_binary() {
  local cli_arch="$1"
  local base_url="https://github.com/$PROXYCTL_REPOSITORY/releases/download/$PROXYCTL_VERSION"
  local binary_name="proxyctl-linux-$cli_arch"
  local tmp_dir expected actual

  tmp_dir="$(mktemp -d /tmp/proxyctl.XXXXXX)"
  log "Downloading proxyctl $PROXYCTL_VERSION for linux-$cli_arch"
  if ! curl -fsSL "$base_url/$binary_name" -o "$tmp_dir/$binary_name" || ! curl -fsSL "$base_url/checksums.txt" -o "$tmp_dir/checksums.txt"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  expected="$(awk -v file="$binary_name" '$2 == file {print $1}' "$tmp_dir/checksums.txt")"
  actual="$(sha256sum "$tmp_dir/$binary_name" | awk '{print $1}')"
  if [[ -z "$expected" || "$expected" != "$actual" ]]; then
    rm -rf "$tmp_dir"
    return 1
  fi
  install -m 0755 "$tmp_dir/$binary_name" "$PROXYCTL_BIN"
  rm -rf "$tmp_dir"
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

yaml_quote() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

url_encode_component() {
  local value="$1"
  local i char hex
  local LC_ALL=C

  for ((i = 0; i < ${#value}; i++)); do
    char="${value:i:1}"
    case "$char" in
      [A-Za-z0-9.~_-]) printf '%s' "$char" ;;
      *) printf -v hex '%02X' "'$char"; printf '%%%s' "$hex" ;;
    esac
  done
}

validate_node_fields() {
  local line_no="$1"
  local name="$2"
  local server="$3"
  local port="$4"
  local password="$5"
  local obfs_password="$6"
  local sni="$7"

  if [[ -z "$name" || -z "$server" || -z "$port" || -z "$password" || -z "$obfs_password" || -z "$sni" ]]; then
    die "nodes.conf line $line_no: empty field"
  fi
  if [[ ! "$port" =~ ^[0-9]+$ || "$port" -lt 1 || "$port" -gt 65535 ]]; then
    die "nodes.conf line $line_no: invalid port"
  fi
  if [[ "$password" == *CHANGE_ME* || "$obfs_password" == *CHANGE_ME* ]]; then
    die "nodes.conf line $line_no: replace placeholder secrets"
  fi
}

write_subscriptions_with_shell() {
  local nodes_file="$1"
  local output_dir="$2"
  local clash_tmp sr_tmp line line_no
  local name server port password obfs_password sni extra
  local node_count=0 seen_nodes=$'\n'

  mkdir -p "$output_dir"
  clash_tmp="$(mktemp "$output_dir/.clash.yaml.tmp.XXXXXX")"
  sr_tmp="$(mktemp "$output_dir/.sr.txt.tmp.XXXXXX")"

  cat > "$clash_tmp" <<'EOF'
mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
ipv6: false

tun:
  enable: true
  stack: mixed
  auto-route: true
  auto-redirect: true
  strict-route: true
  mtu: 1400
  dns-hijack:
    - any:53

dns:
  enable: true
  ipv6: false
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  nameserver:
    - https://dns.alidns.com/dns-query
    - https://doh.pub/dns-query
  fallback:
    - https://1.1.1.1/dns-query
    - https://8.8.8.8/dns-query
  proxy-server-nameserver:
    - https://1.1.1.1/dns-query
    - https://8.8.8.8/dns-query
  fake-ip-filter:
    - "*.lan"
    - "*.local"
    - "*.msftconnecttest.com"
    - "*.msftncsi.com"

proxies:
EOF

  line_no=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line_no=$((line_no + 1))
    line="$(trim "$line")"
    [[ -z "$line" || "$line" == \#* ]] && continue

    IFS='|' read -r name server port password obfs_password sni extra <<< "$line"
    if [[ -n "${extra:-}" ]]; then
      die "nodes.conf line $line_no: expected 6 fields"
    fi
    name="$(trim "$name")"
    server="$(trim "$server")"
    port="$(trim "$port")"
    password="$(trim "$password")"
    obfs_password="$(trim "$obfs_password")"
    sni="$(trim "$sni")"
    validate_node_fields "$line_no" "$name" "$server" "$port" "$password" "$obfs_password" "$sni"
    if [[ "$seen_nodes" == *$'\n'"$name"$'\n'* ]]; then
      die "nodes.conf line $line_no: duplicate node name $name"
    fi
    seen_nodes+="$name"$'\n'
    node_count=$((node_count + 1))

    {
      printf '  - name: %s\n' "$(yaml_quote "$name")"
      printf '    type: hysteria2\n'
      printf '    server: %s\n' "$(yaml_quote "$server")"
      printf '    port: %s\n' "$port"
      printf '    password: %s\n' "$(yaml_quote "$password")"
      printf '    obfs: salamander\n'
      printf '    obfs-password: %s\n' "$(yaml_quote "$obfs_password")"
      printf '    sni: %s\n' "$(yaml_quote "$sni")"
      printf '    skip-cert-verify: true\n'
      printf '    alpn: [h3]\n'
    } >> "$clash_tmp"

    {
      printf 'hysteria2://%s@%s:%s/?sni=%s&insecure=1&obfs=salamander&obfs-password=%s#%s\n' \
        "$(url_encode_component "$password")" \
        "$server" \
        "$port" \
        "$(url_encode_component "$sni")" \
        "$(url_encode_component "$obfs_password")" \
        "$(url_encode_component "$name")"
    } >> "$sr_tmp"
  done < "$nodes_file"

  if [[ "$node_count" -eq 0 ]]; then
    die "nodes.conf has no nodes"
  fi

  cat >> "$clash_tmp" <<'EOF'

proxy-groups:
  - name: 节点选择
    type: select
    proxies:
      - 自动选择
EOF

  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(trim "$line")"
    [[ -z "$line" || "$line" == \#* ]] && continue
    IFS='|' read -r name _ <<< "$line"
    name="$(trim "$name")"
    printf '      - %s\n' "$(yaml_quote "$name")" >> "$clash_tmp"
  done < "$nodes_file"

  cat >> "$clash_tmp" <<'EOF'
      - DIRECT

  - name: 自动选择
    type: url-test
    url: http://www.gstatic.com/generate_204
    interval: 300
    tolerance: 50
    proxies:
EOF

  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(trim "$line")"
    [[ -z "$line" || "$line" == \#* ]] && continue
    IFS='|' read -r name _ <<< "$line"
    name="$(trim "$name")"
    printf '      - %s\n' "$(yaml_quote "$name")" >> "$clash_tmp"
  done < "$nodes_file"

  cat >> "$clash_tmp" <<'EOF'

rule-providers:
  private:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/private.yaml
    path: ./ruleset/private.yaml
    interval: 86400
  cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/cn.yaml
    path: ./ruleset/cn.yaml
    interval: 86400
  geolocation-cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/geolocation-cn.yaml
    path: ./ruleset/geolocation-cn.yaml
    interval: 86400
  geolocation-not-cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/geolocation-!cn.yaml
    path: ./ruleset/geolocation-not-cn.yaml
    interval: 86400
  google:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/google.yaml
    path: ./ruleset/google.yaml
    interval: 86400
  openai:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/openai.yaml
    path: ./ruleset/openai.yaml
    interval: 86400
  anthropic:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/anthropic.yaml
    path: ./ruleset/anthropic.yaml
    interval: 86400
  github:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/github.yaml
    path: ./ruleset/github.yaml
    interval: 86400
  apple-cn:
    type: http
    behavior: domain
    url: https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/apple-cn.yaml
    path: ./ruleset/apple-cn.yaml
    interval: 86400

rules:
  - DOMAIN,play.googleapis.com,节点选择
  - DOMAIN,android.clients.google.com,节点选择
  - DOMAIN,android.googleapis.com,节点选择
  - DOMAIN-SUFFIX,play.google.com,节点选择
  - DOMAIN-SUFFIX,googleplay.com,节点选择
  - DOMAIN-SUFFIX,googleapis.com,节点选择
  - DOMAIN-SUFFIX,googleapis.cn,节点选择
  - DOMAIN-SUFFIX,gvt1.com,节点选择
  - DOMAIN-SUFFIX,gvt2.com,节点选择
  - DOMAIN-SUFFIX,ggpht.com,节点选择
  - DOMAIN-SUFFIX,googleusercontent.com,节点选择
  - DOMAIN-SUFFIX,googleusercontent.cn,节点选择
  - DOMAIN-SUFFIX,android.com,节点选择
  - DOMAIN-SUFFIX,google.com,节点选择
  - DOMAIN-SUFFIX,chatgpt.com,节点选择
  - DOMAIN-SUFFIX,openai.com,节点选择
  - DOMAIN-SUFFIX,oaistatic.com,节点选择
  - DOMAIN-SUFFIX,oaiusercontent.com,节点选择
  - DOMAIN-SUFFIX,anthropic.com,节点选择
  - DOMAIN-SUFFIX,claude.ai,节点选择
  - DOMAIN-SUFFIX,github.com,节点选择
  - DOMAIN-SUFFIX,githubusercontent.com,节点选择
  - RULE-SET,google,节点选择
  - RULE-SET,openai,节点选择
  - RULE-SET,anthropic,节点选择
  - RULE-SET,github,节点选择
  - RULE-SET,private,DIRECT
  - RULE-SET,apple-cn,DIRECT
  - RULE-SET,cn,DIRECT
  - RULE-SET,geolocation-cn,DIRECT
  - RULE-SET,geolocation-not-cn,节点选择
  - GEOIP,CN,DIRECT
  - MATCH,节点选择
EOF

  mv "$clash_tmp" "$output_dir/clash.yaml"
  mv "$sr_tmp" "$output_dir/sr.txt"
  chmod 0644 "$output_dir/clash.yaml" "$output_dir/sr.txt"
  log "Generated $node_count node subscription files with shell renderer"
}

run_proxyctl_merge() {
  local nodes_file="$1"
  local output_dir="$2"
  local cli_arch
  local root

  cli_arch="$(arch)"
  root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

  if command -v proxyctl >/dev/null 2>&1; then
    proxyctl merge --nodes "$nodes_file" --output "$output_dir"
  elif [[ -x "$root/bin/proxyctl-linux-$cli_arch" ]]; then
    "$root/bin/proxyctl-linux-$cli_arch" merge --nodes "$nodes_file" --output "$output_dir"
  elif command -v go >/dev/null 2>&1 && [[ -f "$root/go.mod" ]]; then
    (cd "$root" && go run ./cmd/proxyctl merge --nodes "$nodes_file" --output "$output_dir")
  elif install_proxyctl_binary "$cli_arch"; then
    "$PROXYCTL_BIN" merge --nodes "$nodes_file" --output "$output_dir"
  else
    log_warn "proxyctl not found and automatic download failed; using built-in shell renderer."
    write_subscriptions_with_shell "$nodes_file" "$output_dir"
  fi
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
  if ss -lntp | grep -qE "[:.]${port}[[:space:]]"; then
    local pids
    pids=$(ss -lntp | grep -E "[:.]${port}[[:space:]]" | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u)
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
  if ss -lnup | grep -qE "[:.]${port}[[:space:]]"; then
    local pids
    pids=$(ss -lnup | grep -E "[:.]${port}[[:space:]]" | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u)
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
reset_caddy_apt_source
apt_update_or_die
apt-get install -y curl wget tar openssl ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https qrencode > /dev/null

log "Installing sing-box"
SB_VERSION="$(curl -fsSL https://api.github.com/repos/SagerNet/sing-box/releases/latest | sed -n 's/.*"tag_name": "v\([^"]*\)".*/\1/p' | head -1)"
[[ -n "$SB_VERSION" ]] || die "Failed to get sing-box version"
TMP="$(mktemp -d "/tmp/sb.tmp.XXXXXX")"; trap 'rm -rf "$TMP"' EXIT
wget -qO "$TMP/sb.tgz" "https://github.com/SagerNet/sing-box/releases/download/v${SB_VERSION}/sing-box-${SB_VERSION}-linux-$(arch).tar.gz"
tar -xzf "$TMP/sb.tgz" -C "$TMP"
install -m 0755 "$TMP/sing-box-${SB_VERSION}-linux-$(arch)/sing-box" /usr/local/bin/sing-box

log "Installing Caddy"
install_caddy_apt_source
apt_update_or_die
if ! command -v caddy >/dev/null 2>&1; then
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
if ! systemctl is-active --quiet sing-box || ! ss -lnup | grep -qE "[:.]${HY2_PORT}[[:space:]]"; then
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

run_proxyctl_merge "$NODES_CONF" "$OUT"

# Caddy runs as the caddy user and must be able to traverse the subscription path.
chown root:root /var/www
chmod 0755 /var/www
chown -R caddy:caddy "$SUB_ROOT"
find "$SUB_ROOT" -type d -exec chmod 755 {} +
find "$SUB_ROOT" -type f -exec chmod 644 {} +

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
if ! CADDY_VALIDATE_ERROR="$(caddy validate --config "$CADDY_TMP" --adapter caddyfile 2>&1)"; then
  log_error "caddy validate failed: $CADDY_VALIDATE_ERROR"
  rm -f "$CADDY_TMP"
  die "caddy validate failed. Review the Caddy error above and $LOG_FILE."
fi

if [[ -f "/etc/caddy/Caddyfile" ]]; then
  cp -p "/etc/caddy/Caddyfile" "/etc/caddy/Caddyfile.previous.$TS"
fi
chown root:caddy "$CADDY_TMP"
chmod 0640 "$CADDY_TMP"
mv "$CADDY_TMP" "/etc/caddy/Caddyfile"

if systemctl is-active --quiet caddy; then
  if ! caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1; then
    log_error "caddy reload failed, restoring previous config"
    if [[ -f "/etc/caddy/Caddyfile.previous.$TS" ]]; then
      mv "/etc/caddy/Caddyfile.previous.$TS" "/etc/caddy/Caddyfile"
      caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || true
      if ! systemctl is-active --quiet caddy; then
        die "caddy health check failed even after rollback."
      fi
    fi
    die "Failed to reload caddy with new config."
  fi
else
  systemctl enable --now caddy
fi

# Check external & local access
sleep 1
LOCAL_SUBSCRIPTION_STATUS="$(curl -sk -o /dev/null -w '%{http_code}' --resolve "$DOMAIN:443:127.0.0.1" --connect-timeout 5 "https://$DOMAIN/$TOKEN/clash.yaml" || true)"
if [[ "$LOCAL_SUBSCRIPTION_STATUS" == "403" ]]; then
  log_error "Caddy returned HTTP 403 for the subscription after permissions were applied. Check /etc/caddy/Caddyfile and the requested token path."
  die "Local Caddy subscription check returned HTTP 403."
elif [[ "$LOCAL_SUBSCRIPTION_STATUS" != "200" ]]; then
  log_warn "Local Caddy subscription check returned HTTP ${LOCAL_SUBSCRIPTION_STATUS:-000}. Check Caddy logs and DNS."
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
