# 实测报告 · v1.4.0

此报告描述交付时实际执行的验证，不代表所有供应商、操作系统或客户端已经验证。

## 1. 环境

| 项目 | 实际环境 |
| --- | --- |
| 系统 | Debian 13 / Linux amd64 |
| Go | go1.23.2 linux/amd64 |
| SQLite | 3.46.1；使用仓库内 CGo 绑定 |
| 前端 | 本项目原生 ES Module / CSS；系统 Chromium + Playwright |
| 构建网络 | `GOPROXY=off`，没有第三方 Go 模块下载 |
| 云端凭证 | 未提供；不宣称真实付费供应商调用已验证 |

## 2. Go 自动化测试

执行并通过：

```bash
GOMAXPROCS=2 GOPROXY=off go test -count=1 -timeout 60s ./...
GOMAXPROCS=2 GOPROXY=off go test -race -p 1 -count=1 -timeout 60s ./...
GOPROXY=off go vet ./...
./scripts/doctor.sh
```

共有 **21 个顶层测试函数**；JSON 测试输出包含 **44 条测试通过记录（含父级测试及子测试）**，失败 0。竞态检测没有报告数据竞争，`go vet` 没有报告问题。

主要覆盖：

| 范围 | 实测内容 |
| --- | --- |
| 管理入口 | `/api.json`、管理员认证、Cookie / CSRF、未知动作、错误格式、只读批量调用拒绝写入 |
| SQLite | 事务、查询、版本冲突、凭证密文保存、配置持久化 |
| 客户端 Key | 创建、权限范围、撤销、列表不返回原始密钥 |
| 协议组合 | Chat、Responses、Messages 原生路径，以及 6 个跨协议方向的 JSON 与 SSE |
| 工具调用 | 调用 ID、函数名、分片参数、工具结果回传、零参数 Anthropic 工具流 |
| 严格转换 | 原生保留不透明字段；跨协议不支持的字段明确拒绝 |
| 流处理 | 完整结束、异常 EOF、无效工具参数、手写 Anthropic 流转换、客户端取消向上游传递 |
| 路由与保护 | 429 备用切换、已输出后禁止换模型、RPM / 并发 / 本地预算预留 |
| 用量与重启 | 缓存 token 算术、未知用量保留、重启恢复在途记录 |
| 目录与计数 | 两套模型目录结构、原生 token 计数、明确标记估算值 |
| 上游 URL | 私有地址和不安全 URL 的基础验证 |

这些测试使用 `httptest` 本地上游、手写协议样例与演示模型，验证项目实现；不能代替真实供应商服务的认证、模型能力、价格或套餐限制测试。

## 3. 实际二进制 HTTP 冒烟测试

执行：

```bash
GOPROXY=off CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' \
  -o prism-gateway ./cmd/gateway
python3 scripts/smoke.py --binary ./prism-gateway
```

脚本创建临时工作目录并真正启动、停止、重启二进制，不污染用户工作空间。以下 8 组检查通过：

1. 内嵌首页、静态资源和单一管理入口。
2. Chat 普通 JSON 与 SSE。
3. Responses 普通 JSON 与 SSE。
4. Messages 普通 JSON 与 SSE。
5. token 计数明确标记为估算。
6. OpenAI 与 Anthropic 各自的模型目录。
7. 重启后配置、凭证和用量记录保持。
8. 已撤销客户端 Key 不再能调用模型。

## 4. WebUI 交互验证

实际渲染交付的 HTML、CSS 和 JS，完成 **33 个交互检查点**，没有捕获到页面 JavaScript 异常。范围包括：全部 11 个页面、登录 / 退出、供应商创建与更新、凭证脱敏、模型创建、路由排序 / 模拟、别名、一次性密钥显示与撤销、三协议调试台、请求明细、设置保存、审计抽屉、命令面板、依赖安全删除、深色主题和 390px 宽移动端无横向溢出。

**浏览器环境限制：** 测试容器的受管 Chromium 禁止直接导航至任意 URL。因此测试采用 `about:blank` 加载项目原始前端资源，Playwright 将前端唯一的 `/api.json` 请求转交给本机 HTTP 桥；桥实际调用运行中的 Go 服务，并保留真实登录 Cookie 与 CSRF 流程。没有通过修改受管策略来绕过限制，也没有伪造 API 响应。

这验证了真实页面 DOM、交互逻辑和真实管理接口的联动，但**不是直接在 HTTP 同源页面中完成的完整浏览器端到端测试**。静态路由、HTTP 响应和安全头另由 Go HTTP 测试及二进制冒烟脚本检查。

附带截图来自上述真实页面渲染，数据为明确标注的本地演示调用，不是云端模型成绩或账单。截图文件位于 `docs/screenshots/`。

## 4.1 v1.1.0 增量验证 (macOS arm64)

| 项目 | 实际环境 |
| --- | --- |
| 系统 | macOS 15 (Darwin 25.5.0) / arm64 |
| Go | go1.25.3 darwin/arm64 |
| SQLite | 3.51.0 (系统 SDK) |

执行并通过：

```bash
CGO_ENABLED=1 go build ./...
CGO_ENABLED=1 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
go vet ./...
python3 scripts/smoke.py --binary ./bin/prism-gateway
```

- 冒烟检查 **10 组全部通过**（v1.0.0 为 8 组，新增 `public health probe` 与
  `config export/import round trip`）。
- 覆盖率：`internal/gateway` 65.8%、`internal/sqlite` 63.2%。
- 新增 Go 测试：healthz 免鉴权/405/不泄露运行时细节；导出不含密钥明文；
  导入往返还原模型与别名并保留库中凭证；版本冲突、未知格式、非法结构被拒；
  导入文件无法伪造 `has_key`；SQLite 多语句 SQL 被拒绝且不产生副作用。

