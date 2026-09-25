# 套件统一更新与回滚 — 设计草稿

- 日期：2026-09-25
- 版本目标：v0.10.0 之后的下一个版本（具体版本号待定）
- 状态：**草稿，未批准**。本文从 v0.10 规格中拆出，保留了拆分前各轮审查的修订结果；下文“未闭环问题”必须处理完才能提交审查。
- 来源：`2026-09-18-v0.10-reality-node-install-design.md` 的 §17.1、§18.1–§18.4、§18.7–§18.10、§14.6，以及 §13.2、§13.3 中与 update/rollback 相关的条目。下文保留原来的节号，便于对照审查记录。
- 前置：v0.10.0 已交付（Reality 节点机、发布事务、角色检测、L1/L2/L3 锁、节点机的 `bootstrap-node.sh --upgrade` 过渡升级入口）。本文中的 §4、§7、§11、§12、§13 均指 v0.10 规格的对应章节。

## 0. 拆分原因与未闭环问题

v0.10 规格经过 S1–S4 和 Sol 的四轮复审，新增问题几乎都集中在套件更新与回滚上，恢复模型也分成了 §7 journal 和 `session.meta` 加 `rollback_state` 两套。用户于 2026-09-25 决定把这部分拆出 v0.10。

重新提交审查前，需要先做一个结构决定：是把 update/rollback 的文件替换改写成一个 §7 事务（受管路径为二进制、sidecar 和规范脚本，更新会话只作历史归档），还是继续使用本文的会话模型。选前者，§18.10 的续做判定可以大幅简化，下面的 1.1、1.4 也会随之消失；2026-09-25 的复审建议选前者。这个决定要在处理其余未闭环问题之前做出，否则这些修订可能白做。

未闭环问题（拆分时的最后一轮审查结论）：

1. Sol 最终复核：
   - 1.1 §18.10 的预校验失败一律返回 1、元数据不变，但 §14.6 要求暂存副本被篡改时返回 5、状态为 `rollback_failed`。需要为这类校验失败明确例外，并保持“不调用 restore”。
   - 1.2 §18.3 把安装器原有的 `monitor --help` 检查换成了 `update --help`，会让缺少 monitor 能力的二进制进入监控安装流程。应保留 monitor 检查，再增加 update 检查。`tests/test_install_proxyctl.sh` 的 `valid-existing` 夹具只支持 `monitor --help`，§14.6 需要允许为这个保留用例调整夹具。
   - 1.3 归档回滚写的是“透传 restore 的退出码”，却只列了 0、4、5。现有 restore 在写入失败、前像回滚成功时可能返回 1，无效归档返回 3。需要列全，或者明确映射关系，并补测试。
   - 1.4 §18.7 仍写“最新 `in_progress` 拒绝默认回滚”，与带 `rollback_state` 时默认续做的规则冲突，测试文字要区分有无回滚记录。旧会话没有规范脚本，“每个规范脚本各跑一次”的崩溃点应注明只适用于 v0.10 会话。
2. 第二份独立审查：
   - 2.1（P1）规范脚本由谁、在什么时候首次放置，没有定义。§18.9 靠“规范脚本存在就说明还有未回滚的套件会话”来推断；如果安装器顺手放置了规范脚本，回滚到 v0.9 会话就会被永久阻止。
   - 2.2（P2）套件的组成写死在旧版本的代码里：v0.10 的 update 按固定三个脚本处理。以后只要增删或改名一个脚本，旧版本就无法升级过去。建议由 Release 自带套件清单，或者按 `checksums.txt` 的全部条目处理，并列为长期契约。
   - 2.3（P3）§18.1 只列了“用法错误 1→2”这一处退出码变化；健康检查失败后回滚 1→4、中断 130→1 也要写进 `docs/upgrade.md`。
   - 2.4（P3）没有 `install.json` 的旧订阅中心会被判为“不确定”，update 和 bootstrap 都拒绝执行，需要给出出口。
3. 拆分带来的新问题（已部分处理）：v0.10 已增加 `bootstrap-node.sh --upgrade` 作为节点机的过渡升级入口（v0.10 §12）：没有更新会话，只在 `/var/lib/proxyctl-bootstrap/previous/` 保留最近一次升级前的副本。本文仍须定义：
   - 节点机上的 `proxyctl update` 如何从 v0.10 起步。v0.10 的节点机没有 `update` 命令，所以第一次升级到本版本只能用本版本的 `bootstrap-node.sh --upgrade`。
   - 引入 `proxyctl update` 之后，`bootstrap-node.sh --upgrade` 是保留、改为转调，还是退役；`/var/lib/proxyctl-bootstrap/previous/` 中的旧副本是否导入为更新会话，还是直接清理。
4. 拆分时 v0.10 的前提变了：订阅中心仍通过 v0.9 实现的 `install-proxy.sh update` 从 v0.9 升到 v0.10，所以 §18.8 的两步迁移要改写成“从 v0.10 迁移”。第一步由 v0.10 的 shell `update` 执行，它同样只拉取最新 Release、不能指定 tag。
5. v0.10 给 `install-proxy.sh` 的 install、update、rollback 增加了两项检查：发现 Reality 资产、事务区或墓碑时拒绝；发现遗留发布事务区或墓碑时拒绝（v0.10 §11.2、§11.5）。Go 版的 `update/rollback` 必须继承这两项，§18.7 要有对应用例。
6. 版本措辞：本文的目标已经不是 v0.10，但正文中的“v0.10 格式的会话”“v0.10 的 update”“v0.10 会话”仍沿用拆分前的写法。重新提交前统一改为“新格式会话”“本版本的 update”；真正指已发布 v0.10 的地方（例如 v0.10 的 shell `update`）保留原写法。
7. 降级防护的起点：v0.10 的 `install-proxy.sh` 和 `merge-nodes.sh` 仍会在版本不同时替换二进制（v0.10 §16）。§18.3 的降级防护只能从本版本的脚本开始生效，用户手里的 v0.10 脚本副本仍会降级；§18.8 的“脚本残留”说明要把 v0.10 脚本也列进去。

