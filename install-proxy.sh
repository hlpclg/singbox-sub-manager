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
CADDY_APT_SOURCE_URL="https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt"
CADDY_SIGNING_FINGERPRINT="2F5C3BE9886ACD2913299EFBABA1F9B8875A6661"
CADDY_APT_BACKUP_ORIGINALS=()
CADDY_APT_BACKUP_FILES=()
CADDY_APT_BOOTSTRAP_ORIGINALS=()
CADDY_APT_BOOTSTRAP_FILES=()
PROXYCTL_BIN="/usr/local/bin/proxyctl"
PROXYCTL_REPOSITORY="${PROXYCTL_REPOSITORY:-hlpclg/singbox-sub-manager}"
PROXYCTL_VERSION="${PROXYCTL_VERSION:-v0.4.0}"
PROXYCTL_VALIDATED_BIN=""

# 1. OS & Root Check (Safe Early Exit)
if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: Please run as root (sudo)" >&2
  exit 1
fi

if [[ -f /etc/os-release ]]; then
  # shellcheck disable=SC1091
  source /etc/os-release
  OS="${ID:-}"
  VER="${VERSION_ID:-}"
  if [[ "$OS" == "ubuntu" ]]; then
    if [[ "$VER" != "22.04" && "$VER" != "24.04" ]]; then
      echo "ERROR: Only Ubuntu 22.04 and 24.04 are supported" >&2
      exit 1
    fi
  elif [[ "$OS" == "debian" ]]; then
    if [[ "$VER" != "12" ]]; then
      echo "ERROR: Only Debian 12 is supported" >&2
      exit 1
    fi
  else
    echo "ERROR: Only Ubuntu and Debian are supported" >&2
    exit 1
  fi
else
  echo "ERROR: Could not determine OS" >&2
  exit 1
fi

arch(){ case "$(uname -m)" in x86_64|amd64) echo amd64;; aarch64|arm64) echo arm64;; *) echo "ERROR: Unsupported architecture" >&2; exit 1;; esac; }

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
  return 1
}

validate_caddy_apt_source() {
  local source_file="$1" line normalized deb_count=0
  local repo="https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version main"
  local expected_deb="deb [signed-by=$CADDY_KEYRING] $repo"
  local expected_deb_src="deb-src [signed-by=$CADDY_KEYRING] $repo"

  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" =~ ^[[:space:]]*($|#) ]] && continue
    normalized="$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e 's/[[:space:]][[:space:]]*/ /g')"
    if [[ "$normalized" == "$expected_deb" ]]; then
      ((deb_count += 1))
    elif [[ "$normalized" == "$expected_deb_src" ]]; then
      : # official source-package entry; cloudsmith now ships this alongside deb
    else
      return 1
    fi
  done < "$source_file"

  [[ "$deb_count" -eq 1 ]]
}

backup_caddy_apt_state() {
  local backup_dir="$1" source_dir source_file backup_file index=0 count
  source_dir="$(dirname "$CADDY_APT_LIST")"
  CADDY_APT_BACKUP_ORIGINALS=()
  CADDY_APT_BACKUP_FILES=()

  if [[ -f "$CADDY_KEYRING" ]]; then
    CADDY_APT_BACKUP_ORIGINALS+=("$CADDY_KEYRING")
  fi
  while IFS= read -r -d '' source_file; do
    if [[ "$source_file" == "$CADDY_APT_LIST" ]] || grep -q 'dl.cloudsmith.io/public/caddy/stable' "$source_file" 2>/dev/null; then
      if [[ "$source_file" != "$CADDY_APT_LIST" ]] && ! validate_caddy_apt_source "$source_file"; then
        log_error "Legacy Caddy source is mixed with or differs from the official repository: $source_file"
        return 1
      fi
      CADDY_APT_BACKUP_ORIGINALS+=("$source_file")
    fi
  done < <(find "$source_dir" -maxdepth 1 -type f \( -name '*.list' -o -name '*.sources' \) -print0 2>/dev/null)

  count="${#CADDY_APT_BACKUP_ORIGINALS[@]}"
  for ((index = 0; index < count; index += 1)); do
    source_file="${CADDY_APT_BACKUP_ORIGINALS[$index]}"
    backup_file="$backup_dir/$index"
    if ! mv "$source_file" "$backup_file"; then
      restore_caddy_apt_state "$backup_dir"
      return 1
    fi
    CADDY_APT_BACKUP_FILES+=("$backup_file")
  done
}

