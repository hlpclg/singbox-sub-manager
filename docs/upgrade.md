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

订阅中心用现有的 `install-proxy.sh update` / `rollback`（更新会话、配置快照、健康检查）。README“升级与回滚（install-proxy.sh update / rollback）”一节是通用步骤；下面是从 v0.9 升到 v0.10 的补充。

### 从 v0.9 升到 v0.10

1. 在订阅中心执行 v0.9 的 `sudo ./install-proxy.sh update`（以及需要时的 `rollback`），沿用 v0.9 的更新会话、快照和回滚。v0.9 的脚本会拉取**最新** Release 的 `proxyctl` 并按 `checksums.txt` 校验。
2. v0.10 的 `checksums.txt` 除两个 `proxyctl-linux-*` 二进制外，还新增了 `install-proxy.sh`、`merge-nodes.sh`、`bootstrap-node.sh` 三个脚本条目，文件名都不带路径。v0.9 脚本解析 `checksums.txt` 的方式（要求每个二进制恰好一条匹配）读取它时仍然只会匹配到一条二进制记录，所以 v0.9 的 `update` 不受新增条目影响。
3. v0.10 的 `proxyctl` 在 v0.9 脚本持有 L1 锁期间，以子进程方式执行 `health`、`restore`：`restore` 只取 L2 → L3，不申请 L1，不会被持锁的脚本自己挡住。
4. 升级完成后，**先**执行 `proxyctl version` 确认实际升级到的版本。v0.9 的 `update` 拉取的是最新 Release，所以升级时它可能已经高于 v0.10.0。
5. 再从**与 `proxyctl version` 同一个 tag** 的 Release 下载 `install-proxy.sh` 和 `merge-nodes.sh`，核对 `checksums.txt`，替换旧副本：

   ```bash
   TAG="$(proxyctl version)"
   BASE="https://github.com/hlpclg/singbox-sub-manager/releases/download/${TAG}"
   curl -fsSLO "${BASE}/install-proxy.sh"
   curl -fsSLO "${BASE}/merge-nodes.sh"
   curl -fsSLO "${BASE}/checksums.txt"
   sha256sum -c --ignore-missing checksums.txt
   ```

   `sha256sum -c` 必须对两个脚本都报 OK，再用它们替换你实际在用的旧副本。
6. **删除**内置 `PROXYCTL_VERSION=v0.9.0` 的旧副本（含各目录里残留的拷贝）。原因：脚本内置的 `PROXYCTL_VERSION` 与本机二进制版本不同时，下一次 `install` 或 `merge` 会把本机二进制替换成脚本内置的版本；脚本版本更旧时这就是**降级**。同一个 tag 的脚本与二进制不会互相替换。

不要从 `main` 分支下载脚本，也不要把 v0.10 的二进制和 v0.9 的脚本混用。

### 对 `update`、`monitor` 和发布的影响

- **备份取 L2**：v0.10 起 `proxyctl backup` 创建归档时要取 L2（monitor 使用的锁，等待上限 30 秒）。`install-proxy.sh update` 在更新前做的快照走 `backup`；如果 monitor 恰好有一轮运行超过 30 秒，快照会因锁超时失败，**更新随之中止，不做任何替换**，可以直接重试。
- **发布与 monitor**：`proxyctl node publish` 和带 `--publish` 的变更持有 L1 + L2。发布持有 L2 的这段时间（最长约为 HTTPS 验证超时加渲染时间），`proxyctl monitor` 的 TryLock 可能失败并返回 3，对应的 systemd unit 那一轮会显示为 failed，下一轮自动恢复正常，不需要处理。
- **不是全局串行**：只有 `publish`、`--publish` 变更和 `merge-nodes.sh` 与 backup/restore/monitor 串行。`install-proxy.sh` 在持有 L1、未持有 L2 时调用 `proxyctl merge`，用户手动执行的 `proxyctl merge` / `subscription build` 都不加锁，默认路径的兼容模式 CRUD 只取 L1，独立执行的 `restore` 只取 L2 和 L3；这些旧路径的并发行为本版本不变。标准发布入口是 `proxyctl node publish`。
