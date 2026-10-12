# Reality 节点（施工中，v0.10）

本文档记录 v0.10 独立 Reality 节点功能的实现进度。`proxyctl node service install|reinstall|show`（Task 7）与 `uninstall`、`uninstall --purge [--export-backup PATH]`、`recover [--abandon]`（Task 8）已经可用；交互菜单（`sudo proxyctl node`，Task 10）也已可用；bootstrap 首装脚本尚未交付（Task 11–12）。参数全集见 [docs/cli-reference.md](cli-reference.md)。

完整设计见 [v0.10 定稿规格](superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md)；已完成 Task 的真实验证证据见 [reality-node-validation.md](reality-node-validation.md)。

## 本机代理服务命令（`proxyctl node service`，Task 7–8）

`install`、`reinstall`、`show` 三个命令共同的顺序（规格 §8）：严格解析全部参数（含取值校验）→ 检查 root → 快速预检 → 取锁（`install`/`reinstall` 取 L1 再 L3，`show` 只取 L3，最长等待 10 秒）→ 取锁后复查 → 执行。命令必须以 root 运行；订阅中心角色必须是“否”，否则拒绝（`install`/`reinstall` 需要，快速预检与取锁后各查一次）。

- 快速预检发生在取锁**之前**：`install` 检查 OS/架构/systemd、实例状态（必须未安装）、磁盘空间、`listen_port` 可绑定、选定 `selfcheck_port`、ufw 告警、§9.1 dest 探测；`reinstall` 检查参数配对、OS/架构/systemd、实例状态是否是两个允许来源之一。预检不修改任何东西，失败返回 1，且不取锁。
- 取锁后只按规格 §8 的复查表复查：`install` 复查实例状态、目标路径冲突、`listen_port` 与 `selfcheck_port` 仍可绑定（不重复 dest 探测）；`reinstall` 复查实例状态（来源可能变化）；来源为“已卸载但保留身份”时再复查 `listen_port`，任何来源下 `selfcheck_port` 不可绑定都按下文规则重选。复查失败同样返回 1。
- 参数取值错误（例如 `--port 70000`、IPv6 `--server`、IP 形式的 `--sni`、非法或以 `-` 开头的 `--name`）在解析阶段就返回 2，早于 root 检查、加锁和任何网络请求。

### `sudo proxyctl node service install --name NAME --server ADDR [--port PORT] [--sni SNI]`

- `--name`、`--server` 必填；`--port` 默认 443；`--sni` 默认 `www.microsoft.com`。全部参数遵守严格模式（§4.5）：未知/重复 flag、缺值、位置参数都返回 2；`--name` 不能以 `-` 开头。
- 预检包括 OS/架构/systemd 可用性、磁盘空间、`listen_port` 与 `selfcheck_port` 可绑定、ufw 是否 active（仅告警）、dest 探测（TLS 1.3 握手 `<sni>:443`）。
- 成功后依次：下载并校验 sing-box、生成身份（UUID/X25519 私钥/short_id）、生成服务端配置、`sing-box check`、写入全部资产、`daemon-reload`/`enable`/`start`、本机门禁（§9.2）、本地 Reality 数据通路与外网探测（§9.3–9.4，临时客户端使用已安装的 `/usr/local/lib/proxyctl-reality/versions/<版本>/sing-box`）、写 manifest、提交。
- 成功时 stdout 输出一条可粘贴到订阅中心的 `sudo proxyctl node add --publish ...` 命令（见下）；stderr 提示放行云安全组端口，以及外网探测失败等告警。
- 任一步在写入任何资产之前失败：不做任何修改，返回 1。写入开始之后失败（包括自检未通过）：自动回滚到事务前状态（即完全未安装），返回 4。检测到外部篡改、回滚失败或恢复后复查失败：返回 5，保留事务区供人工处理（`docs/troubleshooting.md`）。

### `sudo proxyctl node service reinstall [--singbox-version V --singbox-sha256 H]`

