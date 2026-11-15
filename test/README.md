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

无设备回归不能代替真实媒体验收。Go 实机入口仅在明确设置 `ASTRA_TEST_DEVICE` 时运行；浏览器端到端入口要求显式 `--adb`。不要因设备在线而自动启用，必须先获得该设备的视频与控制操作授权。

旧 gRPC 适配、protobuf 契约与生成流程已删除；管理接口只使用 HTTP/OpenAPI。

## 本轮本地验收记录

2026-11-15，Linux，`dev/astra`：

- 全仓 `go test ./... -timeout=90s`、`go test -race ./... -timeout=90s`、`go vet ./...`、`go build ./...` 通过。测试显式清除 `ASTRA_TEST_DEVICE`。
- Node SDK 5 项回归通过；`cg-sdk.js`、`app.js` 语法检查通过；Python runner 语法及用例枚举通过。
- 随机凭据、临时端口、独立 loopback 进程下，runner 的 `authentication`、`lifecycle`、`device_management`、`sse`、`authorized_sessions` 五项通过；进程已回收。
- 首次因缺少 `websockets` 跳过的 `browser_lifecycle` 已补跑通过；`connectivity` 在用户授权设备 `127.0.0.1:16448` 上通过。原设备 `127.0.0.1:16384` 拒绝连接，改用设备经用户明确确认。
- Go `TestDeviceSmoke/(silent|no_forwarders)` 通过：122 个视频帧、2 个关键帧、音频接收为零，关键帧请求、控制写入及端口回收通过；未运行有声子用例。
- 真实 Chrome `142.0.7444.175` 视频端到端连续两轮通过：720×1280 H.264 画面，canvas 非空像素与呈现帧检查；BACK/HOME 经 WebRTC DataChannel 发送，设备前台 Activity 改变且解码画面同步变化。最后一轮视频帧由 5→13、17→19，收到音频字节始终为零。
- 页面关闭后设备解除占用，重新签发授权并连接后恢复解码；撤销授权关闭 PeerConnection/DataChannel、解除占用；Release 移除设备并恢复测试前 ADB forward 列表。临时服务和浏览器已回收，未保存设备截图。
- 实测发现并修复授权 WebSocket 未回传协商子协议的问题，已补断言且 `go test -race ./cmd/signaling -timeout=90s` 通过。Playwright 默认 Chromium 缺少 H.264，使用临时解包的官方 Chrome；Android 录屏时窗口焦点可能为 null，使用前台 Activity 加画面变化双重验证控制。
- 音频按用户要求禁用，未测试。触控手势、TURN、Windows 脚本、Android 内运行及人工视觉/音频验收仍未覆盖。
- 未推送、未压缩历史、未创建 PR；这些动作等待另行确认。

## 无音频设备端到端复现

先在隔离 Python 环境安装 `pip install -r test/requirements-e2e.txt`。浏览器必须支持 H.264；通过 `ASTRA_E2E_CHROME` 指向 Chrome 可执行文件。ADB 必须可用，且仅传入已授权设备。

- 跨进程信令：`python test/browser_e2e.py --adb <serial> --phase signaling`
- 真实浏览器视频与控制：`ASTRA_E2E_CHROME=/path/to/google-chrome python test/browser_e2e.py --adb <serial> --phase browser`
- 无音频 Gateway：`ASTRA_TEST_DEVICE=<serial> go test ./agent/internal/gateway -run '^TestDeviceSmoke$/(silent|no_forwarders)$' -v -count=1 -timeout=90s`

脚本构建临时 agentd/signaling，使用随机凭据及 loopback 端口，显式禁用音频，不修改正式配置。它会唤醒设备、打开系统显示设置并发送 BACK/HOME；请先将设备置于无敏感信息的测试状态。不会解除锁屏、安装设备应用或恢复原前台应用。进程与临时服务文件自动回收，Python 环境和浏览器依赖可复用。
