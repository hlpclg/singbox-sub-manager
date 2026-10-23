# v0.10 Task 1 — Reality 运行时兼容性实测记录

本文档是 [v0.10 实施计划](superpowers/plans/2026-09-18-v0.10-reality-node-install.md) Task 1
的实测证据记录。它只记录已经在真实二进制/真实网络上验证过的范围，**不代表 v0.10 已实现**：
本 Task 不产生任何生产生命周期代码，`internal/realitynode` 目前只有一个带
`reality_integration` 构建标签的测试文件。

## 实测环境

- 宿主：Ubuntu 24.04.4 LTS (noble)，`uname -m` = `x86_64`。
- 该容器 **不是以 systemd 作为 PID 1 启动的**（`systemctl status` 返回
  `System has not been booted with systemd as init system (PID 1). Can't operate.`）；
  `systemd-analyze`（255 版）以静态方式可用，见下文。
- Go：本机 `go version go1.24.7 linux/amd64`。
- 无法访问 Docker daemon（`docker info` 报
  `dial unix /var/run/docker.sock: connect: no such file or directory`），因此计划要求的
  `golang:1.22-bookworm` 容器（`g122` helper）在本轮实测环境中不可用。**下面记录的全部
  Go 命令均用本机 Go 1.24 执行，不满足 AGENTS.md「Go 命令必须在 Go 1.22 容器运行」的要求**；
  这是用户在本轮任务开始前明确认可的例外（详见下方"环境限制与范围收窄"），不是自行放宽。
- `shellcheck` 初始未安装；已通过 `apt-get install -y shellcheck` 安装 0.9.0 版本（与
  ci.yml/release.yml 使用的 Ubuntu runner 同源 apt 包）。

## 环境限制与范围收窄（先于实测记录的决策）

在开始下载/实测之前，与用户确认了以下环境能力缺口，并获得针对每一项的明确指示后才继续：

1. **无法提供计划要求的一次性 root/systemd VM**（Debian 12、Ubuntu 22.04/24.04，
   每种至少一台真实目标环境），也无法提供 arm64 真机或可靠仿真。决定：完成本环境下可行的
   部分，其余记录为阻塞，不冒充通过。
2. **无 Docker daemon，无法运行计划规定的 Go 1.22 容器**。决定：改用本机 Go 1.24，
   所有证据明确标注"不满足 AGENTS.md 的 Go 1.22 容器要求"。

以下每一节明确标注"已实测（真实）"或"受阻（原因）"。

## 1. 固定上游资产：SHA256 与归档结构（真实）

命令（2026-09-25 执行）：

```
curl -sSL -o amd64.tar.gz https://github.com/SagerNet/sing-box/releases/download/v1.14.1/sing-box-1.14.1-linux-amd64.tar.gz
curl -sSL -o arm64.tar.gz https://github.com/SagerNet/sing-box/releases/download/v1.14.1/sing-box-1.14.1-linux-arm64.tar.gz
sha256sum amd64.tar.gz arm64.tar.gz
```

实测输出：

```
12cb2816b52febb356f6a885b740cc8758c3f30b8ae0ca8edba80f0d2d35343f  amd64.tar.gz
6060b42fa84c5dcaeae1799af7f61b0f1ae4855d9d5ddc9e02baba17154b3ae2  arm64.tar.gz
```

与计划 `docs/superpowers/plans/2026-09-18-v0.10-reality-node-install.md` 固定的两个值逐字符相符，
**两种架构均已下载并核实，不只是元数据核对**。

归档实际条目（`tar -tzv`）：每个 tarball 含一个目录、`LICENSE`（791 字节）、`libcronet.so`
（约 11–12 MB，未使用的可选组件）、以及目标成员 `sing-box-1.14.1-linux-<arch>/sing-box`
（amd64 约 81 MB，arm64 约 76 MB，均为可执行的普通文件）。规格 §6 的提取规则「只提取这一个
普通文件」与实际归档结构之间**没有冲突**：目标成员确实是唯一匹配的普通文件，大小在
200 MB 上限之内，且不是符号链接。

