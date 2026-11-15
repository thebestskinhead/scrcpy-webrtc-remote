package agent

import (
	"sync"
	"testing"
	"time"

	"scrcpy-webrtc-remote/pkg/common"
	"scrcpy-webrtc-remote/pkg/config"
)

func testController() *Controller {
	return newController(NewConfigStore(&config.AgentConfig{}), config.InstanceConfig{DeviceSerial: "test"}, NewSharedPortPool(30000, 1), nil, ConnectParams{})
}

func TestControllerConcurrentStop(t *testing.T) {
	c := testController()
	ctx := c.newSessionCtx("session")
	if ctx.sessionReady == nil {
		t.Fatal("missing readiness channel")
	}
	c.sessions[ctx.id] = ctx
	c.currentSessionID = ctx.id
	ctx.prepareWG.Add(1)
	go ctx.run()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Stop() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.stopChan:
	case <-time.After(time.Second):
		t.Fatal("stop not signaled")
	}
	select {
	case <-done:
		t.Error("stop returned before prepare completed")
	default:
	}
	ctx.prepareWG.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not finish")
	}
	select {
	case <-ctx.done:
	default:
		t.Error("session not cleaned up")
	}
	if _, _, err := c.manager.Acquire("test"); err == nil {
		t.Error("manager still accepting sessions")
	}
	if err := c.handleBound(common.WsMsg{SessionID: "late"}); err == nil {
		t.Error("accepted session after stop")
	}
	if c.CurrentSession() != nil {
		t.Error("active session after stop")
	}
}

func TestSessionConcurrentStop(t *testing.T) {
	ctx := &SessionCtx{stopChan: make(chan struct{})}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ctx.stop() }()
	}
	wg.Wait()
	select {
	case <-ctx.stopChan:
	default:
		t.Fatal("stop not signaled")
	}
}

func TestStoppedControllerRunReturns(t *testing.T) {
	c := testController()
	c.Stop()
	done := make(chan struct{})
	go func() { c.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stopped controller attempted reconnect")
	}
}

func TestDuplicateBoundKeepsSession(t *testing.T) {
	c := testController()
	defer c.Stop()
	msg := common.WsMsg{SessionID: "session"}
	if err := c.handleBound(msg); err != nil {
		t.Fatal(err)
	}
	first := c.CurrentSession()
	if err := c.handleBound(msg); err != nil {
		t.Fatal(err)
	}
	if c.CurrentSession() != first {
		t.Fatal("duplicate bound replaced session")
	}
}
