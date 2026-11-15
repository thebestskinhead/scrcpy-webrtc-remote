// Command agentdrv drives the HTTP runtime from platform-owned YAML configuration.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	configPath := flag.String("c", "./config/agent.yaml", "platform-owned agent YAML")
	endpoint := flag.String("http", "http://127.0.0.1:17890", "HTTP management URL")
	binary := flag.String("agentd", "", "optional agentd executable to spawn")
	events := flag.Bool("events", false, "print SSE events")
	connectADB := flag.Bool("connect-adb", false, "explicitly connect configured ADB devices")
	flag.Parse()
	token := os.Getenv("ASTRA_MANAGEMENT_TOKEN")
	if token == "" {
		return fmt.Errorf("ASTRA_MANAGEMENT_TOKEN is required")
	}
	u, err := url.Parse(*endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("invalid management URL")
	}
	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		return err
	}
	instances := cfg.Instances()
	if len(instances) == 0 {
		return fmt.Errorf("no instances configured")
	}
	if *binary != "" {
		if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" {
			return fmt.Errorf("spawn requires http://127.0.0.1:<port>")
		}
		executable, err := filepath.Abs(*binary)
		if err != nil {
			return err
		}
		child := exec.Command(executable, "--http-port", u.Port())
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return err
		}
		defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
		if err := waitTCP(u.Host, 10*time.Second); err != nil {
			return err
		}
	}
	if *connectADB {
		agent.AutoConnectDevices(*cfg, instances)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(ctx context.Context, path string, body interface{}) error {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(*endpoint, "/")+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		var result struct {
			Ok      bool   `json:"ok"`
			Code    string `json:"error_code"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
			return err
		}
		if response.StatusCode != 200 || !result.Ok {
			return fmt.Errorf("%s: %s %s", path, result.Code, result.Message)
		}
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := request(ctx, "/v1/init", map[string]interface{}{"global_config": cfg}); err != nil {
		return err
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := request(stop, "/v1/stop", nil); err != nil {
			log.Printf("stop: %v", err)
		}
	}()
	if err := request(ctx, "/v1/start", nil); err != nil {
		return err
	}
	for _, instance := range instances {
		if err := request(ctx, "/v1/devices", map[string]string{"device_serial": instance.DeviceSerial, "instance_id": instance.InstanceID}); err != nil {
			return err
		}
		log.Printf("prepared instance=%s serial=%s", instance.InstanceID, instance.DeviceSerial)
	}
	if *events {
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(*endpoint, "/")+"/v1/events", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		streamClient := &http.Client{CheckRedirect: client.CheckRedirect}
		response, err := streamClient.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return fmt.Errorf("events status %d", response.StatusCode)
		}
		go func() {
			scanner := bufio.NewScanner(response.Body)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "data:") {
					log.Print(scanner.Text())
				}
			}
		}()
	}
	<-ctx.Done()
	return nil
}

func waitTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}
