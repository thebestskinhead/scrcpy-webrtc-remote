// Package config loads the YAML configuration for the signaling server
// and the desktop agent.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ScrcpyConfig holds scrcpy server / encoding settings (shared by all
// device instances of one agent).
type ScrcpyConfig struct {
	ServerVersion        string `yaml:"server_version"`
	JarPath              string `yaml:"jar_path"`
	PortPoolStart        int    `yaml:"port_pool_start"`
	PortPoolSize         int    `yaml:"port_pool_size"`
	MaxSize              int    `yaml:"max_size"`
	VideoBitRate         int    `yaml:"video_bit_rate"`
	MinVideoBitRate      int    `yaml:"min_video_bit_rate"`
	// ClearBitrate / HDBitrate are the "清晰" / "高清" quality tiers.
	// "原画" uses VideoBitRate. Resolution is fixed (MaxSize); quality
	// switching only changes the encoder bitrate.
	ClearBitrate int `yaml:"clear_bitrate"`
	HDBitrate    int `yaml:"hd_bitrate"`
	// WarmKeepSeconds keeps the scrcpy server alive after the browser
	// disconnects, so a reconnect within this window skips the cold start.
	WarmKeepSeconds int `yaml:"warm_keep_seconds"`
	AudioBitRate    int    `yaml:"audio_bit_rate"`
	VideoCodec      string `yaml:"video_codec"`
	AudioCodec      string `yaml:"audio_codec"`
	AudioEncoder    string `yaml:"audio_encoder"`
	AudioEnabled    bool   `yaml:"audio_enabled"`
	PowerOn         bool   `yaml:"power_on"`
	StayAwake       bool   `yaml:"stay_awake"`
	// VideoKeyframeInterval sets the IDR/keyframe interval in seconds passed to
	// the Android encoder via video_codec_options=i-frame-interval=N.
	// Defaults to 2 (2-second GOPs). The scrcpy default is ~20 s (600 frames @
	// 30 fps), which produces large IDR bursts that can freeze browser decoders.
	VideoKeyframeInterval int `yaml:"video_keyframe_interval"`
	// VideoMaxFPS caps the encoder output frame rate (scrcpy max_fps param).
	// Without it, emulator/software encoders vary wildly (10-55 fps in MuMu),
	// which inflates the browser jitter buffer (frame arrival jitter is the
	// lower bound of jitter buffer depth). 0 = no cap (scrcpy default).
	VideoMaxFPS int `yaml:"video_max_fps"`
	// FECDisabled turns off the automatic RED FEC evaluator. Useful for
	// weak-network A/B testing and as a kill switch if RED misbehaves.
	FECDisabled bool `yaml:"fec_disabled"`
}

// TurnServerConfig describes a TURN server with optional static credentials
// (agent-side fallback, used when signaling distributes no TURN server).
type TurnServerConfig struct {
	URLs       []string `yaml:"urls"`
	Username   string   `yaml:"username"`
	Credential string   `yaml:"credential"`
}

// TurnAuthConfig defines TURN authentication for signaling-side distribution.
// The standalone signaling server supports "none" (no auth) and "static"
// (username + credential) modes.
type TurnAuthConfig struct {
	Mode       string `yaml:"mode"`       // "none" or "static"
	Username   string `yaml:"username"`   // static mode
	Credential string `yaml:"credential"` // static mode password/token
}

// TurnIceServerConfig describes one TURN server distributed by the signaling
// server. Auth == nil means no authentication.
type TurnIceServerConfig struct {
	URLs []string        `yaml:"urls"`
	Auth *TurnAuthConfig `yaml:"auth,omitempty"`
}

// SignalingWebRTCConfig holds ICE server configuration for the signaling server.
type SignalingWebRTCConfig struct {
	STUNServers []string              `yaml:"stun_servers"`
	TURNServers []TurnIceServerConfig `yaml:"turn_servers"`
}

// SignalingConfig is used by the standalone signaling server.
type SignalingConfig struct {
	Host      string                 `yaml:"host"`
	Port      int                    `yaml:"port"`
	StaticDir string                 `yaml:"static_dir"`
	WebRTC    *SignalingWebRTCConfig `yaml:"webrtc"`
}

// AgentConfig is used by the standalone desktop/phone agent.
type AgentConfig struct {
	SignalingURL string            `yaml:"signaling_url"`
	ServiceID    string            `yaml:"service_id"`
	InstanceID   string            `yaml:"instance_id"`   // single-instance (legacy)
	DeviceSerial string            `yaml:"device_serial"` // single-instance
	InstanceList []InstanceConfig  `yaml:"instances"`     // multi-instance
	STUNServers  []string          `yaml:"stun_servers"`
	TURNServer   *TurnServerConfig `yaml:"turn_server"`
	Scrcpy       ScrcpyConfig      `yaml:"scrcpy"`
	// ADBPath 指定 adb 可执行文件路径。为空时依赖 PATH 中的 "adb"
	// （手动运行 build/agent 时 PATH 常无 adb，需显式配置，否则
	// scrcpy server 无法启动，会话直接 Error）。
	ADBPath string `yaml:"adb_path"`
}

// InstanceConfig describes a single device managed by the agent.
type InstanceConfig struct {
	InstanceID   string `yaml:"instance_id"`
	DeviceSerial string `yaml:"device_serial"`
}

// Instances returns the list of instances. If 'instances' is non-empty
// it wins; otherwise the legacy single-instance fields are used.
func (c *AgentConfig) Instances() []InstanceConfig {
	if len(c.InstanceList) > 0 {
		return c.InstanceList
	}
	iid := c.InstanceID
	if iid == "" {
		iid = "default"
	}
	return []InstanceConfig{{InstanceID: iid, DeviceSerial: c.DeviceSerial}}
}

func LoadSignaling(path string) (*SignalingConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signaling config: %w", err)
	}
	var cfg SignalingConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse signaling config: %w", err)
	}
	if cfg.Host == "" {
		cfg.Host = "0.0.0.0"
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.StaticDir == "" {
		cfg.StaticDir = "./static"
	}
	if cfg.WebRTC == nil {
		cfg.WebRTC = &SignalingWebRTCConfig{}
	}
	if len(cfg.WebRTC.STUNServers) == 0 {
		cfg.WebRTC.STUNServers = []string{"stun:stun.l.google.com:19302"}
	}
	for i := range cfg.WebRTC.TURNServers {
		t := &cfg.WebRTC.TURNServers[i]
		if t.Auth == nil {
			t.Auth = &TurnAuthConfig{Mode: "none"}
		}
		if t.Auth.Mode == "" {
			t.Auth.Mode = "none"
		}
	}
	return &cfg, nil
}

func LoadAgent(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent config: %w", err)
	}
	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse agent config: %w", err)
	}
	if cfg.SignalingURL == "" {
		cfg.SignalingURL = "ws://127.0.0.1:8080"
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = "default"
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
		cfg.Scrcpy.AudioBitRate = 256000
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
	// VideoMaxFPS 默认 0（不启用）：MuMu 模拟器上 max_fps 会把编码器
	// 拖慢到 1-3s/帧（模拟器 SurfaceFlinger 与 scrcpy max_fps 机制不兼容，
	// 实测 2 分钟仅解码 78 帧）。真机/其他模拟器可按需显式配置。
	if len(cfg.STUNServers) == 0 {
		cfg.STUNServers = []string{"stun:stun.l.google.com:19302"}
	}
	return &cfg, nil
}