Go 测试对应：`TestPinnedReleaseProbe`（`internal/realitynode/probe_integration_test.go`）对两种
架构都执行下载、SHA256 校验、单文件提取与类型/大小断言；此外只在宿主架构（amd64）上执行：

- `sing-box version`：输出首行按空白分词后，token 集合精确包含 `1.14.1`（不是子串匹配）。
- 用规格 §8.1 的精确 JSON 结构（含真实生成的身份）执行 `sing-box check`，通过。

arm64 二进制**只验证了哈希与归档提取**；`version`/`check` 执行在此 amd64 宿主上不可行，
测试中以 `t.Logf` 显式记录为阻塞，不计入通过项。

## 2. 固定 systemd unit 模板（部分真实：静态验证）

`internal/realitynode/testdata/probe/reality.service` 与规格 §8.1 的 unit 文本逐字节一致。

**受阻**：本容器未以 systemd 作为 PID 1 启动，`systemctl start/enable` 无法运行；直接写入
`/etc/systemd/system/` 尝试真实起停也被环境策略拒绝（预期之内——该目录外的持久化写入本就不
应该在没有明确授权的情况下发生）。因此规格要求的「三种 OS 的 unit 原文可用，真实
enable/start/ActiveState」**未能在本容器验证**，需要计划 Task 13 的真实一次性 VM 补齐。

**已实测的替代静态证据**：`systemd-analyze verify`（无需运行中的 systemd 实例，纯静态解析）
接受渲染后的 unit；额外验证了该命令能区分「语法/指令错误」与「ExecStart 目标文件在本机不存在」
两类问题（前者产生 `Unknown key name ... ignoring` 一类独立诊断行，后者产生
`Command ... is not executable: No such file or directory`），因此
`TestRealityUnitProbe` 只在诊断信息**恰好**是「ExecStart 指向的固定生产路径本机不存在」时判定
通过，其余任何诊断都判定失败。命令：

```
systemd-analyze verify <渲染后的 unit 文件路径>
```

## 3. 真实 E1→E2→E3 握手（真实）

`TestRealityDataPathProbe` 用下载并校验过的 amd64 `sing-box` 二进制，启动两个真实子进程：

- 服务端（E1）：规格 §8.1 的精确 JSON 结构，身份用 `crypto/ecdh`（X25519）+ `crypto/rand`
  现场生成（UUIDv4、私钥、8 字节 short_id），私钥不落盘、公钥由 `PublicKey()` 实时推导。
- 客户端（E2）：直接复用 `internal/health/remote.GenerateConfig`（v0.9 已有的生产函数），
  以证明规格要求的"现有 remote.GenerateConfig 生成客户端"这一点是真实可行的，不是假设。

E3 用 Go 标准库 `net/http` 起一个只在测试期间存在的监听器，对任意路径返回 32 字节随机 nonce。
客户端经真实 Reality 握手（TLS1.3 + Reality 认证，dest 为 `www.microsoft.com:443`，真实网络
出网）请求 E3，断言状态码 200 且正文与 nonce 完全相等。

**踩坑记录**（保留是为了防止后来者重复踩同一个坑）：最初用 `curl -x <proxy>` 手工验证时，
无论内层服务是否真的在监听，请求都"成功"返回了目标内容——根源是本容器环境变量
`no_proxy` 显式包含 `127.0.0.1`，而 curl 即使收到显式 `-x` 也会对 `no_proxy` 命中的目标
直接连接、绕过指定代理（`strace` 证实其 `connect()` 直接打到目标端口，而非代理端口）。
这与 sing-box、Reality、路由规则均无关，纯粹是手工验证脚手架的问题；改用
`env -u no_proxy ... curl` 后复现出真实的 502/连接拒绝，证明之前的"通过"是假阳性。
Go 测试改用 `http.Transport{Proxy: http.ProxyURL(...)}`（显式函数式代理选择器，不读取
`NO_PROXY` 环境变量），不受此问题影响，但记录于此以警示：任何后续人工复测都必须显式清空
代理相关环境变量，或改用不受这些变量影响的客户端。

