package gateway

import (
	"errors"
	"scrcpy-webrtc-remote/pkg/config"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerConcurrentAcquire(t *testing.T) {
	m := NewManager(nil, nil, config.ScrcpyConfig{})
	var starts atomic.Int32
	m.start = func(*Session) error { starts.Add(1); return nil }
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := m.Acquire("device"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("starts = %d", starts.Load())
	}
	m.Close()
	if _, _, err := m.Acquire("device"); err == nil {
		t.Fatal("acquire after close succeeded")
	}
}

func TestManagerStaleTimer(t *testing.T) {
	m := NewManager(nil, nil, config.ScrcpyConfig{})
	m.start = func(*Session) error { return nil }
	s, _, _ := m.Acquire("device")
	m.ReleaseDeferred("device", time.Hour)
	old := m.warmTimers["device"]
	m.Acquire("device")
	m.ReleaseDeferred("device", time.Hour)
	m.expireWarm("device", s, old)
	if m.Get("device") != s {
		t.Fatal("stale timer removed current session")
	}
	m.Close()
	if s.State() != StateReleased {
		t.Fatal("warm session survived close")
	}
}

func TestManagerStartFailure(t *testing.T) {
	m := NewManager(nil, nil, config.ScrcpyConfig{})
	m.start = func(*Session) error { return errors.New("start failed") }
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := m.Acquire("device"); err == nil {
			t.Error("missing failure")
		}
		m.Close()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failed start cleanup blocked")
	}
}

func TestManagerConfigurationChangeReplacesWarmSession(t *testing.T) {
	m := NewManager(nil, nil, config.ScrcpyConfig{MaxSize: 720})
	m.start = func(*Session) error { return nil }
	defer m.Close()
	first, _, err := m.Acquire("device")
	if err != nil {
		t.Fatal(err)
	}
	m.ReleaseDeferred("device", time.Hour)
	next, reused, err := m.AcquireConfigured("device", nil, config.ScrcpyConfig{MaxSize: 1080})
	if err != nil || reused || next == first || next.cfg.MaxSize != 1080 {
		t.Fatalf("config not applied: reused=%v err=%v", reused, err)
	}
	if first.State() != StateReleased {
		t.Fatal("old warm session survived")
	}
}

func TestManagerReleaseExpectedInstance(t *testing.T) {
	m := NewManager(nil, nil, config.ScrcpyConfig{})
	m.start = func(*Session) error { return nil }
	first, _, _ := m.Acquire("device")
	m.Release("device")
	second, _, _ := m.Acquire("device")
	m.ReleaseSession("device", first)
	if m.Get("device") != second {
		t.Fatal("old owner removed replacement")
	}
	m.Close()
}
