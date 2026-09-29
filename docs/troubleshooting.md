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

Reality 节点机上用 `sudo proxyctl node service recover` 处理（订阅中心的发布事务由后续版本的 `publish --recover` 处理）；`node service show` 等命令在这种状态下返回 1 并提示存在未完成事务。

- **不要手工删除事务区或墓碑目录，也不要手工编辑其中的 `journal.json`**：快照可能含私钥，且手工修改会破坏恢复所依赖的记录。
- 执行 `sudo proxyctl node service recover`：残留在“执行中/回滚中”的事务被回滚（并恢复 systemd 状态和服务，原本运行的会 restart 并重跑门禁）；已提交的事务只做向前清理；墓碑目录直接整体删除；只含 `journal.json.tmp` 或为空的事务区被丢弃。成功返回 0。recover 本身被中断后可以直接再执行一次。
- 返回 **5**：保留了事务区和现场，stderr 会给出事务区路径。原因是：某个受管路径或 unit 的当前状态不属于该阶段允许的任何一种（内容被外部修改、目录被替换、存在未登记的临时文件），journal 无法识别（未知 schema——请用 journal 里 `proxyctl_version` 所记录的版本执行 recover）或状态序列非法，事务区里除 `journal.json.tmp` 外还有别的东西却没有 `journal.json`，恢复后的本机门禁失败，或回滚超过 120 秒。先人工核对现场；确认问题已排除后重新执行 `recover`。
- 恢复后的门禁失败、但文件已经全部恢复到事务前状态时，可以用 `sudo proxyctl node service recover --abandon` 放弃：它不重启服务、不跑门禁，只删除事务区并保持 systemd 当前状态（返回 0，stderr 警告服务可能没有运行）；之后请人工检查 `systemctl status proxyctl-reality`，必要时执行 `reinstall`。任何受管路径尚未回到事务前状态时 `--abandon` 返回 1 且不做任何修改。
- 不要凭猜测手工修复；需要求助时保留现场，且不要把事务区内容（可能含私钥）贴到 issue 或日志里。
- 已知限制（真实 systemd 上尚未验证，待 Task 13 真机验收）：uninstall/purge 在 unit 文件已删除之后、`daemon-reload` 完成之前发生崩溃，或 `daemon-reload` 调用本身失败时，回滚的降级步骤会对已不存在的 unit 文件执行 `systemctl disable`，真实 systemd 可能拒绝，结果是返回 5（而不是 4 或 0）、现场原样保留。遇到 5 时按上面的通用做法处理：保留现场，不要手工编辑或删除事务区和 `journal.json`，收集 `systemctl status proxyctl-reality` 的输出（不含私钥等敏感内容）后求助。