## 4. 私网/本机拒绝规则（部分真实：回环目标动态验证，其余静态验证）

`TestRealityRoutingProbe` 用同样真实的服务端+客户端进程，验证规格 §8.1 拒绝规则：

**已动态验证（真实，可观测服务端路由决策日志）**：

- `127.0.0.1:<selfcheck_port>`（唯一放行目标）：客户端经隧道请求成功，收到正确 nonce；
  服务端 debug 日志显示 `router: match[0] ip_cidr=127.0.0.1/32 port=<port> => route(direct)`。
- `127.0.0.1:<其他端口>`（不在放行例外内）：先验证该目标在隧道之外确实可直接连通（对照组，
  避免把"不可达"误判为"被拒绝"），再验证经隧道请求未能取回目标内容；服务端日志显示
  `router: match[1] ip_cidr=[...] => reject` 与 `router: connection closed: rejected`。
- `0.0.0.0:<端口>`（绑定到全部接口的本机服务）：同上模式，先证明直接可达，再证明隧道内被拒绝，
  同样命中 `match[1] ... => reject`。

**未动态验证，改用静态证据（原因）**：规格拒绝列表中的 RFC1918（`10.0.0.0/8` 等）、
`100.64.0.0/10`、`169.254.0.0/16`（含 AWS/GCP IMDS `169.254.169.254`）、IPv6 ULA
（`fc00::/7`）、IPv6 link-local（`fe80::/10`）等条目，**没有在本容器动态验证「拒绝规则
生效」**。原因：独立探测确认这个容器的原始网络层会对它并不拥有的地址返回虚假的"连接成功"——

```
python3 -c "import socket,time
for h,p in [('169.254.169.254',80),('10.0.0.1',80),('192.168.1.1',80),('100.64.0.1',80)]:
    s=socket.socket(); s.settimeout(3)
    t=time.time(); s.connect((h,p)); print(h,p,'CONNECTED', time.time()-t)"
# 输出：四个目标全部在 <1ms 内 "CONNECTED"
```

而尝试在本机 `bind()` 这些地址全部失败（`OSError: Cannot assign requested address`），
证明这些地址并非本容器真正拥有或路由到的主机，之前的"connect 成功"是环境的网络虚拟化产物，
不是真实可达性信号。在这种条件下，用它们做"可达对照目标"去证明拒绝规则生效，无法区分
「规则确实拒绝」与「环境本身的伪连通性artefact」——这正是计划明确警告要避免的失败模式
（"不能用目标不可达代替规则拒绝"的反面：也不能用一个自己都无法验证是否真实可达的目标，
去证明拒绝生效）。

因此改为：① 断言渲染后的拒绝规则 `ip_cidr` 列表逐字符包含这些必需网段；②
用真实 amd64 二进制对包含完整拒绝列表的生产配置执行 `sing-box check`，确认 sing-box 接受
该结构。这只证明"配置正确、sing-box 接受"，不证明"运行时确实拒绝"——后者仍需计划 Task 13
在真实网络拓扑（例如真实云 VPC、真实 RFC1918 网段、真实云主机 IMDS 端点）的一次性 VM 上补齐。

基于域名解析到私网地址的拒绝规则（规格 §8.1"以域名形式访问"一条）**未验证**：需要一个
可控的 DNS 基础设施来让测试域名解析到私网地址，本轮判定超出 Task 1 在本环境下的可行范围，
留给 Task 13 或后续实测。

## 5. Go 与 Shell 门禁

以下均已在本环境实际执行（Go 命令用本机 Go 1.24，非计划要求的 1.22 容器，见上方声明）：

