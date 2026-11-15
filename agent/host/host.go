// Package host implements the sidecar lifecycle orchestration: 生命周期
// 状态机（Init/Start/Stop/ReloadConfig）、事件总线和平台 hooks 桥接。
package host

import (
	"sync"
	"sync/atomic"
	"time"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
	"scrcpy-webrtc-remote/pkg/sessionauth"
)

// 生命周期状态机（PLATFORM-API.md §3.1 权威定义）：
//
//	UNINITIALIZED --Init--> INITIALIZED --Start--> RUNNING
//	INITIALIZED / RUNNING --Stop--> UNINITIALIZED
const (
	stateUninitialized int32 = 0
	stateInitialized   int32 = 1
	stateRunning       int32 = 2
)

// EventHub 是有界队列 + fan-out 广播（PLATFORM-API.md §6.2/§6.3）：
// 慢订阅者只丢自己的事件，不阻塞主循环与其它订阅者；断线不重放，
// 平台以 ListDevices 补偿。
type EventHub struct {
	mu   sync.RWMutex
	subs map[chan *Event]struct{}
	q    int
}

// NewEventHub creates an event hub with per-subscriber bounded queues.
func NewEventHub(queueSize int) *EventHub {
	if queueSize <= 0 {
		queueSize = 64
	}
	return &EventHub{subs: make(map[chan *Event]struct{}), q: queueSize}
}

// Publish broadcasts an event to all subscribers (non-blocking; full queues
// drop their own events only).
func (h *EventHub) Publish(ev *Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// slow subscriber drops its own events; never block publishers
		}
	}
}

// Subscribe registers a subscriber. The returned cancel closes the channel
// and unregisters it.
func (h *EventHub) Subscribe() (chan *Event, func()) {
	ch := make(chan *Event, h.q)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			close(ch)
			h.mu.Unlock()
		})
	}
}

// hookBridge 是 PlatformHooks 的实现，把 agent 事件转成 proto AgentEvent
// 后 fan-out 到所有 StreamEvents 订阅者。
type hookBridge struct {
	hub     *EventHub
	store   *agent.ConfigStore
	onError func(code, msg string)

	qosMu sync.Mutex
	qosAt map[string]time.Time // sessionID → last publish (QoS 节流)
}

func newHookBridge(hub *EventHub, store *agent.ConfigStore, onError func(code, msg string)) *hookBridge {
	return &hookBridge{hub: hub, store: store, onError: onError, qosAt: make(map[string]time.Time)}
}

// newEvent 构造带公共字段（serial/instance/session/timestamp）的 AgentEvent。
func (b *hookBridge) newEvent(serial, instanceID, sessionID string) *Event {
	return &Event{
		DeviceSerial: serial,
		InstanceID:   instanceID,
		SessionID:    sessionID,
		TimestampMS:  time.Now().UnixMilli(),
	}
}

func (b *hookBridge) OnDeviceStatus(serial, instanceID string, busy bool) {
	ev := b.newEvent(serial, instanceID, "")
	ev.Type = "device_status"
	ev.Data.Busy = busy
	b.hub.Publish(ev)
}

func (b *hookBridge) OnSessionStarted(serial, instanceID, sessionID string) {
	ev := b.newEvent(serial, instanceID, sessionID)
	ev.Type = "session_started"
	b.hub.Publish(ev)
}

func (b *hookBridge) OnSessionStopped(serial, instanceID, sessionID, reason string) {
	ev := b.newEvent(serial, instanceID, sessionID)
	ev.Type = "session_stopped"
	ev.Data.Reason = reason
	b.qosMu.Lock()
	delete(b.qosAt, sessionID)
	b.qosMu.Unlock()
	b.hub.Publish(ev)
}

func (b *hookBridge) OnStreamDead(serial, instanceID, sessionID string) {
	ev := b.newEvent(serial, instanceID, sessionID)
	ev.Type = "stream_dead"
	b.hub.Publish(ev)
}

