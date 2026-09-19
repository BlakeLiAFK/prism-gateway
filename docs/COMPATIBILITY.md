# 协议兼容边界

## 路由原则

客户端协议与所选模型原生协议相同：原生转发。改写 model、使用供应商凭证、加入自己的 User-Agent、保留允许的会话与版本头；高级请求字段和模型响应不经过公共 IR，因此不会为了统一而丢失厂商专属状态。网关仍检查体积、权限和输出上限。

协议不相同：先解析显式白名单，然后转换公共中间表示。无法转换则拒绝这个候选，尝试别的兼容候选；没有合适候选返回 400，而不是静默删除字段。响应出现未支持的内容时也不伪装转换成功。

| 来源 → 目标 | 普通 JSON | SSE | 覆盖范围 |
| --- | --- | --- | --- |
| Chat → Messages / Responses | 已实现并测 | 已实现并测 | 文本、函数工具、参数增量、结果 |
| Messages → Chat / Responses | 已实现并测 | 已实现并测 | system、文本、tool_use / tool_result |
| Responses → Chat / Messages | 已实现并测 | 已实现并测 | 普通 input、文本消息、function_call / output |
| 三种协议 → 同协议 | 原生转发并测 | 原生转发并测 | 保留原生字段，但仅提供列出的 HTTP 端点 |

常用图片 URL / data URI 引用可转换，前提是模型手动声明支持图片，且目标协议能无损表示。不由网关下载任意图片 URL。图片不是该交付的真实模型识别测试项目。

## 不做“假兼容”

跨协议不支持 Thinking / adaptive thinking、思考签名、redacted thinking、OpenAI reasoning / encrypted reasoning 项、任意 PDF/音频/视频、多模态服务端文件、内建 web search / code interpreter / computer-use tools、store=true / previous_response_id 等服务端状态、批量多候选、任意结构化输出约束、未映射 beta 字段。

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