这轮同时补上了 v1.0.0 报告中列为「尚未验证」的 macOS 实机编译与运行。

## 4.2 v1.2.0 增量验证

同一 macOS arm64 环境执行 `go build` / `go test` / `-race` / `go vet` / `smoke.py`，全部通过。

- 冒烟检查 **11 组**（新增 `runtime log level switching`：后台改级别即刻生效、非法值被拒、可改回）。
- 覆盖率：`cmd/gateway` **0% → 21.6%**、`internal/gateway` 66.5%、`internal/sqlite` 63.2%。
- 新增 Go 测试：`checkListen` 八种地址组合、`checkTLS` 四种参数组合、进程锁冲突与释放后重取、
  日志级别与格式运行时切换（含 JSON 行解析与非法值拒绝）、业务校验错误不写日志。
- WebUI 实测（Playwright，同源 HTTP）：登录、设置页两个新下拉渲染与保存落库、
  版本号由 `auth.status` 下发正确显示、0 console error、无横向溢出。

## 4.3 v1.3.0 增量验证

- 进程冒烟 **12 组**（新增 `listen hot switch + remote guard`：后台切换监听后新地址可服务、
  旧地址已关闭、非回环被拒且服务不中断）。
- 新增 **WebUI 浏览器冒烟** `scripts/ui_smoke.py`，**18 项检查**：登录与 shell 渲染、
  11 个页面逐页渲染、命令面板、主题切换、供应商编辑抽屉、设置保存往返、
  390px 无横向溢出、零 console 错误。已纳入 `make check`。
- 前端换行重排后重跑上述 18 项，全部通过，确认重排未改变行为。
- 覆盖率：`internal/gateway` 67.1%、`internal/sqlite` 63.2%、`cmd/gateway` 13.1%
  （`checkListen` 逻辑迁入 `internal/gateway` 并在那里获得更完整的测试，
  cmd 层比例因此下降，但被测逻辑总量增加）。

## 4.4 v1.4.0 增量验证

**Docker（本轮首次实机验证，此前列为未验证）**

```bash
docker build -t prism-gateway:test .      # 构建阶段内含 GOPROXY=off go test ./...
docker run -d -p <port>:8080 prism-gateway:test
```

镜像在 Linux 容器内完成完整 Go 测试后构建成功。运行验证：`/healthz` 返回 200、
Chat / Responses / Messages 三协议各返回 200、Anthropic SSE 收到 `message_stop` 终止帧、
`docker restart` 后配置与模型清单经数据卷完整保留。

**模糊测试**

3 个目标各跑约 160 万次执行，未发现 panic 或死循环：

| 目标 | 覆盖面 |
| --- | --- |
| `FuzzDecodeCanonical` | 三协议请求解码 + 交叉编码回三协议 |
| `FuzzDecodeCompletion` | 三协议响应解码 + 用量提取（断言不出现负数用量） |
| `FuzzReadSSE` | SSE 帧解析（断言帧数据必来自原始文本、帧数不异常） |

**并发压力**

`-race` 下 40 路并发打同一模型，验证同时在飞的上游请求不超过全局上限、
全局与模型级并发计数在结束后完全归零（不归零会让网关逐渐「假满」直到重启）；
另有 30 路并发验证 RPM 限制既不放过也不全拒。

**覆盖率**

| 包 | v1.3.0 | v1.4.0 |
| --- | --- | --- |
| `internal/gateway` | 67.1% | **80.0%** |
| `internal/sqlite` | 63.2% | **77.8%** |
| `internal/webui` | 0% | **63.6%** |
| `cmd/gateway` | 13.1% | **19.0%** |
| 总计 | — | **77.8%** |

测试与模糊测试函数共 53 个。`scripts/cover.sh` 作为门禁纳入 `make check`，
低于阈值即失败。

**本轮修复的缺陷**

会话亲和的 upsert 语句误用了另一张表名（`requests.requests`），整条语句编译失败，
`sessions` 表自 v1.0.0 起一条记录都没写入过，亲和从未生效；错误被 `Exec` 丢弃因此无人察觉。
已修复并补回归测试（首次写入、冲突分支计数累加、不同会话独立成条），
同时给会话、审计、保留清理、Key 使用时间等后台写入补上错误日志。

## 5. 尚未验证 / 不在交付承诺内

- 原生 Windows 构建；Windows 使用 WSL2 路线。
  (macOS arm64 已于 v1.1.0 验证；Docker/Linux 已于 v1.4.0 验证)
- 真实 OpenCode Go / OpenAI / Anthropic 凭证与付费云端调用。
- 最新 Codex / Claude Code 全部功能的实机端到端验证，特别是签名推理、压缩、WebSocket、服务端工具或响应检索工作流。
- 高并发生产压测、长时间稳定性、独立安全审计、所有第三方 SDK 版本。
- 供应商实际余额、固定账期、RPM / TPM 或套餐条款的精确同步。

完整支持边界见 [COMPATIBILITY.md](COMPATIBILITY.md)；安全和部署注意事项见 [SECURITY.md](SECURITY.md)。

## 6. 可复现性

源码包包含 Go 测试、环境检查脚本和实际进程冒烟脚本。UI 交互检查使用了当前容器专用 HTTP 桥，不把该受环境限制的脚本假称为通用浏览器测试套件。开发者可基于项目管理 RPC 和页面选择器，在正常本地浏览器环境补充原生同源端到端测试。

交付源码包另经独立解压、离线 Go 编译、Go 测试和实际二进制冒烟检查后打包发布。
