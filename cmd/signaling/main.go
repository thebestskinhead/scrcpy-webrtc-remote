// Command signaling — signaling server for the scrcpy WebRTC bridge.
// Handles agent WebSocket registration, browser WebSocket signaling
// (/ws/browser/{svc}/{inst}), relay queries, the /api/* endpoints and
// static frontend hosting (signaling/static, the app UI under /app/).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"scrcpy-webrtc-remote/pkg/common"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
	"scrcpy-webrtc-remote/pkg/sessionauth"
	"scrcpy-webrtc-remote/pkg/signaling"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize: 1024, WriteBufferSize: 1024,
}

func main() {
	configPath := flag.String("c", "./config/signaling.yaml", "config file path")
	managementURL := flag.String("management-url", "http://127.0.0.1:17890", "Astra HTTP management URL")
	flag.Parse()
	agentToken := os.Getenv("ASTRA_AGENT_TOKEN")
	if agentToken == "" || agentToken == os.Getenv("ASTRA_MANAGEMENT_TOKEN") {
		log.Fatal("ASTRA_AGENT_TOKEN is required and must differ from ASTRA_MANAGEMENT_TOKEN")
	}
	auth, err := sessionauth.NewClient(*managementURL, os.Getenv("ASTRA_MANAGEMENT_TOKEN"))
	if err != nil {
		log.Fatalf("session authorization: %v", err)
	}

	cfg, err := config.LoadSignaling(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	opts := signaling.Options{STUNServers: cfg.WebRTC.STUNServers}
	if len(cfg.WebRTC.TURNServers) > 0 {
		t := cfg.WebRTC.TURNServers[0]
		opts.TURNServer = &common.IceServerEntry{URLs: t.URLs}
		if t.Auth != nil {
			opts.TURNServer.Username = t.Auth.Username
			opts.TURNServer.Credential = t.Auth.Credential
		}
	}

	sig := signaling.New(opts)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/agent/", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r, agentToken) {
			return
		}
		svc := pathLast(r.URL.Path)
		if svc == "" {
			http.Error(w, "missing service_id", http.StatusBadRequest)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sig.HandleAgentWS(conn, svc)
	})

	// Browser WebSocket — the app frontend (signaling/static/app) connects
	// here for WebRTC signaling (bound / offer / answer / ICE / QoS).
	mux.HandleFunc("/ws/browser/", authorizedWS(sig, auth, false))
	mux.HandleFunc("/ws/relay/", authorizedWS(sig, auth, true))

	// Health check for internal monitoring
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","agents":%d}`, len(sig.Agents()))
	})

	// API — agent list / health
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r, os.Getenv("ASTRA_MANAGEMENT_TOKEN")) {
			return
		}
		type resp struct {
			ServiceID    string `json:"service_id"`
			InstanceID   string `json:"instance_id"`
			DeviceSerial string `json:"device_serial"`
			Online       bool   `json:"online"`
		}
		agents := sig.Agents()
		list := make([]resp, len(agents))
		for i := range agents {
			// index access — range-by-value would copy AgentInfo (contains a
			// sync.Mutex) and trip `go vet`'s copylocks check
			list[i] = resp{agents[i].ServiceID, agents[i].InstanceID, agents[i].DeviceSerial, true}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","agent_count":%d}`, len(sig.Agents()))
	})

	// Static frontend (signaling/static — the app UI lives under /app/)
	if cfg.StaticDir != "" {
		logger.Info("serving static files", "dir", cfg.StaticDir)
		mux.Handle("/", http.FileServer(http.Dir(cfg.StaticDir)))
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		logger.Info("shutting down")
		sig.Close()
		os.Exit(0)
	}()

	logger.Info("signaling starting (agent+browser+static)", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func pathLast(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

func pathLast2(path string) (string, string) {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) < 2 {
		return "", ""
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}
