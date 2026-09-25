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
4. **已卸载但保留身份**：manifest 的 `state=uninstalled`；`secrets.json`、`instance.json` 通过校验；unit、config、私有二进制目录都不存在。
5. **不一致**：以上都不满足，逐项列出不一致之处，不提供自动修复。

同样完全只读；`锁文件 /run/lock/proxyctl-reality.lock` 不属于实例资产，不参与判定。

### 身份生成与推导（`internal/realitynode`）

- `GenerateIdentity`：生成 UUIDv4、X25519 私钥（base64 raw-url 编码）和 8 字节 short_id。
- `DerivePublicKey`：从私钥实时推导公钥；公钥从不持久化存储。任何错误信息都不回显传入的私钥内容。

X25519 推导的正确性由 RFC 7748 §6.1 的标准测试向量验证（`internal/realitynode/identity_test.go`），向量取自 Go 标准库 `crypto/ecdh` 自身的测试数据。

## 尚未实现

- `proxyctl node service install/reinstall/show/uninstall/recover`：Task 7–8。
- 共享锁编排（L1/L2/L3）和旧写入入口保护：Task 3。
- 事务引擎（journal、回滚、recover）：Task 4–5。
- 订阅中心发布事务、交互菜单、bootstrap 安装：Task 9–12。

在这些 Task 交付之前，本页描述的检测函数只能通过 Go 测试验证，没有对应的 CLI 入口。
