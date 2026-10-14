# CLI 参数参考

本页收录 `proxyctl` 严格模式命令的参数全集（规格 §4.5）。当前包含 `node service install|reinstall|show`（Task 7）、`uninstall`、`recover`（Task 8），以及 `node publish` 和带 `--publish` 的 `node add/edit/enable/disable/remove`（Task 9）。`bootstrap-node.sh` 首次安装模式的参数见文末（Task 11）；升级模式由 Task 12 交付后补充。

旧的 `node list/add/edit/remove/enable/disable/migrate`（不带 `--publish`）解析、输出和 `--nodes PATH` 语义不变，不在本页范围内；用法见 `proxyctl node <子命令> --help` 或 README。

## 严格模式规则（适用于本页全部命令）

- 完整解析全部参数（含 `--help` 和取值校验）先于任何 root 检查、加锁或网络请求；解析失败（用法错误，包括取值不合法，如端口越界、IPv6 的 `--server`、IP 形式的 `--sni`、非法节点名）统一返回退出码 2，不检测 root 和角色、不取锁、不发起网络请求、不修改任何文件。
- 未知 flag、重复 flag、缺少值：返回 2。
- 布尔 flag 只接受 `--x` 或 `--x=true|false` 两种写法。
- 位置参数个数必须与该子命令定义的完全一致；`node service` 全部子命令、`node publish` 和 `node add --publish` 都没有位置参数，`node edit/enable/disable/remove --publish` 恰好一个（节点名）。
- 以 `-` 开头的值（例如节点名）一律拒绝。
- `--help`/`-h` 作为唯一参数时输出该子命令用法并返回 0；与其他参数同时出现时按未知 flag 处理，返回 2。
- flag 可以出现在其他参数之前或之后。

## `proxyctl node service`

顶层用法：`proxyctl node service <install|reinstall|show|uninstall|recover> [args]`；`proxyctl node service --help` 或 `-h` 列出全部子命令，返回 0。

### `install`

```
proxyctl node service install --name NAME --server ADDR [--port PORT] [--sni SNI]
```

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `--name` | 字符串 | 是 | — | 节点名，规则同 `nodes.Validate`；不能以 `-` 开头 |
| `--server` | 字符串 | 是 | — | IPv4 字面量或主机名（≤253 字符，每段 1–63 个 `[A-Za-z0-9-]`，不以 `-` 开头/结尾，至少一个 `.`）；不接受 IPv6 字面量 |
| `--port` | 整数 | 否 | `443` | 1–65535 |
| `--sni` | 字符串 | 否 | `www.microsoft.com` | 规则同 `--server` 的主机名规则，但不能是 IP 字面量 |

无位置参数。执行顺序：解析并校验取值 → root → 角色 → 快速预检（含 dest 探测）→ 取 L1、L3 → 角色与状态复查 → 事务。退出码见下表。

### `reinstall`

```
proxyctl node service reinstall [--singbox-version V --singbox-sha256 H]
```

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `--singbox-version` | 字符串 | 与 `--singbox-sha256` 成对 | 显式指定 sing-box 版本；省略时使用本次 `proxyctl` 编译期固定的版本 |
| `--singbox-sha256` | 字符串 | 与 `--singbox-version` 成对 | 对应架构 tarball 的 SHA256；只给出其中一个返回 2 |

不接受 `--name`、`--server`、`--port`、`--sni`（严格模式下这些是未知 flag，返回 2）。无位置参数。

### `show`

```
proxyctl node service show
```

只读，无参数，无位置参数。

### `uninstall`

```
proxyctl node service uninstall [--purge [--export-backup PATH]]
```

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `--purge` | 布尔（`--purge` 或 `--purge=true|false`） | 否 | 彻底清除：删除身份、manifest 和状态目录；不带时保留身份（“已卸载但保留身份”） |
| `--export-backup` | 字符串（路径） | 否 | 只能与 `--purge` 一起使用（否则返回 2）；空值返回 2；相对路径按当前工作目录解析；PATH 已存在返回 1，不覆盖 |

