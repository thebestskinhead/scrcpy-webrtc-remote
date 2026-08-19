package agent

import (
	"sync"
	"sync/atomic"

	"scrcpy-webrtc-remote/pkg/config"
)

// ConfigStore 持有全局配置（atomic.Pointer，ReloadConfig 全量替换）与
// 每设备覆盖（PrepareDevice 注入）。ForDevice 返回"全局 + 该设备显式覆盖"
// 的合并快照，供 Controller 在会话创建/信令连接时读取：
//   - ReloadConfig 不中断现有会话；新准备设备 / 新会话自动用新全局。
//   - PrepareDevice 的 device_config 显式字段覆盖全局，未设置字段沿用全局。
type ConfigStore struct {
	global atomic.Pointer[config.AgentConfig]

	mu     sync.RWMutex
	device map[string]*config.DeviceConfig // serial → per-device overrides
}

// NewConfigStore creates a store with the given initial global config
// (may be nil — Init 注入前处于未初始化态)。initial 会被归一并拷贝。
func NewConfigStore(initial *config.AgentConfig) *ConfigStore {
	s := &ConfigStore{device: make(map[string]*config.DeviceConfig)}
	if initial != nil {
		cp := applyDefaults(*initial)
		s.global.Store(&cp)
	}
	return s
}

// SetGlobal atomically replaces the global config (ReloadConfig 全量替换）。
// 入参归一（零值字段补默认）后存指针；调用方不应再修改传入对象。
func (s *ConfigStore) SetGlobal(cfg *config.AgentConfig) {
	cp := applyDefaults(*cfg)
	s.global.Store(&cp)
}

// Global returns the current global config (read-only; do not mutate).
func (s *ConfigStore) Global() *config.AgentConfig {
	return s.global.Load()
}

// SetDeviceOverrides stores per-device overrides for serial.
func (s *ConfigStore) SetDeviceOverrides(serial string, d *config.DeviceConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d == nil {
		delete(s.device, serial)
		return
	}
	cp := *d
	s.device[serial] = &cp
}

// ClearDeviceOverrides removes per-device overrides for serial.
func (s *ConfigStore) ClearDeviceOverrides(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.device, serial)
}

// ForDevice returns a merged copy of the global config with the per-device
// overrides applied. Each call allocates a fresh snapshot — callers may keep
// or discard it freely.
func (s *ConfigStore) ForDevice(serial string) *config.AgentConfig {
	g := s.global.Load()
	if g == nil {
		return nil
	}
	s.mu.RLock()
	over := s.device[serial]
	s.mu.RUnlock()

	cp := *g
	if over != nil {
		cp.Scrcpy = config.MergeDeviceConfig(g.Scrcpy, over)
	}
	return &cp
}

// applyDefaults fills zero-valued fields with their contract defaults so a
// platform config missing optional values behaves like the legacy YAML loader.
func applyDefaults(cfg config.AgentConfig) config.AgentConfig {
	if cfg.SignalingURL == "" {
		cfg.SignalingURL = "ws://127.0.0.1:8080"
	}
	if cfg.ServiceID == "" {
		cfg.ServiceID = "default"
	}
	if cfg.Scrcpy.ServerVersion == "" {
		cfg.Scrcpy.ServerVersion = "4.0"
	}
	if cfg.Scrcpy.JarPath == "" {
		cfg.Scrcpy.JarPath = "./scrcpy-server.jar"
	}
	if cfg.Scrcpy.PortPoolStart == 0 {
		cfg.Scrcpy.PortPoolStart = 30000
	}
	if cfg.Scrcpy.PortPoolSize == 0 {
		cfg.Scrcpy.PortPoolSize = 100
	}
	if cfg.Scrcpy.MaxSize == 0 {
		cfg.Scrcpy.MaxSize = 1920
	}
	if cfg.Scrcpy.VideoBitRate == 0 {
		cfg.Scrcpy.VideoBitRate = 8_000_000
	}
	if cfg.Scrcpy.MinVideoBitRate == 0 {
		cfg.Scrcpy.MinVideoBitRate = 300_000
	}
	if cfg.Scrcpy.ClearBitrate == 0 {
		cfg.Scrcpy.ClearBitrate = 3_000_000
	}
	if cfg.Scrcpy.HDBitrate == 0 {
		cfg.Scrcpy.HDBitrate = 6_000_000
	}
	if cfg.Scrcpy.WarmKeepSeconds == 0 {
		cfg.Scrcpy.WarmKeepSeconds = 300
	}
	if cfg.Scrcpy.AudioBitRate == 0 {
		cfg.Scrcpy.AudioBitRate = 256_000
	}
	if cfg.Scrcpy.VideoCodec == "" {
		cfg.Scrcpy.VideoCodec = "h264"
	}
	if cfg.Scrcpy.AudioCodec == "" {
		cfg.Scrcpy.AudioCodec = "opus"
	}
	if cfg.Scrcpy.VideoKeyframeInterval == 0 {
		cfg.Scrcpy.VideoKeyframeInterval = 20
	}
	if len(cfg.STUNServers) == 0 {
		cfg.STUNServers = []string{"stun:stun.l.google.com:19302"}
	}
	if cfg.QoSIntervalMS == 0 {
		cfg.QoSIntervalMS = 2000
	}
	return cfg
}
