# 升级

v0.10 起有两类主机，升级入口不同。**先确认这台主机是哪一类**：订阅中心有 `/etc/singbox-sub-manager/install.json`，独立 Reality 节点机没有（订阅中心角色的完整判定见 [reality-node.md](reality-node.md)）。

| 主机 | 升级入口 | 说明 |
|---|---|---|
| 订阅中心 | `sudo ./install-proxy.sh update` | 带更新会话、配置快照、健康检查和 `rollback`；见 README“升级与回滚”一节。脚本必须与要升级到的二进制来自同一个 tag |
| 独立 Reality 节点机 | `sudo bash bootstrap-node.sh --upgrade` | 只升级 `proxyctl`（及其 `.sha256`），不 exec `proxyctl`、不改实例资产；没有更新会话和回滚命令 |

两者不能互换：

- **不要**在节点机上运行 `install-proxy.sh update`：它的健康检查只适用于订阅中心，v0.10 的脚本发现本机有 Reality 实例资产、事务区或墓碑会拒绝执行（返回 1）。
- **不要**在订阅中心上运行 `bootstrap-node.sh`（含 `--upgrade`）：它在角色不是“否”时返回 1，不下载、不修改。

## 节点机升级（`bootstrap-node.sh --upgrade`）

从**目标版本的 Release** 下载 `bootstrap-node.sh`（脚本内置的版本就是目标版本，不接受环境变量或参数覆盖），再执行：

```bash
curl -fsSLO https://github.com/hlpclg/singbox-sub-manager/releases/download/v0.10.0/bootstrap-node.sh
```

```bash
sudo bash bootstrap-node.sh --upgrade
```

要点（完整的顺序、锁、兼容检查、旧副本、信号和退出码见 [reality-node.md](reality-node.md) 的“升级”一节）：

- `--upgrade` 必须是唯一参数，否则返回 2。
- 本机 `proxyctl` 版本等于目标版本时返回 0（已是该版本，不下载、不修改）；高于目标版本返回 1（不降级）；不存在、`.sha256` 缺失或不符、版本不可解析时返回 1，不执行该二进制。
- 存在未完成的实例事务（`/var/lib/proxyctl-reality-txn/` 或 `/var/lib/.proxyctl-reality-txn.tomb-*`）时返回 1，先执行 `sudo proxyctl node service recover`。
- 替换之前，新版本先对本机 Reality 实例做只读兼容检查（`node service show`）；未知 schema、状态不一致或超过 15 秒都会拒绝升级，已安装的文件和实例资产不变。
- 升级前的 `proxyctl` 与 `.sha256` 保存在 `/var/lib/proxyctl-bootstrap/previous/`（只保留最近一次）。替换或验证失败自动恢复旧版本：返回 4；恢复也失败返回 5，需人工介入。
- 成功后新版本固定的 sing-box 版本可能与实例当前使用的不同：执行 `sudo proxyctl node service show` 查看，必要时 `sudo proxyctl node service reinstall`。

退出码：`0` 成功或已是该版本，`1` 替换之前失败，`2` 用法错误，`4` 已恢复旧版本，`5` 需人工介入。

### 回退与修复

- **人工回滚**：本版本没有回滚命令。只有升级后只做过只读 `node service show`、没有执行过任何 `node service` 变更命令（包括失败或被取消的变更、`recover`）、且实例没有未完成事务时，才可以按 [reality-node.md](reality-node.md) 的步骤从旧副本换回旧版本；做过任何变更之后不要直接换回旧二进制，无法确认时不要执行。
- **安装文件修复**：升级被 SIGKILL 或断电打断，可能留下新二进制配旧 `.sha256`。普通运行会拒绝执行它；用**同一个目标 Release** 的 `bootstrap-node.sh` 加 `--replace-proxyctl` 修复，不要用更旧的脚本。这与上面的降级是两回事。

## 订阅中心升级

按 README“升级与回滚（install-proxy.sh update / rollback）”一节操作。订阅中心从 v0.9 升到 v0.10 时，`install-proxy.sh`、`merge-nodes.sh` 必须与二进制来自同一个 tag。
