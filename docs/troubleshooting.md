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

## 订阅中心角色检测显示"不确定"

v0.10 起，涉及 Reality 节点或发布的命令会先判定本机是否为订阅中心。判定结果为“不确定”时，输出会逐项列出缺失或异常的文件；两种常见原因：

- **Reality 节点机上误创建了 `/etc/singbox-sub-manager/nodes.conf`**：说明在节点机上误执行过订阅中心的 CRUD 命令。删除该文件即可恢复正常判定；不要在 Reality 节点机上手工创建它。
- **v0.4 之前安装、缺少 `install.json` 的订阅中心**：`install.json` 是 v0.4 之后才写入的角色判定依据。重新执行一次 `sudo bash install-proxy.sh install` 即可补写该文件，不影响已有节点和订阅内容。

判定为“否”（不是订阅中心）本身不是错误，只表示本机既不是订阅中心也没有 Reality 节点相关的旧文件残留。
