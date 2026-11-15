package host

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
)

func TestHTTPManagement(t *testing.T) {
	h := New(agent.NewConfigStore(nil), 17890, "test")
	defer h.Stop()
	s, err := NewHTTPServer(h, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewHTTPServer(h, ""); err == nil {
		t.Fatal("empty token accepted")
	}
	cases := []struct {
		name, method, path, body, token string
		status                          int
	}{
		{"missing auth", "GET", "/v1/health", "", "", 401},
		{"wrong auth", "GET", "/v1/health", "", "bad", 401},
		{"query token rejected", "GET", "/v1/health?token=test-token", "", "", 401},
		{"uninitialized", "GET", "/v1/devices", "", "test-token", 409},
		{"unknown field", "POST", "/v1/init", `{"global_config":{},"typo":1}`, "test-token", 400},
		{"multiple objects", "POST", "/v1/init", `{"global_config":{}}{}`, "test-token", 400},
		{"missing config", "POST", "/v1/init", `{}`, "test-token", 400},
		{"init", "POST", "/v1/init", `{"global_config":{}}`, "test-token", 200},
		{"duplicate init", "POST", "/v1/init", `{"global_config":{}}`, "test-token", 409},
		{"start", "POST", "/v1/start", "", "test-token", 200},
		{"list", "GET", "/v1/devices", "", "test-token", 200},
		{"invalid device", "POST", "/v1/devices", `{"instance_id":"test"}`, "test-token", 400},
		{"missing device", "POST", "/v1/devices/missing/reset", `{}`, "test-token", 404},
		{"missing session", "POST", "/v1/sessions/missing/close", `{}`, "test-token", 404},
		{"reload", "PUT", "/v1/config", `{"global_config":{"service_id":"updated"}}`, "test-token", 200},
		{"wrong method", "DELETE", "/v1/config", `{}`, "test-token", 405},
		{"stop", "POST", "/v1/stop", "", "test-token", 200},
		{"stopped", "GET", "/v1/devices", "", "test-token", 409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func TestHTTPBodyLimits(t *testing.T) {
	s, _ := NewHTTPServer(New(agent.NewConfigStore(nil), 0, "test"), "token")
	for _, tc := range []struct {
		contentType, body string
		status            int
	}{
		{"text/plain", `{}`, 415},
		{"application/json", `{"global_config":{"service_id":"` + strings.Repeat("a", maxRequestBytes) + `"}}`, 413},
	} {
		r := httptest.NewRequest("POST", "/v1/init", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("Content-Type", tc.contentType)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("status %d want %d", w.Code, tc.status)
		}
	}
}

func TestHTTPSSEDisconnect(t *testing.T) {
	h := New(agent.NewConfigStore(nil), 0, "test")
	handler, _ := NewHTTPServer(h, "token")
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("invalid SSE response")
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != ": connected\n" {
		t.Fatalf("initial frame %q %v", line, err)
	}
	h.EventHub().Publish(&Event{Type: "device_status", DeviceSerial: "test"})
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			if !strings.Contains(line, `"device_serial":"test"`) {
				t.Fatal(line)
			}
			break
		}
	}
	cancel()
	_, _ = io.Copy(io.Discard, response.Body)
	deadline := time.Now().Add(time.Second)
	for {
		h.hub.mu.RLock()
		count := len(h.hub.subs)
		h.hub.mu.RUnlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SSE subscription leaked")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHostManagementConcurrentStop(t *testing.T) {
	h := New(agent.NewConfigStore(nil), 0, "test")
	h.Init(&config.AgentConfig{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ListDevices()
			h.Health()
			h.ReleaseDevice("missing", "test")
			h.ResetDevice("missing", "test")
			h.ForceCloseSession("missing", "test")
			h.Stop()
		}()
	}
	wg.Wait()
}
