package config

// DeviceConfig carries per-device overrides injected via PrepareDevice.
// Pointer fields distinguish "explicitly set" from "inherit global".
// 与 api/agent.proto DeviceConfig 字段一一对应（见 conv.go）。
type DeviceConfig struct {
	MaxSize               *int  `json:"max_size,omitempty"`
	VideoBitRate          *int  `json:"video_bit_rate,omitempty"`
	MinVideoBitRate       *int  `json:"min_video_bit_rate,omitempty"`
	ClearBitrate          *int  `json:"clear_bitrate,omitempty"`
	HDBitrate             *int  `json:"hd_bitrate,omitempty"`
	VideoKeyframeInterval *int  `json:"video_keyframe_interval,omitempty"`
	VideoMaxFPS           *int  `json:"video_max_fps,omitempty"`
	AudioEnabled          *bool `json:"audio_enabled,omitempty"`
	WarmKeepSeconds       *int  `json:"warm_keep_seconds,omitempty"`
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
