// Package signaling provides a pure WebRTC signaling engine.
// It has ZERO HTTP dependencies — the platform's web layer handles HTTP
// routing, static file serving, and REST APIs. Signaling only takes
// *websocket.Conn and processes the signaling protocol.
//
// Usage:
//
//	sig := signaling.New(signaling.Options{...})
//
//	// Platform wires WS connections into signaling:
//	mux.HandleFunc("/ws/agent/", func(w, r) {
//	    conn, _ := upgrader.Upgrade(w, r, nil)
//	    sig.HandleAgentWS(conn, extractServiceID(r.URL))
//	})
//	mux.HandleFunc("/ws/browser/", func(w, r) {
//	    conn, _ := upgrader.Upgrade(w, r, nil)
//	    sig.HandleBrowserWS(conn, extractSvc(r.URL), extractInst(r.URL))
//	})
//
//	// Platform writes its own REST API:
//	mux.HandleFunc("GET /api/devices", func(w, r) {
//	    json.NewEncoder(w).Encode(sig.Agents())
//	})
package signaling

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"scrcpy-webrtc-remote/pkg/common"
	"scrcpy-webrtc-remote/pkg/logger"
)

// Options configures the signaling engine.
type Options struct {
	STUNServers []string
	TURNServer  *common.IceServerEntry
}

// browserSession tracks one browser WebSocket and the agent it talks to,
// so the session can be torn down when that agent disconnects.
type browserSession struct {
	sessionID  string
	serviceID  string
	instanceID string
}

// Server is a pure signaling engine. It owns no HTTP server and serves
// no routes. The platform is responsible for HTTP and calls HandleAgentWS /
// HandleBrowserWS after WebSocket upgrade.
type Server struct {
	opts   Options
	agents *agentRegistry

	browserSessionMu sync.Mutex
	browserSessions  map[*websocket.Conn]*browserSession // browser WS → session

	// relaySessions maps relay_id → relay client WS. Agent responses of
	// type "relay" are routed back through here (HandleAgentWS read loop),
	// which avoids a second reader competing on the agent connection.
	relaySessionMu sync.Mutex
	relaySessions  map[string]*websocket.Conn
}

// New creates a signaling engine.
func New(opts Options) *Server {
	if len(opts.STUNServers) == 0 {
		opts.STUNServers = []string{"stun:stun.l.google.com:19302"}
	}
	return &Server{
		opts:            opts,
		agents:          newAgentRegistry(),
		browserSessions: make(map[*websocket.Conn]*browserSession),
		relaySessions:   make(map[string]*websocket.Conn),
	}
}

// ---------------------------------------------------------------------------
// Public API — called by the platform's web layer
// ---------------------------------------------------------------------------

// HandleAgentWS processes an agent WebSocket connection. The caller has
// already performed the HTTP upgrade and extracted the service_id.
// This method blocks for the lifetime of the agent connection.
func (s *Server) HandleAgentWS(ws *websocket.Conn, serviceID string) {
	// Wait for register message
	var reg common.WsMsg
	if err := ws.ReadJSON(&reg); err != nil || reg.Type != common.TypeRegister {
		logger.Error("agent register failed", "service", serviceID, "err", err)
		ws.Close()
		return
	}

	info := &AgentInfo{
		ServiceID:    serviceID,
		InstanceID:   reg.InstanceID,
		DeviceSerial: reg.DeviceSerial,
		Conn:         ws,
		WriteMu:      sync.Mutex{},
		RegisteredAt: time.Now(),
	}

	s.agents.register(info)
	defer func() {
		s.agents.remove(info)
		// Agent gone → every browser session bound to it is a zombie (its
		// forward goroutine holds this dead conn). Close them so frontends
		// enter their reconnect flow instead of waiting forever.
		s.closeBrowsersForAgent(serviceID, info.InstanceID)
	}()

	// Ack registration
	writeAgentWS(info, common.WsMsg{Type: "registered"})

	// Send ICE servers
	writeAgentWS(info, common.WsMsg{
		Type:       common.TypeIceServers,
		IceServers: s.buildICEServerEntries(),
	})

	logger.Info("agent registered",
		"service", serviceID,
		"instance", info.InstanceID,
		"serial", info.DeviceSerial)

	// Read loop — relay messages from agent to browser
	for {
		var msg common.WsMsg
		if err := ws.ReadJSON(&msg); err != nil {
			logger.Info("agent disconnected",
				"service", serviceID,
				"instance", info.InstanceID)
			return
		}

		switch msg.Type {
		case common.TypeAnswer, common.TypeIceCandidate, common.TypeError, common.TypeStreamDead:
			s.forwardToBrowser(msg)

		case "relay":
			// Relay responses (query_status / preempt) route back to the
			// relay client by relay_id. Handled here in the single agent
			// read loop — never by a second reader on the agent conn.
			s.forwardToRelay(msg)

		case common.TypePreempted:
			s.preemptBrowser(msg.SessionID)

		case common.TypePing:
			writeAgentWS(info, common.WsMsg{Type: common.TypePong})
		}
	}
}

