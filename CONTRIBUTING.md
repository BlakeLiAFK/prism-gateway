# 参与开发

## 环境

Go 1.23+、C 编译器、SQLite 开发库。没有第三方 Go 模块，不需要 `go mod download`，也不需要 npm。

```bash
./scripts/doctor.sh
make hooks          # 安装 pre-commit（格式）与 pre-push（完整质量门）
make build
```

## 质量门

本项目不使用托管 CI，`make check` 就是唯一的门。它在 `git push` 时由钩子强制执行：

```bash
make check          # vet + staticcheck + 覆盖率门禁 + WebUI 浏览器冒烟
make test           # 仅单元与集成测试
make race           # 竞态检测
make cover          # 覆盖率阈值检查
make ui             # WebUI 冒烟（需 playwright，缺失则跳过）
python3 scripts/smoke.py --binary ./bin/prism-gateway   # 真实进程冒烟
```

覆盖率阈值写在 `scripts/cover.sh`。降低阈值需要在提交信息里说明理由。

## 模糊测试

协议转换与 SSE 解析处理的是外部输入，改动这些文件后请跑一轮：

```bash
go test -run=x -fuzz=FuzzDecodeCanonical -fuzztime=60s ./internal/gateway/
go test -run=x -fuzz=FuzzDecodeCompletion -fuzztime=60s ./internal/gateway/
go test -run=x -fuzz=FuzzReadSSE -fuzztime=60s ./internal/gateway/
```

发现的失败语料会写入 `testdata/fuzz/`，请连同修复一起提交。

## 改前端

前端是内嵌的 ES Module，Go 编译期检查不到它。四个模块：`core.js`（状态与格式化，
零业务依赖）、`ui.js`（通用构件）、`views.js`（页面与编辑抽屉）、`app.js`（外壳与路由）。

跨模块共享的可变状态一律挂在 `core.js` 的 `state` 对象上——ESM 不允许跨模块给
顶层 `let` 赋值。

直接改 `internal/webui/dist/assets/`，重新 `go build`，然后**必须**跑 `make ui`——它会真的启动二进制、用浏览器走完
11 个页面与关键交互，并断言零 console 错误。

## 不变量

`AGENTS.md` 列出了必须保持的约束。其中最容易被无意破坏的几条：

- 管理操作只走 `POST /api.json`，不新增 REST 管理路径
- 运行配置存 SQLite 且后台可改，不为它新增启动参数
- 启动横幅是终端 UI，管理员令牌不得进入日志流
- 跨协议转换只做明确实现的子集，不支持的能力显式报错而非静默丢弃
- 流已开始后不做故障切换

## 提交

- 提交信息用英文，说明「为什么」而不只是「做了什么」
- 每个修复都要带一个失败即报错的测试
- 不提交 `data/`、`dist/`、密钥或任何真实凭证
