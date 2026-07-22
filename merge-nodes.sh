#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="/etc/singbox-sub-manager"
NODES_FILE="${1:-$BASE_DIR/nodes.conf}"
STATE_DIR="/var/lib/singbox-sub-manager"
TOKEN_FILE="${TOKEN_FILE:-$STATE_DIR/token}"
SUB_ROOT="${SUB_ROOT:-/var/www/proxy-sub}"
PROXYCTL_BIN="/usr/local/bin/proxyctl"
PROXYCTL_REPOSITORY="${PROXYCTL_REPOSITORY:-hlpclg/singbox-sub-manager}"
PROXYCTL_VERSION="${PROXYCTL_VERSION:-v0.2.1}"

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

install_proxyctl_binary() {
  local cli_arch="$1"
  local base_url="https://github.com/$PROXYCTL_REPOSITORY/releases/download/$PROXYCTL_VERSION"
  local binary_name="proxyctl-linux-$cli_arch"
  local tmp_dir expected actual

  tmp_dir="$(mktemp -d /tmp/proxyctl.XXXXXX)"
  echo "正在下载 proxyctl $PROXYCTL_VERSION: linux-$cli_arch"
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

[[ $(id -u) -eq 0 ]] || die "请用 sudo 运行：sudo bash merge-nodes.sh [nodes.conf]"
[[ -f "$NODES_FILE" ]] || die "找不到节点文件：$NODES_FILE"
[[ -f "$TOKEN_FILE" ]] || die "找不到 $TOKEN_FILE；请在订阅中心服务器运行。"

TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
[[ "$TOKEN" =~ ^[A-Za-z0-9_-]{16,128}$ ]] || die "订阅 token 格式异常"
OUT="$SUB_ROOT/$TOKEN"
mkdir -p "$OUT"

if ! command -v proxyctl >/dev/null 2>&1; then
  install_proxyctl_binary "$(arch)" || die "无法下载或校验 proxyctl $PROXYCTL_VERSION。请检查 GitHub Release 是否已发布。"
fi

proxyctl merge --nodes "$NODES_FILE" --output "$OUT"
chown -R caddy:caddy "$SUB_ROOT"
find "$SUB_ROOT" -type d -exec chmod 755 {} +
find "$SUB_ROOT" -type f -exec chmod 644 {} +

echo "统一订阅已生成："
echo "  $OUT/clash.yaml"
echo "  $OUT/sr.txt"