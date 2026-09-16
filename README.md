# singbox-sub-manager

一键部署 sing-box Hysteria2 节点，并生成统一的 FlClash 与 Shadowrocket 订阅。

> Deploy sing-box Hysteria2 nodes and generate unified FlClash and Shadowrocket subscriptions.

## 功能

- 一键安装或更新 sing-box
- 一键安装和配置 Caddy
- 部署 Hysteria2 服务端
- 自动生成随机订阅路径
- 自动生成 FlClash/Mihomo 配置
- 自动生成 Shadowrocket 订阅
- 支持多个 Hysteria2 节点合并为一个订阅
- 自动保留订阅 token、Hysteria2 密码和混淆密码
- 安装时自动修复旧版 Caddy APT source 与签名 key
- 自动启用 BBR
- 自动配置适合 macOS FlClash TUN 的规则
- 优先代理 Google Play、Google、OpenAI、Claude、GitHub、YouTube 等服务
- 国内域名、私有网络和 Apple 中国服务默认直连
- Caddy 禁用 HTTP/3，避免与 Hysteria2 的 UDP 443 冲突
- 一条命令备份全部持久化状态，恢复失败时按恢复前映像自动回滚
- 升级 proxyctl 失败时自动回滚二进制、校验文件和配置

## 当前支持的系统

正式支持：

| 系统 | 版本 | 状态 |
|---|---:|---|
| Ubuntu | 22.04 LTS | 支持 |
| Ubuntu | 24.04 LTS | 支持 |
| Debian | 12 | 支持 |

当前版本使用 `apt` 安装依赖，因此暂不支持直接运行在以下系统：

- CentOS
- Rocky Linux
- AlmaLinux
- Fedora
- Amazon Linux
- Arch Linux

后续版本计划增加 Rocky Linux 9、AlmaLinux 9 和 Amazon Linux 2023。

## 适用客户端

- FlClash
- Mihomo Party
- Clash Verge Rev
- Shadowrocket
- 其他兼容 Mihomo Hysteria2 配置或 Hysteria2 URI 的客户端

## 架构

每台节点服务器运行：

```text
sing-box
└── Hysteria2 / UDP 443
```

其中一台服务器同时作为订阅中心：

```text
Caddy
├── HTTPS / TCP 443
├── clash.yaml
└── sr.txt
```

多个节点可以合并到同一个订阅：

```text
JP-HY2 ─┐
SG-HY2 ─┼──> 统一 clash.yaml / sr.txt
US-HY2 ─┘
```

## 部署前准备

需要准备：

1. 一台 Ubuntu 22.04、Ubuntu 24.04 或 Debian 12 服务器
2. 一个已经解析到服务器公网 IP 的域名
3. root 权限或可使用 `sudo` 的用户
4. AWS、Oracle Cloud 或其他云平台安全组已放行相应端口

### 必须开放的端口

| 协议 | 端口 | 用途 |
|---|---:|---|
| UDP | 443 | Hysteria2 |
| TCP | 80 | Let's Encrypt 验证 |
| TCP | 443 | HTTPS 订阅 |

AWS EC2 用户需要同时检查：

- EC2 Security Group
- 系统防火墙
- 域名 DNS 解析
- Elastic IP 或公网 IP 是否正确

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/hlpclg/singbox-sub-manager/main/install-proxy.sh \
  -o install-proxy.sh

chmod +x install-proxy.sh

sudo ./install-proxy.sh sub.example.com admin@example.com
```

也可以直接执行：

```bash
curl -fsSL https://raw.githubusercontent.com/hlpclg/singbox-sub-manager/main/install-proxy.sh \
  | sudo bash -s -- sub.example.com admin@example.com
```

参数说明：

```text
sub.example.com   用于提供订阅文件的域名
admin@example.com Caddy / Let's Encrypt 使用的联系邮箱
```

示例：

```bash
sudo bash install-proxy.sh aws-jp.example.com admin@example.com
```

## 安装结果

脚本运行完成后会输出两个订阅地址：

```text
FlClash:
https://sub.example.com/<随机token>/clash.yaml

