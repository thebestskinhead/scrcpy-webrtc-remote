package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"scrcpy-webrtc-remote/agent/internal/adb"
	"scrcpy-webrtc-remote/agent/internal/portpool"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
	"scrcpy-webrtc-remote/pkg/types"
)

type State string

const (
	StateCreated       State = "Created"
	StatePortAllocated State = "PortAllocated"
	StateForwarded     State = "Forwarded"
	StateServerRunning State = "ServerRunning"
	StateConnected     State = "Connected"
	StateStreaming     State = "Streaming"
	StateError         State = "Error"
	StateReleased      State = "Released"
)

// maxVideoDuration caps the per-frame PTS delta used for RTP timestamp
// pacing. It must be generous enough to admit real long gaps (e.g. a
// static screen where the encoder only emits periodic IDRs every 1-3 s —
// clamping those to 33 ms makes RTP timestamps claim 30 fps while frames
// actually arrive seconds late, which the browser reads as massive jitter
// and deepens its jitter buffer to 500-700 ms, i.e. the operation-latency
// blowup). Only PTS rollover / wild jumps are filtered out.
// 取值 4.7s（非常用取值）。
const maxVideoDuration = 4700 * time.Millisecond

const maxPausedVideoFrames = 300

func mergeConfigNAL(configNAL, payload []byte) []byte {
	annexbPrefix := []byte{0x00, 0x00, 0x00, 0x01}
	endsWithStart := false
	if len(configNAL) >= 4 && configNAL[len(configNAL)-4] == 0 && configNAL[len(configNAL)-3] == 0 &&
		configNAL[len(configNAL)-2] == 0 && configNAL[len(configNAL)-1] == 1 {
		endsWithStart = true
	} else if len(configNAL) >= 3 && configNAL[len(configNAL)-3] == 0 && configNAL[len(configNAL)-2] == 0 &&
		configNAL[len(configNAL)-1] == 1 {
		endsWithStart = true
	}
	combinedLen := len(configNAL) + len(payload)
	if !endsWithStart {
		combinedLen += len(annexbPrefix)
	}
	combined := make([]byte, 0, combinedLen)
	combined = append(combined, configNAL...)
	if !endsWithStart {
		combined = append(combined, annexbPrefix...)
	}
	combined = append(combined, payload...)
	return combined
}

// Sample is an alias for the shared type in pkg/types.
type Sample = types.Sample

type Info struct {
	Serial         string `json:"serial"`
	State          string `json:"state"`
	ADBPort        int    `json:"adb_forward_port"`
	ProfileLevelID string `json:"profile_level_id"`
	VideoCodec     string `json:"video_codec"`
}

type Session struct {
	serial string
	adb    *adb.Client
	pool   *portpool.Pool
	cfg    config.ScrcpyConfig

	state atomic.Value
	adbPort int
	server  *exec.Cmd

	videoConn net.Conn
	audioConn net.Conn
	ctrlConn  net.Conn

	stopCh            chan struct{}
	wg                sync.WaitGroup
	forwardersStarted atomic.Bool

	// output channels for zero-copy forwarding
	VideoCh chan Sample
	AudioCh chan Sample
	CtrlCh  chan DeviceMsg

	// lastConfigNAL holds the most recent H.264 SPS+PPS for injection on every IDR
	lastConfigNAL []byte

	// profile-level-id extracted from first video config packet
	profileLevelID   string
	profileReady     chan struct{}
	profileReadyOnce sync.Once

	// video codec type
	videoCodec string

	// internal control buffer
	ctrlMu  sync.Mutex
	ctrlBuf []byte

	// forwardPaused controls whether video/audio data should be discarded
	// before media pumps are ready. Config packets are still processed.
	forwardPaused atomic.Bool

	// pausedVideoBuf caches video frames during paused=true so that after
	// unpause we can flush starting from the first IDR, avoiding decoder
	// black-screen caused by starting with a P-frame.
	pausedMu       sync.Mutex
	pausedVideoBuf []Sample

	// Per-stream PTS normalizers: each stream normalizes against its own
	// first non-config frame (avoids cross-stream timebase skew — without
	// this, if audio arrives 5s after video, the shared baseline from video
	// makes audio appear 5s ahead, causing WebRTC lip sync to discard all
	// audio frames). They also survive encoder-reset PTS rollbacks.
	audioNorm ptsNormalizer
	videoNorm ptsNormalizer

	// diedCh is closed when all forward goroutines (video/audio/control) have exited,
	// either naturally (encoder crash) or via Release().
	diedCh chan struct{}

	// drainWg tracks channel drain goroutines started in Release(). Release()
	// waits on this group before returning so callers can safely assume all
	// channel references have been drained.
	drainWg sync.WaitGroup
}

