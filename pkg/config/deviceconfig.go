package config

// DeviceConfig carries per-device overrides injected via PrepareDevice.
// Pointer fields distinguish "explicitly set" from "inherit global".
// 与 api/agent.proto DeviceConfig 字段一一对应（见 conv.go）。
type DeviceConfig struct {
	MaxSize              *int
	VideoBitRate         *int
	MinVideoBitRate      *int
	ClearBitrate         *int
	HDBitrate            *int
	VideoKeyframeInterval *int
	VideoMaxFPS          *int
	AudioEnabled         *bool
	WarmKeepSeconds      *int
}

// MergeDeviceConfig returns a copy of base with every explicitly-set
// DeviceConfig field overlaid. 显式字段覆盖全局；未设置字段沿用全局值。
func MergeDeviceConfig(base ScrcpyConfig, d *DeviceConfig) ScrcpyConfig {
	if d == nil {
		return base
	}
	if d.MaxSize != nil {
		base.MaxSize = *d.MaxSize
	}
	if d.VideoBitRate != nil {
		base.VideoBitRate = *d.VideoBitRate
	}
	if d.MinVideoBitRate != nil {
		base.MinVideoBitRate = *d.MinVideoBitRate
	}
	if d.ClearBitrate != nil {
		base.ClearBitrate = *d.ClearBitrate
	}
	if d.HDBitrate != nil {
		base.HDBitrate = *d.HDBitrate
	}
	if d.VideoKeyframeInterval != nil {
		base.VideoKeyframeInterval = *d.VideoKeyframeInterval
	}
	if d.VideoMaxFPS != nil {
		base.VideoMaxFPS = *d.VideoMaxFPS
	}
	if d.AudioEnabled != nil {
		base.AudioEnabled = *d.AudioEnabled
	}
	if d.WarmKeepSeconds != nil {
		base.WarmKeepSeconds = *d.WarmKeepSeconds
	}
	return base
}