Shadowrocket:
https://sub.example.com/<随机token>/sr.txt
```

随机 token 保存于：

```text
/var/lib/singbox-sub-manager/token
```

重复运行安装脚本时，订阅地址默认不会改变。

Hysteria2 密钥保存于：

```text
/etc/singbox-sub-manager/config.env
```

重复运行安装脚本时，以下内容默认不会改变：

- Hysteria2 password
- Salamander obfs password
- 订阅 token
- 订阅 URL

## FlClash 使用方法

1. 打开 FlClash
2. 进入订阅管理
3. 添加脚本输出的 `clash.yaml` 地址
4. 更新订阅
5. 开启 TUN
6. 模式选择 `Rule`
7. 在 `节点选择` 中选择 `自动选择`（测速）、`故障转移`（节点故障时自动切换）或指定节点

配置默认包含：

- TUN mixed stack
- Fake-IP DNS
- 自动路由
- DNS 劫持
- 国内直连
- 国外代理
- Google Play 下载修正规则
- 3 个基础策略组：`节点选择`（手动选择）、`自动选择`（url-test 测速）、`故障转移`（fallback 故障转移）
- 11 个服务策略组，可在客户端单独为每个服务切换出口：
  - 默认出口为 `节点选择`：`AI服务`（OpenAI、Claude）、`GitHub`、`流媒体`（YouTube、Netflix、Spotify）、`Disney`、`TikTok`、`Telegram`、`Google`
  - 默认出口为 `DIRECT`：`Bilibili`、`Apple`、`Microsoft`、`游戏`

## Shadowrocket 使用方法

1. 打开 Shadowrocket
2. 添加订阅
3. 粘贴脚本输出的 `sr.txt` 地址
4. 更新订阅
5. 选择节点并连接

`sr.txt` 每行包含一个 Hysteria2 节点 URI。

## 合并多个节点

订阅中心的节点列表保存在：

```bash
/etc/singbox-sub-manager/nodes.conf
```

在每台节点上安装完成后，收集该节点的公网 IP、Hysteria2 password、Salamander obfs password 和 SNI；然后在订阅中心编辑此文件：

```bash
nano /etc/singbox-sub-manager/nodes.conf
```

格式：

```text
# 注释行会被忽略
JP-HY2|1.1.1.1|443|PASSWORD|OBFS_PASSWORD|www.bing.com
SG-HY2|2.2.2.2|443|PASSWORD|OBFS_PASSWORD|www.bing.com
US-HY2|3.3.3.3|443|PASSWORD|OBFS_PASSWORD|www.bing.com
```

字段说明：

```text
节点名 | 服务器IP或域名 | 端口 | Hysteria2密码 | 混淆密码 | SNI
```

节点名不得与内置策略组名或 `DIRECT`、`REJECT` 相同——即 `节点选择`、`自动选择`、`故障转移`、`AI服务`、`GitHub`、`流媒体`、`Disney`、`TikTok`、`Telegram`、`Google`、`Bilibili`、`Apple`、`Microsoft`、`游戏`、`DIRECT`、`REJECT`，共 16 个保留名。已启用节点使用其中任一名称时，`merge` 与 `validate` 会失败并在输出中指出冲突的节点名；禁用节点不受影响。

重新运行安装脚本会读取 `nodes.conf` 并生成订阅，同时保留当前节点的 token 和密钥：

```bash
sudo ./install-proxy.sh sub.example.com admin@example.com
```

也可以单独运行 `merge-nodes.sh`，避免重装服务：

```bash
curl -fsSL https://raw.githubusercontent.com/hlpclg/singbox-sub-manager/main/merge-nodes.sh \
  -o merge-nodes.sh