func (b *hookBridge) OnQoS(q agent.QoSReport) {
	// 按全局 qos_interval_ms 节流，避免高频 decoder_status 风暴。
	interval := 2 * time.Second
	if g := b.store.Global(); g != nil && g.QoSIntervalMS > 0 {
		interval = time.Duration(g.QoSIntervalMS) * time.Millisecond
	}
	now := time.Now()
	b.qosMu.Lock()
	if last, ok := b.qosAt[q.SessionID]; ok && now.Sub(last) < interval {
		b.qosMu.Unlock()
		return
	}
	b.qosAt[q.SessionID] = now
	b.qosMu.Unlock()

	ev := b.newEvent(q.Serial, q.InstanceID, q.SessionID)
	ev.Type = "qos"
	ev.Data.QoS = &QoSData{
		LossRate: q.LossRate, RTTMs: q.RTTMs, Bitrate: q.Bitrate,
		FPS: q.FPS, ResolutionX: q.ResX, ResolutionY: q.ResY,
	}
	b.hub.Publish(ev)
}

func (b *hookBridge) OnError(serial, instanceID, code, msg string) {
	if b.onError != nil {
		b.onError(code, msg)
	}
	ev := b.newEvent(serial, instanceID, "")
	ev.Type = "agent_error"
	ev.Data.Code, ev.Data.Message = code, msg
	b.hub.Publish(ev)
}

func (b *hookBridge) OnDeviceReleased(serial, instanceID, reason string) {
	ev := b.newEvent(serial, instanceID, "")
	ev.Type = "device_released"
	ev.Data.Reason = reason
	b.hub.Publish(ev)
}

func (b *hookBridge) OnDeviceReset(serial, instanceID, reason string) {
	ev := b.newEvent(serial, instanceID, "")
	ev.Type = "device_reset"
	ev.Data.Reason = reason
	b.hub.Publish(ev)
}

// Host 是 sidecar 生命周期编排：持有 ConfigStore / DeviceManager / EventHub，
// 实现 Init/Start/Stop/ReloadConfig 状态机与健康查询。
type Host struct {
	mu    sync.Mutex
	state int32

	store  *agent.ConfigStore
	hub    *EventHub
	bridge *hookBridge
	dm     *agent.DeviceManager
	grants *sessionauth.Store

	// bootstrapped 表示进程以 --bootstrap 启动（相当于预 Init，但 Init 仍可
	// 调用并全量覆盖，见 PLATFORM-API.md §8.3 决策#6）。
	bootstrapped bool

	// onStop 在 Stop 完成后调用（--one-shot 场景自退）。
	onStop func()

	port      int
	version   string
	startedAt time.Time
	lastError atomic.Value // string
}

// New creates a host bound to the given gRPC listen port. store 可先用
// bootstrap 配置初始化（nil 表示未初始化）。
func New(store *agent.ConfigStore, port int, version string) *Host {
	h := &Host{
		store:     store,
		hub:       NewEventHub(64),
		grants:    sessionauth.NewStore(),
		port:      port,
		version:   version,
		startedAt: time.Now(),
	}
	h.lastError.Store("")
	h.bridge = newHookBridge(h.hub, store, func(code, msg string) {
		h.lastError.Store(code + ": " + msg)
	})
	return h
}

// MarkBootstrapped 使 bootstrap 配置生效：状态进入 INITIALIZED（可立即
// PrepareDevice，无需 Init），但保留 Init 覆盖语义（bootstrapped 标记）。
func (h *Host) MarkBootstrapped() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if atomic.LoadInt32(&h.state) != stateUninitialized || h.store.Global() == nil {
		return
	}
	cfg := h.store.Global()
	pool := agent.NewSharedPortPool(cfg.Scrcpy.PortPoolStart, cfg.Scrcpy.PortPoolSize)
	h.dm = agent.NewDeviceManager(h.store, pool, h.bridge)
	h.bootstrapped = true
	atomic.StoreInt32(&h.state, stateInitialized)
	logger.Info("host bootstrapped", "signaling", cfg.SignalingURL)
}

// OnStop registers a callback invoked after Stop completes (--one-shot).
func (h *Host) OnStop(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onStop = fn
}

// EventHub exposes the event hub for the gRPC StreamEvents server.
func (h *Host) EventHub() *EventHub { return h.hub }

// DeviceManager exposes the device manager (nil before Init).
func (h *Host) DeviceManager() *agent.DeviceManager {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dm
}

// State returns the current lifecycle state.
func (h *Host) State() int32 {
	return atomic.LoadInt32(&h.state)
}