restore_caddy_apt_state() {
  local backup_dir="$1" index count
  rm -f "$CADDY_APT_LIST" "$CADDY_KEYRING"
  count="${#CADDY_APT_BACKUP_ORIGINALS[@]}"
  for ((index = 0; index < count; index += 1)); do
    [[ -e "${CADDY_APT_BACKUP_FILES[$index]}" ]] && mv "${CADDY_APT_BACKUP_FILES[$index]}" "${CADDY_APT_BACKUP_ORIGINALS[$index]}"
  done
  rm -rf "$backup_dir"
}

discard_caddy_apt_state_backup() {
  rm -rf "$1"
}

prepare_caddy_apt_bootstrap() {
  local source_dir
  source_dir="$(dirname "$CADDY_APT_LIST")"
  install -d -m 0755 "$source_dir" "$(dirname "$CADDY_KEYRING")"
  CADDY_APT_BOOTSTRAP_DIR="$(mktemp -d "$source_dir/.caddy-apt-bootstrap.XXXXXX")"
  if ! backup_caddy_apt_state "$CADDY_APT_BOOTSTRAP_DIR"; then
    discard_caddy_apt_state_backup "$CADDY_APT_BOOTSTRAP_DIR"
    CADDY_APT_BOOTSTRAP_DIR=""
    return 1
  fi
  CADDY_APT_BOOTSTRAP_ORIGINALS=()
  CADDY_APT_BOOTSTRAP_FILES=()
  local index backup_count="${#CADDY_APT_BACKUP_ORIGINALS[@]}"
  for ((index = 0; index < backup_count; index += 1)); do
    CADDY_APT_BOOTSTRAP_ORIGINALS+=("${CADDY_APT_BACKUP_ORIGINALS[$index]}")
    CADDY_APT_BOOTSTRAP_FILES+=("${CADDY_APT_BACKUP_FILES[$index]}")
  done
}

restore_caddy_apt_bootstrap() {
  [[ -n "${CADDY_APT_BOOTSTRAP_DIR:-}" ]] || return 0
  CADDY_APT_BACKUP_ORIGINALS=()
  CADDY_APT_BACKUP_FILES=()
  local index bootstrap_count="${#CADDY_APT_BOOTSTRAP_ORIGINALS[@]}"
  for ((index = 0; index < bootstrap_count; index += 1)); do
    CADDY_APT_BACKUP_ORIGINALS+=("${CADDY_APT_BOOTSTRAP_ORIGINALS[$index]}")
    CADDY_APT_BACKUP_FILES+=("${CADDY_APT_BOOTSTRAP_FILES[$index]}")
  done
  restore_caddy_apt_state "$CADDY_APT_BOOTSTRAP_DIR"
  CADDY_APT_BOOTSTRAP_DIR=""
  CADDY_APT_BOOTSTRAP_ORIGINALS=()
  CADDY_APT_BOOTSTRAP_FILES=()
}

bootstrap_caddy_apt_before_dependencies() {
  prepare_caddy_apt_bootstrap || return 1
  if ! apt_update_or_die; then
    restore_caddy_apt_bootstrap
    return 1
  fi
}