chmod +x merge-nodes.sh
sudo ./merge-nodes.sh
```

`merge-nodes.sh` 会下载并校验 GitHub Release 中的 `proxyctl`。这要求项目已发布对应的 `v0.6.0`（或由 `PROXYCTL_VERSION` 指定的）Release。

`proxyctl` 校验与执行逻辑：
- 只复用固定路径 `/usr/local/bin/proxyctl`，且要求版本匹配、当前 SHA256 与同路径 `.sha256` 记录一致；
- 固定路径文件缺失或任一校验不匹配时，从 GitHub Release 下载对应架构二进制并校验 SHA256 及 `version` 输出；
- PATH 中其他同名 `proxyctl` 不会被复用或执行；
- `install-proxy.sh` 若无法安装带 `monitor` 支持且通过校验的 `proxyctl` 会失败退出，不会启用监控 timer；
- `merge-nodes.sh` 无内置渲染器回退，若无法获取校验通过的 `proxyctl` 会明确报错退出。

两个命令都会覆盖生成：

```text
/var/www/proxy-sub/<token>/clash.yaml
/var/www/proxy-sub/<token>/sr.txt
```

不需要重启 Caddy，客户端更新订阅即可。

## 节点管理（proxyctl node）

v0.3 起可用 `proxyctl` 管理 `/etc/singbox-sub-manager/nodes.conf`，无需手改文件。

```bash
# 列出所有节点（含启用状态）
proxyctl node list

# 新增节点（缺省字段在交互终端下会追问；密码不回显）
proxyctl node add --name JP-HY2 --server 1.2.3.4 --port 443 \
  --password 'xxx' --obfs-password 'yyy' --sni www.bing.com

# 修改字段（只改传入项；--name 可改名）
proxyctl node edit JP-HY2 --port 8443

# 启用 / 禁用（禁用的节点保留在文件中，但不进订阅）
proxyctl node enable JP-HY2
proxyctl node disable JP-HY2

# 删除
proxyctl node remove JP-HY2

# 生成订阅（仅包含启用节点）
proxyctl subscription build --nodes /etc/singbox-sub-manager/nodes.conf --output DIR
```

节点文件权限为 `0600`，每次修改前自动备份为 `nodes.conf.bak`。

### 从旧格式迁移

v0.2 的竖线格式（`名称|服务器|端口|密码|obfs|sni`）仍可被 `subscription build` 读取，但节点管理命令要求先迁移一次：

```bash
proxyctl node migrate
```

迁移会把文件重写为分节格式并把原文件备份到 `nodes.conf.bak`。

## 健康检查（proxyctl health）

v0.4 起提供订阅中心本机健康检查（只读），帮助及时发现服务、配置、文件或网络层面的故障。

### 检查命令

```bash
# 默认文本输出
proxyctl health

# 查看耗时等详细诊断信息
proxyctl health --verbose

# 机器可读 JSON 输出（适合监控/告警接入）
proxyctl health --json

