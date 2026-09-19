# 客户端接入指南

先在 WebUI 添加真实供应商和模型，再创建客户端 Key。管理令牌、供应商 Key、客户端 Key 三者不能混用。以下客户端示例是配置方法，**不是已完成所有版本/全部模型联合认证的声明**。本地协议测试见 TEST_REPORT.md。

## OpenAI 风格 SDK / 工具

```text
Base URL: http://127.0.0.1:8080/openai/v1
API Key:  prism_sk_...
Model:    WebUI 中启用的模型 ID 或路由 ID
```

Chat Completions 与 Responses 均使用这个 Base URL。客户端请求是 Responses 时，优先选择原生协议为 `responses` 的模型；Chat 请求优先选择 `chat`。不是所有工具都会读取 `OPENAI_BASE_URL` 环境变量，仍需确认工具本身的配置入口。

## Anthropic SDK / Claude Code

```bash
export ANTHROPIC_BASE_URL='http://127.0.0.1:8080/anthropic'
export ANTHROPIC_AUTH_TOKEN='替换为 prism_sk_...'
export ANTHROPIC_MODEL='替换为网关中的固定 Messages 模型 ID'
```

Claude Code 网关说明提供 Base URL 和网关凭证配置方式。Prism 接受 Bearer 或 x-api-key 两种调用凭证。不要同时保留相互冲突的 API Key / Auth Token 环境设置，更不要只改 Base URL 却把原有订阅登录当作 Prism Key。

对于通用 Anthropic SDK，按 SDK 的 api_key 参数填写网关 Key。Base URL **不要自行再加 /v1**，由客户端 Messages 请求追加 `/v1/messages`。特殊客户端若约定 base_url 含 /v1，则应按其拼接规则调整，避免 `/v1/v1/messages`。

Claude Code 的 thinking、beta headers 和工具内容优先走原生 Messages。上游是否支持某个 beta 由该供应商决定。跨协议不保证处理 Claude Code 所有扩展特性；收到 NO_COMPATIBLE_MODEL 时，改用合适的原生模型而不是关闭安全校验。

## Codex 的自定义 Provider

Prism 不读取 TOML。以下片段属于 **Codex 客户端自身的配置**，与网关的 SQLite-only 配置约束没有冲突。先备份客户端原配置，再按它的官方配置方式增加自定义 Provider；不要整份覆盖已有工具设置。

```toml
model_provider = "prism"
model = "your-native-responses-model-id"

[model_providers.prism]
name = "Prism Gateway"
base_url = "http://127.0.0.1:8080/openai/v1"
env_key = "PRISM_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
```

```bash
export PRISM_API_KEY='替换为 prism_sk_...'
```

这里关闭客户端自动重试，是为了初次接入时避免网关和客户端双层重试、半截工具调用重放。不是声称重试永远不可使用。确认任务幂等性及失败恢复方式后再按客户端能力调整。

必须用网关配置的固定 Responses 模型 ID，尤其客户端发送 reasoning、encrypted state、previous_response_id 等原生字段时。不要把 `demo-responses` 当真正代码模型：它只证明协议路径可达。

若客户端主动调用本版本未实现的 `/responses/compact`、WebSocket、响应检索或其他高级端点，会明确失败。本项目不通过伪造响应掩盖这些缺口。升级客户端之前，先用受控任务回归。

## 会话头

自定义客户端可按每次真实对话传一个稳定 `X-Prism-Session`。同一对话内复用，不同对话分开；不要把所有项目共用一个固定“万能 session”。

网关识别常用会话头，向 OpenCode Go 转发稳定的 `x-opencode-session` 并使用自己的 User-Agent。客户端没有 session 时会生成值并通过响应头返回。需要亲和性的自定义客户端应复用它；不复用时下一次被视为新会话。

## 参考

配置字段核对日期：2026-09-19。客户端行为可能变化；下面是官方来源，而非 Prism 的认证说明。

- Claude Code 网关：https://code.claude.com/docs/en/llm-gateway
- Codex 配置参考：https://developers.openai.com/codex/config-reference/
- OpenCode Go 会话与调用要求：https://opencode.ai/docs/go/
