# Reality 节点（施工中，v0.10）

本文档记录 v0.10 独立 Reality 节点功能的实现进度。`proxyctl node service install|reinstall|show` 已经可用（Task 7）；`uninstall`、`uninstall --purge`、`recover` 尚未交付（Task 8），交互菜单和 bootstrap 首装脚本尚未交付（Task 9–12）。参数全集见 [docs/cli-reference.md](cli-reference.md)。

完整设计见 [v0.10 定稿规格](superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md)；已完成 Task 的真实验证证据见 [reality-node-validation.md](reality-node-validation.md)。

## 本机代理服务命令（`proxyctl node service`，Task 7）

三个命令共同的顺序（规格 §8）：严格解析全部参数（含取值校验）→ 检查 root → 快速预检 → 取锁（`install`/`reinstall` 取 L1 再 L3，`show` 只取 L3，最长等待 10 秒）→ 取锁后复查 → 执行。命令必须以 root 运行；订阅中心角色必须是“否”，否则拒绝（`install`/`reinstall` 需要，快速预检与取锁后各查一次）。

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

### 粘贴输出格式（规格 §10.1）

每个输出块恰好一行不以 `#` 开头的可执行命令；有备选命令时，备选命令连同说明整行以 `#` 开头。所有命令都带 `--publish --min-version v0.10.0`，flag 在节点名之前，参数值用单引号引用（空值 `''`）。四个模板：

```bash
sudo proxyctl node add --publish --min-version v0.10.0 --type vless-reality --server '<server>' --port '<listen_port>' --uuid '<uuid>' --public-key '<pubkey>' --short-id '<short_id>' --sni '<sni>' --name '<name>'
sudo proxyctl node enable --publish --min-version v0.10.0 '<name>'
sudo proxyctl node disable --publish --min-version v0.10.0 '<name>'
sudo proxyctl node remove --publish --min-version v0.10.0 '<name>'
```

这四条命令目前还不能在订阅中心上真正执行——`--publish` 和带 `--publish` 的 `node add/edit/enable/disable/remove` 由 Task 9 交付；在此之前粘贴这些命令会以退出码 2 报“未知 flag”。

### 退出码（`node service` 全部子命令）

| 退出码 | 含义 |
|---|---|
| 0 | 成功（可能伴随外网探测等告警） |
| 1 | 用法之外的失败，且第一个受管资产被修改之前：预检失败、角色或状态不符、锁超时、dest 不可达、下载或 `check` 失败等（包括事务区已创建但尚未修改任何受管路径、已被自动删除的情况） |
| 2 | 用法错误（严格解析失败、缺少必填参数、参数不成对等），不取锁、不发起网络请求、不修改任何文件 |
| 4 | 已自动回滚到事务前状态 |
| 5 | 需人工介入：事务已经开始，而本进程无法确认已经恢复——检测到外部篡改、回滚失败或超时、恢复后复查失败，或提交后的向前清理没有完成；stderr 输出保留的事务区路径，处理步骤见 `docs/troubleshooting.md` |

### 信号与取消（规格 §13.3）

`SIGINT`/`SIGTERM` 取消主 context。锁等待期间收到：立即返回 1。第一个受管路径修改之前收到：删除事务区，返回 1。事务进行中收到：回滚，成功返回 4。回滚和提交后的向前清理一律使用独立于主 context 的 context（回滚最长 120 秒），因此回滚过程中或提交后清理过程中再收到信号也不会被打断：回滚仍然完成并返回 4，已提交的事务仍然向前完成并返回 0。事务开始之后任何不能确认已经恢复的失败都返回 5，而不是 1。

### 已知限制

- 修改后的私网拒绝列表只在 `install`/`reinstall` 时生成；网卡地址或 `--server` 解析结果变化后须重新执行一次 `reinstall`。
- `reinstall` 不接受修改 `name`/`server`/`port`/`sni`；需要这些变化时先 `uninstall --purge`（Task 8）再重新 `install`。
- 本 Task 尚未交付 `uninstall`、`uninstall --purge`、`recover`；出现“未完成事务”状态时，暂时只能参考 `docs/troubleshooting.md` 的人工处理步骤，或等待 Task 8。

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

- `proxyctl node service uninstall`、`uninstall --purge`、`recover`：Task 8。
- 订阅中心发布事务（`node publish`、带 `--publish` 的 CRUD）：Task 9。上文粘贴命令目前还不能在订阅中心执行。
- 交互菜单（`sudo proxyctl node`）：Task 10。
- bootstrap 首装脚本（`bootstrap-node.sh`）及其 `--upgrade` 模式：Task 11–12。

“已实现：只读检测”与“已实现：实例配置、私有下载与自检”两节描述的 Task 2/6 库函数已被上文 `node service install/reinstall/show` 消费，不再是仅能通过 Go 测试触达的独立层。
