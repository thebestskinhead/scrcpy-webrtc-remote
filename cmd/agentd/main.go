// Command agentd serves authenticated HTTP management on loopback.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/agent/host"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
)

const version = "0.1.0"

func main() {
	httpPort := flag.Int("http-port", 17890, "HTTP management port; requires ASTRA_MANAGEMENT_TOKEN")
	bootstrap := flag.String("bootstrap", "",
		"bootstrap JSON file (snake_case GlobalConfig, optional)")
	bootstrapStdin := flag.Bool("bootstrap-stdin", false,
		"read bootstrap JSON from stdin (optional)")
	oneShot := flag.Bool("one-shot", false,
		"exit the process after Stop completes")
	flag.Parse()

	port := *httpPort
	if v := os.Getenv("ASTRA_HTTP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 65535 {
			logger.Error("invalid ASTRA_HTTP_PORT")
			os.Exit(1)
		}
		port = n
	}

	// bootstrap（可选）：仅作启动引导；平台调用 Init 后全量覆盖。
	jsonData, err := loadBootstrap(*bootstrap, *bootstrapStdin)
	if err != nil {
		logger.Error("load bootstrap failed", "err", err)
		os.Exit(1)
	}
	var boot *config.AgentConfig
	if jsonData != nil {
		var ac config.AgentConfig
		if err := json.Unmarshal(jsonData, &ac); err != nil {
			logger.Error("parse bootstrap JSON failed", "err", err)
			os.Exit(1)
		}
		boot = &ac
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}

	defer lis.Close()
	store := agent.NewConfigStore(boot)
	h := host.New(store, lis.Addr().(*net.TCPAddr).Port, version)
	if boot != nil {
		h.MarkBootstrapped()
	}
	if err := serveHTTP(lis, h, *oneShot); err != nil {
		logger.Error("HTTP runtime failed", "err", err)
		os.Exit(1)
	}
}

// loadBootstrap reads the bootstrap JSON from file and/or stdin. Returns nil
// when neither source is provided.
func serveHTTP(lis net.Listener, h *host.Host, oneShot bool) error {
	handler, err := host.NewHTTPServer(h, os.Getenv("ASTRA_MANAGEMENT_TOKEN"))
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	streamCtx, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return streamCtx },
	}
	if oneShot {
		h.OnStop(cancel)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(lis) }()
	logger.Info("agentd HTTP serving", "addr", lis.Addr().String(), "version", version)
	select {
	case <-ctx.Done():
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	cancelStreams()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
		if err == nil {
			err = shutdownErr
		}
	}
	h.Stop()
	return err
}

func loadBootstrap(path string, useStdin bool) ([]byte, error) {
	if path != "" && useStdin {
		return nil, fmt.Errorf("use either --bootstrap or --bootstrap-stdin, not both")
	}
	if path != "" {
		return os.ReadFile(path)
	}
	if useStdin {
		return io.ReadAll(os.Stdin)
	}
	return nil, nil
}