install_caddy_apt_source() {
  (
  local key_tmp source_tmp keyring_tmp source_install_tmp state_backup
  local keyring_dir source_dir activated=0
  keyring_dir="$(dirname "$CADDY_KEYRING")"
  source_dir="$(dirname "$CADDY_APT_LIST")"
  install -d -m 0755 "$keyring_dir" "$source_dir"

  key_tmp="$(mktemp "/tmp/caddy-key.XXXXXX")"
  source_tmp="$(mktemp "/tmp/caddy-source.XXXXXX")"
  keyring_tmp="$(mktemp "$keyring_dir/.caddy-keyring.tmp.XXXXXX")"
  source_install_tmp="$(mktemp "$source_dir/.caddy-source.tmp.XXXXXX")"
  cleanup_caddy_apt_source() {
    rm -f "$key_tmp" "$source_tmp" "$keyring_tmp" "$source_install_tmp"
    if [[ "$activated" -eq 0 && -n "${state_backup:-}" ]]; then
      restore_caddy_apt_state "$state_backup"
    fi
  }
  trap cleanup_caddy_apt_source EXIT

  if ! curl -fsSL "$CADDY_GPG_URL" -o "$key_tmp"; then
    die "Failed to download Caddy APT signing key"
  fi

  if ! curl -fsSL "$CADDY_APT_SOURCE_URL" -o "$source_tmp"; then
    die "Failed to download the official Caddy APT source"
  fi

  if ! gpg --batch --show-keys --with-colons "$key_tmp" 2>/dev/null |
    awk -F: -v expected="$CADDY_SIGNING_FINGERPRINT" \
      '$1 == "fpr" && $10 == expected { found=1 } END { exit(found ? 0 : 1) }'
  then
    die "Downloaded Caddy signing key fingerprint mismatch"
  fi

  validate_caddy_apt_source "$source_tmp" || die "Downloaded Caddy APT source is not the exact official repository entry"

  if ! gpg --batch --yes --dearmor --output "$keyring_tmp" "$key_tmp"; then
    die "Failed to dearmor Caddy APT signing key"
  fi
  chmod 0644 "$keyring_tmp"
  install -m 0644 "$source_tmp" "$source_install_tmp"

  state_backup="$(mktemp -d "$source_dir/.caddy-apt-state.XXXXXX")"
  backup_caddy_apt_state "$state_backup" || die "Failed to safely back up existing Caddy APT configuration"
  mv -f "$keyring_tmp" "$CADDY_KEYRING" || die "Failed to activate Caddy APT signing key"
  mv -f "$source_install_tmp" "$CADDY_APT_LIST" || die "Failed to activate Caddy APT source"
  activated=1
  discard_caddy_apt_state_backup "$state_backup"
  cleanup_caddy_apt_source
  trap - EXIT
  )
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    return 1
  fi
}

install_proxyctl() {
  log "Installing proxyctl"
  PROXYCTL_VALIDATED_BIN=""

  local existing_sha recorded_sha existing_ver
  if [[ -x "$PROXYCTL_BIN" && -f "${PROXYCTL_BIN}.sha256" ]]; then
    existing_sha="$(sha256_file "$PROXYCTL_BIN" 2>/dev/null || true)"
    recorded_sha="$(tr -d '[:space:]' < "${PROXYCTL_BIN}.sha256")"
    if [[ -n "$existing_sha" && "$recorded_sha" == "$existing_sha" ]]; then
      existing_ver="$("$PROXYCTL_BIN" version 2>/dev/null || true)"
    else
      existing_ver=""
    fi
    if [[ "$existing_ver" == "$PROXYCTL_VERSION" ]]; then
      PROXYCTL_VALIDATED_BIN="$PROXYCTL_BIN"
      return 0
    fi
    log "Installed proxyctl cannot be reused; downloading a verified replacement."
  fi

  local arch asset url tmp chk_url chk_tmp expected_sha256 expected_count
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *)
      PROXYCTL_VALIDATED_BIN=""
      log_warn "Unsupported proxyctl architecture; using shell renderer."
      return 1
      ;;
  esac

  asset="proxyctl-linux-${arch}"
  url="https://github.com/${PROXYCTL_REPOSITORY}/releases/download/${PROXYCTL_VERSION}/${asset}"
  chk_url="https://github.com/${PROXYCTL_REPOSITORY}/releases/download/${PROXYCTL_VERSION}/checksums.txt"
  tmp="$(mktemp "/tmp/proxyctl.XXXXXX")"
  chk_tmp="$(mktemp "/tmp/proxyctl.checksums.XXXXXX")"

  if ! curl -fsSL "$url" -o "$tmp" || ! curl -fsSL "$chk_url" -o "$chk_tmp"; then
    rm -f "$tmp" "$chk_tmp"
    PROXYCTL_VALIDATED_BIN=""
    log_warn "Failed to download proxyctl or checksums; using shell renderer."
    return 1
  fi

  expected_count="$(awk -v asset="$asset" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == asset || $2 == "*" asset) { count++ } END { print count+0 }' "$chk_tmp")"
  expected_sha256="$(awk -v asset="$asset" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == asset || $2 == "*" asset) { print tolower($1) }' "$chk_tmp")"
  if [[ "$expected_count" != "1" || -z "$expected_sha256" ]]; then
    rm -f "$tmp" "$chk_tmp"
    PROXYCTL_VALIDATED_BIN=""
    log_warn "Checksum entry not found for $asset; using shell renderer."
    return 1
  fi
  if [[ "$(sha256_file "$tmp" 2>/dev/null || true)" != "$expected_sha256" ]]; then
    rm -f "$tmp" "$chk_tmp"
    PROXYCTL_VALIDATED_BIN=""
    log_warn "proxyctl checksum mismatch; using shell renderer."
    return 1
  fi
  rm -f "$chk_tmp"

  chmod 0755 "$tmp"
  local ver_out
  ver_out="$("$tmp" version 2>/dev/null || true)"
  if [[ "$ver_out" != "$PROXYCTL_VERSION" ]]; then
    rm -f "$tmp"
    PROXYCTL_VALIDATED_BIN=""
    log_warn "Downloaded proxyctl binary version mismatch ('$ver_out' != '$PROXYCTL_VERSION'); using shell renderer."
    return 1
  fi

  local bin_dir bin_tmp sidecar_tmp
  bin_dir="$(dirname "$PROXYCTL_BIN")"
  install -d -m 0755 "$bin_dir"
  bin_tmp="$(mktemp "$bin_dir/.proxyctl.tmp.XXXXXX")"
  sidecar_tmp="$(mktemp "$bin_dir/.proxyctl.sha256.tmp.XXXXXX")"
  install -m 0755 "$tmp" "$bin_tmp"
  printf '%s\n' "$expected_sha256" > "$sidecar_tmp"
  chmod 0644 "$sidecar_tmp"
  mv -f "$bin_tmp" "$PROXYCTL_BIN"
  mv -f "$sidecar_tmp" "${PROXYCTL_BIN}.sha256"
  rm -f "$tmp"
  hash -r 2>/dev/null || true
  PROXYCTL_VALIDATED_BIN="$PROXYCTL_BIN"
  return 0
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