无位置参数；`--abandon` 不属于本命令，出现返回 2。执行顺序：解析 → root → 快速预检 → 取 L1、L3 → 复查实例状态 → （`--export-backup`）导出 → 事务。**不检测订阅中心角色。** stdout：不带 `--purge` 输出 disable（备选注释的 remove）；带 `--purge` 输出 remove（备选注释的 disable）。归档内容、限制和不受支持的解压恢复见 [docs/reality-node.md](reality-node.md)，归档含私钥见 [SECURITY.md](../SECURITY.md)。

### `recover`

```
proxyctl node service recover [--abandon]
```

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `--abandon` | 布尔（`--abandon` 或 `--abandon=true|false`） | 否 | 人工放弃出口：仅当全部受管路径都已处于事务前状态时可用，不重启、不跑门禁，stderr 警告服务可能没有运行；条件不满足返回 1 |

无位置参数。只在“未完成事务”状态可用，否则返回 1。`--abandon` 只允许出现在 `node service recover` 和 `node publish --recover`；在 `install`、`reinstall`、`show`、`uninstall` 上出现一律返回 2（不取锁、不做任何 IO）。成功（回滚、向前清理、丢弃空事务区或墓碑）都返回 0，stdout 无输出；检测到篡改、未知 journal schema、门禁失败或超时返回 5 并输出事务区路径。

## `node publish` 与带 `--publish` 的 CRUD

订阅中心把节点变更发布成可恢复的事务：`nodes.conf`、`clash.yaml`、`sr.txt` 三个文件在同一个持久化事务里逐个替换，通过本机 Caddy 验证后提交，任何一步失败都回滚到事务前状态（规格 §10）。标准发布入口是 `proxyctl node publish`；`proxyctl merge` 与 `proxyctl subscription build` 保留为兼容接口。

```
proxyctl node publish [--min-version vX.Y.Z]
proxyctl node publish --recover [--abandon] [--min-version vX.Y.Z]
proxyctl node add --publish [--min-version vX.Y.Z] --name NAME --server ADDR --port PORT --sni SNI
                  [--type hysteria2|vless-reality] [--password P --obfs-password O | --uuid U --public-key K [--short-id S]]
                  [--enabled[=true|false]]
proxyctl node edit    --publish [--min-version vX.Y.Z] NAME [--name NEW] [--server ADDR] [--port PORT] [--sni SNI]
                  [--password P] [--obfs-password O] [--uuid U] [--public-key K] [--short-id S] [--enabled[=true|false]]
proxyctl node enable|disable|remove --publish [--min-version vX.Y.Z] NAME
```

模式判定（规格 §4.5）：对 `node add/edit/enable/disable/remove`，按该子命令的已知 flag 表逐个 token 扫描，需要取值且没写成 `=` 形式的 flag 会跳过下一个 token；在 flag 位置上遇到 `--publish` 或 `-publish` 才是严格模式。因此 `--name --publish` 里的 `--publish` 是 `--name` 的值，命令仍按原来的兼容模式执行（节点名就叫 `--publish`）。`node publish` 始终是严格模式。

| 参数 | 适用命令 | 说明 |
|---|---|---|
| `--publish` | `node add/edit/enable/disable/remove` | 只接受 `--publish`（不接受 `--publish=...`）；`-publish`（单横线）会触发严格模式，但因拼写不合法最终返回 2，不会回退到兼容模式。`node publish` 本身没有这个 flag |
| `--min-version vX.Y.Z` | 以上全部 | 要求本机 `proxyctl` 版本不低于该值。版本格式 `vMAJOR.MINOR.PATCH`，按数值逐段比较（`v0.10.10` 高于 `v0.10.9`）；本机版本不是这种格式（例如开发构建 `dev`）一律视为无法证明兼容，返回 1；取值本身格式不合法返回 2。只在严格模式下有效；不带它时不做版本检查 |
| `--recover` | `node publish` | 处理遗留的发布事务（回滚，或对已提交的事务只做向前清理；墓碑整体删除；只含 `journal.json.tmp` 或为空的事务区被丢弃）。没有遗留事务时直接返回 0 |
| `--abandon` | `node publish --recover` | 人工放弃出口：仅当三个受管文件和 staging 都已处于事务前状态、且没有未登记临时名时才删除事务区；已提交的事务、条件不满足、没有事务可放弃，均返回 1 且不做任何修改。单独使用（不带 `--recover`）返回 2 |
| `--nodes` | — | 与 `--publish` 同时出现返回 2：发布始终作用于标准布局的默认 `/etc/singbox-sub-manager/nodes.conf` |
| 节点字段 flag | `add`、`edit` | 含义与不带 `--publish` 的同名命令相同。严格模式不交互补问：缺少必填字段（`add` 的 `--name --server --port --sni`，以及 hysteria2 的 `--password --obfs-password` 或 vless-reality 的 `--uuid --public-key`）返回 2，即使 stdin 是 TTY；`--short-id ''` 合法。`add` 的 hysteria2 与 vless-reality 专属 flag 互斥（返回 2）；`edit` 对节点类型不适用的字段 flag 在读取节点后以 1 拒绝 |

