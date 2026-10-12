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

## 交互菜单（`sudo proxyctl node`）里某些项显示“不可用”

菜单的“不可用”后面写着原因，依据是订阅中心角色（[规格 §4.2](superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md)）与 Reality 实例状态（§4.3）：

- “订阅中心角色不确定：…”：按上一节处理（删除节点机上误创建的 `nodes.conf`，或在 v0.4 之前安装的订阅中心上重新运行 `install-proxy.sh install` 补写 `install.json`）后重新打开菜单。
- “本机是订阅中心”/“本机不是订阅中心”：安装、重装只能在节点机上做；添加节点、管理节点、重新发布只能在订阅中心上做。
- “存在未完成的事务”/“存在未完成的发布事务”：按角色行下方给出的命令恢复（`sudo proxyctl node service recover` 或 `sudo proxyctl node publish --recover`），见下一节和再下一节。
- “实例状态不一致：…”：菜单列出不一致的路径，需要人工处理，没有自动修复。

选择添加/管理/重新发布时菜单提示“默认节点文件仍是旧格式”：安装器新装的订阅中心写出的是旧竖线格式，发布要求分节格式。菜单会显示备份位置 `/etc/singbox-sub-manager/nodes.conf.bak`（已有的同名备份会被覆盖），你确认后才执行 `node migrate`。如果你拒绝、输入结束或迁移失败（例如取锁超时），原 `nodes.conf` 保持不变，菜单返回 1，不会继续注册或发布；可以随后手工执行 `sudo proxyctl node migrate` 再重新打开菜单。`nodes.conf` 完全为空（没有任何节点）时菜单不会提示迁移，发布会按“非分节格式”被拒绝；先用不带 `--publish` 的 `sudo proxyctl node add ...` 添加一个节点（写出的就是分节格式），再回到菜单。

## 崩溃后残留的事务目录（`*-txn` / `.proxyctl-*.tomb-*`）

v0.10 起，Reality 实例生命周期操作与订阅中心发布共用同一套持久化事务引擎：任何中途中断（进程被杀、系统断电、取消）都会在磁盘上留下事务区（例如 `/var/lib/proxyctl-reality-txn/`、`/var/lib/singbox-sub-manager/publish/`）或墓碑目录（`.proxyctl-reality-txn.tomb-<txn_id>`、`.publish.tomb-<txn_id>`），直到下一次自动回滚或 `recover` 把它清理干净。

Reality 节点机上用 `sudo proxyctl node service recover` 处理；订阅中心上的发布事务用 `sudo proxyctl node publish --recover` 处理（见下一节）。`node service show` 等命令在这种状态下返回 1 并提示存在未完成事务。

- **不要手工删除事务区或墓碑目录，也不要手工编辑其中的 `journal.json`**：快照可能含私钥，且手工修改会破坏恢复所依赖的记录。
- 执行 `sudo proxyctl node service recover`：残留在“执行中/回滚中”的事务被回滚（并恢复 systemd 状态和服务，原本运行的会 restart 并重跑门禁）；已提交的事务只做向前清理；墓碑目录直接整体删除；只含 `journal.json.tmp` 或为空的事务区被丢弃。成功返回 0。recover 本身被中断后可以直接再执行一次。
- 返回 **5**：保留了事务区和现场，stderr 会给出事务区路径。原因是：某个受管路径或 unit 的当前状态不属于该阶段允许的任何一种（内容被外部修改、目录被替换、存在未登记的临时文件），journal 无法识别（未知 schema——请用 journal 里 `proxyctl_version` 所记录的版本执行 recover）或状态序列非法，事务区里除 `journal.json.tmp` 外还有别的东西却没有 `journal.json`，恢复后的本机门禁失败，或回滚超过 120 秒。先人工核对现场；确认问题已排除后重新执行 `recover`。
- 恢复后的门禁失败、但文件已经全部恢复到事务前状态时，可以用 `sudo proxyctl node service recover --abandon` 放弃：它不重启服务、不跑门禁，只删除事务区并保持 systemd 当前状态（返回 0，stderr 警告服务可能没有运行）；之后请人工检查 `systemctl status proxyctl-reality`，必要时执行 `reinstall`。任何受管路径尚未回到事务前状态时 `--abandon` 返回 1 且不做任何修改。
- 不要凭猜测手工修复；需要求助时保留现场，且不要把事务区内容（可能含私钥）贴到 issue 或日志里。
- 已知限制（真实 systemd 上尚未验证，待 Task 13 真机验收）：uninstall/purge 在 unit 文件已删除之后、`daemon-reload` 完成之前发生崩溃，或 `daemon-reload` 调用本身失败时，回滚的降级步骤会对已不存在的 unit 文件执行 `systemctl disable`，真实 systemd 可能拒绝，结果是返回 5（而不是 4 或 0）、现场原样保留。遇到 5 时按上面的通用做法处理：保留现场，不要手工编辑或删除事务区和 `journal.json`，收集 `systemctl status proxyctl-reality` 的输出（不含私钥等敏感内容）后求助。

## 订阅发布事务（`proxyctl node publish`）的中断与恢复

`proxyctl node publish` 和带 `--publish` 的 `node add/edit/enable/disable/remove` 把 `nodes.conf`、`clash.yaml`、`sr.txt` 作为一个持久化事务发布（用法与退出码见 [cli-reference.md](cli-reference.md)）。

### 原子性声明（请如实理解）

只承诺两点：**每个文件单独原子替换**；**整个事务可恢复**。`clash.yaml` 和 `sr.txt` 之间存在短暂的新旧并存窗口，客户端可能在这个窗口内拿到一新一旧两个文件；**不是**对外整体原子的发布。发布失败或被中断时，三个文件都会恢复到事务前的内容、权限和属主（原本不存在的文件会被删除）。

