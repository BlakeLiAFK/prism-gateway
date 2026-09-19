# Changelog

## 1.3.0 · 2026-09-19

- 监听地址移入 SQLite，可在管理后台修改并立即生效。切换时先占用新地址，
  占不住则配置与服务都保持原样。
- `--allow-remote` 与 `--tls-*` 仍是启动期安全边界：后台无法把监听改到非回环地址。
- `--listen` 降级为救援覆盖，只影响本次启动，不写回配置；救援期间保存其它设置
  不会把服务拽回配置里的地址。
- 移除 `--demo`：WebUI 总览的「启用演示」是等价入口。
- 新增 scripts/ui_smoke.py：真实启动二进制 + 浏览器走完 11 个页面、命令面板、
  主题切换、编辑抽屉、设置往返与 390px 响应式，断言零 console 错误；纳入 make check。
- 前端源码恢复常规换行：app.js 由 195 行/最长 3996 字符变为 868 行/中位 12 字符。

## 1.2.0 · 2026-09-19

- 引入 log/slog 结构化日志。日志级别与格式存在 SQLite，管理后台随时切换，
  立即生效，不新增任何启动参数，也不需要重启进程。
- 管理接口的未预期错误默认只记 request_id 与 action；错误详情可能包含刚提交的
  配置片段，降到 DEBUG 级别，按需在后台打开。
- 启动横幅仍走标准输出：它是终端界面，不是日志，管理员令牌不会进入日志采集链路。
- 抽出 checkListen / checkTLS 纯函数并补测试，cmd/gateway 覆盖率由 0% 升至 21.6%；
  新增进程锁冲突测试。
- 前端版本号改为由 auth.status 下发，不再硬编码。
- 新增 make hooks 安装 pre-push 质量门，替代持续集成。

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