```
$ go test -tags reality_integration -v -count=1 -timeout 180s ./internal/realitynode/
PASS  (TestPinnedReleaseProbe, TestRealityUnitProbe, TestRealityDataPathProbe, TestRealityRoutingProbe 全部通过)

$ go test -tags reality_integration -race -count=2 -timeout 200s ./internal/realitynode/
PASS（两轮重复 + race 均通过；第一次未加下载缓存时的一次 -count=2 运行曾因重复下载触发
release CDN 一过性 502，已通过引入进程内下载缓存 + 3 次重试修复，修复后的重跑见上）

$ go build ./...
$ go vet ./...
$ gofmt -l .              # 无输出
$ GOOS=darwin go build ./...
$ go test -count=1 ./...        # 全部既有包通过，未受影响
$ go test -race -count=1 ./...  # 全部既有包通过，未受影响

$ bash -n tests/test_reality_probe.sh
$ shellcheck tests/test_reality_probe.sh   # shellcheck 0.9.0，apt 安装，无输出
$ bash tests/test_reality_probe.sh --verify-assets   # 真实下载两个 tarball 并核对哈希，exit 0
$ bash tests/test_reality_probe.sh                   # 无参数，usage，exit 2
$ bash tests/test_reality_probe.sh --bogus           # 未知参数，usage，exit 2
```

（曾发现并修复一个真实 bug：`cleanup()` trap 在 `TMP_DIR` 仍为空时以 `[[ -n ... ]] && rm ...`
的短路失败收尾，会把 EXIT trap 的非零状态回写为脚本最终退出码，导致 `usage()` 里显式的
`exit 2` 被吞掉、脚本实际以 1 退出。修复为 trap 函数显式 `return 0`。）

## 6. 未完成/阻塞事项一览

| 项目 | 状态 | 原因 | 由谁补齐 |
|---|---|---|---|
| Debian 12 / Ubuntu 22.04 真实 systemd 一次性 VM 覆盖 | 阻塞 | 本容器非 systemd PID 1，无法提供多 OS VM | 计划 Task 13 |
| arm64 真机执行（version/check/握手/路由） | 阻塞 | 无 arm64 硬件或可信仿真；只验证了哈希与归档提取 | 计划 Task 13 |
| unit 真实 enable/start/ActiveState | 阻塞 | 同上；已用 `systemd-analyze verify` 做静态替代 | 计划 Task 13 |
| RFC1918/100.64.0.0/10/169.254.0.0/16/IPv6 ULA/link-local/IMDS 动态拒绝验证 | 阻塞 | 容器网络层对非自有地址返回伪连通性，无法作为可信对照目标 | 计划 Task 13（真实网络拓扑） |
| 域名解析到私网地址的拒绝 | 未验证 | 需要可控 DNS 基础设施，超出本轮范围 | 后续实测 |
| Go 命令的 Go 1.22 容器合规性 | 不合规 | 本环境无 Docker daemon | 需在有 Docker 的环境重跑，或接受本记录的例外 |

## 结论

固定版本 **sing-box 1.14.1** 的官方资产哈希、归档结构、版本 token、`check` 接受度，以及
规格 §8.1 配置结构下的真实 E1→E2→E3 握手和回环/`0.0.0.0` 私网拒绝规则，均已在本环境下用
真实二进制、真实网络往返验证通过，证据可重复执行（见上方命令）。固定 unit 模板的语法通过
静态 `systemd-analyze verify` 确认。多 OS/多架构的真实 systemd 生命周期覆盖，以及非回环私网
目标的动态拒绝验证，仍然阻塞，未被冒充为已完成——这两类差距需要计划 Task 13 的真实一次性
VM 环境补齐。**本次未创建任何生产生命周期代码，未标记本 Task 完成，未进入 Task 2。**

---

# v0.10 Task 13 — 同 tag Release 集成与自动化流程验证记录

