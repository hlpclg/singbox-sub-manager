# AWS EC2 部署

安全组入站规则至少需要：

- TCP 22：仅允许你的管理 IP
- TCP 80：0.0.0.0/0，用于 ACME 验证
- TCP 443：0.0.0.0/0，用于 HTTPS 订阅
- UDP 443：0.0.0.0/0，用于 Hysteria2

部署前确认域名 A 记录已指向 EC2 弹性 IP，并检查 443 端口占用：

```bash
sudo ss -lntup | grep ':443'
```
