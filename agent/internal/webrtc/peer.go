package bridgewebrtc

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"scrcpy-webrtc-remote/agent/internal/gateway"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
)

var profileLevelIDRe = regexp.MustCompile(`(?i)(profile-level-id=)([0-9a-f]{6})`)

// h264MaxFSFromProfileLevelID returns the H.264 MaxFS (maximum macroblocks per
// frame) for the level encoded in a profile-level-id hex string, e.g.
// "42e01f" → level_idc=0x1f=31 → Level 3.1 → MaxFS=3600.
// Adding max-fs to the SDP fmtp line lets the browser's hardware decoder
// pre-allocate the right buffer size; missing or wrong values are a known
// cause of decoder freeze on large IDR frames.
func h264MaxFSFromProfileLevelID(plid string) int {
	if len(plid) < 6 {
		return 8192
	}
	levelIdc, err := strconv.ParseInt(plid[4:6], 16, 64)
	if err != nil {
		return 8192
	}
	switch levelIdc {
	case 10:
		return 99
	case 11, 12, 13, 20:
		return 396
	case 21:
		return 792
	case 22, 30:
		return 1620
	case 31:
		return 3600
	case 32:
		return 5120
	case 40, 41:
		return 8192
	case 42:
		return 8704
	case 50:
		return 22080
	case 51, 52:
		return 36864
	default:
		return 8192
	}
}

const playoutDelayURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"

// playoutDelayExtInterceptor injects a playout-delay header extension
// (min=0, max=50ms) onto every outbound video RTP packet. This tells the
// browser to render with at most 50ms of jitter buffer — enough to absorb
// IDR frame bursts while keeping latency near zero on LAN scrcpy.
type playoutDelayExtInterceptor struct {
	interceptor.NoOp
	extID   uint8
	enabled atomic.Bool
}

func (p *playoutDelayExtInterceptor) setID(id uint8) {
	p.extID = id
	p.enabled.Store(true)
}

func (p *playoutDelayExtInterceptor) BindLocalStream(
	info *interceptor.StreamInfo,
	writer interceptor.RTPWriter,
) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		if p.enabled.Load() && !header.Extension {
			// min_delay=0, max_delay=50ms (5 * 10ms units)
			//
			// NOTE: NACK retransmissions replay cached rtp headers through
			// the full interceptor chain. These headers may have nil extension
			// internals (allocated lazily by pion), causing SetExtension to
			// panic with nil pointer dereference. Guard with recover.
			func() {
				defer func() { _ = recover() }()
				_ = header.SetExtension(p.extID, []byte{0, 0, 5})
			}()
		}
		return writer.Write(header, payload, attributes)
	})
}

func (p *playoutDelayExtInterceptor) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return p, nil
}

// ---- RED (RFC 2198) sender interceptor --------------------------------
// Wraps each outbound H264 video packet with one redundant copy of the
// previous packet. Can be toggled at runtime without renegotiation.
type redSenderInterceptor struct {
	interceptor.NoOp
	enabled   atomic.Bool
	ready     atomic.Bool // payload types configured from the negotiated answer
	mu        sync.Mutex
	prevPay   []byte
	prevTS    uint32
	primaryPT uint8 // negotiated H264 PT (NOT hardcoded 102 — browsers remap)
	redPT     uint8 // negotiated RED PT (NOT hardcoded 116 — Chrome puts RTX there)
}

func newREDSenderInterceptor(primaryPT, redPT uint8) *redSenderInterceptor {
	return &redSenderInterceptor{primaryPT: primaryPT, redPT: redPT}
}

func (r *redSenderInterceptor) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return r, nil
}

func (r *redSenderInterceptor) SetEnabled(v bool) {
	r.enabled.Store(v)
	if !v {
		r.mu.Lock()
		r.prevPay = nil
		r.mu.Unlock()
	}
}

// SetPayloadTypes configures the negotiated payload types from the answer
// SDP. Must be called after SetLocalDescription. If either PT is missing
// (RED not negotiated), RED is disabled entirely.
func (r *redSenderInterceptor) SetPayloadTypes(primaryPT, redPT uint8) {
	r.mu.Lock()
	r.primaryPT = primaryPT
	r.redPT = redPT
	r.mu.Unlock()
	r.ready.Store(primaryPT != 0 && redPT != 0)
}

func (r *redSenderInterceptor) BindLocalStream(
	info *interceptor.StreamInfo,
	writer interceptor.RTPWriter,
) interceptor.RTPWriter {
	// Only intercept H264 video; pass audio through unchanged.
	if !strings.Contains(strings.ToLower(info.MimeType), "h264") {
		return writer
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attr interceptor.Attributes) (int, error) {
		if !r.enabled.Load() || !r.ready.Load() {
			return writer.Write(header, payload, attr)
		}
		r.mu.Lock()
		prevPay := r.prevPay
		prevTS := r.prevTS
		primaryPT := r.primaryPT
		redPT := r.redPT
		// Only packets that can themselves fit in a redundant block (10-bit
		// block length, ≤1023B) are worth remembering.
		if len(payload) <= 0x3FF {
			newPrev := make([]byte, len(payload))
			copy(newPrev, payload)
			r.prevPay = newPrev
			r.prevTS = header.Timestamp
		} else {
			r.prevPay = nil
		}
		r.mu.Unlock()
		// Size gate: the RED packet (5B headers + redundant + primary) must
		// stay within one MTU — the NACK responder's buffer also rejects
		// payloads >1460B with io.ErrShortBuffer, which used to kill the
		// video pump on the first big IDR after FEC enabled.
		redLen := 1 + len(payload)
		if prevPay != nil {
			redLen = 5 + len(prevPay) + len(payload)
		}
		if redLen > 1400 {
			return writer.Write(header, payload, attr)
		}
		redPayload := buildREDPayload(primaryPT, prevPay, prevTS, header.Timestamp, payload)
		redHdr := *header
		redHdr.PayloadType = redPT
		return writer.Write(&redHdr, redPayload, attr)
	})
}

