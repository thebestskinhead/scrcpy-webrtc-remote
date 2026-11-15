package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"scrcpy-webrtc-remote/pkg/common"
	"scrcpy-webrtc-remote/pkg/sessionauth"
	"scrcpy-webrtc-remote/pkg/signaling"
)

func TestAuthorizedWebSocketLifecycle(t *testing.T) {
	store := sessionauth.NewStore()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management" {
			w.WriteHeader(401)
			return
		}
		var request struct {
			ID       string `json:"session_id"`
			Token    string `json:"token"`
			Service  string `json:"service_id"`
			Instance string `json:"instance_id"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		grant, _, err := store.Validate(request.ID, request.Token, request.Service, request.Instance)
		if err != nil {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(grant)
	}))
	defer authority.Close()
	client, err := sessionauth.NewClient(authority.URL, "management")
	if err != nil {
		t.Fatal(err)
	}
	sig := signaling.New(signaling.Options{})
	defer sig.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		sig.HandleAgentWS(conn, "service")
	})
	mux.HandleFunc("/ws/browser/", authorizedWS(sig, client, false))
	mux.HandleFunc("/ws/relay/", authorizedWS(sig, client, true))
	server := httptest.NewServer(mux)
	defer server.Close()
	base := "ws" + strings.TrimPrefix(server.URL, "http")
	agent, _, err := websocket.DefaultDialer.Dial(base+"/agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	_ = agent.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := agent.WriteJSON(common.WsMsg{Type: common.TypeRegister, InstanceID: "instance", DeviceSerial: "device"}); err != nil {
		t.Fatal(err)
	}
	var message common.WsMsg
	if err := agent.ReadJSON(&message); err != nil || message.Type != "registered" {
		t.Fatalf("register: %+v %v", message, err)
	}
	if err := agent.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	grant, token, err := store.Issue("device", "service", "instance", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"browser", "relay"} {
		url := base + "/ws/" + endpoint + "/service/instance?session_id=" + grant.ID
		_, response, err := websocket.DefaultDialer.Dial(url, nil)
		if err == nil || response == nil || response.StatusCode != 401 {
			t.Fatalf("anonymous %s accepted: %v", endpoint, err)
		}
		response.Body.Close()
	}
	dialer := websocket.Dialer{Subprotocols: []string{"astra.auth." + token}, HandshakeTimeout: time.Second}
	browser, _, err := dialer.Dial(base+"/ws/browser/service/instance?session_id="+grant.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	if browser.Subprotocol() != "astra.auth."+token {
		t.Fatal("authorized subprotocol was not negotiated")
	}
	_ = browser.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := agent.ReadJSON(&message); err != nil || message.Type != common.TypeBound || message.SessionID != grant.ID {
		t.Fatalf("bound scope: %+v %v", message, err)
	}
	if err := browser.ReadJSON(&message); err != nil || message.Type != common.TypeIceServers {
		t.Fatalf("ICE: %+v %v", message, err)
	}
	duplicate, _, err := dialer.Dial(base+"/ws/browser/service/instance?session_id="+grant.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = duplicate.SetReadDeadline(time.Now().Add(time.Second))
	if err := duplicate.ReadJSON(&message); err != nil || message.Type != common.TypeError {
		t.Fatalf("duplicate accepted: %+v %v", message, err)
	}
	duplicate.Close()
	if err := browser.WriteJSON(common.WsMsg{Type: common.TypeOffer, SessionID: "forged"}); err != nil {
		t.Fatal(err)
	}
	if err := agent.ReadJSON(&message); err != nil || message.Type != common.TypeOffer || message.SessionID != grant.ID {
		t.Fatalf("offer scope: %+v %v", message, err)
	}
	if err := browser.WriteJSON(common.WsMsg{Type: common.TypeBound, SessionID: "forged"}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{common.TypeFirstFrame, common.TypeDecoderStats, common.TypeDecoderStatus, common.TypeResetVideo} {
		if err := browser.WriteJSON(common.WsMsg{Type: kind, SessionID: "forged"}); err != nil {
			t.Fatal(err)
		}
		if err := agent.ReadJSON(&message); err != nil || message.Type != kind || message.SessionID != grant.ID {
			t.Fatalf("telemetry scope: %+v %v", message, err)
		}
	}
	store.Revoke(grant.ID)
	if err := browser.ReadJSON(&message); err == nil {
		t.Fatal("revoked browser stayed connected")
	}
	if err := agent.ReadJSON(&message); err != nil || message.Type != common.TypeUnbound || message.SessionID != grant.ID {
		t.Fatalf("unbound: %+v %v", message, err)
	}
	_, response, err := dialer.Dial(base+"/ws/browser/service/instance?session_id="+grant.ID, nil)
	if err == nil || response == nil || response.StatusCode != 401 {
		t.Fatal("revoked credential accepted")
	}
	response.Body.Close()
}
