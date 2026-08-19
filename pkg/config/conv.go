package config

import (
	agentapi "scrcpy-webrtc-remote/api/gen"
)

// proto 互转：api/agent.proto（GlobalConfig/DeviceConfig/ConnectParams）↔ config 结构。
// gRPC 走 proto 二进制（不经 JSON）；bootstrap JSON 用 JSON tag 直接解析进
// AgentConfig（字段名与 proto 一一对应，snake_case）。

// ToAgentConfig converts a proto GlobalConfig into AgentConfig.
// 缺失/零值字段不做默认值填充（默认值在 Init/Prepare 落库时统一归一，见
// configstore.ApplyDefaults）；grpc_listen_port 不在 GlobalConfig 内。
func ToAgentConfig(p *agentapi.GlobalConfig) *AgentConfig {
	if p == nil {
		return nil
	}
	cfg := &AgentConfig{
		SignalingURL:  p.SignalingUrl,
		ServiceID:     p.ServiceId,
		ADBPath:       p.AdbPath,
		QoSIntervalMS: int(p.QosIntervalMs),
		STUNServers:   append([]string(nil), p.StunServers...),
	}
	if p.TurnServer != nil {
		cfg.TURNServer = &TurnServerConfig{
			URLs:       append([]string(nil), p.TurnServer.Urls...),
			Username:   p.TurnServer.Username,
			Credential: p.TurnServer.Credential,
		}
	}
	if p.Scrcpy != nil {
		cfg.Scrcpy = FromProtoScrcpy(p.Scrcpy)
	}
	return cfg
}

// FromProtoScrcpy converts a proto ScrcpyConfig into the config struct.
func FromProtoScrcpy(p *agentapi.ScrcpyConfig) ScrcpyConfig {
	if p == nil {
		return ScrcpyConfig{}
	}
	return ScrcpyConfig{
		ServerVersion:         p.ServerVersion,
		JarPath:               p.JarPath,
		PortPoolStart:         int(p.PortPoolStart),
		PortPoolSize:          int(p.PortPoolSize),
		MaxSize:               int(p.MaxSize),
		VideoBitRate:          int(p.VideoBitRate),
		MinVideoBitRate:       int(p.MinVideoBitRate),
		ClearBitrate:          int(p.ClearBitrate),
		HDBitrate:             int(p.HdBitrate),
		WarmKeepSeconds:       int(p.WarmKeepSeconds),
		AudioBitRate:          int(p.AudioBitRate),
		VideoCodec:            p.VideoCodec,
		AudioCodec:            p.AudioCodec,
		AudioEncoder:          p.AudioEncoder,
		AudioEnabled:          p.AudioEnabled,
		PowerOn:               p.PowerOn,
		StayAwake:             p.StayAwake,
		VideoKeyframeInterval: int(p.VideoKeyframeInterval),
		VideoMaxFPS:           int(p.VideoMaxFps),
		FECDisabled:           p.FecDisabled,
	}
}

// ToDeviceConfig converts a proto DeviceConfig into the config struct.
// proto optional 字段（*int32/*bool）转 config 指针字段（*int/*bool）；
// nil 表"未设置"，非 nil 拷贝值。
func ToDeviceConfig(p *agentapi.DeviceConfig) *DeviceConfig {
	if p == nil {
		return nil
	}
	return &DeviceConfig{
		MaxSize:               i32ToInt(p.MaxSize),
		VideoBitRate:          i32ToInt(p.VideoBitRate),
		MinVideoBitRate:       i32ToInt(p.MinVideoBitRate),
		ClearBitrate:          i32ToInt(p.ClearBitrate),
		HDBitrate:             i32ToInt(p.HdBitrate),
		VideoKeyframeInterval: i32ToInt(p.VideoKeyframeInterval),
		VideoMaxFPS:           i32ToInt(p.VideoMaxFps),
		AudioEnabled:          p.AudioEnabled,
		WarmKeepSeconds:       i32ToInt(p.WarmKeepSeconds),
	}
}

func i32ToInt(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// ToProtoGlobalConfig converts a YAML-loaded AgentConfig into the proto
// GlobalConfig used by Init/ReloadConfig（agentdrv 模拟平台注入配置用）。
func ToProtoGlobalConfig(cfg *AgentConfig) *agentapi.GlobalConfig {
	if cfg == nil {
		return nil
	}
	g := &agentapi.GlobalConfig{
		SignalingUrl:  cfg.SignalingURL,
		ServiceId:     cfg.ServiceID,
		StunServers:   append([]string(nil), cfg.STUNServers...),
		AdbPath:       cfg.ADBPath,
		QosIntervalMs: int32(cfg.QoSIntervalMS),
		Scrcpy:        ToProtoScrcpy(cfg.Scrcpy),
	}
	if cfg.TURNServer != nil {
		g.TurnServer = &agentapi.TurnServer{
			Urls:       append([]string(nil), cfg.TURNServer.URLs...),
			Username:   cfg.TURNServer.Username,
			Credential: cfg.TURNServer.Credential,
		}
	}
	return g
}

// ToProtoScrcpy converts a config ScrcpyConfig into its proto form.
func ToProtoScrcpy(c ScrcpyConfig) *agentapi.ScrcpyConfig {
	return &agentapi.ScrcpyConfig{
		ServerVersion:         c.ServerVersion,
		JarPath:               c.JarPath,
		PortPoolStart:         int32(c.PortPoolStart),
		PortPoolSize:          int32(c.PortPoolSize),
		MaxSize:               int32(c.MaxSize),
		VideoBitRate:          int32(c.VideoBitRate),
		MinVideoBitRate:       int32(c.MinVideoBitRate),
		ClearBitrate:          int32(c.ClearBitrate),
		HdBitrate:             int32(c.HDBitrate),
		WarmKeepSeconds:       int32(c.WarmKeepSeconds),
		AudioBitRate:          int32(c.AudioBitRate),
		VideoCodec:            c.VideoCodec,
		AudioCodec:            c.AudioCodec,
		AudioEncoder:          c.AudioEncoder,
		AudioEnabled:          c.AudioEnabled,
		PowerOn:               c.PowerOn,
		StayAwake:             c.StayAwake,
		VideoKeyframeInterval: int32(c.VideoKeyframeInterval),
		VideoMaxFps:           int32(c.VideoMaxFPS),
		FecDisabled:           c.FECDisabled,
	}
}