// buildREDPayload encodes the current RTP payload in RFC 2198 RED format with
// one level of redundancy (previous packet). Falls back to single-block RED
// when no previous packet is available (first packet of stream).
func buildREDPayload(primaryPT uint8, prevPay []byte, prevTS, currTS uint32, primaryPay []byte) []byte {
	if len(prevPay) == 0 {
		// First packet: no redundant data, single-block RED.
		buf := make([]byte, 1+len(primaryPay))
		buf[0] = primaryPT & 0x7F // F=0, last block
		copy(buf[1:], primaryPay)
		return buf
	}
	tsOffset := currTS - prevTS
	if tsOffset > 0x3FFF {
		tsOffset = 0x3FFF
	}
	prevLen := uint16(len(prevPay))
	if prevLen > 0x3FF {
		prevLen = 0x3FF
	}
	// Redundant block header (4 bytes): F=1 | PT(7) | tsOffset(14) | blockLen(10)
	b0 := byte(0x80 | (primaryPT & 0x7F))
	b1 := byte((tsOffset >> 6) & 0xFF)
	b2 := byte((tsOffset&0x3F)<<2) | byte((prevLen>>8)&0x03)
	b3 := byte(prevLen & 0xFF)
	// Primary block header (1 byte): F=0 | PT(7)
	b4 := primaryPT & 0x7F
	buf := make([]byte, 0, 5+int(prevLen)+len(primaryPay))
	buf = append(buf, b0, b1, b2, b3, b4)
	buf = append(buf, prevPay[:prevLen]...)
	buf = append(buf, primaryPay...)
	return buf
}

type PeerManager struct {
	pc          *webrtc.PeerConnection
	videoTrack  *webrtc.TrackLocalStaticSample
	audioTrack  *webrtc.TrackLocalStaticSample
	videoSender *webrtc.RTPSender
	dataCh      *webrtc.DataChannel

	serial         string
	videoCodec     string
	audioCodec     string
	audioClockRate uint32
	profileLevelID string
	onCtrlRecv        func([]byte)
	onIceCand         func(string, *string, *uint16)
	onConnected       func()
	onRequestKeyframe func() // PLI L1/L2 → lightweight IDR request (0x17)
	onResetVideo      func() // PLI L3 / FIR → full encoder reset (0x11)
	ctrlSendCh     chan []byte

	// PLI two-level recovery state machine
	pliCount             int
	pliState             int        // 0=normal, 1=request(L1), 2=reset(L2)
	lastDecoded          uint32
	decodedAtStateChange uint32     // baseline when leaving normal state
	lastDecodedTime      time.Time
	lastPLITime     time.Time
	lastResetTime   time.Time
	pliBaseInterval time.Duration // base throttle, increases per level

	stopCh         chan struct{}
	wg             sync.WaitGroup
	playoutDelayer *playoutDelayExtInterceptor
	redInterceptor *redSenderInterceptor

	// QoS telemetry (fed by controller.go from browser decoder_status)
	lossRate      atomic.Value // float64, rolling loss percentage
	rttMs         atomic.Value // float64, browser-reported RTT in milliseconds
	fecAutoDisabled atomic.Bool
	rtcpRTTMs     atomic.Value // float64, RTCP ReceiverReport DLSR-based RTT

	// FEC hysteresis
	fecEnabledFlag  atomic.Bool
	lastFECToggle   time.Time
	currentBitrate  atomic.Int32 // current video bitrate (bps), used to gate FEC
}

const (
	pliStateNormal  = 0
	pliStateRequest = 1 // L1: lightweight IDR request (1s throttle)
	pliStateReset   = 2 // L2: full encoder reset (10s throttle, 15s min)
)

// NACK 重传缓冲包数：≈1s @ 30fps。
// 注意：pion nack responder 强制要求 2 的幂（允许 1..32768），
// 因此该值不能调校为非常用数值，保持 1024。
const nackBufferPackets = 1024