本节记录计划 Task 13 在**自动化**层面做了什么、怎么复跑、环境是什么，以及**哪些没有完成**。
规格 §14.5 的两机真机验收、FlClash 实际出口等**没有执行，下面不把任何一项写成“通过”**。
证据针对的是提交 `c5e91ed` 之上的**未提交工作区**；精确的提交对象、Go 全量门禁和独立终审证据由
Task 13 封账时补入计划，不在本节。

## 自动化覆盖了什么

| 项目 | 命令 | 环境 | 结果（本次实测） |
|---|---|---|---|
| 三脚本版本、ci/release 登记规则、release 资产清单与顺序、`checksums.txt`（真实命令对合成 dist）与 v0.9 解析 | `bash tests/test_release_assets.sh` | ubuntu:24.04 容器（arm64，OrbStack），root 与 uid 1000 各一个全新容器 | 145 通过，0 失败 |
| 同上 + 真实 dist（两个二进制+三个脚本）：`sha256sum -c`、清单、二进制版本串 | `DIST_DIR=dist EXPECT_VERSION=v0.10.0 bash tests/test_release_assets.sh` | `golang:1.22@sha256:1cf6c45ba39db9fd6db16922041d074a63c935556a05c5ccb62d181034df7f02` 容器（Debian 12，arm64，另装 gawk 与 qemu-user-static） | 174 通过，0 失败；arm64 二进制本机直接运行，amd64 二进制经 qemu-user 运行，均精确打印 `v0.10.0` |
| 真实 `proxyctl` 的严格模式解析（退出码 2 先于 root/锁/网络/写入） | `bash tests/test_v010_flow.sh` | 同 ubuntu:24.04 容器，root 与 uid 1000 | 12 通过，0 失败，1 个区段显式 SKIP（真实文件系统区段未 opt-in） |
| 节点机侧 fresh→install→show→uninstall→show→reinstall→`uninstall --purge --export-backup`；订阅中心侧 legacy→`migrate`→粘贴节点打印的 add→重复粘贴→`--min-version` 拒绝→节点打印的 disable→enable→remove；中心上 `node service install`/`show`/`bootstrap-node.sh` 被拒绝 | `PROXYCTL_REAL_FS_TESTS=1 PROXYCTL_REAL_CANDIDATE=<proxyctl> bash tests/test_v010_flow.sh`（一次性 root 容器，需访问 github.com 与 SNI 主机） | ubuntu:24.04 容器（arm64），候选二进制由上面的 golang:1.22 容器以 `-X main.Version=v0.10.0` 构建；连续运行两次均 122 通过 0 失败，第二次证明清理干净 | 真实 sing-box 1.14.1 下载并校验、真实配置与本机自检；订阅文件通过 127.0.0.1:443 的一次性 TLS 服务以 HTTPS 取回并与磁盘字节比对 |
| bootstrap 小修 | `bash tests/test_bootstrap_upgrade.sh` | 同上 | 新增 `setsid --wait`、PREVIOUS_DIR 遗留临时文件两组用例；见下“已知冲突” |
| `PROXYCTL_VERSION` 升到 v0.10.0 | `bash tests/test_install_monitor.sh`（第 12、26 行字面量改为 `v0.10.0`，批准的唯一既有测试改动） | 同上 | 通过 |

静态：`bash -n` 与 ShellCheck 0.9.0（ubuntu:24.04 的 apt 包）对三个脚本和全部 `tests/test_*.sh`（`tests/test_caddy_apt.sh` 除外，见下）通过。

CI 登记规则（R1 `bash -n`、R2 ShellCheck、R3 执行、R4 每个脚本被至少一个已登记测试引用、R5 无 `continue-on-error`/`|| true`/步骤级 `if:`）由 `tests/test_release_assets.sh` 对 `ci.yml` 与 `release.yml` 同时检查；
`tests/test_reality_probe.sh` 必须以 `--verify-assets` 登记，`tests/test_caddy_apt.sh` 在 `ci.yml` 以离线模式、在 `release.yml` 以 `CADDY_APT_SMOKE=1` 登记。
`tests/test_caddy_apt.sh` 是唯一的 ShellCheck 例外：它是既有测试，ShellCheck 0.9.0 对它有 info/warning 级别提示，规格 §14.6 禁止修改既有测试，所以只做 `bash -n` 和执行，不做 ShellCheck；例外在 `SHELLCHECK_EXEMPT` 中显式列出。

