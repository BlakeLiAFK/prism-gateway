# Changelog

## 1.15.0 · 2026-09-22

- 采集上游在响应头里声明的限额（名字含 `ratelimit` / `rate-limit` / `x-quota`
  以及 `retry-after`），原样留存在运行状态里，系统设置页新增「上游限额」一节列出。
  各家的头名、单位、重置时间格式都不一样，所以先原样留存看清楚，再谈怎么用。
  重置时间支持四种写法：毫秒/秒时间戳、Go duration（`6m0s`）、RFC3339、纯秒数。
- **剩余归零时直接冷却到重置时刻**，不必再拿下一个请求去撞一次 429。
  只认 `remaining == 0` 这种事实陈述——剩余比例低于某个阈值是需要上下文才能
  解读的推测，各家单位又不一致，据此拉黑候选容易误伤好用的上游。
- 限额快照只存内存，重启即清空：它是「最近一次上游怎么说」的快照，不是账本，
  不值得为此加宽请求记录表。

## 1.14.0 · 2026-09-22

三类「上游通了但这个候选用不了」的情况，以前都会直接甩给调用方，现在统一换候选：

- **404 纳入耗尽类切换**：上游下架某个模型变体就会返回 404（OpenRouter 的
  `:free` 变体说没就没）。这对当前候选是确定性失败，换下一个并冷却 15 分钟。
  在此之前 lite 档第一个候选 404，后面两个活着的候选根本不会被尝试。
- **区分上游 502 与网关自造的 502**：上游网关层回的 502 / 504 说明请求没到模型、
  用量为零，换候选是安全的；网关自己标成 502 的那些（连不上、SSE 类型不对、
  响应读不懂）仍然不重放——请求可能已被部分处理。
- **空回答判定**：上游回 200 却没有任何可用正文时判这个候选失败。推理模型配上
  偏小的 max_tokens 就会这样：输出预算全花在思考上，`finish_reason=length`
  而 content 是空字符串。实测 `deepseek-v4.1-flash` 与 `glm-5.3-flash`
  在 max_tokens=16 时都如此，且两家上游都不接受关闭推理的参数
  （`reasoning_effort:"none"` 被 Command Code 拒为 400，
  `reasoning:{enabled:false}` 与 `thinking:{type:"disabled"}` 均无效）。
  这类失败记 `EMPTY_OUTPUT_TRUNCATED`，不再静默记成 success。
  只作用于非流式：流式一旦开始写就没法回退换候选了。
- 失败记账两处修正：带自有错误码的失败（空回答、跨协议不兼容）记它自己的码，
  不再一律写成 `UPSTREAM_502`；上游回了 200 且给了可信用量时按实际用量记账——
  空回答同样烧掉了推理 token，那是真花出去的钱，抹成零或退回预留值都不对。

## 1.13.0 · 2026-09-22

- 请求记录新增来源 IP 与客户端标识（User-Agent）。`requests` 表加两列，
  老库启动时按 `pragma table_info` 检查后补列，历史记录留空不影响展示。
  取地址的规则：**只有直连地址是回环时才采信 `X-Forwarded-For` 的第一跳**。
  网关与 Caddy 同机，这样拿得到真实 IP；公网直连时伪造的 XFF 一律不认，
  也就不必再维护一份可信代理名单。UA 截断到 200 字符。
- 请求列表重排：时间挪到第一列并精确到秒，Tokens 拆成输入 / 输出两列
  （缓存与写入作为副行，有值才显示），末列显示来源 IP 与客户端名
  （Claude Code / curl / OpenAI Python…，完整 UA 挂 title）。窄屏折叠来源列。
- 搜索框支持按来源 IP 查。
- 请求详情从打印整条 JSON 改成结构化明细表：来源、时间、模型与协议转换、
  四项 token、计价来源、错误码与选择原因逐项列出，原始 JSON 收进折叠区保留。

## 1.12.7 · 2026-09-22

- DeepSeek 余额支持多币种。`balance_infos` 是数组，账号可能同时持有人民币和美元
  两条记录，原来只认第一条——如果第一条恰好是余额为 0 的美元，卡片上就显示 $0.00。
  现在两种币各列一条，主数值取第一个非零的。
- 余额明细里为 0 的项不再占位置。「赠送余额 ¥0.00」摆在那里，一眼看过去像是没钱。

## 1.12.6 · 2026-09-22

- 供应商头像改用各家官方 app icon（Anthropic / Command Code / DeepSeek /
  OpenRouter / Z.AI / OpenCode），压到 96px 随二进制内嵌，不请求外部图片。
  OpenAI 官网挡爬虫拿不到图，它和 Ollama 继续用单色路径——这两家的官方标识
  本来就是单色，不影响辨识。

## 1.12.5 · 2026-09-22

- 修复供应商图标几乎不可见：图标用的 class 名 `brand` 与侧边栏品牌块的
  `.brand{color:#f3f8f3}` 撞名，`fill:currentColor` 被染成近白色，浅色底上等于隐形。
  改用 `provider-logo`。

## 1.12.4 · 2026-09-22

