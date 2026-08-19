// Command agentd — 平台托管 sidecar 入口。
// 进程内起本地 gRPC server（127.0.0.1:<port>），平台以任意语言接入完成
// 生命周期与设备管理。见 docs/PLATFORM-API.md §8.3。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"

	agentapi "scrcpy-webrtc-remote/api/gen"
	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/agent/host"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
)

const version = "0.1.0"

func main() {
	grpcPort := flag.Int("grpc-port", 17890,
		"gRPC listen port (override with env AGENT_GRPC_PORT)")
	bootstrap := flag.String("bootstrap", "",
		"bootstrap JSON file (snake_case GlobalConfig, optional)")
	bootstrapStdin := flag.Bool("bootstrap-stdin", false,
		"read bootstrap JSON from stdin (optional)")
	oneShot := flag.Bool("one-shot", false,
		"exit the process after Stop completes")
	flag.Parse()

	port := *grpcPort
	if v := os.Getenv("AGENT_GRPC_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		} else {
			logger.Warn("invalid AGENT_GRPC_PORT, ignoring", "value", v)
		}
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

	store := agent.NewConfigStore(boot)
	h := host.New(store, port, version)
	if boot != nil {
		h.MarkBootstrapped()
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}

	gs := grpc.NewServer()
	agentapi.RegisterAgentServiceServer(gs, host.NewGRPCServer(h))

	go func() {
		logger.Info("agentd gRPC serving", "addr", addr, "version", version)
		if err := gs.Serve(lis); err != nil {
			logger.Error("grpc serve failed", "err", err)
			os.Exit(1)
		}
	}()

	// --one-shot：平台调 Stop 后进程自收尾（延迟退出，先让 Stop RPC 返回 OK）。
	if *oneShot {
		h.OnStop(func() {
			logger.Info("one-shot mode: exiting after Stop")
			go func() {
				time.Sleep(300 * time.Millisecond)
				os.Exit(0)
			}()
		})
	}

	// SIGINT/SIGTERM 优雅退出：回收全部设备 → 关闭 gRPC → 退出。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("shutdown signal received")
	h.Stop()
	gs.GracefulStop()
	logger.Info("agentd exited cleanly")
}

// loadBootstrap reads the bootstrap JSON from file and/or stdin. Returns nil
// when neither source is provided.
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
