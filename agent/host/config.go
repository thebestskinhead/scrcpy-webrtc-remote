package host

import (
	"fmt"
	"net/url"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
)

func validateConfig(cfg *config.AgentConfig) error {
	if cfg == nil {
		return fmt.Errorf("missing global_config")
	}
	effective := agent.NewConfigStore(cfg).Global()
	u, err := url.Parse(effective.SignalingURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("signaling_url must be ws or wss with a host")
	}
	s := effective.Scrcpy
	if s.PortPoolStart < 1 || s.PortPoolStart > 65535 || s.PortPoolSize < 1 || s.PortPoolSize > 65536-s.PortPoolStart {
		return fmt.Errorf("invalid scrcpy port pool range")
	}
	if s.MaxSize < 0 || s.VideoBitRate < 0 || s.MinVideoBitRate < 0 || s.ClearBitrate < 0 || s.HDBitrate < 0 || s.WarmKeepSeconds < 0 || s.AudioBitRate < 0 || s.VideoKeyframeInterval < 0 || s.VideoMaxFPS < 0 || effective.QoSIntervalMS < 0 {
		return fmt.Errorf("encoding and timing values must be non-negative")
	}
	return nil
}