- 额度主数值只在纯数值时用等宽字体。Z.AI 的「5 小时 1%」带中文，等宽下字距被撑开。

## 1.12.3 · 2026-09-22

- 供应商头像换成各家官方标识，不再是 CC / ZA / API 这种缩写方块。单色路径内联在
  前端里，跟随明暗主题，不请求外部图片。
- 品牌识别先看供应商类型，再按 base_url 认域名：OpenRouter、Ollama 这类是以
  custom 类型接进来的，只看 kind 会全落成一个灰色的 API 方块。

## 1.12.2 · 2026-09-22

- Z.AI 额度主数值改成「5 小时 1%」，原来只写「已用 1%」看不出是哪个窗口的百分比。

## 1.12.1 · 2026-09-22

- Command Code 与 Z.AI 的额度改为真查，不再只写「不支持」。两家的接口都没有
  写进公开文档，但都随官方客户端一起发布，形状是确定的：
  - Command Code：`GET /alpha/billing/credits`，CLI 的 `/usage` 走的就是它，
    鉴权同样是 Bearer + 同一把 Key（官方文档写明 Provider Key 与 CLI Key 同源）。
    展示剩余总额，并拆出月度剩余、附加额度，以及 5 小时 / 本周两个滚动窗口
    ——套餐档真正会卡住请求的是这两个窗口，不是总额。
  - Z.AI：`GET /api/monitor/usage/quota/limit`，取自官方 glm-plan-usage 插件源码。
    鉴权是裸 token，**加 Bearer 前缀会被判为未鉴权**。展示 5 小时 Token 用量百分比
    与一个月 MCP 用量百分比。地址跟随 base_url 域名，智谱开放平台同路径。
- 上游用鉴权失败也回 HTTP 200 的（Z.AI 就是），改为按 body 里的业务码判失败，
  不再把一条错误消息当额度渲染出来。

## 1.12.0 · 2026-09-22

- 供应商卡片显示上游额度。只有少数供应商提供「凭 API Key 就能查」的接口，
  能查的直接查，查不了的写明原因，不做猜测性展示——这一栏和总览里那个
  本地滚动估算的预算水位不是一回事，混在一起看就失去意义了。
  - DeepSeek：`GET /user/balance`（接口挂在根路径，不在 `/v1` 下），
    展示总余额并拆出赠送 / 充值两项。
  - OpenCode Zen：`GET {base}/usage`，形状未公开，走通用扫描，
    把最像余额的字段提为主数值，其余原样列出。
  - OpenRouter（按地址识别）：`GET /api/v1/credits`，展示已充值减已消耗。
  - Command Code：Provider API 只有 chat / responses / messages / models
    四个端点，额度只在 CLI `/usage` 与 Studio 可见，卡片上明确标注不支持。
  - OpenAI / Anthropic / Z.AI：没有按 Key 的余额接口，同样标注原因。
- 新增 DeepSeek 供应商类型，base_url 预设 `https://api.deepseek.com`。
- `provider.usage` 并发查询全部供应商，结果缓存 60 秒；前端在供应商页渲染
  之后异步拉取，上游慢或超时都不挡页面。

## 1.11.0 · 2026-09-22

- 额度耗尽（402）与套餐 / 权限不足（403）纳入候选切换：这类失败对本次请求是
  确定性不可用，和 429「上游暂时忙」不是一回事，但同样应该让位给下一个候选。
  冷却给到 15 分钟——要等人去充值或升级套餐，30 秒后重试只是再撞一次墙。
  带服务端会话状态或内置工具的请求仍然不重放，沿用既有的 safeFallback 判定。
- 路由卡片与供应商卡片补上启用开关，不必再进编辑对话框改一个勾选框。
  模型列表原本就有。
- 模型库在按供应商过滤时直接给出该供应商的同步入口。原来同步只能从供应商
  页面发起，站在模型库里想刷新一下反而找不到门。

## 1.10.0 · 2026-09-22

- 新增 Command Code 与 Z.AI 两种供应商类型。
- Command Code 的模型列表直接声明了每个模型支持的端点，同步时照抄
  `supported_endpoints` 判定原生协议（`/messages` > `/chat/completions` >
  `/responses`），不像 OpenCode 那样维护一张会过期的模型名表。
- Z.AI 同时提供 OpenAI 兼容面和 Anthropic 兼容面，协议由填入的地址决定：
  地址含 `/anthropic` 走 messages，否则走 chat。默认预设为 Coding Plan 端点
  `https://api.z.ai/api/coding/paas/v4`。

## 1.9.1 · 2026-09-19

- 路由列表改为拖拽排序，编辑器里的「列表排序」数字框去掉了。顺序本来就该是
  拖一下的事，`sort` 只是它的存储形式，不该漏到界面上让人手填。
- 新增 `route.reorder`：一次收下完整的 id 顺序批量写入。逐个保存会在中途失败时
  留下一个比原来更错的顺序，而且每存一次就要递增一次配置版本。

## 1.9.0 · 2026-09-19

- 新增第四个协议面 `POST /typesafe/v1/systemone`，接入 TypeSafe 的 System One
  决策模型（Jev）。它返回类型化答案与概率分布，不产生文本，也没有流式。
