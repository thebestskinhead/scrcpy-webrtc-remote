package gateway

import (
	"fmt"
	"sync"
	"time"

	"scrcpy-webrtc-remote/agent/internal/adb"
	"scrcpy-webrtc-remote/agent/internal/portpool"
	"scrcpy-webrtc-remote/pkg/config"
)

type Manager struct {
	adb         *adb.Client
	portPool    *portpool.Pool
	cfg         config.ScrcpyConfig
	sessions    map[string]*Session
	warmTimers  map[string]*time.Timer
	mu          sync.Mutex
	gate        sync.RWMutex
	closed      bool
	deviceLocks sync.Map
	start       func(*Session) error
}

func NewManager(adbClient *adb.Client, portPool *portpool.Pool, cfg config.ScrcpyConfig) *Manager {
	return &Manager{adb: adbClient, portPool: portPool, cfg: cfg,
		sessions: make(map[string]*Session), warmTimers: make(map[string]*time.Timer),
		start: (*Session).Start}
}

func (m *Manager) lockDevice(serial string) func() {
	value, _ := m.deviceLocks.LoadOrStore(serial, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (m *Manager) Acquire(serial string) (*Session, bool, error) {
	return m.acquire(serial, m.adb, m.cfg)
}

func (m *Manager) AcquireConfigured(serial string, client *adb.Client, cfg config.ScrcpyConfig) (*Session, bool, error) {
	return m.acquire(serial, client, cfg)
}

func (m *Manager) acquire(serial string, client *adb.Client, cfg config.ScrcpyConfig) (*Session, bool, error) {
	m.gate.RLock()
	defer m.gate.RUnlock()
	if m.closed {
		return nil, false, fmt.Errorf("manager closed")
	}
	unlock := m.lockDevice(serial)
	defer unlock()
	m.mu.Lock()
	old := m.sessions[serial]
	m.cancelWarmTimerLocked(serial)
	if old != nil && old.Alive() && old.cfg == cfg && old.adb.Path() == client.Path() {
		m.mu.Unlock()
		return old, true, nil
	}
	delete(m.sessions, serial)
	m.mu.Unlock()
	if old != nil {
		old.Release()
	}
	s := NewSession(serial, client, m.portPool, cfg)
	if err := m.start(s); err != nil {
		s.Release()
		return nil, false, err
	}
	m.mu.Lock()
	m.sessions[serial] = s
	m.mu.Unlock()
	return s, false, nil
}

func (m *Manager) Release(serial string) { m.ReleaseSession(serial, nil) }

// ReleaseSession only removes the expected instance, unless expected is nil.
func (m *Manager) ReleaseSession(serial string, expected *Session) {
	m.gate.RLock()
	defer m.gate.RUnlock()
	unlock := m.lockDevice(serial)
	defer unlock()
	m.mu.Lock()
	s := m.sessions[serial]
	if expected != nil && expected != s {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, serial)
	m.cancelWarmTimerLocked(serial)
	m.mu.Unlock()
	if s != nil {
		s.Release()
	}
}

func (m *Manager) ReleaseDeferred(serial string, d time.Duration) {
	if d <= 0 {
		m.Release(serial)
		return
	}
	m.gate.RLock()
	defer m.gate.RUnlock()
	unlock := m.lockDevice(serial)
	defer unlock()
	m.mu.Lock()
	s := m.sessions[serial]
	if m.closed || s == nil {
		m.mu.Unlock()
		return
	}
	m.cancelWarmTimerLocked(serial)
	if !s.Alive() {
		delete(m.sessions, serial)
		m.mu.Unlock()
		s.Release()
		return
	}
	s.SetForwardPaused(true)
	var timer *time.Timer
	ready := make(chan struct{})
	timer = time.AfterFunc(d, func() { <-ready; m.expireWarm(serial, s, timer) })
	m.warmTimers[serial] = timer
	close(ready)
	m.mu.Unlock()
}

func (m *Manager) expireWarm(serial string, expected *Session, timer *time.Timer) {
	m.gate.RLock()
	defer m.gate.RUnlock()
	unlock := m.lockDevice(serial)
	defer unlock()
	m.mu.Lock()
	if m.warmTimers[serial] != timer || m.sessions[serial] != expected {
		m.mu.Unlock()
		return
	}
	delete(m.warmTimers, serial)
	delete(m.sessions, serial)
	m.mu.Unlock()
	expected.Release()
}

func (m *Manager) cancelWarmTimerLocked(serial string) {
	if t, ok := m.warmTimers[serial]; ok {
		t.Stop()
		delete(m.warmTimers, serial)
	}
}

// Close waits for pending acquisitions and releases all active and warm sessions.
func (m *Manager) Close() {
	m.gate.Lock()
	defer m.gate.Unlock()
	m.closed = true
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[string]*Session)
	for serial := range m.warmTimers {
		m.cancelWarmTimerLocked(serial)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Release()
	}
}

func (m *Manager) Get(serial string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[serial]
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		infos = append(infos, s.Info())
	}
	return infos
}
