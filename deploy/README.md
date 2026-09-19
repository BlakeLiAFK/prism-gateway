# 部署示例

| 文件 | 用途 |
| --- | --- |
| `prism-gateway.service` | systemd 单元，含沙箱加固 |
| `com.prism.gateway.plist` | macOS launchd agent |

两个示例都只传 `--db`。**监听地址、日志级别与格式、并发、预算等运行设置一律在管理后台修改，保存即生效**，不要写进服务单元——写进去就需要改文件加重启才能调整。

只有启动期的安全边界需要写在这里：`--allow-remote`、`--tls-cert`、`--tls-key`。

## 日志

两个示例都把日志交给系统（journald / 文件）。级别与格式在后台「系统设置」里改：接日志采集器时切 `json`，临时排障切 `debug`（用完调回 `info`，详情可能回显刚提交的配置片段）。

写文件时请配合 `logrotate` 或 `newsyslog`，本程序不自带轮转。

## 备份

管理后台或 `backup.create` 会用 SQLite 的 `VACUUM INTO` 生成一致性快照，不需要停服务。

快照**不含** `.key` 主密钥。恢复上游凭证必须配套原主密钥，请把 `.key` 单独备份到另一处。

## Docker

见仓库根目录 `Dockerfile`。镜像固定绑 `0.0.0.0:8080` 并带 `--allow-remote`，数据卷挂到 `/data`。