- 不接受 `--name`/`--server`/`--port`/`--sni`（严格模式下这些是未知 flag，返回 2）。`--singbox-version` 与 `--singbox-sha256` 必须成对出现或都不出现，否则返回 2。
- 适用于“已安装”或“已卸载但保留身份”两种状态；复用已有的 UUID/私钥/short_id。不带版本参数时，目标版本是本次 `proxyctl` 编译期固定的 sing-box 版本：已在 `versions/<版本>/` 中且哈希与 manifest 记录一致时直接复用，否则重新下载校验。
- 每次 reinstall 都会重新生成本机地址拒绝列表；`selfcheck_port` 不可绑定时按 install 同样的规则重新选取。
- 私有二进制最多保留两个版本（`singbox_current` 与 `singbox_previous`），**两个版本的目录和二进制都登记在 manifest 中**（含 sha256）。提交后才清理落出窗口的旧版本，并且只清理 manifest 登记过的目录；清理步骤的前状态取自 manifest 登记的 mode/owner/sha256，而不是现场状态，所以被改动过的旧版本二进制会被拒绝删除，而不是被删掉：事务开始前发现不符，实例被判为“不一致”，reinstall 直接返回 1；事务进行中发现不符，回滚并返回 4；提交之后才发现不符，向前清理拒绝执行，返回 5 并保留事务区。
- 目标版本的处理：目标版本已登记（等于 `singbox_current` 或 `singbox_previous`）时不重复写入。不带版本参数直接复用；显式给出版本与哈希时仍会下载并校验，要求解包得到的二进制与已登记的字节一致，一致才复用，不一致返回 1，不做任何修改。目标是 `singbox_previous` 时，当前版本变成新的 previous。目标版本的目录存在于磁盘、但没有登记在 manifest 中时，修改之前就返回 1。复用已有二进制时，同样会在修改任何受管路径之前，用该二进制对新配置执行 `sing-box check`，失败返回 1。
- 来源为“已卸载但保留身份”时，`singbox_previous` 记为空（卸载已经删除了全部私有二进制），全部目录链和二进制都重新创建并重新下载。
- 失败时按与 install 相同的规则回滚（含恢复旧的 config/unit/enabled 状态）。
- 成功时 stdout 输出：来源为“已安装”时是可执行的 `add` 命令（对同名同字段的已启用节点重复执行是安全的空操作）；来源为“已卸载但保留身份”时是可执行的 `enable` 命令，附一条整行注释的备选 `add` 命令。

### `sudo proxyctl node service show`

- 只读，不写任何文件，不做隐式迁移；只取 L3 锁。公钥每次都从私钥实时推导。
- 已安装：stdout 输出可执行的 `add` 命令，附一条整行注释的备选 `enable` 命令。
- 已卸载但保留身份：先输出一行 `#` 注释说明服务未运行，再输出可执行的 `disable` 命令，附一条整行注释的备选 `remove` 命令。
- manifest 记录的 sing-box 版本与本机 `proxyctl` 编译期固定值不同（例如执行过 `bootstrap-node.sh --upgrade` 之后）：stderr 提示执行 `reinstall`，stdout 不受影响。
- 实例处于“未安装”“未完成事务”“不一致”时返回 1，不输出任何命令。

### `sudo proxyctl node service uninstall`（Task 8，规格 §8.4）

- 只适用于“已安装”状态；“已卸载但保留身份”再执行 `uninstall` 返回 1（提示用 `reinstall` 恢复，或 `uninstall --purge` 彻底清除）。
- 顺序同其他变更命令：严格解析 → root → 快速预检（状态、实例目录里只有 manifest 登记过的内容、磁盘空间）→ 取 L1、L3 → 只复查实例状态 → 事务。**不检测订阅中心角色**：规格 §8 的复查表对 uninstall/purge/recover 只列实例状态，并且清理一个卡住的实例不应依赖 token 文件可读。
- 事务步骤：快照 unit、config 和全部已登记版本的私有二进制 → `stop`、`disable` → 删除 unit → `daemon-reload` → 删除 config 和配置目录 → 删除私有二进制的文件和目录（含 `versions/`、`/usr/local/lib/proxyctl-reality/`）→ manifest 改写为 `uninstalled`，这一步完成后即提交。`secrets.json`、`instance.json`、`manifest.json` 保留，字节、mode、属主都不变；已卸载状态下不会生成新的身份，`install` 会被拒绝，`reinstall` 复用原身份。
- 任一步失败（包括取消、崩溃）都从快照完整恢复：文件、目录（在 umask 077 下以记录的 0755/0700 重建，新 inode）以及 systemd 状态（重新 enable，原本 active 则 restart 并重跑门禁）。
- stdout：一条可执行的 `disable` 命令，附一条整行注释的备选 `remove` 命令。stderr 提示 `/usr/local/bin/proxyctl` 及其 `.sha256`、`/var/lib/proxyctl-bootstrap/` 中 bootstrap 的旧副本不属于实例，已保留。

