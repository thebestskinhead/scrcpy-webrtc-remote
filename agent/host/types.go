package host

// Response is shared by management operations, independently of transport.
type Response struct {
	Ok        bool   `json:"ok"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message,omitempty"`
}

type InitResponse struct {
	Response
	ActualPort int `json:"actual_port"`
}

type HealthResponse struct {
	Ok             bool   `json:"ok"`
	Version        string `json:"version"`
	DeviceCount    int    `json:"device_count"`
	ActiveSessions int    `json:"active_sessions"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	LastError      string `json:"last_error,omitempty"`
}

// Event is a best-effort notification; clients reconcile through state queries.
type Event struct {
	Type         string    `json:"type"`
	DeviceSerial string    `json:"device_serial"`
	InstanceID   string    `json:"instance_id"`
	SessionID    string    `json:"session_id,omitempty"`
	TimestampMS  int64     `json:"timestamp_ms"`
	Data         EventData `json:"data"`
}

type EventData struct {
	Busy    bool     `json:"busy,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Code    string   `json:"code,omitempty"`
	Message string   `json:"message,omitempty"`
	QoS     *QoSData `json:"qos,omitempty"`
}

type QoSData struct {
	LossRate    float64 `json:"loss_rate"`
	RTTMs       float64 `json:"rtt_ms"`
	Bitrate     int     `json:"bitrate"`
	FPS         float64 `json:"fps"`
	ResolutionX int     `json:"resolution_x"`
	ResolutionY int     `json:"resolution_y"`
}
