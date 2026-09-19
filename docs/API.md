# 管理 RPC 与模型接口

## 单一管理入口

```http
POST /api.json
Content-Type: application/json
```

```json
{"action":"provider.list","params":{}}
```

成功：`{"ok":true,"data":...,"request_id":"rpc_..."}`。
失败：`{"ok":false,"error":{"code":"...","message":"..."},"request_id":"rpc_..."}`，同时使用相应 HTTP 状态码。未知 action 返回 400；未授权 401；权限/CSRF 403；找不到对象 404；版本冲突 409。不是“所有错误都返回 HTTP 200”。

最多 1 MiB 管理 JSON，请求必须是一个 JSON 对象，不接受尾随第二个对象。所有 action 均经统一分发器。版本 1 没有通用写事务、嵌套 batch 或任意 SQL 接口。

### 管理鉴权

浏览器先调用 `auth.login`，`params.token` 是终端显示的管理员令牌。成功返回 CSRF 值并设置 HttpOnly、SameSite=Strict 的管理会话 Cookie。后续传 `X-Prism-CSRF`。`auth.status` 用于恢复当前会话，`auth.logout` 注销。

命令行可直接传 `Authorization: Bearer <prism_admin_...>`。模型调用 Key 没有管理权限。错误的 Origin / 跨站 Fetch 元信息被拒绝；默认不开放 CORS。

```bash
export PRISM_ADMIN_TOKEN='替换为终端输出的管理员令牌'
curl http://127.0.0.1:8080/api.json \
  -H "Authorization: Bearer $PRISM_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"action":"config.get","params":{}}'
```

不要让浏览器直接调用供应商，不要把上游 Key 注入前端源码。

## 配置版本

`config.get` 返回完整非敏感配置与整数 `version`。配置写入必须携带刚读取的 `params.version`。写入成功返回新 Config，版本加一；旧版本写入 409，不覆盖别人的修改。

```json
{
  "action":"provider.save",
  "params":{
    "version":1,
    "id":"my-provider",
    "provider":{
      "name":"OpenCode Go",
      "kind":"opencode",
      "base_url":"https://opencode.ai/zen/go/v1",
      "auth":"auto",
      "api_key":"填入真实上游 Key",
      "enabled":true,
      "allow_private":false,
      "timeout_sec":300
    }
  }
}
```

新增 Provider 可不传 id，由服务端生成。更新时省略 `api_key` 保持原值，显式空字符串清除。返回只包含 `has_key`，没有真实凭证。

## Action 清单

| Action | params | data |
| --- | --- | --- |
| `auth.status` | `{}` | 登录状态，已登录时包含 csrf |
| `auth.login` | `{token}` | 管理会话状态 / csrf |
| `auth.logout` | `{}` | 注销结果 |
| `config.get` | `{}` | 完整非敏感 Config |
| `system.info` | `{}` | 网关、Go、SQLite、运行时长 |
| `dashboard.get` | `{range:"24h"\|"7d"\|"30d"}` | 概览、24 个时间桶、top_models、recent、quotas、runtime |
| `usage.summary` / `usage.timeseries` | 同 dashboard | 本版本返回同一聚合结构 |
| `provider.list` | `{}` | Provider[] |
| `provider.save` | `{version,id?,provider}` | 新 Config |
| `provider.delete` | `{version,id}` | 新 Config；存在引用则拒绝 |
| `provider.test` | `{id}` | GET /models HTTP 状态与耗时，不调用模型生成 |
| `provider.sync_models` | `{id}` | `job_id`，异步执行 |
| `model.list` | `{}` | Model[] |
| `model.save` | `{version,id?,model}` | 新 Config；model.id 是对外名称 |
| `model.delete` | `{version,id}` | 新 Config；检查路由与别名引用 |
| `route.list` | `{}` | Route[] |
| `route.save` | `{version,id?,route}` | 新 Config |
| `route.delete` | `{version,id}` | 新 Config |
| `route.test` | `{id,protocol?,request?}` | ranked / checks / note；默认模拟 Chat 文本 |
| `alias.list` | `{}` | Alias[] |
| `alias.save` | `{version,alias:{id,target,enabled}}` | 新 Config |
| `alias.delete` | `{version,id}` | 新 Config |
| `settings.get` | `{}` | Settings |
| `settings.update` | `{version,settings}` | 新 Config |
| `quota.list` | `{}` | 每模型本地窗口预算 / 已用及在途预留 |
| `apikey.list` | `{}` | Key 元数据，不返回 hash 或完整密钥 |
| `apikey.create` | `{name,allowed:[]}` | id / key / warning；key 仅创建时返回 |
| `apikey.revoke` | `{id}` | 撤销结果 |
| `session.list` | `{}` | 最近 200 个本地亲和映射 |
| `session.delete` | `{id}` | 解除本地绑定，不重置上游状态 |
| `request.list` | `{page?,page_size?,q?,status?,provider_id?}` | items / page / page_size / total |
| `request.get` | `{id}` | 单次上游尝试元数据 |
| `job.list` | `{}` | 最近 100 个模型同步任务 |
| `job.get` | `{id}` | 任务状态、结果或错误 |
| `audit.list` | `{}` | 最近 200 个审计事件 |
| `playground.run` | `{protocol,model,prompt,session?}` | status / duration_ms / headers / response |
| `demo.enable` | `{version}` | 新 Config，添加本地演示对象 |
| `batch.read` | `{requests:[{action,params},...]}` | 每项独立 ok/data/error 数组 |