func NewSession(serial string, adbClient *adb.Client, pool *portpool.Pool, cfg config.ScrcpyConfig) *Session {
	s := &Session{
		serial:       serial,
		adb:          adbClient,
		pool:         pool,
		cfg:          cfg,
		stopCh:       make(chan struct{}),
		VideoCh:      make(chan Sample, 180),
		AudioCh:      make(chan Sample, 300),
		CtrlCh:       make(chan DeviceMsg, 100),
		profileReady: make(chan struct{}),
		videoCodec:   cfg.VideoCodec,
		diedCh:       make(chan struct{}),
	}
	s.state.Store(StateCreated)
	return s
}

func (s *Session) State() State {
	return s.state.Load().(State)
}

func (s *Session) setState(st State) {
	s.state.Store(st)
	logger.Info("session state changed", "serial", s.serial, "state", st)
}

// Died returns a channel closed when all forward goroutines have exited
// (video, audio, control), indicating that the scrcpy server has terminated
// and the session must be restarted.
func (s *Session) Died() <-chan struct{} {
	return s.diedCh
}

// Alive reports whether the session can be reused: not in a terminal state
// and the scrcpy server has not died (forward goroutines still running).
func (s *Session) Alive() bool {
	st := s.State()
	if st == StateError || st == StateReleased {
		return false
	}
	select {
	case <-s.diedCh:
		return false
	default:
		return true
	}
}

func (s *Session) Start() error {
	if err := s.allocatePort(); err != nil {
		s.setState(StateError)
		return err
	}
	if err := s.setupForward(); err != nil {
		s.setState(StateError)
		s.releasePort()
		return err
	}
	if err := s.startServer(); err != nil {
		s.setState(StateError)
		s.cleanupForward()
		s.releasePort()
		return err
	}
	if err := s.connectStreams(); err != nil {
		s.setState(StateError)
		s.cleanupServer()
		s.cleanupForward()
		s.releasePort()
		return err
	}

	s.setState(StateStreaming)

	return nil
}

// SetForwardPaused 控制 forwardVideo/forwardAudio 是否丢弃数据（true=丢弃）。
// 用于在 media pump 启动前避免 channel 堆积导致 socket 读取阻塞。
func (s *Session) SetForwardPaused(paused bool) {
	s.forwardPaused.Store(paused)
	logger.Info("session forward paused state changed", "serial", s.serial, "paused", paused)
}

// StartForwarders 启动 forwardVideo、forwardAudio 和 controlReader。
// 调用方允许重复调用（通过 forwardersStarted 保护）。
func (s *Session) StartForwarders() {
	if s.forwardersStarted.Swap(true) {
		return
	}
	n := 2 // video + control
	if s.cfg.AudioEnabled {
		n = 3 // video + audio + control
	}
	s.wg.Add(n)
	go func() { defer s.wg.Done(); s.forwardVideo() }()
	if s.cfg.AudioEnabled {
		go func() { defer s.wg.Done(); s.forwardAudio() }()
	}
	go func() { defer s.wg.Done(); s.controlReader() }()
	go func() {
		s.wg.Wait()
		close(s.diedCh)
	}()
}

func (s *Session) allocatePort() error {
	port, err := s.pool.Acquire()
	if err != nil {
		return fmt.Errorf("allocate port: %w", err)
	}
	s.adbPort = port
	s.setState(StatePortAllocated)
	return nil
}

func (s *Session) releasePort() {
	if s.adbPort != 0 {
		s.pool.Release(s.adbPort)
		s.adbPort = 0
	}
}