// Init 注入全局配置（仅 UNINITIALIZED 合法；重复 → ERR_ALREADY_INIT）。
// bootstrap 启动后允许 Init 覆盖（全量替换，见 PLATFORM-API.md §8.3）。
func (h *Host) Init(cfg *config.AgentConfig) *InitResponse {
	h.mu.Lock()
	defer h.mu.Unlock()

	if atomic.LoadInt32(&h.state) != stateUninitialized && !h.bootstrapped {
		return &InitResponse{Response: *errResp("ERR_ALREADY_INIT", "already initialized; call Stop first")}
	}
	if err := validateConfig(cfg); err != nil {
		return &InitResponse{Response: *errResp("ERR_INVALID_ARG", err.Error())}
	}
	if h.bootstrapped && h.dm != nil {
		if len(h.dm.List()) != 0 {
			return &InitResponse{Response: *errResp("ERR_CONFLICT", "stop prepared devices before replacing bootstrap configuration")}
		}
		h.dm.StopAll()
		h.dm = nil
	}

	h.store.SetGlobal(cfg)
	cfg = h.store.Global()
	// 端口池按全局 scrcpy 配置创建（共享给所有设备）。
	if h.dm == nil {
		pool := agent.NewSharedPortPool(cfg.Scrcpy.PortPoolStart, cfg.Scrcpy.PortPoolSize)
		h.dm = agent.NewDeviceManager(h.store, pool, h.bridge)
	}
	h.bootstrapped = false
	atomic.StoreInt32(&h.state, stateInitialized)

	logger.Info("host initialized",
		"signaling", cfg.SignalingURL, "service", cfg.ServiceID,
		"actual_port", h.port)
	return &InitResponse{Response: *okResp(), ActualPort: h.port}
}

// Start 进入运行态（INITIALIZED → RUNNING）。幂等。
func (h *Host) Start() *Response {
	h.mu.Lock()
	defer h.mu.Unlock()

	switch atomic.LoadInt32(&h.state) {
	case stateUninitialized:
		return errResp("ERR_NOT_INIT", "not initialized")
	case stateRunning:
		return okResp()
	}
	atomic.StoreInt32(&h.state, stateRunning)
	logger.Info("host started")
	return okResp()
}

// Stop 优雅停止：回收所有设备、断开会话与信令、状态回 UNINITIALIZED。幂等。
func (h *Host) Stop() *Response {
	h.mu.Lock()
	if h.State() == stateUninitialized {
		h.mu.Unlock()
		return okResp()
	}
	h.grants.RevokeAll()
	if h.dm != nil {
		h.dm.StopAll()
	}

	atomic.StoreInt32(&h.state, stateUninitialized)
	h.dm = nil
	h.bootstrapped = false
	onStop := h.onStop
	h.mu.Unlock()

	logger.Info("host stopped")
	if onStop != nil {
		onStop()
	}
	return okResp()
}

// ReloadConfig 全量替换全局配置（不中断现有会话；新准备设备生效）。
func (h *Host) ReloadConfig(cfg *config.AgentConfig) *Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	if atomic.LoadInt32(&h.state) == stateUninitialized {
		return errResp("ERR_NOT_INIT", "not initialized")
	}
	if err := validateConfig(cfg); err != nil {
		return errResp("ERR_INVALID_ARG", err.Error())
	}
	next := agent.NewConfigStore(cfg).Global()
	previous := h.store.Global()
	if next.Scrcpy.PortPoolStart != previous.Scrcpy.PortPoolStart || next.Scrcpy.PortPoolSize != previous.Scrcpy.PortPoolSize {
		return errResp("ERR_CONFLICT", "port pool changes require Stop and Init")
	}
	h.store.SetGlobal(cfg)
	logger.Info("config reloaded")
	return okResp()
}

// Health 探活。未初始化时 ok=false 且 last_error=ERR_NOT_INIT（L02）。
func (h *Host) Health() *HealthResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	uptime := int64(time.Since(h.startedAt).Seconds())
	lastErr, _ := h.lastError.Load().(string)

	if atomic.LoadInt32(&h.state) == stateUninitialized {
		return &HealthResponse{
			Ok: false, Version: h.version, LastError: "ERR_NOT_INIT",
		}
	}

	dm := h.dm
	var devices []agent.DeviceView
	if dm != nil {
		devices = dm.List()
	}
	active := 0
	for _, d := range devices {
		if d.Busy {
			active++
		}
	}
	return &HealthResponse{
		Ok:             true,
		Version:        h.version,
		DeviceCount:    len(devices),
		ActiveSessions: active,
		UptimeSeconds:  uptime,
		LastError:      lastErr,
	}
}

func okResp() *Response {
	return &Response{Ok: true, ErrorCode: "OK"}
}

func errResp(code, msg string) *Response {
	return &Response{Ok: false, ErrorCode: code, Message: msg}
}