func NewPeerManager(serial string, stunServers []string, turnServer *config.TurnServerConfig, videoCodec string, profileLevelID string, audioCodec string, audioEnabled bool) (*PeerManager, error) {
	if profileLevelID == "" {
		profileLevelID = "42e01f"
	}
	if videoCodec == "" {
		videoCodec = "h264"
	}
	if audioCodec == "" {
		audioCodec = "opus"
	}

	iceServers := []webrtc.ICEServer{}
	for _, url := range stunServers {
		iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{url}})
	}
	if turnServer != nil {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       turnServer.URLs,
			Username:   turnServer.Username,
			Credential: turnServer.Credential,
		})
	}

	config := webrtc.Configuration{ICEServers: iceServers}

	m := &webrtc.MediaEngine{}
	if audioEnabled {
		if audioCodec == "pcmu" || audioCodec == "pcma" {
			mime := webrtc.MimeTypePCMU
			pt := webrtc.PayloadType(0)
			if audioCodec == "pcma" {
				mime = webrtc.MimeTypePCMA
				pt = webrtc.PayloadType(8)
			}
			if err := m.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 8000, Channels: 1},
				PayloadType:        pt,
			}, webrtc.RTPCodecTypeAudio); err != nil {
				return nil, fmt.Errorf("register %s: %w", audioCodec, err)
			}
		} else {
			if err := m.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;stereo=1;useinbandfec=1"},
				PayloadType:        111,
			}, webrtc.RTPCodecTypeAudio); err != nil {
				return nil, fmt.Errorf("register opus: %w", err)
			}
		}
	}

	// Register only the selected video codec
	if videoCodec == "vp8" {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: videoRTCPFeedback()},
			PayloadType:        96,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("register vp8: %w", err)
		}
	}
	if videoCodec == "h264" {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
				SDPFmtpLine:  fmt.Sprintf("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=%s;max-fs=%d", profileLevelID, h264MaxFSFromProfileLevelID(profileLevelID)),
				RTCPFeedback: videoRTCPFeedback()},
			PayloadType: 102,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("register h264: %w", err)
		}
		// RED: redundant H264 wrapper (PT=116, fmtp: primary/redundant PT)
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    "video/red",
				ClockRate:   90000,
				SDPFmtpLine: "102/102",
			},
			PayloadType: 116,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("register red: %w", err)
		}
		// ULPFEC: XOR-based repair packets (PT=117)
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  "video/ulpfec",
				ClockRate: 90000,
			},
			PayloadType: 117,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("register ulpfec: %w", err)
		}
	}

	// Register playout-delay extension → enables receiver.playoutDelayHint=0
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: playoutDelayURI},
		webrtc.RTPCodecTypeVideo,
	); err != nil {
		return nil, fmt.Errorf("register playout-delay extension: %w", err)
	}

	delayer := &playoutDelayExtInterceptor{}
	redInt := newREDSenderInterceptor(102, 116)

	// NACK responder: buffers outgoing RTP packets and auto-retransmits
	// on receiving RTCP TransportLayerNack from the browser. 1024 packets
	// ≈ 1s @ 30fps — increased from 512 to give RTX more budget on weak
	// uplink where NACKs arrive late.
	//
	// IMPORTANT — pion registry order: the FIRST registered interceptor is
	// closest to the SOCKET, the LAST is closest to the TRACK. The responder
	// must sit between RED and the socket so that:
	//   1. it buffers the FINAL packet form (post RED wrapping), and
	//   2. retransmissions flow straight to the socket WITHOUT passing
	//      through redInt again — otherwise every RTX packet gets
	//      RED-wrapped as a bogus "new primary", corrupting the RED
	//      redundant-block bookkeeping for all subsequent packets
	//      (observed: FEC ON + loss → ~85% phantom loss, zero decode).
	nackResp, err := nack.NewResponderInterceptor(nack.ResponderSize(nackBufferPackets))
	if err != nil {
		return nil, fmt.Errorf("create nack responder: %w", err)
	}

	registry := &interceptor.Registry{}
	// Socket-side → track-side registration order:
	//   nackResp → redInt → delayer
	registry.Add(nackResp)
	registry.Add(redInt)
	registry.Add(delayer)

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(registry),
	)
	pcStart := time.Now()
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}
	logger.Info("[SDP] NewPeerConnection timing", "serial", serial,
		"ms", time.Since(pcStart).Milliseconds())

	// Create video track
	var videoTrack *webrtc.TrackLocalStaticSample
	if videoCodec == "vp8" {
		videoTrack, err = webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: videoRTCPFeedback()},
			"video", "scrcpy-video")
	} else {
		videoTrack, err = webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
				SDPFmtpLine:  fmt.Sprintf("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=%s;max-fs=%d", profileLevelID, h264MaxFSFromProfileLevelID(profileLevelID)),
				RTCPFeedback: videoRTCPFeedback()},
			"video", "scrcpy-video")
	}
	if err != nil {
		return nil, fmt.Errorf("create video track: %w", err)
	}

	// Create audio track (skip if audio disabled)
	var audioTrack *webrtc.TrackLocalStaticSample
	var audioMime string
	var audioClockRate uint32
	var audioChannels uint16
	if audioEnabled {
		switch audioCodec {
		case "pcmu":
			audioMime = webrtc.MimeTypePCMU
			audioClockRate = 8000
			audioChannels = 1
		case "pcma":
			audioMime = webrtc.MimeTypePCMA
			audioClockRate = 8000
			audioChannels = 1
		default:
			audioMime = webrtc.MimeTypeOpus
			audioClockRate = 48000
			audioChannels = 2
		}
		audioTrack, err = webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: audioMime, ClockRate: audioClockRate, Channels: audioChannels},
			"audio", "scrcpy-video")
		if err != nil {
			return nil, fmt.Errorf("create audio track: %w", err)
		}
	}

	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return nil, fmt.Errorf("add video track: %w", err)
	}
	if audioTrack != nil {
		if _, err := pc.AddTrack(audioTrack); err != nil {
			return nil, fmt.Errorf("add audio track: %w", err)
		}
	}

	now := time.Now()
	pm := &PeerManager{
		pc:              pc,
		videoTrack:      videoTrack,
		audioTrack:      audioTrack,
		videoSender:     videoSender,
		serial:          serial,
		videoCodec:      videoCodec,
		audioCodec:      audioCodec,
		audioClockRate:  audioClockRate,
		profileLevelID:  profileLevelID,
		ctrlSendCh:      make(chan []byte, 100),
		stopCh:          make(chan struct{}),
		playoutDelayer:  delayer,
		redInterceptor:  redInt,
		lastDecodedTime: now,
		pliBaseInterval: 1 * time.Second,
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Info("peer connection state changed", "serial", serial, "state", state.String())
		if state == webrtc.PeerConnectionStateConnected && pm.onConnected != nil {
			go pm.onConnected()
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Info("ice connection state changed", "serial", serial, "state", state.String())
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		if pm.onIceCand != nil {
			pm.onIceCand(init.Candidate, init.SDPMid, init.SDPMLineIndex)
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		pm.dataCh = dc
		dc.OnOpen(func() {})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString && pm.onCtrlRecv != nil {
				pm.onCtrlRecv(msg.Data)
			}
		})
	})

	return pm, nil
}