**不带 `--publish` 的 CRUD 保持原样**：解析、输出、`--nodes PATH` 语义、`.bak`、退出码都不变，不检测角色、不输出角色行。发布路径和它的区别：作用于固定的默认 `nodes.conf`；不产生 `.bak`（回滚依据是事务快照）；先在内存里算出结果，再渲染、校验、替换、验证。

### 执行顺序

1. 完整解析 argv（取值格式、`--min-version`、`add` 的节点字段校验）。用法错误返回 2，节点字段本身不合法（例如 UUID 格式错误）返回 1；两者都发生在 root 检查、加锁和任何网络请求之前。
2. root、订阅中心角色为“是”（角色行写到 stderr，stdout 保持命令自己的输出）。
3. 快速预检（不修改任何东西）：`--min-version`；`install.json` 的 `domain`、`subscription_root`、`token_file`（不读取任何其他来源，也不创建它们）；默认 `nodes.conf` 是分节格式（旧竖线格式提示先执行 `proxyctl node migrate`）；发布目录 `<subscription_root>/<token>` 存在且是目录（缺失时拒绝并提示重新运行安装器，发布不创建它）；`dirname(subscription_root)` 属于 root 且 group/other 不可写；叶子和整条目录链上没有符号链接；没有遗留的发布事务区或墓碑（有则提示 `publish --recover`）。
4. 取 L1，再取 L2（各最长等待 30 秒，等待期间收到信号立即返回 1），然后重新检查第 2、3 步。
5. 在内存里计算新的节点列表（见下表），渲染 `clash.yaml` 与 `sr.txt`。
6. 开始事务（`/var/lib/singbox-sub-manager/publish/`），登记全部步骤；在 `dirname(subscription_root)` 下创建 staging 目录 `.proxyctl-publish-<txn_id>`（root:root、0700，不在 Caddy 的服务根目录之下），把渲染结果写入 staging，运行与 `proxyctl validate` 相同的校验（同一份实现）。校验失败：删除 staging 和事务区，返回 1。
7. 把存在的文件快照进事务区，然后依次对 `nodes.conf`（0600，root:root；节点列表未变化时跳过）、`clash.yaml`、`sr.txt`（0644，属主沿用前状态，前状态不存在时用 `caddy:caddy`）执行 `write_file`：临时文件创建在目标文件自己的目录里，写入、`fchmod`、`fchown`、fsync 后 `rename` 并 fsync 目录；不从 staging 跨文件系统 rename。
8. 验证：回读发布目录，两个订阅文件的 SHA256 必须与 staging 相同；再通过 HTTPS 分别获取 `/<token>/clash.yaml` 和 `/<token>/sr.txt`（TLS 的 SNI 和 Host 都是 `install.json` 的域名，实际连接 `127.0.0.1:443`），都必须返回 200 且正文 SHA256 与 staging 相同；每个文件超时 10 秒。
9. 提交，删除 staging，删除事务区（快照含节点凭据）。提交之后只有清理，不再回滚。

### 幂等与拒绝规则

| 命令 | 情形 | 结果 |
|---|---|---|
| `add` | 同名节点字段完全相同且已启用 | 不修改 `nodes.conf`，直接重新发布，返回 0（重复注册安全） |
| `add` | 同名、字段相同但已禁用 | 返回 1，提示用 `enable` |
| `add` | 同名但字段不同 | 返回 1，提示用 `edit` |
| `enable` / `disable` | 节点已处于目标状态 | 不修改 `nodes.conf`，直接发布，返回 0 |
| `remove` | 节点不存在 | 不修改 `nodes.conf`，直接发布，返回 0 |
| `edit` / `enable` / `disable` | 节点不存在 | 返回 1 |
| 任意 | 结果中没有任何启用节点（例如 `disable`/`remove` 最后一个启用节点） | 返回 1，提示先添加或启用另一个节点；不发布空订阅 |

