package gateway

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"scrcpy-webrtc-remote/agent/internal/adb"
	"scrcpy-webrtc-remote/agent/internal/portpool"
	"testing"
	"time"

	"scrcpy-webrtc-remote/pkg/config"
)

func TestDeviceSmoke(t *testing.T) {
	serial := os.Getenv("ASTRA_TEST_DEVICE")
	if serial == "" {
		t.Skip("set ASTRA_TEST_DEVICE to explicitly enable device tests")
	}
	jar, err := filepath.Abs("../../../scrcpy-server.jar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jar); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		audio, forward bool
	}{
		{"audio", true, true}, {"silent", false, true}, {"no_forwarders", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			pool := portpool.New(port, 1)
			s := NewSession(serial, adb.New("adb"), pool, config.ScrcpyConfig{
				JarPath: jar, ServerVersion: "4.0", MaxSize: 1280, VideoBitRate: 4000000,
				AudioBitRate: 128000, VideoCodec: "h264", AudioCodec: "opus",
				AudioEnabled: tc.audio, PowerOn: true, VideoKeyframeInterval: 2,
			})
			t.Cleanup(func() {
				start := time.Now()
				done := make(chan struct{})
				go func() { s.Release(); s.Release(); close(done) }()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Error("release exceeded 15s")
					return
				}
				t.Logf("release completed in %s", time.Since(start))
				if s.State() != StateReleased || s.Alive() || s.adbPort != 0 {
					t.Error("incomplete release")
				}
				p, err := pool.Acquire()
				if err != nil || p != port {
					t.Errorf("port not returned: %d, %v", p, err)
				} else {
					pool.Release(p)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				out, err := exec.CommandContext(ctx, "adb", "-s", serial, "forward", "--list").Output()
				if err != nil {
					t.Errorf("check forwards: %v", err)
				}
				for _, line := range strings.Split(string(out), "\n") {
					fields := strings.Fields(line)
					if len(fields) >= 2 && fields[0] == serial && fields[1] == fmt.Sprintf("tcp:%d", port) {
						t.Error("test forward survived release")
					}
				}
			})
			start := time.Now()
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			t.Logf("started in %s", time.Since(start))
			if !tc.forward {
				return
			}
			s.SetForwardPaused(false)
			s.StartForwarders()
			if tc.audio {
				if err := s.SendControl(BuildStartAudio()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.SendControl(BuildRequestKeyframe()); err != nil {
				t.Fatal(err)
			}
			video, audio, keys, videoBytes, audioBytes := 0, 0, 0, 0, 0
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			for {
				select {
				case packet, ok := <-s.VideoCh:
					if !ok {
						t.Fatal("video ended early")
					}
					if len(packet.Data) > 0 {
						video++
						videoBytes += len(packet.Data)
						if packet.KeyFrame {
							keys++
						}
					}
				case packet, ok := <-s.AudioCh:
					if !ok {
						t.Fatal("audio ended early")
					}
					if len(packet.Data) > 0 {
						audio++
						audioBytes += len(packet.Data)
					}
				case _, ok := <-s.CtrlCh:
					if !ok {
						t.Fatal("control ended early")
					}
				case <-timer.C:
					t.Logf("video=%d frames/%d bytes keyframes=%d audio=%d frames/%d bytes; control write succeeded (no input injection)", video, videoBytes, keys, audio, audioBytes)
					if video == 0 || keys == 0 {
						t.Error("missing video/keyframe")
					}
					if tc.audio && audio == 0 {
						t.Error("missing audio packets")
					}
					return
				}
			}
		})
	}
}

func TestReleaseWithoutForwarders(t *testing.T) {
	s := NewSession("test", nil, nil, config.ScrcpyConfig{})
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); s.Release() }()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("release blocked")
	}
	s.StartForwarders()
	if s.forwardersStarted.Load() {
		t.Fatal("forwarders started after release")
	}
	select {
	case <-s.Died():
	default:
		t.Fatal("missing termination notification")
	}
}
