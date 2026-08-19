package agent

import (
	"fmt"
	"sync"

	"scrcpy-webrtc-remote/agent/internal/portpool"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
)

// DeviceView 是 ListDevices 的可观察设备状态快照（grpc 层转 proto）。
type DeviceView struct {
	InstanceID         string
	Serial             string
	Busy               bool
	CurrentSessionID   string
	ConnectedSeconds   int32
	SignalingConnected bool
	Configured         bool
}

// deviceEntry 表示一个已登记设备。configured=false（ResetDevice 后）表示
// 设备仍在管理列表但无 per-device 配置、无运行中 Controller。
type deviceEntry struct {
	serial     string
	instanceID string
	ctrl       *Controller // nil = 被 Reset 清除配置后待重建
	configured bool
}

// DeviceManager 管理 serial → deviceEntry 映射，Prepare/Release/Reset 全程
// 持锁保证原子性（杜绝 bound 重建竞态，见 PLATFORM-API.md §3.10）。
type DeviceManager struct {
	mu      sync.Mutex
	devices map[string]*deviceEntry

	store *ConfigStore
	pool  *portpool.Pool
	hooks PlatformHooks
}

// NewDeviceManager creates a device manager.
func NewDeviceManager(store *ConfigStore, pool *portpool.Pool, hooks PlatformHooks) *DeviceManager {
	return &DeviceManager{
		devices: make(map[string]*deviceEntry),
		store:   store,
		pool:    pool,
		hooks:   hooks,
	}
}

// Prepare registers (or smoothly updates) a device with per-device config.
// 锁内完成配置覆盖与 Controller 创建，锁外启动信令连接 goroutine。
func (dm *DeviceManager) Prepare(serial, instanceID string, devCfg *config.DeviceConfig, cp ConnectParams) error {
	dm.mu.Lock()
	entry := dm.devices[serial]
	if entry == nil {
		entry = &deviceEntry{serial: serial}
		dm.devices[serial] = entry
	}
	entry.instanceID = instanceID
	entry.configured = true
	dm.store.SetDeviceOverrides(serial, devCfg)

	// 已存在且运行中 → 平滑更新（配置下次会话生效，不中断现有会话）。
	// 被 Reset 清除过（ctrl==nil）→ 重建 Controller。
	if entry.ctrl == nil {
		entry.ctrl = newController(dm.store, config.InstanceConfig{
			InstanceID: instanceID, DeviceSerial: serial,
		}, dm.pool, dm.hooks, cp)
		go entry.ctrl.Run()
	} else {
		entry.ctrl.SetConnectParams(cp)
	}
	dm.mu.Unlock()

	if dm.hooks != nil {
		dm.hooks.OnDeviceStatus(serial, instanceID, false)
	}
	logger.Info("device prepared", "serial", serial, "instance", instanceID)
	return nil
}

// Release removes the device entirely: stops its controller (drops the
// signaling connection and all sessions) and clears per-device config.
// reason 透传到 DeviceReleased 事件（平台传入的回收原因）。
// 注意：先取 entry.ctrl 引用再解锁（delete 之后 entry 仍可访问其字段），
// 锁外调用 ctrl.Stop()，避免持锁期间 stop 回调死锁。
func (dm *DeviceManager) Release(serial, reason string) error {
	dm.mu.Lock()
	entry, ok := dm.devices[serial]
	if !ok {
		dm.mu.Unlock()
		return fmt.Errorf("device not found")
	}
	delete(dm.devices, serial)
	ctrl := entry.ctrl
	dm.mu.Unlock()

	dm.store.ClearDeviceOverrides(serial)
	if ctrl != nil {
		ctrl.Stop()
	}
	if dm.hooks != nil {
		dm.hooks.OnDeviceReleased(serial, entry.instanceID, reason)
	}
	logger.Info("device released", "serial", serial, "reason", reason)
	return nil
}

// Reset clears the device's config and disconnects it, but keeps the device
// in the management list (configured=false) until the next PrepareDevice
// re-injects config (见 PLATFORM-API.md §3.10 保留语义）。
// reason 透传到 DeviceReset 事件（平台传入的清除原因）。
//
// 并发防护：锁内置 entry.ctrl=nil + 锁外 ctrl.Stop() 关闭信令 WS，
// 使该 Controller 不再处理新 bound（旧会话被终止），无需额外"清除中"标志。
func (dm *DeviceManager) Reset(serial, reason string) error {
	dm.mu.Lock()
	entry, ok := dm.devices[serial]
	if !ok {
		dm.mu.Unlock()
		return fmt.Errorf("device not found")
	}
	entry.configured = false
	ctrl := entry.ctrl
	entry.ctrl = nil
	dm.mu.Unlock()

	dm.store.ClearDeviceOverrides(serial)
	if ctrl != nil {
		ctrl.Stop()
	}
	if dm.hooks != nil {
		dm.hooks.OnDeviceReset(serial, entry.instanceID, reason)
	}
	logger.Info("device reset (config cleared, entry retained)", "serial", serial, "reason", reason)
	return nil
}

// List returns a snapshot of all managed devices (including reset-but-retained).
func (dm *DeviceManager) List() []DeviceView {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	views := make([]DeviceView, 0, len(dm.devices))
	for _, e := range dm.devices {
		v := DeviceView{
			InstanceID: e.instanceID,
			Serial:     e.serial,
			Configured: e.configured,
		}
		if e.ctrl != nil {
			v.Busy = e.ctrl.Busy()
			v.CurrentSessionID = e.ctrl.CurrentSessionID()
			v.ConnectedSeconds = e.ctrl.ConnectedSeconds()
			v.SignalingConnected = e.ctrl.SignalingConnected()
		}
		views = append(views, v)
	}
	return views
}

// Get returns the controller for a device (nil if not prepared or reset).
func (dm *DeviceManager) Get(serial string) *Controller {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if e, ok := dm.devices[serial]; ok {
		return e.ctrl
	}
	return nil
}

// GetBySessionID finds the controller owning the given session.
func (dm *DeviceManager) GetBySessionID(sessionID string) *Controller {
	if sessionID == "" {
		return nil
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	for _, e := range dm.devices {
		if e.ctrl != nil && e.ctrl.CurrentSessionID() == sessionID {
			return e.ctrl
		}
	}
	return nil
}

// StopAll tears down every device (used by Host.Stop).
func (dm *DeviceManager) StopAll() {
	dm.mu.Lock()
	ctrls := make([]*Controller, 0, len(dm.devices))
	for _, e := range dm.devices {
		if e.ctrl != nil {
			ctrls = append(ctrls, e.ctrl)
			e.ctrl = nil
		}
		e.configured = false
		dm.store.ClearDeviceOverrides(e.serial)
	}
	dm.mu.Unlock()

	for _, c := range ctrls {
		c.Stop()
	}
	logger.Info("all devices stopped")
}