### `sudo proxyctl node service uninstall --purge [--export-backup PATH]`（Task 8，规格 §8.5）

- 可在“已安装”或“已卸载但保留身份”状态下执行。事务先把**全部**受管资产（unit、config、私有二进制、`secrets.json`、`instance.json`、`manifest.json`）快照进事务区；unit 存在时 `stop`、`disable`、删除、`daemon-reload`；再删除 config、私有二进制、身份、manifest 和 `/var/lib/proxyctl-reality/` 本身；全部删除完成后提交。任何一步失败都从快照完整恢复（含 systemd 状态），返回 4。
- 成功后 `/var/lib/proxyctl-reality/`、`/etc/proxyctl-reality/`、unit、私有二进制目录、事务区和墓碑都不存在；本机不再有任何含私钥的文件（**没有隐式备份**）。L3 锁文件 `/run/lock/proxyctl-reality.lock` 不属于实例资产，可能保留。`/usr/local/bin/proxyctl` 和 `/var/lib/proxyctl-bootstrap/` 的旧副本保留（stderr 提示）。
- 实例目录里出现 manifest 没有登记的文件或目录（会让最后的 rmdir 在修改途中失败）时，预检直接返回 1，不修改任何东西；处理掉多余内容后再执行。
- stdout：一条可执行的 `remove` 命令，附一条整行注释的备选 `disable` 命令。
- **`--export-backup PATH`** 是唯一会产生永久备份的途径，只能与 `--purge` 一起使用（否则返回 2），发生在取得 L3 并复查之后、事务开始之前：
  - PATH 已存在（包括符号链接、目录）则返回 1；相对路径按当前工作目录解析；PATH 不能位于会被 purge 删除的目录里（状态目录、配置目录、私有二进制目录、事务区）。
  - 在 PATH 所在目录用 `O_EXCL` 创建临时文件（mode 0600，名字形如 `.proxyctl-export-<随机>.tmp`），写入 tar.gz，内容仅为 `/var/lib/proxyctl-reality/` 和 `/etc/proxyctl-reality/`（存在的那些），成员名是去掉开头 `/` 的绝对路径；fsync 后用 `link` 放到 PATH（`link` 不会覆盖已有文件，目标名在检查之后被别人占用时会失败），再删除临时名。
  - 写入失败、fsync 或 link 失败、收到取消信号：删除临时文件，返回 1，不开始事务，用户已有的文件不会被覆盖。极少见的例外：link 已成功、之后才失败（删除临时名或目录 fsync 失败）时，完整的归档保留在 PATH，返回 1，不开始事务。
  - stderr 警告归档含私钥，请妥善保管。**本版本没有导入命令**：归档只用于留存密钥和实例参数（uuid、私钥、short_id、server、port、sni）供人工查阅或以后版本导入；把它直接解压回原位不是受支持的恢复方式——manifest 会是 `installed`，但 unit 和二进制都不存在，实例会被判为“不一致”。要重新部署请重新 `install`。

### `sudo proxyctl node service recover [--abandon]`（Task 8，规格 §7.4–§7.6、§8.6）

- 只在“未完成事务”状态可用（事务区或墓碑目录存在）；其他状态返回 1，不做任何修改。取锁顺序和其他变更命令相同（L1→L3，取锁后再确认状态），不检测订阅中心角色。它和自动回滚共用同一个恢复引擎，没有第二套恢复逻辑。
- 按残留内容处理：
  - **running / rolling_back**：先校验全部受管路径与临时名（任何一项不属于该阶段允许的状态即判定为外部篡改，见下），再回滚或续做回滚：先降级 systemd，再从快照恢复文件，最后恢复 systemd（原本 active 则 restart 并用事务前的 manifest 重跑本机门禁），删除事务区。修改任何受管路径之前的残留直接丢弃事务区。成功返回 0。
  - **committed**：只向前完成清理（例如 reinstall 提交后要删的旧版本目录），不回滚，删除事务区，返回 0。
  - **墓碑目录**（`.proxyctl-reality-txn.tomb-<txn_id>`）：整体删除，不读取其中内容，返回 0。
  - **有事务区但没有 `journal.json`**：只含 `journal.json.tmp`，或者为空，说明崩溃发生在事务开始阶段，没有修改任何受管路径，整体删除，返回 0；其他任何内容都返回 5，不做修改。
  - recover 自己被中断（进程被杀、系统断电）后再次执行会收敛到同一结果；收到 SIGINT/SIGTERM 时不会中断恢复本身（回滚使用独立的 120 秒预算），只有在恢复开始之前收到才返回 1。