### 17.1 同版本套件统一更新与回滚（原 v0.10 §17.1，目标改为下一个版本）

- `proxyctl`、`install-proxy.sh`、`merge-nodes.sh`、`bootstrap-node.sh` 和 `checksums.txt` 作为同一个 Release tag 的版本套件管理，不得分别追踪“各自最新版本”。
- `proxyctl update [--to TAG]` 下载并校验目标 Release 的完整套件，暂存全部产物后再替换规范安装位置；版本检查与健康检查通过后才提交更新。
- `proxyctl rollback` 恢复同一次更新前的完整套件，不能只恢复 `proxyctl` 二进制。
- Shell 脚本作为 Release assets 纳入 `checksums.txt`；更新不得从可变的 `main` 分支获取脚本。
- 规范脚本路径由实施计划确定，并提供稳定命令入口。用户任意目录中的旧脚本副本不属于可管理资产；README 不再推荐长期执行 `./install-proxy.sh` 或 `./merge-nodes.sh`。
- 旧脚本发现自身套件版本与已安装 `proxyctl` 不一致时，不得自动降级二进制；应拒绝继续并提示使用规范入口完成套件更新。
- 套件更新复用现有更新会话、锁顺序、备份与恢复门禁，但作为独立 Task 验收，须覆盖下载/校验失败、部分替换、健康检查失败、取消、回滚失败和旧脚本降级防护。

## 18. §17 与 §1–§16 的衔接契约

本节补充 §17 落地时与前文的接口约定，不改变 §17 已确认的范围。§18.1 和 §18.2 分别对应用户对 N6、N7 的决定。

### 18.1 统一更新入口（N6）

- `proxyctl update [--to TAG]` 和 `proxyctl rollback [--to ID]` 是套件更新与回滚的**唯一实现**，沿用现有的更新会话目录 `$STATE_DIR/updates`。v0.10 格式的会话会额外记录完整套件的前状态；v0.9 格式的旧会话只能按 §18.9 的例外规则回滚。会话的状态、选择与清理规则见 §18.10。
- `install-proxy.sh update/rollback` 改成薄包装。
  - 包装必须在**任何取锁动作之前**执行 `exec proxyctl update|rollback "$@"`，不调用 `installer_preflight` 或 `acquire_installer_lock`。否则继承下来的 fd 9 会占着 L1，`proxyctl` 在另一个文件描述上申请 L1 时会被挡住，最终超时。
  - 本机 `proxyctl` 没有 update 能力时（`proxyctl update --help` 退出码不是 0），包装报错并返回 1，提示按 §18.8 迁移：v0.10 的脚本本身不替换旧的 `proxyctl`。
- 解析：`update` 和 `rollback` 按严格模式解析（§4.5）；`--help`/`-h` 作为唯一参数时返回 0。
- `rollback --to` 必须兼容现有的三种取值：更新会话名、备份归档名、归档路径（现有逻辑见 `install-proxy.sh` 的 `cmd_rollback`）。
- 退出码变化：现有 `install-proxy.sh update/rollback` 遇到用法错误返回 1，改为薄包装后透传 `proxyctl` 的 2。`docs/upgrade.md` 须写明这一变化。
- 锁与备份按角色区分：
  - 订阅中心：整个运行期间持有 L1，内部的 backup/restore 再取 L2。
  - 节点机：按 L1→L3 取锁（L1 用于共享二进制写入与角色判定，§11.5），不做 backup/restore（backup 归档的内容全是订阅中心的配置）。二进制与 sidecar 的回滚依赖更新会话中保存的旧副本。
  - 两种情况都符合 L1→L2→L3 的顺序。取得锁之后都要重新判定角色。
- 退出码沿用 §13.1。
- 必须覆盖 §17.1 列出的失败矩阵：下载/校验失败、部分替换、健康检查失败、取消、回滚失败、旧脚本降级防护。

### 18.2 按角色更新（N7）

`update` 先按 §4.2、§4.3 判定主机角色，再决定更新范围：

| 主机角色 | 更新范围 | 验证 | 失败处理 |
|---|---|---|---|
| 订阅中心“是” | 完整套件：`proxyctl` 加规范路径下的脚本 | 版本检查 + 健康检查（§17.1） | 恢复完整套件 |
| 订阅中心“否” | 只更新 `/usr/local/bin/proxyctl` 及其 sidecar | 版本检查 + `node service --help` 能力探测 | 恢复旧二进制和 sidecar |
| 订阅中心“不确定” | 拒绝，返回 1 | — | — |

- 订阅中心为“否”时（即节点机）：
  - 更新完成后，如果新版本固定的 sing-box 版本与 manifest 的 `singbox_current` 不同，在 stderr 提示执行 `sudo proxyctl node service reinstall`。`update` 不会自动 reinstall。
  - 实例处于“未完成事务”状态时，拒绝更新，返回 1。
- bootstrap 只负责首次安装（§12）。

### 18.3 旧脚本的降级防护

