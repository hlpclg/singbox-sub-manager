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

## 崩溃后残留的事务目录（`*-txn` / `.proxyctl-*.tomb-*`）

v0.10 起，Reality 实例生命周期操作与订阅中心发布共用同一套持久化事务引擎：任何中途中断（进程被杀、系统断电、取消）都会在磁盘上留下事务区（例如 `/var/lib/proxyctl-reality-txn/`、`/var/lib/singbox-sub-manager/publish/`）或墓碑目录（`.proxyctl-reality-txn.tomb-<txn_id>`、`.publish.tomb-<txn_id>`），直到下一次自动回滚或 `recover` 把它清理干净。

**尚未接入任何用户可调用的命令**——`service recover`、`publish --recover`、`--abandon` 及其退出码含义由后续版本接入后才会出现在 `cli-reference.md`。在那之前：

- 看到上述目录残留，说明有一次操作未能正常完成；不要手工删除事务区或墓碑目录本身（其中的快照可能含私钥），也不要手工编辑其中的 `journal.json`。
- 引擎在设计上保证：只要事务区完整存在，重新执行一次识别它的操作（后续版本会是显式的 `recover`）就能把本机恢复到执行前状态，或在确认已提交后完成收尾清理。只有满足以下任一条件才会拒绝自动处理并保留现场：journal 本身无法识别（未知 schema）或结构非法（状态序列不合法）；某个受管路径或 unit 的当前状态不属于该阶段允许的任何一种（例如内容被外部修改、目录被替换成另一个真实目录、存在未登记的临时文件）。这些情况都需要人工核对现场后再处理，必要时对照 journal 中的 `proxyctl_version` 字段确认写入版本。
- 如遇到残留目录且暂无 `recover` 命令可用，请保留现场并联系维护者，不要凭猜测手工修复。
