# Troubleshooting

## Caddy 无法启动

检查 TCP 443：

```bash
sudo ss -lntp | grep ':443'
sudo journalctl -u caddy -n 100 --no-pager
```

若 Xray 占用：

```bash
sudo systemctl disable --now xray
sudo systemctl restart caddy
```

## Hysteria2 无法连接

检查 UDP 443、安全组和 sing-box：

```bash
sudo ss -lnup | grep ':443'
sudo systemctl status sing-box --no-pager
sudo journalctl -u sing-box -n 100 --no-pager
```

## Google Play 规则模式无法下载

确认 Google Play、`googleapis.com`、`gvt1.com`、`gvt2.com` 和 `googleusercontent.com` 规则位于 CN/DIRECT 规则之前，然后在客户端重新加载配置并清理 DNS 缓存。