- §17.1 要求“旧脚本发现自己的套件版本与已安装的 `proxyctl` 不一致时拒绝继续”，适用于 `install-proxy.sh` 和 `merge-nodes.sh`。
- 这两个脚本对本机 `proxyctl` 分三种情况处理：
  - **二进制不存在**（全新安装）：按现有流程下载固定版本。
  - **二进制存在，但 sidecar 缺失或与二进制哈希不符**：不执行该二进制，不下载，拒绝执行，返回 1，提示人工核对二进制来源。与 §12 bootstrap 不带 `--replace-proxyctl` 时的行为一致，两个脚本都不提供替换开关。
  - **sidecar 有效，但版本与自身 `PROXYCTL_VERSION` 不同或无法解析**：不下载替换，拒绝执行，返回 1，提示使用 `proxyctl update`。
  - **sidecar 有效、版本相同，但能力探测失败**：两个脚本统一以 `proxyctl update --help` 退出码为 0 作为能力探测（现有 `install-proxy.sh` 探测的是 `monitor --help`，`merge-nodes.sh` 不做探测，改为一致）。探测失败时不下载替换，拒绝执行，返回 1，提示人工核对二进制。
  - 只有 sidecar 有效、版本相同、能力探测通过时，才复用本机二进制。
- **检查时机**：这项检查必须在脚本做任何修改之前完成（`install-proxy.sh` 在取得 L1 之后，`merge-nodes.sh` 在取得 L1、L2 之后），拒绝时不下载，也不写业务资产。业务资产指：`/usr/local/bin/proxyctl` 及其 sidecar；`/etc/singbox-sub-manager/` 下的配置（含 `nodes.conf`、`install.json`）；token；sing-box、Caddy、systemd 的配置；订阅输出目录及其中的文件。以下两类写入不算违反：
  - `flock` 在 `/run/lock/` 下创建的锁文件（重启后锁文件可能不存在，取锁本身就会创建它）；
  - `install-proxy.sh` 的 `installer_preflight` 在取锁之前、由 `prepare_runtime_dirs` 创建的空运行目录（现有行为，本版本不改）。

  现有代码的检查位置太晚：`install-proxy.sh` 到第 1141 行才调用 `install_proxyctl`，此前已经写了配置；`merge-nodes.sh` 在第 87 行就创建了输出目录。实施时必须把检查前移。“二进制不存在”时的下载时机不在本条约束之内，现有的下载失败回退到 shell 渲染器的行为保持不变。
- 这条规则没有“旧版本例外”：v0.9 订阅中心的迁移只走 §18.8 的路径。§12 中“没有 update 能力的旧版本允许替换”那一条，只适用于节点机上的 bootstrap（那种主机上没有订阅中心的状态需要保护）。
- bootstrap 的行为以 §12 的决策表为准。

### 18.4 Release 与校验

- `release.yml` 把 `install-proxy.sh`、`merge-nodes.sh`、`bootstrap-node.sh` 作为 Release asset 发布。
- `checksums.txt` 同时覆盖 `proxyctl-linux-*` 和这三个脚本，格式与现有“恰好一条匹配”的解析方式兼容。
- `update` 只从目标 tag 的 Release 获取套件，并按 `checksums.txt` 校验每一个文件。

### 18.7 §17 的补充测试

- 交互迁移（§17.2）：检测到 legacy `nodes.conf` 时，先显示备份位置；用户拒绝则不做修改，确认后才迁移。非交互命令仍拒绝自动迁移。
- `update` 按角色分支（§18.2）：三种角色各走一次成功与失败路径；节点机上处于“未完成事务”状态时拒绝。
- `install-proxy.sh update/rollback` 的薄包装：退出码透传；本机 `proxyctl` 没有 update 能力时报错。
- 旧脚本降级防护（§18.3）：
  - 版本不同时拒绝，sidecar 缺失或不符时拒绝，本机没有 `proxyctl` 时正常安装；
  - 入口级用例：以完整入口运行 `install-proxy.sh install` 和 `merge-nodes.sh`，在两种拒绝情况下，断言返回 1，没有下载，没有写入 §18.3 定义的业务资产。做法是在临时根目录下对比运行前后的文件树。锁文件路径通过环境变量注入到临时目录；比较时锁文件和 `prepare_runtime_dirs` 创建的空运行目录列入白名单，此外任何新增、删除或内容变化都判为失败。锁文件在运行前不存在的用例至少要有一个，证明白名单是必需的。
