# Reality 节点（施工中，v0.10）

本文档记录 v0.10 独立 Reality 节点功能的实现进度。**当前没有可用的 `proxyctl node service` 命令**；本机代理服务的安装、重装、卸载和交互菜单由后续 Task（7–10）交付。本页只描述已经实现、只读的检测层，供排障和后续开发参考。

完整设计见 [v0.10 定稿规格](superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md)；已完成 Task 的真实验证证据见 [reality-node-validation.md](reality-node-validation.md)。

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

以下能力同样**没有 CLI 入口**：只是供 Task 7（install/reinstall/show）消费的库函数，目前只能通过 Go 测试触达。

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

- `proxyctl node service install/reinstall/show/uninstall/recover`：Task 7–8。
- 共享锁编排（L1/L2/L3）和旧写入入口保护：Task 3（已完成，但尚未接入 Reality 生命周期命令，因为命令本身还不存在）。
- 订阅中心发布事务、交互菜单、bootstrap 安装：Task 9–12。

在这些 Task 交付之前，本页描述的全部函数（含 Task 6 新增部分）只能通过 Go 测试验证，没有对应的 CLI 入口。