- **篡改与无法确认**：检测到外部篡改、journal 的 schema 未知（stderr 会给出写入它的 `proxyctl` 版本，请用该版本执行 recover）、门禁失败、回滚超时，返回 5：保留事务区和现场、不修改 journal，stderr 输出事务区路径，处理步骤见 [troubleshooting.md](troubleshooting.md)。
- **`--abandon`**（人工放弃出口）：只有全部受管路径都已经处于事务前状态、且没有未登记的临时名时才可用——这种情况只会出现在“文件已全部恢复，但恢复后的门禁失败”之后。它**不**重启服务、**不**跑门禁，保持 systemd 当前状态，删除事务区，返回 0，并在 stderr 警告服务可能没有运行、需要人工排查（`systemctl status proxyctl-reality`，必要时 `reinstall`）。条件不满足（包括已提交的事务）返回 1，不做任何修改。`--abandon` 只能出现在 `node service recover` 和 `node publish --recover`（订阅中心，见 [cli-reference.md](cli-reference.md)）上，出现在其他命令里返回 2。
- 成功时 stdout 没有输出；stderr 说明做了什么，并提示用 `show` 查看当前状态。

### 粘贴输出格式（规格 §10.1）

每个输出块恰好一行不以 `#` 开头的可执行命令；有备选命令时，备选命令连同说明整行以 `#` 开头。所有命令都带 `--publish --min-version v0.10.0`，flag 在节点名之前，参数值用单引号引用（空值 `''`）。四个模板：

```bash
sudo proxyctl node add --publish --min-version v0.10.0 --type vless-reality --server '<server>' --port '<listen_port>' --uuid '<uuid>' --public-key '<pubkey>' --short-id '<short_id>' --sni '<sni>' --name '<name>'
sudo proxyctl node enable --publish --min-version v0.10.0 '<name>'
sudo proxyctl node disable --publish --min-version v0.10.0 '<name>'
sudo proxyctl node remove --publish --min-version v0.10.0 '<name>'
```

`uninstall` 输出 disable（备选 remove），`uninstall --purge` 输出 remove（备选 disable），格式同上，每块恰好一条可执行命令。这四条命令在订阅中心上由 `proxyctl node add/enable/disable/remove --publish` 执行（v0.10 Task 9 起可用，参数、幂等规则和退出码见 [cli-reference.md](cli-reference.md) 的“`node publish` 与带 `--publish` 的 CRUD”；中断后的处理见 [troubleshooting.md](troubleshooting.md)）。订阅中心的 `proxyctl` 低于 `--min-version v0.10.0` 时命令会返回 1 并提示升级；更旧、不认识 `--publish` 的 `proxyctl` 会以退出码 2 报“未知 flag”，不修改任何东西。

### 退出码（`node service` 全部子命令）

| 退出码 | 含义 |
|---|---|
| 0 | 成功（可能伴随外网探测等告警）；`recover` 无论回滚、向前清理还是丢弃空事务区，成功都是 0 |
| 1 | 用法之外的失败，且第一个受管资产被修改之前：预检失败、角色或状态不符、锁超时、dest 不可达、下载或 `check` 失败、`--export-backup` 失败、`recover --abandon` 条件不满足等（包括事务区已创建但尚未修改任何受管路径、已被自动删除的情况） |
| 2 | 用法错误（严格解析失败、缺少必填参数、参数不成对等），不取锁、不发起网络请求、不修改任何文件 |
| 4 | 已自动回滚到事务前状态 |
| 5 | 需人工介入：事务已经开始，而本进程无法确认已经恢复——检测到外部篡改、回滚失败或超时、恢复后复查失败，或提交后的向前清理没有完成；stderr 输出保留的事务区路径，处理步骤见 `docs/troubleshooting.md` |