# 显式指定域名进行检查
proxyctl health --domain sub.example.com
```

### 检查项（共 15 项）

1. **sing-box service**：systemd 服务 active 状态
2. **caddy service**：systemd 服务 active 状态
3. **UDP 443**：本地端口监听状态
4. **TCP 443**：本地 Loopback 建连验证
5. **TCP 80**：本地 Loopback 建连验证
6. **sing-box config**：配置文件合法性校验
7. **caddy config**：Caddyfile 合法性校验
8. **subscription token**：验证 token 文件与格式
9. **clash.yaml**：验证生成订阅文件可读且非空
10. **sr.txt**：验证生成订阅文件可读且非空
11. **clash subscription**：通过本机 Loopback HTTPS 访问订阅，要求 HTTP 200
12. **TLS certificate**：验证 SNI/主机名匹配及证书有效性
13. **TLS expiry**：证书过期预警（少于 14 天则 WARN）
14. **DNS**：解析当前配置域名，要求至少返回一个 IP
15. **disk space**：检查订阅根目录所在分区空间（少于 500 MB 则 WARN）

### 状态与退出码

- **HEALTHY (0)**: 全部检查 PASS
- **DEGRADED (2)**: 至少一个 WARN，无 FAIL
- **UNHEALTHY (1)**: 至少一个 FAIL
- **Error (3)**: 传参错误或内部运行异常

### 配置与只读声明

健康检查仅读取系统状态、发起本机连接并生成报告，**绝不会修改系统状态**，亦不会泄露敏感 Token 与密码信息。

检查运行所需的配置（如域名、订阅路径）按以下优先级推断：
1. 命令行传入的 `--domain`
2. `/etc/singbox-sub-manager/install.json` 状态文件
3. 从 `/etc/caddy/Caddyfile` 回退推导（兼容旧版安装）

### 远程节点探测说明

`proxyctl monitor` 每轮执行本机健康检查；首次运行及距离上次远程检查达到 30 分钟时，会对 `nodes.conf` 中启用的 Hysteria2 节点执行有界远程探测。远程探测结果失败只进入报告，不会触发本机服务重启；节点配置加载或探测执行错误返回退出码 3，但仍保留已完成的本机状态更新。

`monitor` 不接受 `--remote` 或节点路径参数；远程检查由安装配置中的 `nodes.conf` 自动加载并按 30 分钟节流。需要临时指定节点文件或只运行远程检查时，使用 `proxyctl health --remote --nodes <path>`。

### 自动监控与恢复

安装脚本会启用 `proxyctl-monitor.timer`，按 5 分钟周期执行一次 `proxyctl monitor`。只有本机触发检查连续失败达到阈值、配置校验通过且端口归属明确时，才会重启对应的 `sing-box` 或 `caddy` 服务；每次尝试进入 30 分钟冷却。

管理员控制命令：

```bash
proxyctl monitor pause
proxyctl monitor resume
proxyctl monitor status
```

状态文件位于 `/var/lib/singbox-sub-manager/monitor-state.json`，暂停标记位于 `/var/lib/singbox-sub-manager/monitor-paused`。`monitor` 输出机器可读 JSON；退出码 0 表示最终健康，1 表示恢复失败，2 表示降级但未发生失败恢复，3 表示无法可靠决策。

## 备份与恢复（proxyctl backup / restore）

`proxyctl backup` 把单机全部持久化状态打包成一个可校验的 `.tar.gz` 归档：`/etc/singbox-sub-manager/` 整棵配置树、`/etc/sing-box/config.json`、`/etc/caddy/Caddyfile`、订阅 token、monitor 状态与暂停标记。不存在的路径记入归档清单的“跳过”而不是报错。

```bash
# 创建备份（默认写入 /var/lib/singbox-sub-manager/backups/，保留最近 10 份）
sudo proxyctl backup

# 保留份数改为 3；--keep 0 表示不清理
sudo proxyctl backup --keep 3

# 备份到指定路径（不参与自动命名和保留策略）
sudo proxyctl backup --out /root/before-upgrade.tar.gz

# 列出可用归档（时间倒序，含 proxyctl 版本和大小）
sudo proxyctl backup list
```

**归档包含私钥、订阅 token 和 Hysteria2 密码。** 归档文件权限为 `0600`、备份目录为 `0700`，但一旦复制到别处，保管责任就在你自己：按与服务器同等的安全级别存放，不要放进对象存储公开桶、聊天工具或代码仓库。

保留策略只删除本工具按 `backup-YYYYMMDDTHHMMSSZ[-N].tar.gz` 命名生成的归档，你自己放进备份目录的其他文件不受影响。

### 恢复

```bash
# 先看会改动什么：不加锁、不写盘、不重启
sudo proxyctl restore /var/lib/singbox-sub-manager/backups/backup-20260909T100000Z.tar.gz --dry-run

# 实际恢复：恢复文件 → 重启受影响服务 → 健康复检
sudo proxyctl restore /var/lib/singbox-sub-manager/backups/backup-20260909T100000Z.tar.gz

