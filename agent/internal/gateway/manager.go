package gateway

import (
	"sync"
	"time"

	"scrcpy-webrtc-remote/agent/internal/adb"
	"scrcpy-webrtc-remote/agent/internal/portpool"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
)

type Manager struct {
	adb      *adb.Client
	portPool *portpool.Pool
	cfg      config.ScrcpyConfig
	sessions map[string]*Session
	// warmTimers tracks deferred releases (warm pool). A session with a
	// pending timer stays fully alive (scrcpy server running, forwarding
	// paused) so a reconnect within the window reuses it instantly.
	warmTimers map[string]*time.Timer
	mu         sync.Mutex
}

func NewManager(adbClient *adb.Client, portPool *portpool.Pool, cfg config.ScrcpyConfig) *Manager {
	return &Manager{
		adb:        adbClient,
		portPool:   portPool,
		cfg:        cfg,
		sessions:   make(map[string]*Session),
		warmTimers: make(map[string]*time.Timer),
	}
}

// Acquire returns a usable session for the serial, reusing a warm one when
// possible. reused=true means the scrcpy server was already running (no cold
// start); the caller should re-sync bitrate and request a keyframe.
func (m *Manager) Acquire(serial string) (s *Session, reused bool, err error) {
	m.mu.Lock()
	if existing, ok := m.sessions[serial]; ok && existing.Alive() {
		m.cancelWarmTimerLocked(serial)
		m.mu.Unlock()
		logger.Info("reusing existing session (warm)", "serial", serial)
		return existing, true, nil
	}

	var old *Session
	if existing, ok := m.sessions[serial]; ok {
		old = existing
		delete(m.sessions, serial)
		m.cancelWarmTimerLocked(serial)
	}
	m.mu.Unlock()

	// Release old session outside the lock (Release can block).
	if old != nil {
		logger.Info("old session in bad state, cleaning up", "serial", serial, "state", old.State())
		old.Release()
	}

	logger.Info("creating new session", "serial", serial)
	s = NewSession(serial, m.adb, m.portPool, m.cfg)
	if err := s.Start(); err != nil {
		s.Release()
		return nil, false, err
	}

	m.mu.Lock()
	m.sessions[serial] = s
	m.mu.Unlock()
	return s, false, nil
}

// Release removes the session immediately and cancels any warm timer.
func (m *Manager) Release(serial string) {
	m.mu.Lock()
	s, ok := m.sessions[serial]
	if ok {
		delete(m.sessions, serial)
	}
	m.cancelWarmTimerLocked(serial)
	m.mu.Unlock()

	if ok {
		s.Release()
	}
}

// ReleaseDeferred keeps the scrcpy server alive for d after the browser
// disconnects (warm pool). Forwarding is paused so frames are dropped while
// idle; a reconnect within the window reuses the session via Acquire.
// A session that has already died is released immediately instead.
func (m *Manager) ReleaseDeferred(serial string, d time.Duration) {
	m.mu.Lock()
	s, ok := m.sessions[serial]
	if !ok {
		m.mu.Unlock()
		return
	}
	if !s.Alive() {
		delete(m.sessions, serial)
		m.cancelWarmTimerLocked(serial)
		m.mu.Unlock()
		logger.Info("session already dead, releasing immediately", "serial", serial)
		s.Release()
		return
	}
	s.SetForwardPaused(true)
	m.cancelWarmTimerLocked(serial)
	m.warmTimers[serial] = time.AfterFunc(d, func() { m.expireWarm(serial) })
	m.mu.Unlock()
	logger.Info("session kept warm after disconnect",
		"serial", serial, "warm_keep", d.String())
}

// expireWarm releases the session if it is still warm (not re-acquired).
func (m *Manager) expireWarm(serial string) {
	m.mu.Lock()
	_, stillWarm := m.warmTimers[serial]
	delete(m.warmTimers, serial)
	s, ok := m.sessions[serial]
	if ok {
		delete(m.sessions, serial)
	}
	m.mu.Unlock()

	if !stillWarm || !ok {
		return
	}
	logger.Info("warm keep expired, releasing scrcpy session", "serial", serial)
	s.Release()
}

// cancelWarmTimerLocked stops and removes the warm timer for serial.
// Caller must hold m.mu.
func (m *Manager) cancelWarmTimerLocked(serial string) {
	if t, ok := m.warmTimers[serial]; ok {
		t.Stop()
		delete(m.warmTimers, serial)
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

	var infos []Info
	for _, s := range m.sessions {
		infos = append(infos, s.Info())
	}
	return infos
}