- 旧会话的配置快照（§18.9）：`config_snapshot` 为空时跳过配置恢复并告警；非空但文件缺失时拒绝，返回 1，受管路径和会话状态都不变。
- `rollback` 的中断（§18.10）：写入 `rollback_state=started` 之前收到 SIGTERM，返回 1，会话元数据不变；配置 restore 期间、以及恢复二进制之后收到 SIGTERM，都继续完成，返回 0，状态为 `rolled_back`；让继续恢复的过程失败时，返回 5，状态为 `rollback_failed`。
- 回滚记录与续做判定（§18.10）：
  - 执行顺序：假实现记录的调用顺序为“写 `rollback_state=started` → 配置 restore → 写 `config_done` → 恢复套件文件 → 写 `rolled_back`”；配置 restore 失败时，套件文件的字节都不变，`rollback_state` 仍为 `started`；
  - 没有回滚记录的 `success` 会话，本机前后状态混合（例如一个脚本是前状态、其余是后状态）：拒绝，返回 1，没有任何文件被修改；
  - `rollback_state=started`、套件全是后状态（配置 restore 进行到一半时被强制终止）：再次回滚会重新执行配置 restore，然后恢复套件；此时即使套件全部已是前状态，也不会不执行配置 restore 就直接标记为 `rolled_back`；
  - `rollback_state=config_done`、已恢复部分脚本、二进制仍是后状态：再次回滚不调用 restore，只恢复剩余路径；
  - `rollback_state=config_done`、套件全是前状态：只把 `status` 写为 `rolled_back`，不调用 restore；
  - 有回滚记录时，任一路径既不是前状态也不是后状态：拒绝，返回 1，不做修改；
  - 没有回滚记录的 `in_progress` 会话：套件全是前状态时只标记；前后状态混合时从第 2 步开始正常回滚；
  - 续做前先校验：`rollback_state=started` 时，某个脚本变成第三种内容，或者暂存副本的哈希不符，都拒绝，返回 1，并断言 restore **没有被调用**，配置文件的字节不变；
  - 旧会话的续做：有回滚记录时，二进制已是前状态、sidecar 仍是后状态，可以续做，只重写 sidecar 并标记；没有回滚记录时，同样的状态按 S2 被拒绝；二进制哈希不在 {前, 后} 之中时拒绝；
  - 未完成回滚会阻止 update：存在未完成回滚或 `in_progress` 会话时，`update` 返回 1，不创建新会话；连续多次成功更新之后，清理规则也不会删除未完成回滚的会话；
  - `rollback --to <归档>` 的中断：restore 写文件之前收到 SIGTERM，返回 1，不做修改；写文件之后收到 SIGTERM，restore 走完，返回它的结果码。
- 进程级崩溃测试（§14.2 的崩溃注入方法：以子进程方式重新执行自身，在指定点 `os.Exit`）。对 v0.10 会话和旧会话各跑一遍，覆盖以下崩溃点：
  - `rollback_state=started` 落盘之后、restore 开始之前；
  - restore 成功返回之后、`config_done` 落盘之前；
  - `config_done` 落盘之后、第一个套件文件替换之前；
  - 每个套件文件替换之后（二进制、sidecar、每个规范脚本各一个点）；
  - 最后一个套件文件替换之后、`rolled_back` 落盘之前。

  每个崩溃点都断言三件事：崩溃后 `update` 被拒绝；再次执行 `rollback` 能续做完成；最终二进制、sidecar、规范脚本和配置文件的字节都等于前状态，`status=rolled_back`。
- v0.9 订阅中心迁移到 v0.10，端到端测试（§18.8）：先执行 v0.9 的 `install-proxy.sh update`，再执行 `proxyctl update`，补齐规范脚本；第二步失败时回滚到“二进制已是 v0.10、脚本缺失”的状态，这个状态可以重试。
- 薄包装在 exec 之前从未取得 L1：用一个假的 `proxyctl` 替身，在被 exec 时以 `flock -n` 申请 L1 并记录结果，断言申请成功，且替身没有继承任何指向 L1 锁文件的 fd（检查 `/proc/self/fd`）。
- bootstrap 在 exec 之前已释放 L1：用同样的替身方法断言。
- 共享二进制锁：在持有 L1 时，节点机上的 update、bootstrap、`node service install` 都会等待并超时，返回 1；`install-proxy.sh install` 发现 Reality 资产或事务区时拒绝。
- 会话链（§18.10）：连续两次不带 `--to` 的 `rollback`，依次回滚 §18.8 第 2 步的会话和第 1 步的会话；第 2 步成功之后，第 1 步的 v0.9 会话仍然保留；状态为 `rolled_back` 的会话不会再被默认选中；本机当前状态与会话的后状态不符时，拒绝回滚；最新会话处于 `in_progress` 时拒绝默认回滚，用 `--to` 指定它时按“续做判定”表处理（用例见下面的“回滚记录与续做判定”）；旧会话按 `to_version`、sidecar 一致性和后续 v0.10 会话的前状态哈希判定。
- restore 按 L2→L3 取锁：在节点机上持有 L3 时，独立执行的 restore 会等待并超时。
- 旧会话例外（§18.9）：规范脚本存在时，回滚到 v0.9 会话被拒绝；先回滚 v0.10 套件会话后，再回滚到 v0.9 会话，只恢复二进制、sidecar 和配置快照，并输出告警。
- `rollback --to` 的三种取值都能正常使用；update 和 rollback 的严格模式解析，以及 `--help` 返回 0。
- 节点机上的 update 按 L1→L3 取锁，不取 L2，也不执行 backup。
- `checksums.txt` 覆盖全部套件文件；缺少任何一项，`update` 都拒绝。

（说明：本节中的交互迁移、bootstrap 与 `node service install` 的共享二进制锁、restore 按 L2→L3 取锁，这几条用例仍保留在 v0.10 规格中。）

### 18.8 v0.9 订阅中心迁移到 v0.10（M2，用户决定）

唯一的迁移路径分两步：

1. 在 v0.9 订阅中心上，执行 **v0.9 版本的 `install-proxy.sh update`**。本地已经没有这个脚本时，`docs/upgrade.md` 给出 v0.9.0 tag 下原始文件的固定地址，并说明它只用来执行这一次 `update`。它拉取最新的 release，并沿用 v0.9 已有的更新会话、快照和回滚机制，把 `proxyctl` 升级到新版本。v0.10 不为此开放任何替换旧二进制的例外。
2. 执行 `sudo proxyctl update`。它发现目标版本与本机 `proxyctl` 相同，但规范路径下的脚本缺失或版本不符，就补齐这个版本的完整套件：下载、按 `checksums.txt` 校验、放置规范脚本。这一步开一个新的 v0.10 格式会话，并在会话中记录前状态：规范脚本“不存在”、二进制是当前版本。失败时回滚到第 1 步完成后的状态（二进制是新版本、规范脚本缺失），可以重试。

