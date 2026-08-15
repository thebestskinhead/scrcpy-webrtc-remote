// Package types provides shared media / control types used by both
// the agent and bridge WebRTC layers.
package types

import "time"

// Sample is a decoded media sample (video or audio frame).
type Sample struct {
	Data     []byte
	PTS      time.Duration
	Duration time.Duration
	KeyFrame bool
}

// DeviceMsg is a parsed device-to-client control message
// (clipboard, UHID, etc.).
type DeviceMsg struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Sequence uint64 `json:"sequence"`
	DevID    uint16 `json:"dev_id"`
	DataHex  string `json:"data_hex"`
	Raw      []byte `json:"-"`
}
