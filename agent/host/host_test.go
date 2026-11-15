package host

import (
	"sync"
	"sync/atomic"
	"testing"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
)

func TestHostLifecycle(t *testing.T) {
	h := New(agent.NewConfigStore(nil), 17890, "test")
	if h.Start().ErrorCode != "ERR_NOT_INIT" {
		t.Fatal("start before init accepted")
	}
	if h.Init(nil).ErrorCode != "ERR_INVALID_ARG" {
		t.Fatal("nil config accepted")
	}
	if h.Health().Ok {
		t.Fatal("uninitialized host healthy")
	}
	if r := h.Init(&config.AgentConfig{}); !r.Ok || r.ActualPort != 17890 {
		t.Fatalf("init: %+v", r)
	}
	if h.store.Global().Scrcpy.PortPoolSize <= 0 {
		t.Fatal("defaults not applied")
	}
	if h.Init(&config.AgentConfig{}).ErrorCode != "ERR_ALREADY_INIT" {
		t.Fatal("duplicate init accepted")
	}
	if !h.Start().Ok || !h.Start().Ok || !h.Health().Ok {
		t.Fatal("start failed")
	}
	if !h.ReloadConfig(&config.AgentConfig{ServiceID: "updated"}).Ok {
		t.Fatal("reload failed")
	}
	var calls atomic.Int32
	h.OnStop(func() { calls.Add(1) })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.Stop() }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("stop callback count = %d", calls.Load())
	}
	if h.Health().Ok || h.DeviceManager() != nil {
		t.Fatal("stop incomplete")
	}
	if !h.Init(&config.AgentConfig{}).Ok {
		t.Fatal("reinit failed")
	}
	h.Stop()
}

func TestEventHubSlowSubscriberAndConcurrentCancel(t *testing.T) {
	h := NewEventHub(1)
	ch, cancel := h.Subscribe()
	first := &Event{Type: "device_status"}
	h.Publish(first)
	h.Publish(&Event{Type: "session_started"})
	if got := <-ch; got != first {
		t.Fatal("buffered event replaced")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.Publish(first); cancel() }()
	}
	wg.Wait()
	for range ch {
	}
	if len(h.subs) != 0 {
		t.Fatal("subscriber leaked")
	}
}

func TestHostLifecycleAndEvents(t *testing.T) {
	h := New(agent.NewConfigStore(nil), 17890, "test")
	r := h.Init(&config.AgentConfig{ServiceID: "service"})
	if !r.Ok || r.ActualPort != 17890 {
		t.Fatalf("init: %v", r)
	}
	defer h.Stop()
	ch, cancel := h.EventHub().Subscribe()
	defer cancel()
	h.bridge.OnSessionStopped("serial", "instance", "session", "forced")
	e := <-ch
	if e.Type != "session_stopped" || e.Data.Reason != "forced" || e.SessionID != "session" {
		t.Fatal("event lost fields")
	}
	h.bridge.OnQoS(agent.QoSReport{SessionID: "session", Bitrate: 123, FPS: 30})
	e = <-ch
	if e.Data.QoS == nil || e.Data.QoS.Bitrate != 123 {
		t.Fatal("QoS lost bitrate")
	}
	h.bridge.OnSessionStopped("serial", "instance", "session", "done")
	if len(h.bridge.qosAt) != 0 {
		t.Fatal("QoS throttle entry leaked")
	}
}
