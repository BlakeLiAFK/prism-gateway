# 协议兼容边界

## 路由原则

客户端协议在模型的 `supported_protocols` 列表中（旧配置沿用 `protocol`）：原生转发。改写 model、使用供应商凭证、加入自己的 User-Agent、保留允许的会话与版本头；高级请求字段和模型响应不经过公共 IR，因此不会为了统一而丢失厂商专属状态。请求体里的数字按原字面量转发（大于 2^53 的整数、`1.0` 这样的写法都不会被改写）。网关仍检查体积、权限和输出上限。

协议不相同：先解析显式白名单，然后转换公共中间表示。无法转换则拒绝这个候选，尝试别的兼容候选；没有合适候选返回 400，而不是静默删除字段。响应出现未支持的内容时也不伪装转换成功。

| 来源 → 目标 | 普通 JSON | SSE | 覆盖范围 |
| --- | --- | --- | --- |
| Chat → Messages / Responses | 已实现并测 | 已实现并测 | 文本、函数工具、参数增量、结果 |
| Messages → Chat / Responses | 已实现并测 | 已实现并测 | system、文本、tool_use / tool_result |
| Responses → Chat / Messages | 已实现并测 | 已实现并测 | 普通 input、文本消息、function_call / output |
| 三种协议 → 同协议 | 原生转发并测 | 原生转发并测 | 保留原生字段，但仅提供列出的 HTTP 端点 |

常用图片 URL / data URI 引用可转换，前提是模型允许图像或能力尚未确认，且目标协议能表示。未知能力交上游裁决，不标成已验证支持。不由网关下载任意图片 URL。v1.33.3 已单独验证生产工具结果图片识别；本地测试不代表每个云模型都具备视觉能力。

Claude Code 等客户端的 `tool_result` 也支持文本、图片及混合内容：

- Messages 与 Responses 之间保留工具结果内的图文顺序和调用 ID；Responses 使用 `function_call_output.output` 内容数组。
- Chat 的 `tool` 消息不能携带图片。网关保留 `tool_call_id` 和结果占位，等全部并行工具结果返回后，把完整图文附在 `user` 消息内，用调用 ID 标注为不可信工具输出，并返回 `X-Prism-Adapted: tool-result-images`。图片不会丢弃，但图片与工具的关联变为文本标记，内容所在角色也发生变化；这是兼容映射，不是协议级无损转换。依赖严格工具信任边界的客户端应使用原生 Messages 或 Responses 模型。
- 纯文本工具结果沿用原格式；PDF、音频、嵌套工具调用等未支持的工具结果仍明确拒绝。模型必须启用“支持图像输入”，上游也必须实际具备视觉能力。