# 只恢复文件，不重启任何服务（需要你自行重启才会生效）
sudo proxyctl restore /path/backup.tar.gz --no-restart
```

恢复流程是一个事务：先获取与 monitor 相同的恢复锁，再校验归档（校验和、路径安全、清单与内容一致），创建“恢复前映像”（逐字节记录每个目标文件的内容、权限和属主），然后写入。归档校验在任何写入之前完成，校验不通过时不会改动任何文件。归档中命中 `sing-box`/`caddy` 配置的路径会触发对应服务重启并复用 monitor 的健康复检口径。

复检失败时会自动回滚：按恢复前映像逐字节还原原文件的内容、权限和属主，并删除本次新建的文件，然后再次重启复检。回滚成功退出码为 4；回滚本身失败退出码为 5，此时命令会列出处于不确定状态的路径并保留映像目录供人工恢复。

monitor 正在运行并持有恢复锁时，`restore` 会在有界等待后明确报错，提示稍后重试或先执行 `proxyctl monitor pause`，不会无限等待，也不会绕过锁。

### 恢复对 monitor 的影响

`monitor-state.json` 和 `monitor-paused` 属于备份范围，恢复时按归档中的存在性精确还原：

- 归档在 monitor 暂停期间创建，恢复后 monitor 仍然是暂停的，需要执行 `proxyctl monitor resume` 才会重新生效。
- 归档中没有暂停标记，则恢复会删除当前的暂停标记。
- 状态文件按归档还原，失败计数会一并回退到备份时刻。

`restore` 不会自行启动或恢复 monitor，命令输出会明确说明本次恢复对 monitor 的影响。

### 退出码

- `0` 成功
- `1` I/O 或服务失败（系统处于已知状态）
- `2` 参数错误
- `3` 归档校验或安全预检失败（未写入任何文件）
- `4` 文件已恢复但重启/复检失败，配置已成功回滚
- `5` 回滚失败，需要人工介入

这张表只适用于 `backup` 和 `restore`。`health` 和 `monitor` 有各自的退出码含义（其中 1/2 表示健康状态，3 表示参数或内部错误），不要跨命令套用。

## 升级与回滚（install-proxy.sh update / rollback）

安装语法不变：`sudo ./install-proxy.sh <domain> [email]` 与 `sudo ./install-proxy.sh install --domain ...` 行为完全一致。`update` 和 `rollback` 是新增的子命令，不需要域名参数。

```bash
# 升级 proxyctl 到最新 Release
sudo ./install-proxy.sh update

# 回滚到最近一次升级会话（二进制 + 配置）
sudo ./install-proxy.sh rollback