约束与说明：

- **长期契约**：v0.9 的 `update` 只会取最新的 release，不能指定 tag。所以如果迁移发生在更晚的版本发布之后，第 1 步会直接升到那个版本。“目标版本与本机相同、但套件不完整时补齐”必须作为 `proxyctl update` 的长期契约，由后续版本保留，并在其测试中覆盖。
- **脚本残留**：迁移完成后，用户目录中残留的 v0.9 脚本副本，内置的 `PROXYCTL_VERSION` 仍是 v0.9.0，执行它们会降级二进制。这个行为无法从 v0.10 一侧修正。`docs/upgrade.md` 须明确要求迁移后删除这些副本，改用规范入口，并写明原因。
- **不走此路径的后果**：直接运行 v0.10 的 `install-proxy.sh`（按 §18.3 被拒绝），或者运行 v0.10 的 `install-proxy.sh update`（按 §18.1 被拒绝），都会返回 1，提示按本节的两步完成迁移。
- 测试见 §18.7。

### 18.9 旧更新会话的回滚例外（U2）

§17.1 要求“rollback 恢复同一次更新前的完整套件”，这条要求只适用于 v0.10 格式的会话。v0.9 格式的会话（由 v0.9 的 `install-proxy.sh update` 创建，包括 §18.8 第 1 步创建的会话），只保存了 `proxyctl`、sidecar 和配置快照；v0.9 的 Release 也没有脚本资产。所以这类会话无法恢复旧的脚本套件，列为明确的例外：

- **识别**：会话元数据中没有 v0.10 的套件清单字段，就判定为旧会话。
- **前置条件**：只有在规范路径下的脚本全部不存在时，才允许回滚到旧会话。只要规范脚本存在，就说明之后还有更新的套件会话尚未回滚，这时拒绝执行，返回 1，并提示先回滚最近的套件会话。回滚按会话的逆序进行：先回滚 §18.8 第 2 步创建的 v0.10 会话，这会删除规范脚本；然后才能回滚第 1 步的旧会话。
- **恢复范围**：只恢复 `proxyctl`、sidecar 和配置快照，恢复方式与 v0.9 现有的 rollback 相同。配置快照分两种情况：
  - 会话元数据的 `config_snapshot` 为空（v0.6 及更早的二进制做过的“仅二进制”升级）：跳过配置恢复，只恢复二进制和 sidecar，并在 stderr 告警“配置未回滚”。
  - `config_snapshot` 非空，但快照文件不存在或不可读：视为会话损坏或被篡改。拒绝回滚，返回 1，不修改任何受管路径，会话状态保持不变，并输出会话路径供人工排查。这是对 v0.9 `cmd_rollback` 的有意收紧：v0.9 在这种情况下会静默跳过配置恢复。完成后，在 stderr 告警：本机已回到 v0.9 的二进制，没有受管理的脚本套件，之后需要用 v0.9 版本的脚本管理这台主机，或者重新执行 §18.8 的迁移。
- **不做的事**：不尝试重建 v0.9 的脚本，因为那些脚本没有经过 `checksums.txt` 校验的来源。
- **文档**：`docs/upgrade.md` 须写明这条例外及回滚顺序。

### 18.10 更新会话的状态、选择与清理（R2）

`proxyctl update/rollback` 沿用 v0.9 已有的会话状态值：`in_progress`、`success`、`failed`、`rolled_back`、`rollback_failed`，并补充下列规则。v0.9 的 `cmd_rollback` 回滚后不会修改会话状态，这一行为由 `proxyctl` 替代，旧脚本不再执行回滚（§18.1）。

**回滚后的状态**：任何一次回滚成功后（包括 §18.9 的旧会话），都要把该会话的状态原子更新为 `rolled_back`。回滚失败则标记为 `rollback_failed`。

**默认回滚目标**：不带 `--to` 时，按目录名排序，选择最新的、状态为 `success` 的会话。
- 跳过 `failed` 和 `rolled_back` 状态的会话。
- 最新的会话处于 `rollback_failed` 或 `in_progress`（更新被中断，S1）时，拒绝默认回滚，返回 5，提示人工处理或用 `--to` 明确指定会话。

**链完整性**：回滚之前，本机当前状态必须等于目标会话记录的后状态，否则拒绝执行，返回 1。比较的内容是：
- v0.10 会话：二进制哈希、sidecar，以及规范脚本的哈希或“不存在”；
- 旧会话（S2）：v0.9 的 `session.meta` 没有记录升级后二进制的哈希，所以改用以下三条同时成立来判定：
  - 本机 `proxyctl version` 等于会话的 `to_version`；
  - 当前 sidecar 与二进制一致；
  - 如果存在紧随其后、且已回滚的 v0.10 会话，本机二进制哈希还要等于该会话记录的“前状态二进制哈希”。

  此外还要满足 §18.9 的“规范脚本全部不存在”。

**`in_progress` 会话**（S1）：只能用 `--to` 指定。v0.10 格式的会话按下文“续做判定”处理；旧会话的链完整性放宽为“本机当前状态等于它的前状态或后状态”：
- 等于后状态，或者二进制已被替换：按正常方式回滚；
- 等于前状态，即替换尚未发生：只把会话标记为 `rolled_back`，不修改任何文件。

旧会话的前状态取自 `previous_binary_sha256` 和 `previous_sidecar_sha256`，后状态按上面 S2 的规则判定。

