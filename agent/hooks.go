package agent

// QoSReport 是平台可观察的会话网络/编码快照。数据来自浏览器
// decoder_status 上报（lossRate/rttMs）与编码器当前码率；真实数据
// 依赖浏览器端媒体链路，见 PLATFORM-API.md §11.2。
type QoSReport struct {
	SessionID  string
	Serial     string
	InstanceID string
	LossRate   float64
	RTTMs      float64
	Bitrate    int
	FPS        float64
	ResX, ResY int
}

// PlatformHooks 是 agent 向平台上报生命周期/事件/QoS/错误的扩展点。
// nil 即禁用（沿用 debughooks 的 nil 接口范式）。sidecar 注入 host 实现，
// 经 eventHub fan-out 到所有 StreamEvents 订阅者；DeviceManager 侧
// （PrepareDevice）默认注入 host 的 hookBridge。
//
// 事件时序（与 PLATFORM-API.md §5 一致）：
//
//	PrepareDevice          → OnDeviceStatus(busy=false)（由 DeviceManager 发出）
//	bound（浏览器连 WS）    → OnSessionStarted
//	peer connected         → OnDeviceStatus(busy=true)
//	unbound / preempt      → OnSessionStopped(reason) + OnDeviceStatus(busy=false)
//	scrcpy 中途死亡        → OnStreamDead
//	信令断开 / 会话启动失败  → OnError(code)
type PlatformHooks interface {
	// OnDeviceStatus 设备 busy 状态变化（busy=true 表示 WebRTC 连接建立）。
	OnDeviceStatus(serial, instanceID string, busy bool)

	// OnSessionStarted bound 后会话上下文创建。
	OnSessionStarted(serial, instanceID, sessionID string)

	// OnSessionStopped 会话结束。reason ∈ user_unbound / forced / agent_stop …
	OnSessionStopped(serial, instanceID, sessionID, reason string)

	// OnStreamDead scrcpy server 中途死亡（浏览器侧 stream_dead）。
	OnStreamDead(serial, instanceID, sessionID string)

	// OnQoS 周期性 QoS 快照（节流由实现侧按 qos_interval_ms 采样）。
	OnQoS(q QoSReport)

	// OnError 异步错误上报（ERR_SIGNALING_DISCONNECTED / ERR_ACQUIRE_FAILED …），
	// 仅经 AgentError 事件，不作为 RPC 返回码（见 PLATFORM-API.md §7）。
	OnError(serial, instanceID, code, msg string)

	// OnDeviceReleased 设备被 ReleaseDevice 回收（reason 为平台传入的回收原因）。
	OnDeviceReleased(serial, instanceID, reason string)

	// OnDeviceReset 设备配置被 ResetDevice 清除（reason 为平台传入的清除原因）。
	OnDeviceReset(serial, instanceID, reason string)
}