CI 中 `tests/test_v010_flow.sh` 的真实文件系统区段会打印明确的 SKIP 并计入 skipped；它**不是**条件跳过冒充通过：该区段需要 root、网络和一次性主机，CI 里不运行。

### 变异验证（本 Task 新增逻辑）

- `test_release_assets.sh`：对 workflow 与脚本做了 20 个变异（去掉 merge-nodes 资产、checksums 带路径前缀/重复/用通配符/缺脚本、去掉 cp、去掉标记、去掉某测试的 `bash -n`、把执行行注释掉、ShellCheck 列表缺项、`|| true`、`continue-on-error`、步骤级 `if:`、permissions 增加项、去掉 `--verify-assets`、去掉 DIST_DIR 校验、去掉 `EXPECT_VERSION`、去掉 caddy smoke、merge-nodes 版本回到 v0.9.0 等），全部被抓到。DIST_DIR 模式另验：错误的期望版本、arm64 二进制 `-X` 为 `v0.10.0-rc1`、arm64 二进制不带 `-X`、构建后改动二进制使 checksums 过期、缺少 `EXPECT_VERSION`，均失败。
- `test_v010_flow.sh`：对候选 Go 二进制做了 3 个变异（节点机输出的 `--min-version` 改为 `v0.10.1`、`enable` 与 `disable` 的 `want` 取反、取消 `node service` 的角色检查），均被抓到（分别多项、9、3 项失败）。
- bootstrap 小修：`setsid` 不带 `--wait`、清理时不排除符号链接/不检查属主/匹配目录/glob 过宽，均被新用例抓到（`rm -rf` 单独替换在 `-f` 守卫下行为等价，无法由行为区分）。

## 接缝调查与未覆盖的缺口（B2）

`cmd/proxyctl`、`internal/realitynode`、`internal/transaction`、`internal/publish`、`internal/role`、`internal/hostlock` 不读取任何环境变量，也不提供改变根路径的 flag：实例/订阅中心路径、锁文件、sing-box 下载地址（固定为 GitHub Release）、HTTPS 复验地址（127.0.0.1:443）都编译在内。Go 测试靠包内变量和结构体（`newServiceRuntime`、`newPublishRuntime`、`realitynode.Env`、`publish.Env`）注入，shell 测试无法使用。**本 Task 未修改任何 Go 生产代码**，所以集成流程只能对真实固定路径运行（一次性 root 容器），能借用的接缝只有两个：`systemctl` 从 PATH 解析，Go 读取 `SSL_CERT_FILE`。

因此真实流程里：

- systemd 由**有状态的假 `systemctl`** 代替，它只管理 `proxyctl-reality.service` 的 loaded/enabled/active 状态，`start`/`restart` 真的启动 unit 里的 `ExecStart`（真实 sing-box 与真实生成的配置）。它**不模拟任何 systemd 沙箱**（`ProtectSystem`、`NoNewPrivileges`、`Restart=` 等）。
- Caddy 由 `openssl s_server -WWW` 在 127.0.0.1:443 代替，证书由 `SSL_CERT_FILE` 指向的一次性自签证书信任。
- 节点机输出的 add/disable/enable/remove 命令被原样粘贴执行（`sudo` 与 `proxyctl` 以 shell 函数代替）；两种角色在同一台一次性主机上先后出现，因为角色检测使它们不能同时存在。

## 未完成 / 未覆盖（准确标记）