以上拒绝都发生在事务开始之前，不做任何修改。

### 发布命令的退出码

取值与下表相同（规格 §13.1），在发布命令上的具体含义：**0** 发布成功（包括幂等空操作，以及 `--recover` 的任何成功结果）；**1** 第一个受管路径被修改之前失败——用法之外的预检、角色、版本、锁超时（含等待锁期间收到信号）、渲染校验失败、遗留事务等；**2** 用法错误；**4** 修改已开始后失败（含 Caddy/HTTPS 验证失败、验证超时、收到信号），已自动回滚到事务前状态，且本地三个文件的内容、权限属主和“原本不存在”都已核对恢复，事务区已删除；**5** 需人工介入：检测到外部篡改、回滚失败，或提交后清理未完成，保留事务区并在 stderr 输出路径。回滚成功只按**本地状态**判定；回滚后再做一次 HTTPS 复验，期望值为事务前的哈希（原本不存在的文件跳过），复验失败**只**在 stderr 告警，不改变退出码，也不保留事务区。

### 退出码

| 退出码 | 含义 |
|---|---|
| 0 | 成功（可能伴随告警，写到 stderr）；`recover` 的任何成功结果都是 0 |
| 1 | 用法之外的失败，且发生在第一个受管资产被修改之前：root 或角色不符、快速预检失败（含 dest 探测）、锁超时或等待锁期间收到信号、取锁后复查失败、下载或 `check` 失败、`--export-backup` 失败、`recover --abandon` 条件不满足 |
| 2 | 用法错误：严格解析失败、必填参数缺失、`--singbox-version`/`--singbox-sha256` 未成对出现、`--export-backup` 缺少 `--purge`、`--abandon` 出现在 `recover` 之外等 |
| 4 | 已自动回滚到事务前状态 |
| 5 | 需人工介入：事务已经开始而无法确认已恢复（外部篡改、回滚失败或超时、恢复后复查失败、提交后清理未完成），保留事务区并在 stderr 输出其路径 |

## `proxyctl node`（无参数）：交互菜单

`proxyctl node` 不带任何参数时：

| 条件 | 行为 |
|---|---|
| stdin 与 stdout 都是终端，且是 root | 打开菜单（见下） |
| stdin 与 stdout 都是终端，但不是 root | stderr 提示用 `sudo proxyctl node` 重新运行，返回 1；不读取角色和实例状态，不读取输入 |
| 任一端不是终端 | 输出 `usage: proxyctl node ...` 到 stderr，返回 2（与 v0.9 相同） |

菜单展示 7 个操作和“退出”，每个操作最终都调用与命令行**同一个**入口（`proxyctl node <子命令> ...`），不存在独立的写入路径：

| 项 | 等价命令 | 说明 |
|---|---|---|
| 1 安装 | `sudo proxyctl node service install --name 'N' --server 'S' --port 'P' --sni 'D'` | 公网 IPv4 探测结果只作 `--server` 的默认值，`--port`（默认 443）和 `--sni`（默认 `www.microsoft.com`）同样须确认；取值不合法在确认之前直接返回 2 |
| 2 显示 | `... node service show` | 任何状态下都可选，由命令自己报告状态 |
| 3 重装 | `... node service reinstall` | 状态为已安装或已卸载但保留身份，且订阅中心为“否” |
| 4 卸载 | `... node service uninstall [--purge [--export-backup 'PATH']]` | 已安装可选“保留身份”或“彻底清除”；已卸载但保留身份只能彻底清除；彻底清除时再询问是否导出备份 |
| 5 添加 | `... node add --publish --type vless-reality --server 'S' --port 'P' --uuid 'U' --public-key 'K' --short-id 'I' --sni 'D' --name 'N'` | 只支持 vless-reality；`--short-id` 留空时打印为 `''` |
| 6 管理 | `... node edit --publish [--name ..] [--server ..] [--port ..] [--sni ..] [--uuid ..] [--public-key ..] [--short-id ..] 'NAME'`；`... node enable/disable/remove --publish 'NAME'` | 先列出默认 `nodes.conf` 的节点；edit 只发送你修改过的字段（回车保持不变；short id 输入 `-` 清空）；没有任何修改则取消 |
| 7 发布 | `... node publish` | |

