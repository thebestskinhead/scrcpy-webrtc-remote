package host

import (
	"sort"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
)

type PrepareDeviceRequest struct {
	DeviceSerial  string               `json:"device_serial"`
	InstanceID    string               `json:"instance_id"`
	DeviceConfig  *config.DeviceConfig `json:"device_config,omitempty"`
	ConnectParams agent.ConnectParams  `json:"connect_params,omitempty"`
}

type ListDevicesResponse struct {
	Response
	Devices []agent.DeviceView `json:"devices"`
}

func (h *Host) PrepareDevice(req PrepareDeviceRequest) *Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dm == nil {
		return errResp("ERR_NOT_INIT", "not initialized")
	}
	if req.DeviceSerial == "" || req.InstanceID == "" {
		return errResp("ERR_INVALID_ARG", "device_serial and instance_id are required")
	}
	if err := h.dm.Prepare(req.DeviceSerial, req.InstanceID, req.DeviceConfig, req.ConnectParams); err != nil {
		return errResp("ERR_CONFLICT", err.Error())
	}
	return okResp()
}

func (h *Host) ReleaseDevice(serial, reason string) *Response {
	return h.changeDevice(serial, reason, false)
}

func (h *Host) ResetDevice(serial, reason string) *Response {
	return h.changeDevice(serial, reason, true)
}

func (h *Host) changeDevice(serial, reason string, reset bool) *Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dm == nil {
		return errResp("ERR_NOT_INIT", "not initialized")
	}
	if serial == "" {
		return errResp("ERR_INVALID_ARG", "device_serial is required")
	}
	h.grants.RevokeDevice(serial)
	var err error
	if reset {
		err = h.dm.Reset(serial, reason)
	} else {
		err = h.dm.Release(serial, reason)
	}
	if err != nil {
		return errResp("ERR_DEVICE_NOT_FOUND", err.Error())
	}
	return okResp()
}

func (h *Host) ListDevices() *ListDevicesResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dm == nil {
		return &ListDevicesResponse{Response: *errResp("ERR_NOT_INIT", "not initialized"), Devices: []agent.DeviceView{}}
	}
	devices := h.dm.List()
	sort.Slice(devices, func(i, j int) bool { return devices[i].Serial < devices[j].Serial })
	return &ListDevicesResponse{Response: *okResp(), Devices: devices}
}

func (h *Host) ForceCloseSession(id, reason string) *Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dm == nil {
		return errResp("ERR_NOT_INIT", "not initialized")
	}
	if id == "" {
		return errResp("ERR_INVALID_ARG", "session_id is required")
	}
	h.grants.Revoke(id)
	ctrl := h.dm.GetBySessionID(id)
	if ctrl == nil {
		return errResp("ERR_SESSION_NOT_FOUND", "session not found")
	}
	if err := ctrl.ForceCloseSession(id, reason); err != nil {
		return errResp("ERR_SESSION_NOT_FOUND", err.Error())
	}
	return okResp()
}

func (h *Host) SetQuality(serial, level string) *Response {
	return h.withController(serial, func(c *agent.Controller) error { return c.SetQuality(level) })
}

func (h *Host) SendControl(serial, kind string, data map[string]interface{}) *Response {
	if kind == "" {
		return errResp("ERR_INVALID_ARG", "control type is required")
	}
	return h.withController(serial, func(c *agent.Controller) error { return c.SendControlRaw(kind, data) })
}

func (h *Host) withController(serial string, action func(*agent.Controller) error) *Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dm == nil {
		return errResp("ERR_NOT_INIT", "not initialized")
	}
	ctrl := h.dm.Get(serial)
	if ctrl == nil {
		return errResp("ERR_DEVICE_NOT_FOUND", "device not found")
	}
	if err := action(ctrl); err != nil {
		return errResp("ERR_INVALID_ARG", err.Error())
	}
	return okResp()
}
