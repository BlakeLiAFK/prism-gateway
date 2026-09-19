# 实现架构

```text
                        PRISM GATEWAY / SINGLE GO BINARY

 Browser / CLI                              Coding Clients
       |                                          |
       | GET /                                    |
       v                                          v
  embed.FS WebUI                  /openai/v1/*   /anthropic/v1/*
       |                                          |
       | POST /api.json                           v
       v                              API Key / Scope Validation
  Authentication / CSRF                           |
       |                                          v
  Action Dispatcher                     Fixed Model / Route / Alias
       |                                          |
       v                                          v
  Config / Keys / Jobs / Usage           Capability / Native Preference
       |                                          |
       |                              Session Affinity / Budget / RPM
       |                                          |
       v                                          v
  SQLite Transaction                        In-flight Reservation
       |                                          |
       v                                          v
  Atomic Config Snapshot -------> Native Forward / Explicit Conversion
       |                                          |
       +------------------------------------------+
                                                  |
                                                  v
                                            Provider HTTP
                                                  |
                                                  v
                                           SSE / JSON Output
                                                  |
                                                  v
                                         Usage + Attempt Records
```

## 目录

```text
cmd/gateway/                 启动参数、进程锁、信号与优雅退出
internal/sqlite/             仓库内 C API 薄绑定，串行连接、事务
internal/gateway/
  types.go                   配置结构、校验、错误类型
  store.go                   schema 初始化、配置持久化、快照、密钥加密
  security.go                URL / SSRF 边界、HTTP transport、上游鉴权
  app.go                     /api.json、登录、CRUD、模型同步任务、统计
  engine.go                  调度、配额预留、速率、转发、完成记账
  protocol.go                公共子集 IR、JSON 请求/响应转换
  stream.go                  SSE 解析、原生流转发、三协议事件编码
  gateway_test.go            集成、协议、鉴权、预算与取消测试
internal/webui/
  embed.go                   go:embed dist 与静态 / SPA fallback
  dist/index.html            应用入口
  dist/assets/api.js         唯一 RPC transport
  dist/assets/app.js         页面、编辑器与交互
  dist/assets/app.css        主题、布局、抽屉、响应式与动效
  dist/assets/icons.js       本地 SVG 图标
scripts/                     环境检查、构建与可选 HTTP smoke
examples/                    curl / CLI 示例
```

## 配置更新

管理服务读取 expected version → 持锁克隆当前配置 → 校验引用与参数 → SQLite 短事务写入、递增版本、审计 → 原子发布新快照。失败不发布。配置请求不直接操作路由器内部数据；前端也不直接操作数据库。

配置表之间存在实体 ID / 外键关系。Provider、Model、Route 的类型化属性以每实体 JSON body 保存，不是单个巨大配置 blob。route_models 有显式关联，便于维护引用。未来改变实体结构须扩展 migration 与测试，不绕过 config_version。

每个模型请求取得快照，候选配置在该请求内稳定；瞬时并发与预算仍在 admission 时重新检查。全局并发属于即时保护设置，不保证和在途旧配置一同冻结。

## SQLite 与运行状态

SQLite 采用 WAL、FULL 同步、foreign_keys、busy timeout。绑定有进程内互斥，一个连接负责短查询和事务。数据库文件另有 OS 级进程锁，避免两个进程的内存速率状态互相不知情。数据库不放在网络共享盘，不做多实例共享写入。

配置、在途保留、请求记录、会话绑定持久化；HTTP 连接池、active count、RPM 时间戳与冷却在内存。重启将未完成请求标为 unknown，并保留先前预算，避免把意外终止当免费。sync jobs 标为 interrupted，不自动重放外部操作。

request_logs 不存请求和响应正文，功能名称与 SQL 表名以代码实际实现为准：本版本请求表名为 requests，不存在额外影子数据库。

## 前端

采用原生 ES Modules + CSS 变量 + native dialog + Web Animations，不依赖前端框架编译链。已经提供浏览器可执行源码，Go 直接 embed 同目录 dist。这样使用者只需 Go/C 工具链，不需重建 npm 依赖。

Cmd/Ctrl+K 命令面板、抽屉编辑、拖拽/键盘候选排序、主题切换、实时刷新均走同一 /api.json。后台不会把模型调用 API 与管理请求混成一个 endpoint。主题偏好可存浏览器 localStorage，业务配置只存 SQLite；完整供应商 Key、管理员令牌不存 localStorage。

接口是服务层边界，不允许用后台 SQL 编辑器、CLI 写数据库等方式绕过版本检查。