func (pm *PeerManager) HandleOffer(offerSDP, offerType string) (*webrtc.SessionDescription, error) {
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}
	if err := pm.pc.SetRemoteDescription(offer); err != nil {
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	offerVideoCodecs, offerAudioCodecs := parseSDPCodecs(offerSDP)
	logger.Debug("[SDP] offer video codecs", "serial", pm.serial, "codecs", offerVideoCodecs)
	logger.Debug("[SDP] offer audio codecs", "serial", pm.serial, "codecs", offerAudioCodecs)

	if pm.videoCodec == "h264" {
		offerPLIDs := extractProfileLevelIDs(offerSDP)
		if len(offerPLIDs) > 0 {
			logger.Info("[SDP] offer H264 profile-level-id(s)", "serial", pm.serial, "plids", offerPLIDs)
		}
		logger.Info("[SDP] device H264 profile-level-id", "serial", pm.serial, "plid", pm.profileLevelID)
	}

	answerStart := time.Now()
	answer, err := pm.pc.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("create answer: %w", err)
	}

	// PT remap: browsers renumber payload types (Chrome puts RTX at 116 and
	// red ends up at e.g. 123). The RED sender must use the negotiated PTs
	// and the RED fmtp must name the negotiated H264 PT — otherwise RED
	// packets are misinterpreted as RTX garbage (observed: FEC ON → ~85%
	// phantom loss, zero decoded frames). NOTE: the answer SDP must NOT be
	// modified before SetLocalDescription (pion validates it against the
	// generated answer); the fmtp fix is applied to the wire copy below.
	h264PT := extractNegotiatedPT(answer.SDP, "h264")
	redPT := extractNegotiatedPT(answer.SDP, "red")

	if err := pm.pc.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("set local description: %w", err)
	}
	logger.Info("[SDP] CreateAnswer+SetLocalDescription timing", "serial", pm.serial,
		"ms", time.Since(answerStart).Milliseconds())

	// Trickle ICE: return the answer immediately after SetLocalDescription.
	// ICE gathering has just started; candidates are relayed individually via
	// OnICECandidate (registered by the caller before this call). Blocking on
	// GatheringCompletePromise here would stall the answer for the full
	// gathering duration (host + srflx + TURN relay allocation, observed ~5s)
	// and leave the browser with zero remote candidates in the meantime.
	finalAnswer := pm.pc.LocalDescription()

	// Wire copy: add/fix the RED fmtp for the browser (pion omits it, and
	// Chrome refuses to depayload RED without the redundant-PT mapping).
	// The local description keeps the pion-generated SDP untouched.
	wireAnswer := *finalAnswer
	wireAnswer.SDP = rewriteREDFmtp(wireAnswer.SDP, h264PT, redPT)

	// Extract negotiated playout-delay extension ID
	if extID := extractExtmapID(finalAnswer.SDP, playoutDelayURI); extID != 0 {
		pm.playoutDelayer.setID(extID)
		logger.Info("[SDP] playout-delay extension negotiated", "serial", pm.serial, "ext_id", extID)
	}

	// Configure the RED sender with the negotiated payload types. If RED was
	// not negotiated (redPT == 0), RED stays disabled.
	if pm.redInterceptor != nil {
		pm.redInterceptor.SetPayloadTypes(h264PT, redPT)
		logger.Info("[SDP] RED negotiated payload types", "serial", pm.serial,
			"primary_pt", h264PT, "red_pt", redPT)
	}

	logger.Debug("[SDP] answer SDP dump", "serial", pm.serial, "sdp", finalAnswer.SDP)

	answerVideoCodecs, answerAudioCodecs := parseSDPCodecs(finalAnswer.SDP)
	if len(answerVideoCodecs) > 0 {
		logger.Info("[SDP] answer selected video", "serial", pm.serial, "codec", answerVideoCodecs[0])
	}
	if len(answerAudioCodecs) > 0 {
		logger.Info("[SDP] answer selected audio", "serial", pm.serial, "codec", answerAudioCodecs[0])
	} else {
		logger.Warn("[SDP] answer has NO audio codec", "serial", pm.serial)
	}
	answerPLIDs := extractProfileLevelIDs(finalAnswer.SDP)
	if len(answerPLIDs) > 0 {
		logger.Info("[SDP] answer H264 profile-level-id(s)", "serial", pm.serial, "plids", answerPLIDs)
	}

	return &wireAnswer, nil
}

