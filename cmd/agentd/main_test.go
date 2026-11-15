package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/agent/host"
)

func TestHTTPRuntimeOneShot(t *testing.T) {
	t.Setenv("ASTRA_MANAGEMENT_TOKEN", "test-token")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	h := host.New(agent.NewConfigStore(nil), lis.Addr().(*net.TCPAddr).Port, "test")
	done := make(chan error, 1)
	go func() { done <- serveHTTP(lis, h, true) }()
	client := &http.Client{Timeout: 3 * time.Second}
	request := func(path, body string) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://"+lis.Addr().String()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("response %d %s err=%v", response.StatusCode, data, err)
		}
	}
	request("/v1/init", `{"global_config":{}}`)
	request("/v1/stop", "")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP runtime did not stop")
	}
}