# 回滚到指定的升级会话，或指定的配置归档
sudo ./install-proxy.sh rollback --to 20260910T101112Z
sudo ./install-proxy.sh rollback --to backup-20260909T100000Z.tar.gz
```

`update` 的顺序是固定的，每一步失败都不会让系统停在半生效状态：

1. 查询最新 Release tag，并探测当前 `proxyctl` 是否支持 `backup`/`restore`。
2. 支持时用**当前**二进制创建本次升级专用的配置快照；不支持（v0.6 及更早）时明确提示这是「仅二进制升级」，不会调用旧版没有的子命令，失败时也只能回滚二进制。
3. 把当前二进制和 `.sha256` 复制（不是移动）到升级会话目录，并记录各自的 SHA-256。线上文件全程保持可用。
4. 下载对应架构的二进制，用 Release 的 `checksums.txt` 校验。校验或下载失败时线上二进制和校验文件原样不动。
5. 会话元数据落盘之后，才用同目录临时文件原子替换线上的二进制（`0755`）和校验文件（`0644`）。
6. 用**新**二进制执行 `proxyctl health --json`，只有退出码 0 算成功。

健康检查失败时自动回滚：先用仍在位的新二进制把配置恢复到升级前快照（这样即使旧版本没有 `restore` 也能完成配置回滚），再校验暂存副本的 SHA-256 并原子恢复旧二进制和旧校验文件。暂存副本校验不通过时拒绝回滚并要求人工介入，不会把损坏的文件写上去。

升级会话保存在 `/var/lib/singbox-sub-manager/updates/<时间戳>/`，目录权限 `0700`，内部文件 `0600`。成功升级后保留最近一个成功会话，供之后手动 `rollback`；只有下一次成功升级才会清理更旧的成功会话。失败的会话一律保留，不自动删除。

收到 `SIGINT`/`SIGTERM` 时的行为分三种：会话元数据尚未落盘（还在建快照或暂存旧二进制）时，删除这个不完整的会话并清理下载临时文件，线上文件未被触碰；元数据已落盘后中断，保留整个会话并清理临时文件，之后可以用 `rollback` 回到升级前状态；如果中断发生在健康检查期间，新二进制已经在位、会话状态停在 `in_progress`，不会自动回滚，需要你自己执行 `sudo ./install-proxy.sh rollback`。三种情况都以退出码 130 结束并打印说明。

不完整的会话（没有 `session.meta`）不会被 `rollback` 选中，不带参数的 `rollback` 只会回到最近一个完整会话。

重跑安装脚本会把 proxyctl 拉回安装脚本内置的 `PROXYCTL_VERSION`：`update` 之后如果再执行 `sudo ./install-proxy.sh <domain>`，二进制会被降级回安装脚本固定的版本。升级后需要重跑安装时，用 `sudo PROXYCTL_VERSION=<tag> ./install-proxy.sh <domain>` 指定当前版本。

`rollback` 不带参数时使用最近一次会话：先恢复配置（如果该会话有快照），再校验并恢复二进制对。会话里只有配置快照而没有暂存二进制时，会明确告诉你「二进制未变更」。找不到任何会话或归档时报错退出，不会静默无操作。

## 获取其他节点的连接信息

在每台 Hysteria2 节点服务器上执行：

```bash
sudo cat /etc/singbox-sub-manager/config.env
curl -4 https://api.ipify.org
```

输出类似：

```text
PASSWORD="xxxxxxxx"
OBFS_PASSWORD="xxxxxxxx"
```

再记录该节点的公网 IP、端口和 SNI。

不要将真实密码提交到公开仓库。

## 文件结构

```text
singbox-sub-manager/
├── install-proxy.sh
├── merge-nodes.sh
├── README.md
├── LICENSE
├── SECURITY.md
├── .gitignore
├── Makefile
├── go.mod
├── cmd/
│   └── proxyctl/
├── internal/
├── templates/
├── examples/
│   └── nodes.conf.example
├── tests/
│   ├── test_install_proxyctl.sh
│   └── test_caddy_apt.sh
├── docs/
│   ├── aws.md
│   └── troubleshooting.md
└── .github/
    └── workflows/
        ├── ci.yml
        └── release.yml
```

## 主要文件位置

### sing-box

```text
/usr/local/bin/sing-box
/etc/sing-box/config.json
/etc/singbox-sub-manager/certs/server.crt
/etc/singbox-sub-manager/certs/server.key
/etc/systemd/system/sing-box.service
```

### Caddy

```text
/etc/caddy/Caddyfile
```

### 状态和密钥

```text
/var/lib/singbox-sub-manager/token
/etc/singbox-sub-manager/config.env
/etc/singbox-sub-manager/nodes.conf
/var/log/singbox-sub-manager/installer.log
/var/lib/singbox-sub-manager/backups/            # proxyctl backup 生成的归档（0700）
/var/lib/singbox-sub-manager/restore-snapshots/  # 恢复前映像，事务结束后自动清理
/var/lib/singbox-sub-manager/updates/            # 升级会话：旧二进制、校验文件与配置快照（0700）
```

### 订阅文件

```text
/var/www/proxy-sub/<token>/clash.yaml
/var/www/proxy-sub/<token>/sr.txt
```

## 服务管理

查看服务状态：

```bash
sudo systemctl status sing-box --no-pager
sudo systemctl status caddy --no-pager
```

重启服务：

```bash
sudo systemctl restart sing-box
sudo systemctl restart caddy
```

查看日志：

```bash
sudo journalctl -u sing-box -n 100 --no-pager
sudo journalctl -u caddy -n 100 --no-pager
```

实时查看日志：

```bash
sudo journalctl -u sing-box -f
sudo journalctl -u caddy -f
```

## 验证端口

检查 TCP 443：

```bash
sudo ss -lntp | grep ':443'
```

正常情况下 TCP 443 应由 Caddy 占用。

检查 UDP 443：

```bash
sudo ss -lnup | grep ':443'
```

正常情况下 UDP 443 应由 sing-box 占用。

如果 TCP 443 被 Xray 占用：

```bash
sudo systemctl stop xray
sudo systemctl disable xray
sudo systemctl restart caddy
```

## 验证订阅

```bash
curl -I https://sub.example.com/<token>/clash.yaml
curl -I https://sub.example.com/<token>/sr.txt
```

正常情况下应返回：

```text
HTTP/2 200
```

查看订阅内容：

```bash
curl https://sub.example.com/<token>/clash.yaml
```

## 更新

重新下载最新脚本并执行：

```bash
curl -fsSL https://raw.githubusercontent.com/hlpclg/singbox-sub-manager/main/install-proxy.sh \
  -o install-proxy.sh

