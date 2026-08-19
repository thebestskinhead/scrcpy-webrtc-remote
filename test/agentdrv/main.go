// Command agentdrv — 目标2：模拟平台 driver（属测试步骤，非业务代码）。
//
// 背景：目标1（test/runner.py 自动化用例）覆盖不到完整 WebRTC 链路，不能证明
// sidecar（cmd/agentd）真实可用。目标2 是一个独立的小程序，模拟"平台"侧：
// 沿用改造前逻辑从 agent.yaml 读配置，经 gRPC 把配置注入并驱动 sidecar
// 实现改造前的全部功能（连 signaling → scrcpy → WebRTC），随后用浏览器
// 打开 /app/ 人工验证 sidecar 真的可用。
//
// 由于暂不改造信令服务器，配置仍来自 YAML（复用 pkg/config.LoadAgent），
// 只是把"agent 自己读文件启动"变成"平台读文件后经 gRPC 注入 sidecar"。
//
// 用法:
//
//	# 1) 先启动 signaling 与 agentd
//	go run ./cmd/signaling -c config/signaling.yaml
//	agentd --grpc-port 17890
//
//	# 2) 运行 driver（读 config/agent.yaml → Init/Start/PrepareDevice）
//	go run ./test/agentdrv -c config/agent.yaml --grpc 127.0.0.1:17890 --events
//
//	# 或让 driver 自动拉起 agentd 子进程（模拟平台托管 sidecar）
//	go run ./test/agentdrv -c config/agent.yaml --agentd build/test/agentd.exe --events
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentapi "scrcpy-webrtc-remote/api/gen"
	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
)

func main() {
	configPath := flag.String("c", "./config/agent.yaml", "agent config file path (YAML)")
	grpcAddr := flag.String("grpc", "127.0.0.1:17890", "agentd gRPC address")
	agentdBin := flag.String("agentd", "", "spawn agentd binary at this path (empty = assume already running)")
	events := flag.Bool("events", false, "print StreamEvents as they arrive")
	flag.Parse()

	// ---- 1. 读配置（改造前逻辑：YAML） ----
	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	instances := cfg.Instances()
	if len(instances) == 0 {
		log.Fatalf("no instances configured")
	}
	log.Printf("config loaded: signaling=%s service=%s instances=%d adb=%s",
		cfg.SignalingURL, cfg.ServiceID, len(instances), cfg.ADBPath)

	// ---- 2. 可选：以子进程拉起 agentd（模拟平台托管 sidecar） ----
	var child *exec.Cmd
	if *agentdBin != "" {
		// Windows 下 exec 不解析裸相对路径，统一转绝对路径
		agentdPath, err := filepath.Abs(*agentdBin)
		if err != nil {
			log.Fatalf("resolve agentd path: %v", err)
		}
		if _, err := os.Stat(agentdPath); err != nil {
			log.Fatalf("agentd not found: %s (%v)", agentdPath, err)
		}
		port := grpcPortOf(*grpcAddr)
		child = exec.Command(agentdPath, "--grpc-port", strconv.Itoa(port))
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			log.Fatalf("spawn agentd: %v", err)
		}
		log.Printf("agentd spawned pid=%d addr=%s", child.Process.Pid, *grpcAddr)
		defer child.Process.Kill()
		if err := waitTCP(*grpcAddr, 10*time.Second); err != nil {
			log.Fatalf("agentd not ready: %v", err)
		}
	}

	// ---- 2.5 adb connect（沿用改造前 AutoConnectDevices 逻辑；平台负责设备准备） ----
	agent.AutoConnectDevices(*cfg, instances)

	// ---- 3. gRPC 驱动 sidecar：Init → Start → PrepareDevice ----
	conn, err := grpc.NewClient(*grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("grpc dial: %v", err)
	}
	defer conn.Close()
	stub := agentapi.NewAgentServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initResp, err := stub.Init(ctx, &agentapi.InitRequest{
		GlobalConfig: config.ToProtoGlobalConfig(cfg),
	})
	if err != nil {
		log.Fatalf("Init rpc: %v", err)
	}
	if !initResp.Ok {
		log.Fatalf("Init failed: %s %s", initResp.ErrorCode, initResp.Message)
	}
	log.Printf("Init ok (actual_port=%d)", initResp.ActualPort)

	startResp, err := stub.Start(ctx, &agentapi.Empty{})
	if err != nil {
		log.Fatalf("Start rpc: %v", err)
	}
	if !startResp.Ok {
		log.Fatalf("Start failed: %s %s", startResp.ErrorCode, startResp.Message)
	}
	log.Printf("Start ok")

	for _, inst := range instances {
		prepResp, err := stub.PrepareDevice(ctx, &agentapi.PrepareDeviceRequest{
			InstanceId:   inst.InstanceID,
			DeviceSerial: inst.DeviceSerial,
		})
		if err != nil {
			log.Fatalf("PrepareDevice(%s) rpc: %v", inst.DeviceSerial, err)
		}
		if !prepResp.Ok {
			log.Fatalf("PrepareDevice(%s) failed: %s %s",
				inst.DeviceSerial, prepResp.ErrorCode, prepResp.Message)
		}
		log.Printf("device prepared: instance=%s serial=%s", inst.InstanceID, inst.DeviceSerial)
	}

	// ---- 4. 可选事件流（观察会话/QoS/错误） ----
	if *events {
		stream, err := stub.StreamEvents(ctx, &agentapi.Empty{})
		if err != nil {
			log.Printf("StreamEvents: %v", err)
		} else {
			go func() {
				for {
					ev, err := stream.Recv()
					if err != nil {
						log.Printf("event stream closed: %v", err)
						return
					}
					log.Printf("EVENT %s", formatEvent(ev))
				}
			}()
		}
	}

	// ---- 5. 等待退出信号 → 优雅 Stop ----
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutdown signal, calling Stop")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	stopResp, err := stub.Stop(stopCtx, &agentapi.Empty{})
	if err != nil {
		log.Printf("Stop rpc: %v", err)
	} else if !stopResp.Ok {
		log.Printf("Stop: %s %s", stopResp.ErrorCode, stopResp.Message)
	} else {
		log.Printf("Stop ok")
	}
}

func grpcPortOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 17890
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 17890
	}
	return n
}

func waitTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}

func formatEvent(ev *agentapi.AgentEvent) string {
	parts := []string{"serial=" + ev.GetDeviceSerial(), "inst=" + ev.GetInstanceId()}
	if ev.GetSessionId() != "" {
		parts = append(parts, "session="+ev.GetSessionId())
	}
	switch e := ev.Event.(type) {
	case *agentapi.AgentEvent_DeviceStatus:
		parts = append(parts, "device_status", "busy="+strconv.FormatBool(e.DeviceStatus.Busy))
	case *agentapi.AgentEvent_SessionStarted:
		parts = append(parts, "session_started")
	case *agentapi.AgentEvent_SessionStopped:
		parts = append(parts, "session_stopped", "reason="+e.SessionStopped.Reason)
	case *agentapi.AgentEvent_StreamDead:
		parts = append(parts, "stream_dead")
	case *agentapi.AgentEvent_Qos:
		q := e.Qos
		parts = append(parts, "qos", "loss="+strconv.FormatFloat(q.LossRate, 'f', 2, 64),
			"rtt="+strconv.FormatFloat(q.RttMs, 'f', 0, 64)+"ms",
			"bitrate="+strconv.Itoa(int(q.Bitrate))+"bps",
			"fps="+strconv.FormatFloat(q.Fps, 'f', 1, 64))
	case *agentapi.AgentEvent_AgentError:
		parts = append(parts, "agent_error", e.AgentError.Code+": "+e.AgentError.Message)
	default:
		parts = append(parts, "unknown_event")
	}
	return strings.Join(parts, " ")
}