上述图片工具结果已覆盖本地 HTTP handler 的普通响应和 SSE 测试；测试不代表所有真实供应商模型都能识图。字段依据：[OpenAI function calling](https://developers.openai.com/api/docs/guides/function-calling)、[Claude 工具结果](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls)。

## 不做“假兼容”

跨协议不支持 Thinking / adaptive thinking、思考签名、redacted thinking、OpenAI reasoning / encrypted reasoning 项、任意 PDF/音频/视频、多模态服务端文件、内建 web search / code interpreter / computer-use tools、store=true / previous_response_id 等服务端状态、批量多候选、未支持的结构化输出约束、未映射 beta 字段。

遇到这些字段，应使用兼容的原生协议模型。原生透传也不意味着上游一定接受；最终由上游和客户端版本决定。原生不伪造模型身份，响应可包含上游的真实 model ID；网关自定义名称显示在诊断头与记录中。

Responses 仅提供创建响应 POST；**不提供** GET/DELETE response、cancel、input_items、Files、Batch、Realtime 或 WebSocket。请使用同步 HTTP/SSE，不要启用 `background:true` 工作流。服务端会话 continuation 必须显式固定模型，不能走 auto 路由。继续会话还依赖上游自己的保留策略，不由网关托管。

## SSE

三协议各有独立输出事件编码器。工具调用保留 call_id / tool_use ID；参数按 JSON 增量传输。完整工具 JSON 校验失败、reasoning 类型无法转换、无终止事件 EOF、上游错误事件等不会产生伪造的成功终止。

流开始之后不切模型。客户端取消会传递到上游 HTTP context，不后台继续生成。对于供应商已经计算但没报告的 token，账务保留不确定状态。

没有上游 usage 时，本地账务标记未知，不当作免费。跨协议响应中某些要求数值的 usage 结构可能出现兼容占位 0，**不得以这些占位计算真实账单**；本地记录的 cost_known / usage_mode 才用于判断是否有可核对依据。原生模式保留供应商 usage 原样。

## 速率、预算与会话

模型生成有全局并发、模型并发、模型 RPM、上游冷却和本地金额预算。达到本地限制直接 429，不创建无界队列。额度是按配置模型 ID 记录；相同实际模型跨不同供应商凭证的官方共享限额无法自动得知。

5h / 7d / 30d 是本项目滚动窗口，不是 OpenCode Go 的官方账期镜像。预留输入按序列化字节粗估，可能偏高或偏低，不能用来声称绝对不超支。缓存写入价格只采用手动统一单价，未细分所有供应商的不同缓存期限定价。

会话没有 prompt 数据库，不会自动续命缓存。不主动制造请求，不跨账号绕过额度，不把已付费订阅转换成无限公共 API。

## 接入建议

先在调试台验证一个固定原生模型，再测试包含 tools 的真实 Agent 工作流。Claude Code 优先固定 Messages 模型，Codex 优先固定 Responses 模型。只有明确属于公共子集的任务才跨协议路由。

本交付完成的是本地协议与真实 HTTP fixture 测试，没有使用用户的真实账户密钥，也没有将某个最新版本的 Codex / Claude Code 与所有云模型逐一回归。因此不声称所有 Agent 功能 100% 兼容。

## 参考资料

以下为实现参考，不是本项目获得官方认证的证明。核对日期：2026-09-19。

- OpenCode Go API 与客户端会话说明：https://opencode.ai/docs/go/
- Anthropic Messages：https://platform.claude.com/docs/en/api/messages/create
- Anthropic Streaming：https://platform.claude.com/docs/en/build-with-claude/streaming
- Anthropic Token Count：https://platform.claude.com/docs/en/api/messages/count_tokens
- OpenAI Responses Streaming：https://developers.openai.com/api/docs/guides/streaming-responses


## 上游附带推理内容时的跨协议行为

部分上游会在 OpenAI 格式响应里附带 `reasoning` / `reasoning_details`（实测：
OpenRouter 上的 DeepSeek、Nemotron 等）。

- **原生协议调用**：响应原样透传，厂商字段完整保留，不受影响。
- **跨协议调用**：返回 `UPSTREAM_INCOMPATIBLE`（HTTP 502）。Anthropic 的 thinking
  块需要签名，网关无法凭空构造；静默丢弃推理内容同样不可接受。

遇到这个错误时上游是正常的——请求记录里有完整的 token 用量。三种解决办法：

1. 对该模型使用它的原生协议；
2. 改用不返回推理内容的模型；
3. 在模型设置里开启 **跨协议时丢弃推理内容**（`drop_reasoning`，默认关闭）。

第三项是一个显式取舍：开启后跨协议转换会丢掉推理内容，并在响应头
`X-Prism-Dropped: reasoning` 标注。非流式只在确实丢弃时标注；流式无法等到
发现推理内容再补响应头，因此开关开启且跨协议时先行声明。

这个开关只覆盖推理内容。refusal、annotations 等其它不可转换内容仍然拒绝。

### 请求历史里的推理内容

会话先由原生推理候选服务过，历史里就会带着推理内容：Messages 的 `thinking` /
`redacted_thinking` 块、Chat 消息上的 `reasoning` / `reasoning_content` /
`reasoning_details`、Responses `input` 里的 `reasoning` 项。跨协议候选无法表达它们，
未开启开关时该候选被淘汰，错误信息为 `跨协议不支持内容类型 thinking` 等。

开启 `drop_reasoning` 后，网关在**发给这个候选的副本**上去掉上述内容，并在响应头标注
`X-Prism-Dropped: reasoning`。其余内容全部保留：用户输入、回复正文、工具调用与结果、
图片、系统提示词。去掉推理后只剩角色和空正文的 assistant 消息一并跳过；同一条消息上
还带着别的字段（如 Chat 的 `refusal`）时不跳过，交给转换器判定，无法表达的仍然拒绝。
客户端回传的空值推理字段（`"reasoning": null` 等）会从副本里去掉，但不算丢弃、不标注。

网关不保存也不修改会话历史。同一请求里的原生候选、以及之后切回的原生候选，收到的仍是
客户端发出的原始历史，签名不变。因此路由里暂停原生候选、会话落到跨协议候选、再恢复
原生候选，这个过程不丢已有的推理块；跨协议候选自己那几轮的推理按上文规则丢弃，
不会进入历史。

切回原生候选时，上游是否接受「部分轮次没有推理块」的历史取决于各上游实现，网关不做保证。

这里只处理历史里的推理内容。请求顶层的硬性控制不受影响：`thinking: disabled`、
Responses 的 `reasoning` / `include` 参数等仍按上文规则拒绝，不会因为开启了开关而被改写。

### 请求历史里的文档

Claude Code 用 Read 读 PDF 之后，历史里会带一个 `document` 块（在 `tool_result` 里），
用户也可能直接附上文档。跨协议候选收不了它，默认该候选被淘汰，错误信息为
`跨协议不支持内容类型 document`。

在模型设置里开启 **跨协议时用占位说明替换文档**（`drop_documents`，默认关闭）后，网关在
发给这个候选的副本上把每个 `document` 块换成一行文本：

    [document omitted by gateway: this upstream cannot receive documents across protocols]

并在响应头标注 `X-Prism-Dropped: documents`。用占位而不是删除，是为了不让 `tool_result`
变空、不打乱角色交替，也让模型知道这里原本有文档且没有送达，而不是凭空作答。

这是一个有损的取舍：该候选看不到文档原文，只看得到此前各轮围绕文档的对话。原请求不动，
原生候选与之后切回的原生候选仍收到文档原件。

- 与 `drop_reasoning` 相互独立，开启其中一个不会带上另一个。两者同时生效时响应头为
  `X-Prism-Dropped: reasoning,documents`。
- 只覆盖 Messages 的 `document` 块。Chat 的 `file`、Responses 的 `input_file` 不在范围内，
  仍然拒绝。
- 网关不提取文档文本。

### 候选全部被淘汰时

请求返回 400 `NO_COMPATIBLE_MODEL`，错误信息逐个列出淘汰原因，并保留英文字段名：Claude Code 在第三方端点上
收到含 `safeguards` 一词的 400 后，本次对话不再发送该字段，改为向用户询问权限；这个契约有回归测试保护。
路由里可以把一个原生 Anthropic 模型放在后面的组接住这类请求，见 API.md 的路由说明。同时：

- 请求记录里留一条 `error_code=NO_COMPATIBLE_MODEL`、`usage_mode=incompatible` 的记录，
  原因写在「选择原因」里（最长 500 字符）。它没有到达任何模型，不计入用量汇总。
- 请求的是路由（含经别名访问）、且淘汰原因不全是暂停或停用时，按 `route` 告警事件推送一次，
  沿用 30 分钟限频，限频键按路由计，同一路由的多个别名不会各发一次。直接按模型 ID 调用不告警。
- 候选被暂停或停用时，淘汰原因固定为 `candidate_paused` / `disabled`，不会被「不支持工具」
  等与请求内容相关的原因覆盖。
- `x-cmd-zdr` 把候选全部筛掉时响应仍为 `ZDR_UNSUPPORTED`，同样留一条记录，但不告警；
  候选在此之前就已全部被淘汰时，返回的是真实的淘汰原因而不是 ZDR 错误。
- 统计口径：这类记录没有到达任何模型，不进入用量汇总（用量页、Key 花费）和失败率定时任务；
  总览的请求数与错误数、指标导出、路由实时面板直接读请求明细，会计入它们，因为这是客户端
  真实收到的错误。所以同一时段总览的错误数可能多于用量页。存在这类请求时，总览会在统计卡片下方用一行说明写出条数与原因（`dashboard.get` 的 `summary.unreached_requests`）。

### 上游错误的交付

上游返回非 2xx 时，网关不改写它：最后一个候选的状态码与错误原话（去掉令牌形态的内容，最长 4000 字）按客户端
协议封装后原样交出，Messages 协议沿用上游的错误 `type`（不是 Anthropic 定义过的类型时按状态码映射）。
Claude Code 会从 `input length and \`max_tokens\` exceed context limit: a + b > c` 这类原话里自行调整参数，
改写它就等于拿掉了这个能力。请求记录的 `error_detail` 存同一句话的 300 字摘要。

上游 400 在同一请求内换下一个候选（请求被明确拒绝、没有生成内容）；5xx、连接失败、超时与流首事件错误不换，
该候选冷却 30 秒后由客户端自己的重试落到别处。所有候选都在冷却时，`Retry-After` 为最短剩余冷却，剩余超过
60 秒附 `x-should-retry: false`。网关不发明客户端不读的响应头，也不伪造 `anthropic-ratelimit-unified-*`。

路由页会对「本路由其他候选原生支持、而此候选需要跨协议」且未开启对应开关的候选显示
「会话切入受限」提示。只提示，不拦截保存。


## 被接受但不转发的请求字段

以下字段不影响模型看到的内容，跨协议时也没有对应概念。网关接受它们，
但不会转发给上游，并在 `X-Prism-Ignored` 响应头里列出实际忽略了哪些：

| 字段 | 说明 |
| --- | --- |
| `metadata` | Anthropic 规范中它不参与生成 |
| `context_management.edits.clear_thinking_20251015` | 无推理历史时接受省略 keep、keep="all"、keep={type:"all"}、keep={type:"thinking_turns",value:正整数} 并通过 X-Prism-Ignored 披露；其他服务端上下文编辑不转换 |
| `output_config.effort` | 仅努力程度偏好；`format` 单独转换并保留约束 |
| `cache_control` | 提示缓存标记，只影响成本不影响内容 |
| `service_tier` / `store:false` / `user` | 供应商侧调度与调用方标识；store:true 必须使用原生协议 |

`thinking` 不在此列——它会真正改变模型行为。开启模型的 `drop_reasoning` 后，仅已知的 `enabled` / `adaptive` 推理输出偏好（可带 `budget_tokens` 与取值为 `omitted` / `summarized` 的 `display`，Claude Code 固定带 `display`）可被忽略并披露。`disabled`、额外思考控制或未知类型不会被改成上游默认值；不能等价转换时仍拒绝。

无候选可用时，错误信息会逐个列出候选被排除的原因，例如
`没有兼容此请求的候选模型（m_x: 跨协议转换不支持字段 "thinking"…）`。


## v1.34.0：三家主力供应商的能力接入

核对日期：2026-10-01。供应商原生协议是各家的兼容接口，不代表底层模型实现了全部参数。

| 供应商 | 原生协议选择 | 多模态与工具边界 |
| --- | --- | --- |
| OpenRouter | Chat / Messages / Responses；模型同步保留三种兼容入口 | Chat 原样支持图片、PDF `file`、`input_audio`、`video_url` 等消息；Messages 支持图片/PDF。实际能力看模型和上游；不把 Chat 媒体块塞进另一协议 |
| DeepSeek | 官方当前 `deepseek-flash` / `deepseek-v4-pro` 配置三种协议；Messages 使用独立地址 | Flash 视觉、工具调用及历史推理原样传递；Responses 为无状态。Messages 不支持 `output_config.format`，网关拒绝该候选以免忽略格式约束 |
| Command Code | 以每个模型 `supported_endpoints` 为准，不推断全模型都支持三协议 | API 接受文本/图片，拒绝文件/音频；工具和推理仍取决于具体模型 |

模型保存 `supported_protocols`，`protocol` 是跨协议时使用的默认目标。请求匹配原生入口时不经过公共转换结构，因此推理签名、工具类型、JSON Schema、媒体、供应商参数保留。供应商的 `protocol_urls` 可分别覆盖各协议 Base URL，管理界面可运行时修改；所有地址仍受 SSRF 和配置版本检查。

同步刷新未锁定的协议声明，`protocols_locked` 可保护人工协议配置。旧模型的能力开关保持原意；新同步且未声明的工具/视觉能力使用 `tools_unknown` / `vision_unknown` 标为未知，交上游决定。显式禁用仍在本地拒绝。缺失协议列表的旧记录沿用默认协议，显式空列表表示协议未确认、不能路由。

跨协议 JSON Schema：Messages `output_config.format`、Chat `response_format.json_schema`、Responses `text.format` 已映射公共子集。不能等价表达的格式或语义字段明确拒绝，不删掉硬约束再继续生成。严格工具参数约束、并行调用控制、文本数组工具结果纳入转换回归。中途 system/developer 消息在 Chat/Responses 保留位置，Messages 无法表达时拒绝。

非流式与流式的 `reasoning_details` 纳入推理检测。`drop_reasoning` 默认关闭；显式开启才丢弃，并披露 `X-Prism-Dropped`。推理内容不是普通文本，不能编造签名来跨协议冒充。完整工具参数必须是有效对象，不把截断 JSON 转成 `input:null`。

ZDR 为供应商专属约束：`x-cmd-zdr: 1` 只走 Command Code 候选并原样转发；OpenRouter 的 `provider.zdr:true` 只走 OpenRouter。两种隐私机制不互转，也不能通过失败切换把约束交给不支持的供应商。

流在终止事件停止读取，无终止事件、损坏参数及空正文耗尽等不会伪装成功。原生音频、图片、拒绝和服务端工具输出不再按空正文误判。502/504 无法证明上游未执行，现不自动重放；`store:true` 与其他服务端状态或工具执行同样禁止自动重放。

此版本覆盖已开放的对话 HTTP/SSE 端点。独立的文件上传、Batch、语音转写/TTS、视频生成作业、Realtime 和 WebSocket 端点尚未实现；消息内媒体保真不等于这些独立 API 已支持。真实上游验证与离线契约测试应分开记录。

参考：[OpenRouter Messages](https://openrouter.ai/docs/api/api-reference/anthropic-messages/create-messages)、[OpenRouter 多模态](https://openrouter.ai/docs/guides/overview/multimodal/overview)、[DeepSeek Responses](https://api-docs.deepseek.com/guides/responses_api/)、[DeepSeek Messages](https://api-docs.deepseek.com/guides/anthropic_api/)、[Command Code Provider](https://commandcode.ai/docs/provider)。

## v1.34.2：客户端默认值与多轮历史

- 普通 Messages 工具可显式声明 `type:"custom"`、`defer_loading:false`、`allowed_callers:["direct"]`；工具历史的 `caller:{type:"direct"}` 可转换，工具关联和参数保留。延迟加载或服务端调用方仍需原生协议。
- 历史文本的空 `annotations` / `citations` / `logprobs` 不再误拒；含实际引用或概率内容时仍不能静默丢失。
- 空上下文编辑合法。DeepSeek Messages 无推理历史时的 `clear_thinking` 空操作会移除并通过 `X-Prism-Ignored` 披露；真正需要编辑历史时仍不冒充支持。
- 原生 Responses 的 `queued` / `in_progress` 状态保真，不当作空回答失败；独立异步查询/取消端点仍不在本次对话接口范围内。

- Chat 请求的前置校验失败不会被后续消息解码覆盖；损坏的格式约束、未知字段和跨协议 `store:true` 必须显式拒绝。Chat/Responses 的纯文本格式默认值可显式声明。
- 跨协议流必须确认每个工具调用完成；缺少完成证据、截断或不可表示的停止原因返回错误。Messages 上下文窗口耗尽映射为 length/incomplete，绝不伪装正常完成。

本轮规范依据（2026-10-01核对）：[Anthropic工具类型](https://platform.claude.com/docs/en/api/typescript/messages)、[工具默认值](https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-reference)、[停止原因](https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons)、[DeepSeek推理控制](https://api-docs.deepseek.com/guides/thinking_mode/)。

### 输出上限与客户端模型校验

合法的 `length` / `max_tokens` / `incomplete(max_output_tokens)` 结束即使正文为空也保持成功、停止原因和实际用量，不自动换候选或增加输出预算。Claude Code `/model` 使用 `max_tokens=1` 校验可调用性，适用相同规则。没有合法结束原因的空响应仍按失败处理；跨协议推理丢弃仍需显式开启，截断工具仍不得冒充完整调用。

依据：[Anthropic 停止原因](https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons)、[OpenAI 推理预算](https://developers.openai.com/api/docs/guides/reasoning#allocating-space-for-reasoning)。