// extractNegotiatedPT returns the payload type bound to codecName
// (case-insensitive: "h264", "red", ...) within the video m-section of the
// given SDP. Returns 0 when the codec was not negotiated.
func extractNegotiatedPT(sdpStr, codecName string) uint8 {
	inVideo := false
	for _, line := range strings.Split(sdpStr, "\r\n") {
		if strings.HasPrefix(line, "m=") {
			inVideo = strings.HasPrefix(line, "m=video")
			continue
		}
		if !inVideo || !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		rest := strings.TrimPrefix(line, "a=rtpmap:")
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) != 2 {
			continue
		}
		enc := strings.SplitN(parts[1], "/", 2)[0]
		if strings.EqualFold(enc, codecName) {
			if pt, err := strconv.Atoi(parts[0]); err == nil && pt > 0 && pt <= 127 {
				return uint8(pt)
			}
		}
	}
	return 0
}

// rewriteREDFmtp ensures the RED fmtp line exists and names the negotiated
// H264 PT. pion may answer with the locally registered "102/102" even after
// PT remapping, or omit the fmtp entirely (observed: no a=fmtp for red at
// all — Chrome then cannot depayload RED). Chrome requires
// "a=fmtp:<redPT> <h264PT>/<h264PT>" to interpret RED blocks.
func rewriteREDFmtp(sdpStr string, h264PT, redPT uint8) string {
	if h264PT == 0 || redPT == 0 {
		return sdpStr
	}
	want := fmt.Sprintf("a=fmtp:%d %d/%d", redPT, h264PT, h264PT)
	old := fmt.Sprintf("a=fmtp:%d 102/102", redPT)
	if strings.Contains(sdpStr, old) {
		return strings.Replace(sdpStr, old, want, 1)
	}
	if strings.Contains(sdpStr, want) {
		return sdpStr
	}
	// No fmtp line for RED at all → insert right after the rtpmap line.
	rtpmap := fmt.Sprintf("a=rtpmap:%d red/90000\r\n", redPT)
	if !strings.Contains(sdpStr, rtpmap) {
		return sdpStr
	}
	return strings.Replace(sdpStr, rtpmap, rtpmap+want+"\r\n", 1)
}

func parseSDPCodecs(sdp string) (video []string, audio []string) {
	lines := strings.Split(sdp, "\r\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "a=rtpmap:") {
			parts := strings.SplitN(line, " ", 2)
			if len(parts) == 2 {
				codecInfo := strings.SplitN(parts[1], "/", 2)
				if len(codecInfo) > 0 {
					codec := strings.ToUpper(codecInfo[0])
					if codec == "OPUS" || codec == "AAC" || codec == "G722" || codec == "PCMU" || codec == "PCMA" {
						audio = append(audio, codec)
					} else if codec == "H264" || codec == "H265" || codec == "VP8" || codec == "VP9" || codec == "AV1" {
						video = append(video, codec)
					}
				}
			}
		}
	}
	return
}

func extractProfileLevelIDs(sdp string) []string {
	var plids []string
	for _, m := range profileLevelIDRe.FindAllStringSubmatch(sdp, -1) {
		plids = append(plids, strings.ToLower(m[2]))
	}
	return plids
}

func extractExtmapID(sdp, uri string) uint8 {
	lines := strings.Split(sdp, "\r\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "a=extmap:") {
			continue
		}
		if !strings.Contains(line, uri) {
			continue
		}
		rest := strings.TrimPrefix(line, "a=extmap:")
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) > 0 {
			idStr := strings.SplitN(parts[0], "/", 2)[0]
			var n uint64
			for _, c := range idStr {
				if c < '0' || c > '9' {
					return 0
				}
				n = n*10 + uint64(c-'0')
				if n > 255 {
					return 0
				}
			}
			return uint8(n)
		}
	}
	return 0
}

func (pm *PeerManager) AddICECandidate(candidateJSON string, sdpMid *string, sdpMLineIndex *uint16) error {
	cand := webrtc.ICECandidateInit{
		Candidate:     candidateJSON,
		SDPMid:        sdpMid,
		SDPMLineIndex: sdpMLineIndex,
	}
	return pm.pc.AddICECandidate(cand)
}

func (pm *PeerManager) SetOnControlRecv(cb func([]byte)) { pm.onCtrlRecv = cb }
func (pm *PeerManager) SetOnICECandidate(cb func(string, *string, *uint16)) { pm.onIceCand = cb }
func (pm *PeerManager) SetOnConnected(cb func()) { pm.onConnected = cb }

func (pm *PeerManager) SetOnRequestKeyframe(cb func()) { pm.onRequestKeyframe = cb }
func (pm *PeerManager) SetOnResetVideo(cb func())      { pm.onResetVideo = cb }