chmod +x install-proxy.sh

sudo ./install-proxy.sh sub.example.com admin@example.com
```

默认会保留现有：

- 订阅 token
- Hysteria2 密码
- 混淆密码

但会重新生成：

- sing-box 配置
- Caddy 配置
- FlClash 订阅
- Shadowrocket 订阅

如果手工修改过生成后的 `clash.yaml`，重新运行脚本后会被覆盖。应当修改脚本或模板，而不是直接长期修改生成文件。

## 常见问题

### 1. Caddy 无法启动，提示 443 被占用

检查：

```bash
sudo ss -lntp | grep ':443'
```

常见原因是旧的 Xray、Nginx、Apache 或其他服务正在监听 TCP 443。

停止冲突服务：

```bash
sudo systemctl stop xray
sudo systemctl disable xray
sudo systemctl restart caddy
```

### 2. Hysteria2 无法连接

检查 UDP 443：

```bash
sudo ss -lnup | grep ':443'
```

检查云平台安全组是否开放：

```text
UDP 443
```

查看日志：

```bash
sudo journalctl -u sing-box -n 100 --no-pager
```

### 3. HTTPS 订阅无法访问

确认：

- 域名已经解析到服务器公网 IP
- TCP 80 和 TCP 443 已开放
- Caddy 服务正常
- DNS 没有错误地指向旧服务器

检查：

```bash
sudo systemctl status caddy --no-pager
sudo journalctl -u caddy -n 100 --no-pager
```

如果本机检查返回 `403`，确认 Caddy 可以遍历订阅目录：

```bash
sudo namei -l /var/www/proxy-sub/<token>/clash.yaml
sudo ls -ld /var/www /var/www/proxy-sub /var/www/proxy-sub/<token>
```

正常情况下 `/var/www` 和订阅目录应对 Caddy 服务用户可遍历；重新运行安装脚本会修正订阅目录的属主和权限。

### 4. 安装 Caddy 时出现 `NO_PUBKEY`

先确认正在运行最新版 `install-proxy.sh`。脚本会在安装 Caddy 前删除旧的纯 Caddy source，下载官方 source 与 keyring，并验证 active key `ABA1F9B8875A6661`。

若仍失败，检查实际生效的文件：

```bash
sudo cat /etc/apt/sources.list.d/caddy-stable.list
sudo gpg --show-keys --with-colons /usr/share/keyrings/caddy-stable-archive-keyring.gpg \
  | awk -F: '$1 == "fpr" {print $10}'
