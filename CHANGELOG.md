# Changelog

## 1.1.0 · 2026-09-19

- 新增免鉴权 `GET /healthz` 存活探针，执行一次数据库查询，异常返回 503；
  只暴露进程级信息，支持 GET/HEAD。
- 新增 `config.export` / `config.import` 管理 action：无凭证的配置快照迁移，
  导入按 provider id 保留本机已有加密凭证，走版本校验、Validate 与审计。
- SQLite 绑定拒绝多语句 SQL，不再静默丢弃 prepare 后的剩余语句。
- 补充 healthz、导出导入往返、凭证保留与多语句拒绝的自动化测试；
  冒烟脚本增加两组检查。

## 1.0.0 · 2026-09-19

Initial executable source delivery: SQLite configuration, encrypted upstream
credentials, immutable config snapshots, single management RPC, embedded Chinese
WebUI, three protocol surfaces, native proxy and strict subset translation,
SSE tool streaming, local budgets/RPM/concurrency, session affinity, safe fallback,
usage/audit metadata, model discovery jobs and explicitly labeled local Sandbox.

Includes offline-capable build scripts, automated protocol and HTTP integration
tests, deployment notes, client examples and compatibility boundaries.