// IsConnected returns true if the PeerConnection is in the "connected" state.
func (pm *PeerManager) IsConnected() bool {
	if pm == nil || pm.pc == nil {
		return false
	}
	return pm.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
}

func (pm *PeerManager) UpdateFramesDecoded(decoded uint32) {
	if decoded > pm.lastDecoded {
		pm.lastDecoded = decoded
		pm.lastDecodedTime = time.Now()
	}
}

func (pm *PeerManager) SendControl(data []byte) error {
	if pm.dataCh == nil || pm.dataCh.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("data channel not ready")
	}
	return pm.dataCh.Send(data)
}

func (pm *PeerManager) StartMediaPumps(videoCh <-chan gateway.Sample, audioCh <-chan gateway.Sample, ctrlCh <-chan gateway.DeviceMsg) {
	logger.Info("starting media pumps", "serial", pm.serial)
	pm.wg.Add(4)
	go func() { defer pm.wg.Done(); pm.videoPump(videoCh) }()
	go func() { defer pm.wg.Done(); pm.audioPump(audioCh) }()
	go func() { defer pm.wg.Done(); pm.ctrlPump(ctrlCh) }()
	go func() { defer pm.wg.Done(); pm.rtcpListener() }()
}

// rtcpListener handles RTCP feedback with two-level PLI recovery:
//
//	L1 (request): lightweight IDR via onRequestKeyframe → BuildRequestKeyframe(0x17)
//	L2 (reset):   full encoder reset via onResetVideo → BuildResetVideo(0x11)
//
// NACK-based per-packet retransmission is handled transparently by
// nack.ResponderInterceptor, so PLI is reserved for cases where the
// decoder is truly stuck (e.g. lost SPS/PPS, encoder state mismatch).
//
// FIR always triggers immediate L2 reset (min 15s throttle).
func (pm *PeerManager) rtcpListener() {
	logger.Info("rtcp listener started", "serial", pm.serial)
	logger.Debug("PLI state machine config",
		"serial", pm.serial,
		"pliBaseInterval", pm.pliBaseInterval,
		"note", "0 means no L1 throttle – first PLI fires immediately")

	pm.lastPLITime = time.Now()

	const minResetInterval = 14 * time.Second // 编码器重置最小间隔（调校取值）
	var lastFIR time.Time

	for {
		select {
		case <-pm.stopCh:
			return
		default:
		}

		rtcpPackets, _, err := pm.videoSender.ReadRTCP()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		for _, pkt := range rtcpPackets {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication:
				now := time.Now()
				// Throttle: L1 uses pliBaseInterval, L2 uses 10s
				throttle := pm.pliBaseInterval
				if pm.pliState == pliStateReset {
					throttle = 10 * time.Second
				}
				elapsed := now.Sub(pm.lastPLITime)
				logger.Debug("RTCP PLI received",
					"serial", pm.serial,
					"pliState", pm.pliState,
					"pliCount", pm.pliCount,
					"throttle", throttle,
					"elapsed_since_last", elapsed,
					"throttled", elapsed < throttle)
				if elapsed < throttle {
					continue
				}
				pm.lastPLITime = now
				pm.pliCount++

				// Self-healing: if decoded has increased since entering
				// recovery, the decoder has recovered — go back to normal.
				if pm.pliState != pliStateNormal && pm.lastDecoded > pm.decodedAtStateChange {
					logger.Info("PLI recovery: decoded grew, back to normal",
						"serial", pm.serial, "decoded", pm.lastDecoded, "baseline", pm.decodedAtStateChange)
					pm.pliState = pliStateNormal
					pm.pliCount = 0
				}

				switch pm.pliState {
				case pliStateNormal:
					pm.pliCount = 1
					pm.pliState = pliStateRequest
					pm.lastDecodedTime = now
					pm.decodedAtStateChange = pm.lastDecoded
					logger.Warn("PLI L1: requesting lightweight IDR",
						"serial", pm.serial, "count", pm.pliCount)
					if pm.onRequestKeyframe != nil {
						go pm.onRequestKeyframe()
					}

				case pliStateRequest:
					// 3s without decoded growth or 6+ PLI → escalate to L2
					if pm.pliCount >= 6 || now.Sub(pm.lastDecodedTime) > 3*time.Second {
						if now.Sub(pm.lastResetTime) < minResetInterval {
							logger.Warn("PLI L2: reset throttled",
								"serial", pm.serial, "since", now.Sub(pm.lastResetTime))
							continue
						}
						pm.pliState = pliStateReset
						pm.pliCount = 0
						pm.lastResetTime = now
						logger.Error("PLI L2: stream unrecoverable, resetting encoder",
							"serial", pm.serial)
						if pm.onResetVideo != nil {
							go pm.onResetVideo()
						}
					} else {
						logger.Warn("PLI L1: requesting lightweight IDR",
							"serial", pm.serial, "count", pm.pliCount)
						if pm.onRequestKeyframe != nil {
							go pm.onRequestKeyframe()
						}
					}

				case pliStateReset:
					if now.Sub(pm.lastResetTime) > minResetInterval {
						pm.lastResetTime = now
						logger.Error("PLI L2: retry encoder reset",
							"serial", pm.serial)
						if pm.onResetVideo != nil {
							go pm.onResetVideo()
						}
					}
				}

			case *rtcp.FullIntraRequest:
				logger.Warn("RTCP FIR received", "serial", pm.serial)
				if time.Since(lastFIR) < minResetInterval {
					continue
				}
				lastFIR = time.Now()
				pm.pliState = pliStateReset
				pm.pliCount = 0
				pm.lastResetTime = time.Now()
				if pm.onResetVideo != nil {
					go pm.onResetVideo()
				}

			// TransportLayerNack is handled by nack.ResponderInterceptor
			// at the interceptor layer — retransmits buffered packets
			// automatically and never reaches this switch.

			case *rtcp.ReceiverReport:
				// Extract DLSR (Delay Since Last Sender Report) from
				// reception blocks as a complementary RTT signal.
				// DLSR alone is a lower-bound estimate; the browser's
				// candidate-pair RTT may under-report in TURN relay
				// scenarios, so we use max(browser, DLSR).
				for _, report := range pkt.(*rtcp.ReceiverReport).Reports {
					if report.Delay > 0 {
						// Delay is in units of 1/65536 seconds
						dlsrMs := float64(report.Delay) / 65536.0 * 1000
						pm.rtcpRTTMs.Store(dlsrMs)
						break
					}
				}

			default:
				continue
			}
		}
	}
}

