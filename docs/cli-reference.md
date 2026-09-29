# CLI 参数参考

本页收录 `proxyctl` 严格模式命令的参数全集（规格 §4.5）。当前只包含 `node service install|reinstall|show`（Task 7）。`node publish` 和带 `--publish` 的 `node add/edit/enable/disable/remove` 由 Task 9 交付后补充到本页；`bootstrap-node.sh` 的参数由 Task 11–12 交付后补充。

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

顶层用法：`proxyctl node service <install|reinstall|show> [args]`；`proxyctl node service --help` 或 `-h` 列出三个子命令，返回 0。

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

### 退出码

| 退出码 | 含义 |
|---|---|
| 0 | 成功（可能伴随告警，写到 stderr） |
| 1 | 用法之外的失败，且发生在第一个受管资产被修改之前：root 或角色不符、快速预检失败（含 dest 探测）、锁超时或等待锁期间收到信号、取锁后复查失败、下载或 `check` 失败 |
| 2 | 用法错误：严格解析失败、必填参数缺失、`--singbox-version`/`--singbox-sha256` 未成对出现等 |
| 4 | 已自动回滚到事务前状态 |
| 5 | 需人工介入：事务已经开始而无法确认已恢复（外部篡改、回滚失败或超时、恢复后复查失败、提交后清理未完成），保留事务区并在 stderr 输出其路径 |

详细的命令行为、粘贴输出格式和已知限制见 [docs/reality-node.md](reality-node.md)。