### 信号与取消（规格 §13.3）

`SIGINT`/`SIGTERM` 取消主 context。锁等待期间收到：立即返回 1。第一个受管路径修改之前收到：删除事务区，返回 1。事务进行中收到：回滚，成功返回 4。回滚和提交后的向前清理一律使用独立于主 context 的 context（回滚最长 120 秒），因此回滚过程中或提交后清理过程中再收到信号也不会被打断：回滚仍然完成并返回 4，已提交的事务仍然向前完成并返回 0。事务开始之后任何不能确认已经恢复的失败都返回 5，而不是 1。

### 已知限制

- 修改后的私网拒绝列表只在 `install`/`reinstall` 时生成；网卡地址或 `--server` 解析结果变化后须重新执行一次 `reinstall`。
- `reinstall` 不接受修改 `name`/`server`/`port`/`sni`；需要这些变化时先 `uninstall --purge` 再重新 `install`（公钥、UUID、short_id 会重新生成，订阅中心需要先 `remove` 旧节点再 `add`）。
- `--export-backup` 归档没有受支持的导入或解压恢复方式，见上文。
- 真实 systemd 上 uninstall/purge 的一个恢复窗口尚未验证，待 Task 13 真机验收：unit 文件已删除之后、`daemon-reload` 完成之前发生崩溃，或 `daemon-reload` 调用本身失败时，回滚的降级步骤会对已不存在的 unit 文件执行 `systemctl disable`，真实 systemd 可能拒绝，结果是返回 5（而不是 4 或 0），事务区和现场原样保留；处理方式见 [troubleshooting.md](troubleshooting.md) 中关于退出码 5 的通用做法。

## 交互菜单（`sudo proxyctl node`，Task 10，规格 §4.4）

节点机上不带参数运行 `sudo proxyctl node`（stdin 与 stdout 都是终端）即可用菜单完成安装与日常管理，不必记住命令行参数；管道或脚本里仍只输出用法并返回 2。非 root 时只提示用 `sudo` 重新运行（返回 1），不读取角色。菜单首行显示“订阅中心=是/否/不确定”与 Reality 代理服务的状态；当前状态下不可用的项照样显示并注明原因。存在未完成的事务时，角色行下方给出 `sudo proxyctl node service recover`（发布事务则是 `sudo proxyctl node publish --recover`），菜单没有恢复项。

节点机上可用的项：

- **安装 VLESS Reality 服务**：依次问节点名称、服务端地址、监听端口、SNI。服务端地址的默认值来自对 `api.ipify.org` 的 IPv4 探测（10 秒超时，仅 IPv4），**只是默认值，须你确认或修改**；探测失败时手动输入。SNI 默认 `www.microsoft.com`，同样须确认。提示：大量部署共用同一个 SNI，与云厂商的 IP 段组合在一起，可能成为可识别的特征，建议按需选择不同的、本机所在网络可达的站点。
- **显示注册命令**：等同 `node service show`，输出粘贴到订阅中心的那一条命令。
- **幂等重装**：等同 `node service reinstall`。
- **卸载**：先选“保留身份”或“彻底清除”（已卸载但保留身份的状态只能彻底清除）；彻底清除时再询问是否导出备份归档（归档含私钥，没有受支持的导入方式，见上文）。

每一项都是：收集输入 → 打印等价的完整命令 → 询问确认 → 调用同一个 `node service ...` 命令。**确认之前不取锁、不写入任何文件**；所有确认默认“否”（`[y/N]`），只有只读的“显示注册命令”默认“是”（`[Y/n]`，直接回车即执行 `node service show`）。回答“否”（或任何不是 `y`/`yes` 的内容）、在默认“否”的确认处直接回车、或输入结束（Ctrl-D），都是无副作用的取消。确认之后菜单把控制权交给该命令，退出码、报错和 SIGINT/SIGTERM 行为与直接运行它完全一致（菜单不改写它们）。订阅中心一侧的菜单项见 README“交互菜单”一节和 [cli-reference.md](cli-reference.md)。

