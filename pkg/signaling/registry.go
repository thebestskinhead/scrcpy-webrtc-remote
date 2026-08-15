package signaling

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// AgentInfo holds the registered metadata and WebSocket for one agent.
type AgentInfo struct {
	ServiceID    string
	InstanceID   string
	DeviceSerial string
	Conn         *websocket.Conn
	WriteMu      sync.Mutex
	RegisteredAt time.Time
}

func (a *AgentInfo) fullID() string { return a.ServiceID + "/" + a.InstanceID }

type agentRegistry struct {
	mu   sync.RWMutex
	byID map[string]*AgentInfo
}

func newAgentRegistry() *agentRegistry {
	return &agentRegistry{byID: make(map[string]*AgentInfo)}
}

func (r *agentRegistry) register(info *AgentInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := info.fullID()
	if old, ok := r.byID[id]; ok && old.Conn != info.Conn {
		old.Conn.Close()
	}
	r.byID[id] = info
}

func (r *agentRegistry) remove(info *AgentInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := info.fullID()
	if cur, ok := r.byID[id]; ok && cur == info {
		delete(r.byID, id)
	}
}

func (r *agentRegistry) lookup(serviceID, instanceID string) *AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[serviceID+"/"+instanceID]
}

func (r *agentRegistry) all() []*AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*AgentInfo, 0, len(r.byID))
	for _, a := range r.byID {
		out = append(out, a)
	}
	return out
}
