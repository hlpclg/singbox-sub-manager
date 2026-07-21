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
- 自动启用 BBR
- 自动配置适合 macOS FlClash TUN 的规则
- 优先代理 Google Play、Google、OpenAI、Claude、GitHub、YouTube 等服务
- 国内域名、私有网络和 Apple 中国服务默认直连
- Caddy 禁用 HTTP/3，避免与 Hysteria2 的 UDP 443 冲突

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
/etc/proxy-sub-token
```

重复运行安装脚本时，订阅地址默认不会改变。

Hysteria2 密钥保存于：

```text
/etc/proxy-state/hy2-secret.env
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
7. 在 `节点选择` 中选择 `自动选择` 或指定节点

配置默认包含：

- TUN mixed stack
- Fake-IP DNS
- 自动路由
- DNS 劫持
- 国内直连
- 国外代理
- Google Play 下载修正规则
- OpenAI、Claude、Google、GitHub 等优先代理规则

## Shadowrocket 使用方法

1. 打开 Shadowrocket
2. 添加订阅
3. 粘贴脚本输出的 `sr.txt` 地址
4. 更新订阅
5. 选择节点并连接

`sr.txt` 每行包含一个 Hysteria2 节点 URI。

## 合并多个节点

在订阅中心服务器下载 `merge-nodes.sh`：

```bash
curl -fsSL https://raw.githubusercontent.com/hlpclg/singbox-sub-manager/main/merge-nodes.sh \
  -o merge-nodes.sh

chmod +x merge-nodes.sh
```

编辑节点列表：

```bash
nano merge-nodes.sh
```

格式：

```bash
NODES=(
  "JP-HY2|1.1.1.1|443|PASSWORD|OBFS_PASSWORD|www.bing.com"
  "SG-HY2|2.2.2.2|443|PASSWORD|OBFS_PASSWORD|www.bing.com"
  "US-HY2|3.3.3.3|443|PASSWORD|OBFS_PASSWORD|www.bing.com"
)
```

字段说明：

```text
节点名 | 服务器IP或域名 | 端口 | Hysteria2密码 | 混淆密码 | SNI
```

然后运行：

```bash
sudo bash merge-nodes.sh
```

脚本会覆盖生成：

```text
/var/www/proxy-sub/<token>/clash.yaml
/var/www/proxy-sub/<token>/sr.txt
```

不需要重启 Caddy，客户端更新订阅即可。

## 获取其他节点的连接信息

在每台 Hysteria2 节点服务器上执行：

```bash
sudo cat /etc/proxy-state/hy2-secret.env
curl -4 ifconfig.me
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
├── docs/
│   ├── aws.md
│   └── troubleshooting.md
└── .github/
    └── workflows/
        └── ci.yml
```

## 主要文件位置

### sing-box

```text
/usr/local/bin/sing-box
/etc/sing-box/config.json
/etc/sing-box/cert/server.crt
/etc/sing-box/cert/server.key
/etc/systemd/system/sing-box.service
```

### Caddy

```text
/etc/caddy/Caddyfile
```

### 状态和密钥

```text
/etc/proxy-sub-token
/etc/proxy-state/hy2-secret.env
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

### 4. Google Play 在规则模式无法下载

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

### 5. 全局模式正常，规则模式异常

这通常说明：

- 节点本身正常
- 服务端正常
- 问题在规则顺序、DNS 或规则提供器下载

查看 FlClash 日志，确认目标域名最终匹配了哪一条规则。

### 6. MetaCubeX 规则下载失败

客户端需要能访问 GitHub Raw。首次加载规则时，如果当前网络无法直连 GitHub，可能下载失败。

可尝试：

- 先使用全局代理完成首次规则下载
- 重新更新订阅
- 重启客户端
- 检查系统时间是否正确

## 安全说明

公开仓库中不得提交：

- `/etc/proxy-state/hy2-secret.env`
- `/etc/proxy-sub-token`
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
- 自动故障切换
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
