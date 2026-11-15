package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"scrcpy-webrtc-remote/pkg/sessionauth"
	"scrcpy-webrtc-remote/pkg/signaling"
)

func requireBearer(w http.ResponseWriter, r *http.Request, token string) bool {
	actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	expected := sha256.Sum256([]byte("Bearer " + token))
	if token == "" || len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "authorization required", http.StatusUnauthorized)
		return false
	}
	return true
}

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
		authorizedUpgrader := upgrader
		authorizedUpgrader.Subprotocols = []string{"astra.auth." + token}
		conn, err := authorizedUpgrader.Upgrade(w, r, nil)
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
