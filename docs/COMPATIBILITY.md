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


## 被接受但不转发的请求字段

以下字段不影响模型看到的内容，跨协议时也没有对应概念。网关接受它们，
但不会转发给上游，并在 `X-Prism-Ignored` 响应头里列出实际忽略了哪些：

| 字段 | 说明 |
| --- | --- |
| `metadata` | Anthropic 规范中它不参与生成 |
| `context_management.edits.clear_thinking_20251015` | 无推理历史时接受省略 keep、keep="all"、keep={type:"all"}、keep={type:"thinking_turns",value:正整数} 并通过 X-Prism-Ignored 披露；其他服务端上下文编辑不转换 |
| `output_config.effort` | 仅努力程度偏好；`format` 单独转换并保留约束 |
| `cache_control` | 提示缓存标记，只影响成本不影响内容 |
| `service_tier` / `store` / `user` | 供应商侧调度、留存与调用方标识 |

`thinking` 不在此列——它会真正改变模型行为。开启模型的 `drop_reasoning` 后
该字段会被忽略（因为推理内容反正也要丢弃），否则请求会被拒绝并点名该字段。

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
