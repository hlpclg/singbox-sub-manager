#!/usr/bin/env bash
set -euo pipefail

NODES_FILE="${1:-nodes.conf}"
TOKEN_FILE="${TOKEN_FILE:-/etc/proxy-sub-token}"
SUB_ROOT="${SUB_ROOT:-/var/www/proxy-sub}"

if [[ $(id -u) -ne 0 ]]; then
  echo "请用 sudo 运行：sudo bash merge-nodes.sh nodes.conf" >&2
  exit 1
fi
if [[ ! -f "$NODES_FILE" ]]; then
  echo "找不到节点文件：$NODES_FILE" >&2
  exit 1
fi
if [[ ! -f "$TOKEN_FILE" ]]; then
  echo "找不到 $TOKEN_FILE；请在订阅中心服务器运行。" >&2
  exit 1
fi

TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
[[ "$TOKEN" =~ ^[A-Za-z0-9_-]{16,128}$ ]] || { echo "订阅 token 格式异常" >&2; exit 1; }
OUT="$SUB_ROOT/$TOKEN"
mkdir -p "$OUT"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "$(uname -m)" in x86_64|amd64) CLI_ARCH=amd64;; aarch64|arm64) CLI_ARCH=arm64;; *) echo "不支持的架构" >&2; exit 1;; esac
if command -v proxyctl >/dev/null 2>&1; then
  proxyctl merge --nodes "$NODES_FILE" --output "$OUT"
elif [[ -x "$ROOT/bin/proxyctl-linux-$CLI_ARCH" ]]; then
  "$ROOT/bin/proxyctl-linux-$CLI_ARCH" merge --nodes "$NODES_FILE" --output "$OUT"
elif command -v go >/dev/null 2>&1 && [[ -f "$ROOT/go.mod" ]]; then
  (cd "$ROOT" && go run ./cmd/proxyctl merge --nodes "$NODES_FILE" --output "$OUT")
else
  echo "找不到 proxyctl。请下载完整 release 包，或安装 Go 后执行 make build。" >&2
  exit 1
fi

chown -R caddy:caddy "$SUB_ROOT" 2>/dev/null || true
find "$OUT" -type d -exec chmod 755 {} +
find "$OUT" -type f -exec chmod 644 {} +

echo "统一订阅已生成："
echo "  $OUT/clash.yaml"
echo "  $OUT/sr.txt"