yaml_quote() {
  local value="$1"
  if [[ "$value" =~ [[:cntrl:]] ]]; then
    die "Invalid configuration value: contains control characters."
  fi
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

contains_control_chars() {
  local value="$1"
  [[ "$value" == *$'\n'* || "$value" == *$'\r'* || "$value" == *$'\t'* ]]
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

  for value in "$name" "$server" "$port" "$password" "$obfs_password" "$sni"; do
    if contains_control_chars "$value"; then
      die "nodes.conf line $line_no: control characters are not allowed"
    fi
  done

  if [[ -z "$name" || -z "$server" || -z "$port" || -z "$password" || -z "$obfs_password" || -z "$sni" ]]; then
    die "nodes.conf line $line_no: empty field"
  fi

  case "$name" in
    DIRECT|自动选择|节点选择)
      die "nodes.conf line $line_no: reserved node name: $name"
      ;;
    *"|"*)
      die "nodes.conf line $line_no: node name cannot contain pipe character"
      ;;
  esac

  if [[ "$server" == *" "* || "$server" == *"/"* || "$server" == *":"* || "$server" == *"|"* ]]; then
    die "nodes.conf line $line_no: server contains invalid characters"
  fi

  if [[ "$sni" == *" "* || "$sni" == *"/"* || "$sni" == *":"* || "$sni" == *"|"* ]]; then
    die "nodes.conf line $line_no: sni contains invalid characters"
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

  if [[ -n "$PROXYCTL_VALIDATED_BIN" && -x "$PROXYCTL_VALIDATED_BIN" ]]; then
    "$PROXYCTL_VALIDATED_BIN" merge --nodes "$nodes_file" --output "$output_dir"
  else
    log_warn "Validated proxyctl unavailable; using built-in shell renderer."
    write_subscriptions_with_shell "$nodes_file" "$output_dir"
  fi
}



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
  migration_tmp="$(mktemp "$STATE_DIR/.migration.tmp.XXXXXX")"
  {
    printf 'completed_at=%s\n' "$(date -Is)"
    printf 'migrated=%s\n' "$migrated"
  } > "$migration_tmp"
  chmod 0600 "$migration_tmp"
  mv "$migration_tmp" "$MIGRATION_FILE"
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
  if [[ ! "$TOKEN" =~ ^[A-Fa-f0-9]{32}$ ]]; then
    die "Invalid token file: must be a 32-character hex string. Please repair or remove it."
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
if ! bootstrap_caddy_apt_before_dependencies; then
  die "apt-get update failed after quarantining the Caddy APT configuration"
fi
if ! apt-get install -y curl wget tar openssl ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https qrencode > /dev/null; then
  restore_caddy_apt_bootstrap
  die "Failed to install dependencies after quarantining the Caddy APT configuration"
fi