约定：

- 流程是“收集全部输入 → 打印等价命令（flag 在节点名之前，参数值单引号，空值 `''`）→ 询问确认 → 调用命令”。**确认之前不取锁、不写入**；所有确认默认“否”（`[y/N]`），只有只读的“显示”（`node service show`）默认“是”（`[Y/n]`），直接回车即执行。回答 `n`（或任何不是 `y`/`yes` 的内容）、在默认“否”的确认处直接回车、输入结束（EOF），或在选择类提示处输入 `0`/`q`，都在没有任何副作用的情况下取消，返回 0。确认之后的取消（锁等待、事务中收到 SIGINT/SIGTERM）完全由被调用的命令按 §13.3 处理，菜单没有自己的信号处理。
- 菜单返回被调用命令的退出码（0/1/2/4/5 的含义见各命令）；报错原样输出，菜单不改写。菜单自己发现的输入问题在确认之前报告：install 的参数不合法、端口不是整数、节点名以 `-` 开头返回 2；添加/编辑的节点字段语义不合法返回 1（与 `--publish` 命令自己的预检一致）。
- 不可用项和原因按规格 §4.2、§4.3：安装/重装要求订阅中心为“否”，添加/管理/发布要求订阅中心为“是”；角色“不确定”时这些项都不可用并列出原因；存在未完成的发布事务时添加/管理/发布不可用。
- 添加/管理/发布前，如果默认 `nodes.conf` 仍是旧格式：显示备份位置 `nodes.conf.bak`，请求确认后执行 `proxyctl node migrate`（沿用迁移的备份语义和默认 `nodes.conf` 的锁）。拒绝、EOF 或迁移失败：原文件保持不变，不继续注册或发布，返回 1（迁移失败时返回迁移命令自己的退出码）。非交互命令（`node publish`、`--publish` 的 CRUD）遇到旧格式仍然只拒绝（返回 1），不会自动迁移。

详细的命令行为、粘贴输出格式和已知限制见 [docs/reality-node.md](reality-node.md)。

## `bootstrap-node.sh`（首次安装模式）

```text
sudo bash bootstrap-node.sh
sudo bash bootstrap-node.sh [--replace-proxyctl] --name NAME --server ADDR [--port PORT] [--sni SNI]
```

脚本自己只认识 `--replace-proxyctl`；其余参数不解释，原样传给 `proxyctl node service install`（解析、取值校验和退出码以该命令为准，见上文 `install`）。

| 形式 | 行为 |
|---|---|
| 无参数，stdin 与 stdout 都是终端 | `proxyctl` 确认后 `exec proxyctl node`（交互菜单） |
| 无参数，任一端不是终端 | `proxyctl` 确认后输出用法，返回 2 |
| 带参数 | 去掉 `--replace-proxyctl`，其余参数逐字节不变地 `exec proxyctl node service install ...` |

- `--replace-proxyctl`：本机 `/usr/local/bin/proxyctl` 的 `.sha256` 缺失或与二进制不符时，授权重新安装固定版本的 `proxyctl`。它不会替换或降级校验通过的 `proxyctl`，也不会传给 `proxyctl`。可以和其他参数同时出现，位置不限。
- 固定版本（`v0.10.0`）是脚本内置的，不接受环境变量或参数覆盖。下载每个文件限时 120 秒，锁等待最长 30 秒。
- 退出码：`1` 修改之前失败（非 root、OS 或架构不支持、订阅中心角色不是“否”、取锁超时、下载或校验失败、本机 `proxyctl` 决策表要求停止）；`2` 用法错误；带参数或进入菜单时，其余退出码就是被 exec 的 `proxyctl` 命令的退出码。
- 决策表、角色预检和锁顺序见 [reality-node.md](reality-node.md) 的“首次安装”一节。