`batch.read` 只接受实现中列出的只读 action，1–10 项，不保证多个查询处于同一个数据库读快照，不接收任何写操作。同步模型任务网络调用在 SQLite 事务外完成；提交时版本冲突会失败，不自动覆盖同时发生的编辑。

## 模型与路由结构

```json
{
  "action":"model.save",
  "params":{
    "version":2,
    "model":{
      "id":"my-coder",
      "name":"My coding model",
      "provider_id":"my-provider",
      "upstream":"供应商实际模型ID",
      "protocol":"messages",
      "enabled":true,
      "tools":true,
      "vision":false,
      "native_count":false,
      "context_window":128000,
      "max_output_tokens":4096,
      "concurrency":2,
      "rpm":0,
      "pricing_set":false,
      "input_price":0,
      "output_price":0,
      "cache_price":0,
      "write_price":0,
      "limit_5h":0,
      "limit_7d":0,
      "limit_30d":0
    }
  }
}
```

价格是 USD / 百万 token。限额是 USD，0 表示不添加该项本地限制。上例能力和容量是结构示例，不是任何型号的性能承诺。`pricing_set=false` 表示未知，不能据此设置非零美元限额。没有 `quota.reset` 上游重置功能。

```json
{
  "action":"route.save",
  "params":{
    "version":3,
    "route":{
      "id":"auto-coding",
      "name":"Coding pool",
      "strategy":"priority",
      "enabled":true,
      "affinity":true,
      "candidates":[{"model_id":"my-coder","weight":10}],
      "description":"My internal coding route"
    }
  }
}
```

更新已有模型/路由可在 params.id 指定对象，并只提交变化字段。数组替换整体列表，不做隐藏的逐项 merge。模型、路由、别名对外 ID 共用一个命名空间。

## 运行设置

`settings.get` / `settings.update` 管理运行设置。全部字段保存即生效，不需要重启进程：

| 字段 | 取值 | 说明 |
| --- | --- | --- |
| `app_name` | 文本 | 后台识别名称 |
| `default_route` | 文本 | 配置指引用；调用仍须显式传 model |
| `global_concurrency` | 1–512 | 超出返回 429，不排队 |
| `max_body_mb` | 1–32 | 请求体上限 |
| `retention_days` | 31–3650 | 请求记录保留天数 |
| `session_ttl_hours` | 1–720 | 会话亲和映射保留时长 |
| `allow_estimated_count` | 布尔 | 原生计数不可用时是否返回本地估算 |
| `listen` | `host:port` | 监听地址，保存后立即切换 |
| `metrics_enabled` | 布尔 | 是否开放 `/metrics`，默认关闭 |
| `log_level` | `debug` / `info` / `warn` / `error` | 默认 `info` |
| `log_format` | `text` / `json` | 默认 `text`；接采集器时用 `json` |

```json
{"action":"settings.update","params":{"version":7,"settings":{"log_level":"debug","log_format":"json"}}}
```

`log_level=debug` 会额外输出管理接口的错误详情。**这些详情可能包含管理员刚提交的配置片段**（例如误填到 Base URL 里的 Key），排障结束后请调回 `info`。默认级别只记录 `request_id` 与 `action`。

启动横幅、管理员令牌和 TLS 警告写终端标准输出，不经过 slog，因此不会进入日志采集链路。

### 监听地址切换

修改 `listen` 时服务端先 `bind` 新地址，成功后才写配置并接管；占不住就返回 400，**配置与正在服务的监听都保持原样**，已建立的连接不受影响。

非回环地址要求进程以 `--allow-remote` 启动，否则返回 `REMOTE_NOT_ALLOWED`。这是启动期的安全边界：拿到管理令牌不等于可以把网关暴露到公网。

`--listen` 是救援覆盖，只影响本次启动、不写回配置。救援期间保存其它设置不会把服务拽回配置里那个地址——切换与否以 `listen` 字段本身是否变化为准。

## 配置导出与导入

`config.export` 返回可直接保存为文件的业务配置快照：

