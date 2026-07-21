#!/usr/bin/env bash
set -euo pipefail

DOMAIN="${1:-}"
EMAIL="${2:-admin@example.com}"
HY2_PORT="${HY2_PORT:-443}"
NODE_NAME="${NODE_NAME:-AWS-HY2}"
SNI="${SNI:-www.bing.com}"
STATE_DIR="/etc/proxy-state"
TOKEN_FILE="/etc/proxy-sub-token"
SECRET_FILE="$STATE_DIR/hy2-secret.env"
CONF_DIR="/etc/sing-box"
SUB_ROOT="/var/www/proxy-sub"

log(){ printf '\n>>> %s\n' "$*"; }
die(){ echo "错误：$*" >&2; exit 1; }
[[ $(id -u) -eq 0 ]] || die "请使用 sudo 运行"
[[ -n "$DOMAIN" ]] || die "用法：sudo bash install-proxy.sh sub.example.com admin@example.com"
[[ "$DOMAIN" =~ ^[A-Za-z0-9.-]+$ ]] || die "域名格式异常"

arch(){ case "$(uname -m)" in x86_64|amd64) echo amd64;; aarch64|arm64) echo arm64;; *) die "不支持的架构";; esac; }

log "安装依赖"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y curl wget tar openssl ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https qrencode

log "检查 TCP 443 占用"
if ss -lntp | grep -qE '[:.]443[[:space:]]' && ! ss -lntp | grep -E '[:.]443[[:space:]]' | grep -q caddy; then
  ss -lntp | grep -E '[:.]443[[:space:]]' || true
  die "TCP 443 已被其他程序占用，请先停止 Xray/Nginx 等服务"
fi

log "安装 sing-box"
SB_VERSION="$(curl -fsSL https://api.github.com/repos/SagerNet/sing-box/releases/latest | sed -n 's/.*"tag_name": "v\([^"]*\)".*/\1/p' | head -1)"
[[ -n "$SB_VERSION" ]] || die "无法获取 sing-box 版本"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
wget -qO "$TMP/sb.tgz" "https://github.com/SagerNet/sing-box/releases/download/v${SB_VERSION}/sing-box-${SB_VERSION}-linux-$(arch).tar.gz"
tar -xzf "$TMP/sb.tgz" -C "$TMP"
install -m 0755 "$TMP/sing-box-${SB_VERSION}-linux-$(arch)/sing-box" /usr/local/bin/sing-box

log "安装 Caddy"
if ! command -v caddy >/dev/null 2>&1; then
  install -d -m 0755 /usr/share/keyrings
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -y
  apt-get install -y caddy
fi

mkdir -p "$STATE_DIR" "$CONF_DIR/cert" "$SUB_ROOT"
if [[ -f "$SECRET_FILE" ]]; then
  # shellcheck disable=SC1090
  source "$SECRET_FILE"
fi
PASSWORD="${PASSWORD:-$(openssl rand -hex 24)}"
OBFS_PASSWORD="${OBFS_PASSWORD:-$(openssl rand -hex 24)}"
printf 'PASSWORD=%q\nOBFS_PASSWORD=%q\n' "$PASSWORD" "$OBFS_PASSWORD" > "$SECRET_FILE"
chmod 600 "$SECRET_FILE"

if [[ -f "$TOKEN_FILE" ]]; then TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"; else TOKEN="$(openssl rand -hex 16)"; printf '%s\n' "$TOKEN" > "$TOKEN_FILE"; chmod 600 "$TOKEN_FILE"; fi
OUT="$SUB_ROOT/$TOKEN"; mkdir -p "$OUT"

log "生成 Hysteria2 证书和配置"
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=$SNI" -keyout "$CONF_DIR/cert/server.key" -out "$CONF_DIR/cert/server.crt" >/dev/null 2>&1
chmod 600 "$CONF_DIR/cert/server.key"
cat > "$CONF_DIR/config.json" <<EOF
{
  "log": {"level":"info","timestamp":true},
  "inbounds": [{
    "type":"hysteria2","tag":"hy2-in","listen":"::","listen_port":$HY2_PORT,
    "users":[{"password":"$PASSWORD"}],
    "obfs":{"type":"salamander","password":"$OBFS_PASSWORD"},
    "tls":{"enabled":true,"server_name":"$SNI","certificate_path":"$CONF_DIR/cert/server.crt","key_path":"$CONF_DIR/cert/server.key"}
  }],
  "outbounds": [{"type":"direct","tag":"direct"}]
}
EOF
sing-box check -c "$CONF_DIR/config.json"

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

PUBLIC_IP="$(curl -fsS4 --max-time 10 https://api.ipify.org || true)"
[[ -n "$PUBLIC_IP" ]] || die "无法获取公网 IPv4"

log "生成单节点订阅"
cat > "$TMP/nodes.conf" <<EOF
$NODE_NAME|$PUBLIC_IP|$HY2_PORT|$PASSWORD|$OBFS_PASSWORD|$SNI
EOF
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if command -v proxyctl >/dev/null 2>&1; then
  proxyctl merge --nodes "$TMP/nodes.conf" --output "$OUT"
elif [[ -x "$ROOT/bin/proxyctl-linux-$(arch)" ]]; then
  "$ROOT/bin/proxyctl-linux-$(arch)" merge --nodes "$TMP/nodes.conf" --output "$OUT"
elif command -v go >/dev/null 2>&1 && [[ -f "$ROOT/go.mod" ]]; then
  (cd "$ROOT" && go run ./cmd/proxyctl merge --nodes "$TMP/nodes.conf" --output "$OUT")
else
  die "找不到 proxyctl。请使用完整 release 包，或安装 Go 后执行 make build"
fi

log "配置 Caddy"
cat > /etc/caddy/Caddyfile <<EOF
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
caddy fmt --overwrite /etc/caddy/Caddyfile >/dev/null
caddy validate --config /etc/caddy/Caddyfile
systemctl enable --now caddy
systemctl restart caddy
chown -R caddy:caddy "$SUB_ROOT" 2>/dev/null || true

cat > /etc/sysctl.d/99-proxy-installer.conf <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
EOF
sysctl --system >/dev/null 2>&1 || true

echo
echo "部署完成"
echo "FlClash:      https://$DOMAIN/$TOKEN/clash.yaml"
echo "Shadowrocket: https://$DOMAIN/$TOKEN/sr.txt"
echo "状态文件: $SECRET_FILE 和 $TOKEN_FILE"
echo "注意：AWS 安全组需放行 TCP 80/443 和 UDP 443。"