## 已实现：只读检测

### 订阅中心角色（`internal/role`）

判定本机是否为订阅中心，三态结果：

- **是**：`install.json` 可解析，`domain` 非空，`subscription_root` 是已存在目录，`token_file` 指向的 token 文件有效，且默认 `nodes.conf` 存在。
- **否**：`install.json`、默认 token 文件、默认 `nodes.conf` 三者都不存在。
- **不确定**：其余情况，逐项列出缺失或异常的文件。常见原因和处理方式见 [troubleshooting.md](troubleshooting.md#订阅中心角色检测显示不确定)。

判定完全只读，不创建、不修改任何文件。

### Reality 实例状态（`internal/realitynode`）

按优先级判定本机 Reality 代理实例的状态，命中第一条即停止：

1. **未完成事务**：事务区或墓碑目录存在（优先于其他一切判断）。
2. **未安装**：`/etc/systemd/system/proxyctl-reality.service`、`/etc/proxyctl-reality/`、`/var/lib/proxyctl-reality/`、`/usr/local/lib/proxyctl-reality/` 全部不存在。
3. **已安装**：manifest 的 `state=installed`，且 manifest 记录的每项资产（unit、config、私有二进制按内容哈希；`secrets.json`、`instance.json` 按 mode/owner 和内部 schema 字段）都通过校验。
4. **已卸载但保留身份**：manifest 的 `state=uninstalled`；`secrets.json`、`instance.json` 各有一条 manifest 动态状态条目，且按该条目通过 mode/owner 和内部 schema 字段校验；unit、config、私有二进制目录都不存在。
5. **不一致**：以上都不满足，逐项列出不一致之处，不提供自动修复。

同样完全只读；`锁文件 /run/lock/proxyctl-reality.lock` 不属于实例资产，不参与判定。

### 身份生成与推导（`internal/realitynode`）

- `GenerateIdentity`：生成 UUIDv4、X25519 私钥（base64 raw-url 编码）和 8 字节 short_id。
- `DerivePublicKey`：从私钥实时推导公钥；公钥从不持久化存储。任何错误信息都不回显传入的私钥内容。

X25519 推导的正确性由 RFC 7748 §6.1 的标准测试向量验证（`internal/realitynode/identity_test.go`），向量取自 Go 标准库 `crypto/ecdh` 自身的测试数据。

## 已实现：实例配置、私有下载与自检（`internal/realitynode`，Task 6）

以下能力已被上文 `node service install/reinstall/show` 消费（Task 7）。

### 私有 sing-box 下载与校验（`download.go`）

- 固定版本 `1.14.1`，两种架构（amd64/arm64）的 tarball SHA256 编译期写死；`reinstall --singbox-version/--singbox-sha256` 走同一套校验路径。
- 下载、SHA256 校验、解包（只接受目标成员这一个普通文件，拒绝符号链接/硬链接/其他类型/重复条目，大小上限 200 MB）都发生在调用方提供的事务区 `dl/` 目录（经目录 fd 操作，不经字符串路径）。
- `sing-box version` 按空白分词精确匹配版本 token（`1.12.1` 不会被 `1.12.10` 误匹配）；`sing-box check -c <config>` 用于配置校验。
- 所有外部命令（`version`/`check`，以及 `systemd.go` 的 `systemctl`）通过同一个 `ExecRunner`：每次调用有固定超时，子进程置于独立进程组，超时或取消时对整个进程组发 `SIGKILL` 并 `Wait`，避免孙进程残留端口或文件描述符。

### 服务配置、地址与端口选择（`config.go`）

- `--name`/`--server`/`--port`/`--sni` 的参数校验：`--server` 接受 IPv4 字面量或主机名（拒绝 IPv6 字面量），`--sni` 只接受主机名（拒绝任何 IP 字面量）。
- **SNI 是共用的伪装特征，这本身是一项限制，而不是可以随意忽略的细节**：默认 `www.microsoft.com`，同一个 `--sni` 可以被多个 Reality 节点共用。但大量部署共用同一个 SNI，与云厂商的 IP 段组合在一起，可能成为可被识别的特征（规格 §4.4）。交互式安装时须由用户确认或修改这个默认值，不能不假思索地照单全收。
- 服务端配置 JSON 的字段结构与 Task 1 用固定版本 sing-box 真实验证过的结构逐字段一致（`internal/realitynode/probe_integration_test.go` 的 `renderServerConfig`）。
- **私网拒绝规则**：生产配置显式列出 RFC 1918、`100.64.0.0/10`、链路本地、IMDS、IPv6 ULA/link-local 等固定拒绝列表，另外拒绝“本机地址列表”——本机全部非回环接口的 IPv4/IPv6 地址，以及 `--server` 对应的地址（IPv4 字面量直接加入；主机名解析出的全部 A/AAAA 记录加入，覆盖 1:1 NAT 场景下公网地址经隧道绕回本机的风险）。**唯一放行的私网目标**是 `127.0.0.1:<selfcheck_port>`，仅供自检使用。
- **地址变化不会自动生效**：本机地址列表只在 install、reinstall 时重新生成；网卡地址或 `--server` 解析结果变化后，必须执行一次 `reinstall` 才能刷新拒绝列表。
- `selfcheck_port` 从 `/proc/sys/net/ipv4/ip_local_port_range` 下限之外的 `[10000, 下限-1]` 区间随机选取，避开 `listen_port`，确认可绑定 `127.0.0.1`，最多尝试 10 次；范围为空或 10 次都失败时判为错误。reinstall 时如果记录的端口不再可绑定，会按同一规则重新选取。
- systemd unit 内容固定，与 Task 1 的实测夹具（`testdata/probe/reality.service`）逐字节一致，只替换 sing-box 版本号。

### 门禁与自检（`systemd.go`、`selfcheck.go`）

- `Systemctl` 是 `internal/transaction.Systemd` 接口的生产实现（`systemctl` 调用，15 秒超时），并提供 `BuildGate` 构造出的“本机门禁”（规格 §9.2）：最多轮询 10 秒，确认 unit `ActiveState=active`、`0.0.0.0:<listen_port>` 有 TCP 监听、监听 socket 属于 unit 的 `MainPID`、`/proc/<MainPID>/exe` 等于目标版本的私有二进制路径，再执行一次 `sing-box check`。这个 `transaction.Gate` 既用于正向安装/重装的自检，也用于回滚后的门禁复查（回滚时目标 manifest 是事务前 manifest,由调用方传入）。
- `ProbeDest` 实现 dest 探测（TLS 1.3 握手 `<sni>:443`，系统根证书校验，ALPN 提供 `h2`/`http/1.1` 但不强制协商到 h2）。**dest 探测只能证明安装那一刻可达**：dest 站点后续不可用，或被上游封锁，握手会失败，但不会有任何后续自动告警——两次自检之间无法持续监控 dest 可用性。
- `RunSelfcheck` 实现本地 Reality 数据通路（E1/E2/E3）与外网探测：`proxyctl` 自身持有 E3（`127.0.0.1:<selfcheck_port>`）返回随机 nonce；临时客户端复用 `health/remote.GenerateConfig` 生成配置，写入事务区（0600，含 uuid），经 E2 请求 E3 验证 nonce；成功后复用同一客户端探测外网（默认 `https://www.gstatic.com/generate_204`），失败只告警，不影响自检结果（结果里单独有一个 egress 分类字段，方便区分“外网告警”和“整体成功”）。四类失败（dest 不可达/本机服务/协议/出口外网）用类型区分，供 CLI 层映射退出码，不依赖错误文本；E2 的最多三次启动重试和 nonce 请求共用规格 §13.2 的同一个 30 秒预算，不是各自独立的 30 秒。E3 绑定失败（端口已被占用）时会尽力给出占用者的 PID 和进程名（读取 `/proc`，查不到时不影响“本机服务”这一判定，只是诊断信息缺失）。
- 任何退出路径（成功、失败、取消、各类超时）都保证：临时客户端进程被杀且已 `Wait`、E3 监听已关闭、HTTP 连接已释放，端口可立即重新绑定。

## 尚未实现

- bootstrap 首装脚本（`bootstrap-node.sh`）及其 `--upgrade` 模式：Task 11–12。

“已实现：只读检测”与“已实现：实例配置、私有下载与自检”两节描述的 Task 2/6 库函数已被上文 `node service install/reinstall/show` 消费，不再是仅能通过 Go 测试触达的独立层。