```json
{
  "format": "prism.config/1",
  "gateway_version": "1.1.0",
  "exported_at": 1758240000000,
  "config": {"version": 7, "providers": [], "models": [], "routes": [], "aliases": [], "settings": {}}
}
```

**导出文件不含任何凭证。** 上游 API Key、客户端 `prism_sk_...`、管理员令牌都不在其中，可以安全提交到私有配置仓库或随工单传递。它也不含请求记录、用量与审计日志。

`config.import` 用快照整体替换供应商、模型、路由、别名和设置：

```json
{"action":"config.import","params":{"version":7,"format":"prism.config/1","config":{}}}
```

- `params.version` 必须是当前配置版本，冲突返回 409，与其他写入一致。
- 按 provider `id` 保留库中已有的加密凭证；文件无法携带也无法伪造凭证。
- `has_key` 由库中实际凭证决定，导入文件里的声明被忽略。
- 返回 `providers_missing_key`，列出需要重新填写 Key 的供应商 id。
- 客户端 Key、管理员令牌、请求记录、用量与会话不在替换范围内。
- 整体走一次事务与 `Validate`；任一项不合法则全部不生效。

这是配置迁移与版本化手段，不是数据库备份。完整备份仍是停进程后复制整个 `data` 目录（含 `.key`）。

## 备份

`backup.create` 用 SQLite 的 `VACUUM INTO` 生成一致性快照，不需要停服务：

```json
{"ok":true,"data":{"path":"/data/backups/gateway-20260919-104414-257.db","bytes":122880,
 "note":"快照不含 .key 主密钥；恢复上游凭证必须配套原主密钥"}}
```

路径由服务端决定（数据库同目录的 `backups/`），不接受客户端指定，避免路径穿越。同名文件不会被覆盖。`backup.list` 列出已有快照。

快照里的上游凭证仍是密文。把它交给新实例时必须同时提供原来的 `.key`，否则启动会因无法解密而失败——这是设计行为，不是故障。

## 指标

```http
GET /metrics
Authorization: Bearer <管理员令牌>
```

Prometheus 文本格式。**默认关闭**，需在「系统设置」里开启（关闭时返回 404，不暴露端点存在）。开启后仍要求管理员令牌：指标含模型名、调用量与费用估算，属于内部经营信息。

导出的指标：`prism_build_info`、`prism_uptime_seconds`、`prism_config_version`、`prism_active_requests`、`prism_requests_total`、`prism_request_duration_ms_total`、`prism_tokens_total`、`prism_cost_nano_total`。后四项按 `model` / `protocol` / `status` 分标签，统计窗口为近 30 天。

不含 prompt、回答、密钥或会话内容。`prism_cost_nano_total` 是本地估算，不是供应商账单。

## 健康检查

```http
GET /healthz
```

```json
{"ok":true,"version":"1.1.0","uptime_ms":10523}
```

唯一免鉴权端点，供反向代理与容器编排探活。它执行一次数据库查询：数据库不可用时返回 `503` 且 `ok:false`。只暴露进程级信息，不返回配置、用量或运行时细节。支持 `GET` 与 `HEAD`，其余方法返回 `405`。

## 标准模型端点

```text
POST /openai/v1/chat/completions
POST /openai/v1/responses
GET  /openai/v1/models
GET  /openai/v1/models/{id}

POST /anthropic/v1/messages
POST /anthropic/v1/messages/count_tokens
GET  /anthropic/v1/models
GET  /anthropic/v1/models/{id}
```

凭证：`Authorization: Bearer prism_sk_...` 或 `x-api-key: prism_sk_...`。不要将它们换成管理员令牌或上游令牌。catalog 包括被授权的已启用模型、路由和别名；使用网关生成的目录元数据，不代表上游真实模型发布日期。能力兼容仍在请求时验证。

诊断头：`X-Request-ID`、`X-Prism-Config-Version`、`X-Prism-Model`、`X-Prism-Provider`、`X-Prism-Upstream-Model`、`X-Prism-Protocol-Mode`、`X-Prism-Session`；演示附 `X-Prism-Demo: true`。

`X-Prism-Token-Count-Mode` 为 `provider` 或 `estimated`，不是 `exact`。原生计数失败不静默换成本地估算。

## 请求记录

一个客户端请求可能因上游 429/503产生多个 attempt。`parent_id` 是客户端 X-Request-ID，`id` 是单次 attempt。Dashboard 的请求数按 attempt 统计，不是假装所有重试只发生过一次。日志只保存元数据，不保存 prompt、回答、工具参数、完整 Key 或上游错误响应正文。

`cost_nano` 是十亿分之一美元；`cost_known` 表示有上游 token 用量且手动确认计价，不等于上游已开票。`usage_mode=reserved_unknown/reserved_after_restart` 表示不确定并保留预留，不应当作实际结算金额。
