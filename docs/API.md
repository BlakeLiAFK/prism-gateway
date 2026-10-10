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
| `provider.list` | `{}` | Provider[] |
| `provider.save` | `{version,id?,provider}` | 新 Config |
| `provider.delete` | `{version,id}` | 新 Config；存在引用则拒绝 |
| `route.stats` | `{minutes?}` | 近 N 分钟（默认 60）各路由的实际落点 share（每个候选含 attempts、success、affinity 命中次数）与运行状态 runtime |
| `route.live` | `{}` | 实时面板数据（缓存 5 秒）：全局并发；被路由引用的模型的并发、RPM、首字节、会话数、近 5 分钟请求与单请求 tok/s；各路由的并发 / 容量、近 60 秒 RPM 与输出 tok/s、近 5 分钟成功率、会话数、加权首字节 |
| `alert.get` | `{}` | 告警推送配置：`enabled`、`kind`（telegram / webhook）、`chat_id`、`has_secret`、`events`；不返回凭证 |
| `alert.save` | `{enabled, kind, secret?, chat_id?, events:{quota,failures,route}}` | 保存告警配置；`kind` 为 `telegram`、`webhook`（POST JSON `{event,text,at}`）或 `webhook_text`（POST 纯文本正文）；`secret` 留空表示沿用已保存的 Token / 地址，加密存储 |
| `alert.test` | `{}` | 立即发送一条测试消息 |
| `watch.token_create` | `{name?, days?}` | 生成终端监控令牌：`id`、`token`（只返回这一次）、`name`、`expires_at`；`days` 为 1 到 30，默认 7；同时有效的令牌最多 20 个 |
| `watch.token_list` | `{}` | 有效的监控令牌（不含令牌本身）：`id`、`name`、`created_at`、`expires_at`、`last_used_at` |
| `watch.token_revoke` | `{id}` | 撤销监控令牌；正在使用它的终端在几秒内断开 |
| `schedule.get` | `{}` | 定时任务配置、每项最近一次运行 `runs.{task}:{at,status,result}`、推送通道是否可用 `channel` |
| `schedule.save` | 配置字段（只覆盖提交的字段） | `utc_offset`、`{backup,quota,report,expiry,maintain,upstream}_enabled`、`backup_hour`、`backup_keep`、`quota_percent`、`quota_minutes`、`report_hour`、`expiry_days`、`maintain_hour`、`upstream_hours`、`free_enabled`、`free_hours`、`free_route`、`free_min_context`、`free_require_tools`、`free_drop_reasoning`、`free_exclude`（逗号分隔的上游 ID） |
| `schedule.run` | `{"task":"backup"}` | 立即运行一次，返回 `{at,status,result}`；`result` 为空表示无事可做 |
| `model.stats` | `{days?}` | 读取用量小时汇总表，近 N 天（默认 7，最多 90）按模型与供应商汇总的请求、成功、失败、tokens、估算花费、未计价次数、平均耗时、最后使用时间 |
| `model.usage` | `{id, days?, tz_offset_min?}` | 单个模型近 N 天（默认 30）的每日用量（按浏览器时区分日）与按 `agent_role` 的拆分 |
| `request.reprice` | `{model_id?, dry_run?}` | 按当前单价补录价格确认之前的请求：只处理 `usage_mode=reported_tokens`、未计价、非演示且模型已确认价格的记录，补录后标为 `reported_tokens_repriced`；`dry_run` 只返回条数与金额 |
| `provider.reorder` | `{version,ids:[]}` | 新 Config；按 ids 顺序整体重排供应商，未列出的保持原序号 |
| `provider.test` | `{id}` | GET /models HTTP 状态与耗时，不调用模型生成 |
| `provider.sync_models` | `{id}` | `job_id`，异步执行 |
| `provider.usage` | `{}` | 每个供应商一项：supported / note / headline / fields / error / limits；结果缓存 60 秒 |
| `model.list` | `{}` | Model[] |
| `model.save` | `{version,id?,model}` | 新 Config；model.id 是对外名称 |
| `model.delete` | `{version,id}` | 新 Config；检查路由与别名引用 |
| `route.list` | `{}` | Route[] |
| `route.save` | `{version,id?,route}` | 新 Config |
| `route.delete` | `{version,id}` | 新 Config |
| `route.reorder` | `{version,ids:[]}` | 新 Config；按 ids 顺序整体重排路由，未列出的保持原序号 |
| `route.test` | `{id,protocol?,request?}` | ranked / checks / note；默认模拟 Chat 文本 |
| `alias.list` | `{}` | Alias[] |
| `alias.save` | `{version,alias:{id,target,enabled}}` | 新 Config |
| `alias.delete` | `{version,id}` | 新 Config |
| `settings.get` | `{}` | Settings |
| `settings.update` | `{version,settings}` | 新 Config |
| `quota.list` | `{}` | 每模型本地窗口预算 / 已用及在途预留 |
| `apikey.list` | `{days?}` | 网关 Key 列表（不含密钥），每把附带近 N 天（默认 7）用量 `usage` |
| `key.usage` | `{id, days?, tz_offset_min?}` | 单把 Key 近 N 天（默认 30）的每日用量、常用模型、常用路由与近 7 天来源 |
| `apikey.create` | `{name,allowed:[]}` | id / key / warning；key 仅创建时返回；可带 `expires_at`（毫秒，0 为永久）、`limit_day` / `limit_month`（USD，近 24 小时 / 30 天）、`rpm` |
| `apikey.update` | `{id, name, allowed?, expires_at?, limit_day?, limit_month?, rpm?}` | 修改 Key 的名称、授权范围、到期与限额，密钥不变，立即生效 |
| `apikey.revoke` | `{id}` | 撤销结果 |
| `session.list` | `{}` | 有效期内最近 200 个本地亲和映射，附 `expires_at` |
| `session.delete` | `{id}` 或 `{model_id}` | 解除本地绑定（给 model_id 时解除该模型的全部绑定，返回 unbound 数量），不重置上游状态 |
| `request.list` | `{page?,page_size?,q?,status?,provider_id?,from?,to?,min_input_tokens?,max_input_tokens?,min_output_tokens?,max_output_tokens?,min_cost?,max_cost?,sort?}` | items / page / page_size / total / from / to；默认过去24小时，单次最多30天 |
| `request.get` | `{id}` | 单次上游尝试元数据 |
| `job.list` | `{}` | 最近 100 个模型同步任务 |
| `job.get` | `{id}` | 任务状态、结果或错误 |
| `audit.list` | `{}` | 最近 200 个审计事件 |
| `audit.search` | `{page?,page_size?,q?,action?,target?,from?,to?}` | `{items,page,page_size,total,pages}`；每页默认30、最多100；时间为毫秒；操作/目标模糊筛选 |
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
      "enabled":true,
      "description":"My internal coding route",
      "groups":[
        {"name":"日常","strategy":"weighted","affinity":true,
         "candidates":[{"model_id":"my-coder","weight":10},{"model_id":"my-coder-b","weight":10}]},
        {"name":"兜底","strategy":"priority","affinity":true,
         "candidates":[{"model_id":"my-claude","weight":10}]}
      ]
    }
  }
}
```

路由由 1 到 5 个**组**按顺序组成。请求先在第 1 组按该组的 `strategy` 选候选；本组全部被淘汰、冷却、并发已满或上游失败，
或都接不了这次请求时，才轮到第 2 组，依此类推。每组有自己的 `strategy` 与 `affinity`。同一个模型只能出现在一个组里；
组内可以没有候选（新建路由、免费模型路由本来就允许）。`candidates[].disabled:true` 暂停候选。

`strategy` 决定一个组内候选的尝试顺序：

| 取值 | 规则 |
| --- | --- |
| `priority` | 按候选顺序 |
| `balanced` | 权重优先：`weight` 最高的先试，设了本地预算时按用量降权；没配预算时就是按权重固定排序，不做随机分流 |
| `weighted` | 按 `weight` 比例随机抽签，备选顺序同样按剩余权重抽取 |
| `latency` | 按上游 2xx 响应头耗时的滑动平均升序，没有样本的先试。非流式请求的响应头要等生成结束，长回答会被算慢 |
| `cost` | 按 `input_price + output_price` 升序，`pricing_set=false` 的排最后 |
| `least_busy` | 按当前并发 / 并发上限升序 |

所有策略共用三条规则：本组会话亲和的模型排最前；冷却中、并发已满或本地 RPM 已满的候选排本组最后（不剔除）；原生协议在同分时优先。
后一组的所有候选一律排在前一组之后。

**会话亲和按组记**：键是「网关 Key + 客户端会话头」加组序号，第 1 组的键与分组之前相同，升级前的会话绑定不受影响。
落到第 2 组并成功时只写第 2 组的绑定，第 1 组的绑定不动：第 1 组恢复后下一次请求回到它原来的模型；同一会话连续落在第 2 组时，
第 2 组的亲和让它一直用同一个模型（缓存与上下文一致）。Claude Code 的子代理与主线程共用 `X-Claude-Code-Session-Id`，
网关会再拼上 `x-claude-code-agent-id`，让每个子代理单独亲和。故障切换成功后，如果本组原绑定模型仍是合格候选、
且冷却剩余不超过 5 分钟，绑定保持不变，冷却结束后回到原模型；否则改绑到本次成功的模型。
请求记录的 `agent_role` 列保存 Claude Code 声明的角色（如 `subagent:Explore`）；
客户端设置 `CLAUDE_CODE_GATEWAY_HINT_HEADERS=1` 后才会发送类别与类型，否则子代理只记为 `subagent`。

请求落到第 2 组及以后时，按路由推送一次 `route` 告警（30 分钟限频，键 `route-group:<路由 ID>`），因为那通常意味着前面的组都挂了、
正在用更贵的模型。前面的组被暂停、停用、为空，或本来就接不了这类请求（如 Claude Code auto 权限模式的分类器请求）时不告警：那是预期行为，不是故障。`route.test` 的结果按组序列出候选并带上 `group`。

同一请求内换候选的条件：上游 429 / 503 / 529（按 Retry-After 冷却）、402 / 403 / 404（冷却 15 分钟）、400（请求被明确拒绝，不冷却）、
200 但正文为空，以及流的第一个事件为过载或限流错误。上游 5xx、连接失败、超时与其他首事件错误不换候选：状态码与错误原话（脱敏）原样返回，
该候选冷却 30 秒，客户端重试时会落到别处。全部候选都被准入拒绝时返回 429，`Retry-After` 为最短剩余冷却；剩余超过 60 秒附 `x-should-retry: false`。

**旧格式兼容**：1.36.0 及以前的路由是单组，字段 `strategy` / `affinity` / `candidates` 直接在路由上，候选可带 `fallback:true`。
加载旧配置、`config.import` 和 `route.save` 都会自动规范化：单组路由变成一个「主」组，`fallback` 候选拆成第 2 组「兜底」（`priority`、亲和开）。
旧客户端仍可用旧字段保存只有一组的路由（落到第 1 组）；路由已有多组时旧字段会被拒绝（400 `此路由已分组`），因为它没法表达改哪一组。
`groups` 与旧字段不能出现在同一个请求里。输出只有 `groups`。**升级到 1.37.0 之后不要回滚到更早的版本**：旧版本不认识 `groups`，路由会变成没有候选；必须回滚时先从备份恢复配置。

`route.save` 可带 `enable_models:true`：在同一事务里启用所有候选模型。

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

### 远程备份（R2 / S3 兼容）

| Action | 参数 | 说明 |
| --- | --- | --- |
| `r2.get` | `{}` | 存储配置；只返回 `has_secret`，不回显 Secret |
| `r2.save` | `{endpoint, bucket, prefix, region?, access_key_id, secret?}` | `secret` 留空沿用已保存的值；`region` 默认 `auto`；前缀只允许 `[A-Za-z0-9._/-]` |
| `r2.test` | `{}` | 写入、列出、删除一个探测对象，三步都成功才算连通 |
| `backup.remote_list` | `{}` | 网关上传的备份 `[{key, size, modified_at, at}]`，新的在前；`at` 取自对象名里的 UTC 时间 |

定时上传由定时任务 `remote` 完成（`schedule.save` 的 `remote_enabled`、`remote_minutes` 10–1440、`remote_keep` 1–1000），
对象名为 `{prefix}gateway-YYYYMMDD-HHMMSS-mmm.db.gz`，快照在内存中生成并压缩，本机不落盘。

### 在线还原

`backup.restore`：`{"source":"local","name":"gateway-….db","confirm":true}` 或 `{"source":"remote","name":"prism/gateway-….db.gz","confirm":true}`。
备份载入内存后校验完整性、是否 Prism 数据库、schema 版本并试解密全部上游凭证，全部通过才写入线上库。
还原前生成本机安全快照；`admin_digest`、管理会话、R2 配置与定时任务状态沿用当前值。返回 `{restored_from, safety_backup, config_version}`。

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

`count_tokens` 只计数、不生成：它不计入网关 Key 的每分钟请求数与花费上限，也不写请求记录。这是有意的取舍——Claude Code 每轮会多次调用它，计入 RPM 会误伤低 RPM 的 Key；上游的原生计数接口本身不计费。已过期或已吊销的 Key 照常被拒绝，模型授权范围照常检查。

## 请求记录

一个客户端请求可能因上游 429/503产生多个 attempt。`parent_id` 是客户端 X-Request-ID，`id` 是单次 attempt。Dashboard 的请求数按 attempt 统计，不是假装所有重试只发生过一次。日志只保存元数据，不保存 prompt、回答、工具参数、完整 Key 或上游错误响应正文。

`cost_nano` 是十亿分之一美元；`usage_mode=reported_cost` 表示采用 OpenRouter `usage.cost` 实际费用（包括明确的零），`cost_known=1`，金额已知与 token 是否完整独立。其余已知费用使用确认的本地价格估算；缺失、非法及负金额不会当作免费。转换或客户端输出失败仍保留已获得的可信实际费用。`usage_mode=reserved_unknown/reserved_after_restart` 表示不确定并保留预留，不应当作实际结算金额。


## 终端实时监控

后台「系统设置 / 终端监控」生成一条带令牌的 curl 命令，粘贴到终端就能实时看到网关的状态与事件。
入口仍是 `POST /api.json`，action 为 `watch.stream`，不新增 URL。

```sh
curl -sN --fail-with-body https://HOST/api.json \
  -H "Authorization: Bearer prism_watch_..." -H "Content-Type: application/json" \
  -d '{"action":"watch.stream","params":{"mode":"dashboard","width":100,"height":40,"color":true}}'
