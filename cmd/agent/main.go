package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
)

func main() {
	configPath := flag.String("c", "./config/agent.yaml", "config file path")
	flag.Parse()

	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	instances := cfg.Instances()
	if len(instances) == 0 {
		log.Fatalf("no instances configured")
	}

	logger.Info("agent starting",
		"signaling", cfg.SignalingURL,
		"service", cfg.ServiceID,
		"instance_count", len(instances))

	// Auto-connect TCP/IP devices (e.g. 127.0.0.1:16384) that are not yet
	// listed in `adb devices`.
	agent.AutoConnectDevices(*cfg, instances)

	// Create a shared port pool across all instances.
	// Each Controller had its own pool with the same range [30000..30099],
	// causing ADB forward conflicts when two instances tried to use the
	// same local TCP port for different devices.
	sharedPool := agent.NewSharedPortPool(cfg.Scrcpy.PortPoolStart, cfg.Scrcpy.PortPoolSize)
	logger.Info("shared port pool created",
		"start", cfg.Scrcpy.PortPoolStart,
		"size", cfg.Scrcpy.PortPoolSize)

	var wg sync.WaitGroup

	for _, inst := range instances {
		wg.Add(1)
		go func(inst config.InstanceConfig) {
			defer wg.Done()
			ctrl, err := agent.NewController(*cfg, inst, sharedPool)
			if err != nil {
				logger.Error("create controller failed",
					"instance", inst.InstanceID, "err", err)
				return
			}
			ctrl.Run() // infinite retry loop, only returns on os.Exit
		}(inst)
	}

	// Graceful shutdown
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		logger.Info("shutting down agent")
		os.Exit(0)
	}()

	wg.Wait()
}
