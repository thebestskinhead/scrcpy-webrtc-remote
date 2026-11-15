# HTTP 管理与 SDK 测试

接口契约以 `api/openapi.yaml` 为准。`runner.py` 使用 Python 标准库调用 HTTP/SSE，只有浏览器信令用例需要 `requirements.txt` 中的 `websockets`，无需生成 stub。

## 本地回归

- `go test -race ./... -timeout=90s`：核心生命周期、HTTP、授权与本地假 Agent WebSocket 集成测试。确保未设置 `ASTRA_TEST_DEVICE`。
- `node --test test/sdk.test.cjs`：事件隔离、输入解绑、异步取消和授权失败关闭。
- `python test/runner.py --list`：列出外部服务集成用例。

## HTTP 集成

在三个终端设置相同的、随机生成的 `ASTRA_MANAGEMENT_TOKEN`。另为 agentd 和 signaling 设置相同的 `ASTRA_AGENT_TOKEN`，它必须与管理令牌不同。两种令牌仅供受信后端进程使用，不得放入浏览器、URL 或源码。浏览器默认只允许同源连接；跨主机部署请通过同源 TLS 网关代理信令。

1. 启动管理服务：`go run ./cmd/agentd --http-port 17890`。
2. 启动信令：`go run ./cmd/signaling -c config/signaling.yaml --management-url http://127.0.0.1:17890`。
3. 安装 Python 依赖后运行：`python test/runner.py --http http://127.0.0.1:17890 --signaling ws://127.0.0.1:8080`。

**仅使用独立测试实例**：runner 会执行 Stop/Init，并回收实例内设备。不要指向生产服务或正在运行 agentdrv 的实例。

默认用例不发送 SDP offer、不调用 ADB；测试 HTTP 状态机、设备注册管理、SSE、授权冲突/抢占/撤销及浏览器 bound/unbound。`--adb <serial>` 仅增加指定设备的信令注册检查，不代表音视频或控制验收。单用例示例：`python test/runner.py --case authorized_sessions`。

## 平台 driver 与浏览器

`go run ./test/agentdrv -c config/agent.yaml --http http://127.0.0.1:17890 --events` 从平台 YAML 注入配置并准备设备；可用 `--agentd <path>` 托管子进程，只有显式添加 `--connect-adb` 才执行 ADB 自动连接。退出时调用 HTTP Stop。

页面需要平台后端签发的授权。由平台实现 `window.AstraAuthorizeSession({service_id, instance_id, preempt})`，返回 `POST /v1/sessions` 的响应；静态测试也可在连接前提供 `window.AstraSessionAuthorization`。服务端映射用户与设备并执行访问控制，不能把管理令牌下发给页面。

授权使用 `session.session_id` 查询参数和 `astra.auth.<token>` WebSocket 子协议。默认拒绝占用；显式抢占必须由平台后端调用签发接口并设置 `preempt: true`。独立信令进程每秒重新校验，校验超时为三秒，撤销生效可能延迟约四秒；过期按连接定时器关闭。

## 验收边界

本地测试不能代替真实音视频、浏览器解码、触控或音频可听性验收。实机入口仅在明确设置 `ASTRA_TEST_DEVICE` 时运行，不应因 ADB 在线而自动开启。

旧 gRPC 适配、protobuf 契约与生成流程已删除；管理接口只使用 HTTP/OpenAPI。

## 本轮本地验收记录

2026-11-15，Linux，`dev/astra`：

- 全仓 `go test ./... -timeout=90s`、`go test -race ./... -timeout=90s`、`go vet ./...`、`go build ./...` 通过。测试显式清除 `ASTRA_TEST_DEVICE`。
- Node SDK 5 项回归通过；`cg-sdk.js`、`app.js` 语法检查通过；Python runner 语法及用例枚举通过。
- 随机凭据、临时端口、独立 loopback 进程下，runner 的 `authentication`、`lifecycle`、`device_management`、`sse`、`authorized_sessions` 五项通过；进程已回收。
- 完整 Python 集成首次因缺少 `websockets` 模块而未启动；`browser_lifecycle` 未执行。Go 的本地 WebSocket 授权/撤销回归通过，但不替代该 Python 跨进程用例。
- 未执行 `connectivity`、ADB、真实媒体、TURN 或 Android 内运行测试。PowerShell 脚本未在 Windows 执行。
- 未推送、未压缩历史、未创建 PR；这些动作等待另行确认。
