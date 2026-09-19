# 安全、部署与运维

## 信任模型

面向个人和受信任内部管理员。它不是经过外部安全审计的公共托管平台，也不实现租户隔离、SSO、多管理员细粒度 RBAC、商业账单或抗 DDoS。默认仅监听 127.0.0.1。

管理员拥有添加供应商、读取记录、创建调用 Key、修改预算的权力。调用 Key 只能访问模型端点，不能获取配置或管理凭证。完整调用 Key 只在创建时返回，数据库保存 SHA-256 校验值；管理员令牌也是高熵随机串，不是短口令。

供应商凭证使用 AES-GCM 加密后写 SQLite，主密钥为同目录的 32-byte `.key`。这保护单独泄漏的配置/数据库凭证，但**不保护整台主机被控制**。数据库、主密钥、备份、终端首次启动输出及容器日志都属于敏感资产。

## HTTP 防护

管理接口仅接受 POST JSON；会话 Cookie HttpOnly、SameSite=Strict，Cookie 鉴权写请求要求 CSRF token。Origin 与请求主机不匹配时拒绝。Bearer 管理令牌用于受控 CLI，不下发到页面 localStorage。

页面启用 CSP、X-Content-Type-Options、frame-ancestors/X-Frame-Options，同源资源，不引用 CDN。动态字符串经 HTML escape。单请求 JSON、上游普通响应与 SSE frame 设有大小限制；这不等于已经完成恶意流量压力审计。

默认不转发供应商错误响应正文，防止意外泄漏上游敏感内容。请求日志不保存 prompt/回答；外部反向代理、客户端日志及供应商仍可能记录它们。

## 健康检查端点

`GET /healthz` 是唯一免鉴权端点。它只返回 `ok`、`version`、`uptime_ms`，不返回配置、供应商、用量或运行时细节，并且是只读的。

即便如此，它仍会向匿名访问者暴露「此处运行着 Prism Gateway 及其版本号」。默认回环监听下没有影响；放到公网反向代理后时，建议只对探活来源放行该路径，或在代理层重写。

它执行一次数据库查询后再返回：数据库不可用时返回 `503`，所以可以直接用作容器与负载均衡的存活探针。

## 配置导出的边界

`config.export` 产出的 JSON **不含**上游 API Key、客户端 `prism_sk_...` 或管理员令牌，可以安全进入私有配置仓库。但它仍然是内部拓扑信息：供应商名称、Base URL、模型清单与预算阈值都在里面。不要公开发布。

`config.import` 需要管理员权限，按 provider id 保留本机已有的加密凭证，导入文件无法携带也无法伪造凭证或 `has_key`。

## SSRF 和网络

供应商 Base URL 不允许内嵌账号密码、query 或 fragment。默认只接受 HTTPS 并拒绝私有地址；拨号时再次检查 DNS 解析地址，禁用自动 HTTP redirect，防止把凭证跟随重定向发走。不会自动使用环境中的通用 HTTP 代理。

连接 Ollama / vLLM 等本地服务时，管理员必须显式开启“允许私有网络 / 明文 HTTP”。这个开关是实际扩大访问范围，不是装饰。不要为陌生上游随意开启。

## TLS 与远程使用

最简单的远程方式是 SSH 隧道，保持服务回环监听：

```bash
ssh -L 8080:127.0.0.1:8080 user@your-server
```

直接使用 TLS：

```bash
./prism-gateway --listen 0.0.0.0:8443 --allow-remote \
  --tls-cert /secure/server.crt --tls-key /secure/server.key
```

使用反向代理时保持它到网关的链路受信任，透传原始 Host，关闭流式缓冲，并设置足够的 upstream timeout。网关不会盲信外部 X-Forwarded-Proto 来设置 Secure Cookie；本机直接 TLS 会设置 Secure。HTTPS 代理场景要由代理正确限制后端访问，并可在代理层追加 Cookie Secure 属性。不要把未加密后端端口暴露到公网。

## Docker（可选、未在本交付环境执行镜像构建）

```bash
docker build -t prism-gateway:local .
docker volume create prism-data
docker run --rm --name prism-gateway \
  -p 127.0.0.1:8080:8080 \
  -v prism-data:/data \
  prism-gateway:local
```

容器内监听 0.0.0.0，但上述宿主机端口仅绑定回环。镜像非 root UID 10001；用 bind mount 时须给该 UID 数据目录读写权。首次管理员令牌会写入容器日志，日志访问权限要保护。Docker 构建需要网络、基础镜像与系统包；代码本身没有 npm 或 Go module 下载。

## 数据一致性与备份

一个 SQLite 文件只启动一个网关进程。不要将 SQLite 放进 NFS/SMB 供多机器并发写入；不支持多实例高可用。配置热更新无需重启，监听地址与证书等启动项需重启。

备份步骤：停止进程；确认已退出；复制完整 data 目录；保护备份文件权限；再启动。恢复时保持数据库和 `.key` 成对。升级前备份，旧二进制对未知 schema 会拒绝启动而不是悄悄误读。

默认生成请求元数据保留 90 天，至少 31 天，以覆盖本地 30 天预算。清理由运行进程定时触发。管理员审计、Key 元数据和 job 表没有无限量自动归档策略；长期使用时应结合实际容量规划维护，不直接改活跃数据表。

撤销调用 Key 影响新请求，不强制杀死已在执行的请求。`session.delete` 只解除本地亲和绑定。没有“重置供应商额度”接口。

## 未知用量和恢复

网络异常、客户端取消、缺少上游 usage、进程中断都可能使实际消费不可确认。账务保留请求预留，标为 unknown/reserved，不退成免费。金额基于手动单价而非账单查询；运维应按供应商账单核对。本地预算不是严格的资金托管或原子余额系统。

RPM / cooldown 在内存，重启后重新开始；持久预算不清空。官方限制始终以供应商响应为准。本地计数接口不是生成请求账本的一部分，元数据诊断操作也不计入“模型生成请求”图表。