### 常见结果

- **退出码 4**：已自动回滚。最常见的原因是 Caddy 没有运行、证书异常，或 HTTPS 返回的状态不是 200、正文与刚写入的文件不一致（stderr 会说明是哪个文件、哪种差异）。验证是通过 `127.0.0.1:443` 并以订阅域名作 SNI/Host 完成的，与公网 DNS 无关：先看 `systemctl status caddy`、`proxyctl health`，修好后重新执行同一条命令（`add` 等命令幂等，可以重复执行）。回滚成功只按本地三个文件的状态判定，事务区已经删除；回滚后会再做一次 HTTPS 复验，复验失败只在 stderr 告警，不影响退出码，也不会遗留事务区。
- **退出码 1**：修改开始之前被拒绝，没有任何变化。常见原因：`nodes.conf` 仍是旧竖线格式（先执行 `sudo proxyctl node migrate`）；发布目录 `<subscription_root>/<token>` 不存在（重新运行安装器，发布不会创建它）；结果里没有任何启用节点；`--min-version` 高于本机版本；锁被占用（另一个发布、backup、restore、monitor 或安装脚本正在运行）；`dirname(subscription_root)`（通常是 `/var/www`）对 group/other 可写或不属于 root；`nodes.conf`、订阅文件或目录是符号链接。
- **monitor 可能有一轮显示 failed**：发布持有恢复锁（L2）期间（最长约为 HTTPS 验证超时加渲染时间），`proxyctl monitor` 那一轮会因为取不到锁返回 3，对应的 systemd unit 那一轮显示为 failed，下一轮自动恢复正常。
- **`.bak` 的差异**：发布路径**不**产生 `nodes.conf.bak`，回滚以事务快照为准（事务结束后快照随事务区删除）；不带 `--publish` 的 `node add/edit/...` 仍调用原来的写入函数，照旧产生 `.bak`。

### 遗留的发布事务（事务区 `/var/lib/singbox-sub-manager/publish/` 或墓碑 `.publish.tomb-<txn_id>`）

进程被杀、断电，或提交后清理失败时，会在 `/var/lib/singbox-sub-manager/` 下留下事务区或墓碑。此时下列命令都会拒绝执行并返回 1，提示先恢复：`proxyctl node publish`、写默认 `nodes.conf` 的 `node add/edit/enable/disable/remove/migrate`（不带 `--publish` 也一样）、独立执行的 `proxyctl restore`、`install-proxy.sh` 的 install/update/rollback、`merge-nodes.sh`。恢复：

```bash
sudo proxyctl node publish --recover
```

- 处在“执行中/回滚中”的事务被回滚：三个文件恢复到事务前状态，staging 目录被删除，成功返回 0，并再做一次仅告警的 HTTPS 复验；已提交的事务只做向前清理（删除 staging 和事务区），已发布的内容保留；墓碑整体删除（不读取其内容）；只含 `journal.json.tmp` 或为空的事务区被丢弃；没有任何遗留时也返回 0。`recover` 自己被中断后可以直接再执行一次。
- 返回 **5**：保留了事务区，stderr 给出路径。原因是三个受管文件或 staging 的当前状态不属于该阶段允许的任何一种（例如 `restore` 或人手在两次操作之间改了文件、目录被替换、存在未登记的 `.proxyctl-txn-*` 临时文件、journal 的 schema 未知——请用 journal 里 `proxyctl_version` 所记录的版本执行 recover）或回滚超过 120 秒。5 不会修改任何路径和 journal。
- `sudo proxyctl node publish --recover --abandon`：仅当三个文件和 staging 都已经回到事务前状态、且没有未登记临时名时才可用；它直接删除事务区（返回 0），不做任何其他动作。条件不满足（包括已提交的事务）返回 1，不做任何修改。
- **仍然返回 5 时的人工出口**（例如路径被上面几类以外的程序改写，`--abandon` 又因为路径不在事务前状态而不可用）：先人工核对 `nodes.conf`、`<subscription_root>/<token>/clash.yaml` 和 `sr.txt` 的当前内容，确认**可以接受当前状态**之后，删除 `/var/lib/singbox-sub-manager/publish/` 和 `/var/lib/singbox-sub-manager/.publish.tomb-*` 目录，再执行一次 `sudo proxyctl node publish`，由它按当前的 `nodes.conf` 重新渲染并发布三个文件。同时清理 `dirname(subscription_root)`（通常是 `/var/www`）下残留的 `.proxyctl-publish-*` staging 目录，以及发布目录和 `/etc/singbox-sub-manager/` 里残留的 `.proxyctl-txn-*` 临时文件。事务区的快照含节点凭据：删除前不要把其中内容贴到 issue 或日志里。这是最后手段：只有在 `--recover` 与 `--abandon` 都不可用时才这样做。

### 已知并发限制

“发布与 backup/restore/monitor 串行”只对 `proxyctl node publish`、带 `--publish` 的变更和 `merge-nodes.sh` 成立，**不是**全局保证：`install-proxy.sh` 在只持有安装锁时调用 `proxyctl merge` 写订阅文件、用户手动执行的 `proxyctl merge` / `subscription build`（不加锁）、以及写默认 `nodes.conf` 的兼容模式 CRUD（只取安装锁）与独立 `restore`（取恢复锁和 Reality 节点锁）之间，仍可能并发写入——这些是已知的旧路径，本版本不解决。因此把 `proxyctl node publish` 作为标准发布入口。
