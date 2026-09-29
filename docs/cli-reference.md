# CLI 参数参考

本页收录 `proxyctl` 严格模式命令的参数全集（规格 §4.5）。当前包含 `node service install|reinstall|show`（Task 7）和 `uninstall`、`recover`（Task 8）。`node publish` 和带 `--publish` 的 `node add/edit/enable/disable/remove` 由 Task 9 交付后补充到本页；`bootstrap-node.sh` 的参数由 Task 11–12 交付后补充。

旧的 `node list/add/edit/remove/enable/disable/migrate`（不带 `--publish`）解析、输出和 `--nodes PATH` 语义不变，不在本页范围内；用法见 `proxyctl node <子命令> --help` 或 README。

## 严格模式规则（适用于本页全部命令）

- 完整解析全部参数（含 `--help` 和取值校验）先于任何 root 检查、加锁或网络请求；解析失败（用法错误，包括取值不合法，如端口越界、IPv6 的 `--server`、IP 形式的 `--sni`、非法节点名）统一返回退出码 2，不检测 root 和角色、不取锁、不发起网络请求、不修改任何文件。
- 未知 flag、重复 flag、缺少值：返回 2。
- 布尔 flag 只接受 `--x` 或 `--x=true|false` 两种写法。
- 位置参数个数必须与该子命令定义的完全一致；`node service` 全部子命令都没有位置参数。
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

无位置参数。只在“未完成事务”状态可用，否则返回 1。`--abandon` 只允许出现在 `node service recover`（Task 9 起还有 `node publish --recover`）；在 `install`、`reinstall`、`show`、`uninstall` 上出现一律返回 2（不取锁、不做任何 IO）。成功（回滚、向前清理、丢弃空事务区或墓碑）都返回 0，stdout 无输出；检测到篡改、未知 journal schema、门禁失败或超时返回 5 并输出事务区路径。

### 退出码

| 退出码 | 含义 |
|---|---|
| 0 | 成功（可能伴随告警，写到 stderr）；`recover` 的任何成功结果都是 0 |
| 1 | 用法之外的失败，且发生在第一个受管资产被修改之前：root 或角色不符、快速预检失败（含 dest 探测）、锁超时或等待锁期间收到信号、取锁后复查失败、下载或 `check` 失败、`--export-backup` 失败、`recover --abandon` 条件不满足 |
| 2 | 用法错误：严格解析失败、必填参数缺失、`--singbox-version`/`--singbox-sha256` 未成对出现、`--export-backup` 缺少 `--purge`、`--abandon` 出现在 `recover` 之外等 |
| 4 | 已自动回滚到事务前状态 |
| 5 | 需人工介入：事务已经开始而无法确认已恢复（外部篡改、回滚失败或超时、恢复后复查失败、提交后清理未完成），保留事务区并在 stderr 输出其路径 |

详细的命令行为、粘贴输出格式和已知限制见 [docs/reality-node.md](reality-node.md)。
