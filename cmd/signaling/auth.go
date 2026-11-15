package main

import (
	"context"
	"net/http"
	"strings"

	"scrcpy-webrtc-remote/pkg/sessionauth"
	"scrcpy-webrtc-remote/pkg/signaling"
)

func authorizedWS(sig *signaling.Server, auth *sessionauth.Client, relay bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		service, instance := pathLast2(r.URL.Path)
		id := r.URL.Query().Get("session_id")
		var token string
		for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
			protocol = strings.TrimSpace(protocol)
			if strings.HasPrefix(protocol, "astra.auth.") {
				if token != "" {
					http.Error(w, "invalid authorization", http.StatusUnauthorized)
					return
				}
				token = strings.TrimPrefix(protocol, "astra.auth.")
			}
		}
		if service == "" || instance == "" || id == "" || token == "" {
			http.Error(w, "session authorization required", http.StatusUnauthorized)
			return
		}
		grant, err := auth.Validate(r.Context(), id, token, service, instance)
		if err != nil {
			http.Error(w, "invalid session authorization", http.StatusUnauthorized)
			return
		}
		matched := false
		agents := sig.Agents()
		for i := range agents {
			if agents[i].ServiceID == service && agents[i].InstanceID == instance && agents[i].DeviceSerial == grant.DeviceSerial {
				matched = true
				break
			}
		}
		if !matched {
			http.Error(w, "authorized device offline", http.StatusConflict)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(1 << 20)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		revoked := auth.Watch(ctx, grant, token)
		if relay {
			sig.HandleAuthorizedRelayWS(conn, service, instance, grant.ID, revoked, grant.ExpiresAt)
		} else {
			sig.HandleAuthorizedBrowserWS(conn, service, instance, grant.ID, revoked, grant.ExpiresAt)
		}
	}
}