这条检查保证回滚只能按逆序逐级进行，不会跳过中间的会话。

**清理规则**：更新成功后执行清理：
- 保留最新的 3 个 `success` 会话。默认回滚链由未回滚的 success 会话按时间排列组成，所以这 3 个就是链上最近的三级，§18.8 的两步迁移链一定完整保留；
- 删除这 3 个之外、状态为 `success` 的会话；
- 删除比保留下来的最早 `success` 会话更旧的 `failed` 和 `rolled_back` 会话；
- 不存在任何 `success` 会话时，只保留最新的 3 个 `failed` 或 `rolled_back` 会话，更旧的删除；
- 不删除 `rollback_failed` 和 `in_progress` 会话，它们留作人工排查和回滚的依据；
- 不删除未完成回滚的会话（见下文“未完成回滚”），直到它的 `status` 变为 `rolled_back`。

迁移之后如果仍运行残留的 v0.9 `install-proxy.sh update`，它的 `prune_successful_sessions` 只保留最新的一个成功会话，会破坏上面的保留规则。这是 §18.8 要求删除残留 v0.9 脚本的另一个原因，`docs/upgrade.md` 须写明。

`--to` 的三种取值（§18.1）不受以上选择规则影响，但同样要通过链完整性检查。

**会话与 §7 事务区的关系**：更新会话目录不是 §7 的事务区，也不是受管路径。`update` 和 `rollback` 的受管路径相同：`/usr/local/bin/proxyctl`、它的 sidecar、（订阅中心上的）规范脚本，以及配置快照 restore 会写入的路径。§13.3 中“删除事务区”的规定对这两个命令不适用，改按下面的中断规则处理。

**`update` 的中断**（SIGINT/SIGTERM）：

- 会话元数据还没有落盘：删除整个会话目录，返回 1。
- 元数据已落盘、但还没有替换任何受管路径：保留会话目录和其中的暂存副本，把状态原子更新为 `failed`，返回 1。这里不保留 `in_progress`，因为按上文，`in_progress` 会让默认回滚返回 5，而此时本机并没有被修改。
- 已替换过受管路径：改用独立 context（最长 120 秒，§13.2），按下文“回滚记录与执行顺序”把全部受管路径恢复到会话记录的前状态。成功后状态为 `rolled_back`，返回 4；失败或超时时状态为 `rollback_failed`，返回 5。
- 只有进程被强制终止（SIGKILL、断电）时，会话才会停留在 `in_progress`，按下文的“续做判定”处理。
- 所有情况下都要删除下载临时文件。

**回滚记录与执行顺序**：所有会话回滚都遵守同一顺序。这包括 `rollback` 命令，也包括 `update` 在健康检查失败或中断后执行的自动回滚。会话元数据增加下列字段，写法与 `status` 相同（同目录临时文件、fsync、rename、fsync 目录）：

- `rollback_state`：取值 `started` 或 `config_done`；
- `rollback_post_binary_sha256`、`rollback_post_sidecar_sha256`：开始回滚时本机二进制和 sidecar 的哈希。v0.10 会话本来就记录了每条套件路径的后状态，这两个字段主要供旧会话使用，但所有会话都写入。

执行顺序：

1. **全部校验，不做修改**：
   - 链完整性，按下文“续做判定”；
   - 需要恢复的每条套件路径，其暂存副本的哈希都与会话记录一致；
   - `config_snapshot` 非空时，快照文件存在且可读（§18.9）。

   任何一项不通过，就返回 1，不调用 restore，不修改元数据。
2. 在修改任何受管路径之前，用**一次**原子写入同时记录 `rollback_state=started` 和两个 `rollback_post_*` 哈希。续做时这两个哈希不再改写。
3. 执行配置快照的 restore；`config_snapshot` 为空时跳过。restore 成功返回之后，才把 `rollback_state` 写为 `config_done`。
4. 状态为 `config_done` 之后，才开始恢复二进制、sidecar 和规范脚本。
5. 全部恢复完成后，把 `status` 写为 `rolled_back`。

所以只要 `rollback_state` 不是 `config_done`，配置就可能还没恢复完；在这之前，套件文件一律没有动过。旧会话由 v0.10 的 `proxyctl` 回滚时，同样写入这些字段；v0.9 格式的 `key=value` 元数据可以直接追加字段。

**未完成回滚**：`rollback_state` 已存在、而 `status` 不是 `rolled_back` 的会话，称为“未完成回滚”。这时配置可能只恢复了一部分，而套件可能仍然完整，所以：

- `update` 在取锁之后、创建新会话之前检查。只要存在未完成回滚，或者存在 `in_progress` 会话，就拒绝执行，返回 1，并提示先用 `rollback --to <会话>` 处理该会话。v0.9 创建的、没有回滚记录的 `rollback_failed` 会话不在此列，保持 v0.9 的行为。
- 默认回滚：最新的会话是未完成回滚、且 `status` 为 `success` 或 `in_progress` 时，默认选中它续做，不再按“最新的 success”去选。带回滚记录的 `in_progress` 会话不适用上文“拒绝默认回滚”的规定，因为它的回滚已经开始，只差完成。`status` 为 `rollback_failed` 时，仍按上文返回 5，需要用 `--to` 明确指定。
- 清理规则不删除未完成回滚的会话（见清理规则最后一条）。

**`rollback` 的中断**（SIGINT/SIGTERM）：rollback 的目标本身就是恢复到前状态，中途撤销等于再做一次 update，所以已开始恢复时一律**继续完成**，不撤销。