sudo apt-get update
```

source 应包含 `signed-by=/usr/share/keyrings/caddy-stable-archive-keyring.gpg`，输出的指纹中应包含以 `ABA1F9B8875A6661` 结尾的一项。

### 5. Google Play 在规则模式无法下载

本项目已将以下 Google Play 相关域名放在中国直连规则之前：

```text
play.googleapis.com
android.clients.google.com
android.googleapis.com
googleapis.com
gvt1.com
gvt2.com
ggpht.com
googleusercontent.com
googleusercontent.cn
googleapis.cn
android.com
google.com
```

更新订阅后，在 FlClash 中重新加载配置。

仍有问题时：

1. 清理 FlClash DNS 缓存
2. 重新启动 TUN
3. 删除旧订阅后重新添加
4. 确认当前模式为 `Rule`
5. 确认 Google Play 流量命中了 `节点选择`

### 6. 全局模式正常，规则模式异常

这通常说明：

- 节点本身正常
- 服务端正常
- 问题在规则顺序、DNS 或规则提供器下载

查看 FlClash 日志，确认目标域名最终匹配了哪一条规则。

### 7. MetaCubeX 规则下载失败

客户端需要能访问 GitHub Raw。首次加载规则时，如果当前网络无法直连 GitHub，可能下载失败。

可尝试：

- 先使用全局代理完成首次规则下载
- 重新更新订阅
- 重启客户端
- 检查系统时间是否正确

## 安全说明

公开仓库中不得提交：

- `/etc/singbox-sub-manager/config.env`
- `/var/lib/singbox-sub-manager/token`
- 真实的 `nodes.conf`
- Hysteria2 password
- obfs password
- 服务器 SSH 私钥
- 云平台 Access Key
- 真实订阅 URL

建议：

- 使用随机且足够长的密码
- 使用随机订阅 token
- 定期更新系统
- 仅开放必要端口
- 限制 SSH 来源 IP
- 禁止密码登录 SSH，改用密钥
- 不在 Issue 中粘贴完整配置和密钥

如发现安全问题，请参阅 [SECURITY.md](SECURITY.md)。

## 隐私说明

本项目不会主动收集、上传或分析用户数据。

安装脚本会访问以下公开服务：

- GitHub Releases：下载 sing-box
- Caddy 官方软件源：安装 Caddy
- Let's Encrypt：签发 HTTPS 证书
- 公网 IP 查询服务：获取服务器公网 IP
- MetaCubeX GitHub 仓库：客户端下载规则集

用户应自行审查脚本后再运行。

## 开发

Shell 脚本语法检查：

```bash
bash -n install-proxy.sh
bash -n merge-nodes.sh
```

Go 测试：

```bash
go test ./...
go vet ./...
```

构建：

```bash
make build
```

### 发布 SOP

新版本（如 v0.6.0）发布标准流程：

1. **工作区审查**：确认工作区状态并审查修改 (`git status --short` 与 `git diff`)；
2. **本地测试**：在具备工具的环境中运行全部测试 (`bash -n install-proxy.sh`, `bash -n merge-nodes.sh`, `bash tests/test_install_proxyctl.sh`, `bash tests/test_install_json.sh`, `bash tests/test_install_monitor.sh`, `go test -v ./...`)；
3. **代码提交**：获取用户明确授权后提交并推送至 `main` 分支；
4. **打 Tag 推送**：获取用户明确授权后创建并推送对应版本 Tag（例如 `git tag v0.6.0`）；
5. **CI 门禁等待**：等待 GitHub Actions 的 `caddy-apt-smoke` (Ubuntu 22.04 / Ubuntu 24.04 / Debian 12) 与 `release` 工作流全部成功通过；
6. **发布校验**：在 GitHub Release 页面确认 `proxyctl-linux-amd64`、`proxyctl-linux-arm64` 及 `checksums.txt` 3 个 Asset 已发布，且 `./dist/proxyctl-linux-amd64 version` 正确输出 Tag 名称。

## 路线图

计划支持：

- Rocky Linux 9
- AlmaLinux 9
- Amazon Linux 2023
- Reality
- TUIC
- 多协议订阅
- 独立节点配置文件
- 自动拉取远程节点
- Web 管理页面
- 节点健康检查
- 自动故障切换与恢复
- 自动更新 sing-box
- 自动备份和恢复
- Docker 部署

路线图不代表确定的发布时间。

## 贡献

欢迎提交：

- Bug report
- 文档修正
- 新系统适配
- 新协议支持
- 客户端兼容性修复
- 安全改进

提交 Pull Request 前请确保：

```bash
bash -n install-proxy.sh
bash -n merge-nodes.sh
go test ./...
go vet ./...
```

## 免责声明

本项目仅用于合法的网络连接、远程办公、隐私保护和技术研究。

使用者应遵守所在国家或地区的法律法规、云服务商条款以及网络服务条款。项目作者不对滥用、服务中断、数据损失、账号封禁或其他直接或间接损失承担责任。

## License

[MIT License](LICENSE)
