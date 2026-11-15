# HTTP 管理接入

机器可读契约见 [`api/openapi.yaml`](../api/openapi.yaml)，启动及集成测试见 [`test/README.md`](../test/README.md)。管理 API 替代旧 gRPC，无需生成代码。

## 部署与凭据

`agentd --http-port 17890` 默认仅监听 loopback，`ASTRA_HTTP_PORT` 可覆盖端口。必须设置随机 `ASTRA_MANAGEMENT_TOKEN`，所有 `/v1` 请求（包括健康检查和 SSE）携带 `Authorization: Bearer <管理令牌>`。

独立 signaling 通过 `--management-url http://127.0.0.1:17890` 校验授权，使用同一管理令牌。Agent 与 signaling 另设置相同的 `ASTRA_AGENT_TOKEN`，它必须不同于管理令牌。Agent 凭据用于 `/ws/agent/{service}` 注册；这是受信后端域凭据，不提供逐设备 Agent 身份限制。`GET /api/agents` 要求管理令牌。

浏览器绝不能持有上述后端令牌。平台后端负责用户认证、目标设备访问策略与显式接管授权。浏览器与 relay 只接收短期会话凭据。默认 Origin 同源校验；公网入口使用同源 TLS 网关，跨主机后端链路使用 TLS。不要把管理端口直接暴露到公网。

## 生命周期与错误

正常顺序：`POST /v1/init` → `POST /v1/start` → `POST /v1/devices` → 创建授权 → 浏览器连接。Init 传入 `global_config`，Start/Stop 不要求 JSON 请求体。重复 Start、Stop 幂等；重复 Init 返回 409。Stop 清理设备、授权及配置，随后可重新 Init。

一般响应为 `ok`、`error_code`、可选 `message`，成功码为 `OK`。业务字段直接置于顶层，不存在统一 `data` 包装。Health 返回独立结构；HTTP 200 只表示健康查询成功，未初始化时其 `ok` 为 false。Validate 成功直接返回授权对象。

| HTTP 状态 | 含义 |
| --- | --- |
| 400 | 参数或 JSON 非法 |
| 401 | 管理凭据或会话授权非法 |
| 404 | 设备或活动会话不存在 |
| 409 | 生命周期、设备归属或授权占用冲突 |
| 413 | JSON 请求超过 1 MiB |
| 415 | JSON 路由未使用 `application/json` |
| 500 | 内部操作失败 |

JSON 路由拒绝未知字段和多个 JSON 值。未知路由/不支持方法由标准 HTTP 路由器返回 404/405，不保证 JSON 错误体。客户端应先检查状态及响应类型，再解析。

操作不是后台任务接口，不返回 202/job ID。Stop、Release、Reset 同步执行清理流程。Prepare 成功仅表示登记并启动信令连接，不等于媒体就绪；查询 `GET /v1/devices` 的 `signaling_connected`、`configured`、`busy`、`current_session_id` 确认状态。强制结束媒体会话最多等待十秒，超时后可能已移除管理状态，不能把 API 成功等同于所有外部进程已确认退出。画质接口成功仅代表请求被接收，不承诺编码器已经应用。

## 设备与配置

设备准备请求含 `device_serial` 与 `instance_id`，可附加 `device_config` 和 `connect_params`，字段以 OpenAPI 为准。Release 删除条目；Reset 清理连接及资源但保留未配置条目，再次 Prepare 恢复。两者都会撤销设备授权。

`PUT /v1/config` 为全量配置替换并补默认值，不是 PATCH。设备覆盖继续参与合并；未设置或 null 的指针字段继承全局值，显式 0/false 是覆盖。

- QoS 上报间隔影响后续回调。
- 新媒体会话读取最新配置；scrcpy 配置或 ADB 路径变化时，下次获取会话重建不匹配的暖会话，不改造当前活动媒体会话。
- 信令连接参数在下次连接时读取，不主动踢掉现有连接。
- 端口池范围变化返回 409，需 Stop → Init。

## 会话授权

`POST /v1/sessions` 接收 `device_serial`、可选 `ttl_seconds` 和 `preempt`，返回 `session` 和只在签发时返回的 `token`。TTL 默认或零值为 300 秒，上限 86400 秒。授权是设备预约，不表示 WebRTC 已连接。默认冲突拒绝；只有后端显式设置 `preempt: true` 才替换旧授权并结束旧活动会话。

浏览器连接 `/ws/browser/{service}/{instance}?session_id={session.session_id}`，WebSocket 子协议为 `astra.auth.<token>`。Relay 使用 `/ws/relay/...` 及相同授权，只允许 `query_status`，其 `relay_id` 必须等于授权会话 ID，不能发起抢占。Token 不得放 URL、日志或源码。同一授权的重复 browser/relay 连接会被拒绝，不支持多人观看。

`POST /v1/sessions/validate` 是管理鉴权保护的后端校验端点，输入 `session_id`、`token`、`service_id`、`instance_id`，返回匹配的授权对象；目标不匹配、过期或撤销返回 401。信令校验失败或无法完成续查时关闭连接。独立进程每秒检查，单次超时三秒，撤销传播可能约四秒；不承诺同步全局断开，也不支持多节点租约调度。

`DELETE /v1/sessions/{id}` 幂等撤销授权并关闭存在的活动会话。`POST /v1/sessions/{id}/close` 先撤销授权，再关闭活动会话：没有活动会话可返回 404，因此 404 不代表无副作用。浏览器自然断线不自动撤销预约；平台应主动 DELETE，或等待 TTL 到期。进程内授权不持久化，重启后需重新签发。

## SSE

`GET /v1/events` 使用携带管理 Authorization 的 fetch streaming 或后端 HTTP 流客户端；原生 EventSource 无法直接设置该请求头，不应改成 URL 令牌。

每个 `data:` 帧包含一个事件 JSON：`type`、`timestamp_ms`、`session_id`、`device_serial` 及 `data`。最多 64 个并发订阅，超出返回 429 / `ERR_LIMIT`。每订阅者队列 64，慢消费者仅丢自己的事件；15 秒心跳，写入截止十秒。不支持重放或 Last-Event-ID，断线重连后重新查询设备列表。事件是尽力通知，不可作为唯一事实来源。

## 验收边界

本地回归只使用模拟设备和本地 socket，不验证真实画面、音频、触控或 TURN。不要设置 `ASTRA_TEST_DEVICE`，也不要因设备在线而自动执行实机测试。宿主 ADB 模式与未来 Android 内运行不是同一验收目标。