log "Installing sing-box"
SB_VERSION="$(curl -fsSL https://api.github.com/repos/SagerNet/sing-box/releases/latest | sed -n 's/.*"tag_name": "v\([^"]*\)".*/\1/p' | head -1)"
[[ -n "$SB_VERSION" ]] || die "Failed to get sing-box version"
TMP="$(mktemp -d "/tmp/sb.tmp.XXXXXX")"; trap 'rm -rf "$TMP"' EXIT
wget -qO "$TMP/sb.tgz" "https://github.com/SagerNet/sing-box/releases/download/v${SB_VERSION}/sing-box-${SB_VERSION}-linux-$(arch).tar.gz"
tar -xzf "$TMP/sb.tgz" -C "$TMP"
SB_NEW="/usr/local/bin/sing-box.new"
install -m 0755 "$TMP/sing-box-${SB_VERSION}-linux-$(arch)/sing-box" "$SB_NEW"

if ! "$SB_NEW" version >/dev/null 2>&1; then
  rm -f "$SB_NEW"
  die "sing-box binary is invalid or corrupted."
fi

log "Installing Caddy"
if ! install_caddy_apt_source; then
  restore_caddy_apt_bootstrap
  die "Failed to install the verified Caddy APT configuration"
fi
if ! apt_update_or_die; then
  restore_caddy_apt_bootstrap
  die "apt-get update failed after installing the verified Caddy APT configuration"
fi
discard_caddy_apt_state_backup "$CADDY_APT_BOOTSTRAP_DIR"
CADDY_APT_BOOTSTRAP_DIR=""
if ! command -v caddy >/dev/null 2>&1; then
  apt-get install -y caddy > /dev/null
fi

# 7. Generate Hysteria2 Certificates
log "Checking Hysteria2 certificates"
if [[ ! -f "$CERTS_DIR/server.crt" && ! -f "$CERTS_DIR/server.key" ]]; then
  log "Generating new self-signed certificate"
  openssl req -x509 -newkey rsa:2048 -nodes -days 365 -subj "/CN=$SNI" -keyout "$CERTS_DIR/server.key" -out "$CERTS_DIR/server.crt" >/dev/null 2>&1
elif [[ ! -f "$CERTS_DIR/server.crt" || ! -f "$CERTS_DIR/server.key" ]]; then
  die "Certificate is incomplete. Both server.crt and server.key must be present or absent. Please repair."
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

if ! "$SB_NEW" check -c "$SB_TMP"; then
  rm -f "$SB_TMP" "$SB_NEW"
  die "sing-box config check failed with new binary."
fi

TS="$(date +%Y%m%d%H%M%S)"
if [[ -f "/usr/local/bin/sing-box" ]]; then
  mv "/usr/local/bin/sing-box" "/usr/local/bin/sing-box.previous.$TS"
fi
mv "$SB_NEW" "/usr/local/bin/sing-box"

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
  log_error "sing-box failed to start or bind UDP port $HY2_PORT. Restoring previous binary and config."
  if [[ -f "/usr/local/bin/sing-box.previous.$TS" ]]; then
    mv "/usr/local/bin/sing-box.previous.$TS" "/usr/local/bin/sing-box"
  else
    rm -f "/usr/local/bin/sing-box"
  fi
  if [[ -f "$CONF_DIR/config.json.previous.$TS" ]]; then
    mv "$CONF_DIR/config.json.previous.$TS" "$CONF_DIR/config.json"
  else
    rm -f "$CONF_DIR/config.json"
  fi
  systemctl daemon-reload
  if [[ -f "/usr/local/bin/sing-box" ]]; then
    systemctl restart sing-box || true
    sleep 1
    if ! systemctl is-active --quiet sing-box; then
      systemctl disable --now sing-box || true
      die "sing-box health check failed even after full rollback."
    fi
  else
    systemctl disable --now sing-box || true
  fi
  die "sing-box health check failed. Full rollback performed."
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
    log "nodes.conf exists. Checking if IP needs update..."
    nodes_tmp="$(mktemp "$BASE_DIR/.nodes.conf.tmp.XXXXXX")"

    awk -F'|' -v OFS='|' \
      -v target="$NODE_NAME" \
      -v server="$PUBLIC_IP" \
      -v port="$HY2_PORT" \
      -v password="$PASSWORD" \
      -v obfs="$OBFS_PASSWORD" \
      -v sni="$SNI" '
      BEGIN { updated = 0 }

      /^[[:space:]]*#/ || /^[[:space:]]*$/ {
        print
        next
      }

      $1 == target && updated == 0 {
        print target, server, port, password, obfs, sni
        updated = 1
        next
      }

      {
        print
      }

      END {
        if (updated == 0) {
          print target, server, port, password, obfs, sni
        }
      }
    ' "$NODES_CONF" > "$nodes_tmp"

    chmod 0600 "$nodes_tmp"
    mv "$nodes_tmp" "$NODES_CONF"
  fi
fi

if ! install_proxyctl; then
  log_warn "proxyctl is unavailable; built-in shell renderer will be used."
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
  if ! systemctl enable --now caddy; then
    log_error "Failed to start caddy with new config."
    if [[ -f "/etc/caddy/Caddyfile.previous.$TS" ]]; then
      mv "/etc/caddy/Caddyfile.previous.$TS" "/etc/caddy/Caddyfile"
      systemctl restart caddy || true
    else
      rm -f /etc/caddy/Caddyfile
      systemctl stop caddy || true
    fi
    die "Caddy failed to start. Configuration reverted."
  fi
fi

# Check external & local access
status="000"
curl_err=""
for i in $(seq 1 24); do
  curl_out="$(
    curl -sk \
      -o /dev/null \
      -w '%{http_code}' \
      --resolve "$DOMAIN:443:127.0.0.1" \
      --connect-timeout 5 \
      "https://$DOMAIN/$TOKEN/clash.yaml" 2>&1 || true
  )"
  
  if [[ "$curl_out" =~ ^[0-9]{3}$ ]]; then
    status="$curl_out"
    curl_err=""
  else
    status="000"
    curl_err="$curl_out"
  fi

  [[ "$status" == "200" ]] && break
  
  if [[ "$status" == "403" ]]; then
    log_error "Caddy returned HTTP 403. Check /etc/caddy/Caddyfile and the requested token path."
    break
  fi
  sleep 5
