# Changelog

## 1.6.1 · 2026-09-19

- 修复流式的推理丢弃声明加错了位置：`text/event-stream` 在文件里出现两次，
  声明被加到了演示模式分支，真实上游的转换流并没有带上
  `X-Prism-Dropped`。已补测试锁住该路径。

## 1.6.0 · 2026-09-19

- 模型新增 `drop_reasoning` 开关，默认关闭。上游返回推理内容而又需要跨协议调用时
  （实测 OpenRouter 上的免费模型几乎都会返回 reasoning），由管理员对具体模型显式
  开启，跨协议转换会丢弃推理内容并在响应头 `X-Prism-Dropped: reasoning` 标注。
  关闭时行为不变：明确拒绝，而不是悄悄截断。
  开关只覆盖推理内容，refusal、annotations 等仍照旧拒绝。
- 非流式响应只在确实丢弃时标注；流式无法等到发现推理内容再补响应头，
  因此开关开启且跨协议时先行声明。
- WebUI 模型编辑器加入该开关，界面冒烟覆盖其默认状态。

## 1.5.2 · 2026-09-19

- 修复反向代理后会话 Cookie 丢失 Secure 标志。判定只看 r.TLS，而反代部署下
  TLS 在代理侧终止，结果恰恰是 HTTPS 部署拿不到 Secure 保护。现在在请求来自
  回环地址时采信 X-Forwarded-Proto；非回环来源的该头一律不信，因为公网客户端
  可以随意伪造。

## 1.5.1 · 2026-09-19

- 跨协议转换失败不再报成「上游请求失败（HTTP 502）」。真实上游（OpenRouter 上的
  DeepSeek、Nemotron 等）会在 OpenAI 响应里附带 reasoning 与 reasoning_details，
  转成 Anthropic 格式需要 thinking 签名，凭空构造等于伪造推理，因此必须拒绝——
  但原先的错误信息把人引向排查上游，而上游根本没出问题（记录里 input_tokens、
  output_tokens、duration_ms 俱全）。现在返回 UPSTREAM_INCOMPATIBLE，
  说明是响应内容无法跨协议转换，并提示改用该模型的原生协议。
- 网关自己构造的错误不再被笼统的「上游请求失败」覆盖；上游错误正文仍不转发。

## 1.5.0 · 2026-09-19

- 前端拆分为 core / ui / views / app 四个 ES Module，仍然没有构建步骤。
  core 只有纯函数与常量、零业务依赖，因此先于其它模块求值，
  其余模块之间的函数级循环引用是安全的。
  单文件 868 行 → 四个文件 37 / 220 / 217 / 403 行。
- 跨模块共享的可变全局收进 state 对象：ESM 的 live binding 只允许跨模块读，
  不允许赋值，原来的顶层 let 无法拆分。
- 修复登录失败处理吞掉真实异常：enterApp 抛错时登录页已被替换，
  原写法在 catch 里二次抛错，把根本原因盖掉了。
- WebUI 冒烟脚本在断言失败时输出已捕获的页面错误。
- 进程冒烟与内嵌资源测试逐个校验四个模块可服务——入口能加载但某个被 import
  的模块 404 时，页面会静默空白。

## 1.4.0 · 2026-09-19

修复

- 会话亲和记录从未写入：upsert 的冲突分支误用了另一张表的名字
  （requests.requests），整条语句编译失败，而 Exec 的错误被丢弃，
  因此该功能自 1.0.0 起静默失效。同时给会话、审计、保留清理、
  Key 使用时间等后台写入补上错误日志，杜绝同类静默失败。
- 保留清理只在 ticker 触发，运行不满一小时就重启的实例永远不会清理。
  改为启动时先执行一次。

新增

- GET /metrics 导出 Prometheus 文本，默认关闭，开启后仍需管理员令牌。
  只含聚合计数，不含 prompt、密钥或会话内容。
- backup.create / backup.list：用 SQLite VACUUM INTO 生成一致性快照，
  不需要停服务。快照不含 .key，恢复凭证仍需配套主密钥。
- deploy/ 提供 systemd 单元（含沙箱加固）与 launchd agent 示例。
- CONTRIBUTING.md 与 SECURITY.md。

测试与工程化

- 测试函数 33 → 53，新增 3 个模糊测试目标（协议解码、补全解码、SSE 解析），
  各跑 160 万次执行无 crash。
- 新增并发压力测试：验证全局并发上限、计数器完全释放、RPM 限制。
- 覆盖率 internal/gateway 67.1% → 80.0%、sqlite 63.2% → 77.8%、
  webui 0% → 63.6%、cmd 13.1% → 19.0%。
- scripts/cover.sh 覆盖率门禁，低于阈值即失败，已纳入 make check。
- staticcheck 接入 make lint（未安装则跳过），修掉它发现的无效赋值。
- make release 产出可复现构建与 SHA256 校验和。
- pre-commit 钩子检查 gofmt。
- Docker 镜像完成实机构建与运行验证：三协议、SSE、重启持久化全部通过。

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