- **与三种对话协议之间禁止互相转换**。两边没有等价语义：一边是消息序列与文本
  增量，一边是状态加问题、答案加概率。允许转换只能靠编造文本或把类型化答案
  字符串化塞进 content，两者都会静默改变调用方拿到的东西。跨协议命中时直接
  拒绝并说明原因，System One 模型也不列入对话协议的模型目录。
- 请求在发往上游之前完成结构校验（state、题型、criteria 数量、流式开关）。
  上游对这些同样会报 422，但那时配额已经预留、请求记录也写下了，而且上游的
  错误正文不转发，调用方只会看到一句「上游请求失败」。
- 调试台增加 System One 面板：可视化编辑三种题型，答案以概率条呈现。
- 上游 529（TypeSafe 的过载码）纳入安全故障切换，语义等同 503。502 仍不重试：
  它往往代表上游真的坏了，重放解决不了。
- 配额、计费、请求记录、客户端 Key 鉴权与现有协议完全一致，无需另建一套。

## 1.8.1 · 2026-09-19

- 路由列表按新增的 `sort` 字段排序，路由编辑器提供「列表排序」输入。此前按 ID
  字母序排出 auto / lite / max / pro，与 lite → auto → pro → max 的能力梯度无关。
  `sort` 存在既有的 JSON body 内，不改表结构；老数据取 0，退化为原来的字母序。
- 修复候选搜索的过滤不生效：`hidden` 属性设上了，但 `.setting-row{display:grid}`
  盖过了浏览器默认的 `[hidden]{display:none}`，429 行照常全部显示。计数正确而
  列表不动，界面上看像是搜索坏了。
- WebUI 冒烟脚本此前断言的是 `hidden` 属性而不是实际可见性，因此这条问题一直
  测试通过。断言改用 `offsetParent`。

## 1.8.0 · 2026-09-19

- 同步模型时把上游已声明的能力一并带过来：名称、上下文、输出上限、工具、图像
  与各项单价。此前只取 id，其余硬编码成上下文 128000、输出上限 4096——
  4096 连 Claude Code 要的 64000 都不够，于是同步等于白做，还得一个个手填。
- 单价按每百万 token 存储并收敛浮点误差（0.00000045 × 1e6 会得到
  0.44999999999999996，直接存会一路显示到界面上）。
- **`pricing_set` 仍然保持 false**：价格抄过来了，但没有人对着自己的账户确认过，
  不能当作已知费用。这条约束不因为同步而放松。
- 模型 ID 直接用上游原名（如 `deepseek/deepseek-v4-flash`），客户端传 model
  时更直观；与已有模型、路由或别名重名时才退回带 provider 前缀的形式。
- 路由编辑器的候选列表加搜索。已勾选的行在任何搜索条件下都保持可见——
  把用户已经选中的东西藏起来，会让人以为选择丢了。

## 1.7.2 · 2026-09-19

- `--reset-admin` 轮换完即退出，不再继续进入服务循环。此前「停服务 → 轮换 →
  启动」这套标准操作会卡在中间那一步：轮换命令不返回，后面的启动命令永远等不到
  执行，留下一个不受 systemd 管理、还占着数据库锁的游离进程。
- 令牌文件的提示改为「与数据库和主密钥同等保护」，不再建议读后删除——
  它 0600 且仅属主可读，是遗失令牌之外唯一的查询入口。

## 1.7.1 · 2026-09-19

- 标准输出不是终端时，管理员令牌改为写入 `<db>.admin-token`（0600），
  stdout 只给出路径。此前的做法是让横幅走 stdout 而不走 slog，理由正是
  「令牌不得进入日志采集链路」——但 systemd 会把 stdout 收进 journald，
  容器运行时会收进日志驱动，令牌照样落进了日志系统。
- 新增 `scripts/loadtest.py`：并发与长稳压测，用本地演示模型施压，
  不花钱也不依赖外部服务。报告吞吐、延迟分布、内存轨迹，
  并在计数器未归零或内存持续增长时判定失败。

## 1.7.0 · 2026-09-19

真实 Claude Code 接入暴露出来的一组问题。此前只用 curl 测过协议端点，
单轮纯文本能通，但 Claude Code 实际发出的请求带着一批网关不认识的字段，
整个请求被拒，而错误只说「没有兼容此请求的候选模型」。

- 无兼容候选的错误现在带上每个候选被排除的具体原因。这些原因本来就算出来了，
  只是没告诉调用方，等于让人去猜。
- 新增「可安全忽略」的请求字段：metadata、context_management、output_config、
  service_tier、store、user、cache_control。它们不影响模型看到的内容，
  跨协议时也没有对应概念。被忽略的字段会出现在 `X-Prism-Ignored` 响应头里——
  忽略可以，不说不行。
- 请求侧的 thinking 归 drop_reasoning 管：已经接受丢弃推理内容的模型，
  再要求思考没有意义，因此忽略该字段而不是让整个请求失败；
  未开启该开关时仍然拒绝，并点名是 thinking 导致的。
- cache_control 嵌在内容块里而非顶层，单独递归扫描以便如实汇报。

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