- 在第 2 步之前：立即返回 1，会话元数据不变。
- 第 2 步之后：忽略主 context 的取消，改用独立 context（最长 120 秒，§13.2）把剩余步骤做完：
  - 全部完成：`status` 为 `rolled_back`，返回 0，并在 stderr 说明“中断请求在恢复开始之后收到，已继续完成回滚”；
  - 失败或超时：`status` 为 `rollback_failed`，返回 5，并输出会话路径和尚未恢复的路径。
- 进程被强制终止时，`status` 保持原值，但 `rollback_state` 已存在，会话成为“未完成回滚”。再次回滚这个会话时，按下文的“续做判定”处理。

**`rollback --to <归档>` 的中断**：这条入口不涉及会话，但同样由 `proxyctl rollback` 自己安装 SIGINT/SIGTERM 处理，不依赖 `cmdRestore` 现有的实现（现有实现使用 `context.Background()`，没有信号处理）：

- restore 取得 L2、开始写文件之前收到中断：返回 1，不做修改。
- restore 开始写文件之后收到中断：忽略主 context 的取消，改用独立 context（最长 120 秒）让 restore 走完。restore 自身失败时，按它现有的前像回滚处理。退出码透传 restore 的结果：成功为 0，restore 已自行回滚为 4，回滚失败为 5。
- 进程被强制终止：只有 restore 现有的前像保留在 `restore-snapshots` 中，本版本不为归档回滚增加续做机制。§16 与 `docs/upgrade.md` 须写明这一限制。
- 独立执行的 `proxyctl restore` 命令的信号行为不在本版本范围内，保持不变。

**续做判定**（v0.10 格式的会话）：只有下表列出的情况可以放宽链完整性检查。“逐路径”指对二进制、sidecar 和每个规范脚本逐一检查：每条路径都必须等于会话记录的前状态或后状态（哈希，或者“不存在”），任何一条两者都不是，就拒绝回滚，返回 1。这项判定属于第 1 步，所以拒绝时不调用 restore，也不修改任何东西。

| `status` | `rollback_state` | 判定与处理 |
|---|---|---|
| `success` | 无 | 不放宽：本机必须整体等于后状态，否则返回 1。前后状态混合的情况可能是外部改动，一律拒绝 |
| `success`、`in_progress` 或 `rollback_failed` | `started` | 先逐路径判定，并校验暂存副本；全部通过后，从第 3 步重新执行配置 restore（可重复执行），再只恢复仍处于后状态的路径 |
| `success`、`in_progress` 或 `rollback_failed` | `config_done` | 逐路径判定。全部路径已是前状态：只把 `status` 写为 `rolled_back`；否则只恢复仍处于后状态的路径，不再调用 restore |
| `in_progress` | 无 | update 被强制终止，配置没有被改动（update 只备份配置，不修改配置）。逐路径判定。全部路径已是前状态：替换尚未发生，只把 `status` 写为 `rolled_back`；否则从第 2 步开始正常回滚 |

这张表代替上文 S1 中“二进制是否已被替换”的判断依据：套件更新可能先替换了部分脚本、还没替换二进制，只看二进制判定不了。

**旧会话的续做**：

- 没有回滚记录的旧会话：严格按 S2 判定。
- 有回滚记录的旧会话：不再走 S2（二进制已恢复、sidecar 尚未恢复时，S2 必然不成立），改为逐路径判定：
  - 二进制哈希 ∈ {`previous_binary_sha256`, `rollback_post_binary_sha256`}；
  - sidecar 哈希 ∈ {`previous_sidecar_sha256`, `rollback_post_sidecar_sha256`}；
  - 暂存副本的哈希与会话记录一致；
  - §18.9 的“规范脚本全部不存在”仍然适用。

  任一项不满足，就拒绝，返回 1，不调用 restore。通过之后，按 `rollback_state` 从第 3 步或第 4 步续做，只恢复仍处于后状态的路径。


### 14.6 既有 shell 测试门禁的变更（待用户批准）

§18.1 把 `install-proxy.sh update/rollback` 改成薄包装，§18.3 把“版本不同就重新下载”改成拒绝，§18.10 改了会话保留规则。`tests/test_install_update.sh` 和 `tests/test_install_proxyctl.sh` 中的部分用例因此失效。按仓库 `AGENTS.md`，修改门禁须经用户明确批准。本节是这次变更的完整清单，实施时不得超出本清单改动既有测试。

处理方式分四种：保留（不改）；迁移（行为不变，改由 Go 的 `proxyctl update/rollback` 测试覆盖，原 shell 用例随被测函数一起删除）；改写（行为按本规格变化，断言改为新行为）；退役（场景在 v0.10 中不再存在）。

`tests/test_install_update.sh`（19 个用例）：

