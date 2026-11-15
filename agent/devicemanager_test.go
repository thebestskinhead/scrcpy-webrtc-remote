package agent

import (
	"sync"
	"testing"

	"scrcpy-webrtc-remote/pkg/config"
)

func TestDeviceManagerConcurrentLifecycle(t *testing.T) {
	store := NewConfigStore(&config.AgentConfig{})
	dm := NewDeviceManager(store, NewSharedPortPool(30000, 2), nil)
	dm.run = func(*Controller) {}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = dm.Prepare("test", "instance", nil, ConnectParams{})
				_ = dm.Reset("test", "test")
				_ = dm.Prepare("test", "instance", nil, ConnectParams{})
				_ = dm.Release("test", "test")
			}
		}()
	}
	wg.Wait()
	if err := dm.Prepare("test", "instance", nil, ConnectParams{}); err != nil {
		t.Fatal(err)
	}
	if err := dm.Prepare("test", "other", nil, ConnectParams{}); err == nil {
		t.Fatal("changed live instance identity")
	}
	dm.StopAll()
	if err := dm.Prepare("test", "instance", nil, ConnectParams{}); err == nil {
		t.Fatal("prepared after stop")
	}
	if dm.Get("test") != nil {
		t.Fatal("controller survived stop")
	}
}

func TestConfigSnapshotsOwnMutableValues(t *testing.T) {
	cfg := &config.AgentConfig{STUNServers: []string{"original"}, TURNServer: &config.TurnServerConfig{URLs: []string{"turn:original"}}}
	store := NewConfigStore(cfg)
	cfg.STUNServers[0] = "changed"
	cfg.TURNServer.URLs[0] = "changed"
	size := 720
	store.SetDeviceOverrides("test", &config.DeviceConfig{MaxSize: &size})
	size = 1
	first := store.ForDevice("test")
	if first.Scrcpy.MaxSize != 720 || first.STUNServers[0] != "original" || first.TURNServer.URLs[0] != "turn:original" {
		t.Fatal("input mutation changed stored config")
	}
	first.STUNServers[0] = "changed"
	first.TURNServer.URLs[0] = "changed"
	if store.Global().STUNServers[0] != "original" || store.Global().TURNServer.URLs[0] != "turn:original" {
		t.Fatal("snapshot mutated store")
	}
}

func TestConcurrentConnectionParams(t *testing.T) {
	c := testController()
	defer c.Stop()
	cp := ConnectParams{WSHeaders: map[string]string{"test": "original"}}
	c.SetConnectParams(cp)
	cp.WSHeaders["test"] = "changed"
	if c.connectionParams().WSHeaders["test"] != "original" {
		t.Fatal("parameter alias")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.SetConnectParams(cp)
			snapshot := c.connectionParams()
			snapshot.WSHeaders["test"] = "private"
		}()
	}
	wg.Wait()
}