func (s *Session) setupForward() error {
	_ = s.adb.ForwardRemove(s.serial, s.adbPort)
	time.Sleep(200 * time.Millisecond)

	if err := s.adb.Forward(s.serial, s.adbPort, "scrcpy"); err != nil {
		return fmt.Errorf("adb forward: %w", err)
	}
	s.setState(StateForwarded)
	return nil
}

func (s *Session) cleanupForward() {
	if s.adbPort != 0 {
		for i := 0; i < 3; i++ {
			if err := s.adb.ForwardRemove(s.serial, s.adbPort); err == nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func (s *Session) startServer() error {
	cmd, stdout, stderr, err := s.adb.StartServer(
		s.serial,
		s.cfg.JarPath,
		s.cfg.ServerVersion,
		s.cfg.MaxSize,
		s.cfg.VideoBitRate,
		s.cfg.AudioBitRate,
		s.cfg.VideoCodec,
		s.cfg.AudioCodec,
		s.cfg.AudioEncoder,
		s.cfg.PowerOn,
		s.cfg.StayAwake,
		s.cfg.AudioEnabled,
		s.cfg.VideoKeyframeInterval,
		s.cfg.VideoMaxFPS,
	)
	if err != nil {
		return err
	}
	s.server = cmd
	s.setState(StateServerRunning)

	// Wait until the server has actually created its abstract socket before
	// anyone dials. adb forward accepts local TCP connections even when the
	// device-side abstract socket does not exist yet — the connection then
	// EOFs immediately on read, which the dial-retry loop cannot detect.
	if !s.waitForServerSocket(10 * time.Second) {
		logger.Warn("scrcpy server socket wait timeout, connecting anyway",
			"serial", s.serial)
	}

	// Forward scrcpy server stdout/stderr to bridge logs
	go s.logServerOutput(stdout, "stdout")
	go s.logServerOutput(stderr, "stderr")
	return nil
}

// waitForServerSocket polls /proc/net/unix until the scrcpy abstract socket
// appears (server bound) or the timeout elapses.
func (s *Session) waitForServerSocket(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := s.adb.ShellOutput(s.serial,
			"grep -c '@scrcpy' /proc/net/unix 2>/dev/null")
		if err == nil && out != "" && out != "0" {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func (s *Session) logServerOutput(r io.ReadCloser, name string) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	first := true
	for scanner.Scan() {
		if first {
			logger.Info("scrcpy server "+name+" first line", "serial", s.serial, "msg", scanner.Text())
			first = false
		} else {
			logger.Info("scrcpy server "+name, "serial", s.serial, "msg", scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("scrcpy server "+name+" scanner error", "serial", s.serial, "err", err)
	}
	logger.Info("scrcpy server "+name+" stream ended", "serial", s.serial)
}

func (s *Session) cleanupServer() {
	if s.server != nil && s.server.Process != nil {
		s.server.Process.Signal(os.Interrupt)
		_ = s.server.Wait()
	}
}

func (s *Session) connectStreams() error {
	host := "127.0.0.1"
	var lastErr error

	// Short, frequent retries: the scrcpy server typically opens its
	// LocalServerSocket within ~1s of spawn, and there is no fixed startup
	// sleep upstream anymore — poll briskly so startup latency, not the
	// retry interval, decides how fast we connect.
	for attempt := 0; attempt < 40; attempt++ {
		if attempt > 0 {
			time.Sleep(250 * time.Millisecond)
		}

		// Video
		vc, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, s.adbPort), 5*time.Second)
		if err != nil {
			lastErr = err
			logger.Warn("video connect failed", "serial", s.serial, "attempt", attempt+1, "err", err)
			continue
		}
		s.videoConn = vc

		// Audio (skip if disabled)
		var ac net.Conn
		if s.cfg.AudioEnabled {
			var err error
			ac, err = net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, s.adbPort), 5*time.Second)
			if err != nil {
				lastErr = err
				logger.Warn("audio connect failed", "serial", s.serial, "attempt", attempt+1, "err", err)
				_ = vc.Close()
				s.videoConn = nil
				continue
			}
			s.audioConn = ac
		} else {
			logger.Info("audio disabled, skipping audio connection", "serial", s.serial)
			s.audioConn = nil
		}

		// Control
		cc, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, s.adbPort), 5*time.Second)
		if err != nil {
			lastErr = err
			logger.Warn("control connect failed", "serial", s.serial, "attempt", attempt+1, "err", err)
			_ = vc.Close()
			if ac != nil {
				_ = ac.Close()
			} else if s.audioConn != nil {
				_ = s.audioConn.Close()
			}
			s.videoConn = nil
			s.audioConn = nil
			continue
		}
		s.ctrlConn = cc

		s.setState(StateConnected)
		return nil
	}

	return fmt.Errorf("connect streams after retries: %w", lastErr)
}

