package host

import (
	"errors"
	"time"

	"scrcpy-webrtc-remote/pkg/sessionauth"
)

type CreateSessionRequest struct {
	DeviceSerial string `json:"device_serial"`
	TTLSeconds   int    `json:"ttl_seconds,omitempty"`
	Preempt      bool   `json:"preempt,omitempty"`
}

type CreateSessionResponse struct {
	Response
	Grant *sessionauth.Grant `json:"session,omitempty"`
	Token string             `json:"token,omitempty"`
}

func (h *Host) CreateSession(req CreateSessionRequest) *CreateSessionResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	fail := func(code, message string) *CreateSessionResponse {
		return &CreateSessionResponse{Response: *errResp(code, message)}
	}
	if h.dm == nil {
		return fail("ERR_NOT_INIT", "not initialized")
	}
	if req.DeviceSerial == "" || req.TTLSeconds < 0 || req.TTLSeconds > int(sessionauth.MaxTTL/time.Second) {
		return fail("ERR_INVALID_ARG", "invalid device_serial or ttl_seconds")
	}
	ctrl := h.dm.Get(req.DeviceSerial)
	if ctrl == nil {
		return fail("ERR_DEVICE_NOT_FOUND", "device not prepared")
	}
	var instance string
	for _, device := range h.dm.List() {
		if device.Serial == req.DeviceSerial {
			instance = device.InstanceID
			break
		}
	}
	if ctrl.CurrentSessionID() != "" && !req.Preempt {
		return fail("ERR_CONFLICT", "device has an active session")
	}
	service := ctrl.ServiceID()
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = 300
	}
	grant, token, err := h.grants.Issue(req.DeviceSerial, service, instance, time.Duration(ttl)*time.Second, req.Preempt)
	if errors.Is(err, sessionauth.ErrConflict) {
		return fail("ERR_CONFLICT", err.Error())
	}
	if err != nil {
		return fail("ERR_INTERNAL", "unable to issue session authorization")
	}
	if req.Preempt {
		if current := ctrl.CurrentSessionID(); current != "" {
			if err := ctrl.ForceCloseSession(current, "preempted"); err != nil {
				h.grants.Revoke(grant.ID)
				return fail("ERR_CONFLICT", "unable to close previous session")
			}
		}
	}
	return &CreateSessionResponse{Response: *okResp(), Grant: &grant, Token: token}
}

func (h *Host) RevokeSession(id string) *Response {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dm == nil {
		return errResp("ERR_NOT_INIT", "not initialized")
	}
	if id == "" {
		return errResp("ERR_INVALID_ARG", "session_id is required")
	}
	h.grants.Revoke(id)
	if ctrl := h.dm.GetBySessionID(id); ctrl != nil {
		if err := ctrl.ForceCloseSession(id, "revoked"); err != nil {
			return errResp("ERR_CONFLICT", err.Error())
		}
	}
	return okResp()
}

func (h *Host) ValidateSession(id, token, service, instance string) (sessionauth.Grant, <-chan struct{}, error) {
	return h.grants.Validate(id, token, service, instance)
}
