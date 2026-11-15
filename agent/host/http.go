package host

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"scrcpy-webrtc-remote/pkg/config"
)

const maxRequestBytes = 1 << 20

type HTTPServer struct {
	h         *Host
	tokenHash [32]byte
	mux       *http.ServeMux
	streams   chan struct{}
}

func NewHTTPServer(h *Host, token string) (*HTTPServer, error) {
	if h == nil || strings.TrimSpace(token) == "" {
		return nil, errors.New("host and management token are required")
	}
	s := &HTTPServer{h: h, tokenHash: sha256.Sum256([]byte(token)), mux: http.NewServeMux(), streams: make(chan struct{}, 64)}
	s.mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, h.Health()) })
	s.mux.HandleFunc("POST /v1/init", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			GlobalConfig *config.AgentConfig `json:"global_config"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		result := h.Init(req.GlobalConfig)
		writeJSON(w, responseStatus(&result.Response), result)
	})
	s.mux.HandleFunc("POST /v1/start", func(w http.ResponseWriter, r *http.Request) { writeResponse(w, h.Start()) })
	s.mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) { writeResponse(w, h.Stop()) })
	s.mux.HandleFunc("PUT /v1/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			GlobalConfig *config.AgentConfig `json:"global_config"`
		}
		if decodeJSON(w, r, &req) {
			writeResponse(w, h.ReloadConfig(req.GlobalConfig))
		}
	})
	s.mux.HandleFunc("GET /v1/devices", func(w http.ResponseWriter, r *http.Request) {
		result := h.ListDevices()
		writeJSON(w, responseStatus(&result.Response), result)
	})
	s.mux.HandleFunc("POST /v1/devices", func(w http.ResponseWriter, r *http.Request) {
		var req PrepareDeviceRequest
		if decodeJSON(w, r, &req) {
			writeResponse(w, h.PrepareDevice(req))
		}
	})
	for _, action := range []string{"release", "reset"} {
		s.mux.HandleFunc("POST /v1/devices/{serial}/"+action, func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Reason string `json:"reason"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			if action == "reset" {
				writeResponse(w, h.ResetDevice(r.PathValue("serial"), req.Reason))
			} else {
				writeResponse(w, h.ReleaseDevice(r.PathValue("serial"), req.Reason))
			}
		})
	}
	s.mux.HandleFunc("PUT /v1/devices/{serial}/quality", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Level string `json:"level"`
		}
		if decodeJSON(w, r, &req) {
			writeResponse(w, h.SetQuality(r.PathValue("serial"), req.Level))
		}
	})
	s.mux.HandleFunc("POST /v1/devices/{serial}/control", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		if decodeJSON(w, r, &req) {
			writeResponse(w, h.SendControl(r.PathValue("serial"), req.Type, req.Data))
		}
	})
	s.mux.HandleFunc("POST /v1/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Reason string `json:"reason"`
		}
		if decodeJSON(w, r, &req) {
			writeResponse(w, h.ForceCloseSession(r.PathValue("id"), req.Reason))
		}
	})
	s.mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req CreateSessionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		result := h.CreateSession(req)
		writeJSON(w, responseStatus(&result.Response), result)
	})
	s.mux.HandleFunc("DELETE /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, h.RevokeSession(r.PathValue("id")))
	})
	s.mux.HandleFunc("POST /v1/sessions/validate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID       string `json:"session_id"`
			Token    string `json:"token"`
			Service  string `json:"service_id"`
			Instance string `json:"instance_id"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		grant, _, err := h.ValidateSession(req.ID, req.Token, req.Service, req.Instance)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, errResp("ERR_UNAUTHORIZED", "invalid session authorization"))
			return
		}
		writeJSON(w, http.StatusOK, grant)
	})
	s.mux.HandleFunc("GET /v1/events", s.events)
	return s, nil
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	values := r.Header.Values("Authorization")
	var token string
	if len(values) == 1 {
		parts := strings.Fields(values[0])
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token = parts[1]
		}
	}
	hash := sha256.Sum256([]byte(token))
	if token == "" || subtle.ConstantTimeCompare(hash[:], s.tokenHash[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, errResp("ERR_UNAUTHORIZED", "valid bearer token required"))
		return
	}
	s.mux.ServeHTTP(w, r)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target interface{}) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errResp("ERR_INVALID_ARG", "Content-Type must be application/json"))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(target)
	if err == nil {
		var extra interface{}
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("expected one JSON object")
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, errResp("ERR_INVALID_ARG", "invalid JSON request"))
		return false
	}
	return true
}

func responseStatus(r *Response) int {
	if r.Ok {
		return http.StatusOK
	}
	switch r.ErrorCode {
	case "ERR_INVALID_ARG":
		return http.StatusBadRequest
	case "ERR_NOT_INIT", "ERR_ALREADY_INIT", "ERR_CONFLICT":
		return http.StatusConflict
	case "ERR_DEVICE_NOT_FOUND", "ERR_SESSION_NOT_FOUND":
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func writeResponse(w http.ResponseWriter, r *Response) { writeJSON(w, responseStatus(r), r) }

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *HTTPServer) events(w http.ResponseWriter, r *http.Request) {
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		writeJSON(w, http.StatusTooManyRequests, errResp("ERR_LIMIT", "too many event subscribers"))
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeJSON(w, http.StatusInternalServerError, errResp("ERR_INTERNAL", "streaming unavailable"))
		return
	}
	ch, cancel := s.h.EventHub().Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	send := func(text string) error {
		if err := rc.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := io.WriteString(w, text); err != nil {
			return err
		}
		return rc.Flush()
	}
	defer rc.SetWriteDeadline(time.Time{})
	if send(": connected\n\n") != nil {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if send(": heartbeat\n\n") != nil {
				return
			}
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if send(fmt.Sprintf("data: %s\n\n", data)) != nil {
				return
			}
		}
	}
}