func (s *Session) SendControl(data []byte) error {
	s.ctrlMu.Lock()
	defer s.ctrlMu.Unlock()
	if s.ctrlConn == nil {
		return fmt.Errorf("control socket not connected")
	}
	_, err := s.ctrlConn.Write(data)
	return err
}

func (s *Session) WaitProfileLevelID(timeout time.Duration) string {
	select {
	case <-s.profileReady:
		return s.profileLevelID
	case <-time.After(timeout):
		logger.Error("profile-level-id probe timeout, fallback to 42e01f", "serial", s.serial)
		return "42e01f"
	}
}

func (s *Session) setProfileLevelID(plid string) {
	s.profileLevelID = plid
	s.profileReadyOnce.Do(func() {
		close(s.profileReady)
	})
}

// ptsNormalizer converts device PTS values into a monotonic zero-based
// stream. It survives encoder restarts: when the device PTS rolls back
// (scrcpy "Video capture reset" starts MediaCodec PTS from ~0 again while
// the old baseline is kept), the baseline is rebased so output never goes
// backwards — otherwise the browser discards every subsequent frame as
// "too old" and the stream never recovers (observed: any PLI L2 encoder
// reset permanently zeroed browser decode while packets flowed fine).
type ptsNormalizer struct {
	mu       sync.Mutex
	started  bool
	base     int64 // ns subtracted from raw
	lastNorm int64 // ns
	lastRaw  int64 // ns
}

func (n *ptsNormalizer) normalize(raw time.Duration) time.Duration {
	r := raw.Nanoseconds()
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.started {
		n.started = true
		n.base = r
		n.lastRaw = r
		n.lastNorm = 0
		return 0
	}
	if r < n.lastRaw {
		// Rollback (encoder reset): continue right after the last value.
		n.base = r - n.lastNorm - int64(16*time.Millisecond)
		logger.Warn("PTS rollback detected (encoder reset?), rebasing",
			"raw_ms", r/1e6, "last_norm_ms", n.lastNorm/1e6)
	}
	n.lastRaw = r
	norm := r - n.base
	if norm < n.lastNorm {
		norm = n.lastNorm
	}
	n.lastNorm = norm
	return time.Duration(norm)
}

// normalizeAudioPTS subtracts the audio stream's own first-frame PTS baseline
// and survives encoder restarts (see ptsNormalizer).
func (s *Session) normalizeAudioPTS(pts time.Duration) time.Duration {
	return s.audioNorm.normalize(pts)
}

// normalizeVideoPTS subtracts the video stream's own first-frame PTS baseline
// and survives encoder restarts (see ptsNormalizer).
func (s *Session) normalizeVideoPTS(pts time.Duration) time.Duration {
	return s.videoNorm.normalize(pts)
}

func (s *Session) bufferPausedVideo(sample Sample) {
	s.pausedMu.Lock()
	defer s.pausedMu.Unlock()
	if sample.KeyFrame {
		s.pausedVideoBuf = nil
	}
	s.pausedVideoBuf = append(s.pausedVideoBuf, sample)
	if len(s.pausedVideoBuf) > maxPausedVideoFrames {
		// Trim oldest frames while keeping at least one keyframe if possible.
		// Simple strategy: drop the first half.
		cut := len(s.pausedVideoBuf) - maxPausedVideoFrames/2
		s.pausedVideoBuf = s.pausedVideoBuf[cut:]
	}
}

