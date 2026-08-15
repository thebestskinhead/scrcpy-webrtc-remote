package adb

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"scrcpy-webrtc-remote/pkg/logger"
)

type Client struct {
	path string
}

func New(path string) *Client {
	if path == "" {
		path = "adb"
	}
	return &Client{path: path}
}

func (c *Client) args(serial string, extra ...string) []string {
	a := []string{"-s", serial}
	a = append(a, extra...)
	return a
}

func (c *Client) Forward(serial string, localPort int, remoteAbstract string) error {
	cmd := exec.Command(c.path, c.args(serial, "forward",
		fmt.Sprintf("tcp:%d", localPort),
		fmt.Sprintf("localabstract:%s", remoteAbstract))...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb forward failed: %w, output: %s", err, string(out))
	}
	return nil
}

func (c *Client) ForwardRemove(serial string, localPort int) error {
	cmd := exec.Command(c.path, c.args(serial, "forward", "--remove",
		fmt.Sprintf("tcp:%d", localPort))...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// ignore error if forward does not exist
		if strings.Contains(string(out), "does not exist") {
			return nil
		}
		return fmt.Errorf("adb forward --remove failed: %w, output: %s", err, string(out))
	}
	return nil
}

func (c *Client) Push(serial, local, remote string) error {
	cmd := exec.Command(c.path, c.args(serial, "push", local, remote)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb push failed: %w, output: %s", err, string(out))
	}
	return nil
}

func (c *Client) Shell(serial, command string) error {
	cmd := exec.Command(c.path, c.args(serial, "shell", command)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb shell failed: %w, output: %s", err, string(out))
	}
	_ = out
	return nil
}

// Devices returns the list of connected device serials from `adb devices`.
func (c *Client) Devices() ([]string, error) {
	cmd := exec.Command(c.path, "devices")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("adb devices failed: %w, output: %s", err, string(out))
	}
	var serials []string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "\tdevice") || strings.HasSuffix(line, " device") {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				serials = append(serials, parts[0])
			}
		}
	}
	return serials, nil
}

// Connect tries `adb connect <serial>` (for TCP/IP devices).
func (c *Client) Connect(serial string) error {
	cmd := exec.Command(c.path, "connect", serial)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb connect failed: %w, output: %s", err, string(out))
	}
	return nil
}

// ShellOutput runs a shell command on the device and returns its stdout.
func (c *Client) ShellOutput(serial, command string) (string, error) {
	cmd := exec.Command(c.path, c.args(serial, "shell", command)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// jarUpToDate reports whether the device-side jar already matches the local
// one (by size), so the push can be skipped on repeat sessions.
func (c *Client) jarUpToDate(serial, local, remote string) bool {
	localSize, err := os.Stat(local)
	if err != nil {
		return false
	}
	out, err := c.ShellOutput(serial, fmt.Sprintf("stat -c %%s %s 2>/dev/null", remote))
	if err != nil || out == "" {
		return false
	}
	remoteSize, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return false
	}
	return remoteSize == localSize.Size()
}

// killStaleServer kills any leftover scrcpy server and polls until it is
// gone (replacing the previous fixed 1.5s sleep — usually done in ~100ms,
// and skipped entirely when nothing was running).
func (c *Client) killStaleServer(serial string) {
	out, _ := c.ShellOutput(serial, "pgrep -f 'app_process.*scrcpy' 2>/dev/null")
	if out == "" {
		return // no stale server — nothing to kill or wait for
	}
	_ = c.Shell(serial, "pkill -f 'app_process.*scrcpy' 2>/dev/null || true")
	for i := 0; i < 15; i++ {
		time.Sleep(100 * time.Millisecond)
		out, _ := c.ShellOutput(serial, "pgrep -f 'app_process.*scrcpy' 2>/dev/null")
		if out == "" {
			return
		}
	}
}

func (c *Client) StartServer(serial string, jarPath, serverVersion string, maxSize, videoBitRate, audioBitRate int, videoCodec, audioCodec, audioEncoder string, powerOn, stayAwake, audioEnabled bool, videoKeyframeInterval, videoMaxFPS int) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	remoteJar := "/data/local/tmp/scrcpy-server.jar"
	if c.jarUpToDate(serial, jarPath, remoteJar) {
		logger.Info("scrcpy-server.jar unchanged on device, skipping push", "serial", serial)
	} else if err := c.Push(serial, jarPath, remoteJar); err != nil {
		return nil, nil, nil, fmt.Errorf("push scrcpy-server.jar: %w", err)
	}

	c.killStaleServer(serial)

	parts := []string{
		fmt.Sprintf("CLASSPATH=%s app_process /", remoteJar),
		fmt.Sprintf("com.genymobile.scrcpy.Server %s", serverVersion),
		"tunnel_forward=true",
		"cleanup=false",
		"audio=" + strconv.FormatBool(audioEnabled),
		"control=true",
		"send_frame_meta=true",
		"send_stream_meta=true",
		fmt.Sprintf("video_codec=%s", videoCodec),
		fmt.Sprintf("max_size=%d", maxSize),
		fmt.Sprintf("video_bit_rate=%d", videoBitRate),
	}

	if videoMaxFPS > 0 {
		// Cap the encoder frame rate. Without a cap, emulator/software
		// encoders follow surface change rate (MuMu实测 10-55fps 波动),
		// making frame arrival jitter the dominant term of the browser
		// jitter buffer depth (实测 100-700ms 堆积).
		parts = append(parts, fmt.Sprintf("max_fps=%d", videoMaxFPS))
	}

	if videoKeyframeInterval > 0 {
		parts = append(parts,
			fmt.Sprintf("video_codec_options=i-frame-interval=%d", videoKeyframeInterval),
		)
	}

	if audioEnabled {
		parts = append(parts,
			fmt.Sprintf("audio_codec=%s", audioCodec),
			fmt.Sprintf("audio_bit_rate=%d", audioBitRate),
		)
	}

	if audioEncoder != "" && audioEnabled {
		parts = append(parts, fmt.Sprintf("audio_encoder=%s", audioEncoder))
	}
	if powerOn {
		parts = append(parts, "power_on=true")
	}
	if stayAwake {
		parts = append(parts, "stay_awake=true")
	}

	shellCmd := strings.Join(parts, " ")
	cmd := exec.Command(c.path, "-s", serial, "shell", shellCmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start scrcpy server: %w", err)
	}

	// No fixed startup sleep here: connectStreams() retries with backoff
	// until the LocalServerSocket accepts connections, so any startup
	// latency is absorbed there with ~250ms granularity instead of a
	// blind 4-second wait.
	return cmd, stdout, stderr, nil
}