// HandleRelayWS processes a relay WebSocket connection from the platform's
// web server. Unlike HandleBrowserWS, there is no bound/unbound lifecycle —
// this is a simple bidirectional forwarder between the platform and agent.
//
// Protocol (language-agnostic, just JSON over WebSocket):
//
//	Platform → Signaling (forwarded to agent as-is):
//	  {"type":"relay","relay_id":"...","payload":{"type":"query_status"}}
//
//	Signaling → Platform (agent's relay response, routed by relay_id):
//	  {"type":"relay","relay_id":"...","payload":{"type":"status_resp","busy":false}}
//
// The agent→platform direction is NOT read here — agent messages are
// demultiplexed in the single HandleAgentWS read loop and routed back by
// relay_id, which avoids two goroutines competing to read the agent conn.
func (s *Server) HandleRelayWS(ws *websocket.Conn, serviceID, instanceID string) {
	// registered relay_ids owned by this connection, for cleanup
	owned := make(map[string]bool)
	defer func() {
		s.relaySessionMu.Lock()
		for id := range owned {
			if cur, ok := s.relaySessions[id]; ok && cur == ws {
				delete(s.relaySessions, id)
			}
		}
		s.relaySessionMu.Unlock()
		ws.Close()
	}()

	for {
		var msg common.WsMsg
		if err := ws.ReadJSON(&msg); err != nil {
			return
		}
		if msg.Type != "relay" || msg.Payload == nil || msg.RelayID == "" {
			continue
		}

		// Register relay_id → this WS so the agent's response can route back
		s.relaySessionMu.Lock()
		s.relaySessions[msg.RelayID] = ws
		s.relaySessionMu.Unlock()
		owned[msg.RelayID] = true

		// Forward the relay envelope as-is to the CURRENT agent
		// (looked up per message so an agent reconnect doesn't wedge relays)
		agent := s.agents.lookup(serviceID, instanceID)
		if agent == nil {
			writeWS(ws, common.WsMsg{
				Type:    "relay",
				RelayID: msg.RelayID,
				Payload: map[string]any{"type": "error", "message": "device offline"},
			})
			continue
		}
		writeAgentWS(agent, msg)
	}
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getStringPtr(m map[string]any, key string) *string {
	if v, ok := m[key].(string); ok {
		return &v
	}
	return nil
}

// HandleBrowserWS processes a browser WebSocket connection. The caller
// has already performed the HTTP upgrade and extracted service_id + instance_id.
// This method starts background goroutines and returns immediately.
func (s *Server) HandleBrowserWS(ws *websocket.Conn, serviceID, instanceID string) {
	sessionID := uuid.New().String()

	agent := s.agents.lookup(serviceID, instanceID)
	if agent == nil {
		writeWS(ws, common.WsMsg{Type: common.TypeError, PCID: "device not found", Message: "device not found"})
		ws.Close()
		return
	}

	s.browserSessionMu.Lock()
	s.browserSessions[ws] = &browserSession{
		sessionID:  sessionID,
		serviceID:  serviceID,
		instanceID: instanceID,
	}
	s.browserSessionMu.Unlock()

	// Tell agent a browser connected
	writeAgentWS(agent, common.WsMsg{
		Type:      common.TypeBound,
		SessionID: sessionID,
	})

	// Send ICE servers to browser
	writeWS(ws, common.WsMsg{
		Type:       common.TypeIceServers,
		IceServers: s.buildICEServerEntries(),
	})

	// Browser → Agent: read from browser WS, forward to agent
	go func() {
		defer func() {
			// On browser disconnect, tell the CURRENT agent (it may have
			// reconnected since this session was created)
			if cur := s.agents.lookup(serviceID, instanceID); cur != nil {
				writeAgentWS(cur, common.WsMsg{
					Type:      common.TypeUnbound,
					SessionID: sessionID,
				})
			}
			s.browserSessionMu.Lock()
			delete(s.browserSessions, ws)
			s.browserSessionMu.Unlock()
			ws.Close()
		}()
		for {
			var msg common.WsMsg
			if err := ws.ReadJSON(&msg); err != nil {
				return
			}
			msg.SessionID = sessionID
			// Look up the agent per message: after an agent reconnect the
			// old conn is dead and writes into it would silently vanish.
			cur := s.agents.lookup(serviceID, instanceID)
			if cur == nil {
				writeWS(ws, common.WsMsg{Type: common.TypeError, Message: "device offline"})
				return
			}
			writeAgentWS(cur, msg)
		}
	}()
}

// Agents returns a snapshot of all registered agents. The platform
// calls this from its REST API handler.
func (s *Server) Agents() []AgentInfo {
	list := s.agents.all()
	result := make([]AgentInfo, len(list))
	for i, a := range list {
		result[i] = AgentInfo{
			ServiceID:    a.ServiceID,
			InstanceID:   a.InstanceID,
			DeviceSerial: a.DeviceSerial,
			RegisteredAt: a.RegisteredAt,
		}
	}
	return result
}

// Close shuts down all agent connections.
func (s *Server) Close() {
	for _, a := range s.agents.all() {
		a.Conn.Close()
	}
}

// ---------------------------------------------------------------------------
// Internal: forwarding & preemption
// ---------------------------------------------------------------------------

func (s *Server) forwardToBrowser(msg common.WsMsg) {
	s.browserSessionMu.Lock()
	defer s.browserSessionMu.Unlock()
	for ws, sess := range s.browserSessions {
		if sess.sessionID == msg.SessionID {
			writeWS(ws, msg)
			return
		}
	}
}

func (s *Server) preemptBrowser(sessionID string) {
	s.browserSessionMu.Lock()
	defer s.browserSessionMu.Unlock()
	for ws, sess := range s.browserSessions {
		if sess.sessionID == sessionID {
			ws.Close()
			delete(s.browserSessions, ws)
			return
		}
	}
}

// closeBrowsersForAgent closes every browser session bound to the given
// agent identity. Called when that agent's WS drops — the sessions' forward
// goroutines held the dead agent conn and could never recover on their own.
// Frontend sees a WS close and enters its normal reconnect flow.
func (s *Server) closeBrowsersForAgent(serviceID, instanceID string) {
	s.browserSessionMu.Lock()
	defer s.browserSessionMu.Unlock()
	for ws, sess := range s.browserSessions {
		if sess.serviceID == serviceID && sess.instanceID == instanceID {
			logger.Info("closing browser session of disconnected agent",
				"session_id", sess.sessionID)
			ws.Close()
			delete(s.browserSessions, ws)
		}
	}
}

// forwardToRelay routes an agent's relay response back to the relay client
// that issued the request, matched by relay_id.
func (s *Server) forwardToRelay(msg common.WsMsg) {
	s.relaySessionMu.Lock()
	ws, ok := s.relaySessions[msg.RelayID]
	s.relaySessionMu.Unlock()
	if !ok {
		logger.Warn("relay response for unknown relay_id", "relay_id", msg.RelayID)
		return
	}
	writeWS(ws, msg)
}

// ---------------------------------------------------------------------------
// Internal: ICE servers
// ---------------------------------------------------------------------------

func (s *Server) buildICEServerEntries() []common.IceServerEntry {
	entries := make([]common.IceServerEntry, 0, len(s.opts.STUNServers)+1)
	for _, u := range s.opts.STUNServers {
		if u == "" {
			continue
		}
		entries = append(entries, common.IceServerEntry{URLs: []string{u}})
	}
	if s.opts.TURNServer != nil {
		// Skip entries with no URLs — an empty uri makes the browser's
		// RTCPeerConnection constructor reject the whole iceServers array
		// ("ICE server parsing failed: Empty uri").
		valid := make([]string, 0, len(s.opts.TURNServer.URLs))
		for _, u := range s.opts.TURNServer.URLs {
			if u != "" {
				valid = append(valid, u)
			}
		}
		if len(valid) > 0 {
			e := *s.opts.TURNServer
			e.URLs = valid
			entries = append(entries, e)
		} else {
			logger.Warn("TURN server configured with no URLs, skipping")
		}
	}
	return entries
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeAgentWS writes to an agent connection under its write mutex.
// Agent conns are written from many goroutines (browser forwarders, pong
// replies, relay forwarders); gorilla/websocket panics on concurrent writes.
func writeAgentWS(a *AgentInfo, msg common.WsMsg) {
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	writeWS(a.Conn, msg)
}

func writeWS(ws *websocket.Conn, msg common.WsMsg) {
	ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := ws.WriteJSON(msg); err != nil {
		logger.Warn("ws write error", "err", err)
	}
}

// Keep json import for Agents() marshaling hint
var _ = json.Encoder{}