| 项目 | 状态 | 说明 |
|---|---|---|
| 规格 §14.5 六步两机真机验收（订阅中心 + 全新 AWS 节点机） | **未完成** | 没有访问授权的一次性订阅中心和节点 VM；上表的容器流程不能替代 |
| FlClash 更新订阅后握手成功、实际用作出口 | **未完成** | 无客户端访问 |
| §14.5 第 3 步各受限目标（IMDS、本机其他端口、`0.0.0.0:22`、`<server>:22`、IPv6 `fd00:ec2::254`、解析到私网的域名）经节点的动态拒绝 | **未完成** | Task 1 遗留的私网动态拒绝验证仍然阻塞（容器网络层对非自有地址返回伪连通性）；本 Task 未补 |
| 真实 systemd（unit 沙箱、`Restart=`、journald、多 OS Debian 12/Ubuntu 22.04/24.04 的真实 VM） | **未完成** | 只有假 `systemctl`；Task 1 遗留项未关闭 |
| arm64 真机的 sing-box 握手/路由 | **未完成** | arm64 容器里的流程使用 arm64 sing-box 下载与自检通过，但那是容器，不是 arm64 真机 VM；amd64 同理（本机容器是 arm64，未在 amd64 实机/容器上运行流程测试，CI 之外的 amd64 实跑待补） |
| 真实 GitHub Release 下载 `bootstrap-node.sh` 及其固定 tag 资产 | **未完成** | v0.10.0 未发布；bootstrap 的下载路径只由既有 `tests/test_bootstrap_*.sh` 用假 `curl` 覆盖，本 Task 的流程测试没有走 bootstrap 首装 |
| 真实终端 Ctrl-C | **未完成** | 信号路径仍只由脚本测试里的 `kill -INT` 与 Go 的 context 取消测试覆盖 |
| 真实 Go 候选段（`test_bootstrap_upgrade.sh` 的 `PROXYCTL_REAL_FS_TESTS=1` 区段）未入 CI | **仍未入 CI** | CI 显示 SKIP 计数；封账审查时在 ubuntu:24.04 容器以 root + 真实候选单独重跑：902 通过、0 失败、0 跳过 |
| Go 全量门禁（Go 1.22 容器 `go test -race`、`go vet`、`gofmt -l`） | 不在本节 | 由主会话在封账时复跑，本节只写聚焦验证 |

## 本 Task 发现的问题（未修，范围外或需决策）

- **`go version -m` 无法校验 `-X main.Version`**：release 构建使用 `-trimpath`，Go 在 `-trimpath` 下不把 `-ldflags` 写进构建信息（golang/go#52372），所以计划里“arm64 用 `go version -m` 检查 `-X main.Version`”无法成立。改为在 release 里安装 `qemu-user-static`，用 qemu-user 真正运行 arm64 二进制并比较 `version` 的输出。
- **Debian 12 默认 awk（mawk 1.3.4 20200120）不支持 `{64}` 区间表达式（已修复）**：`install-proxy.sh`、`merge-nodes.sh`、`bootstrap-node.sh` 校验 `checksums.txt` 的 awk 原先用 `$1 ~ /^[[:xdigit:]]{64}$/`，在该 awk 上对任何文件匹配 0 条。已改为语义等价的 `length($1) == 64 && $1 ~ /^[[:xdigit:]]+$/`（`*name` 与大小写处理不变，仍要求恰好一条匹配）。`tests/test_release_assets.sh` 新增静态断言（三个脚本不含 `{n}` 区间）与谓词等价表，在 mawk 1.3.4 20200120 和 gawk 5.2.1 下均通过；v0.9 已发布脚本无法修复，其 `update` 在 Debian 12 上仍受此限制。
- **既有断言放宽（已获用户批准）**：`bootstrap-node.sh` 的 `setsid --wait` 修改（C1）使 `tests/test_bootstrap_upgrade.sh` 里既有断言 `the compatibility check child gets fds 8 and 9 closed` 的正则 `setsid timeout .*node service show.*8>&- 9>&-` 不再匹配；已批准仅把它放宽为 `setsid( --wait)? timeout ...`，fd 关闭的断言意图不变，别的断言不动。