func (pm *PeerManager) videoPump(ch <-chan gateway.Sample) {
	logger.Info("video pump started", "serial", pm.serial)
	count := 0
	errCount := 0
	for {
		select {
		case <-pm.stopCh:
			return
		case sample, ok := <-ch:
			if !ok {
				return
			}
			rtpTS := uint32(sample.PTS.Microseconds() * 90 / 1000)
			if err := pm.videoTrack.WriteSample(media.Sample{
				Data:            sample.Data,
				PacketTimestamp: rtpTS,
				Duration:        sample.Duration,
			}); err != nil {
				// Transient write errors (e.g. a single oversized packet)
				// must NOT kill the pump — drop the sample and continue.
				errCount++
				if errCount <= 5 || errCount%300 == 0 {
					logger.Warn("write video sample (dropped, pump continues)",
						"serial", pm.serial, "err", err, "err_count", errCount)
				}
				continue
			}
			count++
			if count <= 5 || count%600 == 0 {
				logger.Debug("video sample written", "serial", pm.serial, "count", count, "size", len(sample.Data), "pts", sample.PTS, "rtp_ts", rtpTS)
			}
		}
	}
}

func (pm *PeerManager) audioPump(ch <-chan gateway.Sample) {
	for {
		select {
		case <-pm.stopCh:
			return
		case sample, ok := <-ch:
			if !ok {
				return
			}
			isG711 := pm.audioCodec == "pcmu" || pm.audioCodec == "pcma"
			var sampleCount uint32
			if isG711 {
				sampleCount = uint32(len(sample.Data))
			} else {
				sampleCount = uint32(float64(pm.audioClockRate) * sample.Duration.Seconds())
			}
			_ = pm.audioTrack.WriteSample(media.Sample{
				Data:     sample.Data,
				Duration: time.Duration(float64(sampleCount) / float64(pm.audioClockRate) * float64(time.Second)),
			})
		}
	}
}

func (pm *PeerManager) ctrlPump(ch <-chan gateway.DeviceMsg) {
	for {
		select {
		case <-pm.stopCh:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(msg)
			_ = pm.SendControl(data)
		}
	}
}

func videoRTCPFeedback() []webrtc.RTCPFeedback {
	return []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "ccm", Parameter: "fir"},
	}
}

func (pm *PeerManager) Close() error {
	// Listen for ICE connection state "closed" before pc.Close() overwrites
	// the OnICEConnectionStateChange callback. pion's pc.Close() is async
	// (ICE agent / UDP socket release happens in background). Waiting for
	// "closed" here avoids port conflicts when a new PeerConnection is
	// created immediately after this one is closed.
	iceClosed := make(chan struct{})
	var iceOnce sync.Once
	pm.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateClosed {
			iceOnce.Do(func() { close(iceClosed) })
		}
	})

	// If the ICE connection is already closed (e.g. the original callback
	// set during NewPeerManager already observed the "closed" transition
	// before pc.Close() was called), the newly registered callback above
	// will never fire because it only observes state *changes*.
	// Pre-close iceClosed here so the select below returns immediately.
	if pm.pc.ICEConnectionState() == webrtc.ICEConnectionStateClosed {
		iceOnce.Do(func() { close(iceClosed) })
	}

	// Close PeerConnection first to unblock ReadRTCP(), then signal
	// goroutines and wait for them to exit.
	_ = pm.pc.Close()
	close(pm.stopCh)
	pm.wg.Wait()

	// Wait for ICE agent to release UDP socket (or give up after 2s).
	select {
	case <-iceClosed:
	case <-time.After(2 * time.Second):
		logger.Warn("peer close: ICE did not transition to closed within 2s",
			"serial", pm.serial)
	}

	return nil
}

// ---- QoS telemetry getters/setters ------------------------------------

func (pm *PeerManager) GetLossRate() float64 {
	if v, ok := pm.lossRate.Load().(float64); ok {
		return v
	}
	return 0
}