```

| 参数 | 说明 |
| --- | --- |
| `mode` | `dashboard`（默认，原地刷新，约 2 秒一帧）或 `log`（逐行追加，可接 grep、tee） |
| `width` / `height` | 终端宽高，范围 60 到 240、12 到 80，默认 100 与 40；后台生成的命令用 `tput` 自动填入。日志模式默认 240 列 |
| `color` | 是否输出 ANSI 颜色，默认关闭。仪表盘模式始终使用光标控制序列，所以输出不要重定向到文件 |

响应是持续推送的 `text/plain`（分块传输），**不是**带 `data:` 前缀的标准 SSE，curl 直接打印才干净。

**认证。** 只认监控令牌（`prism_watch_` 前缀）。它只能打开这个流，调用任何其他 action 都按未登录处理；
管理员令牌与网关 Key 不能用于监控流。令牌默认 7 天有效，可随时撤销，库里只存摘要。
令牌无效时返回 401，用法错误返回 400，连接数超限返回 429，服务正在停止时返回 503。
每个令牌最多 3 个并发连接，全局最多 16 个。

**事件。**

| 类型 | 含义 |
| --- | --- |
| `OK` / `FAIL` / `CXL` | 请求完成、失败、客户端取消，含路由与实际模型、耗时、输出 token、原生或转换 |
| `REJ` | 没有候选能处理的请求（淘汰或准入被拒），含原因 |
| `COOL` | 模型进入冷却及时长 |
| `ALERT` | 告警，不论推送通道是否开启 |
| `ADMIN` | 配置变更与其他管理操作，取自审计记录 |
| `JOB` | 定时任务结果 |
| `SYS` | 连接状态：令牌剩余时间、事件丢弃、令牌失效、服务即将重启 |

事件只含元数据（模型、耗时、错误码），不含提示词、回答和密钥。来自客户端或上游的字符串在进入事件前
会清洗控制字符与转义序列，不会在管理员的终端里画出任意内容。

**行为。**

- 新连接先回看最近的事件；日志模式连续 20 秒没有事件时输出一行状态，也起保活作用。
- 订阅者读不过来时丢弃事件并在流里提示数量，绝不拖慢请求处理。
- 令牌每 5 秒复核一次，撤销或过期后流自动结束。
- 服务停止时先结束所有监控流，不会占用优雅停止的等待时间。生成的命令带断线重连，
  部署重启后约 2 秒自动接上；令牌失效（HTTP 401）时停止重连。
- 仪表盘的并发、RPM、首字节等数字来自路由实时面板，最多滞后 5 秒；事件与趋势是实时的。
- 事件与趋势按尝试计数，不是按请求计数：一次请求先在候选 A 上被限流、再切到候选 B 成功，会产生两条事件。
- 事件只保存在内存里，重启后清空；趋势图覆盖最近 30 分钟。

反向代理需要对这个响应关闭缓冲。网关已设置 `Cache-Control: no-cache, no-transform` 与 `X-Accel-Buffering: no`。

## 自动价格、报告与告警

以下字段通过 `schedule.get/save` 读写，新增任务默认关闭。`schedule.run` 可手动运行对应任务。

| 任务 ID | 配置字段 | 默认与范围 |
| --- | --- | --- |
| price | price_enabled、price_hours | 关闭；24小时，1–168 |
| weekly | weekly_enabled、weekly_weekday、weekly_hour | 关闭；周一（1–7）、9点（0–23） |
| spend | spend_enabled、spend_multiple、spend_minimum | 关闭；2倍、最低10美元；每小时检查 |
| failure | failure_enabled、failure_minutes、failure_min_samples、failure_percent、failure_cooldown_minutes | 关闭；15分钟、20样本、20%、60分钟冷却；每5分钟检查 |
| key-budget | key_budget_enabled、key_budget_percent、key_budget_cooldown_minutes | 关闭；80%、1440分钟冷却；每小时检查 |

- `model.save` 的 `model.price_locked` 默认为 false；自动价格同步跳过锁定模型，仅处理启用的 OpenRouter 供应商，先完整获取再通过配置事务写入。缺失或非法价格不覆盖现值，无变化不生成配置版本。开启同步或手动运行即确认有效上游标价。
- 周报覆盖上一个完整周一至周日，按配置时区、星期和时刻发送；错过本周时刻会补跑一次。日报、周报中的模型优先使用显示名称，删除后回退 ID。
- 花费异常比较今日与前七个完整自然日均值；均值为零不计算倍率。统计口径包含已记录实际费用、本地估算及异常预留，不能视为供应商账单。
- 失败率按请求尝试的精确分钟窗口统计，排除演示与进行中请求；错误与状态未知均计入，保留原有连续失败告警。
- Key 预警沿用限额的滚动24小时/30天口径，不是自然日/月。每个 Key 和窗口独立冷却；推送失败不记录去重。

## 缓存命中统计

`model.stats` 和 `model.usage` 的汇总项包含 `cache_hit_rate`（0–1或null）、`cache_samples`、`cache_known_requests`、`cache_null_requests`、`cache_input_tokens`、`cache_hit_tokens`、`cache_null_rate`。`route.stats` 返回 `cache`，按请求的路由/别名 ID 索引同类统计（小时汇总覆盖所选分钟窗口所在小时）。

命中率为明确报告缓存用量的请求中，缓存读取 token / 总输入 token；Messages 输入包含普通输入、缓存读取及缓存写入。缺失字段与显式 null 不算零命中，零分母返回 null；旧记录不能推断覆盖。界面显示样本数量，缓存命中不能单独证明会话亲和有效。独立缓存汇总表保留400天，不改变旧用量表结构，兼容交接时仍在排空的旧进程。


## 模型多协议与能力状态（v1.34.0）

`model.save` 支持 `supported_protocols`（chat/messages/responses/systemone 数组）和 `protocols_locked`。非空列表须包含默认 `protocol`，System One 不与对话协议混用。省略/`null` 保留旧式单协议语义，显式 `[]` 表示尚无确认协议，模型不会被路由。同步只更新未锁定的上游协议声明，不覆盖已有人工工具/视觉、名称、价格、预算设置；默认协议失效时改为新声明中的首个协议。

`tools_unknown` / `vision_unknown` 为 true 时，能力未确认，由上游判断；不是已验证支持。管理员显式提交 `tools` / `vision` 而不提交对应 unknown 字段，会清除未知状态。

`provider.save` 支持 `protocol_urls`，例如 `{"messages":"https://api.deepseek.com/anthropic/v1"}`。值是协议 Base URL，网关追加 `/messages` 等路径；空 map 清除覆盖，缺省沿用 `base_url`。支持运行时修改，遵循相同 URL 安全校验与配置版本约束。

### 临时请求结构诊断

`settings.update` 的 `settings.request_diagnostics_until` 为 Unix 秒截止时间，0 为关闭，最大为当前时间加 900 秒。只采集启用窗口内开始、且在截止前收到原生 Messages HTTP 400 的请求；窗口内最多 20 条，重启不重置。延长期限时重叠窗口内样本仍计数。

`request.get` 返回 `diagnostic` JSON 字符串，含 `before` / `after` 与 `captured_structure_changed`。结构仅含消息数量、角色枚举、内容块类型及截断标记；比较范围仅为采集到的脱敏结构，不代表全文相同。未知枚举仅标记 `unknown_string`，不保存原值。诊断沿用请求记录保留期，不进入模型响应。

### 请求记录范围与排序

`request.list` 按请求开始时间筛选。`from` / `to` 必须成对提供，为非负安全整数 Unix 毫秒，区间为 `[from, to)`；省略时使用服务器当前时间之前24小时。起始必须早于结束，跨度最多30天；不提供查询全部的入口，`all_time:true` 返回400。响应回传实际 `from` / `to`，翻页须复用该窗口。可查询任意历史区间，但单次仍受30天限制。

Token 范围 `min_input_tokens` / `max_input_tokens` / `min_output_tokens` / `max_output_tokens` 接受非负安全整数或纯数字字符串，最大9007199254740991。金额范围 `min_cost` / `max_cost` **只接受 USD 十进制字符串**（例如 `"0.10"`），最多9位小数，精确换算为 `cost_nano`，不经过浮点数。所有范围含上下限，留空或省略不限制；最小值大于最大值、负数、非法类型或溢出返回400。各范围与关键词、状态、供应商条件按AND组合。

`sort` 为 `newest`（默认）、`input_desc`、`output_desc`、`cost_desc`。排序覆盖全部命中记录，然后分页；相同值按开始时间和记录ID倒序排列。仍以一次上游尝试为一行，不合并同请求的多次候选尝试。

金额范围仅匹配 `cost_known=1` 且 `usage_mode` 为 `reported_tokens`、`reported_tokens_repriced`、`reported_cost`、`rejected` 的记录。进行中预留、结果不明保留和未知金额不当成零，也不混入消费筛选。Token 范围仅匹配 `reported_tokens`、`reported_tokens_repriced`，以及 `reported_cost` 中对应Token值大于0的记录：旧数据没有独立Token已知标记，仅报告金额时的零无法证明是已知用量，因此不猜测。未知值排序靠后。估算价值仍非供应商最终账单，不改变现有计费或预算语义。