func (s *Session) flushPausedVideo(configNAL []byte) []Sample {
	s.pausedMu.Lock()
	defer s.pausedMu.Unlock()
	if len(s.pausedVideoBuf) == 0 {
		return nil
	}
	logger.Info("flushPausedVideo: flushing buffered frames",
		"serial", s.serial,
		"buffered_count", len(s.pausedVideoBuf),
		"has_configNAL", len(configNAL) > 0)
	// Find first IDR in buffer
	start := -1
	for i, sample := range s.pausedVideoBuf {
		if sample.KeyFrame {
			start = i
			break
		}
	}
	if start == -1 {
		logger.Warn("flushPausedVideo: no IDR in buffer, discarding all", "serial", s.serial, "count", len(s.pausedVideoBuf))
		s.pausedVideoBuf = nil
		return nil
	}
	// 只保留第一个 IDR（合并 SPS/PPS）作为解码起点，丢弃 IDR 之后的所有
	// 旧帧：缓冲帧是 paused 期间（数秒前）的旧画面，全部发送会让浏览器
	// 先播"过去"的画面（启动画面滞后 + 无谓突发）；NACK 恢复依赖拦截器
	// 对已发包的缓冲，与这些旧帧无关。浏览器在 IDR 后立即收到现场帧。
	dropped := len(s.pausedVideoBuf) - start - 1
	if start > 0 {
		logger.Info("flushPausedVideo: dropping P-frames before first IDR",
			"serial", s.serial, "dropped", start, "kept", len(s.pausedVideoBuf)-start)
	}
	sample := s.pausedVideoBuf[start]
	if len(configNAL) > 0 {
		sample.Data = mergeConfigNAL(configNAL, sample.Data)
	}
	s.pausedVideoBuf = nil
	logger.Info("flushPausedVideo: kept only first IDR, discarded stale frames",
		"serial", s.serial, "dropped", dropped)
	return []Sample{sample}
}