done

if [[ "$status" != "200" ]]; then
  if [[ -n "$curl_err" ]]; then
    log_error "Local Caddy health check failed. Last curl error: $curl_err"
  else
    log_error "Local Caddy health check failed with HTTP $status."
  fi
  log_error "Restoring previous Caddyfile."
  if [[ -f "/etc/caddy/Caddyfile.previous.$TS" ]]; then
    mv "/etc/caddy/Caddyfile.previous.$TS" "/etc/caddy/Caddyfile"
    caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || log_warn "Restored Caddyfile failed validation"
    systemctl restart caddy || true
  else
    rm -f /etc/caddy/Caddyfile
    systemctl stop caddy || true
  fi
  die "Caddy health check failed."
fi

if ! curl -sf --connect-timeout 10 "https://$DOMAIN/$TOKEN/clash.yaml" >/dev/null; then
  log_warn "External curl check to Caddy failed. This may be due to DNS propagation."
fi

# 11. Monitor
cat > /etc/systemd/system/proxyctl-monitor.service <<'EOF2'
[Unit]
Description=Proxyctl Health Monitor

[Service]
Type=oneshot
ExecStart=/usr/local/bin/proxyctl monitor
SuccessExitStatus=2
EOF2

cat > /etc/systemd/system/proxyctl-monitor.timer <<'EOF2'
[Unit]
Description=Proxyctl Health Monitor Timer

[Timer]
OnCalendar=*:0/5
Persistent=true
AccuracySec=15s
RandomizedDelaySec=15s

[Install]
WantedBy=timers.target
EOF2

systemctl daemon-reload
systemctl enable --now proxyctl-monitor.timer

# 12. Sysctl
cat > /etc/sysctl.d/99-proxy-installer.conf <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
EOF
sysctl --system >/dev/null 2>&1 || true

# 13. Write install state
INSTALL_JSON="$BASE_DIR/install.json"
INSTALL_JSON_TMP="$(mktemp "$BASE_DIR/.install.json.tmp.XXXXXX")"
trap 'rm -rf "$TMP" "$INSTALL_JSON_TMP"' EXIT
cat > "$INSTALL_JSON_TMP" <<EOF2
{
  "domain": "$DOMAIN",
  "singbox_config": "/etc/sing-box/config.json",
  "caddy_config": "/etc/caddy/Caddyfile",
  "subscription_root": "$SUB_ROOT",
  "token_file": "$TOKEN_FILE"
}
EOF2
chmod 0644 "$INSTALL_JSON_TMP"
mv "$INSTALL_JSON_TMP" "$INSTALL_JSON"

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
