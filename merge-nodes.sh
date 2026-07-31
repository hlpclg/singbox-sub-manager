#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="/etc/singbox-sub-manager"
NODES_FILE="${1:-$BASE_DIR/nodes.conf}"
STATE_DIR="/var/lib/singbox-sub-manager"
TOKEN_FILE="${TOKEN_FILE:-$STATE_DIR/token}"
SUB_ROOT="${SUB_ROOT:-/var/www/proxy-sub}"
PROXYCTL_BIN="${PROXYCTL_BIN:-/usr/local/bin/proxyctl}"
PROXYCTL_REPOSITORY="${PROXYCTL_REPOSITORY:-hlpclg/singbox-sub-manager}"
PROXYCTL_VERSION="${PROXYCTL_VERSION:-v0.2.2}"
PROXYCTL_VALIDATED_BIN=""

die() {
  echo "$1" >&2
  exit 1
}

arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "不支持的架构" ;;
  esac
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

install_proxyctl_binary() {
  local cli_arch="$1"
  local base_url="https://github.com/$PROXYCTL_REPOSITORY/releases/download/$PROXYCTL_VERSION"
  local binary_name="proxyctl-linux-$cli_arch"
  local tmp_dir expected expected_count actual

  tmp_dir="$(mktemp -d /tmp/proxyctl.XXXXXX)"
  echo "正在下载 proxyctl $PROXYCTL_VERSION: linux-$cli_arch"
  if ! curl -fsSL "$base_url/$binary_name" -o "$tmp_dir/$binary_name" || ! curl -fsSL "$base_url/checksums.txt" -o "$tmp_dir/checksums.txt"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  expected_count="$(awk -v file="$binary_name" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == file || $2 == "*" file) { count++ } END { print count+0 }' "$tmp_dir/checksums.txt")"
  expected="$(awk -v file="$binary_name" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == file || $2 == "*" file) { print tolower($1) }' "$tmp_dir/checksums.txt")"
  actual="$(sha256_file "$tmp_dir/$binary_name" 2>/dev/null || true)"
  if [[ "$expected_count" != "1" || -z "$expected" || "$expected" != "$actual" ]]; then
    rm -rf "$tmp_dir"
    return 1
  fi
  chmod 0755 "$tmp_dir/$binary_name"
  local ver_out
  ver_out="$("$tmp_dir/$binary_name" version 2>/dev/null || true)"
  if [[ "$ver_out" != "$PROXYCTL_VERSION" ]]; then
    rm -rf "$tmp_dir"
    return 1
  fi

  local bin_dir bin_tmp sidecar_tmp
  bin_dir="$(dirname "$PROXYCTL_BIN")"
  install -d -m 0755 "$bin_dir"
  bin_tmp="$(mktemp "$bin_dir/.proxyctl.tmp.XXXXXX")"
  sidecar_tmp="$(mktemp "$bin_dir/.proxyctl.sha256.tmp.XXXXXX")"
  install -m 0755 "$tmp_dir/$binary_name" "$bin_tmp"
  printf '%s\n' "$expected" > "$sidecar_tmp"
  chmod 0644 "$sidecar_tmp"
  mv -f "$bin_tmp" "$PROXYCTL_BIN"
  mv -f "$sidecar_tmp" "${PROXYCTL_BIN}.sha256"
  rm -rf "$tmp_dir"
  hash -r 2>/dev/null || true
  PROXYCTL_VALIDATED_BIN="$PROXYCTL_BIN"
  return 0
}

[[ $(id -u) -eq 0 ]] || die "请用 sudo 运行：sudo bash merge-nodes.sh [nodes.conf]"
[[ -f "$NODES_FILE" ]] || die "找不到节点文件：$NODES_FILE"
[[ -f "$TOKEN_FILE" ]] || die "找不到 $TOKEN_FILE；请在订阅中心服务器运行。"

TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
[[ "$TOKEN" =~ ^[A-Za-z0-9_-]{16,128}$ ]] || die "订阅 token 格式异常"
OUT="$SUB_ROOT/$TOKEN"
mkdir -p "$OUT"

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
  fi
fi

if [[ -z "$PROXYCTL_VALIDATED_BIN" ]]; then
  install_proxyctl_binary "$(arch)" || die "无法下载或校验 proxyctl ${PROXYCTL_VERSION}。请检查 GitHub Release 是否已发布。"
fi

if [[ -z "$PROXYCTL_VALIDATED_BIN" || ! -x "$PROXYCTL_VALIDATED_BIN" ]]; then
  die "proxyctl ${PROXYCTL_VERSION} 未通过校验或无法执行"
fi

"$PROXYCTL_VALIDATED_BIN" merge --nodes "$NODES_FILE" --output "$OUT"
chown -R caddy:caddy "$SUB_ROOT"
find "$SUB_ROOT" -type d -exec chmod 755 {} +
find "$SUB_ROOT" -type f -exec chmod 644 {} +

echo "统一订阅已生成："
echo "  $OUT/clash.yaml"
echo "  $OUT/sr.txt"