func (s *Session) forwardVideo() {
	defer close(s.VideoCh)
	logger.Info("forwardVideo goroutine started", "serial", s.serial)

	reader := NewReader(s.videoConn)

	// scrcpy 4.0: read only codec ID (4 bytes), width/height come via SessionMeta
	codecID, err := reader.ReadVideoCodecID()
	if err != nil {
		logger.Error("video handshake failed", "serial", s.serial, "err", err)
		return
	}
	logger.Info("video codec id read", "serial", s.serial, "codec_id", fmt.Sprintf("%08x", codecID))

	isH264 := s.videoCodec == "h264"

	var configNAL []byte
	var lastRawPTS time.Duration
	first := true
	profileProbed := false
	packetCount := 0

	// VP8: no SPS config packets, so signal ready immediately
	if !isH264 && !profileProbed {
		profileProbed = true
		logger.Info("non-H264 codec detected, skipping profile-level-id probe", "serial", s.serial)
		s.setProfileLevelID(s.videoCodec)
	}

	for {
		select {
		case <-s.stopCh:
			logger.Info("forwardVideo stopped by stopCh", "serial", s.serial)
			return
		default:
		}

		packetCount++
		if packetCount <= 5 || packetCount%600 == 0 {
			logger.Debug("forwardVideo waiting for packet", "serial", s.serial, "seq", packetCount)
		}

		header, payload, sessionMeta, err := reader.ReadPacket()
		if err != nil {
			logger.Error("video reader stopped", "serial", s.serial, "err", err, "seq", packetCount)
			return
		}

		// scrcpy 4.0: handle session meta packet (contains width/height)
		if sessionMeta != nil {
			logger.Info("received session meta",
				"serial", s.serial,
				"width", sessionMeta.Width,
				"height", sessionMeta.Height,
				"is_client_resize", sessionMeta.IsClientResize)
			continue
		}

		if packetCount <= 5 || packetCount%600 == 0 {
			logger.Debug("forwardVideo packet read", "serial", s.serial, "seq", packetCount, "is_config", header.IsConfig(), "packet_size", len(payload))
		}

		// H.264 config packet handling (SPS/PPS)
		if header.IsConfig() && isH264 {
			configNAL = append([]byte(nil), payload...)
			s.lastConfigNAL = configNAL

			if !profileProbed {
				profileProbed = true
				plid := tryExtractProfileLevelID(configNAL)
				if plid != "" {
					normalized := NormalizeProfileLevelID(plid)
					s.setProfileLevelID(normalized)
					logger.Info("profile-level-id extracted from config",
						"serial", s.serial, "raw", plid, "normalized", normalized)
				} else {
					logger.Error("no SPS found in config packet, fallback to 42e01f", "serial", s.serial)
					s.setProfileLevelID("42e01f")
				}
			}
			continue
		}

		// First non-config packet: if profile not probed, try to find SPS in payload (H.264 only)
		if isH264 && !profileProbed {
			profileProbed = true
			plid := tryExtractProfileLevelID(payload)
			if plid != "" {
				normalized := NormalizeProfileLevelID(plid)
				s.setProfileLevelID(normalized)
				logger.Info("profile-level-id extracted from payload",
					"serial", s.serial, "raw", plid, "normalized", normalized)
			} else {
				logger.Error("no SPS found in first packet, fallback to 42e01f", "serial", s.serial)
				s.setProfileLevelID("42e01f")
			}
		}

		rawPTS := header.PTS()
		duration := 33 * time.Millisecond
		if !first {
			if diff := rawPTS - lastRawPTS; diff > 0 && diff < maxVideoDuration {
				duration = diff
			}
		}
		lastRawPTS = rawPTS
		first = false
		pts := s.normalizeVideoPTS(rawPTS)

		keyFrame := header.IsKeyFrame()

		// Prepare payload (AnnexB) before paused check so that buffered
		// frames are already in the correct format for WebRTC.
		// Only H.264 needs AnnexB start code normalization.
		if isH264 {
			annexbPrefix := []byte{0x00, 0x00, 0x00, 0x01}
			if len(payload) >= 4 && payload[0] == 0 && payload[1] == 0 && payload[2] == 0 && payload[3] == 1 {
				// already AnnexB with 4-byte start code
			} else if len(payload) >= 3 && payload[0] == 0 && payload[1] == 0 && payload[2] == 1 {
				// already AnnexB with 3-byte start code
			} else {
				payload = append(annexbPrefix, payload...)
			}
		}

		isPaused := s.forwardPaused.Load()

		// When transitioning from paused to unpaused, flush buffered frames
		if !isPaused {
			toSend := s.flushPausedVideo(configNAL)
			if len(toSend) > 0 {
				configNAL = nil
				for _, sample := range toSend {
					select {
					case s.VideoCh <- sample:
					case <-s.stopCh:
						return
					}
				}
			}
		}

		if isPaused {
			if packetCount <= 5 || packetCount%600 == 0 {
				logger.Debug("forwardVideo buffering frame (paused)", "serial", s.serial, "seq", packetCount, "packet_size", len(payload), "keyframe", keyFrame)
			}
			s.bufferPausedVideo(Sample{Data: payload, PTS: pts, Duration: duration, KeyFrame: keyFrame})
			continue
		}

		// Inject SPS/PPS on every IDR (keyframe) so the browser decoder always
		// has fresh sequence parameters. scrcpy encoder only sends SPS/PPS once
		// at stream start, but the browser needs them re-confirmed after any
		// encoder reset or stream disruption.
		effConfig := configNAL
		if len(effConfig) == 0 && keyFrame && len(s.lastConfigNAL) > 0 {
			effConfig = s.lastConfigNAL
		}
		if len(effConfig) > 0 {
			payload = mergeConfigNAL(effConfig, payload)
			configNAL = nil
		}

		select {
		case s.VideoCh <- Sample{Data: payload, PTS: pts, Duration: duration, KeyFrame: keyFrame}:
		case <-s.stopCh:
			return
		}
	}
}