| 用例 | 处理 | 替代断言 |
|---|---|---|
| `update_checksum_mismatch` | 迁移 | 校验和不符返回 1；二进制、sidecar、规范脚本不变；下载临时文件已删除 |
| `update_download_timeout` | 迁移 | 下载超时返回 1，已安装文件不变 |
| `update_sidecar_replaced_atomically` | 迁移 | 成功后二进制为 0755、sidecar 为 0644，内容与校验和一致，没有残留的替换临时文件 |
| `update_success_retains_latest_rollback_session` | 迁移 | 成功后会话保留旧二进制和旧 sidecar，状态为 `success` |
| `update_next_success_prunes_previous_session` | 改写 | 按 §18.10：连续 4 次成功更新后只剩最新 3 个 `success` 会话 |
| `update_session_permissions_and_atomic_metadata` | 迁移 | 会话目录 0700、文件 0600；首次替换之前元数据已经落盘；替换失败时已安装二进制不变 |
| `update_healthcheck_failure_rolls_back_binary_sidecar_and_config` | 迁移 | 顺序不变量：健康检查失败 → 恢复配置快照 → 恢复二进制和 sidecar（套件会话还要恢复规范脚本）；会话状态为 `rolled_back`；退出码 4 |
| `update_staged_binary_checksum_mismatch_blocks_rollback` | 迁移 | 暂存副本被篡改时不写回；会话状态为 `rollback_failed`；返回 5 |
| `update_old_binary_without_backup_restore_degrades_safely` | 退役 | v0.10 的 `update` 由已安装的 `proxyctl` 自身执行，一定有 backup/restore 能力。旧会话中没有配置快照的情况，改由新用例按 §18.9 覆盖：回滚这种旧会话时跳过配置恢复，只恢复二进制和 sidecar，stderr 告警“配置未回滚”，并且不调用 restore |
| `update_interrupt_preserves_staged_binary` | 改写 | 按 §18.10 的中断规则：下载期间收到 SIGTERM，会话目录、元数据和旧二进制副本都保留，状态为 `failed`（不再是 `in_progress`）；已安装文件不变；临时文件已删除；退出码由 130 改为 1。另加两个用例：元数据落盘之前中断时，会话目录被删除，返回 1；替换之后中断时，回滚成功返回 4，状态为 `rolled_back` |
| `rollback_no_snapshot_errors` | 迁移 | 没有会话时返回 1 并说明原因；`--to` 指向不存在的目标时返回 1 |
| `rollback_config_only_reports_binary_unchanged` | 迁移 | `--to <归档>` 只做 restore，二进制不变，并在输出中说明 |
| `rollback_restores_session_binary_and_config` | 迁移 | 默认回滚恢复二进制、sidecar 和配置；会话状态改为 `rolled_back`（§18.10 新增断言） |
| `update_dispatches_before_domain_validation` | 保留 | — |
| `rollback_dispatches_to_cmd_rollback` | 改写 | 前半段保留。后半段 `rollback --to` 缺值：包装原样 exec 替身 `proxyctl` 并透传其退出码 2，不进入安装流程 |
| `update_success_keeps_failed_sessions` | 改写 | 按 §18.10：`in_progress` 会话保留；比保留下来的最早 `success` 会话更旧的 `failed` 会话被删除 |
| `rollback_ignores_incomplete_sessions` | 迁移 | 没有元数据的会话目录不会成为默认回滚目标 |
| `update_production_paths_carry_timeouts` | 迁移 | Go 实现中套件下载和健康检查都有超时（§13.2），由注入时钟的测试断言 |
| `help_works_without_root` | 保留 | — |

`tests/test_install_proxyctl.sh`（12 个场景）：

| 场景 | 处理 | 替代断言 |
|---|---|---|
| `valid-download`、`valid-existing`、`path-binary-ignored`、`wrong-download-version`、`checksum-mismatch`、`http-404`、`download-failure-ignores-path`、`nodes_conf_is_sectioned-detection`、`run_proxyctl_merge-sectioned-rejection` | 保留 | — |
| `bad-existing-version-redownloads` | 改写 | 按 §18.3：已安装版本不同时拒绝，返回 1，二进制和 sidecar 字节不变，并提示使用 `proxyctl update` |
| `bad-sidecar-redownloads` | 改写 | 按 §18.3：sidecar 缺失或不符时不执行该二进制（替身二进制的执行标记不存在），拒绝并返回 1，二进制和 sidecar 字节不变 |
| `merge-nodes-path-isolation` | 保留断言，调整夹具 | `merge-nodes.sh` 新增 L1→L2 加锁后，锁文件路径须能通过环境变量注入，测试改用临时目录中的锁文件；断言不变 |

两个拒绝用例的证明要求：

- 现有夹具 `run_install_case` 用 `install_proxyctl || true` 吞掉了退出状态，无法证明新断言。夹具须改为记录真实退出码，交给 verify 函数断言。这只改夹具，不放宽既有断言：保留的 9 个场景仍按原来的方式判定。
- 两个拒绝用例都要断言：退出码为 1；`curl` 替身没有被调用（没有下载）；`PROXYCTL_BIN`、sidecar 的字节不变；用例目录中没有新增文件或目录。
- 函数级用例证明不了“在任何修改之前拒绝”，所以 §18.7 另加入口级用例（见 §18.7 旧脚本降级防护）。

`tests/test_install_json.sh`、`tests/test_install_monitor.sh` 预计不受影响。实施中如需改动，同样须先报批。


## 附：从 v0.10 规格移出的超时与取消条目

- §13.2 超时：`update` 套件下载，每个文件 120 秒（沿用 v0.9 的 `DOWNLOAD_TIMEOUT`）；`update` 健康检查 120 秒（沿用 v0.9 的 `HEALTH_TIMEOUT`）；`update`/`rollback` 取消后的独立恢复 120 秒（含配置 restore 的服务重启 30 秒与复检 30 秒）。
- §13.1：退出码表同样适用于 `proxyctl update/rollback`。
- §13.3：`proxyctl update/rollback` 没有 §7 事务区，中断时对更新会话的处理以 §18.10 为准。退出码与 §13.3 其他条目一致，只有一处例外：`rollback` 在开始恢复之后收到中断、并继续完成时返回 0。
- §16：`rollback --to <归档>` 在 restore 写文件期间被强制终止时，没有续做机制，只能依赖 restore 现有的前像人工恢复。