func (pm *PeerManager) GetRTTMs() float64 {
	// Use the larger of browser-reported RTT and RTCP-based RTT.
	// The browser's candidate-pair RTT may underestimate in TURN relay
	// scenarios; RTCP DLSR provides a complementary lower-bound signal.
	browserRTT := float64(0)
	if v, ok := pm.rttMs.Load().(float64); ok {
		browserRTT = v
	}
	rtcpRTT := float64(0)
	if v, ok := pm.rtcpRTTMs.Load().(float64); ok {
		rtcpRTT = v
	}
	if rtcpRTT > browserRTT {
		return rtcpRTT
	}
	return browserRTT
}

// SetCurrentBitrate stores the current video encoder bitrate so the
// FEC evaluator can gate itself at low bitrates (FEC adds ~50 % overhead
// and would cancel out the benefit of a reduced bitrate).
func (pm *PeerManager) SetCurrentBitrate(bps int) {
	pm.currentBitrate.Store(int32(bps))
}

// GetCurrentBitrate returns the last stored encoder bitrate or 0 if unset.
func (pm *PeerManager) GetCurrentBitrate() int {
	return int(pm.currentBitrate.Load())
}

// UpdateQoS accepts loss rate (%) and RTT (ms) from the browser's
// decoder_status telemetry and re-evaluates FEC state.
func (pm *PeerManager) UpdateQoS(lossRatePct, rttMsVal float64) {
	pm.lossRate.Store(lossRatePct)
	pm.rttMs.Store(rttMsVal)
	pm.evaluateFEC()
}

// SetFECAutoDisabled gates the automatic FEC evaluator (config kill switch).
func (pm *PeerManager) SetFECAutoDisabled(v bool) {
	pm.fecAutoDisabled.Store(v)
}

// ---- FEC auto-decision -------------------------------------------------

// FEC 判定硬编码参数（调校后的非常用取值）。
const (
	fecLossThreshold     = 2.1   // loss > 2.1% → consider FEC
	fecRTTMax            = 180.0 // RTT < 180ms → still useful for FEC on weak uplink
	fecWeakUplinkLossMin = 5.5   // loss > 5.5% + any RTT → weak uplink, force FEC ON
	fecHysteresisMin     = 9 * time.Second
)

// evaluateFEC decides whether to enable RED FEC based on:
//
//	loss ↑ + RTT low/stable → random loss → FEC ON
//	loss ↑ + RTT rising      → congestion  → FEC OFF (unless weak-uplink override)
//	loss ↓                    → normal      → FEC OFF
func (pm *PeerManager) evaluateFEC() {
	if pm.redInterceptor == nil || pm.fecAutoDisabled.Load() {
		return
	}

	lossRate := pm.GetLossRate()
	rttMs := pm.GetRTTMs() // max(browser RTT, RTCP DLSR RTT)

	if time.Since(pm.lastFECToggle) < fecHysteresisMin {
		return
	}

	cur := pm.fecEnabledFlag.Load()

	// Weak-uplink override: when loss is high we enable FEC regardless of
	// RTT, because on congested cellular/WiFi uplinks redundancy is often
	// the only way to recover packets that were simply dropped by the
	// intermediate radio link.
	if lossRate > fecWeakUplinkLossMin {
		if !cur {
			pm.lastFECToggle = time.Now()
			pm._setFECEnabled(true)
			logger.Info("FEC auto ON (weak uplink override)",
				"serial", pm.serial,
				"lossRate", fmt.Sprintf("%.1f%%", lossRate),
				"rttMs", fmt.Sprintf("%.0f", rttMs))
		}
		return
	}

	if lossRate > fecLossThreshold && rttMs > 0 && rttMs < fecRTTMax {
		// Random loss: high loss, low RTT
		if !cur {
			pm.lastFECToggle = time.Now()
			pm._setFECEnabled(true)
			logger.Info("FEC auto ON (random loss)",
				"serial", pm.serial,
				"lossRate", fmt.Sprintf("%.1f%%", lossRate),
				"rttMs", fmt.Sprintf("%.0f", rttMs))
		}
	} else if lossRate > fecLossThreshold && rttMs >= fecRTTMax {
		// Congestion: high loss, high RTT — FEC would make it worse
		if cur {
			pm.lastFECToggle = time.Now()
			pm._setFECEnabled(false)
			logger.Warn("FEC auto OFF (congestion)",
				"serial", pm.serial,
				"lossRate", fmt.Sprintf("%.1f%%", lossRate),
				"rttMs", fmt.Sprintf("%.0f", rttMs))
		}
	} else if lossRate < fecLossThreshold {
		// Normal: low loss, no need for FEC
		if cur {
			pm.lastFECToggle = time.Now()
			pm._setFECEnabled(false)
			logger.Info("FEC auto OFF (loss normal)",
				"serial", pm.serial,
				"lossRate", fmt.Sprintf("%.1f%%", lossRate))
		}
	}
}

// _setFECEnabled is the internal toggle; use evaluateFEC for auto-decision.
func (pm *PeerManager) _setFECEnabled(v bool) {
	pm.redInterceptor.SetEnabled(v)
	pm.fecEnabledFlag.Store(v)
	logger.Info("FEC (RED) state changed", "serial", pm.serial, "enabled", v)
}

// SetFECEnabled provides a manual override for FEC; prefer UpdateQoS in
// production to let the auto-decision logic run.
func (pm *PeerManager) SetFECEnabled(v bool) {
	pm._setFECEnabled(v)
}