func (s *Session) forwardAudio() {
	defer close(s.AudioCh)
	logger.Info("forwardAudio goroutine started", "serial", s.serial)

	reader := NewReader(s.audioConn)
	codecID, err := reader.ReadAudioCodecID()
	if err != nil {
		logger.Error("audio handshake failed", "serial", s.serial, "err", err)
		// scrcpy 4.0: if codecID is 0 (stream disabled), non-fatal — mirror video only
		if codecID == 0 {
			logger.Warn("audio stream disabled by server, mirroring video only", "serial", s.serial)
			return
		}
		return
	}
	logger.Info("audio codec id read", "serial", s.serial, "codec_id", fmt.Sprintf("%08x", codecID))

	packetCount := 0

	for {
		select {
		case <-s.stopCh:
			logger.Info("forwardAudio stopped by stopCh", "serial", s.serial)
			return
		default:
		}

		packetCount++
		if packetCount <= 5 || packetCount%600 == 0 {
			logger.Debug("forwardAudio waiting for packet", "serial", s.serial, "seq", packetCount)
		}

		header, payload, _, err := reader.ReadPacket()
		if err != nil {
			logger.Error("audio reader stopped", "serial", s.serial, "err", err, "seq", packetCount)
			return
		}

		// Opus config packet: discard
		if header.IsConfig() {
			continue
		}

		rawPTS := header.PTS()
		// scrcpy Opus encoder always outputs fixed 20ms frames (960 samples @ 48kHz).
		// Using PTS diffs as duration causes RTP timestamp/payload mismatch when
		// Android-side PTS gaps are 40–60ms (CPU jitter, USB batching, DTX resume),
		// which makes NetEQ see phantom gaps and inflate jitter buffer to 1000ms+.
		const duration = 20 * time.Millisecond

		if s.forwardPaused.Load() {
			if packetCount <= 5 || packetCount%600 == 0 {
				logger.Debug("forwardAudio dropping frame (paused)", "serial", s.serial, "seq", packetCount, "packet_size", len(payload))
			}
			continue
		}

		// Set PTS baseline from the first frame that actually gets forwarded
		// (after unpause), not from early silent/DTX frames during pause.
		pts := s.normalizeAudioPTS(rawPTS)

		select {
		case s.AudioCh <- Sample{Data: payload, PTS: pts, Duration: duration}:
		case <-s.stopCh:
			return
		}
	}
}

func (s *Session) controlReader() {
	defer close(s.CtrlCh)

	buf := make([]byte, 8192)
	var leftover []byte

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		if s.ctrlConn == nil {
			return
		}

		s.ctrlConn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := s.ctrlConn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			logger.Info("control reader stopped", "serial", s.serial, "err", err)
			return
		}
		if n == 0 {
			return
		}

		// Accumulate partial data from previous read
		data := append(leftover, buf[:n]...)
		leftover = nil

		msgs, remainder, err := ParseDeviceMessages(data)
		if err != nil {
			logger.Warn("control parse error", "serial", s.serial, "err", err)
			continue
		}

		for _, msg := range msgs {
			select {
			case s.CtrlCh <- msg:
			case <-s.stopCh:
				return
			}
		}

		if len(remainder) > 0 {
			leftover = remainder
		}
	}
}

func (s *Session) Info() Info {
	return Info{
		Serial:         s.serial,
		State:          string(s.State()),
		ADBPort:        s.adbPort,
		ProfileLevelID: s.profileLevelID,
		VideoCodec:     s.videoCodec,
	}
}

func (s *Session) GetVideoCodec() string {
	return s.videoCodec
}

func (s *Session) GetAudioCodec() string {
	return s.cfg.AudioCodec
}

func (s *Session) Release() {
	if s.State() == StateReleased {
		return
	}
	s.setState(StateReleased)
	close(s.stopCh)

	// Close connections to unblock readers
	if s.videoConn != nil {
		_ = s.videoConn.Close()
	}
	if s.audioConn != nil {
		_ = s.audioConn.Close()
	}
	if s.ctrlConn != nil {
		_ = s.ctrlConn.Close()
	}

	s.wg.Wait()

	// Drain channels to unblock writers. Use drainWg so callers can wait
	// for drain completion before creating a new session.
	s.drainWg.Add(3)
	go func() { defer s.drainWg.Done(); for range s.VideoCh {} }()
	go func() { defer s.drainWg.Done(); for range s.AudioCh {} }()
	go func() { defer s.drainWg.Done(); for range s.CtrlCh  {} }()
	s.drainWg.Wait()

	s.cleanupServer()
	s.cleanupForward()
	s.releasePort()

	logger.Info("session fully released", "serial", s.serial)
}
